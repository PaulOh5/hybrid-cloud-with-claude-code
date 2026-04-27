# Runbook: h20a 단일 호스트 → 두 EC2 VM 분리 마이그레이션

> 대상: 운영자 (이 작업 1회만)
> 소요: 20–30분 (DNS TTL 단축 사전 작업 제외)
> 다운타임: DNS 전파 시간 (TTL 60s)만큼
> 산출 아키텍처:
> - main-api VM (`hybrid-main-api.exe.xyz`, public DNS `qlaud.net`) → main-api + frontend + Postgres + admin CLI + Caddy
> - ssh-proxy VM (`hybrid-ssh-proxy.exe.xyz`, public DNS `*.qlaud.net`) → ssh-proxy + certbot
> - h20a → compute-agent only (GPU 호스트로만)

> ⚠ **DB는 초기화합니다.** h20a Postgres의 데이터(사용자 계정, 인스턴스 행, 세션, SSH 키, 크레딧 원장 등)는 **모두 폐기**되고 새 빈 DB로 시작합니다. Phase 2 베타 사용자 0이라 영향이 0이지만, 의도된 동작인지 컷오버 전에 다시 확인하세요. 데이터를 보존해야 하는 경우 이 런북 대신 [pg_dump/pg_restore 변형](#variant-data-preserving-cutover)을 따르세요.

이 문서는 **단방향 컷오버**입니다. DNS 전환 전까지는 새 VM이 idle이라 DNS만 안 바꾸면 위험 0. DNS 전환 후 h20a 서비스를 정지하면 그 시점부터 롤백 비용이 커집니다.

## 0. 사전 조건

- [ ] EC2 VM 두 대 프로비전 완료 (Ubuntu 24.04 LTS 권장, t3.medium 이상)
- [ ] AWS 보안 그룹:
  - main-api VM inbound: TCP/22 (SSH 운영자), TCP/80, TCP/443 (Caddy + ACME)
  - main-api VM inbound: TCP/8080 from ssh-proxy VM private IP (사설망 RPC)
  - ssh-proxy VM inbound: TCP/22 (SSH 운영자), TCP/22 → 사용자 SSH 입구라 0.0.0.0/0, TCP/8443 (mux)
- [ ] 두 VM이 같은 VPC subnet에 있고 사설 IP 통신 검증 완료
- [ ] Route53에 `qlaud.net` zone + IAM credentials (DNS-01용)
- [ ] DNS 현재 상태 (변경 전):
  - `qlaud.net` A → h20a IP
  - `*.qlaud.net` A → h20a IP
- [ ] `DEPLOY_*` GitHub secrets는 **아직 안 바꾼 상태** (h20a 가리킴)

## 1. DNS TTL 단축 (24시간 전 또는 즉시)

```
qlaud.net      A  <h20a IP>   TTL 60     (← 60초로)
*.qlaud.net    A  <h20a IP>   TTL 60
```

기본 TTL이 600s 이상이면 컷오버 직전 60s로 줄여 두기. Route53은 즉시 적용되지만 외부 캐시는 옛 TTL만큼 살아있음.

## 2. VM 부트스트랩 (1회)

각 VM에 SSH 접속 후:

```bash
# 두 VM 공통: 운영자 본인 SSH 접속용 키, AWS creds 준비
sudo apt-get update -y
sudo apt-get install -y git
sudo git clone https://github.com/PaulOh5/hybrid-cloud-with-claude-code.git /opt/bootstrap/hybrid-cloud
cd /opt/bootstrap/hybrid-cloud
```

### 2.1 main-api VM

```bash
sudo AWS_ACCESS_KEY_ID=AKIA... \
     AWS_SECRET_ACCESS_KEY=... \
     ./scripts/bootstrap-vm.sh --role main-api
```

수행 결과:
- `hybrid` 사용자 + lingering 활성
- Postgres 16 + `hybrid` role + `hybrid` database
- Caddy 설치 + `/etc/caddy/Caddyfile` (`Caddyfile.main-api`) + Route53 creds
- systemd user units (`hybrid-main-api`, `hybrid-frontend`) 설치 + enable (시작은 아직 — env 파일 없음)
- `/etc/hybrid/postgres.password` 생성

확인:
```bash
sudo systemctl status caddy           # active (running) — DNS-01 cert 발급 대기 중
sudo -u hybrid loginctl show-user hybrid | grep Linger    # Linger=yes
sudo -u postgres psql -c '\du hybrid' # role 확인
```

### 2.2 ssh-proxy VM

```bash
sudo AWS_ACCESS_KEY_ID=AKIA... \
     AWS_SECRET_ACCESS_KEY=... \
     ./scripts/bootstrap-vm.sh --role ssh-proxy
```

수행 결과:
- `hybrid` 사용자 + lingering
- certbot + Route53 plugin 설치, `*.qlaud.net` wildcard 발급
- `/etc/hybrid/mux-fullchain.pem` / `mux-privkey.pem` 심볼릭 링크
- 갱신 hook (post-renewal에서 ssh-proxy reload)
- systemd user unit (`hybrid-ssh-proxy`) 설치 + enable

확인:
```bash
sudo ls -la /etc/letsencrypt/live/qlaud.net/
sudo ls -la /etc/hybrid/mux-fullchain.pem
openssl x509 -noout -ext subjectAltName \
    < /etc/letsencrypt/live/qlaud.net/fullchain.pem
# 기대: DNS:*.qlaud.net
```

## 3. .env.production 작성 (각 VM)

부트스트랩은 env 파일을 만들지 않습니다 — 비밀 값을 운영자가 직접 작성.

### 3.1 main-api VM `~hybrid/hybrid-cloud/.env.production`

```
# Postgres (local)
DATABASE_URL=postgres://hybrid:<PG_PASSWORD>@127.0.0.1:5432/hybrid?sslmode=disable
POSTGRES_USER=hybrid
POSTGRES_DB=hybrid
POSTGRES_PASSWORD=<cat /etc/hybrid/postgres.password 결과>

# main-api
MAIN_API_HTTP_ADDR=:8080
MAIN_API_GRPC_ADDR=:8081
MAIN_API_ADMIN_TOKEN=<32+ bytes>
MAIN_API_AGENT_TOKEN=<32+ bytes — h20a compute-agent도 동일 값 사용>
MAIN_API_INTERNAL_TOKEN=<32+ bytes — ssh-proxy도 동일 값 사용>
MAIN_API_TUNNEL_SECRET=<32+ bytes>
MAIN_API_TICKET_TTL=15s
MAIN_API_HEARTBEAT_TTL=60s
MAIN_API_SWEEP_INTERVAL=10s
MAIN_API_COOKIE_SECURE=true
MAIN_API_COOKIE_DOMAIN=qlaud.net
MAIN_API_TRUSTED_PROXY_HOPS=1   # Caddy

# frontend
PORT=3000
NEXT_PUBLIC_API_URL=https://qlaud.net
MAIN_API_URL=http://127.0.0.1:8080
```

```bash
sudo chown hybrid:hybrid /home/hybrid/hybrid-cloud/.env.production
sudo chmod 600 /home/hybrid/hybrid-cloud/.env.production
```

### 3.2 ssh-proxy VM `~hybrid/hybrid-cloud/.env.production`

```
# ssh-proxy listener (외부 :22)
SSH_PROXY_LISTEN=:22
SSH_PROXY_ZONE=qlaud.net
SSH_PROXY_HOST_KEY=/home/hybrid/hybrid-cloud/ssh-proxy-hostkey

# main-api 사설망 호출 (Phase 2.0 P9 — 같은 InternalToken)
SSH_PROXY_API_ENDPOINT=http://<main-api VM 사설 IP>:8080
SSH_PROXY_INTERNAL_TOKEN=<MAIN_API_INTERNAL_TOKEN과 동일>

# Phase 2.1 mux endpoint
SSH_PROXY_MUX_LISTEN=:8443
SSH_PROXY_MUX_CERT=/etc/hybrid/mux-fullchain.pem
SSH_PROXY_MUX_KEY=/etc/hybrid/mux-privkey.pem
SSH_PROXY_METRICS_LISTEN=127.0.0.1:9092
```

`<main-api VM 사설 IP>` — AWS console에서 main-api EC2의 private IP 확인 (`10.x.y.z`).

`:22` 바인딩 — `setcap cap_net_bind_service+ep $HOME/hybrid-cloud/bin/ssh-proxy` 가 deploy 시 필요. 현재 deploy.sh는 setcap 안 함 — **operator가 첫 deploy 후 1회 수행**:
```bash
sudo setcap cap_net_bind_service+ep ~hybrid/hybrid-cloud/bin/ssh-proxy
```

## 4. GitHub Secrets 설정

Repo Settings → Environments → 3개 environment 생성:

| Environment | Secrets |
|---|---|
| `production-main-api` | `DEPLOY_HOST=hybrid-main-api.exe.xyz`, `DEPLOY_USER=hybrid`, `DEPLOY_PORT=22`, `DEPLOY_SSH_KEY=<private key>`, `DEPLOY_KNOWN_HOSTS=<ssh-keyscan output>`, `DEPLOY_PUBLIC_URL=https://qlaud.net` |
| `production-ssh-proxy` | 같은 형식 + `MUX_HOST=mux.qlaud.net`, `MUX_PORT=8443`. `DEPLOY_HOST=hybrid-ssh-proxy.exe.xyz` |
| `production-h20a` | secrets 0건 (self-hosted runner 사용) |

`DEPLOY_KNOWN_HOSTS` 생성:
```bash
ssh-keyscan -t ed25519 -p 22 hybrid-main-api.exe.xyz
ssh-keyscan -t ed25519 -p 22 hybrid-ssh-proxy.exe.xyz
```
출력을 그대로 secret value에 붙여넣기.

각 VM의 `~hybrid/.ssh/authorized_keys`에 `DEPLOY_SSH_KEY`의 public 짝 등록.

## 5. 첫 deploy (DNS는 아직 h20a 가리킴 — VM은 트래픽 0)

GitHub Actions `Deploy` 워크플로우 실행:
- Inputs: `targets=main-api,ssh-proxy` (compute-agent는 아직 h20a)
- 빌드 → main-api VM에 main-api/admin/frontend 배포 → ssh-proxy VM에 ssh-proxy 배포

각 deploy job의 external smoke 처리:
- DNS가 아직 h20a 가리키므로 `https://qlaud.net/login` 호출은 옛 환경으로 갑니다 → 통과해도 새 VM 검증 아님.
- **첫 deploy 시에만** workflow input `skip_external_smoke=true`로 설정해 외부 smoke step 건너뛰기. deploy.sh의 로컬 smoke (loopback :8080)가 새 VM 내부 동작을 검증합니다.
- DNS 전환(단계 7) 이후 평상시 deploy는 `skip_external_smoke=false` (기본값) — 외부 smoke가 새 VM의 실제 응답을 확인.

## 6. DB는 빈 상태에서 시작 — 별도 작업 없음

단계 5의 첫 deploy가 \`main-api --migrate-only\`를 실행해 빈 \`hybrid\` DB에 모든 마이그레이션을 적용합니다 (\`00001_init.sql\` ~ \`00005_phase2_teams.sql\`). 별도 데이터 이전 단계 없음.

확인 (선택):
```bash
ssh hybrid@hybrid-main-api.exe.xyz \
  "psql -U hybrid -d hybrid -c '\dt'"
# 기대: users, sessions, zones, nodes, gpu_profiles, gpu_slots,
#       instances, instance_events, credits, credit_ledger, ssh_keys,
#       node_tokens, teams, team_members, goose_db_version

ssh hybrid@hybrid-main-api.exe.xyz \
  "psql -U hybrid -d hybrid -c 'select count(*) from users'"
# 기대: 0
```

### 6.1 첫 admin 사용자 시드

DB가 비었으므로 admin 계정도 없습니다. dashboard 등록 후 SQL로 admin 권한 부여:

```bash
# 1. 브라우저에서 https://qlaud.net/register (단계 7 이후 가능)
#    또는 단계 7 전에 임시 호스트 헤더로 등록:
curl --resolve qlaud.net:443:<main-api VM public IP> \
    -X POST https://qlaud.net/api/v1/auth/register \
    -H 'Content-Type: application/json' \
    -d '{"email":"ops@qlaud.net","password":"<10자 이상 임시 비번>"}'

# 2. SQL로 is_admin=true 토글
ssh hybrid@hybrid-main-api.exe.xyz \
    "psql -U hybrid -d hybrid -c \"update users set is_admin = true where email = 'ops@qlaud.net'\""
```

### 6.2 default zone 시드 확인

\`00001_init.sql\`의 마지막 \`insert into zones\` 가 \`dc-seoul-1\` 기본 zone을 자동 생성. 확인:
```bash
ssh hybrid@hybrid-main-api.exe.xyz \
    "psql -U hybrid -d hybrid -c 'select name, is_default from zones'"
# 기대: dc-seoul-1, t
```

이후 단계 9에서 h20a compute-agent가 재기동되면 자동으로 nodes 행이 생성됩니다.

## 7. DNS 전환

Route53에서 두 A 레코드 갱신:
```
qlaud.net      A  <main-api VM public IP>   TTL 60
*.qlaud.net    A  <ssh-proxy VM public IP>  TTL 60
```

전파 확인:
```bash
dig +short qlaud.net @8.8.8.8
dig +short test-$(date +%s).qlaud.net @8.8.8.8
```

기대: 첫 줄은 main-api VM IP, 둘째 줄은 ssh-proxy VM IP.

## 8. 사용자 트래픽 검증

```bash
# 웹 + API
curl -fsI https://qlaud.net/login | head -1                     # HTTP/2 200
curl -s -o /dev/null -w '%{http_code}\n' https://qlaud.net/api/v1/instances  # 401

# mux endpoint
openssl s_client -connect mux.qlaud.net:8443 -servername mux.qlaud.net </dev/null 2>/dev/null \
    | openssl x509 -noout -ext subjectAltName | head -3
# 기대: DNS:*.qlaud.net

# (h20a compute-agent가 아직 h20a 안에서 main-api에 연결 중인지)
ssh h20a "journalctl --user -u hybrid-compute-agent -n 20 --no-pager"
# 마지막 reconnect 시도가 실패하고 있다면 다음 단계 (compute-agent endpoint 갱신)
```

## 9. h20a compute-agent endpoint 갱신

h20a의 `~hybrid/hybrid-cloud/.env.production`:

```bash
ssh h20a "sed -i.bak \
    -e 's|^AGENT_API_ENDPOINT=.*|AGENT_API_ENDPOINT=qlaud.net:443|' \
    -e 's|^AGENT_MUX_ENDPOINT=.*|AGENT_MUX_ENDPOINT=mux.qlaud.net:8443|' \
    ~/hybrid-cloud/.env.production"
ssh h20a "systemctl --user restart hybrid-compute-agent"
ssh h20a "journalctl --user -u hybrid-compute-agent -n 30 --no-pager"
```

기대 로그:
- `compute-agent starting endpoint=qlaud.net:443`
- `registered node_id=...`
- `mux endpoint configured endpoint=mux.qlaud.net:8443`
- `muxclient: attached`

## 10. h20a 서비스 정리

main-api / ssh-proxy / frontend / Caddy / Postgres 모두 비활성화. compute-agent만 유지.

```bash
ssh h20a "
  systemctl --user disable --now hybrid-main-api hybrid-ssh-proxy hybrid-frontend
  sudo systemctl disable --now caddy
  sudo systemctl disable --now docker  # 또는 hybrid-postgres 컨테이너만 stop
"
```

(h20a Postgres 데이터는 즉시 삭제하지 말고 1주 정도 보존 — 마이그 검증 후 폐기.)

## 11. 정리 + 검증

- [ ] dashboard 로그인 + 인스턴스 목록 정상
- [ ] 기존 인스턴스에 `ssh ubuntu@{prefix}.qlaud.net` 접속 라운드트립 정상 (mux 경로)
- [ ] `https://qlaud.net/admin/nodes` 에서 h20a 노드 status=online
- [ ] Phase 2 베타 노드(있으면) attach 정상
- [ ] Postgres backup이 main-api VM에서 매일 정상 (cron 아직 설정 안 했으면 추가)
- [ ] DNS TTL 600s로 복원 (운영 안정 확인 후)

## 롤백

DB 초기화 컷오버는 새 VM 쪽 데이터가 fresh start이므로 **DB 백업/복원 분기 자체가 없음** — DNS만 되돌리면 즉시 옛 환경으로 복귀합니다.

| 시점 | 롤백 비용 |
|---|---|
| 단계 5 후, DNS 전환 전 | 새 VM은 idle. DNS 안 건드리면 위험 0 |
| 단계 7 후, 단계 9 전 | DNS만 되돌림. 그동안 새 DB에 등록한 admin 계정 등은 손실 (베타 사용자 0이라 영향 0) |
| 단계 9 후 | DNS + compute-agent endpoint 둘 다 되돌림. 옛 h20a Postgres가 살아있으면 그대로 복귀 |
| 단계 10 후 | h20a 서비스 다시 enable + start 필요. ~10분 소요 |

h20a Postgres 데이터는 단계 11까지 폐기하지 않고 보존 — 만약 단계 9-10 후에도 옛 데이터를 복구해야 하면 \`docker start hybrid-postgres\` 한 번이면 됩니다.

## 향후

- preflight.sh refactor (현재 h20a 전용 — role-aware로) — Phase 2.4 들어가기 전 권장
- Postgres backup 원격 사본 (S3 등) 자동화
- Caddy 갱신 모니터링 (h20a에서는 events hook로 ssh-proxy reload — 새 환경은 certbot이 자체 cron 사용)

---

## 부록: 데이터 보존 컷오버 (variant: data-preserving cutover) {#variant-data-preserving-cutover}

기본 런북은 DB를 초기화하지만, 데이터 보존이 필요하면 단계 6을 다음으로 교체:

```bash
# 1. h20a에서 dump (다운타임 시작 — h20a main-api 정지 후)
ssh h20a "systemctl --user stop hybrid-main-api"
ssh h20a "docker exec hybrid-postgres pg_dump -U hybrid -F c hybrid" > hybrid-cutover.dump

# 2. main-api VM으로 전송
scp hybrid-cutover.dump hybrid@hybrid-main-api.exe.xyz:~/

# 3. 새 VM에서 restore. 첫 deploy의 --migrate-only 가 schema를 이미
#    만들어 둔 상태이므로 데이터만 채우려면 --data-only 사용:
ssh hybrid@hybrid-main-api.exe.xyz "
  set -e
  pg_restore -U hybrid -d hybrid --data-only --disable-triggers ~/hybrid-cutover.dump
"

# 4. 무결성 확인
ssh hybrid@hybrid-main-api.exe.xyz \
  "psql -U hybrid -d hybrid -c \"select count(*) from users; select count(*) from nodes; select count(*) from instances;\""
```

이 변형을 쓰면 다운타임이 5–10분으로 늘어납니다. 단계 6.1의 admin 시드는 건너뛰세요 (기존 admin이 dump에 포함됨).
