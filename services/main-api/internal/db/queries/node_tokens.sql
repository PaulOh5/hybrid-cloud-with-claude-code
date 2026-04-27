-- Phase 2 (Bring Your Own Node) — see plan Task 0.3, ADR-009.
-- node_tokens carries the operator-issued credentials an agent uses to
-- register as a specific node. Plaintext is never stored; rows hold the
-- bcrypt hash only.

-- name: NodeTokenInsert :one
insert into node_tokens (node_id, token_hash, created_by)
values ($1, $2, $3)
returning *;

-- name: NodeTokenRevoke :exec
update node_tokens
set revoked_at = now()
where id = $1
  and revoked_at is null;

-- name: GetActiveNodeToken :one
-- Lookup used by the /internal/agent-auth handler (Task 0.4) to confirm a
-- presented hash belongs to the claimed node and has not been revoked.
select *
from node_tokens
where node_id    = $1
  and token_hash = $2
  and revoked_at is null;

-- name: ListNodeTokens :many
-- Admin CLI (Task 3.2) listing — both active and revoked, newest first.
select *
from node_tokens
where node_id = $1
order by created_at desc;

-- name: ListActiveNodeTokens :many
-- Used by the agentauth handler (Task 0.4). Returns the bcrypt hashes the
-- handler bcrypt-compares the presented plaintext token against. Filtering
-- revoked rows in SQL keeps a just-revoked credential out of the loop the
-- moment NodeTokenRevoke runs.
select *
from node_tokens
where node_id    = $1
  and revoked_at is null;
