-- +goose Up
-- +goose StatementBegin

-- Phase 2.3 (Task 3.1) — teams + team_members.
--
-- The Phase 2 spec uses "owner team" terminology for ACL on beta nodes.
-- Task 0.3 added nodes.owner_team_id as a soft uuid reference but did not
-- introduce the table the reference points at; this migration closes that
-- gap. owner_team_id stays a soft reference (no FK) so a deleted team
-- does not cascade-delete a node row that an operator may want to reassign.
--
-- Membership is intentionally simple — no roles, no nesting. Phase 3 may
-- extend this; until then a user is either in a team or not.

create table teams (
    id          uuid primary key default gen_random_uuid(),
    name        text not null unique,
    description text not null default '',
    created_at  timestamptz not null default now()
);

create table team_members (
    team_id   uuid not null references teams(id) on delete cascade,
    user_id   uuid not null references users(id) on delete cascade,
    joined_at timestamptz not null default now(),
    primary key (team_id, user_id)
);
create index team_members_user_id_idx on team_members(user_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

drop table if exists team_members;
drop table if exists teams;

-- +goose StatementEnd
