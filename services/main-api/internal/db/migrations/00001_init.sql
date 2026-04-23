-- +goose Up
-- +goose StatementBegin

create extension if not exists "uuid-ossp";
create extension if not exists "pgcrypto";

-- --- Users ------------------------------------------------------------------
create table users (
    id            uuid primary key default gen_random_uuid(),
    email         text not null unique,
    password_hash text not null,
    is_admin      boolean not null default false,
    created_at    timestamptz not null default now(),
    updated_at    timestamptz not null default now()
);

-- --- Sessions ---------------------------------------------------------------
create table sessions (
    id          uuid primary key default gen_random_uuid(),
    user_id     uuid not null references users(id) on delete cascade,
    token_hash  text not null unique,
    expires_at  timestamptz not null,
    created_at  timestamptz not null default now()
);
create index sessions_user_id_idx on sessions(user_id);
create index sessions_expires_at_idx on sessions(expires_at);

-- --- Zones ------------------------------------------------------------------
create table zones (
    id          uuid primary key default gen_random_uuid(),
    name        text not null unique,
    description text not null default '',
    is_default  boolean not null default false,
    created_at  timestamptz not null default now()
);

-- Ensure exactly one default zone at most.
create unique index zones_single_default_idx on zones(is_default) where is_default;

-- --- Nodes ------------------------------------------------------------------
create type node_status as enum ('offline', 'online', 'degraded', 'draining');

create table nodes (
    id                uuid primary key default gen_random_uuid(),
    zone_id           uuid not null references zones(id) on delete restrict,
    node_name         text not null unique,
    hostname          text not null default '',
    agent_version     text not null default '',
    status            node_status not null default 'offline',
    topology_json     jsonb not null default '{}'::jsonb,
    profile_hash      text not null default '',
    last_heartbeat_at timestamptz,
    registered_at     timestamptz not null default now(),
    updated_at        timestamptz not null default now()
);
create index nodes_zone_id_idx on nodes(zone_id);
create index nodes_status_idx on nodes(status);

-- --- GPU profiles (per-node static layouts) --------------------------------
create table gpu_profiles (
    id          uuid primary key default gen_random_uuid(),
    node_id     uuid not null references nodes(id) on delete cascade,
    name        text not null,
    layout_json jsonb not null,
    -- Config file hash so agent and api can detect drift.
    version     text not null,
    created_at  timestamptz not null default now(),
    unique (node_id, name)
);

-- --- GPU slots --------------------------------------------------------------
create type slot_status as enum ('free', 'reserved', 'in_use', 'draining');

create table gpu_slots (
    id                  uuid primary key default gen_random_uuid(),
    node_id             uuid not null references nodes(id) on delete cascade,
    slot_index          integer not null,
    gpu_count           integer not null check (gpu_count > 0),
    gpu_indices         integer[] not null,
    nvlink_domain       text not null default '',
    status              slot_status not null default 'free',
    current_instance_id uuid,
    unique (node_id, slot_index)
);
create index gpu_slots_node_status_idx on gpu_slots(node_id, status);

-- --- Instances --------------------------------------------------------------
create type instance_state as enum (
    'pending',
    'provisioning',
    'running',
    'stopping',
    'stopped',
    'failed'
);

create table instances (
    id               uuid primary key default gen_random_uuid(),
    -- Nullable until Phase 7 (auth). Admin-created instances have null owner.
    owner_id         uuid references users(id) on delete restrict,
    node_id          uuid not null references nodes(id) on delete restrict,
    name             text not null,
    state            instance_state not null default 'pending',
    memory_mb        integer not null check (memory_mb > 0),
    vcpus            integer not null check (vcpus > 0),
    gpu_count        integer not null default 0 check (gpu_count >= 0),
    slot_indices     integer[] not null default '{}'::integer[],
    ssh_pubkeys      text[] not null default '{}'::text[],
    vm_internal_ip   inet,
    image_ref        text not null default '',
    error_message    text not null default '',
    created_at       timestamptz not null default now(),
    updated_at       timestamptz not null default now()
);
create index instances_owner_id_idx on instances(owner_id);
create index instances_node_id_idx on instances(node_id);
create index instances_state_idx on instances(state);

alter table gpu_slots
    add constraint gpu_slots_current_instance_fkey
    foreign key (current_instance_id) references instances(id) on delete set null;

-- --- Instance audit log -----------------------------------------------------
create table instance_events (
    id          bigserial primary key,
    instance_id uuid not null references instances(id) on delete cascade,
    from_state  instance_state,
    to_state    instance_state not null,
    reason      text not null default '',
    metadata    jsonb not null default '{}'::jsonb,
    created_at  timestamptz not null default now()
);
create index instance_events_instance_id_idx on instance_events(instance_id, created_at);

-- --- Credits (cached running balance) --------------------------------------
create table credits (
    user_id       uuid primary key references users(id) on delete cascade,
    balance_milli bigint not null default 0,
    updated_at    timestamptz not null default now()
);

-- --- Credit ledger (append-only) --------------------------------------------
create table credit_ledger (
    id              bigserial primary key,
    user_id         uuid not null references users(id) on delete restrict,
    delta_milli     bigint not null,
    reason          text not null,
    instance_id     uuid references instances(id) on delete set null,
    idempotency_key text not null unique,
    metadata        jsonb not null default '{}'::jsonb,
    created_at      timestamptz not null default now()
);
create index credit_ledger_user_created_idx on credit_ledger(user_id, created_at desc);

-- Block mutation of credit_ledger rows.
-- +goose StatementEnd

-- +goose StatementBegin
create function credit_ledger_immutable() returns trigger language plpgsql as $$
begin
    raise exception 'credit_ledger is append-only';
end;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
create trigger credit_ledger_no_update
    before update or delete on credit_ledger
    for each row execute function credit_ledger_immutable();
-- +goose StatementEnd

-- +goose StatementBegin
-- Seed the default zone so Phase 1 nodes have a place to land.
insert into zones (name, description, is_default)
values ('dc-seoul-1', 'default datacenter zone (seed)', true);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
drop trigger if exists credit_ledger_no_update on credit_ledger;
drop function if exists credit_ledger_immutable();
drop table if exists credit_ledger;
drop table if exists credits;
drop table if exists instance_events;
alter table if exists gpu_slots drop constraint if exists gpu_slots_current_instance_fkey;
drop table if exists instances;
drop type if exists instance_state;
drop table if exists gpu_slots;
drop type if exists slot_status;
drop table if exists gpu_profiles;
drop table if exists nodes;
drop type if exists node_status;
drop table if exists zones;
drop table if exists sessions;
drop table if exists users;
-- +goose StatementEnd
