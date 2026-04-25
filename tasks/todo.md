# Phase 1 MVP — Task Checklist

> Detailed plan: [tasks/plan.md](./plan.md) · Spec: [docs/specs/phase-1-mvp.md](../docs/specs/phase-1-mvp.md)
> Gate markers: ✱ = 기술 가정 검증 필수. 실패 시 후속 Phase 진입 금지.

---

## Phase 1 · Foundation
- [ ] **1.1** Monorepo 골격 + CI 파이프라인 — `go.work` · pnpm workspaces · Makefile · golangci-lint · GH Actions (M)
- [ ] **1.2** Proto 계약 v1 + 코드 생성 — `proto/agent.proto` · buf · Go+TS 스텁 (S)
- [ ] **1.3** Postgres 스키마 v1 + sqlc 파이프라인 — goose 마이그·sqlc·9 테이블 (M)
- [ ] **✔ Checkpoint 1:** lint·test·build 녹색, DB 연결 확인, 인간 리뷰

## Phase 2 · Agent Enrollment
- [ ] **2.1** main-api gRPC 서버 — AgentStream 수용 · nodes upsert · heartbeat TTL (M)
- [ ] **2.2** compute-agent gRPC 클라이언트 + 토폴로지 수집 (NVLink·IOMMU) (M)
- [ ] **2.3** `GET /admin/nodes` REST 엔드포인트 + 토큰 인증 (S)
- [ ] **✔ Checkpoint 2:** 실 노드 등록·offline 감지, 인간 리뷰
- [ ] **📋 Open Q P1 결정 필요 (2.1 전):** agent↔api 인증 방식 (mTLS vs Bearer)
- [ ] **📋 Open Q P2 결정 필요 (3.1 전):** Nested KVM CI 러너

## Phase 3 · Empty VM Lifecycle
- [ ] **3.1** compute-agent libvirt 래퍼 — 도메인 생성·파괴·상태 이벤트 (M)
- [ ] **3.2** cloud-init userdata 생성기 — NoCloud ISO · SSH 키 주입 (M)
- [ ] **3.3** 인스턴스 상태 기계 + DB 모델 — idempotent 전이 · 감사 로그 (M)
- [ ] **3.4** admin `POST/DELETE /admin/instances` — GPU 없이 VM 생성·파괴 (M)
- [ ] **✔ Checkpoint 3:** admin E2E로 VM 생성·파괴, 직접 SSH 성공
- [ ] **📋 Assumption A4 착수:** 24h 스트레스(1000회 생성/파괴) 시작
- [ ] **📋 Open Q P7 결정 필요 (3.2 전):** 게스트 OS 보안 베이스라인

## Phase 4 · Single-GPU Passthrough ✱ (A5 GATE)
- [ ] **4.1** GPU 디스커버리 + vfio-pci 바인딩 — IOMMU·컴패니언 디바이스 (M)
- [ ] **4.2** GPU 슬롯 예약 프로토콜 — advisory lock · 트랜잭션 안전 (M)
- [ ] **4.3** domain XML 빌더 확장 — vfio hostdev · 멀티 슬롯 지원 (S)
- [ ] **4.4** main-api 스케줄러 — `gpu_count=1` 인스턴스 E2E (S)
- [ ] **✱ Checkpoint 4 (A5 GATE):** 1 GPU VM에서 nvidia-smi 성공, 성능 손실 ≤ 5% 문서화. **실패 시 다음 Phase 금지.**

## Phase 5 · Multi-GPU Profiles ✱ (A6 GATE)
- [ ] **5.1** 프로파일 설정 포맷 + 노드 바인딩 — YAML·해시 동기화 (M)
- [ ] **5.2** 슬롯 할당기 — NVLink pair·PCIe 스위치 제약 (S)
- [ ] **5.3** GPU 리셋 + 재할당 청결성 — FLR·nvidia-smi -r·메모리 클리어 (M)
- [ ] **5.4** 멀티 GPU 인스턴스 E2E — 1/2/4 크기 지원 (S)
- [ ] **✱ Checkpoint 5 (A6 GATE):** 4 GPU 노드에서 `1·1·1·1` / `2·2` / `4` 순차 시연, 마커 테스트 10회 통과
- [ ] **📋 Open Q P3 결정 필요 (5.1 전):** 프로파일 핫 리로드 vs 재부팅

## Phase 6 · SSH Proxy ✱ (A7 GATE)
- [ ] **6.1** ssh-proxy 스켈레톤 — SSH 서버·subdomain 파싱·메트릭 (M)
- [ ] **6.2** 라우팅 조회 + 티켓 발급 — main-api `/internal/ssh-ticket` HMAC (S)
- [ ] **6.3** agent 터널 + 릴레이 — `OpenTunnel` RPC · bidi bytes 복사 (M)
- [ ] **6.4** 와일드카드 DNS + 부하 테스트 — 100 세션 · p95 ≤ 30ms (S)
- [ ] **✱ Checkpoint 6 (A7 GATE):** 도메인 SSH 접속 성공, 부하 테스트 통과
- [ ] **📋 Open Q P4 결정 필요 (6.1 전):** ssh-proxy HA 요구 수준
- [ ] **📋 Open Q P8 결정 필요 (6.4 전):** DNS 공급자·도메인 관리

## Phase 7 · User Accounts + Auth
- [x] **7.1** 사용자·세션 스키마 + 인증 엔드포인트 — bcrypt·쿠키·레이트 리밋 (M)
- [x] **7.2** 세션 미들웨어 + 사용자 컨텍스트 (S)
- [x] **7.3** 인스턴스 소유권 + RBAC — `owner_id` · 404 enumerate 방지 (S)
- [x] **✔ Checkpoint 7:** 두 유저 간 자원 격리, S3 조건 충족

## Phase 8 · Frontend
- [x] **8.1** Next.js 쉘 + 인증 플로우 UI — App Router·shadcn/ui·login·register (M)
- [x] **8.2** 인스턴스 리스트 + 생성 UI — GPU 크기 선택 1/2/4 (M)
- [x] **8.3** 인스턴스 상세 — SSH 접속 정보·사용자 SSH 키 관리 (M)
- [x] **✔ Checkpoint 8:** Playwright E2E — 가입 → 로그인 → SSH 키 → 인스턴스 → SSH 접속 → 파괴
- [ ] **📋 Open Q P5 결정 필요 (8.3 전):** SSH 키 관리 UX 모델

## Phase 9 · Credits & Billing
- [ ] **9.1** 크레딧 원장 + 관리자 수동 충전 — append-only · 감사 로그 (M)
- [ ] **9.2** 러닝 시간 과금 워커 — 멱등 키 · 30초 주기 (M)
- [ ] **9.3** 크레딧 부족 게이트 — create 402 · running 자동 정지 (S)
- [ ] **✔ Checkpoint 9:** 30분 러닝 청구액 ±2원
- [ ] **📋 Open Q P6 결정 필요 (9.2 전):** 요금 단위·최소 청구

## Phase 10 · Admin Dashboard + Operational Readiness
- [ ] **10.1** 관리자 UI 페이지 — 5 섹션 · 인스턴스 강제 종료 (M)
- [ ] **10.2** Prometheus 메트릭 + Grafana 대시보드 — 3 대시보드·알람 룰 (M)
- [ ] **10.3** Loki 로그 + 구조화 로깅 — 14일 보존 (S)
- [ ] **10.4** 런북 + 백업 + 롤백 — 5 런북·복구 리허설 1회 (M)
- [ ] **✔ Checkpoint 10:** Success Criteria 전 항목 통과 · 30일 관찰 계획 · 런칭 GO/NO-GO

---

## Parallel-executable 세트

동시 작업자가 2명 이상일 때:
- `1.2` + `1.3` (proto와 schema 독립)
- `3.1` + `3.2` (libvirt와 cloud-init 독립)
- `10.2`/`10.3`은 기능 Phase와 병행 가능

반드시 순차:
- Phase 4 → 5 → 6 (기술 게이트 사슬)
- 1 → 2 (모든 gRPC 서비스 기반)

---

## 병행 리서치 (구현과 별도 트랙, Phase 5 완료 전 결론)

- [ ] **A1 리서치:** 기존 구매자 30명 인터뷰 — 수익화 의향
- [ ] **A2 리서치:** 국내 GPU 클라우드 수요자 10명 인터뷰 — 가격·지연 민감도
- [ ] **A3 리서치:** 베타 파트너 NPS (Phase 7 이후 Private Zone 시범)
- [ ] **A8 리서치:** 구매 DB 세그먼트로 베타 풀 규모 확인
