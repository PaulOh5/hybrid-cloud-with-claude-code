-- name: InsertCreditLedgerEntry :one
-- 원장 entry 추가. unique(idempotency_key) 충돌은 caller가 23505로 처리.
insert into credit_ledger (
    user_id, delta_milli, reason, instance_id, idempotency_key, metadata
) values ($1, $2, $3, $4, $5, $6)
returning *;

-- name: UpsertCredits :exec
-- 잔액 캐시 누적 갱신. 새 사용자면 row 생성.
insert into credits (user_id, balance_milli)
values ($1, $2)
on conflict (user_id) do update
set balance_milli = credits.balance_milli + excluded.balance_milli,
    updated_at    = now();

-- name: GetCredits :one
-- 사용자 잔액 row. 없으면 pgx.ErrNoRows.
select * from credits where user_id = $1;

-- name: ListCreditLedgerEntries :many
select * from credit_ledger
where user_id = $1
order by created_at desc, id desc
limit $2;

-- name: ListBillableRunningInstances :many
-- billing worker가 매 tick에 호출. owner_id 가 null인 admin/test 인스턴스는
-- 청구 대상에서 제외. updated_at 은 어떤 시점부터 청구할지 계산용 (state
-- 가 running 으로 바뀐 시각 ~).
select id, owner_id, gpu_count, updated_at
from instances
where state = 'running' and owner_id is not null;

-- name: ListUsersWithNegativeBalance :many
-- 잔액 ≤ 0 사용자 — 9.3 게이트가 stop dispatch 대상으로 사용.
select user_id from credits where balance_milli <= 0;
