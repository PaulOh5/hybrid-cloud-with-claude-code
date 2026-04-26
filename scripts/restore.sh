#!/usr/bin/env bash
# Phase 10.4 — Postgres restore helper.
#
# Restores a gzipped pg_dump artifact (as produced by scripts/backup.sh)
# into the database pointed to by DATABASE_URL. Refuses to overwrite a
# non-empty database without --force; this keeps an accidental restore
# from clobbering live data.
#
# Usage:
#   DATABASE_URL=postgres://… ./scripts/restore.sh ./backups/hybrid-….sql.gz
#   ./scripts/restore.sh ./backups/hybrid-….sql.gz --force
#   ./scripts/restore.sh ./backups/hybrid-….sql.gz --dry-run
set -euo pipefail

FORCE=0
DRY_RUN=0
ARCHIVE=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --force) FORCE=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    -*) echo "unknown flag: $1" >&2; exit 2 ;;
    *) ARCHIVE="$1"; shift ;;
  esac
done

if [[ -z "${DATABASE_URL:-}" ]]; then
  echo "DATABASE_URL is required" >&2
  exit 2
fi
if [[ -z "$ARCHIVE" ]]; then
  echo "usage: restore.sh <archive.sql.gz> [--force] [--dry-run]" >&2
  exit 2
fi
if [[ ! -f "$ARCHIVE" ]]; then
  echo "archive not found: $ARCHIVE" >&2
  exit 2
fi

# Prefer host psql; fall back to the postgres docker container.
PG_CONTAINER="${PG_CONTAINER:-hybrid-postgres}"
if command -v psql >/dev/null 2>&1; then
  psql_via=(psql)
elif command -v docker >/dev/null 2>&1 \
     && docker ps --format '{{.Names}}' | grep -qx "$PG_CONTAINER"; then
  psql_via=(docker exec -i "$PG_CONTAINER" psql)
else
  echo "psql not on PATH and container '$PG_CONTAINER' not running" >&2
  exit 4
fi

if [[ $DRY_RUN -eq 1 ]]; then
  echo "[dry-run] would restore $ARCHIVE → $DATABASE_URL"
  echo "[dry-run] psql via: ${psql_via[*]}"
  echo "[dry-run] gunzip -t check…"
  gunzip -t "$ARCHIVE"
  echo "[dry-run] OK"
  exit 0
fi

# Refuse to overwrite a non-empty schema unless --force is set.
table_count=$("${psql_via[@]}" "$DATABASE_URL" -tAc "select count(*) from pg_tables where schemaname='public'")
table_count="${table_count//[[:space:]]/}"
if [[ "$table_count" -gt 0 && $FORCE -eq 0 ]]; then
  cat >&2 <<EOF
target database has $table_count tables in 'public' — refusing to overwrite.
re-run with --force to drop the schema before restoring.
EOF
  exit 3
fi

if [[ $FORCE -eq 1 && "$table_count" -gt 0 ]]; then
  echo "dropping public schema (force)…"
  "${psql_via[@]}" "$DATABASE_URL" -c "drop schema public cascade; create schema public;"
fi

echo "restoring $ARCHIVE → $DATABASE_URL (via ${psql_via[*]})"
gunzip -c "$ARCHIVE" | "${psql_via[@]}" "$DATABASE_URL" --set ON_ERROR_STOP=1 >/dev/null
echo "restore complete"
