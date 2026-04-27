# Runbook — Deploying to production (h20a)

Phase 1 ships from main to a single host (`h20a` / qlaud.net) via the `Deploy`
GitHub Actions workflow. The workflow is **manual** (`workflow_dispatch`) by
design — there is no staging environment yet, so the operator is the gate.

## TL;DR — happy-path deploy

1. Confirm CI is green for the SHA you want to ship: `gh run list --workflow=ci.yml --branch main`
2. Trigger the workflow:
   ```
   gh workflow run deploy.yml
   ```
   …or via the GitHub UI: **Actions → Deploy → Run workflow** (default = main HEAD).
3. Watch the run. The `deploy.sh` step on h20a prints to `~/hybrid-cloud/deploys/<timestamp>-<sha>.log` — tail it if you want more detail than the GitHub stream.
4. After "external smoke OK", spot-check the dashboard at <https://qlaud.net> and `/admin/nodes`.

## What the pipeline does

```
verify-ci → build → rsync to h20a/staging → deploy.sh on h20a → external smoke
```

`scripts/deploy.sh` is the on-host entry point. It:

1. Pre-flight checks (postgres healthy, ≥1 GB free, all four artifacts present)
2. DB backup via `scripts/backup.sh`
3. Saves `.bak` of every binary and the current frontend dir (mirrors `full-rollback.md`)
4. **Runs migrations from the new binary using `--migrate-only`** — if migrations fail, binaries are *not* swapped and the pre-deploy state is intact (no rollback needed)
5. Promotes the new binaries + frontend
6. `systemctl --user restart` each service in dependency order, with a smoke check between each
7. **Auto-rollback** if any post-promote smoke check fails: restores `.bak` binaries, restores DB from the backup taken in step 2, restarts services

Smoke checks per service:
- `main-api` — `curl /metrics` (proves HTTP listener bound + Prometheus registry wired)
- `ssh-proxy` — TCP listen on `SSH_PROXY_LISTEN`
- `compute-agent` — `systemctl is-active` (no inbound port; offline marker watched externally)
- `frontend` — `curl /login`

External smoke (from the GitHub runner, after on-host deploy returns):
- `GET https://qlaud.net/login` → 200
- `GET https://qlaud.net/api/v1/instances` → 401 (auth middleware is active)

## One-time setup on h20a

Skip this section once it has been done on the host.

### 1. Deploy user

A non-root user owns `~/hybrid-cloud`. Add it to `libvirt` and (if running ssh-proxy on a privileged port) grant `CAP_NET_BIND_SERVICE` to the binary. Enable lingering so user-level systemd starts on boot without an interactive login:

```bash
sudo useradd -m -s /bin/bash hybrid     # if it does not already exist
sudo usermod -aG libvirt,docker hybrid
sudo loginctl enable-linger hybrid
```

### 2. Layout

As the deploy user:

```
~/hybrid-cloud/
├── bin/                # binaries — populated by deploys
├── web/current/        # Next.js standalone — populated by deploys
├── deploys/            # per-deploy log files
├── staging/            # transient upload dirs (auto-created per deploy)
├── scripts/            # checked out from this repo (deploy.sh, backup.sh, restore.sh)
└── .env.production     # secrets — NEVER committed; created manually
```

`scripts/` and `infra/` are checked out from the repo:

```bash
git clone https://github.com/<org>/<repo>.git ~/hybrid-cloud-src
ln -s ~/hybrid-cloud-src/scripts ~/hybrid-cloud/scripts
ln -s ~/hybrid-cloud-src/infra   ~/hybrid-cloud/infra
```

`git pull` in `~/hybrid-cloud-src` keeps deploy logic + runbooks current. The
deploy workflow does **not** ship `deploy.sh` itself — changing deploy logic
mid-flight would be a foot-gun.

### 3. `.env.production`

Copy `.env.example` to `~/hybrid-cloud/.env.production` and fill real values:

- Strong `MAIN_API_ADMIN_TOKEN`, `MAIN_API_AGENT_TOKEN`, `SSH_PROXY_INTERNAL_TOKEN`
- Real `DATABASE_URL` (pointing at the postgres docker container)
- `AGENT_BASE_IMAGE`, `AGENT_IMAGE_DIR`, `AGENT_SEED_DIR` for the libvirt host
- `PORT=3000` for the frontend (the systemd unit reads it)

Permissions: `chmod 600 ~/hybrid-cloud/.env.production`.

### 4. systemd units

```bash
mkdir -p ~/.config/systemd/user
cp ~/hybrid-cloud/infra/systemd/hybrid-*.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable hybrid-main-api hybrid-ssh-proxy hybrid-compute-agent hybrid-frontend
```

(Do not `start` yet — the first deploy will populate `bin/` and bring them
up via `systemctl restart`.)

### 5. GitHub repo secrets

Set under **Settings → Secrets and variables → Actions**:

| Secret | Value |
| --- | --- |
| `DEPLOY_HOST` | e.g. `h20a.qlaud.net` |
| `DEPLOY_USER` | e.g. `hybrid` |
| `DEPLOY_SSH_PORT` | e.g. `12831` (h20a does not listen on 22) |
| `DEPLOY_SSH_KEY` | private key (ed25519) — public half goes in `~hybrid/.ssh/authorized_keys` on h20a |
| `DEPLOY_KNOWN_HOSTS` | output of `ssh-keyscan -p 12831 -t ed25519 h20a.qlaud.net` (pin the host key — the bracketed `[host]:port` format must match the port used at connect time) |
| `DEPLOY_PUBLIC_URL` | e.g. `https://qlaud.net` |

The deploy key should be dedicated to CD (not your personal key). Consider
restricting it on h20a with `command=`/`from=` in `authorized_keys` if the
deploy user has shell access for other purposes.

### 6. TLS edge (Caddy + Route53 DNS-01)

`main-api` (`:8080`) and the frontend (`:3000`) speak plain HTTP on loopback;
TLS termination + reverse proxy lives in Caddy on the host. h20a's firewall
geo-blocks foreign IPs on `:80`/`:443`, so HTTP-01 / TLS-ALPN-01 cannot
complete — Let's Encrypt validators come from rotating non-KR vantage points.
We use the **DNS-01** challenge against Route53 (where `qlaud.net` is hosted),
which only needs outbound HTTPS to the AWS API and is unaffected by the
inbound geo-fence.

#### One-time install

1. **Build a Caddy with the route53 plugin.** The apt-shipped `/usr/bin/caddy`
   has no DNS plugins. Build a parallel binary in `/usr/local/bin/caddy` and
   leave the apt package alone (so apt upgrades don't fight us):
   ```bash
   sudo apt install -y golang-go
   go install github.com/caddyserver/xcaddy/cmd/xcaddy@latest
   ~/go/bin/xcaddy build \
     --with github.com/caddy-dns/route53 \
     --output /tmp/caddy
   sudo install -m 0755 -o root -g root /tmp/caddy /usr/local/bin/caddy
   /usr/local/bin/caddy list-modules | grep route53   # sanity check
   ```

2. **Drop the repo Caddyfile and systemd override into place.** From a
   `~/hybrid-cloud-src` checkout:
   ```bash
   sudo install -m 0644 -o root -g root infra/caddy/Caddyfile /etc/caddy/Caddyfile
   sudo mkdir -p /etc/systemd/system/caddy.service.d
   sudo install -m 0644 -o root -g root \
     infra/caddy/caddy.service.d/override.conf \
     /etc/systemd/system/caddy.service.d/override.conf
   sudo systemctl daemon-reload
   ```

3. **Create an IAM user for the ACME client** with the policy in
   `infra/caddy/route53-policy.json` (replace the zone ID placeholder with
   the qlaud.net hosted zone ID). Issue a long-lived access key for that
   user — Caddy needs it persistently for renewals.

4. **Inject the credentials**:
   ```bash
   sudo install -m 0600 -o root -g root /dev/stdin /etc/caddy/caddy.env <<'EOF'
   AWS_ACCESS_KEY_ID=AKIA...
   AWS_SECRET_ACCESS_KEY=...
   AWS_REGION=us-east-1
   EOF
   ```

5. **Restart and watch the first issuance**:
   ```bash
   sudo systemctl restart caddy
   sudo journalctl -u caddy -f
   # expect "certificate obtained successfully" within ~60s
   sudo ls /var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory/qlaud.net/
   ```

6. **Verify externally** (from any KR network):
   ```bash
   curl -sI https://qlaud.net/login | head -1   # expect HTTP/2 200
   curl -s -o /dev/null -w '%{http_code}\n' https://qlaud.net/api/v1/instances   # expect 401
   openssl s_client -connect qlaud.net:443 -servername qlaud.net </dev/null 2>/dev/null \
     | openssl x509 -noout -issuer -dates
   ```

#### Renewal

Caddy renews automatically (~30 days before expiry) using the same Route53
credentials. No cron, no manual intervention. To verify the next renewal
date:
```bash
curl -s localhost:2019/pki/ca/local/certificates 2>/dev/null   # admin API (loopback)
sudo journalctl -u caddy --since '7 days ago' | grep -iE 'renew|obtain'
```

#### Backup

The `/var/lib/caddy/` tree contains the LE account key and certificate
material. Back it up alongside the Postgres backups so a host rebuild
doesn't force a fresh ACME account / re-issuance:
```bash
sudo tar -C / -czf caddy-state.tgz var/lib/caddy
```

#### Rollback

If a Caddyfile change breaks routing, the apt binary at `/usr/bin/caddy`
plus the previous Caddyfile are still on disk. Either:
- `sudo systemctl edit caddy --force` and revert the override (drops back to
  apt binary + previous /etc/caddy/Caddyfile), or
- restore the prior `/etc/caddy/Caddyfile` and `sudo systemctl reload caddy`.

The certificate state under `/var/lib/caddy/` is unaffected by Caddyfile
edits — restarts/reloads do not re-issue.

### 7. GitHub Environment (recommended)

Create a `production` environment under **Settings → Environments** with at
least one required reviewer. The deploy job already references
`environment: production` so the workflow will pause for approval before the
SSH steps run.

## When something goes wrong

The pipeline already auto-rolls-back inside `deploy.sh` when a service fails
its smoke check. But auto-rollback only fires while the script is running;
problems detected later are manual.

| Symptom | Action |
| --- | --- |
| Workflow red at `verify-ci` | Wait for CI to pass, or fix the broken commit |
| Workflow red at `Run deploy.sh` | Read the on-host log at `~/hybrid-cloud/deploys/*.log`. Auto-rollback should have completed; if it failed mid-rollback, follow [full-rollback.md](./full-rollback.md) |
| Workflow green but app misbehaving | Manual rollback via [full-rollback.md](./full-rollback.md). `.bak` binaries are kept until the next deploy |
| `--migrate-only` failed (workflow red at "migrate" step in `deploy.sh`) | Schema is at the version the failing migration started from. New binaries are NOT yet promoted. Fix the migration in a follow-up commit and re-deploy. No rollback needed |

`.bak` files are deliberately preserved across the deploy — the next deploy
overwrites them. Once you observe the new release is healthy, you can ignore
them; if you spot a problem within the same deploy window, restore via
[full-rollback.md](./full-rollback.md).

## Phase 2 evolution

When the system grows beyond a single host, this pipeline needs:

- A **staging** environment that gets every main merge automatically; prod
  remains manual but only deployable from a SHA that staging has soaked.
- **Compute-agent fleet rollout** as a separate workflow (per-node SSH or
  agent self-update). The current pipeline only ships agent to h20a.
- **Image-based deploys** (GHCR) for main-api / ssh-proxy / frontend. The
  agent likely stays as a host binary because of libvirt.
