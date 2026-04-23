-- name: CreateInstance :one
insert into instances (
    owner_id, node_id, name, state,
    memory_mb, vcpus, gpu_count, slot_indices,
    ssh_pubkeys, image_ref
) values (
    $1, $2, $3, 'pending',
    $4, $5, $6, $7,
    $8, $9
)
returning *;

-- name: GetInstance :one
select * from instances where id = $1;

-- name: ListInstances :many
select * from instances
where (sqlc.narg('owner_id')::uuid is null or owner_id = sqlc.narg('owner_id'))
order by created_at desc;

-- name: UpdateInstanceState :one
update instances
set state          = $2,
    error_message  = coalesce(sqlc.narg('error_message')::text, error_message),
    vm_internal_ip = coalesce(sqlc.narg('vm_internal_ip')::inet, vm_internal_ip),
    updated_at     = now()
where id = $1
returning *;

-- name: DeleteInstance :exec
delete from instances where id = $1;

-- name: InsertInstanceEvent :exec
insert into instance_events (instance_id, from_state, to_state, reason, metadata)
values ($1, $2, $3, $4, $5);

-- name: ListInstanceEvents :many
select * from instance_events
where instance_id = $1
order by created_at;
