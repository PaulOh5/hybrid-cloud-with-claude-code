-- name: InsertSlot :one
insert into gpu_slots (node_id, slot_index, gpu_count, gpu_indices, nvlink_domain)
values ($1, $2, $3, $4, $5)
returning *;

-- name: ListSlotsForNode :many
select * from gpu_slots
where node_id = $1
order by slot_index;

-- name: CountFreeSlotsByGPUCount :one
select count(*)::bigint as free_count
from gpu_slots
where node_id = $1 and gpu_count = $2 and status = 'free';

-- name: LockNodeForReservation :exec
-- pg_advisory_xact_lock serialises all reservations for the same node inside
-- a single advisory-lock namespace derived from the node UUID. Held until
-- COMMIT/ROLLBACK.
select pg_advisory_xact_lock(hashtextextended($1::text, 42));

-- name: ReserveFreeSlots :many
-- Atomically flips up to $3 free slots of size $2 to reserved and returns
-- them. Callers should LockNodeForReservation first so two schedulers do
-- not race on overlapping free sets.
--
-- Phase 5 ordering: prefer slots whose GPUs share an NVLink domain so
-- multi-GPU VMs land on interconnected GPUs when a choice exists. Tie-break
-- by slot_index for deterministic selection.
--
-- CTE form (rather than `id in (subquery LIMIT N FOR UPDATE)`) is required:
-- the planner can re-evaluate the IN-subquery per outer row, silently
-- breaking LIMIT and over-reserving slots. PostgreSQL materialises CTEs that
-- perform FOR UPDATE / UPDATE, so LIMIT $3 actually holds.
with picked as (
    select gs.id from gpu_slots gs
    where gs.node_id = $1 and gs.status = 'free' and gs.gpu_count = $2
    order by
        case when gs.nvlink_domain <> '' then 0 else 1 end,
        gs.nvlink_domain,
        gs.slot_index
    limit $3
    for update
)
update gpu_slots s
set status = 'reserved'
from picked
where s.id = picked.id
returning s.*;

-- name: BindSlotToInstance :one
update gpu_slots
set status              = 'in_use',
    current_instance_id = $2
where id = $1 and status = 'reserved'
returning *;

-- name: ReleaseSlotsForInstance :execrows
update gpu_slots
set status              = 'free',
    current_instance_id = null
where current_instance_id = $1;

-- name: ReleaseReservedSlots :execrows
-- Rollback helper for the scheduler: frees slots that were reserved but not
-- yet bound to an instance (e.g. CreateInstance dispatch failed).
update gpu_slots
set status              = 'free',
    current_instance_id = null
where id = any(sqlc.arg('ids')::uuid[]) and status = 'reserved';

-- name: ReleaseAllOrphanReservedSlots :execrows
-- Startup-time sweeper. Reservations are transactional-ish state held by
-- the live main-api process; on a fresh boot every slot still in 'reserved'
-- without a current_instance_id is by definition orphaned (the process that
-- reserved it died before binding). Frees them so capacity is not lost
-- across restarts. Bound (in_use) slots are left untouched.
update gpu_slots
set status              = 'free',
    current_instance_id = null
where status = 'reserved' and current_instance_id is null;

-- name: CountNonFreeSlotsForNode :one
select count(*)::bigint from gpu_slots where node_id = $1 and status <> 'free';

-- name: DeleteSlotsForNode :execrows
delete from gpu_slots where node_id = $1;

-- name: UpdateNodeProfileHash :exec
update nodes set profile_hash = $2, updated_at = now() where id = $1;
