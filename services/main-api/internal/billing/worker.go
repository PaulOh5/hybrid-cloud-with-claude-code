package billing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/credit"
	"hybridcloud/services/main-api/internal/db/dbstore"
	agentv1 "hybridcloud/shared/proto/agent/v1"
)

// CreditPoster is the slice of *credit.Repo the worker writes to.
type CreditPoster interface {
	Post(ctx context.Context, in credit.PostInput) (dbstore.CreditLedger, error)
	UsersWithNegativeBalance(ctx context.Context) ([]uuid.UUID, error)
}

// InstanceLister is the slice of dbstore.Queries the worker reads from.
type InstanceLister interface {
	ListBillableRunningInstances(ctx context.Context) ([]dbstore.ListBillableRunningInstancesRow, error)
	ListInstances(ctx context.Context, ownerID uuid.NullUUID) ([]dbstore.Instance, error)
}

// Dispatcher dispatches DestroyInstance for users whose balance went ≤ 0.
type Dispatcher interface {
	Send(nodeID uuid.UUID, msg *agentv1.ControlMessage) error
}

// Worker bills running instances and stops users out of credit.
type Worker struct {
	Instances  InstanceLister
	Credits    CreditPoster
	Rates      *RateTable
	Dispatcher Dispatcher
	Tick       time.Duration
	Now        func() time.Time
	Log        *slog.Logger
}

// Run loops until ctx is cancelled, ticking on Worker.Tick.
func (w *Worker) Run(ctx context.Context) error {
	w.applyDefaults()
	t := time.NewTicker(w.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := w.runOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				w.Log.Error("billing tick", "err", err)
			}
		}
	}
}

// RunOnce exposes a single iteration for tests.
func (w *Worker) RunOnce(ctx context.Context) error {
	w.applyDefaults()
	return w.runOnce(ctx)
}

func (w *Worker) applyDefaults() {
	if w.Tick <= 0 {
		w.Tick = 30 * time.Second
	}
	if w.Now == nil {
		w.Now = time.Now
	}
	if w.Log == nil {
		w.Log = slog.Default()
	}
}

func (w *Worker) runOnce(ctx context.Context) error {
	if err := w.charge(ctx); err != nil {
		return fmt.Errorf("charge: %w", err)
	}
	if err := w.sweepBankrupt(ctx); err != nil {
		return fmt.Errorf("sweep: %w", err)
	}
	return nil
}

// charge inserts a ledger entry for each running instance for the current
// minute bucket. The unique idempotency_key prevents double-billing across
// tick overlap, restarts, or skewed runs.
func (w *Worker) charge(ctx context.Context) error {
	rows, err := w.Instances.ListBillableRunningInstances(ctx)
	if err != nil {
		return err
	}
	bucket := w.Now().UTC().Unix() / 60
	for _, row := range rows {
		if !row.OwnerID.Valid {
			continue
		}
		rate := w.Rates.MilliPerMinute(row.GpuCount)
		if rate == 0 {
			continue
		}
		key := fmt.Sprintf("billing:%s:%d", row.ID, bucket)
		_, err := w.Credits.Post(ctx, credit.PostInput{
			UserID:         row.OwnerID.UUID,
			DeltaMilli:     -rate,
			Reason:         "running_minute",
			InstanceID:     uuid.NullUUID{UUID: row.ID, Valid: true},
			IdempotencyKey: key,
		})
		if err != nil && !errors.Is(err, credit.ErrDuplicateIdempotency) {
			w.Log.Warn("charge instance", "instance_id", row.ID, "err", err)
		}
	}
	return nil
}

// sweepBankrupt finds users whose balance is ≤ 0 and dispatches
// DestroyInstance for each of their running instances. The credit gate at
// create time keeps new instances from launching; this loop catches the
// drain-during-run case.
func (w *Worker) sweepBankrupt(ctx context.Context) error {
	if w.Dispatcher == nil {
		return nil
	}
	users, err := w.Credits.UsersWithNegativeBalance(ctx)
	if err != nil {
		return err
	}
	for _, userID := range users {
		instances, err := w.Instances.ListInstances(ctx, uuid.NullUUID{UUID: userID, Valid: true})
		if err != nil {
			w.Log.Warn("list instances for sweep", "user_id", userID, "err", err)
			continue
		}
		for _, inst := range instances {
			if inst.State != dbstore.InstanceStateRunning && inst.State != dbstore.InstanceStateProvisioning {
				continue
			}
			msg := &agentv1.ControlMessage{
				Payload: &agentv1.ControlMessage_DestroyInstance{
					DestroyInstance: &agentv1.DestroyInstance{InstanceId: inst.ID.String()},
				},
			}
			if err := w.Dispatcher.Send(inst.NodeID, msg); err != nil {
				w.Log.Warn("dispatch destroy for bankrupt user",
					"user_id", userID, "instance_id", inst.ID, "err", err)
			} else {
				w.Log.Info("dispatched destroy for bankrupt user",
					"user_id", userID, "instance_id", inst.ID)
			}
		}
	}
	return nil
}
