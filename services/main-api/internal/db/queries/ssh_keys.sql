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
