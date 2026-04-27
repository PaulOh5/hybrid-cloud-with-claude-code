# Spec: 하이브리드 GPU 클라우드 — Phase 2 Bring Your Own Node (Beta)

> 상태: Draft v1 — 구현 착수 전 리뷰·승인 필요
> 작성일: 2026-04-26
> 기반: [docs/ideas/phase-2-bring-your-own-node.md](../ideas/phase-2-bring-your-own-node.md)
> 선행 스펙: [docs/specs/phase-1-mvp.md](./phase-1-mvp.md) — 본 스펙은 Phase 1을 **대체하지 않고** 베타 노드 인입·데이터 평면 분리만 추가
> 커버리지: Phase 2 (2–4개월, Phase 1 후반과 병행) — 신뢰 베타 5–10명 노드 인입. **노드 자가가입·정산·자동 업데이트는 Phase 3**

---

## 1. Objective

### 무엇을 만드나
신뢰된 베타 파트너 5–10명이 자기 GPU 워크스테이션에 `compute-agent`를 설치하면, **단일 아웃바운드 연결만 허용되는 가정·사무실 NAT 환경**에서도 Phase 1과 동일한 사용자 UX(`ssh ubuntu@{prefix}.hybrid-cloud.com`)로 외부 유료 사용자가 그 노드의 VM에 접속할 수 있게 한다. 노드의 인스턴스 생성 권한은 **소유자 팀(owner team)** 으로 제한하여 Phase 3 커뮤니티 풀로 가는 ACL 토글만 남기는 형태로 설계한다.

### 왜
- Phase 3(커뮤니티 노드, 단일 풀 하이브리드)의 기술·운영 리스크를 베타로 선행 흡수
- 워크스테이션 구매자에게 "내 노드를 등록 가능"이라는 수익화 옵션의 첫 신호
- Phase 1의 단일 DC 의존도를 낮춰 zone 다양성 확보 (정전·점검 시 우회 경로)

### 사용자 (Persona delta)
| 페르소나 | Phase 2에서의 역할 |
|---------|---------|
| **Beta Partner (Node Owner)** | 자기 GPU 워크스테이션에 agent 설치, 노드의 인스턴스 owner team 멤버. SSH로 운영자가 배포한 진단 스크립트 1회 실행 |
| **Internal Operator** | 노드별 토큰 발급, 베타 파트너 1:1 안내, 노드 GPU 프로파일 YAML 사전 배포, 오프라인 노드 처리 |
| **Paid User (= Phase 1 동일)** | 변동 없음. 인스턴스 생성 시 어느 zone(DC vs Beta)에 배치되는지 알 필요 없음 — 다만 Phase 2 베타 단계에선 `owner_team` ACL이 적용된 노드의 슬롯은 paid user 스케줄러가 후보에서 제외 |

### Success (상위 3개, 상세는 §8)
1. 베타 파트너 1명 이상이 자기 노드를 등록한 뒤 그 노드의 VM에 외부 유료 사용자(=owner team 멤버)가 Phase 1과 동일 UX로 SSH 접속 — 60초 이내, 추가 지연 ≤ 30ms
2. agent의 두 영속 outbound 연결(control + data) 중 어느 한쪽이 끊겨도 정의된 grace period(§8 N6) 내에 깨끗히 회복하거나 인스턴스가 자동 정리됨
3. ssh-proxy의 mux 흡수 후에도 Phase 1 N3(동시 100 SSH 세션 p95 ≤ 30ms, 드롭 ≤ 0.1%)이 회귀 없이 유지

---

## 2. Tech Stack

Phase 1 스택 그대로 + 다음 delta:

### 신규 의존성
- **`github.com/hashicorp/yamux`** — agent ↔ ssh-proxy 데이터 평면 다중화
  - 사유: per-stream flow control, keepalive 내장, Go 친화적, ssh-proxy도 Go라 단일 라이브러리. (HTTP/2 stream / SSH transport 라이브러리 검토 후 yamux 채택 — `Q3 closed`)
- **`crypto/tls` (표준)** — agent ↔ ssh-proxy 채널은 TLS 1.3 (TCP 위에 TLS 위에 yamux). 인증서: ssh-proxy는 도메인 인증서, agent는 TLS 클라이언트 인증서 미사용 → 토큰 헤더로 대체 (§ADR-009)

### Phase 1과 변경 없음
- libvirt-go, gRPC, pgx/v5, sqlc, goose, zerolog, prometheus, Next.js 15, Tailwind 등 모두 그대로

### Phase 2 ADR (계획 단계에서 제안, 구현 착수 시 `docs/adr/`에 정식 등재)
- **ADR-008** Phase 2 데이터 평면 = **Split Plane** (gRPC control on `agent → main-api` + yamux/TLS data on `agent → ssh-proxy`). 단일 채널 합치기(D1)·외부 mesh(D3/Tailscale)·신규 tunnel-server는 명시적으로 기각.
- **ADR-009** agent ↔ ssh-proxy 인증 = **TLS + Phase 1 `agent_token` 재사용**. ssh-proxy는 토큰을 직접 검증하지 않고 main-api `/internal/agent-auth`에 위임 (단일 진실 소스). mTLS는 Phase 3 진입 전 재고.
- **ADR-010** 노드 오프라인 grace period = 단계적 — heartbeat miss 30s `degraded` (관리자 알림), 90s `quarantined` (신규 SSH 거부), 300s `evicted` (인스턴스 강제 stop + 슬롯 회수). 인스턴스 자동 삭제는 Phase 2에선 안 함 (베타 신뢰 관계).
- **ADR-011** ACL 정책 레이어 = `nodes.access_policy` 컬럼 (`owner_team` | `public`). 스케줄러가 슬롯 후보 필터링에 사용. Phase 2 = 모든 베타 노드 `owner_team`. Phase 3 = 운영자 토글만 변경.
- **ADR-012** `proto/agent.proto` 의 `Register.agent_tunnel_endpoint` (필드 6) **의미 반전** — 종전 "agent가 ssh-proxy에 광고하는 자기 LAN 다이얼 주소"에서 "이 노드의 mux 세션 ID(agent가 ssh-proxy에 등록 시 알게 됨)"로 의미 변경. Phase 1 운영 사용자 0명이라 wire-level breaking change 자유.

---

## 3. Commands

Phase 1 명령은 모두 그대로 동작. 추가:

```bash
# === 운영자: 베타 노드 토큰 발급 ===
go run ./services/main-api/cmd/admin -- node-token create \
    --node-name "byo-userA-rtx4090" --owner-team "team-userA"

# 출력: agent.toml에 채워 넣을 토큰 + ssh-proxy data-plane endpoint

# === 베타 파트너: agent 첫 실행 ===
sudo systemctl edit compute-agent.service     # 환경변수 + 토큰 주입
sudo systemctl start compute-agent
journalctl -u compute-agent -f                # control + data 양 채널 attach 로그 확인

# === 운영자: 노드 사전 진단 (Phase 2 A5 검증) ===
./scripts/byo-node-precheck.sh                # IOMMU·vfio-pci·커널 체크 → 운영자에게 stdout

# === 부하 시나리오: mux 회귀 검증 ===
make e2e-mux-load                             # docker-compose.test.yml 기동 + 100 SSH 세션 분산
                                              # (50 in-DC node + 50 in-mux node)
```

---

## 4. Project Structure (Phase 1에 추가되는 부분만)

```
hybrid-cloud/
├─ services/
│  ├─ compute-agent/internal/
│  │  ├─ tunnel/                          # ⚠️ 의미 변경 — 종전 TCP 인바운드 listener
│  │  │                                   #    → outbound mux client (yamux dialer)
│  │  │                                   #    기존 server.go·verifier.go는 archive 후 신규 구현
│  │  └─ muxclient/                       # ✨ 신규 — agent → ssh-proxy yamux 세션 관리
│  │     ├─ dialer.go                     #    TLS dial + auth header + yamux session open
│  │     ├─ stream.go                     #    accept inbound streams from ssh-proxy
│  │     └─ reconnect.go                  #    backoff + clean teardown of existing instances
│  ├─ ssh-proxy/internal/
│  │  ├─ muxregistry/                     # ✨ 신규 — node_id → live yamux.Session
│  │  │  ├─ registry.go
│  │  │  └─ heartbeat.go                  #    yamux keepalive + agent disconnect
│  │  ├─ muxserver/                       # ✨ 신규 — TLS listener accepting agents
│  │  │  ├─ server.go
│  │  │  └─ auth.go                       #    delegates token check to main-api
│  │  └─ tunnelhandler/relay.go           # ⚠️ 변경 — net.Dial(tunnel_endpoint)
│  │                                      #    → muxregistry.OpenStream(node_id)
│  └─ main-api/internal/
│     ├─ node/                            # ⚠️ 추가 — access_policy, owner_team_id, last_seen_*
│     ├─ scheduler/                       # ⚠️ 추가 — ACL filter (owner_team only for byo nodes)
│     └─ agentauth/                       # ✨ 신규 — POST /internal/agent-auth (ssh-proxy → here)
├─ scripts/
│  └─ byo-node-precheck.sh                # ✨ 신규 — 운영자가 베타 노드에서 1회 실행
├─ docs/
│  ├─ specs/phase-2-bring-your-own-node.md  # 본 문서
│  └─ runbooks/byo-node-onboarding.md      # ✨ 신규
├─ infra/
│  └─ docker-compose.test.yml             # ⚠️ 추가 — 베타 노드 시뮬레이터 컨테이너
```

`internal/tunnel/`은 의미가 완전히 달라지므로 신규 디렉토리 `muxclient/`로 분리하고 기존 `tunnel/`은 PR에서 제거. 외부 import 0건이라 deprecation 윈도 없음.

---

## 5. Code Style

Phase 1과 동일. (스펙 §5 그대로 — 동일 golangci-lint, 인터페이스 소비자 지향, 컨텍스트 전파 의무, Zod 경계 검증)

**예시: muxclient의 표준 형태**

```go
// services/compute-agent/internal/muxclient/dialer.go
package muxclient

import (
    "context"
    "crypto/tls"
    "fmt"
    "net"
    "time"

    "github.com/hashicorp/yamux"
    "github.com/rs/zerolog/log"
)

// Dial opens a TLS connection to ssh-proxy's mux endpoint, authenticates the
// agent with token+node_id, and wraps the connection in a yamux client
// session. The session is the data plane: ssh-proxy opens streams *into* the
// agent for each user SSH session.
func Dial(ctx context.Context, cfg Config) (*yamux.Session, error) {
    raw, err := (&net.Dialer{Timeout: 5 * time.Second}).
        DialContext(ctx, "tcp", cfg.Endpoint)
    if err != nil {
        return nil, fmt.Errorf("dial mux endpoint: %w", err)
    }
    tlsConn := tls.Client(raw, &tls.Config{ServerName: cfg.ServerName, MinVersion: tls.VersionTLS13})
    if err := tlsConn.HandshakeContext(ctx); err != nil {
        _ = raw.Close()
        return nil, fmt.Errorf("tls handshake: %w", err)
    }

    // Auth header: one JSON line, validated by ssh-proxy via main-api before yamux init.
    if err := writeAuth(tlsConn, cfg.NodeID, cfg.AgentToken); err != nil {
        _ = tlsConn.Close()
        return nil, fmt.Errorf("auth: %w", err)
    }

    sess, err := yamux.Client(tlsConn, yamuxConfig())
    if err != nil {
        _ = tlsConn.Close()
        return nil, fmt.Errorf("yamux client: %w", err)
    }
    log.Info().Str("node_id", cfg.NodeID).Msg("data plane attached")
    return sess, nil
}
```

---

## 6. Testing Strategy

Phase 1 전략 유지 + 다음 추가:

### 신규 단위 테스트
- `muxclient.Dial` — TLS 핸드셰이크 실패, 인증 거부, yamux init 실패 각각 분리된 에러 반환 (테이블 드리븐)
- `muxregistry` — 동시 노드 등록·삭제·재등록 race-free, ghost session(이전 reconnect 잔재) 자동 정리
- `agentauth` HTTP 핸들러 — 토큰 누락/형식 오류/만료/취소된 노드 각각 다른 에러

### 신규 통합 테스트 (`//go:build integration`)
- 베타 노드 시뮬레이터 컨테이너 ↔ ssh-proxy ↔ main-api 셋의 실 도커 네트워크에서 SSH 라운드트립
- agent kill→reconnect 시 in-flight 사용자 세션 정리 + 신규 SSH는 30s 내 정상 라우팅 (ADR-010 검증)
- 한 yamux 세션에서 1 stream이 `dd` 큰 전송 + 9 stream 인터랙티브 동시 → 인터랙티브 stream 지연 회귀 없음 (one-pager A3)

### Mandatory Prove-It (Phase 2 추가)
다음 경로는 통합 테스트 없이는 머지 금지:
- `muxclient` ↔ `muxserver` 정상·비정상 종료 시 양쪽 cleanup 순서
- ssh-proxy `tunnelhandler.Relay`의 mux 경로(node_id→stream open) 와 Phase 1 직접 dial 경로 동시 회귀
- `scheduler` 의 ACL 필터 — `access_policy=owner_team` 노드 슬롯이 비-owner 사용자 스케줄링 후보에 절대 들어가지 않음

### 부하 회귀 (Phase 1 N3 보존 검증)
`make e2e-mux-load`: 100 동시 SSH 세션을 50/50으로 (Phase 1 직접 dial 노드 50 + Phase 2 mux 노드 50) 분산. p95 추가 지연 측정값을 PR 본문에 첨부.

---

## 7. Boundaries

Phase 1 §7의 모든 규칙 유지 + Phase 2 추가:

### Always do (Phase 2 추가)
- Phase 1 ssh-proxy ↔ agent 직접 dial 경로의 회귀 테스트가 Phase 2 PR마다 통과
- `agent_token` 폐기는 main-api DB 단일 소스에서 — ssh-proxy 측 캐시 TTL ≤ 60s
- agent 재연결 시 in-flight 사용자 SSH 세션은 **명시적으로 끊고** 사용자에게 재시도 안내 (이행 보장 안 함, 정책)
- 베타 노드의 GPU 분할 프로파일 YAML은 Phase 1과 동일 정적 프로파일 — agent가 hash와 함께 Register에 첨부

### Ask first (Phase 2 추가)
- **mux 라이브러리 교체** (yamux → 다른 것) — ADR-008 재검토 필요
- **agent ↔ ssh-proxy 인증 방식 변경** (토큰 → mTLS 등) — ADR-009 재검토 필요
- **agent 자동 업데이트** 메커니즘 도입 시도 (Phase 2 out-of-scope, Phase 3 결정 사항)
- **ACL 정책 추가** — `owner_team` / `public` 외 새 정책 (예: `whitelist`, `pay-per-use`) 도입
- **노드 자가가입** 플로우 (운영자 1:1 인입을 자동화)

### Never do (Phase 2 추가)
- Phase 2 단계에서 베타 노드를 paid public user 스케줄링에 노출 — ACL 토글로 분명히 분리
- 단일 gRPC 채널에 SSH 데이터를 합치는 변형(D1) — control/data HoL blocking 시한폭탄 (ADR-008)
- 별도 tunnel-server·gateway 서비스 신설 (사용자 명시 거부)
- WireGuard / Tailscale / SSH-in-SSH 제안 — 일괄 기각된 안 (one-pager §Not Doing)
- 베타 노드 운영자 토큰을 git·로그·이슈에 첨부

---

## 8. Success Criteria

Phase 2가 "완료"되려면 **전부** 충족:

### 기능 (Functional)
- [ ] **F1. 베타 노드 인입 E2E:** 운영자가 노드 토큰 발급 → 파트너가 agent 설치·기동 → 5분 내 main-api `nodes` 테이블에 `online` + `access_policy=owner_team` 으로 등재
- [ ] **F2. 베타 노드 SSH 접속:** owner team 멤버가 베타 노드의 VM에 `ssh {prefix}.hybrid-cloud.com` 으로 Phase 1과 동일 UX 접속, 60초 이내 성공
- [ ] **F3. ACL 격리:** owner team 외 사용자가 베타 노드 슬롯을 스케줄링하려 하면 명확한 403/404. 베타 노드는 일반 인스턴스 생성 폼의 zone 옵션에 노출되지 않음
- [ ] **F4. mux 동시 흐름:** 한 yamux 세션이 ≥10 동시 SSH stream을 처리, 큰 stream이 작은 stream을 starve하지 않음 (one-pager A3)
- [ ] **F5. 데이터 평면 단절·복구:** ssh-proxy 측 mux endpoint 재시작 시 agent가 자동 재연결, 새 SSH 요청은 ≤ 30s 내 정상 라우팅. 진행 중 SSH는 끊김 (ADR-010)

### 비기능 (Non-functional)
- [ ] **N1. 단일 outbound 제약 충족:** agent 노드의 outbound 연결은 main-api(:443) + ssh-proxy mux(:8443) 두 종류. 다른 outbound 0건 (`ss -tnp` / iptables count 검증). `:8443`은 Caddy가 :443을 점유해 ssh-proxy를 별도 외부 포트로 둔 결정 (plan.md P11). 가정망 NAT는 :8443 outbound 일반 허용; 사무실/대기업망에서 차단 가능성은 precheck 스크립트로 사전 점검
- [ ] **N2. Phase 1 N3 회귀 없음:** mux 흡수 후 동시 100 SSH 세션 부하에서 p95 추가 지연 ≤ 30ms, 드롭 ≤ 0.1% (Phase 1 N3 동일 기준)
- [ ] **N3. Control/data 분리 입증:** main-api에 SSH 바이트 트래픽 0 — main-api 컨테이너 인터페이스 캡처에 사용자 SSH 페이로드 미관찰 (ADR-008 invariant)
- [ ] **N4. 한국 가정망 24h 생존:** KT/SKB/LG 가정망 베타 노드 1대 이상에서 두 outbound 연결이 24h 단절 0회 (사용자 병렬 검증, A1)
- [ ] **N5. mux 흐름 제어 적합:** 1 stream `dd` 1GB + 9 stream 인터랙티브 echo round-trip — 인터랙티브 RTT p95 ≤ 60ms (one-pager A3)
- [ ] **N6. Grace period 동작:** heartbeat miss 30s/90s/300s 각각에서 노드 상태 `degraded`/`quarantined`/`evicted`로 전환, 인스턴스는 `quarantined`에서 신규 SSH 거부, `evicted`에서 force stop + 슬롯 회수 (ADR-010)

### 보안 (Security)
- [ ] **S1. agent ↔ ssh-proxy TLS:** TLS 1.3 강제, 다운그레이드 거부 (test: TLS 1.2 클라이언트 거부 확인)
- [ ] **S2. 토큰 검증 단일 소스:** ssh-proxy 캐시 TTL ≤ 60s, main-api에서 토큰 폐기 후 ≤ 60s 내 신규 mux 세션 거부
- [ ] **S3. 베타 노드 운영자 권한:** Beta Partner는 자기 노드의 호스트 root이지만 main-api·ssh-proxy 측에서 **다른 노드의 ticket 위조·도청 불가** (테스트: 한 노드의 agent_token으로 다른 node_id 등록 시도 → 거부)
- [ ] **S4. ACL 우회 방지:** 스케줄러 ACL 우회 경로 0건 — DB·API·관리자 콘솔 모든 슬롯 할당 경로에서 `access_policy` 검사 통과 후에만 reservation

### 가정 검증 (Assumption Closure)
- [ ] **A1. 한국 가정망 keepalive·NAT 24h+ 생존** — 사용자 책임, **개발 블로커 아님**. 베타 1대로 24h 데이터 1회 이상 (one-pager A1)
- [ ] **A2. mux 흡수 후 N3 보존** — N2와 동일 게이트
- [ ] **A3. yamux flow control 적합성** — N5와 동일 게이트
- [ ] **A4. agent 재연결 시 in-flight 정책 충분성** — 베타 1주 운영 시 NPS ≥ 7 (one-pager A4)
- [ ] **A5. 베타 노드 환경 원격 사전 점검** — `byo-node-precheck.sh` 작성 + 베타 1명에 시도, 운영자 1시간 이내 노드 인증

### 운영 (Operational Readiness)
- [ ] **O1. 런북:** `docs/runbooks/byo-node-onboarding.md` (운영자 절차) + `byo-node-offline.md` (오프라인 노드 처리)
- [ ] **O2. 모니터링:** Grafana 대시보드 — node별 mux 세션 상태, 활성 stream 수, control/data 채널 RTT, agent_version 분포
- [ ] **O3. 토큰 발급·폐기 절차:** 운영자 admin CLI + 감사 로그 `node_tokens` 테이블에 발급/폐기 기록
- [ ] **O4. 베타 파트너 안내문 v1:** 설치 절차, 정책(재연결 시 진행 SSH 끊김), 신고 채널

---

## 9. Open Questions

| # | 질문 | 결정 필요 시점 | 비고 |
|---|------|--------------|------|
| Q1 | agent 노드의 동시 인스턴스 한계 — mux 세션당 stream 상한을 몇으로 둘 것인가 | 부하 테스트 설계 시 | 단일 노드 GPU 4개 = 최대 4 SSH 세션 보통이라 충분히 여유. 안전마진 결정 |
| Q2 | agent 자동 업데이트 — 베타 5명 수동이지만 모델은 어디서 결정? | Phase 3 spec 진입 전 | Phase 2 out-of-scope. Phase 3 spec에서 결정 |
| Q3 | 베타 파트너의 노드 GPU 프로파일 변경 시 절차 — 운영자가 SSH로 변경 vs 파트너 self-serve | Phase 2 후반 | 자기 노드인데 운영자만 변경 가능한 건 운영 마찰. 베타 1주 후 결정 |
| Q4 | 베타 단계 수익 정산 — 일단 무료 vs 기여도 기록만 | 베타 시작 전 | 정산 코드는 Phase 3. 기록만 하는 옵션? |
| Q5 | ssh-proxy SPOF — Phase 3 수십~수백 노드 시 수평 확장 모델 | Phase 3 spec 진입 전 | sticky session by node_id? consistent hash? Phase 2 not in scope |
| Q6 | mux 세션의 keepalive 주기 — 한국 가정망 NAT 평균 타임아웃에 맞춰야 함 | A1 24h 데이터 수령 후 | yamux 기본 30s. KT 일부 라우터 60s 이상 idle drop 보고 있음 |

---

## 10. 리뷰·승인

구현 착수 전 체크:
- [ ] 본 스펙 6개 코어 영역(Objective / Stack+Commands / Structure / Code Style / Testing / Boundaries) 커버
- [ ] Success Criteria가 구체적·테스트 가능
- [ ] Boundaries (Phase 1 inherited + Phase 2 추가) 명확
- [ ] ADR-008 ~ ADR-012 5건이 `docs/adr/` 에 정식 등재
- [ ] 사용자(프로젝트 오너) 리뷰·승인

다음 Phase로 이행:
- Plan: 본 스펙 → 모듈 의존성·구현 순서·병렬화 가능성 → `/agent-skills:plan`
- Tasks: 구현 단위 작업 분해 (mux 채널 → ACL → grace period → 부하 회귀 순서 권장)
- Implement: TDD 기반 단계 실행 → `/agent-skills:build`
