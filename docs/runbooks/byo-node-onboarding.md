# Runbook: BYO Node Onboarding (Phase 2)

> 대상: 운영자(operator)
> 소요: 신뢰 베타 파트너 1명 인입에 1–2시간 (precheck 포함)
> 선행 PR: #18 (Phase 2.0 Foundation), #19 (Phase 2.1 mux), #21 (Phase 2.2 mux relay)
> 관련 ADR: 008 (Split Plane), 009 (TLS+token), 011 (access policy)
> 산출물: 베타 파트너의 GPU 워크스테이션이 hybrid-cloud zone에 등재되어 owner team 멤버가 SSH 접속 가능

이 문서는 **운영자 절차**입니다. 파트너에게 보내는 외부용 가이드는 [byo-node-partner-guide.md](./byo-node-partner-guide.md).

---

## 0. 사전 준비 (1회만, 운영자 본인)

### 0.1. 환경

- `kubectl`/`ssh` 등 인프라 도구 — 없음. SQL access만 있으면 됨
- `bin/admin` 빌드: `cd /home/baro/hybrid-cloud && make build`
- DB 접근: `DATABASE_URL=postgres://...` (운영자가 직접 보유 / 또는 jump host 경유)
- mux endpoint 정보: `mux.qlaud.net:8443` (Phase 2.1 P11)

### 0.2. 운영자 점검

```bash
# admin CLI 동작 확인
DATABASE_URL=$DB_URL ./bin/admin help

# main-api / ssh-proxy 서비스 상태
ssh h20a.qlaud.net "systemctl --user status hybrid-main-api hybrid-ssh-proxy"

# Caddy + cert 상태 (mux.qlaud.net 응답 확인)
openssl s_client -connect mux.qlaud.net:8443 -servername mux.qlaud.net </dev/null 2>/dev/null \
    | openssl x509 -noout -subject -ext subjectAltName | head -3
# 기대 출력: SAN에 *.qlaud.net 포함
```

---

## 1. 파트너 정보 + owner team 등록

### 1.1. 파트너의 dashboard 계정

파트너에게 https://qlaud.net/register 에서 계정 생성을 요청. 이메일 알아두기 (`partner@example.com`).

### 1.2. 운영자 측 team 생성

```bash
DATABASE_URL=$DB_URL ./bin/admin team create \
    --name "byo-${PARTNER_NAME}" \
    --description "Phase 2 partner ${PARTNER_NAME}, ${PARTNER_HOST_DESCRIPTION}"
# 출력: team created  id=<TEAM_UUID>  name=byo-...
```

**TEAM_UUID 메모.** 다음 단계에서 사용.

### 1.3. 파트너를 team에 추가

```bash
DATABASE_URL=$DB_URL ./bin/admin team add-member \
    --team-id "$TEAM_UUID" \
    --user-email "partner@example.com"
# 출력: added user partner@example.com (<USER_UUID>) to team <TEAM_UUID>
```

운영자 본인도 트러블슈팅을 위해 team에 추가하는 것을 권장:

```bash
DATABASE_URL=$DB_URL ./bin/admin team add-member \
    --team-id "$TEAM_UUID" \
    --user-email "ops@qlaud.net"
```

---

## 2. precheck 스크립트 실행

### 2.1. 스크립트 전달

```bash
scp scripts/byo-node-precheck.sh partner@partner-host:~/
```

또는 파트너 가이드 [byo-node-partner-guide.md](./byo-node-partner-guide.md)의 다운로드 링크.

### 2.2. 파트너가 실행 → 결과를 운영자에 전송

기대 출력 (GO):

```
Result
  pass=8  warn=0  fail=0

GO — node ready for Phase 2 onboarding.
```

### 2.3. NO-GO 분기

| 블로커 | 분기 |
|---|---|
| IOMMU not active | 파트너에 BIOS VT-d/AMD-Vi 활성화 + GRUB `intel_iommu=on` 또는 `amd_iommu=on` 안내 → 재부팅 후 재실행 |
| vfio-pci 미적재 | `sudo modprobe vfio-pci` 즉시 가능, 영구화는 `/etc/modules-load.d/vfio-pci.conf` |
| 커널 < 5.15 | Ubuntu 22.04 LTS HWE 또는 24.04 LTS로 업그레이드 권장 |
| NVIDIA 드라이버 미적재 | 파트너 GPU 모델에 맞는 드라이버 설치 (Ada/Hopper는 535+) |
| outbound :8443 차단 | 가정망에선 매우 드물지만 사무실/대기업망에서 발생 가능. 파트너에 확인 + ISP/IT팀과 협의. 차단 풀 수 없으면 인입 보류 |
| 자원 부족 | RAM/디스크 확보. RAM 8 GiB / 디스크 100 GiB는 1개 인스턴스 최소치 |

전체 GO 받기 전에는 다음 단계 진행 금지.

---

## 3. node 행 사전 생성 (gRPC Register 전 준비)

베타 파트너 노드는 gRPC Register로 노드 행이 자동 생성되지만, 토큰 발급 시점에 행이 이미 있어야 admin CLI가 동작. 파트너에게 우선 agent 패키지를 설치하고 **shared `AGENT_API_TOKEN` 만으로** 1회 시동시켜 행을 생성하게 함.

또는 운영자가 직접 SQL로 placeholder 행을 추가할 수도 있으나, gRPC 경로가 더 안전 (zone_id 등 자동 처리).

```sql
-- 직접 추가 시 (선택)
insert into nodes (zone_id, node_name, hostname, agent_version, status)
select id, 'byo-userA-rtx4090', '', '0.0.0', 'offline' from zones where is_default;
```

---

## 4. 노드 토큰 발급 + ACL 적용

```bash
DATABASE_URL=$DB_URL \
SSH_PROXY_MUX_ENDPOINT=mux.qlaud.net:8443 \
./bin/admin node-token create \
    --node-name "byo-userA-rtx4090" \
    --owner-team "byo-${PARTNER_NAME}"
```

기대 출력:

```
Token created (visible once — copy it now).

  AGENT_API_TOKEN=<43자 base64url 토큰>
  AGENT_MUX_ENDPOINT=mux.qlaud.net:8443

Token id:    <TOKEN_UUID>
Node:        byo-userA-rtx4090 (<NODE_UUID>)
Owner team:  byo-userA (<TEAM_UUID>)
```

⚠ **이 토큰은 1회만 표시됩니다.** 즉시 복사 → 안전 채널로 파트너에 전달 (서명된 이메일, Signal 등).

이 명령은 동시에:
- `node_tokens` 행 추가 (bcrypt hash)
- `nodes.access_policy = 'owner_team'`
- `nodes.owner_team_id = <TEAM_UUID>`

DB 상태 확인:

```sql
select node_name, access_policy, owner_team_id from nodes where node_name = 'byo-userA-rtx4090';
-- access_policy='owner_team', owner_team_id=<TEAM_UUID>
```

---

## 5. 파트너에 안내문 전달

다음 내용을 파트너에게 (안전 채널로) 전송:

```
안녕하세요. hybrid-cloud BYO 노드 등록이 거의 완료되었습니다.
다음 두 환경변수를 agent 설정에 추가하고 서비스를 재시작해주세요.

  AGENT_API_TOKEN=<위에서 받은 토큰>
  AGENT_MUX_ENDPOINT=mux.qlaud.net:8443

자세한 절차는 첨부 가이드(byo-node-partner-guide.md)의 §3-§5를
참고해주세요.

토큰은 1회만 표시되어 분실 시 재발급이 필요합니다.
```

---

## 6. agent 부팅 + mux attach 확인

파트너 측 agent를 (재)시작하고 운영자가 모니터링:

```bash
# 6.1. ssh-proxy 측 mux 어태치 로그
ssh h20a.qlaud.net "journalctl --user -u hybrid-ssh-proxy -f -n 50" \
    | grep -i 'muxserver: agent attached\|muxserver:'

# 기대 라인:
# {"level":"info","msg":"muxserver: agent attached","node_id":"<NODE_UUID>","access_policy":"owner_team","agent_version":"..."}
```

```bash
# 6.2. main-api /admin/nodes에서 online 확인
curl -s -H "Authorization: Bearer $MAIN_API_ADMIN_TOKEN" https://qlaud.net/admin/nodes \
    | jq '.nodes[] | select(.node_name == "byo-userA-rtx4090")'
# 기대: status="online", access_policy="owner_team"
```

```bash
# 6.3. /metrics에서 mux_sessions_accepted_total 증가 확인 (운영 ssh-proxy)
ssh h20a.qlaud.net "curl -s localhost:9092/metrics | grep -E 'mux_sessions_accepted_total|mux_auth_failures_total'"
```

**5분 내 muxserver attached 로그가 안 보이면**: 파트너 agent 로그 요청, 흔한 원인:
- AGENT_MUX_ENDPOINT 오타
- 토큰 복사 실패 (개행 포함됨)
- 방화벽 outbound 차단 (precheck에서 잡혔어야 하지만 재확인)

---

## 7. 첫 인스턴스 생성 — 라운드트립 검증

운영자가 파트너 계정으로 dashboard 로그인 (또는 파트너에 시연 요청).

1. 대시보드 → Instances → New
2. Node 선택지에 `byo-userA-rtx4090` 표시 확인
3. 1×GPU 인스턴스 생성 (default profile)
4. State: pending → provisioning → running 전이 (≤ 60s)
5. Dashboard에 `ssh ubuntu@<prefix>.qlaud.net` 명령 표시
6. SSH 접속 → `nvidia-smi` 동작 확인

비-멤버(예: 운영자가 다른 계정)로 같은 노드 인스턴스 생성 시도 → **404** (S3 enumerate prevention) 확인.

---

## 8. 인입 완료 기록

운영자 노트에 다음 정보 보관:

| 필드 | 값 |
|---|---|
| 파트너 이메일 | partner@example.com |
| Node ID | `<NODE_UUID>` |
| Team ID | `<TEAM_UUID>` |
| Token ID (활성) | `<TOKEN_UUID>` |
| 인입 일시 | 2026-MM-DDTHH:MMZ |
| precheck 결과 | GO (warn=N) |
| 첫 인스턴스 라운드트립 | OK / 이슈 |

이 정보는 Phase 2.5 NPS 조사 + Phase 3 spec 인풋 (`docs/research/phase2-pilot-retro.md`)의 자료가 됩니다.

---

## 트러블슈팅 분기

| 증상 | 1차 진단 | 2차 |
|---|---|---|
| `mux.qlaud.net:8443` TLS handshake 실패 | `openssl s_client` SAN 확인 → `*.qlaud.net` 포함되어야 함 | Caddy 재발급 (PR #16 참조), `events on cert_obtained` 훅 동작 확인 |
| `mux_auth_failures_total{reason="unauthenticated"}` 증가 | 토큰 오타 또는 폐기됨 | `admin node-token list` → revoked_at NULL 확인 |
| `mux_auth_failures_total{reason="tls_downgrade"}` 증가 | 파트너 agent 버전 < Phase 2.1 | agent 재배포 |
| Dashboard에서 베타 노드 안 보임 (파트너 본인) | team 멤버십 누락 | `admin team list-members --team-id` |
| Dashboard에서 베타 노드 보이는데 인스턴스 생성 시 404 | node.access_policy 확인 | `select access_policy, owner_team_id from nodes where ...` |
| Agent 재연결 시 인스턴스 SSH 끊김 | 정책상 의도 (스펙 §7 Always) | 파트너에 재시도 안내 — Phase 3에서 재고 |

---

## 토큰 재발급 / 폐기

```bash
# 폐기
DATABASE_URL=$DB_URL ./bin/admin node-token revoke --token-id "$TOKEN_UUID"

# 활성/폐기 목록
DATABASE_URL=$DB_URL ./bin/admin node-token list --node-name "byo-userA-rtx4090"

# 새 토큰 발급 (위 §4 와 동일)
```

토큰 폐기는 main-api에서 즉시 반영, 단 ssh-proxy ↔ main-api 캐시 (60s)와 별도. **새 mux 세션 거부까지 최장 60s.**

---

## 베타 노드 폐기 (decommission)

파트너가 인입을 종료하기로 한 경우:

1. 토큰 폐기: `admin node-token revoke ...`
2. 진행 중 인스턴스 stop: dashboard 또는 admin API
3. ACL 해제 (선택): `update nodes set access_policy='public', owner_team_id=null where ...` — 또는 노드 자체 제거 정책에 따라
4. team 멤버 제거 (필요 시)

DB 행은 보존 (감사 + Phase 3 회고 자료).
