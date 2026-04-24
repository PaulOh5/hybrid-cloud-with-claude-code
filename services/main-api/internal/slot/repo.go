// Package slot manages GPU slot reservation for instance creation. A "slot"
// is a fixed unit of passthrough-eligible GPUs on a node (per Phase 4
// gpu_count=1; Phase 5 adds multi-GPU profile layouts).
//
// The reservation primitive takes a pg_advisory_xact_lock keyed on the
// target node so two schedulers cannot race on overlapping free sets. Every
// reservation runs in its own transaction: the caller either Binds the
// reserved slots to an instance (promoting reserved→in_use) or releases
// them back to free on dispatch failure.
package slot

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"hybridcloud/services/main-api/internal/db/dbstore"
)

// ErrNoFreeSlots is returned when the node has no slots matching the size
// requested.
var ErrNoFreeSlots = errors.New("slot: no free slots for requested size")

// TxBeginner is the narrow subset of pgxpool.Pool the repo uses.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Repo reserves and releases slots on behalf of the scheduler.
type Repo struct {
	beg     TxBeginner
	queries *dbstore.Queries
}

// NewRepo wires a Repo against the pgx pool + sqlc queries.
func NewRepo(beg TxBeginner, queries *dbstore.Queries) *Repo {
	return &Repo{beg: beg, queries: queries}
}

// Reservation is the return value of Reserve. Slots holds the full sqlc rows
// so the caller can forward slot_indices to the compute-agent.
type Reservation struct {
	Slots []dbstore.GpuSlot
}

// SlotIndices flattens the reservation into the int32 indices the agent RPC
// expects.
func (r Reservation) SlotIndices() []int32 {
	out := make([]int32, 0, len(r.Slots))
	for _, s := range r.Slots {
		out = append(out, s.SlotIndex)
	}
	return out
}

// IDs returns the slot UUIDs; useful for Release on rollback.
func (r Reservation) IDs() []uuid.UUID {
	out := make([]uuid.UUID, 0, len(r.Slots))
	for _, s := range r.Slots {
		out = append(out, s.ID)
	}
	return out
}

// Reserve atomically picks `count` free slots of `gpuSize` on the node and
// marks them reserved. Always called inside a fresh tx (we own the tx so the
// advisory lock is released on COMMIT/ROLLBACK).
//
// Callers Bind(tx, reservation, instanceID) within the same outer tx to
// promote reserved→in_use. Forgotten reservations are cleaned up by a future
// scheduler run via ReleaseReserved.
func (r *Repo) Reserve(ctx context.Context, nodeID uuid.UUID, gpuSize, count int32) (Reservation, error) {
	if count <= 0 {
		return Reservation{}, fmt.Errorf("slot: count must be > 0, got %d", count)
	}
	var res Reservation
	err := r.inTx(ctx, func(q *dbstore.Queries) error {
		if err := q.LockNodeForReservation(ctx, nodeID.String()); err != nil {
			return fmt.Errorf("advisory lock: %w", err)
		}
		slots, err := q.ReserveFreeSlots(ctx, dbstore.ReserveFreeSlotsParams{
			NodeID:   nodeID,
			GpuCount: gpuSize,
			Limit:    count,
		})
		if err != nil {
			return fmt.Errorf("reserve: %w", err)
		}
		if int64(len(slots)) < int64(count) {
			// Not enough free — abort so the reservation rolls back.
			return fmt.Errorf("%w: have %d, need %d", ErrNoFreeSlots, len(slots), count)
		}
		res.Slots = slots
		return nil
	})
	if err != nil {
		return Reservation{}, err
	}
	return res, nil
}

// BindToInstance promotes every reserved slot in res to in_use with the
// given instance_id. Call this only after the instance row is committed and
// the CreateInstance control message has been accepted by the agent.
func (r *Repo) BindToInstance(ctx context.Context, res Reservation, instanceID uuid.UUID) error {
	return r.inTx(ctx, func(q *dbstore.Queries) error {
		for _, s := range res.Slots {
			if _, err := q.BindSlotToInstance(ctx, dbstore.BindSlotToInstanceParams{
				ID:                s.ID,
				CurrentInstanceID: uuid.NullUUID{UUID: instanceID, Valid: true},
			}); err != nil {
				return fmt.Errorf("bind slot %s: %w", s.ID, err)
			}
		}
		return nil
	})
}

// ReleaseReserved rolls back a reservation (e.g. dispatch failed before the
// slot could be bound). Slots that were already in_use are left untouched.
func (r *Repo) ReleaseReserved(ctx context.Context, res Reservation) error {
	if len(res.Slots) == 0 {
		return nil
	}
	_, err := r.queries.ReleaseReservedSlots(ctx, res.IDs())
	return err
}

// ReleaseForInstance transitions every slot owned by instanceID back to free.
// Used on VM teardown.
func (r *Repo) ReleaseForInstance(ctx context.Context, instanceID uuid.UUID) (int64, error) {
	return r.queries.ReleaseSlotsForInstance(ctx, uuid.NullUUID{UUID: instanceID, Valid: true})
}

// SyncResult is what SyncFromProfile tells the caller — did we replace the
// slots, do nothing, or refuse because slots are in use.
type SyncResult int

const (
	SyncUnchanged SyncResult = iota // profile hash matches DB; no-op
	SyncReplaced                    // DB slots replaced with profile slots
	SyncDegraded                    // profile differs but existing slots are in-use → keep old
)

// ProfileSlot mirrors the proto SlotSpec so callers outside the grpc stream
// can sync without importing the proto package.
type ProfileSlot struct {
	SlotIndex    int32
	GpuCount     int32
	GpuIndices   []int32
	NvlinkDomain string
}

// SyncFromProfile reconciles the DB's gpu_slots for nodeID with the list
// reported by the agent. If the new hash matches the node's stored
// profile_hash it is a no-op. Otherwise slots are replaced when every
// existing slot is free; when any slot is in_use we refuse to touch them
// (the caller should mark the node degraded and alert the operator).
func (r *Repo) SyncFromProfile(
	ctx context.Context,
	nodeID uuid.UUID,
	profileHash string,
	slots []ProfileSlot,
) (SyncResult, error) {
	// Short-circuit: if hash matches, nothing to do.
	node, err := r.queries.GetNode(ctx, nodeID)
	if err != nil {
		return SyncUnchanged, fmt.Errorf("load node: %w", err)
	}
	if node.ProfileHash == profileHash && profileHash != "" {
		return SyncUnchanged, nil
	}

	var result SyncResult
	err = r.inTx(ctx, func(q *dbstore.Queries) error {
		inUse, err := q.CountNonFreeSlotsForNode(ctx, nodeID)
		if err != nil {
			return fmt.Errorf("count non-free: %w", err)
		}
		if inUse > 0 {
			result = SyncDegraded
			return nil
		}
		if _, err := q.DeleteSlotsForNode(ctx, nodeID); err != nil {
			return fmt.Errorf("delete existing: %w", err)
		}
		for _, s := range slots {
			if _, err := q.InsertSlot(ctx, dbstore.InsertSlotParams{
				NodeID:       nodeID,
				SlotIndex:    s.SlotIndex,
				GpuCount:     s.GpuCount,
				GpuIndices:   s.GpuIndices,
				NvlinkDomain: s.NvlinkDomain,
			}); err != nil {
				return fmt.Errorf("insert slot %d: %w", s.SlotIndex, err)
			}
		}
		if err := q.UpdateNodeProfileHash(ctx, dbstore.UpdateNodeProfileHashParams{
			ID:          nodeID,
			ProfileHash: profileHash,
		}); err != nil {
			return fmt.Errorf("update profile_hash: %w", err)
		}
		result = SyncReplaced
		return nil
	})
	return result, err
}

// --- helpers ---------------------------------------------------------------

func (r *Repo) inTx(ctx context.Context, fn func(q *dbstore.Queries) error) error {
	tx, err := r.beg.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(r.queries.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
