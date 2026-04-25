package metrics

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// DomainQueries is the slice of dbstore queries the refresher needs.
type DomainQueries interface {
	ListInstances(ctx context.Context, ownerID uuid.NullUUID) ([]dbstore.Instance, error)
	ListSlotsForAdminView(ctx context.Context) ([]dbstore.ListSlotsForAdminViewRow, error)
}

// DomainRefresher repeatedly samples DB state and exposes it via
// instance_total{state} + gpu_slot_used{node_name,slot_index}. Cheap point-
// in-time gauge — no event-driven plumbing — keeps the implementation
// simple at the cost of a refresh-interval lag.
type DomainRefresher struct {
	Queries  DomainQueries
	Coll     *Collectors
	Interval time.Duration
	Log      *slog.Logger
}

// Run loops until ctx is cancelled, sampling on Interval.
func (d *DomainRefresher) Run(ctx context.Context) error {
	if d.Interval <= 0 {
		d.Interval = 15 * time.Second
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	t := time.NewTicker(d.Interval)
	defer t.Stop()
	if err := d.Sample(ctx); err != nil {
		d.Log.Warn("metrics initial sample", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := d.Sample(ctx); err != nil && !errors.Is(err, context.Canceled) {
				d.Log.Warn("metrics sample", "err", err)
			}
		}
	}
}

// Sample takes a single snapshot. Exposed for tests and one-off use.
func (d *DomainRefresher) Sample(ctx context.Context) error {
	instances, err := d.Queries.ListInstances(ctx, uuid.NullUUID{Valid: false})
	if err != nil {
		return err
	}
	counts := map[dbstore.InstanceState]int{
		dbstore.InstanceStatePending:      0,
		dbstore.InstanceStateProvisioning: 0,
		dbstore.InstanceStateRunning:      0,
		dbstore.InstanceStateStopping:     0,
		dbstore.InstanceStateStopped:      0,
		dbstore.InstanceStateFailed:       0,
	}
	for _, inst := range instances {
		counts[inst.State]++
	}
	for state, n := range counts {
		d.Coll.InstanceTotal.WithLabelValues(string(state)).Set(float64(n))
	}

	slots, err := d.Queries.ListSlotsForAdminView(ctx)
	if err != nil {
		return err
	}
	d.Coll.GPUSlotUsed.Reset()
	for _, s := range slots {
		used := 0.0
		if s.Status == dbstore.SlotStatusInUse {
			used = 1.0
		}
		d.Coll.GPUSlotUsed.
			WithLabelValues(s.NodeName, strconv.Itoa(int(s.SlotIndex))).
			Set(used)
	}
	return nil
}
