# ADR-008: Phase 2 데이터 평면 — Split Plane

- **Status:** Accepted (Phase 2)
- **Date:** 2026-04-26
- **Phase:** 2 (Bring Your Own Node)
- **선행 스펙:** [docs/specs/phase-2-bring-your-own-node.md](../specs/phase-2-bring-your-own-node.md) §2

## Context

Phase 1은 ssh-proxy가 노드의 LAN 주소로 직접 TCP dial하여 SSH 데이터를 중계했다. 베타 파트너 노드는 가정·사무실 NAT 뒤에 있어 inbound TCP가 불가능하다. 모든 트래픽은 agent가 시작한 outbound 연결 위에서만 흘러야 한다.

요구 사항:
- **단일 outbound 제약:** agent는 `main-api:443` (control) + `ssh-proxy:443` (data) 이외 outbound 0건
- **분리 보장:** main-api에 사용자 SSH 페이로드가 흐르지 않아야 함 (기존 control plane 부하 보호)
- **Phase 1 회귀 무결성:** Phase 1 N3 (100 SSH 세션 p95 ≤ 30ms) 유지
- **신규 서비스 0:** 운영 부담 추가 금지

## Decision

**Split Plane** — 두 영속 outbound 채널:

| 채널 | 출발 | 도착 | 프로토콜 | 용도 |
|------|------|------|----------|------|
| Control | compute-agent | main-api | gRPC over TLS:443 | Register, Heartbeat, InstanceStatus, CreateInstance/DestroyInstance |
| Data | compute-agent | ssh-proxy | yamux over TLS:443 | 사용자 SSH 바이트 양방향 |

ssh-proxy가 데이터 평면을 흡수한다. 기존 ssh-proxy 프로세스에 yamux server를 추가, `tunnelhandler.Relay`가 `net.Dial(LAN endpoint)` 대신 `muxregistry.OpenStream(node_id)`를 호출.

## Considered Alternatives

| 안 | 요약 | 기각 사유 |
|----|------|----------|
| **D1. 단일 채널 (gRPC streaming)** | control + data를 하나의 gRPC 양방향 stream에 합침 | gRPC HTTP/2 stream은 head-of-line blocking — 한 SSH 세션이 stuck하면 control 메시지(heartbeat 등)가 지연. 부하 시 시한폭탄 |
| **D3. 외부 mesh (Tailscale/WireGuard)** | 베타 노드를 mesh에 합류 | (a) 베타 파트너에 third-party 의존성 강제, (b) 운영 도메인 외부로 노출, (c) `ssh ubuntu@{prefix}.qlaud.net` UX 변경 — Phase 1 약속 위반 |
| **E. 신규 tunnel-server 서비스** | gateway 전담 신규 Go 바이너리 | 운영 부담 (배포·모니터링·SPOF 추가) — 사용자가 명시 거부 ("신규 서비스 X") |
| **C. SSH-in-SSH** | agent → ssh-proxy로 reverse SSH 터널, 그 안에 SSH | 디버깅 지옥, OpenSSH 다중 세션 멀티플렉싱 한계, agent 재연결 시 stale tunnel 처리 복잡 |
| **별도 tunnel-server** | ssh-proxy와 별개 mux gateway | 단일 책임은 좋으나 베타 5–10명에 과대. ssh-proxy CPU 여유분으로 충분 (Phase 2.4 부하 게이트로 검증) |

## Consequences

### 긍정
- 신규 서비스 0, 운영 표면 그대로
- ssh-proxy의 `tunnelhandler` 분기점 한 줄 → mux 분리 격리됨
- main-api에 SSH 트래픽 0 — N3(spec invariant) 자연 충족
- ADR-009의 토큰 단일 진실 소스(main-api) 와 깔끔히 직교

### 부정 / 위험
- ssh-proxy CPU·goroutine 부담 증가 → Phase 1 N3 회귀 가능 (R3). **Phase 2.4 부하 게이트로 측정.** 미통과 시 yamux 옵션 / worker pool 재평가
- ssh-proxy SPOF 영향이 Phase 3에서 폭증 (R9, Q5) — Phase 2 out-of-scope. Phase 3 spec에서 sticky session/consistent hash 결정
- yamux 흐름 제어가 인터랙티브/대용량 stream 혼재에 부적합할 위험 (R1) — Phase 2.4 N5 게이트로 측정

### 후속
- Phase 3 진입 전 ssh-proxy 수평 확장 모델 결정 (Q5)
- yamux 설정값(KeepAliveInterval 등)은 ADR 본문에 비고정. Phase 2.4 측정 후 plan.md P10에 반영

## References
- 스펙 §2 ADR-008 요약
- one-pager [docs/ideas/phase-2-bring-your-own-node.md](../ideas/phase-2-bring-your-own-node.md) §Not Doing
