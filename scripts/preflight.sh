#!/usr/bin/env bash
# Pre-deploy host validation. Catches config drift / environment problems
# *before* deploy.sh starts mutating production state.
#
# Exit codes:
#   0   all critical checks passed (warnings allowed)
#   1   one or more critical failures
#
# Invoked by .github/workflows/deploy.yml's `preflight` job and safe to run
# manually on h20a:
#     bash ~/hybrid-cloud/scripts/preflight.sh
#
# DO NOT print secrets. Only print key names and lengths/booleans.

set -uo pipefail

DEPLOY_ROOT="${DEPLOY_ROOT:-$HOME/hybrid-cloud}"
ENV_FILE="$DEPLOY_ROOT/.env.production"
PG_CONTAINER="${PG_CONTAINER:-hybrid-postgres}"

errors=0
warnings=0
ok()   { printf '[ OK ] %s\n' "$*"; }
warn() { printf '[WARN] %s\n' "$*"; warnings=$((warnings+1)); }
err()  { printf '[FAIL] %s\n' "$*"; errors=$((errors+1)); }

# Show but never leak: print the key, never the value.
require_key() {
  local k="$1"
  if [[ -z "${!k:-}" ]]; then
    err "env key missing: $k"
    return 1
  fi
  ok "env key present: $k"
}

# --- 1. .env.production -----------------------------------------------------

if [[ ! -e "$ENV_FILE" ]]; then
  err ".env.production missing at $ENV_FILE"
  echo
  echo "=== preflight: $errors errors, $warnings warnings ==="
  exit 1
fi
if [[ ! -r "$ENV_FILE" ]]; then
  err ".env.production unreadable: $ENV_FILE"
  exit 1
fi
perm=$(stat -c '%a' "$ENV_FILE")
if [[ "$perm" == "600" ]]; then
  ok ".env.production mode 600"
else
  warn ".env.production mode is $perm (recommend 600 — secrets file)"
fi

# Source so subsequent checks see the values.
set -a
# shellcheck source=/dev/null
source "$ENV_FILE"
set +a

# --- 2. Required env keys ---------------------------------------------------

for k in DATABASE_URL \
         MAIN_API_ADMIN_TOKEN MAIN_API_AGENT_TOKEN \
         MAIN_API_INTERNAL_TOKEN MAIN_API_TUNNEL_SECRET \
         MAIN_API_HTTP_ADDR \
         SSH_PROXY_LISTEN SSH_PROXY_INTERNAL_TOKEN SSH_PROXY_HOST_KEY \
         AGENT_API_TOKEN AGENT_TUNNEL_SECRET \
         AGENT_PROFILE AGENT_IMAGE_DIR AGENT_SEED_DIR; do
  require_key "$k" >/dev/null 2>&1 || true
  # Re-emit OK/FAIL with output (require_key was silenced above so we control format)
  if [[ -z "${!k:-}" ]]; then
    err "env key missing: $k"
  else
    ok "env key present: $k"
  fi
done

# --- 3. Paired secrets must match -------------------------------------------

check_pair() {
  local a="$1" b="$2" reason="$3"
  if [[ -z "${!a:-}" || -z "${!b:-}" ]]; then
    return  # require_key already reported; skip pair check
  fi
  if [[ "${!a}" == "${!b}" ]]; then
    ok "$a == $b"
  else
    err "$a != $b — $reason"
  fi
}
check_pair MAIN_API_AGENT_TOKEN     AGENT_API_TOKEN          "compute-agent gRPC will be rejected"
check_pair MAIN_API_INTERNAL_TOKEN  SSH_PROXY_INTERNAL_TOKEN "ssh-proxy ticket lookups will 401"
check_pair MAIN_API_TUNNEL_SECRET   AGENT_TUNNEL_SECRET      "HMAC ticket verify will fail"

if [[ -n "${MAIN_API_TUNNEL_SECRET:-}" ]]; then
  if (( ${#MAIN_API_TUNNEL_SECRET} >= 16 )); then
    ok "MAIN_API_TUNNEL_SECRET length ${#MAIN_API_TUNNEL_SECRET} (≥16)"
  else
    err "MAIN_API_TUNNEL_SECRET length ${#MAIN_API_TUNNEL_SECRET} < 16 (main-api will refuse to start)"
  fi
fi

# --- 4. AGENT_BASE_IMAGE ----------------------------------------------------

if [[ -n "${AGENT_BASE_IMAGE:-}" ]]; then
  if [[ -r "$AGENT_BASE_IMAGE" ]]; then
    sz=$(stat -c '%s' "$AGENT_BASE_IMAGE")
    ok "AGENT_BASE_IMAGE readable ($((sz/1024/1024)) MB): $AGENT_BASE_IMAGE"
  else
    err "AGENT_BASE_IMAGE missing/unreadable: $AGENT_BASE_IMAGE"
  fi
else
  warn "AGENT_BASE_IMAGE not set — VM creation will fail at qemu-img"
fi

# --- 5. AGENT_PROFILE -------------------------------------------------------

if [[ -n "${AGENT_PROFILE:-}" && -r "$AGENT_PROFILE" ]]; then
  active=$(grep -E '^active:' "$AGENT_PROFILE" 2>/dev/null | head -1 | sed -E 's/.*"([^"]+)".*/\1/')
  if [[ -n "$active" ]]; then
    ok "AGENT_PROFILE readable, active=$active"
  else
    warn "AGENT_PROFILE readable but no active layout parsed: $AGENT_PROFILE"
  fi
else
  err "AGENT_PROFILE missing/unreadable: ${AGENT_PROFILE:-<unset>}"
fi

# --- 6. Image / seed dirs writable -----------------------------------------

for v in AGENT_IMAGE_DIR AGENT_SEED_DIR; do
  d="${!v:-}"
  if [[ -z "$d" ]]; then continue; fi
  if [[ -d "$d" && -w "$d" ]]; then
    ok "$v writable: $d"
  else
    err "$v not writable by $(id -un): $d"
  fi
done

# --- 7. ssh-proxy host key path ---------------------------------------------

hk="${SSH_PROXY_HOST_KEY:-}"
if [[ -n "$hk" ]]; then
  if [[ -e "$hk" ]]; then
    if [[ -r "$hk" && -w "$hk" ]]; then
      ok "SSH_PROXY_HOST_KEY exists and r/w by $(id -un)"
    else
      err "SSH_PROXY_HOST_KEY exists but not r/w by $(id -un): $hk"
    fi
  else
    parent=$(dirname "$hk")
    if [[ -d "$parent" && -w "$parent" ]]; then
      ok "SSH_PROXY_HOST_KEY parent dir writable (key will be generated on first start): $parent"
    else
      err "SSH_PROXY_HOST_KEY parent dir not writable by $(id -un): $parent"
    fi
  fi
fi

# --- 8. Postgres container --------------------------------------------------

if docker ps --filter "name=$PG_CONTAINER" --format '{{.Status}}' | grep -qE 'healthy|Up'; then
  ok "$PG_CONTAINER container running"
else
  err "$PG_CONTAINER container not running (deploy backup step needs it)"
fi

# --- 9. pg_dump available (host or via docker exec) ------------------------

if command -v pg_dump >/dev/null 2>&1; then
  ok "pg_dump on host PATH ($(pg_dump --version 2>&1 | head -1))"
elif docker exec "$PG_CONTAINER" pg_dump --version >/dev/null 2>&1; then
  ok "pg_dump available via docker exec $PG_CONTAINER"
else
  err "pg_dump unavailable (neither host nor $PG_CONTAINER)"
fi

# --- 10. Disk space ---------------------------------------------------------

free_home_kb=$(df -P "$HOME" | awk 'NR==2 {print $4}')
if [[ "$free_home_kb" -gt 1048576 ]]; then
  ok "\$HOME has $((free_home_kb/1024)) MB free (>1 GB)"
else
  err "\$HOME has only $((free_home_kb/1024)) MB free; deploy needs >1 GB"
fi

if [[ -d /var/lib/hybrid ]]; then
  free_var_kb=$(df -P /var/lib/hybrid | awk 'NR==2 {print $4}')
  if [[ "$free_var_kb" -gt 5242880 ]]; then
    ok "/var/lib/hybrid has $((free_var_kb/1024/1024)) GB free (>5 GB)"
  else
    warn "/var/lib/hybrid has only $((free_var_kb/1024)) MB free; per-VM disks may run out"
  fi
fi

# --- 11. systemd units installed + enabled ---------------------------------

for u in hybrid-main-api hybrid-ssh-proxy hybrid-compute-agent hybrid-frontend; do
  if systemctl --user is-enabled "$u" >/dev/null 2>&1; then
    ok "$u systemd unit enabled"
  else
    err "$u systemd unit missing or not enabled"
  fi
done

# --- 12. GPU slots free count (informational) ------------------------------

if [[ -n "${DATABASE_URL:-}" ]]; then
  free_count=$(docker exec "$PG_CONTAINER" psql -U "${POSTGRES_USER:-hybrid}" -d "${POSTGRES_DB:-hybrid}" \
    -tAc "select count(*) from gpu_slots where status='free'" 2>/dev/null | tr -d ' ')
  if [[ "$free_count" =~ ^[0-9]+$ ]]; then
    if [[ "$free_count" -gt 0 ]]; then
      ok "free GPU slots: $free_count"
    else
      warn "free GPU slots: 0 (existing instances will keep running, new ones will be rejected)"
    fi
  fi
fi

# --- 13. Suspicious dev defaults -------------------------------------------

if [[ "${AGENT_NODE_NAME:-}" == "dev-node-01" ]]; then
  warn "AGENT_NODE_NAME='dev-node-01' is the .env.example default — set a meaningful node name"
fi
if [[ "${MAIN_API_ADMIN_TOKEN:-}" == "dev-admin-token-change-me" ]]; then
  err "MAIN_API_ADMIN_TOKEN still equals the .env.example placeholder — rotate"
fi
if [[ "${MAIN_API_AGENT_TOKEN:-}" == "dev-agent-token-change-me" ]]; then
  err "MAIN_API_AGENT_TOKEN still equals the .env.example placeholder — rotate"
fi

# --- summary ----------------------------------------------------------------

echo
echo "=== preflight: $errors errors, $warnings warnings ==="
[[ "$errors" -eq 0 ]]
