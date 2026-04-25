-- +goose Up
-- +goose StatementBegin

-- Phase 9 introduced an append-only trigger on credit_ledger; the original
-- schema also kept a FK from credit_ledger.instance_id to instances(id) with
-- ON DELETE SET NULL. Dropping an instance triggers an UPDATE on the
-- ledger rows that reference it — which the immutability trigger blocks,
-- making instance deletion impossible once any billing row exists.
--
-- Resolution: drop the FK and treat instance_id as a soft reference. Ledger
-- rows are append-only by design, so a dangling instance_id after deletion
-- is the correct semantic (the historical record stays intact).
alter table credit_ledger drop constraint if exists credit_ledger_instance_id_fkey;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Re-adding the FK would re-introduce the bug; the down migration is
-- documentation only.
alter table credit_ledger
    add constraint credit_ledger_instance_id_fkey
    foreign key (instance_id) references instances(id) on delete set null;
-- +goose StatementEnd
