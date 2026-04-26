-- name: CreateSSHKey :one
insert into ssh_keys (user_id, label, pubkey, fingerprint)
values ($1, $2, $3, $4)
returning *;

-- name: ListSSHKeysForUser :many
select * from ssh_keys
where user_id = $1
order by created_at desc;

-- name: DeleteSSHKeyForUser :execrows
delete from ssh_keys
where id = $1 and user_id = $2;

-- name: GetSSHKeyForUser :one
select * from ssh_keys
where id = $1 and user_id = $2;

-- name: LookupSSHKeyByFingerprint :one
-- ssh-proxy authenticates a user by SSH key fingerprint. Returns the row so
-- the caller can scope subsequent lookups by owner_id without the fingerprint
-- being globally unique (we still trust the (user_id, fingerprint) unique
-- constraint per the schema).
select * from ssh_keys
where fingerprint = $1
limit 1;
