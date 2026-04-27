#!/usr/bin/env bash
# bootstrap-vm.sh — one-time VM provisioning for the two-VM Phase 2 split.
#
# Usage (as root, on a fresh Ubuntu 24.04 EC2 VM with the repo cloned at
# /opt/bootstrap/hybrid-cloud or similar):
#
#     sudo AWS_ACCESS_KEY_ID=AKIA... AWS_SECRET_ACCESS_KEY=... \
#          ./scripts/bootstrap-vm.sh --role main-api
#
#     sudo AWS_ACCESS_KEY_ID=AKIA... AWS_SECRET_ACCESS_KEY=... \
#          ./scripts/bootstrap-vm.sh --role ssh-proxy
#
# Idempotent — re-run is safe, will skip already-applied steps.
#
# What it sets up (per role):
#   main-api  -> Postgres 16 + Caddy + hybrid user + systemd user units
#                + Caddyfile (qlaud.net apex + www) + AWS creds for ACME DNS-01
#   ssh-proxy -> certbot + Route53 plugin + cert-renewal post-hook
#                + hybrid user + systemd user units (ssh-proxy)
#                + /etc/hybrid/{mux-fullchain,mux-privkey}.pem symlinks
#
# Environment variables:
#   ROLE                      Set via --role; or AWS_*=... ROLE=main-api
#   AWS_ACCESS_KEY_ID         Required (Route53 DNS-01)
#   AWS_SECRET_ACCESS_KEY     Required
#   AWS_REGION                Default us-east-1 (Route53 is global)
#   ACME_EMAIL                Default ops@qlaud.net
#   POSTGRES_PASSWORD         main-api role only; defaults to a random value
#                             written into /etc/hybrid/postgres.password.
#                             Re-runs leave the existing file untouched.
#
# This script does NOT:
#   - Open security-group ports (operator does this in AWS console)
#   - Restore Postgres data (use scripts/restore.sh after first deploy)
#   - Start service units (no .env.production yet — first deploy provides it)
set -euo pipefail

ROLE=""
ACME_EMAIL="${ACME_EMAIL:-ops@qlaud.net}"
AWS_REGION="${AWS_REGION:-us-east-1}"
DEPLOY_USER="${DEPLOY_USER:-hybrid}"
DEPLOY_HOME="/home/${DEPLOY_USER}"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --role)
            ROLE="$2"
            shift 2
            ;;
        -h|--help)
            sed -n '1,/^set -euo/p' "$0" | sed -n '/^# /p'
            exit 0
            ;;
        *)
            echo "unknown arg: $1" >&2
            exit 2
            ;;
    esac
done

[[ "${ROLE}" == "main-api" || "${ROLE}" == "ssh-proxy" ]] || {
    echo "ERROR: --role must be 'main-api' or 'ssh-proxy'" >&2
    exit 2
}
[[ "$EUID" -eq 0 ]] || { echo "ERROR: must run as root" >&2; exit 2; }
[[ -n "${AWS_ACCESS_KEY_ID:-}" && -n "${AWS_SECRET_ACCESS_KEY:-}" ]] || {
    echo "ERROR: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are required (Route53 DNS-01)" >&2
    exit 2
}

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
log()  { printf '[bootstrap] %s\n' "$*"; }
warn() { printf '[bootstrap] WARN %s\n' "$*" >&2; }

# --- common: hybrid user + dirs --------------------------------------

ensure_user() {
    if ! id "${DEPLOY_USER}" &>/dev/null; then
        log "creating user ${DEPLOY_USER}"
        useradd --create-home --shell /bin/bash "${DEPLOY_USER}"
    else
        log "user ${DEPLOY_USER} already exists"
    fi
    # Linger so user-level systemd survives logout. Required for systemctl
    # --user to be reachable after the bootstrap shell exits.
    loginctl enable-linger "${DEPLOY_USER}" 2>/dev/null || true

    install -d -m 0755 -o "${DEPLOY_USER}" -g "${DEPLOY_USER}" \
        "${DEPLOY_HOME}/hybrid-cloud" \
        "${DEPLOY_HOME}/hybrid-cloud/bin" \
        "${DEPLOY_HOME}/hybrid-cloud/web" \
        "${DEPLOY_HOME}/hybrid-cloud/deploys" \
        "${DEPLOY_HOME}/hybrid-cloud/staging" \
        "${DEPLOY_HOME}/backups" \
        "${DEPLOY_HOME}/.config/systemd/user"
}

install_systemd_units() {
    local units=()
    case "${ROLE}" in
        main-api)
            units=("hybrid-main-api.service" "hybrid-frontend.service")
            ;;
        ssh-proxy)
            units=("hybrid-ssh-proxy.service")
            ;;
    esac
    for u in "${units[@]}"; do
        log "installing systemd unit ${u}"
        install -m 0644 -o "${DEPLOY_USER}" -g "${DEPLOY_USER}" \
            "${REPO_ROOT}/infra/systemd/${u}" \
            "${DEPLOY_HOME}/.config/systemd/user/${u}"
    done
    sudo -u "${DEPLOY_USER}" XDG_RUNTIME_DIR="/run/user/$(id -u "${DEPLOY_USER}")" \
        systemctl --user daemon-reload
    for u in "${units[@]}"; do
        sudo -u "${DEPLOY_USER}" XDG_RUNTIME_DIR="/run/user/$(id -u "${DEPLOY_USER}")" \
            systemctl --user enable "${u}" 2>/dev/null || true
    done
}

# --- main-api role: Postgres + Caddy ---------------------------------

bootstrap_main_api() {
    log "role=main-api: installing apt packages"
    DEBIAN_FRONTEND=noninteractive apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
        ca-certificates curl gnupg jq postgresql-16 \
        debian-keyring debian-archive-keyring apt-transport-https

    # Node.js 22 (LTS) for the Next.js standalone frontend. Ubuntu's stock
    # nodejs lags behind what Next.js 16 expects (>=20), so use NodeSource.
    if ! command -v node >/dev/null 2>&1 || ! node --version | grep -q '^v2[2-9]'; then
        log "installing Node.js 22 LTS via NodeSource"
        curl -fsSL https://deb.nodesource.com/setup_22.x | bash -
        DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nodejs
    fi

    if ! command -v caddy >/dev/null 2>&1; then
        log "installing Caddy from official apt repo"
        curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' \
            | gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
        curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' \
            > /etc/apt/sources.list.d/caddy-stable.list
        apt-get update -qq
        apt-get install -y -qq caddy
    fi

    # The cloudsmith Caddy build does NOT include the route53 DNS provider.
    # Caddy 2.7+ supports dynamic add-package which downloads xcaddy and
    # rebuilds /usr/bin/caddy with the requested module. Idempotent — the
    # command errors out if the module is already present, so we check first.
    if ! caddy list-modules 2>/dev/null | grep -q '^dns.providers.route53'; then
        log "adding caddy-dns/route53 module via 'caddy add-package'"
        caddy add-package github.com/caddy-dns/route53
    fi

    # Postgres: create role + database. Re-runs are idempotent because we
    # only INSERT if missing.
    POSTGRES_PASSWORD_FILE="/etc/hybrid/postgres.password"
    install -d -m 0750 -o root -g "${DEPLOY_USER}" /etc/hybrid
    if [[ ! -s "${POSTGRES_PASSWORD_FILE}" ]]; then
        local pw
        pw="${POSTGRES_PASSWORD:-$(openssl rand -base64 32 | tr -d '=+/' | cut -c1-32)}"
        printf '%s' "${pw}" > "${POSTGRES_PASSWORD_FILE}"
        chmod 0640 "${POSTGRES_PASSWORD_FILE}"
        chown root:"${DEPLOY_USER}" "${POSTGRES_PASSWORD_FILE}"
        log "wrote ${POSTGRES_PASSWORD_FILE} (group-readable by ${DEPLOY_USER})"
    fi
    local pgpw
    pgpw="$(cat "${POSTGRES_PASSWORD_FILE}")"

    sudo -u postgres psql -tAc \
        "SELECT 1 FROM pg_roles WHERE rolname='${DEPLOY_USER}'" | grep -q 1 \
        || sudo -u postgres psql -c \
            "CREATE ROLE ${DEPLOY_USER} LOGIN PASSWORD '${pgpw}'"
    sudo -u postgres psql -tAc \
        "SELECT 1 FROM pg_database WHERE datname='hybrid'" | grep -q 1 \
        || sudo -u postgres createdb -O "${DEPLOY_USER}" hybrid

    # Caddy: drop the main-api Caddyfile, configure Route53 creds.
    log "configuring Caddy for qlaud.net"
    install -m 0644 "${REPO_ROOT}/infra/caddy/Caddyfile.main-api" /etc/caddy/Caddyfile

    install -d -m 0755 /etc/systemd/system/caddy.service.d
    cat > /etc/systemd/system/caddy.service.d/override.conf <<EOF
[Service]
Environment=AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID}
Environment=AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY}
Environment=AWS_REGION=${AWS_REGION}
EOF
    chmod 0640 /etc/systemd/system/caddy.service.d/override.conf
    systemctl daemon-reload
    systemctl restart caddy

    log "main-api VM ready. Next: GitHub Actions deploy will drop bin/main-api,"
    log "  bin/admin, and the frontend tarball, plus .env.production."
}

# --- ssh-proxy role: certbot + DNS-01 wildcard -----------------------

bootstrap_ssh_proxy() {
    log "role=ssh-proxy: installing apt packages"
    DEBIAN_FRONTEND=noninteractive apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
        ca-certificates certbot python3-certbot-dns-route53 jq

    install -d -m 0755 /etc/hybrid

    # AWS creds for certbot's route53 plugin. Stored under /root because
    # certbot runs as root and we want the file readable only by root.
    local awsdir=/root/.aws
    install -d -m 0700 "${awsdir}"
    cat > "${awsdir}/credentials" <<EOF
[default]
aws_access_key_id = ${AWS_ACCESS_KEY_ID}
aws_secret_access_key = ${AWS_SECRET_ACCESS_KEY}
EOF
    cat > "${awsdir}/config" <<EOF
[default]
region = ${AWS_REGION}
EOF
    chmod 0600 "${awsdir}/credentials" "${awsdir}/config"

    # Obtain *.qlaud.net wildcard. Idempotent — certbot returns OK if a
    # cert already exists and isn't due for renewal.
    if [[ ! -f /etc/letsencrypt/live/qlaud.net/fullchain.pem ]]; then
        log "obtaining *.qlaud.net wildcard via Route53 DNS-01"
        certbot certonly \
            --dns-route53 \
            -d "*.qlaud.net" \
            --email "${ACME_EMAIL}" \
            --agree-tos \
            --non-interactive \
            --no-eff-email \
            --keep-until-expiring
    else
        log "wildcard cert already present at /etc/letsencrypt/live/qlaud.net/"
    fi

    # Cert files are mode 0600 root:root by default; ssh-proxy runs as
    # the hybrid user so the live/ + archive/ dirs need group-read.
    chgrp -R "${DEPLOY_USER}" /etc/letsencrypt/{live,archive,renewal} 2>/dev/null || true
    chmod -R g+rX /etc/letsencrypt/{live,archive,renewal} 2>/dev/null || true

    # Stable symlinks the ssh-proxy env file points at.
    ln -sfn /etc/letsencrypt/live/qlaud.net/fullchain.pem /etc/hybrid/mux-fullchain.pem
    ln -sfn /etc/letsencrypt/live/qlaud.net/privkey.pem   /etc/hybrid/mux-privkey.pem

    # Post-renewal hook: reload hybrid-ssh-proxy via the user bus so it
    # picks up the new cert without dropping live mux sessions any more
    # than the listener restart already does.
    install -d -m 0755 /etc/letsencrypt/renewal-hooks/post
    cat > /etc/letsencrypt/renewal-hooks/post/10-reload-ssh-proxy.sh <<EOF
#!/bin/bash
# Reach the hybrid user's systemd via XDG_RUNTIME_DIR; lingering keeps
# /run/user/<uid> alive after operator logout.
set -e
uid=\$(id -u ${DEPLOY_USER})
sudo -u ${DEPLOY_USER} XDG_RUNTIME_DIR=/run/user/\${uid} \\
    systemctl --user restart hybrid-ssh-proxy.service
EOF
    chmod 0755 /etc/letsencrypt/renewal-hooks/post/10-reload-ssh-proxy.sh

    log "ssh-proxy VM ready. Next: GitHub Actions deploy will drop bin/ssh-proxy,"
    log "  plus .env.production with SSH_PROXY_MUX_CERT=/etc/hybrid/mux-fullchain.pem"
    log "  and SSH_PROXY_MUX_KEY=/etc/hybrid/mux-privkey.pem"
}

# --- main ------------------------------------------------------------

ensure_user

case "${ROLE}" in
    main-api)
        bootstrap_main_api
        ;;
    ssh-proxy)
        bootstrap_ssh_proxy
        ;;
esac

install_systemd_units

log "bootstrap complete (role=${ROLE})"
