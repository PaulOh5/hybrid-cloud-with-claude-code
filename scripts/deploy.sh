#!/usr/bin/env bash
# deploy.sh — role-aware deployment helper invoked over SSH by the GitHub
# Actions Deploy workflow on the target VM.
#
# Usage:
#   deploy.sh --role main-api      <staging-dir> <commit-sha>
#   deploy.sh --role ssh-proxy     <staging-dir> <commit-sha>
#   deploy.sh --role compute-agent <staging-dir> <commit-sha>
#
# Per-role responsibilities:
#
#   main-api:    Postgres backup → run --migrate-only on the NEW binary →
#                promote main-api + admin + frontend → restart hybrid-
#                main-api + hybrid-frontend → smoke /metrics + /login.
#
#   ssh-proxy:   Verify cert files exist → promote ssh-proxy → restart
#                hybrid-ssh-proxy → smoke TCP listen on user SSH port +
#                mux port.
#
#   compute-agent: Promote compute-agent → restart hybrid-compute-agent →
#                smoke systemd is-active.
#
# Auto-rollback: if a smoke check fails after the binary swap, the prior
# binaries (and, for main-api, the DB backup) are restored before exit.
#
# Env required (sourced from $DEPLOY_ROOT/.env.production by systemd):
#   DATABASE_URL          (main-api role)
#   MAIN_API_HTTP_ADDR    (main-api role; smoke URL derivation)
#   SSH_PROXY_LISTEN      (ssh-proxy role)
#   SSH_PROXY_MUX_LISTEN  (ssh-proxy role; optional)
#   PORT                  (main-api role; frontend Next.js, default 3000)

set -euo pipefail

ROLE=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --role)
            ROLE="$2"
            shift 2
            ;;
        --) shift; break ;;
        --*)
            echo "unknown flag: $1" >&2
            exit 2
            ;;
        *) break ;;
    esac
done

[[ "${ROLE}" == "main-api" || "${ROLE}" == "ssh-proxy" || "${ROLE}" == "compute-agent" ]] || {
    echo "ERROR: --role must be one of: main-api, ssh-proxy, compute-agent" >&2
    exit 2
}

STAGING="${1:?staging dir required}"
COMMIT_SHA="${2:?commit sha required}"

DEPLOY_ROOT="${DEPLOY_ROOT:-$HOME/hybrid-cloud}"
BIN_DIR="$DEPLOY_ROOT/bin"
WEB_DIR="$DEPLOY_ROOT/web"
DEPLOYS_DIR="$DEPLOY_ROOT/deploys"
ENV_FILE="$DEPLOY_ROOT/.env.production"
export BACKUP_DIR="${BACKUP_DIR:-$HOME/backups}"

mkdir -p "$DEPLOYS_DIR" "$BIN_DIR" "$WEB_DIR" "$BACKUP_DIR"
LOG="$DEPLOYS_DIR/$(date -u +%Y%m%dT%H%M%S)-${ROLE}-${COMMIT_SHA:0:8}.log"
exec > >(tee -a "$LOG") 2>&1

log()  { printf '[deploy:%s] %s %s\n' "$ROLE" "$(date -u +%FT%TZ)" "$*"; }
fail() { printf '[deploy:%s] FAIL %s\n' "$ROLE" "$*" >&2; exit 1; }

log "starting commit=$COMMIT_SHA staging=$STAGING log=$LOG"

[[ -r "$ENV_FILE" ]] || fail "missing $ENV_FILE"
# shellcheck source=/dev/null
set -a; source "$ENV_FILE"; set +a

# Map per-role staging artifacts and required smoke ports.
case "${ROLE}" in
    main-api)
        REQUIRED_ARTIFACTS=(main-api admin frontend.tar.gz)
        UNITS=(hybrid-main-api hybrid-frontend)
        HTTP_PORT="${MAIN_API_HTTP_ADDR##*:}"
        FRONTEND_PORT="${PORT:-3000}"
        ;;
    ssh-proxy)
        REQUIRED_ARTIFACTS=(ssh-proxy)
        UNITS=(hybrid-ssh-proxy)
        SSH_PROXY_PORT="${SSH_PROXY_LISTEN##*:}"
        MUX_PORT=""
        if [[ -n "${SSH_PROXY_MUX_LISTEN:-}" ]]; then
            MUX_PORT="${SSH_PROXY_MUX_LISTEN##*:}"
        fi
        ;;
    compute-agent)
        REQUIRED_ARTIFACTS=(compute-agent)
        UNITS=(hybrid-compute-agent)
        ;;
esac

# --- 1. PRE-FLIGHT --------------------------------------------------------

log "preflight: free disk on \$HOME"
free_kb=$(df -P "$HOME" | awk 'NR==2 {print $4}')
[[ "$free_kb" -gt 1048576 ]] || fail "less than 1GB free in \$HOME"

log "preflight: staging artifacts (${REQUIRED_ARTIFACTS[*]})"
for f in "${REQUIRED_ARTIFACTS[@]}"; do
    [[ -e "$STAGING/$f" ]] || fail "missing $STAGING/$f"
done

if [[ "${ROLE}" == "main-api" ]]; then
    log "preflight: Postgres reachable via DATABASE_URL"
    pg_isready -d "$DATABASE_URL" >/dev/null 2>&1 \
        || fail "Postgres not reachable (pg_isready failed)"
fi

if [[ "${ROLE}" == "ssh-proxy" ]]; then
    log "preflight: TLS cert + key readable"
    [[ -r "${SSH_PROXY_MUX_CERT:-/dev/null}" ]] \
        || fail "SSH_PROXY_MUX_CERT not readable: ${SSH_PROXY_MUX_CERT:-unset}"
    [[ -r "${SSH_PROXY_MUX_KEY:-/dev/null}" ]] \
        || fail "SSH_PROXY_MUX_KEY not readable: ${SSH_PROXY_MUX_KEY:-unset}"
fi

# --- 2. DB BACKUP (main-api only) ----------------------------------------

LATEST_BACKUP=""
if [[ "${ROLE}" == "main-api" ]]; then
    log "backup: scripts/backup.sh"
    "$DEPLOY_ROOT/scripts/backup.sh" || fail "backup script failed"
    LATEST_BACKUP=$(ls -t "$BACKUP_DIR"/hybrid-*.sql.gz 2>/dev/null | head -1)
    [[ -n "$LATEST_BACKUP" ]] || fail "backup produced no file"
    log "backup: $LATEST_BACKUP"
fi

# --- 3. SAVE .bak --------------------------------------------------------

log "backup binaries → .bak"
case "${ROLE}" in
    main-api)
        for svc in main-api admin; do
            [[ -x "$BIN_DIR/$svc" ]] && cp -f "$BIN_DIR/$svc" "$BIN_DIR/$svc.bak"
        done
        if [[ -d "$WEB_DIR/current" ]]; then
            rm -rf "$WEB_DIR/current.bak"
            cp -a "$WEB_DIR/current" "$WEB_DIR/current.bak"
        fi
        ;;
    ssh-proxy)
        [[ -x "$BIN_DIR/ssh-proxy" ]] && cp -f "$BIN_DIR/ssh-proxy" "$BIN_DIR/ssh-proxy.bak"
        ;;
    compute-agent)
        [[ -x "$BIN_DIR/compute-agent" ]] && cp -f "$BIN_DIR/compute-agent" "$BIN_DIR/compute-agent.bak"
        ;;
esac

# --- 4. MIGRATE (main-api only) ------------------------------------------

if [[ "${ROLE}" == "main-api" ]]; then
    log "migrate: ${STAGING}/main-api --migrate-only"
    chmod +x "$STAGING/main-api"
    "$STAGING/main-api" --migrate-only || fail "migration failed (binaries not swapped)"
fi

# --- 5. PROMOTE -----------------------------------------------------------

log "promote binaries"
case "${ROLE}" in
    main-api)
        chmod +x "$STAGING/main-api" "$STAGING/admin"
        mv -f "$STAGING/main-api" "$BIN_DIR/main-api"
        mv -f "$STAGING/admin"    "$BIN_DIR/admin"

        log "promote frontend"
        rm -rf "$WEB_DIR/current"
        mkdir -p "$WEB_DIR/current"
        tar -xzf "$STAGING/frontend.tar.gz" -C "$WEB_DIR/current"
        ;;
    ssh-proxy)
        chmod +x "$STAGING/ssh-proxy"
        mv -f "$STAGING/ssh-proxy" "$BIN_DIR/ssh-proxy"
        ;;
    compute-agent)
        chmod +x "$STAGING/compute-agent"
        mv -f "$STAGING/compute-agent" "$BIN_DIR/compute-agent"
        ;;
esac

# --- 6. RESTART + SMOKE ---------------------------------------------------

ROLLED_BACK=0
rollback() {
    [[ "$ROLLED_BACK" == 1 ]] && return
    ROLLED_BACK=1
    log "ROLLBACK: $1 — restoring .bak binaries"
    case "${ROLE}" in
        main-api)
            for svc in main-api admin; do
                [[ -x "$BIN_DIR/$svc.bak" ]] && mv -f "$BIN_DIR/$svc.bak" "$BIN_DIR/$svc"
            done
            if [[ -d "$WEB_DIR/current.bak" ]]; then
                rm -rf "$WEB_DIR/current"
                mv "$WEB_DIR/current.bak" "$WEB_DIR/current"
            fi
            if [[ -n "$LATEST_BACKUP" ]]; then
                log "ROLLBACK: restoring DB from $LATEST_BACKUP"
                "$DEPLOY_ROOT/scripts/restore.sh" "$LATEST_BACKUP" --force \
                    || log "ROLLBACK: restore.sh exited non-zero (manual recovery may be needed)"
            fi
            ;;
        ssh-proxy)
            [[ -x "$BIN_DIR/ssh-proxy.bak" ]] && mv -f "$BIN_DIR/ssh-proxy.bak" "$BIN_DIR/ssh-proxy"
            ;;
        compute-agent)
            [[ -x "$BIN_DIR/compute-agent.bak" ]] && mv -f "$BIN_DIR/compute-agent.bak" "$BIN_DIR/compute-agent"
            ;;
    esac
    for unit in "${UNITS[@]}"; do
        systemctl --user restart "$unit" 2>/dev/null \
            || log "ROLLBACK: systemctl restart $unit returned non-zero"
    done
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

case "${ROLE}" in
    main-api)
        # main-api: /metrics on the listener — server bound + prom registry wired.
        restart_and_check hybrid-main-api \
            "curl -fs http://127.0.0.1:${HTTP_PORT}/metrics" 60 \
            || rollback "main-api unhealthy"
        # frontend: Next.js standalone /login.
        restart_and_check hybrid-frontend \
            "curl -fs http://127.0.0.1:${FRONTEND_PORT}/login" 60 \
            || rollback "frontend unhealthy"
        ;;
    ssh-proxy)
        restart_and_check hybrid-ssh-proxy \
            "bash -c '</dev/tcp/127.0.0.1/${SSH_PROXY_PORT}'" 30 \
            || rollback "ssh-proxy not listening on $SSH_PROXY_PORT"
        if [[ -n "$MUX_PORT" ]]; then
            restart_and_check hybrid-ssh-proxy \
                "bash -c '</dev/tcp/127.0.0.1/${MUX_PORT}'" 5 \
                || rollback "ssh-proxy mux not listening on $MUX_PORT"
        fi
        ;;
    compute-agent)
        # compute-agent talks gRPC out, no inbound socket. Rely on systemd
        # active state; main-api side will mark it offline if heartbeat stops.
        restart_and_check hybrid-compute-agent \
            "systemctl --user is-active hybrid-compute-agent" 30 \
            || rollback "compute-agent not active"
        ;;
esac

trap - ERR

log "deploy: success role=$ROLE commit=$COMMIT_SHA log=$LOG"
