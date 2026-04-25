#!/usr/bin/env bash
# Deploys a built artifact set on the local host. Invoked over SSH by the
# `Deploy` GitHub Actions workflow after rsync uploads the staging dir.
#
# Flow:
#   1. Pre-flight (postgres up, disk space, prior services running)
#   2. DB backup via scripts/backup.sh
#   3. .bak the current binaries (mirrors docs/runbooks/full-rollback.md)
#   4. Run migrations via the NEW binary's --migrate-only mode. Abort if it
#      fails (binaries not yet swapped → no rollback needed)
#   5. Promote new binaries + frontend release dir
#   6. systemctl --user restart <unit> with smoke check between each
#   7. On any smoke failure after step 5: restore .bak binaries and DB,
#      restart the prior versions
#
# Usage: deploy.sh <staging-dir> <commit-sha>
#
# Env required (sourced from $DEPLOY_ROOT/.env.production by systemd; this
# script reads only POSTGRES_* + DATABASE_URL for backup/restore):
#   DATABASE_URL  postgres://...
#   MAIN_API_HTTP_ADDR  e.g. :8080  (used to derive smoke URL)
#   SSH_PROXY_LISTEN    e.g. :2222  (used to derive smoke port)

set -euo pipefail

STAGING="${1:?staging dir required}"
COMMIT_SHA="${2:?commit sha required}"

DEPLOY_ROOT="${DEPLOY_ROOT:-$HOME/hybrid-cloud}"
BIN_DIR="$DEPLOY_ROOT/bin"
WEB_DIR="$DEPLOY_ROOT/web"
DEPLOYS_DIR="$DEPLOY_ROOT/deploys"
ENV_FILE="$DEPLOY_ROOT/.env.production"
BACKUP_DIR="${BACKUP_DIR:-$HOME/backups}"

mkdir -p "$DEPLOYS_DIR" "$BIN_DIR" "$WEB_DIR" "$BACKUP_DIR"
LOG="$DEPLOYS_DIR/$(date -u +%Y%m%dT%H%M%S)-${COMMIT_SHA:0:8}.log"
exec > >(tee -a "$LOG") 2>&1

log()  { printf '[deploy] %s %s\n' "$(date -u +%FT%TZ)" "$*"; }
fail() { printf '[deploy] FAIL %s\n' "$*" >&2; exit 1; }

log "starting commit=$COMMIT_SHA staging=$STAGING log=$LOG"

# Source env so we can read DB creds and ports for smoke checks.
[[ -r "$ENV_FILE" ]] || fail "missing $ENV_FILE"
# shellcheck source=/dev/null
set -a; source "$ENV_FILE"; set +a

# Smoke URLs derived from env (assume the listen addrs bind on localhost too).
HTTP_PORT="${MAIN_API_HTTP_ADDR##*:}"
SSH_PROXY_PORT="${SSH_PROXY_LISTEN##*:}"
FRONTEND_PORT="${PORT:-3000}"

# --- 1. PRE-FLIGHT --------------------------------------------------------

log "preflight: docker postgres health"
docker ps --filter name=hybrid-postgres --format '{{.Status}}' | grep -q 'healthy\|Up' \
  || fail "postgres container not healthy"

log "preflight: free disk on \$HOME"
free_kb=$(df -P "$HOME" | awk 'NR==2 {print $4}')
[[ "$free_kb" -gt 1048576 ]] || fail "less than 1GB free in \$HOME"

log "preflight: staging artifacts"
for f in main-api ssh-proxy compute-agent frontend.tar.gz; do
  [[ -e "$STAGING/$f" ]] || fail "missing $STAGING/$f"
done

# --- 2. DB BACKUP ---------------------------------------------------------

log "backup: scripts/backup.sh"
"$DEPLOY_ROOT/scripts/backup.sh" || fail "backup script failed"
LATEST_BACKUP=$(ls -t "$BACKUP_DIR"/hybrid-*.sql.gz 2>/dev/null | head -1)
[[ -n "$LATEST_BACKUP" ]] || fail "backup produced no file"
log "backup: $LATEST_BACKUP"

# --- 3. SAVE .bak ---------------------------------------------------------

log "backup binaries → .bak"
for svc in main-api ssh-proxy compute-agent; do
  if [[ -x "$BIN_DIR/$svc" ]]; then
    cp -f "$BIN_DIR/$svc" "$BIN_DIR/$svc.bak"
  fi
done
if [[ -d "$WEB_DIR/current" ]]; then
  rm -rf "$WEB_DIR/current.bak"
  cp -a "$WEB_DIR/current" "$WEB_DIR/current.bak"
fi

# --- 4. MIGRATE -----------------------------------------------------------
# Run migrations from the NEW binary BEFORE swapping it in. If goose fails,
# binaries are still the old version and nothing else has changed.

log "migrate: ${STAGING}/main-api --migrate-only"
chmod +x "$STAGING/main-api"
"$STAGING/main-api" --migrate-only || fail "migration failed (binaries not swapped)"

# --- 5. PROMOTE -----------------------------------------------------------

log "promote binaries"
chmod +x "$STAGING/ssh-proxy" "$STAGING/compute-agent"
mv -f "$STAGING/main-api"       "$BIN_DIR/main-api"
mv -f "$STAGING/ssh-proxy"      "$BIN_DIR/ssh-proxy"
mv -f "$STAGING/compute-agent"  "$BIN_DIR/compute-agent"

log "promote frontend"
rm -rf "$WEB_DIR/current"
mkdir -p "$WEB_DIR/current"
tar -xzf "$STAGING/frontend.tar.gz" -C "$WEB_DIR/current"

# --- 6. RESTART + SMOKE ---------------------------------------------------
# Auto-rollback if any service fails its smoke check after the binary swap.

ROLLED_BACK=0
rollback() {
  [[ "$ROLLED_BACK" == 1 ]] && return
  ROLLED_BACK=1
  log "ROLLBACK: $1 — restoring .bak binaries and DB"
  for svc in main-api ssh-proxy compute-agent; do
    if [[ -x "$BIN_DIR/$svc.bak" ]]; then
      mv -f "$BIN_DIR/$svc.bak" "$BIN_DIR/$svc"
    fi
  done
  if [[ -d "$WEB_DIR/current.bak" ]]; then
    rm -rf "$WEB_DIR/current"
    mv "$WEB_DIR/current.bak" "$WEB_DIR/current"
  fi
  log "ROLLBACK: restoring DB from $LATEST_BACKUP"
  "$DEPLOY_ROOT/scripts/restore.sh" "$LATEST_BACKUP" --force \
    || log "ROLLBACK: restore.sh exited non-zero (manual recovery may be needed)"
  systemctl --user restart hybrid-main-api hybrid-ssh-proxy hybrid-compute-agent hybrid-frontend \
    || log "ROLLBACK: systemctl restart returned non-zero"
  fail "rollback complete due to: $1"
}
trap 'rollback "unexpected error at line $LINENO"' ERR

restart_and_check() {
  local unit="$1" check_cmd="$2" timeout="${3:-30}"
  log "restart $unit"
  systemctl --user restart "$unit"
  for ((i=1; i<=timeout; i++)); do
    if eval "$check_cmd" >/dev/null 2>&1; then
      log "$unit healthy after ${i}s"
      return 0
    fi
    sleep 1
  done
  return 1
}

# main-api: HTTP /metrics handler is on the same listener as /api → 200 means
# the server has bound + the prometheus registry is wired.
restart_and_check hybrid-main-api \
  "curl -fs http://127.0.0.1:${HTTP_PORT}/metrics" 60 \
  || rollback "main-api unhealthy"

# ssh-proxy: TCP listen check (no auth-free HTTP endpoint exposed).
restart_and_check hybrid-ssh-proxy \
  "bash -c '</dev/tcp/127.0.0.1/${SSH_PROXY_PORT}'" 30 \
  || rollback "ssh-proxy not listening on $SSH_PROXY_PORT"

# compute-agent: rely on systemd active state — agent talks gRPC out, no
# inbound socket to probe. main-api side will mark it offline if heartbeat
# stops; that is observed in the post-deploy external smoke (workflow side).
restart_and_check hybrid-compute-agent \
  "systemctl --user is-active hybrid-compute-agent" 30 \
  || rollback "compute-agent not active"

# frontend: Next.js standalone listens on $PORT (default 3000).
restart_and_check hybrid-frontend \
  "curl -fs http://127.0.0.1:${FRONTEND_PORT}/login" 60 \
  || rollback "frontend unhealthy"

# Success: drop the rollback trap so subsequent cleanup errors do not fire it.
trap - ERR

# Keep .bak around so the operator can manually revert per
# docs/runbooks/full-rollback.md within the next deploy window.
log "deploy: success commit=$COMMIT_SHA log=$LOG"
