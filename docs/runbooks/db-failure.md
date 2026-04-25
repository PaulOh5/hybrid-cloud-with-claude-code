# Runbook — Database failure

## Detect
- main-api log spammed with `pq: connection refused` / `context deadline exceeded`.
- All `/api/v1/*` endpoints return 500.
- Grafana shows API 5xx rate alert firing.

## Stabilise
1. The dashboard is dark — there's no in-flight write to lose. Don't
   force-rebuild yet.
2. If the DB is healthy but slow, raise the connection-pool size before
   touching the DB itself.

## Diagnose
1. Check the postgres container:
   ```bash
   ssh h20a 'docker ps --filter name=hybrid-postgres --format "{{.Status}}"'
   ssh h20a 'docker logs --tail 100 hybrid-postgres'
   ```
2. Common signals:
   - `disk full` → expand volume / purge `/var/lib/postgresql/data/pg_wal`.
   - `out of memory` → raise container limit, drop concurrent workers.
   - WAL corruption → restore from backup (next section).

## Recover
### Soft restart
```bash
ssh h20a 'docker restart hybrid-postgres'
ssh h20a 'docker exec hybrid-postgres pg_isready -U hybrid -d hybrid'
ssh h20a 'pkill -f bin/main-api; sleep 1; nohup ~/hybrid-cloud/run-api.sh > ~/hybrid-cloud/logs/main-api.log 2>&1 & disown'
```

### Restore from backup
```bash
# On the operator machine
DATABASE_URL=postgres://hybrid:hybrid@h20a:5432/hybrid ./scripts/restore.sh ./backups/hybrid-2026-04-25.sql.gz
```
See [`scripts/restore.sh`](../../scripts/restore.sh) for safety guards (it
refuses to overwrite a non-empty DB without `--force`).

## Post-mortem
- Note the approximate data loss window (gap between last good backup and incident).
- If WAL was lost: switch to streaming replication for the next iteration.
- If disk full was the cause: add an alert on `node_filesystem_avail_bytes`.
