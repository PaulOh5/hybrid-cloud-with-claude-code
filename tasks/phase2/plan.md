# Implementation Plan: Phase 2 — Bring Your Own Node (Beta)

> 작성일: 2026-04-26
> 기반 스펙: [docs/specs/phase-2-bring-your-own-node.md](../../docs/specs/phase-2-bring-your-own-node.md)
> 선행: Phase 1 MVP (`tasks/phase1/plan.md`) 의 Phase 1 ~ Phase 6 (특히 Phase 6 SSH 프록시) 완료 가정
> 상태: Draft v1 — 인간 리뷰·승인 대기

---

## Overview

Phase 1 SSH 프록시의 데이터 평면을 **agent → ssh-proxy yamux/TLS 채널**로 분리하고, 베타 파트너 5–10명의 GPU 워크스테이션을 단일 outbound 제약 환경에서 인입한다. 신규 서비스 0개, 데이터 평면은 ssh-proxy에 흡수, ACL은 `owner_team`/`public` 토글로 Phase 3 호환.

5개 페이즈 · 19개 태스크 · 4개 게이트로 분해. 핵심 리스크는 **Phase 2.4의 Phase 1 N3 회귀**(mux 흡수 후 100 SSH 세션 p95 ≤ 30ms 보존) 와 **Phase 4의 베타 1명 실 인입 후 24h NAT 생존(A1)** — 둘 다 게이트로 명시.

### 의도된 실행 순서의 근거
1. **하부 데이터 평면 우선**: mux 채널이 동작해야 ACL·운영 코드가 의미 있다. Phase 2.1–2.5에 집중.
2. **Phase 1 회귀 무결성**: 매 PR마다 Phase 1 직접 dial 경로 회귀 테스트가 필수. mux 코드는 별도 패키지에 격리, tunnelhandler 분기점만 한 줄.
3. **베타 인입은 마지막**: 운영 절차·런북·모니터링이 갖춰진 뒤에야 실 파트너 인입. Phase 4에서 1명만 시작.
4. **A1(NAT 24h+ 생존)은 사용자 병행 검증**: 개발 블로커 아님. 베타 1명에 한해 데이터 수집만.

---

## Architecture Decisions (스펙 §2에서 확정, 구현 착수 시 `docs/adr/`에 등재)

- **ADR-008** 데이터 평면 = Split Plane (gRPC control + yamux/TLS data on ssh-proxy). 단일 채널·외부 mesh·신규 서비스 모두 기각.
- **ADR-009** agent ↔ ssh-proxy 인증 = TLS 1.3 + Phase 1 `agent_token` 재사용. 검증은 ssh-proxy → main-api `/internal/agent-auth` 위임 (단일 진실 소스).
- **ADR-010** 노드 오프라인 grace period = 30s `degraded` / 90s `quarantined` (신규 SSH 거부) / 300s `evicted` (인스턴스 force stop + 슬롯 회수). 인스턴스 자동 삭제는 안 함.
- **ADR-011** ACL 정책 레이어 = `nodes.access_policy ∈ {owner_team, public}`. 스케줄러 슬롯 후보 필터 단계에서 검사.
- **ADR-012** `proto/agent.proto` `Register.agent_tunnel_endpoint` (필드 6) 의미 반전 — 기존 "agent LAN dial 주소" → "ssh-proxy mux endpoint advertised back to agent". Phase 1 외부 사용자 0이라 wire-level breaking change 자유.

---

## Dependency Graph

```
[Phase 1 MVP completed]
        │
        ▼
[Phase 2.0: Foundation — ADR · proto · DB · /internal/agent-auth]
        │
        ▼
[Phase 2.1: Mux Channel — agent muxclient ↔ ssh-proxy muxserver]
        │
        ▼
[Phase 2.2: Mux Relay — tunnelhandler 분기 (mux vs Phase 1 direct)]
        │
        ▼   (mux SSH path works E2E)
        │
        ├─► [Phase 2.3: ACL & Onboarding] ──┐
        │           (owner_team isolation,   │
        │            admin CLI, precheck)    │
        │                                    │
        ├─► [Phase 2.4: Grace & Ops] ──── ◄──┤
        │           ✱ N2/N3 GATE             │
        │           (state machine, monitoring,
        │            mux load regression)    │
        │                                    │
        └─► [Phase 2.5: Beta Pilot] ◄────────┘
                    ✱ A1/A4 GATE
                    (1명 실 인입, 24h NAT, NPS)
```

✱ = 게이트. 실패 시 후속 진입 금지.

---

## Task List

> 사이즈: XS (1 파일) · S (1–2) · M (3–5) · L (5–8, 분해 검토).

---

### Phase 2.0: Foundation

> 목표: 데이터 평면 변경에 필요한 계약·DB·ADR 정비. 코드 동작 변화 없음 (proto/DB만 갱신, 새 경로 미사용).

#### Task 0.1: ADR-008 ~ ADR-012 정식 등재
**Description:** 스펙 §2의 5개 결정 사항을 `docs/adr/` 형식 문서로 등재. 기각된 안(D1/D3/E/C/별도 tunnel-server)을 반드시 ADR-008 내 "Considered Alternatives"로 명시 — 향후 재제안 시 이 문서로 차단.

**Acceptance:**
- [ ] `docs/adr/008-phase2-split-plane-data-plane.md`
- [ ] `docs/adr/009-agent-ssh-proxy-tls-token-auth.md`
- [ ] `docs/adr/010-node-offline-grace-periods.md`
- [ ] `docs/adr/011-node-access-policy-acl.md`
- [ ] `docs/adr/012-proto-agent-tunnel-endpoint-semantic-flip.md`
- [ ] 각 ADR에 `Status: Accepted (Phase 2)`, 결정 일자, 기각 대안 표

**Verification:** PR diff 리뷰 — 5건 모두 머지

**Dependencies:** None
**Files (~5):** `docs/adr/008..012-*.md`
**Size:** S

---

#### Task 0.2: proto 의미 반전 + 신규 메시지
**Description:** `proto/agent.proto` `Register.agent_tunnel_endpoint` 필드 주석 변경 + 신규 필드 추가:
- `Register.mux_session_id` (선택, agent가 ssh-proxy에 mux 등록 직후 control plane에 보고)
- `Heartbeat.agent_version`은 이미 있다면 그대로, 없다면 추가
- 변경 사항 wire-level이라도 main-api·compute-agent의 모든 사용처 일괄 갱신

**Acceptance:**
- [ ] `proto/agent.proto` 갱신 + `make proto` 재실행 시 idempotent diff 0
- [ ] Go 스텁 `services/*/internal/pb/` 갱신
- [ ] 종전 필드 사용처는 `// TODO(Phase 2.2): replace` 마크 후 컴파일은 유지

**Verification:** `make proto && make build && make test` 녹색 — 의미는 미사용 상태

**Dependencies:** 0.1
**Files (~3):** `proto/agent.proto`, 사용처 표시
**Size:** S

---

#### Task 0.3: DB 마이그 — `nodes.access_policy`, `node_tokens`
**Description:** Goose 마이그레이션 1개 파일에:
- `nodes` 테이블에 `access_policy text NOT NULL DEFAULT 'public'`, `owner_team_id uuid NULL`, `last_data_plane_at timestamptz NULL`, `node_state text NOT NULL DEFAULT 'online'` 추가
- `node_tokens (id, node_id, token_hash, created_at, revoked_at, created_by)` 신규 (운영자 발급 감사용)
- Phase 1 기존 노드는 `access_policy='public'`로 마이그 (기존 DC 노드는 Phase 1과 동일 동작)

**Acceptance:**
- [ ] `make migrate-up && make migrate-down && make migrate-up` 깨끗
- [ ] sqlc generate 통과, 신규 쿼리 `NodeAccessPolicy`, `NodeTokenInsert`, `NodeTokenRevoke` 생성
- [ ] Phase 1 통합 테스트 회귀 없음

**Verification:** schema 통합 테스트 `node_access_policy_test.go` — 마이그 적용 후 기본값 검증

**Dependencies:** 0.1
**Files (~4):** 마이그 1개, 쿼리 SQL, sqlc 생성물 갱신, 테스트
**Size:** M

---

#### Task 0.4: main-api `/internal/agent-auth` 엔드포인트
**Description:** ssh-proxy가 mux 핸드셰이크 시 호출할 내부 엔드포인트. `POST /internal/agent-auth {node_id, token}` → 200 if 토큰이 해당 node_id의 활성 토큰과 일치, else 401. 60s TTL 캐싱 권장(스펙 S2).

**P9 결정 (2026-04-27):** Phase 1 `MAIN_API_INTERNAL_TOKEN` 그대로 재사용. 별도 토큰·mTLS 없음. 이유: ssh-proxy·main-api 동일 신뢰 경계, Phase 1 일관성, Phase 2 규모(베타 5–10)에 mTLS 과대.

**사전 검증 (Task 착수 전):**
- [ ] main-api `/internal/*` 라우팅이 public 포트(`:8080`)가 아닌 internal 포트에만 노출되는지 확인. 노출돼 있으면 internal 포트 분리부터 (별도 small task로 분리).

**Acceptance:**
- [ ] 엔드포인트는 internal token으로 인증 (Phase 1 `/internal/ssh-ticket` 동일 미들웨어 재사용 — 코드 path 동일)
- [ ] 응답 페이로드: `{ok, node_id, owner_team_id, access_policy, agent_version_seen}`
- [ ] 폐기된 토큰(`revoked_at IS NOT NULL`) 거부
- [ ] `MAIN_API_INTERNAL_TOKEN` 미설정 또는 < 32 bytes 시 main-api 부팅 실패 (Phase 1 동일 정책 확인)

**Verification:** 단위 + 통합 — 만료·폐기·잘못된 형식 각각 401

**Dependencies:** 0.3
**Files (~3):** `services/main-api/internal/agentauth/*.go`, 라우터 등록, 테스트
**Size:** S

---

**Checkpoint 0 — Foundation**
- [ ] `make lint && make test && make build` 녹색
- [ ] Phase 1 통합 테스트 100% 회귀 없음 (직접 dial SSH 경로 정상)
- [ ] 5개 ADR 머지
- [ ] 인간 리뷰

---

### Phase 2.1: Mux Channel Plumbing

> 목표: agent ↔ ssh-proxy 사이에 yamux/TLS 데이터 채널이 attach되고 keepalive가 유지된다. **아직 사용자 SSH 트래픽은 안 흐름** — 채널만.

#### Task 1.1: ssh-proxy `muxserver` — TLS listener + 인증
**Description:** ssh-proxy의 mux endpoint(`mux.qlaud.net:8443`, P11 결정)에서 TLS 1.3 리스닝. agent가 연결하면 헤더 한 줄(JSON: `{node_id, token, agent_version}`)을 받아 main-api `/internal/agent-auth`에 검증 위임. 성공 시 yamux Server 세션 시작.

**P10 결정 (2026-04-27) — yamux Config 공통값** (muxserver·muxclient 양쪽 동일):
```go
yamux.Config{
    KeepAliveInterval:      15 * time.Second,  // 한국 가정망 NAT 마진
    StreamOpenTimeout:      30 * time.Second,  // 75s 디폴트 단축
    EnableKeepAlive:        true,
    MaxStreamWindowSize:    256 * 1024,        // 디폴트
    ConnectionWriteTimeout: 10 * time.Second,  // 디폴트
    StreamCloseTimeout:     5 * time.Minute,   // 디폴트
    AcceptBacklog:          256,               // 디폴트
}
```

**P11 결정 (2026-04-27, 갱신 2026-04-27 PM):** mux endpoint = `mux.qlaud.net:8443`. 초기 결정의 `:443`은 Caddy(웹 사이트)가 이미 점유하고 있어 충돌 — 옵션 (b) "ssh-proxy를 별도 외부 포트(:8443)" 채택. 옵션 (a) Caddy L4 SNI 라우팅 대비 운영 단순함 우선 (Caddy 재빌드 불필요). PR 머지 전 운영 측 준비:
- [ ] DNS A 레코드 `mux.qlaud.net` → ssh-proxy 호스트 IP
- [ ] TLS cert (`*.qlaud.net` wildcard) Caddy가 발급, ssh-proxy는 파일시스템 공유 (caddy 그룹 멤버십)
- [ ] LB/방화벽 inbound 0.0.0.0/0 → ssh-proxy:8443 allow
- [ ] Phase 2.3 Task 3.3 precheck 스크립트가 `nc -zv mux.qlaud.net 8443` 검증 추가 — 사무실/대기업망에서 :8443 outbound 차단 가능성 사전 점검

**Acceptance:**
- [ ] `services/ssh-proxy/internal/muxserver/server.go` — `Serve(ctx, lis, deps)` 시그니처
- [ ] yamux Config 위 P10 값 사용, 공유 헬퍼 함수 `yamuxConfig()` 한 곳에서 정의 (server·client 동일 import)
- [ ] 인증 실패 시 즉시 close + warn 로그 + Prometheus `mux_auth_failures_total{reason}`
- [ ] TLS 1.2 다운그레이드 거부 (S1)
- [ ] 인증 결과 60s 캐싱 (S2)

**Verification:** 단위 — 정상/만료/폐기/TLS1.2 4가지 케이스 테이블 드리븐. `mux_auth_failures_total{reason="tls_downgrade"}` 증가 확인

**Dependencies:** 0.4
**Files (~4):** `internal/muxserver/server.go`, `auth.go`, 테스트, 라우터/cmd 와이어링
**Size:** M

---

#### Task 1.2: ssh-proxy `muxregistry` — node_id → session
**Description:** mux 세션을 node_id로 인덱싱하는 in-memory 레지스트리. 재연결 시 기존 세션은 깨끗히 close + 신규 등록 (ghost session 방지). yamux keepalive 사용 — 30s 기본, 한국 가정망 NAT 상황에 맞춰 설정 가능 (Q6).

**Acceptance:**
- [ ] `Register(node_id, sess) → prevSess` (있으면 close)
- [ ] `OpenStream(ctx, node_id) → (yamux.Stream, err)` — 미등록·dead 시 즉시 에러
- [ ] yamux ping 실패 시 자동 deregister + main-api에 `node_state=degraded` 보고
- [ ] race-free: registry 락 정리, table-driven concurrent test

**Verification:** 단위 + 통합 — 동시 reconnect 100회 시 ghost session 0건. yamux 강제 종료 시 OpenStream 에러 발생.

**Dependencies:** 1.1
**Files (~3):** `internal/muxregistry/registry.go`, `heartbeat.go`, 테스트
**Size:** M

---

#### Task 1.3: compute-agent `muxclient` — TLS dial + yamux client + 재연결
**Description:** 신규 패키지. agent 부팅 후 main-api에 Register 성공하면(=node_id 확보) 별도 goroutine으로 ssh-proxy mux endpoint에 TLS dial → 인증 헤더 전송 → yamux Client 세션 보유. 끊기면 exponential backoff (1s ~ 60s). 재연결 정책: 진행 중 stream은 명시적으로 끊고 새 세션 시작 (스펙 §7 Always — 사용자에 재시도 안내).

**Acceptance:**
- [ ] `muxclient.Run(ctx, cfg, onAttach func(*yamux.Session))` 시그니처
- [ ] cfg에 `Endpoint`, `ServerName`, `NodeID`, `AgentToken`, `AgentVersion`, `KeepaliveInterval` 포함
- [ ] yamux Server에서 inbound stream을 accept하는 goroutine — Phase 2.2에서 stream 핸들러 연결
- [ ] 재연결 시 이전 세션의 모든 stream cancel

**Verification:** 단위 — 인증 실패·TLS 실패·yamux init 실패 각 분리 에러. 통합 — ssh-proxy mux 재시작 시 30s 내 재연결.

**Dependencies:** 0.2, 1.1
**Files (~4):** `services/compute-agent/internal/muxclient/{dialer,session,reconnect}.go`, 테스트
**Size:** M

---

#### Task 1.4: 기존 `tunnel/` 패키지 제거
**Description:** Phase 1의 `services/compute-agent/internal/tunnel/` (TCP 인바운드 서버) 는 Phase 2 데이터 평면에 사용되지 않음. agent_tunnel_endpoint 의미 반전(ADR-012) 따라 listener·verifier 모두 archive 후 제거. cmd 와이어링도 정리.

**Acceptance:**
- [ ] `internal/tunnel/` 디렉토리 삭제
- [ ] `services/compute-agent/cmd/compute-agent/main.go` 에서 tunnel.Serve 호출 제거
- [ ] 관련 config 키 제거 (`agent.tunnel_listen_addr` 등)
- [ ] 컴파일·테스트 녹색 (Phase 1 SSH 경로 회귀는 ssh-proxy 측 직접 dial로만 통과해야 함 — 현재 e2e 환경의 dial 주소는 운영자가 노드 LAN 명시)

**Verification:** `make lint && make test && make build` 녹색. Phase 1 e2e 통합 테스트 회귀 없음 (Phase 1 e2e가 직접 dial 경로에 의존하면 1.5에서 mux 경로로 전환).

**Dependencies:** 1.3
**Files (~3):** 디렉토리 삭제, cmd, config
**Size:** S

---

**Checkpoint 1 — Mux Channel Plumbing**
- [ ] agent 기동 시 ssh-proxy mux endpoint에 attach, `muxregistry`에 등록 확인 (admin 디버그 엔드포인트 또는 메트릭)
- [ ] 네트워크 단절 시뮬레이션 (iptables drop) 후 30s 내 재연결
- [ ] Phase 1 SSH 직접 dial 경로 회귀 — Phase 2.2 전이라 SSH 사용자 트래픽은 여전히 직접 dial로 흐름
- [ ] 인간 리뷰

---

### Phase 2.2: Mux Relay — 사용자 SSH 트래픽 mux 경로 전환

> 목표: ssh-proxy `tunnelhandler` 가 직접 TCP dial 대신 muxregistry로 stream open. 사용자 SSH 종단간 라운드트립이 mux 경로로 흐른다.

#### Task 2.1: main-api ticket 페이로드 — node_id 추가
**Description:** `sshticket.Ticket` 에 `NodeID` 는 이미 있음. ssh-proxy가 ticket 페이로드에서 node_id를 읽고 `muxregistry.OpenStream(node_id)` 호출 가능하도록 검증. `TunnelEndpoint` 필드는 deprecated 마크(ADR-012) 후 v1.x 호환 위해 빈 문자열 발급.

**Acceptance:**
- [ ] ticket 발급 시 `TunnelEndpoint=""` (deprecated)
- [ ] `NodeID`는 정확히 `instances.node_id` 그대로
- [ ] Phase 1 ticket verifier(현재 agent 측)는 사용 안 됨 — 같이 archive

**Verification:** 단위 — 발급된 ticket을 ssh-proxy에서 디코드 시 node_id 정확

**Dependencies:** 0.3
**Files (~2):** `services/main-api/internal/sshticket/ticket.go`, 테스트
**Size:** XS

---

#### Task 2.2: ssh-proxy `tunnelhandler.Relay` — mux 경로
**Description:** `services/ssh-proxy/internal/tunnelhandler/relay.go` 의 `net.DialTimeout("tcp", p.TunnelEndpoint, ...)` 부분을 `muxregistry.OpenStream(ctx, ticket.NodeID)` 로 교체. yamux stream 위에 SSH 바이트 양방향 복사. ticket 헤더 포맷 변경 — agent는 이미 stream을 받아 들어오므로 ticket 헤더를 stream 첫 줄에 그대로 흘려보내고 agent가 다시 검증.

**Acceptance:**
- [ ] Relay가 `muxregistry.OpenStream` 사용, 기존 net.Dial 분기 제거
- [ ] node_id로 등록된 mux 세션 없을 시 즉시 ssh.Channel 닫고 클라이언트에 명시적 에러 메시지 반환
- [ ] 기존 idle timeout / context 취소 / cleanup 의미 보존

**Verification:** 통합 — agent를 mux 경로로 attach한 시뮬 노드에 SSH 접속 → echo command → 정상 응답. agent 끊김 시 in-flight SSH가 즉시 절단됨

**Dependencies:** 1.2, 2.1
**Files (~2):** `relay.go`, 테스트
**Size:** S

---

#### Task 2.3: compute-agent — inbound stream → VM SSHD relay
**Description:** muxclient가 받는 inbound stream의 첫 줄로 ticket(JSON 헤더) 파싱. main-api에서 발급한 ticket을 agent가 검증(기존 `tunnel.Verifier` 로직 재사용). VM 내부 IP:port로 TCP dial 후 stream ↔ TCP 양방향 copy.

**Acceptance:**
- [ ] `services/compute-agent/internal/muxclient/handler.go` — stream accept handler
- [ ] ticket 검증 실패 시 stream close + `mux_relay_failures_total{reason}` 증가
- [ ] VM dial timeout 5s 그대로
- [ ] idle timeout 30분 그대로

**Verification:** 통합 — 시뮬 VM(컨테이너 sshd) 띄우고 mux 경로로 ssh echo

**Dependencies:** 1.3, 2.1
**Files (~3):** `muxclient/handler.go`, ticket verifier 이전, 통합 테스트
**Size:** M

---

#### Task 2.4: 기존 ssh-proxy `tunnelhandler` Phase 1 직접 dial 분기 제거
**Description:** Phase 1의 `net.Dial(TunnelEndpoint)` 경로는 mux 경로로 완전 대체된다. Phase 1 사용자 0명 + ADR-012 의해 분기 유지 필요 없음. 분기 제거 + 회귀 테스트는 mux 경로 단일 경로로 통합.

**Acceptance:**
- [ ] `relay.go`에서 net.Dial 분기 코드 제거
- [ ] e2e 테스트 docker-compose가 mux 경로로 SSH 라운드트립 검증
- [ ] Phase 1 spec §8 N3(100 SSH 세션 p95 ≤ 30ms)는 Phase 2.4에서 별도 mux 부하 회귀로 검증

**Verification:** `make e2e` 통과

**Dependencies:** 2.2, 2.3
**Files (~2):** `relay.go`, e2e compose
**Size:** S

---

**Checkpoint 2 — Mux Relay**
- [ ] **F4(부분):** mux 단일 세션이 ≥10 동시 SSH stream 처리, starvation 회귀 없음 (간이 부하)
- [ ] e2e: 시뮬 노드 1개 → SSH ssh-proxy → mux → VM → 정상 명령 라운드트립
- [ ] Phase 1 e2e 회귀 100% 통과 (Phase 1 노드를 mux 경로로 마이그레이션 후)
- [ ] 인간 리뷰

---

### Phase 2.3: ACL & Onboarding

> 목표: 베타 노드를 owner_team에만 노출. 운영자 1:1 인입 절차 갖춤.

#### Task 3.1: 스케줄러 ACL 필터 + RBAC 통합
**Description:** `services/main-api/internal/scheduler/`의 노드 후보 필터 단계에서 `nodes.access_policy` 검사. `owner_team`인 경우 요청 사용자가 `owner_team_id`의 멤버여야 함. 일반 사용자는 `public` 노드 후보만 보임.

**Acceptance:**
- [ ] `Pick(user, gpu_count) → slot` 가 ACL 통과한 노드만 후보로 함
- [ ] 비-owner 사용자에게 owner_team 노드의 슬롯 후보가 절대 노출되지 않음
- [ ] 인스턴스 생성 시 ACL 위반은 404 (S3 enumerate 방지 패턴, Phase 1 §7.3과 일관)

**Verification:** 통합 테스트 — owner / non-owner 두 사용자가 동일 베타 노드에 대해 인스턴스 생성 시도. owner만 성공.

**Dependencies:** 0.3
**Files (~3):** `internal/scheduler/pick.go`, ACL 헬퍼, 테스트
**Size:** M

---

#### Task 3.2: admin CLI — `node-token create|list|revoke`
**Description:** `services/main-api/cmd/admin` 또는 신규 admin CLI 바이너리. 노드별 토큰 발급, 리스트, 폐기. owner_team_id를 필수 입력. 발급 시 stdout에 agent.toml에 채워 넣을 토큰 출력 (1회만 보이고 DB에는 hash만).

**Acceptance:**
- [ ] `admin node-token create --node-name X --owner-team Y` → 출력에 토큰 + ssh-proxy mux endpoint
- [ ] `admin node-token list --node-id X` → 활성/폐기 목록
- [ ] `admin node-token revoke --token-id X` → 폐기 (revoked_at 세팅)
- [ ] DB에는 bcrypt hash만, 평문 토큰 미저장

**Verification:** 통합 테스트 — 발급 → /internal/agent-auth 200 → 폐기 → 60s 후 401

**Dependencies:** 0.3, 0.4
**Files (~4):** admin CLI, 쿼리, 테스트, README
**Size:** M

---

#### Task 3.3: `byo-node-precheck.sh` 진단 스크립트
**Description:** 운영자가 베타 파트너 노드에 SSH로 1회 실행. IOMMU 활성·vfio-pci 모듈·커널 버전·NVIDIA 드라이버·outbound 443 reach·디스크 여유·RAM 등 점검. stdout에 GO/NO-GO + 상세.

**Acceptance:**
- [ ] `scripts/byo-node-precheck.sh` 실행 가능, exit code 0=GO 1=NO-GO
- [ ] 점검 항목: IOMMU, vfio-pci, kernel ≥ 5.15, NVIDIA driver, outbound 443→main-api, outbound 443→ssh-proxy mux endpoint, free RAM, free disk
- [ ] curl·ip·dmesg 등만 사용, 외부 의존성 없음

**Verification:** Phase 1 dev 노드에 실행 시 모두 GO. 베타 1명에 시도해 운영자 1시간 이내 노드 인증 (A5)

**Dependencies:** None (병행 가능)
**Files (~2):** `scripts/byo-node-precheck.sh`, README
**Size:** S

---

#### Task 3.4: 운영 런북 `byo-node-onboarding.md`
**Description:** 운영자 1:1 인입 절차 문서. 단계: (a) 파트너 정보·owner_team 등록, (b) 토큰 발급, (c) 파트너에 안내, (d) precheck 실행, (e) agent 설치, (f) main-api `/admin/nodes`에서 online 확인, (g) 첫 인스턴스 생성 테스트.

**Acceptance:**
- [ ] `docs/runbooks/byo-node-onboarding.md`
- [ ] 각 단계의 명령·예상 출력·실패 시 분기 포함
- [ ] 베타 파트너에게 줄 외부용 안내문 부속 문서 (`byo-node-partner-guide.md`)

**Verification:** 운영자가 문서만 보고 시뮬 노드 인입 시도, 막히는 부분 없이 완료

**Dependencies:** 3.2, 3.3
**Files (~2):** 2개 런북
**Size:** S

---

**Checkpoint 3 — ACL & Onboarding**
- [ ] **F1, F3 충족:** 시뮬 베타 노드 1개 등록 + owner team 멤버만 SSH 접속 성공, 비-owner는 슬롯 후보 미노출
- [ ] 운영자 admin CLI로 토큰 발급/폐기 라이프사이클 검증
- [ ] 인간 리뷰

---

### Phase 2.4: Grace & Ops ✱ (validates N2/N3)

> **Phase 2 핵심 게이트**: mux 흡수 후에도 Phase 1 N3(100 SSH 세션 p95 ≤ 30ms, 드롭 ≤ 0.1%) 유지. 실패 시 mux 라이브러리·Worker pool 재평가.

#### Task 4.1: 노드 상태기계 — degraded/quarantined/evicted
**Description:** main-api 워커가 30초 주기로 `nodes.last_heartbeat_at`, `nodes.last_data_plane_at` 모니터. 임계 초과 시 `node_state` 전이. 전이는 single-direction (online → degraded → quarantined → evicted), 회복은 heartbeat·data plane 모두 회복 시 online 으로 복귀(다만 evicted 후 인스턴스 회수된 노드는 운영자 수동 승인 필요).

**Acceptance:**
- [ ] 30초 주기 워커 (Phase 1 9.2 과금 워커 패턴 재사용)
- [ ] heartbeat miss 30s → `degraded`, 90s → `quarantined`, 300s → `evicted`
- [ ] data plane (mux) 단절도 같은 임계로 평가 — 둘 다 살아야 online
- [ ] 전이 시 감사 로그 + 메트릭 `node_state_transitions_total{from, to}`

**Verification:** 통합 — agent stop → 30/90/300s 시점에 정확한 상태 관찰. 재기동 시 online 복귀 검증.

**Dependencies:** 0.3, 1.2
**Files (~4):** `internal/node/watcher.go`, 쿼리, 테스트
**Size:** M

---

#### Task 4.2: 인스턴스 정책 — quarantined/evicted 처리
**Description:** ticket 발급 시점에 노드 상태 검사:
- `quarantined`: 신규 ticket 발급 거부 (404, 명시 메시지) — 진행 중 SSH는 mux 끊기면 자동 종료
- `evicted`: 인스턴스 force stop (libvirt destroy 호출 + slot 회수 + DB `state=stopped`). 베타에선 인스턴스 자동 삭제 안 함, stopped 상태로 보존.

**Acceptance:**
- [ ] `/internal/ssh-ticket` 핸들러가 노드 상태 quarantined/evicted 시 발급 거부
- [ ] node_state 전이 워커가 evicted 진입 시 그 노드의 모든 running 인스턴스 force stop
- [ ] 슬롯은 회수되지만 instance row는 보존, 사용자에게는 `stopped` 노출

**Verification:** 통합 — agent kill 후 5분 시점에 인스턴스 stopped, 슬롯 가용

**Dependencies:** 4.1
**Files (~3):** sshticket 핸들러, eviction 핸들러, 테스트
**Size:** M

---

#### Task 4.3: 모니터링 대시보드 + 알람
**Description:** Phase 1 Grafana 대시보드에 BYO 섹션 추가. 메트릭:
- `mux_sessions_active{node_id}`
- `mux_streams_active{node_id}`
- `mux_auth_failures_total{reason}`
- `node_state{state}` gauge
- `agent_version{version}` gauge
- 두 채널 RTT (control: gRPC ping, data: yamux ping)

알람: 노드 quarantined 5분 이상, mux_auth_failures rate > 10/min.

**Acceptance:**
- [ ] Phase 1 대시보드에 BYO 패널 4개 추가
- [ ] 알람 룰 2개 추가 (Phase 1 prometheus rules에 append)

**Verification:** **N2 데이터:** Phase 2.4 부하 시나리오 후 대시보드에서 mux 세션·stream·RTT 관찰 가능

**Dependencies:** 1.2, 4.1
**Files (~3):** Grafana JSON, prometheus rules, 메트릭 코드 추가
**Size:** S

---

#### Task 4.4: ✱ 부하 회귀 — Phase 1 N3 보존 검증
**Description:** `make e2e-mux-load` 신규 타겟. docker-compose.test.yml 에 (a) Phase 1 직접 dial 시뮬 노드 50개, (b) Phase 2 mux 시뮬 노드 50개, 동시 100 SSH 세션 5분 유지. p95 추가 지연·드롭율 측정.

별도로 yamux flow control 검증: 1 stream `dd if=/dev/zero` 1GB 전송 + 9 stream 인터랙티브 echo 동시 → 인터랙티브 stream RTT p95 ≤ 60ms (one-pager A3, 스펙 N5).

**Acceptance:**
- [ ] `make e2e-mux-load` Phase 1 N3 기준(p95 ≤ 30ms, 드롭 ≤ 0.1%) 통과
- [ ] flow control 시나리오 N5 통과
- [ ] 결과를 `docs/research/phase2-n2-n3-n5.md`에 수치 기록

**Verification:** **N2/N3/N5 GATE:** PR 본문에 측정값 첨부. 미통과 시 yamux 옵션 튜닝 → 재측정. Worker pool / stream 상한 / 버퍼 크기 등 조정 영역.

**Dependencies:** 2.4, 4.3
**Files (~3):** docker-compose.test.yml, 부하 스크립트, 결과 문서
**Size:** M

---

**Checkpoint 4 — N2/N3 GATE**
- [ ] Phase 1 N3 회귀 없음 (p95 ≤ 30ms, 드롭 ≤ 0.1%)
- [ ] N5 flow control 시나리오 통과
- [ ] **N3(spec) invariant:** main-api 인터페이스에 SSH 페이로드 0건 — `tcpdump` 패킷 카운트 검증
- [ ] **실패 시 Phase 2.5 진입 금지.** mux 라이브러리·옵션 재평가.
- [ ] 인간 리뷰

---

### Phase 2.5: Beta Pilot ✱ (validates A1/A4)

> 목표: 베타 파트너 1명 실 인입. 24h NAT 생존 데이터 수집 시작 (사용자 병행, 개발 블로커 아님). NPS 1주 운영.

#### Task 5.1: 베타 파트너 1명 실 인입
**Description:** 운영자가 `byo-node-onboarding.md` 따라 신뢰 파트너 1명 노드 인입. owner_team에 파트너 + 운영자 1인 등록. 첫 인스턴스 생성·SSH 접속 검증.

**Acceptance:**
- [ ] 파트너 노드가 main-api `/admin/nodes`에 online + access_policy=owner_team 등재
- [ ] owner_team 멤버가 `ssh ubuntu@{prefix}.hybrid-cloud.com`로 60초 이내 접속
- [ ] 비-owner_team 사용자가 인스턴스 생성 시도 → 베타 노드 슬롯 미노출

**Verification:** 운영자 + 파트너 1:1 시연 세션 + 스크린샷 기록

**Dependencies:** Checkpoint 4
**Files (~1):** 인입 기록 문서
**Size:** S

---

#### Task 5.2: A1 24h NAT 생존 데이터 수집
**Description:** 베타 파트너 노드에서 두 outbound 채널의 attach 시각·재연결 횟수·평균 RTT를 24h+ 수집. agent 측 메트릭(prometheus pushgateway 또는 zerolog JSON 라인). 사용자 병행 검증 — **개발 블로커 아님**.

**Acceptance:**
- [ ] 24h 연속 데이터 1세트 이상 (2일치 권장)
- [ ] control / data 채널 각 단절 횟수 기록
- [ ] 단절 후 재연결까지 p95 시간

**Verification:** **A1 closure:** `docs/research/phase2-a1-korea-nat.md` — 데이터 + 결론. KT/SKB/LG 망 종류 명시. yamux keepalive 주기 결정에 반영(Q6).

**Dependencies:** 5.1
**Files (~1):** 결과 문서
**Size:** S

---

#### Task 5.3: A4 1주 운영 + NPS
**Description:** 베타 1명을 1주 운영하면서 agent 재연결 시 in-flight SSH 끊김 정책의 사용자 수용성 확인. 사용자 NPS·자유 피드백.

**Acceptance:**
- [ ] 1주 누적 SSH 세션 수 ≥ 20
- [ ] 재연결 발생 횟수·당시 사용자 영향 기록
- [ ] NPS ≥ 7

**Verification:** **A4 closure:** `docs/research/phase2-a4-reconnect-policy.md` — NPS 미달 시 정책 변경(가능: stream resume, 다만 Phase 2 out-of-scope)

**Dependencies:** 5.2
**Files (~1):** 결과 문서
**Size:** S

---

#### Task 5.4: 베타 운영 1주 회고 + Phase 3 입력 정리
**Description:** 베타 1주 후 회고. (a) Phase 3 ssh-proxy SPOF 결정(Q5)에 필요한 부하 데이터, (b) agent 자동 업데이트 모델(Q2) 우선순위, (c) 정산 기록(Q4) 포함 여부, (d) 베타 → 더 많은 파트너 확장 시 추가 자동화 항목.

**Acceptance:**
- [ ] `docs/research/phase2-pilot-retro.md` — 4개 영역 결론 + Phase 3 spec 인풋 권장사항

**Verification:** Phase 3 spec 작성 시 본 문서 참조 가능 상태

**Dependencies:** 5.3
**Files (~1):** 결과 문서
**Size:** S

---

**Checkpoint 5 — Beta Pilot (A1/A4 GATE)**
- [ ] 베타 1명 1주 운영 무사고
- [ ] A1 24h+ 데이터 수령
- [ ] A4 NPS ≥ 7
- [ ] Phase 2 Success Criteria §8 전 항목 체크
- [ ] **Phase 3 진입 GO/NO-GO** 인간 승인

---

## Parallelization Opportunities

| 병렬 가능 조합 | 이유 | 전제 |
|---|---|---|
| 0.1 (ADR) ↔ 0.3 (DB) | 독립 | None |
| 1.1 (muxserver) ↔ 1.3 (muxclient) | 독립 패키지, 인증 헤더 포맷만 사전 합의 | 0.2, 0.4 완료 |
| 3.3 (precheck script) ↔ 3.1/3.2 | 스크립트는 코드 의존 없음 | None |
| 4.3 (모니터링) ↔ 4.1/4.2 | 메트릭 코드는 별도 축 | 1.2 완료 |

| 반드시 순차 | 이유 |
|---|---|
| 1.x → 2.x → 3.x | mux 채널·relay 동작이 ACL 검증의 전제 |
| 2.4 (Phase 1 분기 제거) → 4.4 (부하 회귀) | 회귀 측정은 단일 경로(=mux)에서 |
| 4.4 (N2/N3 GATE) → 5.x | 게이트 통과 전 실 베타 인입 금지 |

---

## Risks and Mitigations

| # | Risk | Impact | Likelihood | Mitigation |
|---|------|--------|-----------|------------|
| R1 | yamux 흐름 제어 부적합 → N5 미달 (큰 stream이 인터랙티브 starvation) | High | Med | Phase 2.4 부하 회귀에서 즉시 측정. 실패 시 yamux Window 옵션 튜닝 또는 stream priority 우회 패치 |
| R2 | 한국 가정망 NAT가 30s yamux ping보다 짧은 idle drop | Med | Med | Q6 — A1 데이터 수령 후 keepalive 5–15s 범위 조정. 기본값으로 시작 |
| R3 | mux 흡수 후 ssh-proxy CPU·goroutine 폭증 → Phase 1 N3 회귀 | High | Low | Phase 2.4 게이트에서 측정. p95 초과 시 yamux Server 옵션 + worker pool 도입 |
| R4 | agent 재연결 시 사용자가 매번 끊김에 불만 (A4 NPS 미달) | Med | Med | Phase 2.5 1주 베타에서 측정. 미달 시 Phase 2 안에선 정책 유지(스펙 결정) + Phase 3 spec에 stream resume 검토 |
| R5 | 베타 파트너 노드 환경 변수성(BIOS·커널·드라이버) | Med | High | precheck 스크립트(Task 3.3) 운영자가 사전 점검. 실패 시 1:1 대응 |
| R6 | ssh-proxy mux endpoint를 노린 인증 brute force | Med | Low | Phase 1 ssh-proxy hardening 패턴 재사용 (rate limit per peer IP). `mux_auth_failures_total` 알람 |
| R7 | Phase 1 사용자 SSH 경로(직접 dial)와 Phase 2 mux 경로 동시 운영 시 분기 버그 | Med | Med | Phase 2.2 의 Task 2.4에서 분기 자체를 제거 (단일 mux 경로). Phase 1 사용자 0이라 가능 |
| R8 | A1(24h NAT) 데이터 미수령 → Phase 3 spec 입력 부재 | Med | Med | A1은 사용자 병행 검증, **개발 블로커 아님**. Phase 3 spec 진입 시점에 데이터 없으면 conservative keepalive 결정 |
| R9 | ssh-proxy SPOF가 Phase 3 확장 시 폭증 (Q5 미해결) | Low (Phase 2) | High (Phase 3) | Phase 2 not in scope, 그러나 Phase 5.4 회고에서 Phase 3 인풋 정리 |
| R10 | 베타 파트너 노드 root 권한자가 다른 노드 ticket 위조 시도 | High | Low | S3 — agent_token + node_id 매칭 검증 + ticket의 node_id가 mux session의 node_id와 정확히 일치 검사 (Task 2.3 verifier) |

---

## Open Questions (스펙에서 이어짐 + 계획 수립 중 추가)

스펙 §9의 Q1–Q6 외에 계획 수립 중 떠오른 항목:

| # | 질문 | 결정 필요 시점 | 차단 Task |
|---|------|---------------|-----------|
| ~~**P9**~~ | ~~`/internal/agent-auth` 인증 방식~~ — **결정 (2026-04-27): Option A. Phase 1 `MAIN_API_INTERNAL_TOKEN` 재사용.** 단, Task 0.4 착수 전에 `/internal/*` 라우팅이 public 포트 노출 안 됐는지 검증 (미충족 시 Phase 2 안에서 internal 포트 분리 보강) | ~~Task 0.4 전~~ → **닫힘** | ~~0.4~~ |
| ~~**P10**~~ | ~~yamux 옵션 기본값~~ — **결정 (2026-04-27):** `KeepAliveInterval=15s`, `StreamOpenTimeout=30s`, 나머지(`MaxStreamWindowSize`, `ConnectionWriteTimeout`, `StreamCloseTimeout`, `AcceptBacklog`)는 yamux 디폴트. Phase 2.4 N5/N2 측정 후 필요시 조정 | ~~Task 1.1 전~~ → **닫힘** | ~~1.1~~ |
| ~~**P11**~~ | ~~mux endpoint 포트·도메인~~ — **결정 (2026-04-27, 갱신 PM): `mux.qlaud.net:8443`.** Caddy가 :443 점유하므로 ssh-proxy는 별도 외부 포트(:8443) 채택. Caddy L4 SNI 라우팅(option a)도 검토했으나 caddy-l4 모듈 빌드 필요해 운영 단순함 우선. 운영 측 준비물: (a) DNS A 레코드 `mux.qlaud.net` → ssh-proxy 호스트, (b) Caddy의 `*.qlaud.net` wildcard cert 파일시스템 공유 (caddy 그룹), (c) ssh-proxy 호스트:8443 inbound allow, (d) Phase 2.3 Task 3.3 precheck에 `nc -zv mux.qlaud.net 8443` 추가 (사무실/대기업망 outbound 차단 사전 점검) | ~~Task 1.1 전~~ → **닫힘** | ~~1.1~~ |
| **P12** | 베타 파트너 1번 후보 — 누구, 언제 컨택 | Phase 2.5 시작 전 | 5.1 |
| **P13** | precheck 스크립트가 NVIDIA driver 버전 어디까지 강제 | Task 3.3 전 | 3.3 |
| **P14** | force stop 시 사용자 인스턴스 데이터 보존 정책 — 디스크 이미지 보존 vs 즉시 삭제 | Task 4.2 전 | 4.2 |

스펙 Q1–Q6 중:
- **Q3 (mux 라이브러리 yamux vs HTTP/2 vs SSH)**: 스펙 단계에서 yamux로 결정. 변경 없음.
- **Q4 (grace period 길이)**: 스펙 단계에서 30/90/300s 결정. 변경 없음.
- **Q1 (stream 상한), Q6 (keepalive 주기)**: A1 데이터 수령 후 결정. Phase 2.5 산출물.
- **Q2 (자동 업데이트), Q5 (ssh-proxy SPOF)**: Phase 3 spec 결정 사항. Phase 2 not in scope.

---

## 리뷰·승인 체크리스트

구현 착수 전:

- [ ] 모든 태스크에 Acceptance · Verification · Dependencies · 파일·사이즈 기재됨
- [ ] XL 태스크 없음 (모두 XS/S/M)
- [ ] Checkpoint가 각 Phase 종료마다 존재
- [ ] N2/N3 게이트(Phase 2.4)가 실 베타 인입(Phase 2.5) 앞에 배치
- [ ] A1 검증은 사용자 병행, **개발 블로커 아님** 명시
- [ ] Open Questions P9–P14가 해당 Task 시작 전에 결정되도록 명시
- [ ] 인간 프로젝트 오너 승인
