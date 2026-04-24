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
update gpu_slots s
set status = 'reserved'
where s.id in (
    select inner_s.id from gpu_slots inner_s
    where inner_s.node_id = $1 and inner_s.status = 'free' and inner_s.gpu_count = $2
    order by inner_s.slot_index
    limit $3
    for update
)
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

-- name: CountNonFreeSlotsForNode :one
select count(*)::bigint from gpu_slots where node_id = $1 and status <> 'free';

-- name: DeleteSlotsForNode :execrows
delete from gpu_slots where node_id = $1;

-- name: UpdateNodeProfileHash :exec
update nodes set profile_hash = $2, updated_at = now() where id = $1;
