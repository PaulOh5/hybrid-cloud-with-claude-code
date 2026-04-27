# ADR-009: agent ↔ ssh-proxy 인증 — TLS 1.3 + Phase 1 agent_token 재사용

- **Status:** Accepted (Phase 2)
- **Date:** 2026-04-26
- **Phase:** 2 (Bring Your Own Node)
- **선행 스펙:** [docs/specs/phase-2-bring-your-own-node.md](../specs/phase-2-bring-your-own-node.md) §2

## Context

Split Plane(ADR-008) 데이터 채널은 agent가 outbound로 ssh-proxy:443에 TLS 연결을 연다. agent가 자기 신원을 ssh-proxy에 어떻게 증명하는가?

요구 사항:
- **단일 진실 소스:** agent_token의 발급·폐기는 main-api DB. ssh-proxy가 별도 토큰을 갖지 않음
- **Phase 1 일관:** Phase 1 control plane 인증은 `agent_token` (gRPC metadata). 동일 토큰 재사용
- **베타 규모(5–10명)에 적합:** mTLS 클라이언트 인증서 발급·로테이션 운영 부담 회피
- **폐기 반영:** main-api에서 토큰 폐기 후 60s 내 ssh-proxy가 신규 mux 세션 거부 (S2)

## Decision

**TLS 1.3 + agent_token 헤더 + main-api 위임 검증.**

1. agent → ssh-proxy: TLS 1.3 강제 (다운그레이드 거부). 인증서는 ssh-proxy의 도메인 cert (`*.qlaud.net`). agent는 클라이언트 인증서 미사용
2. TLS 핸드셰이크 직후 agent가 1줄 JSON 헤더 전송: `{"node_id":"...","token":"...","agent_version":"..."}`
3. ssh-proxy는 main-api `/internal/agent-auth`에 POST하여 검증 위임. 결과는 60s TTL 캐시 (S2)
4. 검증 성공 시 yamux Server 세션 시작. 실패 시 즉시 close + `mux_auth_failures_total{reason}` 카운터

## Considered Alternatives

| 안 | 기각 사유 |
|----|----------|
| **mTLS** | 클라이언트 cert 발급·갱신 PKI 운영 필요. 베타 5–10명에 과대. 노드 토큰 폐기보다 cert 폐기가 느림 (CRL/OCSP 인프라 부재) |
| **ssh-proxy 자체 토큰 DB** | 두 곳 동기화 시 일관성 깨짐. 토큰 폐기 절차 분기 |
| **JWT + 단일 서명키** | 폐기 메커니즘이 어색 (블랙리스트 또는 단명 토큰 → 갱신 RPC 필요). main-api 호출 1회로 끝나는 단순함 우위 |
| **TLS 1.2 허용** | 다운그레이드 공격 표면. 베타 환경 하드웨어 모두 1.3 지원 (Go 1.21+ 표준) |

## Consequences

### 긍정
- 운영 표면 최소: agent_token 발급 절차(Task 3.2 admin CLI) 하나로 control + data 평면 모두 인증
- 폐기 반영 ≤ 60s 보장 (Task 0.4 캐시 TTL)
- Phase 1 hardening 패턴(rate limit, brute-force 방어) 그대로 ssh-proxy mux endpoint에 적용 (R6)

### 부정 / 위험
- ssh-proxy → main-api 호출 추가 (60s 캐시 hit 외엔 RTT 추가) — 베타 규모에선 무시 가능
- 토큰 leak 시 폐기까지 ≤ 60s gap. 베타 신뢰 관계로 수용. Phase 3에서 mTLS 재고

### 후속
- Phase 3 진입 전 mTLS 채택 여부 재검토 (스펙 §2 ADR-009 비고)
- main-api `/internal/agent-auth` 인증은 Phase 1 `MAIN_API_INTERNAL_TOKEN` 재사용 (plan.md P9 결정, 2026-04-27)

## References
- 스펙 §8 S1, S2 — TLS 1.3 강제, 토큰 단일 진실 소스
- plan.md Task 0.4, 1.1 (Auth header 포맷·캐싱)
