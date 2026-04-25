# Runbook — Full Rollback

When to use: a fresh deploy is broken and the fastest path back to healthy
is reverting binaries + DB schema together.

## Pre-flight
- Confirm the previous binary set is still at `~/hybrid-cloud/bin/main-api.bak` (and similar `.bak` for ssh-proxy / compute-agent if applicable).
- Confirm the most recent backup is from before the bad migration:
  ```bash
  ls -lh /backups/hybrid-*.sql.gz | tail
  ```

## Steps

### 1. Stop services
```bash
ssh h20a 'pkill -f bin/main-api; pkill -f bin/ssh-proxy; pkill -f bin/compute-agent'
```

### 2. Restore DB
```bash
DATABASE_URL=postgres://hybrid:hybrid@h20a:5432/hybrid \
  ./scripts/restore.sh ./backups/hybrid-<good-date>.sql.gz --force
```
`--force` is required because the schema is non-empty. The script drops the
schema before restoring.

### 3. Roll back binaries
```bash
ssh h20a 'cd ~/hybrid-cloud/bin && cp main-api.bak main-api && cp ssh-proxy.bak ssh-proxy && cp compute-agent.bak compute-agent'
```

### 4. Bring services back in order
1. main-api → wait for `/admin/nodes` to 200.
2. ssh-proxy → verify direct-tcpip rejection (means proxy is up).
3. compute-agent → wait for `last_heartbeat_at` to refresh.

### 5. Smoke test
- Login on the dashboard.
- Try `GET /api/v1/instances` (list).
- If admin: hit `/admin/instances` and verify counts match expected.

### 6. Communicate
- Post the rollback to the operator log with timestamp + reason.
- File a follow-up issue describing what triggered the rollback.

## Don'ts
- Don't restore over a partial schema without `--force` — silent
  incompatibilities can compound.
- Don't roll back binaries WITHOUT also rolling back DB if the new release
  added a migration. New binary on old schema fails differently than old
  binary on new schema; pick one.
