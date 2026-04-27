-- +goose Up
-- +goose StatementBegin

-- Phase 2 (Bring Your Own Node) — see ADR-010, ADR-011, plan Task 0.3.
--
-- Adds:
--   nodes.access_policy   — ACL toggle (Phase 2 = owner_team for beta nodes,
--                           public for Phase 1 DC nodes; Phase 3 community
--                           nodes flip back to public via admin command).
--   nodes.owner_team_id   — UUID of the team that owns the node when
--                           access_policy='owner_team'. Soft reference
--                           (no FK) — teams table is Phase 3.
--   nodes.last_data_plane_at
--                         — last yamux ping observed by ssh-proxy. The
--                           grace-period state machine (ADR-010, Phase 2.4)
--                           reads this alongside last_heartbeat_at.
--   nodes.node_state      — Phase 2 state machine value: 'online' /
--                           'degraded' / 'quarantined' / 'evicted'. Stored
--                           as text rather than a new enum so Phase 2.4 can
--                           extend the vocabulary without another DDL.
--                           Coexists with the Phase 1 status enum until
--                           Phase 2.4 consolidates.
--
-- Adds:
--   node_tokens           — operator-issued tokens granting an agent the
--                           right to register as a specific node. Hash only;
--                           the plaintext is shown to the operator once at
--                           issuance and never persisted. Revocation is via
--                           setting revoked_at; lookups must filter active.

alter table nodes
    add column access_policy      text not null default 'public'
        check (access_policy in ('owner_team', 'public')),
    add column owner_team_id      uuid,
    add column last_data_plane_at timestamptz,
    add column node_state         text not null default 'online';

-- Existing Phase 1 nodes are public DC nodes. The DEFAULT on the column
-- already pins them to 'public', but be explicit for clarity in down/up
-- cycles when rows are reapplied.
update nodes set access_policy = 'public' where access_policy is null;

-- Enforce that owner_team policy carries an owner team id. The CHECK lives
-- on the table so admin CLI / sqlc inserts cannot create the inconsistent
-- combination. Phase 3 may relax this if 'owner_team' becomes optional.
alter table nodes
    add constraint nodes_owner_team_id_required
    check (
        (access_policy = 'public')
        or (access_policy = 'owner_team' and owner_team_id is not null)
    );

create index nodes_access_policy_owner_idx
    on nodes(access_policy, owner_team_id);
create index nodes_node_state_idx on nodes(node_state);

create table node_tokens (
    id          uuid primary key default gen_random_uuid(),
    node_id     uuid not null references nodes(id) on delete cascade,
    token_hash  text not null,
    created_at  timestamptz not null default now(),
    revoked_at  timestamptz,
    -- Operator (admin user) who issued the token. Nullable for backfilled
    -- rows / legacy issuance paths; admin CLI always populates it.
    created_by  uuid references users(id) on delete set null
);
create index node_tokens_node_active_idx
    on node_tokens(node_id) where revoked_at is null;
create unique index node_tokens_node_hash_idx
    on node_tokens(node_id, token_hash);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists node_tokens;

alter table nodes drop constraint if exists nodes_owner_team_id_required;
drop index if exists nodes_access_policy_owner_idx;
drop index if exists nodes_node_state_idx;

alter table nodes
    drop column if exists node_state,
    drop column if exists last_data_plane_at,
    drop column if exists owner_team_id,
    drop column if exists access_policy;

-- +goose StatementEnd
