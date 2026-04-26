package instance

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// TxBeginner is the narrow subset of pgxpool.Pool / pgx.Conn we need to open
// transactions. pgxpool.Pool implements this interface.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Repo persists instances and their lifecycle events.
type Repo struct {
	beg     TxBeginner
	queries *dbstore.Queries
}

// NewRepo builds a Repo from a pgx beginner (pool) and its sqlc Queries.
func NewRepo(beg TxBeginner, queries *dbstore.Queries) *Repo {
	return &Repo{beg: beg, queries: queries}
}

// CreateInput bundles the fields needed to insert a row; no transitions are
// necessary because an instance is born in state=pending.
type CreateInput struct {
	OwnerID     uuid.NullUUID
	NodeID      uuid.UUID
	Name        string
	MemoryMiB   int32
	VCPUs       int32
	GPUCount    int32
	SlotIndices []int32
	SSHPubkeys  []string
	ImageRef    string
}

// Create inserts a new instance + its initial audit event in one transaction.
func (r *Repo) Create(ctx context.Context, in CreateInput) (dbstore.Instance, error) {
	var inst dbstore.Instance
	slotIdx := in.SlotIndices
	if slotIdx == nil {
		slotIdx = []int32{}
	}
	pubkeys := in.SSHPubkeys
	if pubkeys == nil {
		pubkeys = []string{}
	}
	err := r.inTx(ctx, func(q *dbstore.Queries) error {
		created, err := q.CreateInstance(ctx, dbstore.CreateInstanceParams{
			OwnerID:     in.OwnerID,
			NodeID:      in.NodeID,
			Name:        in.Name,
			MemoryMb:    in.MemoryMiB,
			Vcpus:       in.VCPUs,
			GpuCount:    in.GPUCount,
			SlotIndices: slotIdx,
			SshPubkeys:  pubkeys,
			ImageRef:    in.ImageRef,
		})
		if err != nil {
			return fmt.Errorf("insert instance: %w", err)
		}
		if err := q.InsertInstanceEvent(ctx, dbstore.InsertInstanceEventParams{
			InstanceID: created.ID,
			FromState:  dbstore.NullInstanceState{Valid: false},
			ToState:    StatePending,
			Reason:     "created",
			Metadata:   []byte(`{}`),
		}); err != nil {
			return fmt.Errorf("insert event: %w", err)
		}
		inst = created
		return nil
	})
	return inst, err
}

// TransitionOptions hold optional payload attached to a transition.
type TransitionOptions struct {
	Reason         string
	ErrorMessage   string     // sets instances.error_message when non-empty
	VMInternalIP   netip.Addr // sets instances.vm_internal_ip when valid
	EventMetadata  []byte     // raw JSON attached to the event row
	SkipIdempotent bool       // if true, same-state transitions fail instead of being no-ops
}

// Transition advances an instance to a new state, writing an audit event in
// the same transaction. The target state must be reachable from the current
// state per the state machine.
func (r *Repo) Transition(
	ctx context.Context,
	id uuid.UUID,
	to State,
	opts TransitionOptions,
) (dbstore.Instance, error) {
	if err := Validate(to); err != nil {
		return dbstore.Instance{}, err
	}

	var updated dbstore.Instance
	err := r.inTx(ctx, func(q *dbstore.Queries) error {
		current, err := q.GetInstance(ctx, id)
		if err != nil {
			return fmt.Errorf("load instance: %w", err)
		}
		from := current.State

		if from == to && opts.SkipIdempotent {
			return fmt.Errorf("%w: idempotent %s->%s rejected", ErrInvalidTransition, from, to)
		}
		if !CanTransition(from, to) {
			return fmt.Errorf("%w: %s->%s", ErrInvalidTransition, from, to)
		}

		params := dbstore.UpdateInstanceStateParams{
			ID:    id,
			State: to,
		}
		if opts.ErrorMessage != "" {
			params.ErrorMessage = pgtype.Text{String: opts.ErrorMessage, Valid: true}
		}
		if opts.VMInternalIP.IsValid() {
			addr := opts.VMInternalIP
			params.VmInternalIp = &addr
		}

		updated, err = q.UpdateInstanceState(ctx, params)
		if err != nil {
			return fmt.Errorf("update state: %w", err)
		}

		meta := opts.EventMetadata
		if len(meta) == 0 {
			meta = []byte("{}")
		}
		if err := q.InsertInstanceEvent(ctx, dbstore.InsertInstanceEventParams{
			InstanceID: id,
			FromState:  dbstore.NullInstanceState{InstanceState: from, Valid: true},
			ToState:    to,
			Reason:     opts.Reason,
			Metadata:   meta,
		}); err != nil {
			return fmt.Errorf("insert event: %w", err)
		}
		return nil
	})
	return updated, err
}

// Get reads one instance.
func (r *Repo) Get(ctx context.Context, id uuid.UUID) (dbstore.Instance, error) {
	return r.queries.GetInstance(ctx, id)
}

// Delete removes an instance row. Callers should transition to a terminal
// state first; this is the final cleanup.
func (r *Repo) Delete(ctx context.Context, id uuid.UUID) error {
	return r.queries.DeleteInstance(ctx, id)
}

// ListForOwner returns instances for the given owner (or all when ownerID is
// nil-valued). Phase 3 admin path passes OwnerID.Valid=false.
func (r *Repo) ListForOwner(ctx context.Context, ownerID uuid.NullUUID) ([]dbstore.Instance, error) {
	return r.queries.ListInstances(ctx, ownerID)
}

// FindByOwnerAndIDPrefix resolves the short subdomain form (`{prefix}.zone`)
// scoped to a single owner. Phase 6 ssh-proxy + Phase 9 audit both route by
// owner so a fingerprint mismatch never leaks instance existence.
func (r *Repo) FindByOwnerAndIDPrefix(
	ctx context.Context, ownerID uuid.UUID, prefix string,
) ([]dbstore.Instance, error) {
	return r.queries.ListInstancesByOwnerAndIDPrefix(ctx, dbstore.ListInstancesByOwnerAndIDPrefixParams{
		OwnerID: uuid.NullUUID{UUID: ownerID, Valid: true},
		Column2: pgtype.Text{String: prefix, Valid: true},
	})
}

// --- internals -------------------------------------------------------------

func (r *Repo) inTx(ctx context.Context, fn func(q *dbstore.Queries) error) error {
	tx, err := r.beg.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		// Rollback is a no-op if the tx was already committed.
		_ = tx.Rollback(ctx)
	}()
	if err := fn(r.queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
