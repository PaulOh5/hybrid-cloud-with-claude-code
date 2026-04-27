# ADR-010: 노드 오프라인 grace period — 30s/90s/300s 단계 전이

- **Status:** Accepted (Phase 2)
- **Date:** 2026-04-26
- **Phase:** 2 (Bring Your Own Node)
- **선행 스펙:** [docs/specs/phase-2-bring-your-own-node.md](../specs/phase-2-bring-your-own-node.md) §2, §8 N6

## Context

가정·사무실 NAT 뒤 베타 노드는 일시 단절(라우터 재부팅, ISP 점검, NAT idle drop)이 잦다. 단절 즉시 인스턴스를 force stop하면 사용자 워크로드가 매번 죽는다. 무한정 보존하면 슬롯이 누수된다.

요구 사항:
- 짧은 단절은 invisible (사용자 영향 없이 회복)
- 중간 단절은 **신규 SSH 차단**으로 사고 격리
- 장기 단절은 **슬롯 회수** (다른 사용자 인입 가능)
- 인스턴스 자동 삭제는 **금지** (베타 신뢰 관계, 디스크 이미지 보존)

판정 신호:
- **Heartbeat (control plane, gRPC)** miss
- **Data plane (mux session) liveness** — yamux ping 실패

둘 다 살아야 `online`.

## Decision

| 임계 | 상태 | 동작 |
|------|------|------|
| heartbeat 또는 data plane miss ≥ **30s** | `degraded` | 운영자 알림. 사용자 영향 없음. 신규 SSH 허용 |
| miss ≥ **90s** | `quarantined` | 신규 ticket 발급 거부 (`/internal/ssh-ticket` 404). 진행 중 SSH는 mux 끊김으로 자연 절단 |
| miss ≥ **300s** | `evicted` | 인스턴스 force stop (`virsh destroy`) + 슬롯 회수 + DB `instances.state=stopped`. 인스턴스 row와 디스크 이미지 보존 |

복귀 정책:
- `degraded` / `quarantined` → 양쪽 채널 회복 시 자동 `online`
- `evicted` → **운영자 수동 승인 필요**. 인스턴스는 `stopped` 상태로 보존, 사용자가 재기동 요청해야 함

전이는 단방향 진행(`online → degraded → quarantined → evicted`). 회복은 직접 `online`으로만 가능 (중간 단계 거치지 않음).

## Considered Alternatives

| 안 | 기각 사유 |
|----|----------|
| **단일 임계 (5분 → force stop)** | 짧은 단절도 사용자 데이터 손실. 네트워크 변동 잦은 한국 가정망에 부적합 |
| **무한정 보존** | 슬롯 누수. 베타 5–10대 규모에선 buffer 있으나 Phase 3 확장 시 폭증 |
| **인스턴스 자동 삭제** | 베타 신뢰 관계 위반. 디스크 이미지(사용자 작업) 손실. 베타 단계 정책상 stopped 상태로 보존 후 운영자 결정 |
| **heartbeat만 판정** | mux session이 ghost로 남아도 `online` — 사용자 SSH가 즉시 끊김 (UX 일관성 깨짐) |
| **임계 5/30/120s 등 더 짧게** | 한국 ISP 라우터 NAT idle 60s 보고 있음 — 정상 노드를 false-positive `quarantined` 처리할 위험 |

## Consequences

### 긍정
- 사용자 영향 단계적 — 짧은 단절 invisible, 중간 단절 운영자 가시화, 장기 단절 슬롯 회수
- 인스턴스 보존으로 베타 파트너 데이터 안전 — Phase 2 신뢰 베타 약속 충족
- 두 채널 동시 판정으로 ghost session 격리

### 부정 / 위험
- `quarantined` 상태에서 진행 중 SSH는 끊김 (스펙 §7 Always — 사용자에 재시도 안내). NPS 영향 가능 (R4) — Phase 2.5 1주 베타에서 측정. 미달 시 Phase 3에서 stream resume 검토
- 30/90/300s 절대값은 한국 가정망 NAT 평균에 기반한 추정. **A1 24h 데이터(Phase 2.5)** 수령 후 조정 가능. Phase 2.4 게이트 외 별도 룩

### 후속
- Phase 2.5 A1 closure 후 keepalive 주기(yamux) 와 같이 grace 임계값 재평가 (Q6)
- `evicted` 후 운영자 수동 승인 UI는 Phase 2 콘솔에서 admin route만 — Phase 3에 사용자 self-serve 검토

## References
- 스펙 §8 N6, F5
- plan.md Task 4.1, 4.2 (구현)
- 한국 가정망 NAT idle 보고 — Phase 2.5 A1 자료에 결과 누적
