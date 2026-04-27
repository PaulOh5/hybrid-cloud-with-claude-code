# ADR-012: `proto/agent.proto` `Register.agent_tunnel_endpoint` 의미 반전

- **Status:** Accepted (Phase 2)
- **Date:** 2026-04-26
- **Phase:** 2 (Bring Your Own Node)
- **선행 스펙:** [docs/specs/phase-2-bring-your-own-node.md](../specs/phase-2-bring-your-own-node.md) §2

## Context

Phase 1에서 `Register.agent_tunnel_endpoint` (필드 6)는 "agent가 ssh-proxy에 광고하는 자기 LAN dial 주소" 였다. ssh-proxy가 그 주소로 inbound TCP dial한다.

Phase 2 Split Plane(ADR-008) 채택으로 ssh-proxy가 더 이상 LAN으로 dial하지 않는다 — agent가 outbound mux 세션을 열고 ssh-proxy가 그 세션 위로 stream을 open한다. 기존 의미는 무용.

선택지:
1. 필드 deprecation 후 신규 필드 추가
2. 의미 반전 (필드 6 그대로 의미만 변경)
3. 필드 제거

## Decision

**의미 반전** — 필드 6 그대로 두고 의미를 "ssh-proxy mux endpoint advertised back to agent" 로 변경.

근거:
- Phase 1 운영 사용자 0명 → wire-level breaking change 자유 (스펙 §2)
- proto 메시지 크기 변동 없음
- 신규 필드 추가는 의미 혼란 (deprecated 필드와 신규 필드 공존)

추가 변경 (plan.md Task 0.2):
- `Register.mux_session_id` (선택) — agent가 ssh-proxy mux 등록 직후 control plane에 보고
- `Heartbeat.agent_version` — 누락 시 추가 (운영 가시성 O2)

## Considered Alternatives

| 안 | 기각 사유 |
|----|----------|
| **필드 6 deprecate + 신규 필드 7** | wire 호환 무의미 (사용자 0). 코드 노이즈 (deprecated 필드 사용처 추적) |
| **필드 6 제거 + 신규 필드 7** | proto 필드 번호 재사용 금지 규칙 위반 (Google proto 가이드). 필드 번호 재사용 안 하더라도 cosmetic ugliness |
| **메시지 자체 분리 (`MuxRegister`)** | Register/Heartbeat 단순 구조 깨짐. 필드 1개 차이로 별도 메시지 과대 |

## Consequences

### 긍정
- proto 변경 1줄 (주석 + 사용처 의미 반전), 신규 메시지·필드 번호 0
- agent ↔ main-api 인터페이스 안정 — 필드 번호·이름 그대로

### 부정 / 위험
- **wire-level breaking change** — Phase 1 외부 노드 0명 가정에 의존. 누군가 미공식으로 Phase 1 agent를 베타 환경에 붙이면 의미 충돌. 베타 운영자 1:1 인입 절차로 차단
- 코드 리뷰어가 "왜 의미가 바뀌었나"를 ADR로만 알 수 있음 — proto 주석에 "see ADR-012" 명시 (Task 0.2)

### 후속
- Phase 1 사용자 0명 가정이 깨지는 시점 (= Phase 3 또는 외부 알파 출시) 이전에 wire breaking change 자유 종료. 향후 Phase 3 proto 변경은 deprecate-and-replace 패턴 강제

## References
- 스펙 §2 ADR-012
- plan.md Task 0.2 (구현)
