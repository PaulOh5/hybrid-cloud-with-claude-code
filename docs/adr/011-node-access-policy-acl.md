# ADR-011: 노드 ACL — `nodes.access_policy ∈ {owner_team, public}`

- **Status:** Accepted (Phase 2)
- **Date:** 2026-04-26
- **Phase:** 2 (Bring Your Own Node)
- **선행 스펙:** [docs/specs/phase-2-bring-your-own-node.md](../specs/phase-2-bring-your-own-node.md) §2

## Context

Phase 2 베타 노드는 **소유자 팀 멤버만** 사용해야 한다 (스펙 F3). Phase 1 DC 노드는 모든 paid user에게 열려야 한다. Phase 3은 커뮤니티 풀로 전환 — 같은 ACL 메커니즘이 토글 한 번으로 호환되어야 한다.

요구 사항:
- 베타 노드 슬롯은 **owner team 외 사용자에게 후보로도 노출되지 않음** (S3 enumerate 방지)
- Phase 1 DC 노드는 동일 메커니즘에서 `public`으로 마이그하면 무회귀 동작
- Phase 3 진입 시 ACL 토글만으로 베타 노드 → 커뮤니티 풀 전환

## Decision

`nodes` 테이블에 두 컬럼 추가:

```sql
access_policy text NOT NULL DEFAULT 'public' CHECK (access_policy IN ('owner_team', 'public'))
owner_team_id uuid NULL REFERENCES teams(id)
```

스케줄러 슬롯 후보 필터 단계에서:

```
filter(slot):
  if slot.node.access_policy == 'public':       allow
  if slot.node.access_policy == 'owner_team':
    allow if user IN team(slot.node.owner_team_id)
    deny otherwise (404, 슬롯이 존재하지 않는 것처럼)
```

ACL 위반 시 **404** 반환 (S3 enumerate 방지 패턴, Phase 1 §7.3 일관).

Phase 1 마이그레이션: 기존 모든 노드 → `access_policy='public'`, `owner_team_id=NULL` (현 동작 보존).

## Considered Alternatives

| 안 | 기각 사유 |
|----|----------|
| **별도 `node_acl_rules` 테이블 (RBAC 일반화)** | 베타 5–10명에 과설계. Phase 3 커뮤니티 도입 시점에 다시 검토 |
| **불리언 `nodes.is_beta`** | Phase 3 전환 시 컬럼 의미 변경 필요. enum이 명확 |
| **whitelist 사용자 ID 배열** | 팀 멤버십 변경 시 노드 row 갱신 필요. team 추상화로 깔끔 |
| **ACL을 인스턴스 생성 시점에만 검사** | enumerate 노출 (사용자가 zone 옵션에서 베타 노드 발견 가능). 후보 필터 단계에서 검사해야 안전 |

## Consequences

### 긍정
- DB 컬럼 2개 + 스케줄러 분기 1줄로 베타·DC·Phase 3 모두 호환
- 404 반환 패턴으로 enumerate 방지 (S3 단일 진실 소스)
- Phase 3 토글: 운영자 admin CLI로 `UPDATE nodes SET access_policy='public'` 만 실행

### 부정 / 위험
- enum 2값으로 표현력 제한 — `whitelist`/`pay-per-use` 등 도입 시 ADR 추가 필요 (스펙 §7 Ask first)
- `owner_team_id NULL` 인 `owner_team` 정책 노드는 무효 — 마이그·INSERT에서 CHECK 또는 트리거 필요. 마이그 단계에서 보장

### 후속
- Phase 3 spec 진입 시 ACL enum 확장 검토 (`whitelist`, `pay_per_use`)
- 인스턴스 생성 zone UI 옵션은 access_policy 필터를 거친 노드만 노출 (Phase 2 frontend 작업, plan.md 별도 task)

## References
- 스펙 §8 F3, S3, S4
- plan.md Task 0.3 (DB 마이그), Task 3.1 (스케줄러 ACL)
