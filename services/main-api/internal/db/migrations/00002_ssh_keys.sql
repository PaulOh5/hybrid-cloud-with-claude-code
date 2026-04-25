-- +goose Up
-- +goose StatementBegin

create table ssh_keys (
    id          uuid primary key default gen_random_uuid(),
    user_id     uuid not null references users(id) on delete cascade,
    label       text not null,
    pubkey      text not null,
    -- SHA-256 fingerprint of the parsed pubkey body (base64). Used to
    -- detect duplicates without storing the full key twice.
    fingerprint text not null,
    created_at  timestamptz not null default now(),
    unique (user_id, fingerprint)
);
create index ssh_keys_user_id_idx on ssh_keys(user_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
drop table if exists ssh_keys;
-- +goose StatementEnd
