# Phase 2 BYO — Task Checklist

> Detailed plan: [tasks/phase2/plan.md](./plan.md) · Spec: [docs/specs/phase-2-bring-your-own-node.md](../../docs/specs/phase-2-bring-your-own-node.md)
> Gate markers: ✱ = 게이트. 실패 시 후속 Phase 진입 금지.

---

## Phase 2.0 · Foundation
- [ ] **0.1** ADR-008 ~ ADR-012 정식 등재 — Split Plane / TLS+token / grace periods / ACL / proto 의미 반전 (S)
- [ ] **0.2** proto 의미 반전 + 신규 메시지 — `agent_tunnel_endpoint` 주석 변경, `mux_session_id` 추가, `agent_version` 노출 (S)
- [ ] **0.3** DB 마이그 — `nodes.access_policy`, `owner_team_id`, `last_data_plane_at`, `node_state`, `node_tokens` 테이블 (M)
- [ ] **0.4** main-api `/internal/agent-auth` 엔드포인트 — token + node_id 검증, 60s 캐시 (S)
  - 사전 검증: `/internal/*` 가 public 포트 노출 안 됐는지 확인
- [ ] **✔ Checkpoint 0:** lint·test·build 녹색, Phase 1 회귀 0건, ADR 5건 머지, 인간 리뷰
- [x] **📋 Open Q P9 결정 (2026-04-27):** Option A — Phase 1 `MAIN_API_INTERNAL_TOKEN` 재사용 (별도 토큰·mTLS 안 함)

## Phase 2.1 · Mux Channel Plumbing
- [ ] **1.1** ssh-proxy `muxserver` — TLS 1.3 listener + 인증 헤더 + main-api 위임 + 60s 캐시 (M)
  - 운영 측 사전 준비: DNS `mux.qlaud.net` A 레코드, `*.qlaud.net` cert, ssh-proxy:443 inbound allow
- [ ] **1.2** ssh-proxy `muxregistry` — node_id → session, ghost session 정리, yamux ping (M)
- [ ] **1.3** compute-agent `muxclient` — TLS dial + yamux client + 재연결 + stream accept (M)
- [ ] **1.4** 기존 `compute-agent/internal/tunnel/` 패키지 제거 — ADR-012 의미 반전 적용 (S)
- [ ] **✔ Checkpoint 1:** agent ↔ ssh-proxy mux attach, 30s 내 재연결, Phase 1 SSH 회귀 없음, 인간 리뷰
- [x] **📋 Open Q P10 결정 (2026-04-27):** `KeepAliveInterval=15s`, `StreamOpenTimeout=30s`, 나머지 yamux 디폴트
- [x] **📋 Open Q P11 결정 (2026-04-27):** `mux.qlaud.net:443` (운영 도메인은 `qlaud.net`)

## Phase 2.2 · Mux Relay
- [ ] **2.1** main-api ticket 페이로드 — node_id 활용, `TunnelEndpoint` deprecated 처리 (XS)
- [ ] **2.2** ssh-proxy `tunnelhandler.Relay` — `net.Dial` → `muxregistry.OpenStream(node_id)` (S)
- [ ] **2.3** compute-agent inbound stream → VM SSHD relay — ticket 검증 + dial + idle timeout (M)
- [ ] **2.4** 기존 ssh-proxy 직접 dial 분기 제거 — 단일 mux 경로화 (S)
- [ ] **✔ Checkpoint 2:** 시뮬 노드에서 mux 경로 SSH 라운드트립, Phase 1 e2e 회귀 통과, 인간 리뷰

## Phase 2.3 · ACL & Onboarding
- [ ] **3.1** 스케줄러 ACL 필터 + RBAC 통합 — `nodes.access_policy` 검사, owner_team 멤버십 (M)
- [ ] **3.2** admin CLI — `node-token create|list|revoke` + bcrypt hash + 발급 stdout (M)
- [ ] **3.3** `byo-node-precheck.sh` 진단 스크립트 — IOMMU·vfio-pci·커널·드라이버·outbound (S)
- [ ] **3.4** 운영 런북 `byo-node-onboarding.md` + 파트너 안내문 (S)
- [ ] **✔ Checkpoint 3:** F1·F3 충족, 운영자 admin CLI 라이프사이클 검증, 인간 리뷰
- [ ] **📋 Open Q P13 결정 (3.3 전):** NVIDIA driver 버전 강제 범위

## Phase 2.4 · Grace & Ops ✱ (N2/N3 GATE)
- [ ] **4.1** 노드 상태기계 — degraded(30s)/quarantined(90s)/evicted(300s) 워커 (M)
- [ ] **4.2** 인스턴스 정책 — quarantined 신규 ticket 거부, evicted force stop + 슬롯 회수 (M)
- [ ] **4.3** Grafana 대시보드 + 알람 — mux sessions/streams, node_state, agent_version, RTT (S)
- [ ] **4.4** ✱ 부하 회귀 — `make e2e-mux-load` 100 동시 SSH p95 ≤ 30ms · N5 flow control (M)
- [ ] **✱ Checkpoint 4 (N2/N3 GATE):** Phase 1 N3 회귀 없음, N5 통과, main-api에 SSH 페이로드 0건. **실패 시 다음 Phase 금지.**
- [ ] **📋 Open Q P14 결정 (4.2 전):** force stop 시 인스턴스 디스크 보존 정책

## Phase 2.5 · Beta Pilot ✱ (A1/A4 GATE)
- [ ] **5.1** 베타 파트너 1명 실 인입 — onboarding 런북 따라 1:1 인입, 첫 SSH 검증 (S)
- [ ] **5.2** A1 24h NAT 생존 데이터 수집 — control/data 채널 attach 시각·재연결 횟수·RTT (S)
- [ ] **5.3** A4 1주 운영 + NPS — 재연결 정책 수용성, NPS ≥ 7 (S)
- [ ] **5.4** 베타 1주 회고 — Phase 3 입력 (SPOF·자동업데이트·정산 기록·확장 자동화) (S)
- [ ] **✱ Checkpoint 5 (A1/A4 GATE):** 무사고 1주, A1 데이터 수령, NPS ≥ 7, Phase 3 GO/NO-GO 인간 승인
- [ ] **📋 Open Q P12 결정 (5.1 전):** 베타 파트너 1번 후보

---

## Parallel-executable 세트

동시 작업자 2명 이상일 때:
- `0.1` (ADR) ↔ `0.3` (DB) — 독립
- `1.1` (muxserver) ↔ `1.3` (muxclient) — 패키지 분리, 인증 헤더 포맷만 사전 합의
- `3.3` (precheck) ↔ `3.1`/`3.2` — 스크립트는 코드 의존 없음
- `4.3` (모니터링) ↔ `4.1`/`4.2` — 메트릭은 별도 축

반드시 순차:
- `1.x` → `2.x` → `3.x` (mux 채널·relay 동작이 ACL의 전제)
- `2.4` (분기 제거) → `4.4` (단일 경로 회귀 측정)
- `4.4` (N2/N3 GATE) → `5.x` (실 베타)

---

## 사용자 병행 트랙 (개발 블로커 아님)

- [ ] **A1 검증:** 한국 가정망 24h+ NAT 생존 데이터 — Phase 2.5 베타 인입 후 자동 수집 시작
- [ ] **베타 파트너 1명 컨택:** Phase 2.5 시작 전 (P12)

---

## 스펙 Open Questions 처리 상태

| Spec Q# | 상태 | 비고 |
|---------|------|------|
| Q1 stream 상한 | A1 데이터 후 | Phase 2.5 산출물 반영 |
| Q2 자동 업데이트 | Phase 2 out | Phase 3 spec |
| Q3 mux 라이브러리 | **결정** | yamux (스펙 §2 ADR-008) |
| Q4 grace period | **결정** | 30/90/300s (스펙 §2 ADR-010) |
| Q5 ssh-proxy SPOF | Phase 2 out | Phase 3 spec |
| Q6 keepalive 주기 | A1 데이터 후 | yamux 기본 30s 시작 |
