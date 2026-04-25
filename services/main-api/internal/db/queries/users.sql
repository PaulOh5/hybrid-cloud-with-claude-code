-- name: CreateUser :one
insert into users (email, password_hash, is_admin)
values ($1, $2, $3)
returning *;

-- name: GetUserByEmail :one
select * from users where email = $1;

-- name: GetUser :one
select * from users where id = $1;

-- name: CreateSession :one
insert into sessions (user_id, token_hash, expires_at)
values ($1, $2, $3)
returning *;

-- name: GetSessionByTokenHash :one
select * from sessions
where token_hash = $1
  and expires_at > now();

-- name: DeleteSessionByTokenHash :exec
delete from sessions where token_hash = $1;

-- name: DeleteExpiredSessions :execrows
delete from sessions where expires_at <= now();
