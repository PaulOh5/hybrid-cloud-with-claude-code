-- name: UpsertNode :one
insert into nodes (
    zone_id, node_name, hostname, agent_version,
    status, topology_json, last_heartbeat_at
) values (
    $1, $2, $3, $4,
    'online', $5, now()
)
on conflict (node_name) do update set
    hostname          = excluded.hostname,
    agent_version     = excluded.agent_version,
    status            = 'online',
    topology_json     = excluded.topology_json,
    last_heartbeat_at = now(),
    updated_at        = now()
returning *;

-- name: GetNode :one
select * from nodes where id = $1;

-- name: GetNodeByName :one
select * from nodes where node_name = $1;

-- name: ListNodes :many
select * from nodes order by node_name;

-- name: TouchNodeHeartbeat :exec
update nodes
set last_heartbeat_at = now(),
    status            = case when status = 'offline' then 'online' else status end,
    updated_at        = now()
where id = $1;

-- name: MarkStaleNodesOffline :execrows
update nodes
set status     = 'offline',
    updated_at = now()
where status <> 'offline'
  and (last_heartbeat_at is null or last_heartbeat_at < $1::timestamptz);

-- name: GetDefaultZone :one
select * from zones where is_default = true limit 1;
