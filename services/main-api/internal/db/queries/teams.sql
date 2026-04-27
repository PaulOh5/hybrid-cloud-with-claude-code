-- Phase 2.3 (Task 3.1) — team membership queries used by the ACL helper
-- and the admin CLI (Task 3.2).

-- name: TeamCreate :one
insert into teams (name, description)
values ($1, $2)
returning *;

-- name: TeamGetByName :one
select * from teams where name = $1;

-- name: TeamGet :one
select * from teams where id = $1;

-- name: TeamList :many
select * from teams order by name;

-- name: TeamMemberAdd :exec
-- Idempotent — re-adding a member is a no-op so the admin CLI can be
-- invoked safely from runbooks without a "is this already a member" probe.
insert into team_members (team_id, user_id)
values ($1, $2)
on conflict (team_id, user_id) do nothing;

-- name: TeamMemberRemove :exec
delete from team_members
where team_id = $1 and user_id = $2;

-- name: TeamMembersForTeam :many
select tm.user_id, tm.joined_at, u.email
from team_members tm
join users u on u.id = tm.user_id
where tm.team_id = $1
order by u.email;

-- name: IsUserInTeam :one
-- Used by the ACL helper. Single boolean keeps the code path narrow.
select exists(
    select 1 from team_members
    where user_id = $1 and team_id = $2
) as is_member;

-- name: TeamIDsForUser :many
-- Used by the node-list ACL filter to keep the SQL inline-friendly.
select team_id from team_members where user_id = $1;
