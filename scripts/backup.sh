#!/usr/bin/env bash
# Phase 10.4 — Postgres backup helper.
#
# Streams pg_dump from the configured DATABASE_URL to a timestamped gzipped
# file in $BACKUP_DIR (default ./backups). Add this to cron for daily
# rotation; pair with a remote-copy step (rsync, S3, …) so a single host
# loss doesn't take the backup with it.
#
# Usage:
#   DATABASE_URL=postgres://… ./scripts/backup.sh
#   ./scripts/backup.sh --dry-run     # exercise connectivity without writing
#   ./scripts/backup.sh --keep-days 14
set -euo pipefail

DRY_RUN=0
KEEP_DAYS=30

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY_RUN=1; shift ;;
    --keep-days) KEEP_DAYS=$2; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

if [[ -z "${DATABASE_URL:-}" ]]; then
  echo "DATABASE_URL is required" >&2
  exit 2
fi

BACKUP_DIR="${BACKUP_DIR:-./backups}"
mkdir -p "$BACKUP_DIR"

# Prefer host pg_dump; fall back to the postgres docker container so the
# operator does not need a separate postgresql-client install on the host.
PG_CONTAINER="${PG_CONTAINER:-hybrid-postgres}"
if command -v pg_dump >/dev/null 2>&1; then
  pg_dump_via=(pg_dump)
elif command -v docker >/dev/null 2>&1 \
     && docker ps --format '{{.Names}}' | grep -qx "$PG_CONTAINER"; then
  pg_dump_via=(docker exec -i "$PG_CONTAINER" pg_dump)
else
  echo "pg_dump not on PATH and container '$PG_CONTAINER' not running" >&2
  exit 2
fi

stamp=$(date -u +"%Y-%m-%dT%H%M%SZ")
out="$BACKUP_DIR/hybrid-${stamp}.sql.gz"

if [[ $DRY_RUN -eq 1 ]]; then
  echo "[dry-run] target: $out"
  echo "[dry-run] BACKUP_DIR writable: $(test -w "$BACKUP_DIR" && echo yes || echo no)"
  echo "[dry-run] pg_dump via: ${pg_dump_via[*]}"
  # Connectivity check is best-effort: pg_isready / psql may not be
  # installed locally. Skipping it doesn't fail the dry-run — pg_dump
  # itself will surface a clear error on the real run.
  if command -v pg_isready >/dev/null 2>&1; then
    pg_isready -d "$DATABASE_URL" || echo "[dry-run] WARNING: pg_isready failed"
  elif command -v psql >/dev/null 2>&1; then
    psql "$DATABASE_URL" -c 'select 1' >/dev/null \
      || echo "[dry-run] WARNING: psql connect failed"
  else
    echo "[dry-run] postgres client tools not installed; skipping connectivity probe"
  fi
  echo "[dry-run] OK"
  exit 0
fi

echo "Backing up to $out (via ${pg_dump_via[*]})"
"${pg_dump_via[@]}" --no-owner --no-acl "$DATABASE_URL" | gzip -9 > "$out.tmp"
mv "$out.tmp" "$out"
size=$(stat -c%s "$out" 2>/dev/null || stat -f%z "$out")
echo "wrote $out ($size bytes)"

# Trim old backups.
find "$BACKUP_DIR" -maxdepth 1 -name 'hybrid-*.sql.gz' -mtime +"$KEEP_DAYS" -print -delete \
  || true
