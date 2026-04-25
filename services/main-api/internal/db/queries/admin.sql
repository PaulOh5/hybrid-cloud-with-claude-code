-- name: ListUsersAdminView :many
-- Phase 10.1 admin dashboard: users + cached balance + active instance count.
-- Active = not in terminal state (stopped/failed) so admins see "currently in
-- use" without seeing the deletion graveyard.
select
    u.id,
    u.email,
    u.is_admin,
    u.created_at,
    coalesce(c.balance_milli, 0)::bigint as balance_milli,
    (
        select count(*)
        from instances i
        where i.owner_id = u.id and i.state in ('pending', 'provisioning', 'running', 'stopping')
    )::bigint as active_instance_count
from users u
left join credits c on c.user_id = u.id
order by u.created_at desc
limit $1;

-- name: ListSlotsForAdminView :many
-- Slot view across all nodes — admin needs the global picture, not per-node.
select
    s.id,
    s.node_id,
    n.node_name,
    s.slot_index,
    s.gpu_count,
    s.gpu_indices,
    s.nvlink_domain,
    s.status,
    s.current_instance_id
from gpu_slots s
join nodes n on n.id = s.node_id
order by n.node_name, s.slot_index;
