# Phase 2 — Bring Your Own Node

> 작성일: 2026-04-26
> 상태: 방향 합의, 가정 검증·세부 설계 진입 전
> 선행: docs/specs/phase-1-mvp.md, docs/ideas/hybrid-gpu-cloud.md

## Problem Statement

**How Might We** — 신뢰된 베타 파트너(5–10명)의 GPU 워크스테이션이 **단일 아웃바운드
연결**(프로토콜 무관)만 낼 수 있는 환경에서, ssh-proxy → 그 노드의 VM으로 들어가는 외부 SSH
트래픽을 Phase 1 DC와 동일한 UX로 라우팅할 수 있는가?

## Recommended Direction — Split Plane (gRPC control + ssh-proxy data mux)

agent는 두 개의 영속 아웃바운드 연결을 유지한다:

1. **Control plane** — `agent → main-api` 기존 gRPC bidi stream (proto/agent.proto 그대로)
2. **Data plane** — `agent → ssh-proxy` yamux-over-TLS 신규 채널. 외부 SSH 사용자의 바이트만 통과

ssh-proxy에 mux 모듈을 흡수한다(별도 서비스 X). Phase 1의 `tunnelhandler`/`ticketclient`
계약은 유지하되, 내부 동작이 "노드 LAN으로 다이얼" → "이 노드의 mux 세션으로 다이얼"
로 바뀐다. main-api는 SSH 바이트를 만지지 않고 control plane으로 깨끗히 남는다.

ACL 모델은 처음부터 community 호환으로 설계하고, Phase 2 단계에서는 베타 노드의 인스턴스
생성 권한을 **소유자 팀으로 제한**하는 정책 레이어만 둔다. Phase 3에서 ACL을 풀면 동일
코드가 공유 풀로 전환된다.

이 방향이 채택된 이유: head-of-line blocking 회피 / Phase 1 책임 분할 보존 / 신규 서비스 0개 /
사용자 명시 제약(아웃바운드 단일·프로토콜 무관) 충족.

## Key Assumptions to Validate

- [ ] **A1. 한국 가정망 keepalive·NAT 타임아웃** — KT/SKB/LG 가정망에서 두 개의 영속 outbound
  TLS 연결이 24h+ 유지된다. **검증 책임: 사용자 (개발과 병렬). 본 항목은 개발 블로커가 아님.**
- [ ] **A2. ssh-proxy mux 흡수 후 N3 보존** — 동시 100 SSH 세션에서 추가 지연 p95 ≤ 30ms,
  드롭 ≤ 0.1% (Phase 1 N3) 이 mux 추가 후에도 유지. 검증: 부하 테스트 (Phase 1 부하 스크립트 확장).
- [ ] **A3. yamux flow control 적합성** — 한 SSH 세션의 큰 전송이 다른 세션·heartbeat 굶주림
  안 만듬. 검증: 1개 세션 `dd if=/dev/zero` + 9개 세션 인터랙티브 동시 실행.
- [ ] **A4. agent 재연결 시 in-flight 세션 처리** — agent 잠시 끊겼다가 재연결될 때 기존 SSH
  세션을 끊고 깨끗히 다시 시작하는 정책으로 충분(이행 보장 안 함). 검증: 베타에 정책 공지 +
  1주 운영 시 사용자 NPS.
- [ ] **A5. 베타 노드 환경(IOMMU·vfio-pci·커널) 원격 사전 점검 가능** — 운영자가 SSH로
  진단 스크립트만 돌려도 Phase 1 수준 노드로 인증 가능. 검증: 진단 스크립트 작성 + 베타 1명에 시도.

## MVP Scope

**In (Phase 2):**
- compute-agent: ssh-proxy 측으로의 두 번째 영속 outbound 연결 + yamux 다중화
- agent ↔ ssh-proxy 인증 — Phase 1의 `agent_token` 재사용 vs mTLS 결정 후 구현
- ssh-proxy: mux 세션 레지스트리. ticket → 노드 mux 세션 ID 매핑. agent 끊김 시 세션 정리
- proto 변경: `Register.agent_tunnel_endpoint`의 의미 반전 — ssh-proxy 측에서 agent로
  광고하는 mux endpoint로 변경 (Phase 1 사용자 없음 → breaking change 자유)
- 베타 노드 등록 운영자 플로우: 운영자가 노드별 토큰 발급 → 파트너에 안내 → agent 설치
- ACL 정책 레이어: 노드의 인스턴스 생성 권한을 owner team만 허용 (community 코드 호환)
- 노드 오프라인 grace period 정의 + 인스턴스 자동 정리 정책
- Phase 1 부하 시나리오에 mux 경로 추가한 통합 E2E

**Out (Phase 2 — 명시적 미포함):**
- 셀프 가입 / 노드 자동 등록 (Phase 3)
- 노드 소유자 수익 정산·쉐어 (Phase 3)
- 자동 환경 검증 (IOMMU/vfio-pci/커널 자동 진단) — 수동 진단 스크립트로 시작
- agent 자동 업데이트 (Open Q1)
- 메트릭·콘솔 등 SSH 외 트래픽의 mux화

## Not Doing (and Why)

- **D1 (단일 gRPC stream에 SSH 합치기)** — control/data head-of-line blocking 시한폭탄
- **D3 (WireGuard mesh)** — NAT 매트릭스 검증 부담 + 가상 NIC UX 마찰
- **E (Tailscale/Headscale)** — 별도 계정 UX 마찰 거부됨
- **C (ssh -R reverse forwarding)** — SSH-in-SSH 디버깅 가시성 부족
- **별도 tunnel-server 서비스 신설** — 운영 부담 vs 효익 미흡, 사용자 거부됨
- **분산 edge router** — 5–10명 베타 over-engineering
- **노드 자체 검증 자동화** — Phase 2 베타 운영자 1:1 대응으로 충분

## Open Questions

- **Q1.** agent ↔ ssh-proxy 인증: Phase 1 `agent_token` 재사용 vs mTLS 별도. 보안·운영 부담 비교
- **Q2.** agent 자동 업데이트 메커니즘 — Phase 2 in vs out? 5대 수동 갱신은 가능하나 모델은
  처음부터 정해두는 것이 좋음
- **Q3.** mux protocol — yamux vs HTTP/2 stream vs SSH transport. 라이브러리 성숙도·관찰성 비교
- **Q4.** 노드 오프라인 grace period 길이 — VM 자동 종료 vs 일시 정지 정책
- **Q5.** Phase 1 spec §9 Q3 (agent ↔ main-api 인증) 은 본 spec에서 같이 결정
- **Q6.** ssh-proxy 단일 인스턴스가 Phase 3 노드 수십~수백 대 시 SPOF — Phase 3 진입 전
  수평 확장 설계 필요 (이번 phase에는 not in scope)
