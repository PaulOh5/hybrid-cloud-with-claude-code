# Implementation Plan: 하이브리드 GPU 클라우드 — Phase 1 MVP

> 작성일: 2026-04-23
> 기반 스펙: [docs/specs/phase-1-mvp.md](../docs/specs/phase-1-mvp.md)
> 상태: Draft v1 — 인간 리뷰·승인 대기. 구현 착수 전 Assumption Gate(A1–A8) 중 기술 가정(A4–A7) 일부는 Phase 4 이전에 검증되도록 배치.

---

## Overview

자체 DC의 멀티 GPU 워크스테이션 노드를 단일 zone 사설 GPU 클라우드로 묶어 외부 유료 수요자에게 KVM VM을 판매하는 **Phase 1 MVP**를 10개 페이즈 · 31개 태스크로 분해한다. 각 페이즈는 **수직 슬라이스(complete vertical path)**로 구성되어, 완료 시점마다 실제 동작하는 기능 하나가 추가된다. 기술적 최대 리스크(GPU passthrough · 멀티 GPU 분할 청결성 · SSH 프록시)를 **초반 3–5 페이즈**에 집중 배치하여 빠르게 실패 확인·방향 조정할 수 있게 한다.

### 의도된 실행 순서의 근거
1. **리스크 우선:** Phase 4·5·6이 스펙의 `A5`(GPU passthrough), `A6`(재할당 청결성), `A7`(SSH 프록시 부하)을 각각 닫는다. 여기서 막히면 나머지 페이즈가 무의미하므로 빠른 실패를 유도.
2. **사용자 가치 나중:** Auth·Frontend·Credits는 기술 기반이 증명된 뒤 덧붙인다 (Phase 7–10). 초반 슬라이스는 **admin REST만으로 E2E** 검증.
3. **수직 슬라이스:** "모든 DB 먼저, 모든 API 다음"식 수평 분리 금지. 각 페이즈는 "기능 하나가 추가로 관찰 가능"해야 완료.

---

## Architecture Decisions (스펙에서 확정된 것 재확인)

- **ADR-001** 하이퍼바이저: libvirt + QEMU/KVM. Firecracker 배제 (GPU passthrough 지원 제한).
- **ADR-002** GPU 분할: 정적 프로파일(`1·1·1·1` / `2·2` / `4`). 동적 패킹·MIG는 Phase 2+.
- **ADR-003** 노드↔API 통신: **agent 아웃바운드 gRPC bidi stream** (Phase 2의 NAT 뒤 노드까지 재사용).
- **ADR-004** 이미지 provisioning: Ubuntu cloud image + cloud-init.
- **ADR-005** Monorepo (Go workspaces + pnpm workspaces).
- **신규 ADR-006(계획 단계에서 제안)** SSH 프록시 릴레이 방식: 사용자 SSH → ssh-proxy → **main-api 발급 단기 티켓 검증** → **agent가 연 내부 터널**로 VM SSHD 접근. (대안: proxy가 VM에 직접 접근 — Phase 2 NAT 호환을 깨므로 배제.)
- **신규 ADR-007(계획 단계에서 제안)** 슬롯 할당 동시성: PostgreSQL **advisory lock** per `(node_id)` + `slot` 테이블 상태를 같은 트랜잭션에서 갱신. (대안: 분산 락·optimistic retry — 초기 단일 API 인스턴스에서 과함.)

**ADR-003/006/007은 구현 착수 시점에 `docs/adr/`에 정식 등재.**

---

## Dependency Graph (주요 흐름)

```
 [Foundation: Monorepo + CI + Proto + Postgres schema]
        │
        ├─► [Phase 2: Agent enrollment]  ──► Node registry + topology
        │           │
        │           └─► [Phase 3: Empty VM lifecycle]
        │                       │     (libvirt create/destroy + cloud-init)
        │                       │
        │                       └─► [Phase 4: Single-GPU passthrough] ✱ validates A5
        │                                   │
        │                                   └─► [Phase 5: Multi-GPU profiles] ✱ validates A6
        │                                               │
        │                                               └─► [Phase 6: SSH proxy]   ✱ validates A7
        │                                                           │
        │                                                           └─► [Phase 7: Auth]
        │                                                                   │
        │                                                                   ├─► [Phase 8: Frontend]
        │                                                                   │
        │                                                                   └─► [Phase 9: Credits & billing]
        │                                                                               │
        └─► (throughout) ────────────────────────────────────► [Phase 10: Observability + runbooks]
```

✱ = 기술 가정 검증 게이트. 실패 시 상위 태스크 진행 불가.

---

## Task List

> 사이즈 표기: XS (1 파일) · S (1–2 파일) · M (3–5 파일) · L (5–8 파일, 분해 검토 대상).

---

### Phase 1: Foundation

#### Task 1.1: Monorepo 골격 + CI 파이프라인
**Description:** Go workspaces, pnpm workspaces, Makefile, GitHub Actions(또는 동등) lint+test CI를 세팅하여 이후 모든 서비스 코드가 꽂힐 빈 레포 구조를 만든다.

**Acceptance:**
- [ ] `go.work` 존재, `services/compute-agent`·`services/main-api`·`services/ssh-proxy` 최소 `cmd/` + `main.go`(빈 `println`)
- [ ] `pnpm-workspace.yaml` 존재, `web/frontend` Next.js 15 스캐폴드
- [ ] `Makefile`에 `dev`, `build`, `test`, `lint`, `proto`, `migrate-up/down`, `e2e` 타겟
- [ ] CI에서 `make lint && make test` 녹색
- [ ] `.golangci.yml`, `.editorconfig`, `.gitignore`, `.env.example` 커밋

**Verification:** `make lint && make test && make build` 모두 통과 · CI PR 1회 녹색 · `gitleaks` 기본 통과

**Dependencies:** None
**Files (~7):** `go.work`, `pnpm-workspace.yaml`, `Makefile`, `.golangci.yml`, `.github/workflows/ci.yml`, `services/*/cmd/main.go` 스텁, `web/frontend/*`
**Size:** M

---

#### Task 1.2: Proto 계약 v1 + 코드 생성
**Description:** compute-agent ↔ main-api 간 gRPC bidi stream 계약을 proto로 정의. 초기엔 Register·Heartbeat·Topology·CreateInstance·DestroyInstance·InstanceStatus 메시지만.

**Acceptance:**
- [ ] `proto/agent.proto`에 위 6개 RPC/메시지 정의
- [ ] `buf lint`, `buf breaking`(기준 없음) 통과
- [ ] `make proto`로 Go 스텁 `services/*/internal/pb` + TS 클라이언트 `web/frontend/src/lib/grpc` 생성
- [ ] 생성물 `.gitignore` 제외 or 체크인 정책 문서화 (권장: 체크인)

**Verification:** `make proto` 재실행 시 diff 없음 (idempotent) · Go/TS 양쪽 `import` 가능

**Dependencies:** 1.1
**Files (~3):** `proto/agent.proto`, `buf.yaml`, `buf.gen.yaml`
**Size:** S

---

#### Task 1.3: Postgres 스키마 v1 + sqlc 파이프라인
**Description:** 초기 스키마(users, zones, nodes, gpu_profiles, gpu_slots, instances, sessions, credits, credit_ledger). Goose 마이그레이션·sqlc 쿼리 빌드·CI에서 스키마 드리프트 검사.

**Acceptance:**
- [ ] 9개 테이블 마이그레이션 1개 파일 + 역방향 `down` 동작
- [ ] `sqlc generate` 통과, 타입 생성물이 `internal/db/`에 위치
- [ ] `make migrate-up && make migrate-down && make migrate-up` 깨끗
- [ ] `docker-compose up postgres`로 CI 테스트 postgres 기동

**Verification:** `services/main-api/test/integration/schema_test.go` — testcontainers postgres에 마이그 적용 후 기본 삽입·조회 성공

**Dependencies:** 1.1
**Files (~5):** `services/main-api/db/migrations/001_init.sql`, `services/main-api/db/queries/*.sql`, `sqlc.yaml`, 통합 테스트
**Size:** M

---

**Checkpoint 1 — Foundation**
- [ ] `make lint && make test && make build` 녹색
- [ ] CI 녹색
- [ ] 빈 main-api 프로세스가 postgres에 커넥션 성공 후 `SELECT 1`
- [ ] 인간 리뷰

---

### Phase 2: Agent Enrollment

> 목표: compute-agent가 기동하면 main-api에 등록되고 topology·heartbeat가 관찰 가능.

#### Task 2.1: main-api gRPC 서버 — AgentStream 수용
**Description:** Bidi stream 수용, 첫 메시지는 `Register`(노드 식별자·호스트명·GPU 목록·NVLink 맵). 이후 heartbeat/topology 업데이트 수신, nodes 테이블 upsert.

**Acceptance:**
- [ ] `main-api`가 gRPC(mTLS 또는 Bearer — **Open Q3** 참조, 기본은 Bearer)로 `:8081` 리슨
- [ ] `Register` 수신 시 `nodes` 테이블 upsert, `node_id` 응답
- [ ] heartbeat 60초 이내 없으면 `nodes.status=offline`

**Verification:** 통합 테스트 `agent_stream_test.go` — 가짜 클라이언트가 Register→Heartbeat 3회 후 DB 상태 검증

**Dependencies:** 1.2, 1.3
**Files (~4):** `services/main-api/internal/grpc/agent_stream.go`, `internal/node/repo.go`, 테스트
**Size:** M

---

#### Task 2.2: compute-agent gRPC 클라이언트 + 토폴로지 수집
**Description:** 부팅 시 GPU 목록(`nvidia-smi -q -x`), NVLink 토폴로지(`nvidia-smi topo -m`), IOMMU 그룹 정보 수집하고 Register → 주기 heartbeat 송신. 재연결 exponential backoff.

**Acceptance:**
- [ ] `compute-agent --config` 시작 시 main-api에 Register
- [ ] Topology struct에 `[{gpu_idx, pci_addr, iommu_group, nvlink_peers[]}]` 포함
- [ ] 연결 끊김 시 2^n (최대 60s) 재시도

**Verification:** nested KVM dev VM에 데모 실행 → `GET /admin/nodes`에 등록 노드와 GPU 4개 보임 · 수동으로 agent 중단·재시작 후 복구 확인

**Dependencies:** 1.2, 2.1
**Files (~5):** `services/compute-agent/internal/stream/client.go`, `internal/gpu/topology.go`, `cmd/main.go`, 설정, 테스트
**Size:** M

---

#### Task 2.3: admin REST `GET /admin/nodes`
**Description:** 내부 운영용 REST (임시 고정 API 키). 등록된 노드, GPU 개수, 상태, 마지막 heartbeat 반환.

**Acceptance:**
- [ ] `GET /admin/nodes` 응답 JSON에 `[{id, hostname, gpu_count, status, last_heartbeat}]`
- [ ] 잘못된/없는 토큰은 401

**Verification:** 단위 테스트 + `curl`로 수동 확인

**Dependencies:** 2.1
**Files (~3):** `services/main-api/internal/api/admin_nodes.go`, 미들웨어, 테스트
**Size:** S

---

**Checkpoint 2 — Agent Enrollment**
- [ ] 실 워크스테이션(또는 GPU 패스스루 가능한 dev 머신) 1대에서 compute-agent 기동 시 `/admin/nodes`에 즉시 보임
- [ ] agent 프로세스 강제 종료 시 60초 이내 `offline`
- [ ] 통합 테스트 녹색
- [ ] 인간 리뷰

---

### Phase 3: Empty VM Lifecycle (No GPU)

> 목표: admin이 REST로 "빈 VM" 생성·삭제. GPU 없이 libvirt + cloud-init 경로만 검증.

#### Task 3.1: compute-agent libvirt 래퍼
**Description:** `digitalocean/go-libvirt` 사용. DomainDefineXML / DomainCreate / DomainDestroy / DomainUndefineFlags. 상태 변화 콜백 (`DomainEventLifecycle`) 수신.

**Acceptance:**
- [ ] `libvirt.Manager.CreateDomain(xml)`, `DestroyDomain(id)` 동작
- [ ] 상태 전이 이벤트를 내부 채널로 노출
- [ ] 에러 case (XML 오류, 이미 존재, 없음) 래핑

**Verification:** 통합 테스트 (integration build tag) — 실제 libvirtd에 최소 XML로 VM 생성 후 파괴. nested KVM CI 러너에서 실행.

**Dependencies:** 1.1
**Files (~3):** `services/compute-agent/internal/libvirt/manager.go`, 테스트, XML 빌더 최소 버전
**Size:** M

---

#### Task 3.2: cloud-init userdata 생성기
**Description:** 인스턴스 생성 시 (a) SSH 키 주입, (b) 호스트명 설정, (c) systemd user-data 로그 경로 확인용 첫 부트 메시지. NoCloud 방식(ISO seed).

**Acceptance:**
- [ ] `cloudinit.Build(req) → (userdata, metadata, isoPath)`
- [ ] 생성된 ISO를 libvirt XML `<disk>`에 cdrom으로 추가
- [ ] VM 부팅 후 10초 이내 `/var/log/cloud-init.log`에 "finished" 기록 확인 가능

**Verification:** 통합 테스트 — Ubuntu 24.04 cloud image로 VM 부팅, agent가 `DomainQemuAgentCommand`로 `uname -a` 받아옴

**Dependencies:** 3.1
**Files (~3):** `services/compute-agent/internal/cloudinit/*.go`, 테스트
**Size:** M

---

#### Task 3.3: 인스턴스 상태 기계 + DB 모델
**Description:** `pending → provisioning → running → stopping → stopped | failed` 상태 기계. 전이는 idempotent. DB 트랜잭션 내 상태 갱신.

**Acceptance:**
- [ ] `instance.Transition(from, to, reason)` 함수 존재, 불법 전이는 에러
- [ ] 상태 변경 시 `instance_events` 감사 로그 기록
- [ ] 재시작 후 진행 중이던 `provisioning` 상태 복구 또는 `failed` 마킹 로직

**Verification:** 상태 전이 테이블 드리븐 테스트 전체 녹색

**Dependencies:** 1.3
**Files (~3):** `services/main-api/internal/instance/state.go`, 쿼리, 테스트
**Size:** M

---

#### Task 3.4: admin `POST /admin/instances` — 빈 VM 생성
**Description:** 노드 지정 + 메모리·vCPU만 파라미터로 받아 VM 생성 RPC를 agent에게 전달. GPU는 아직 없음.

**Acceptance:**
- [ ] `POST /admin/instances {node_id, memory_mb, vcpus, ssh_pubkey}` → 201 + instance_id
- [ ] agent가 RPC 수신 → libvirt VM 생성 → `running` 보고까지 60초 이내
- [ ] `DELETE /admin/instances/{id}` 로 정상 파괴

**Verification:** E2E integration — admin API 호출 → 실제 VM 부팅 → VM의 IP로 직접 SSH 성공 → DELETE → 파괴 확인

**Dependencies:** 2.1, 2.2, 3.1, 3.2, 3.3
**Files (~4):** `services/main-api/internal/api/admin_instances.go`, agent 측 RPC 핸들러, 테스트
**Size:** M

---

**Checkpoint 3 — Empty VM Lifecycle**
- [ ] admin이 E2E로 VM 생성·파괴. 직접 SSH 접속 성공 (도메인 아님)
- [ ] **Assumption A4 착수**: 24h 스트레스 (반복 생성/파괴 1000회) 시작 — 실패율 기록
- [ ] 인간 리뷰: 다음 Phase는 리스크 큰 GPU passthrough — 하드웨어 준비 확인

---

### Phase 4: Single-GPU Passthrough ✱ (validates A5)

> **가정 A5 검증 게이트.** 여기서 막히면 Phase 5–10 전부 blocker.

#### Task 4.1: GPU 디스커버리 + vfio-pci 바인딩
**Description:** 부팅 시 compute-agent가 (a) IOMMU enabled 확인, (b) GPU PCI 주소 → IOMMU 그룹 매핑, (c) 지정 GPU들을 `vfio-pci` 드라이버로 리바인드. 컴패니언 디바이스(audio, USB on some boards) 동시 처리.

**Acceptance:**
- [ ] `gpu.Bindable(pciAddr) → ok bool, reason string` 진단 함수
- [ ] 노드 부트스트랩 스크립트 `scripts/node-bootstrap.sh`로 IOMMU 활성 확인 + modprobe vfio-pci + override driver
- [ ] compute-agent가 바인딩 상태를 heartbeat에 포함

**Verification:** 실제 GPU 4개 노드에서 `lspci -k` 결과가 `Kernel driver in use: vfio-pci` · `/admin/nodes/{id}` 응답에 "gpu_bindable: 4/4"

**Dependencies:** 2.2
**Files (~4):** `internal/gpu/vfio.go`, `internal/gpu/iommu.go`, `scripts/node-bootstrap.sh`, 테스트 (단위만 — 실환경은 수동)
**Size:** M

---

#### Task 4.2: GPU 슬롯 예약 프로토콜
**Description:** main-api가 `slot.Reserve(node_id, count) → slot_ids[]` 트랜잭션으로 슬롯을 차지, 실패 시 곧바로 롤백. 예약 후 agent에 CreateInstance 요청 시 slot_ids 포함.

**Acceptance:**
- [ ] `slot` 테이블 PostgreSQL advisory lock `pg_advisory_xact_lock(node_id)` 내에서 상태 전환
- [ ] 동시에 3개 요청 → 정확히 `floor(capacity/count)` 개만 성공
- [ ] 해제는 instance destroyed 이벤트에서 트리거

**Verification:** 통합 테스트 — 100개 동시 예약 요청 → DB 상태 최종 일관성 검증

**Dependencies:** 1.3, 3.3
**Files (~3):** `services/main-api/internal/scheduler/slot.go`, 쿼리, 테스트
**Size:** M

---

#### Task 4.3: domain XML 빌더 확장 — vfio hostdev
**Description:** 도메인 XML에 `<hostdev mode='subsystem' type='pci' managed='yes'>` 블록을 슬롯 GPU 개수만큼 삽입. NVIDIA 가짜-ID 회피 플래그(QEMU cmdline) 필요 시 적용.

**Acceptance:**
- [ ] XML 빌더가 `[]gpu.Slot` 입력 받아 올바른 hostdev 생성
- [ ] 생성된 XML로 VM 부팅 후 내부에서 `nvidia-smi`가 정확한 GPU 개수·모델 표시
- [ ] `DomainReset()` 또는 파괴 후 호스트 `lspci` 상태 복원 확인

**Verification:** 실환경 통합 — 1 GPU VM 부팅 → nvidia-smi 출력 → 파괴 → 호스트에서 `lspci -v`로 vfio-pci 재바운드 확인

**Dependencies:** 3.1, 4.1, 4.2
**Files (~2):** `internal/libvirt/xml.go` 확장, 테스트 + 통합 스크립트
**Size:** S

---

#### Task 4.4: main-api 스케줄러 — `gpu_count=1` 인스턴스
**Description:** `POST /admin/instances`에 `gpu_count=1`을 받아 스케줄러가 빈 슬롯 있는 노드·슬롯 선택, 예약, agent에 전달.

**Acceptance:**
- [ ] `POST /admin/instances {gpu_count:1, ...}` 성공 시 slot 1 점유, 실패 시 409
- [ ] `DELETE` 시 슬롯 해제
- [ ] 여러 노드 중 가용성 기준 간단 선택(빈 슬롯 개수 내림차순)

**Verification:** E2E — 1 GPU VM 생성 · SSH 접속 · `nvidia-smi` · 파괴 · 슬롯 회수

**Dependencies:** 4.2, 4.3
**Files (~3):** `internal/scheduler/pick.go`, API 확장, 테스트
**Size:** S

---

**Checkpoint 4 — A5 GATE (단일 GPU 패스스루)**
- [ ] E2E 생성 → nvidia-smi 정상 → 파괴 → 호스트 상태 복원
- [ ] **A5 closure 문서화:** `docs/research/a5-gpu-passthrough.md` — 벤치마크(`nvidia-smi --query-gpu`, 기본 CUDA sample) 수치가 베어메탈 대비 ≤ 5% 하락
- [ ] **실패 시 Phase 5 진입 금지.** BIOS·하드웨어 점검·hypervisor 재평가.
- [ ] 인간 리뷰 — 이 게이트가 사업 방향의 진위를 확정

---

### Phase 5: Multi-GPU Profiles ✱ (validates A6)

#### Task 5.1: 프로파일 설정 포맷 + 노드 바인딩
**Description:** 노드별 YAML 파일 `configs/profiles/{node_id}.yaml`에 허용 레이아웃 정의. 예: 4 GPU 노드에 `1·1·1·1` / `2·2` / `4` 명시. 프로파일은 **정적**, 런타임 변경 없음 (Open Q4의 Phase 1 답).

**Acceptance:**
- [ ] YAML 스키마 + 로더 + 검증 (IOMMU 그룹·NVLink 페어와 일치 확인)
- [ ] compute-agent 기동 시 main-api에 프로파일 동기화 (Heartbeat에 프로파일 해시 포함)
- [ ] 불일치 시 노드 `degraded` 마킹

**Verification:** 단위 테스트 + 통합 — 4 GPU 노드에 세 가지 레이아웃 각각 적용 후 정합성 검증

**Dependencies:** 4.1
**Files (~4):** `configs/profiles/README.md`, `internal/profile/loader.go`, 검증, 테스트
**Size:** M

---

#### Task 5.2: 슬롯 할당기 — 프로파일 제약 존중
**Description:** `scheduler.Pick(nodes, gpu_count)`가 프로파일 내 "같은 NVLink 도메인" 또는 "같은 PCIe 스위치" 슬롯을 선호. 크기 없는 경우(프래그멘테이션) 409 반환 — 동적 패킹 없음.

**Acceptance:**
- [ ] 2 GPU 요청 시 NVLink pair 우선, 없으면 동일 PCIe 스위치, 둘 다 없으면 거절
- [ ] 할당 결정 로그에 근거(선택 이유) 기록

**Verification:** 테이블 드리븐 테스트 — 다양한 토폴로지 상황에서 예상 슬롯 반환

**Dependencies:** 4.2, 5.1
**Files (~2):** `internal/scheduler/profile_aware.go`, 테스트
**Size:** S

---

#### Task 5.3: GPU 리셋 + 재할당 청결성
**Description:** VM 파괴 시 compute-agent가 (a) `virsh destroy`, (b) vfio-pci 재바인드, (c) GPU 리셋(해당되는 경우 `nvidia-smi -r` 또는 PCI FLR), (d) 메모리 클리어를 위한 **더미 GPU 워크로드** 실행하여 이전 상태 잔존 불가능하게 함.

**Acceptance:**
- [ ] `gpu.ResetSlots(slots)` 공개 함수 존재
- [ ] VM 파괴 후 다음 VM에 동일 슬롯 할당까지 90초 이내

**Verification:** **A6 청결성 테스트** — VM1 기동 → GPU 메모리에 32MB 마커 작성 → VM1 파괴 → VM2 기동(같은 슬롯) → 같은 GPU 버퍼 덤프에서 마커 미발견

**Dependencies:** 4.3
**Files (~3):** `internal/gpu/reset.go`, 테스트 스크립트, 통합 테스트
**Size:** M

---

#### Task 5.4: 멀티 GPU 인스턴스 E2E
**Description:** `gpu_count ∈ {1, 2, 4}`를 허용. 스케줄러·XML 빌더·에이전트가 전부 멀티 GPU 경로 처리.

**Acceptance:**
- [ ] 2 GPU VM 기동 후 `nvidia-smi topo -m` 결과가 NVLink 페어
- [ ] 4 GPU VM 기동 후 모든 GPU 가시
- [ ] 3 GPU 요청은 400 (프로파일에 없음)

**Verification:** **데모**: 4 GPU 노드 1대에 3가지 레이아웃 순차 시연 (1·1·1·1 → 파괴 → 2·2 → 파괴 → 4)

**Dependencies:** 5.1, 5.2, 5.3
**Files (~2):** API 확장, E2E 스크립트
**Size:** S

---

**Checkpoint 5 — A6 GATE (멀티 GPU 분할)**
- [ ] 3가지 프로파일 모두 동작 · 성능 손실 ≤ 5% (각 프로파일별 벤치 결과 문서)
- [ ] **A6 closure:** `docs/research/a6-slot-cleanliness.md` — 마커 테스트 10회 반복 모두 통과
- [ ] 인간 리뷰

---

### Phase 6: SSH Proxy ✱ (validates A7)

> 목표: `ssh ubuntu@{subdomain}.hybrid-cloud.com`으로 VM 진입. 직접 IP 사용 중단.

#### Task 6.1: ssh-proxy 스켈레톤
**Description:** `golang.org/x/crypto/ssh`로 SSH 서버 기동. 사용자가 connect 하면 subdomain 파싱, 세션 컨텍스트 생성. (아직 라우팅은 없음 — 즉시 closed.)

**Acceptance:**
- [ ] `:22` 리슨, 사용자 host key verification 응답
- [ ] subdomain → `instance_id` 후보 추출
- [ ] 메트릭: 접속·거절·평균 지연

**Verification:** 단위 테스트 + `ssh -v foo.example.com` 수동 접속 시 subdomain 로깅

**Dependencies:** 1.1
**Files (~3):** `services/ssh-proxy/cmd/main.go`, `internal/session/*`, 테스트
**Size:** M

---

#### Task 6.2: 라우팅 조회 + 티켓 발급
**Description:** ssh-proxy가 main-api에 `POST /internal/ssh-ticket {instance_id, user_pubkey_fingerprint}` → 5초 유효 HMAC 티켓 + 타겟 agent·VM 엔드포인트 반환.

**Acceptance:**
- [ ] main-api `/internal/ssh-ticket` 엔드포인트(내부 토큰 인증)
- [ ] instance 존재 + running 상태일 때만 발급, 아니면 404
- [ ] 티켓에 `{node_id, vm_internal_ip, port, exp, sig}`

**Verification:** 단위 + 만료 테스트

**Dependencies:** 2.1, 3.3
**Files (~3):** main-api 엔드포인트, ssh-proxy 클라이언트, 테스트
**Size:** S

---

#### Task 6.3: agent 터널 + 릴레이
**Description:** ssh-proxy가 agent의 gRPC `OpenTunnel(ticket)` 호출 → agent가 VM 내부 IP:22로 로컬 TCP 연결 후 스트림으로 bytes 왕복. ssh-proxy는 그 스트림을 사용자 SSH 소켓과 양방향 복사.

**Acceptance:**
- [ ] `OpenTunnel` RPC: 티켓 서명 검증 → 내부 접속 → bidi stream
- [ ] 동시 여러 세션 가능
- [ ] 사용자 측 disconnect 시 agent 측 소켓 즉시 정리

**Verification:** **E2E** — `ssh ubuntu@{random}.example.com` 접속 후 `nvidia-smi` 실행 성공 → exit → 자원 회수 확인 · **100 동시 세션 부하**

**Dependencies:** 6.1, 6.2, 4.4
**Files (~4):** ssh-proxy relay, agent OpenTunnel, proto 업데이트, E2E 테스트
**Size:** M

---

#### Task 6.4: 와일드카드 DNS + 부하 테스트
**Description:** `*.hybrid-cloud.com` A 레코드 설정(운영 DNS 변경 필요 — Open Q1), 100 동시 세션 재현용 부하 스크립트.

**Acceptance:**
- [ ] DNS resolve 확인 문서
- [ ] 부하 스크립트 `scripts/ssh_load.sh` 100 세션 동시 유지 5분 · 드롭률 ≤ 0.1%
- [ ] p95 추가 지연 ≤ 30ms

**Verification:** **A7 closure:** `docs/research/a7-ssh-proxy-load.md`

**Dependencies:** 6.3
**Files (~2):** 스크립트, 문서
**Size:** S

---

**Checkpoint 6 — A7 GATE (SSH 프록시)**
- [ ] 도메인 기반 SSH 접속 표준 경로로 채택
- [ ] 인간 리뷰

---

### Phase 7: User Accounts + Auth

> 지금까지는 admin API만. 이제 일반 사용자 경로 추가.

#### Task 7.1: 사용자·세션 스키마 + 인증 엔드포인트
**Description:** register / login / logout / whoami. bcrypt 해시, 세션 쿠키 (httpOnly, Secure, SameSite=Lax). 크레딧 티어는 기본값 0.

**Acceptance:**
- [ ] `POST /api/v1/auth/register|login|logout`, `GET /api/v1/auth/me`
- [ ] 레이트 리밋: login IP별 5회/분
- [ ] 패스워드 정책: 최소 10자

**Verification:** 단위 + 통합 + gosec scan 통과

**Dependencies:** 1.3
**Files (~5):** `internal/auth/*`, `api/auth_handlers.go`, 테스트
**Size:** M

---

#### Task 7.2: 세션 미들웨어 + 사용자 컨텍스트
**Description:** 모든 `/api/v1/*` 엔드포인트가 세션 쿠키로 사용자 식별. admin API는 별도 토큰으로 분리 유지.

**Acceptance:**
- [ ] 미들웨어가 `user_id`를 context에 주입
- [ ] 비인증 요청은 401

**Verification:** 통합 테스트 — 여러 케이스

**Dependencies:** 7.1
**Files (~2):** 미들웨어, 테스트
**Size:** S

---

#### Task 7.3: 인스턴스 소유권 + RBAC
**Description:** `instances` 테이블에 `owner_id` 추가. 사용자는 자기 인스턴스만 CRUD. admin은 모두.

**Acceptance:**
- [ ] `GET/POST/DELETE /api/v1/instances`(사용자용) 이 자기 것만
- [ ] 타 사용자 인스턴스 접근 시 404 (not 403 — enumerate 방지)

**Verification:** **S3 조건**: 사용자A 토큰으로 사용자B instance_id DELETE → 404 · 사용자A 인스턴스는 정상 CRUD

**Dependencies:** 4.4, 7.2
**Files (~3):** 쿼리 WHERE owner_id, 미들웨어, 테스트
**Size:** S

---

**Checkpoint 7 — Auth**
- [ ] 두 유저 동시 인스턴스 생성 → 서로 못 봄
- [ ] 인간 리뷰

---

### Phase 8: Frontend

#### Task 8.1: Next.js 쉘 + 인증 플로우 UI
**Description:** App Router 기반 레이아웃, shadcn/ui 설치, 로그인/회원가입 페이지, 세션 쿠키 사용하는 API 클라이언트.

**Acceptance:**
- [ ] `/login`, `/register`, `/logout` 페이지
- [ ] Zod로 입력 검증
- [ ] 미인증 접근 시 `/login`으로 리다이렉트

**Verification:** Playwright E2E — 회원가입 → 로그인 → 대시보드 진입

**Dependencies:** 7.1
**Files (~6):** `app/layout.tsx`, `app/login/*`, `app/register/*`, `lib/api/auth.ts`, 테스트
**Size:** M

---

#### Task 8.2: 인스턴스 리스트 + 생성 UI
**Description:** 인스턴스 목록 페이지, 생성 폼(이름 + GPU 크기 선택 1/2/4), 삭제 버튼, 실시간 상태 갱신.

**Acceptance:**
- [ ] `/instances` — 테이블 뷰
- [ ] `/instances/new` — 생성 폼, 성공 시 `/instances/[id]`
- [ ] 상태 `pending`/`provisioning`/`running` 폴링 (5초)

**Verification:** Playwright E2E — 생성 → running 확인

**Dependencies:** 7.3, 8.1
**Files (~6):** 페이지들, 컴포넌트, API 훅, 테스트
**Size:** M

---

#### Task 8.3: 인스턴스 상세 — SSH 접속 정보
**Description:** 인스턴스 카드에 `ssh ubuntu@{subdomain}.hybrid-cloud.com` 명령어와 "복사" 버튼. 게스트 SSH pubkey 등록 UI.

**Acceptance:**
- [ ] SSH 명령어 렌더링
- [ ] 사용자 SSH pubkey 관리 UI (`/settings/ssh-keys`)
- [ ] pubkey 주입이 cloud-init에 반영

**Verification:** **F2 조건**: UI에서 전체 플로우로 SSH 접속 성공

**Dependencies:** 6.3, 8.2
**Files (~5):** 페이지, SSH keys API 훅, 설정 페이지, 테스트
**Size:** M

---

**Checkpoint 8 — Frontend E2E**
- [ ] Playwright 테스트: 신규 사용자 가입 → 로그인 → SSH 키 등록 → 인스턴스 생성 → SSH 접속 → 파괴
- [ ] 인간 리뷰

---

### Phase 9: Credits & Billing

#### Task 9.1: 크레딧 원장 + 관리자 수동 충전
**Description:** `credit_ledger` 테이블(append-only). `credits` 뷰는 집계. 관리자 API `POST /admin/users/{id}/credits {amount, reason}`.

**Acceptance:**
- [ ] 원장 기록은 append-only (업데이트·삭제 금지 — DB 제약)
- [ ] 잔액은 뷰 또는 캐시된 집계
- [ ] 관리자 충전 감사 로그

**Verification:** 단위 + 통합 · 악의적 중복 충전 시도 테스트

**Dependencies:** 1.3, 7.2
**Files (~4):** 마이그, 쿼리, 엔드포인트, 테스트
**Size:** M

---

#### Task 9.2: 러닝 시간 과금 워커
**Description:** main-api 내 워커가 30초마다 `running` 인스턴스를 스캔, 경과 시간에 해당하는 크레딧을 원장에 기록. 멱등 키 `(instance_id, minute_bucket)` 유니크.

**Acceptance:**
- [ ] 워커 종료·재시작 시 중복 청구 없음
- [ ] GPU 개수 × 요금표(YAML 설정) 기반 계산
- [ ] 트랜잭션 안전

**Verification:** 가상 인스턴스 1시간 러닝 시뮬 — 청구액이 예상치 ±1초 오차 이내

**Dependencies:** 9.1
**Files (~3):** 워커, 요금 설정, 테스트
**Size:** M

---

#### Task 9.3: 크레딧 부족 게이트
**Description:** 생성 시 잔액 체크, 러닝 중 잔액 0 이하 되면 자동 정지(5분 유예 후).

**Acceptance:**
- [ ] 잔액 ≤ 0 인 사용자가 create 시도 → 402
- [ ] 러닝 중 잔액 소진 → `stopping` 전이 후 알림 이메일 플레이스홀더

**Verification:** 통합 시나리오 — 저크레딧 사용자 끝까지 시뮬

**Dependencies:** 9.2, 7.3
**Files (~2):** 미들웨어/훅, 테스트
**Size:** S

---

**Checkpoint 9 — Credits**
- [ ] 30분 러닝 후 청구액 ±2원 오차
- [ ] 인간 리뷰

---

### Phase 10: Admin Dashboard + Operational Readiness

#### Task 10.1: 관리자 UI 페이지
**Description:** `/admin/*` — 노드·슬롯·사용자·크레딧·인스턴스 강제 종료. 일반 프론트에 서브 라우트로 통합(admin 롤 전용).

**Acceptance:**
- [ ] 5개 섹션 페이지 · 기본 필터
- [ ] 일반 사용자 접근 시 404

**Verification:** Playwright admin flow

**Dependencies:** 8.2
**Files (~7):** `app/admin/*`
**Size:** M

---

#### Task 10.2: Prometheus 메트릭 + Grafana 대시보드
**Description:** 세 서비스에 `/metrics` 노출. 커스텀 메트릭: `gpu_slot_used`, `instance_total{status}`, `ssh_sessions_active`, `api_request_duration_seconds`. Grafana 프로비저닝 JSON 포함.

**Acceptance:**
- [ ] docker-compose에 prometheus+grafana
- [ ] 3개 대시보드(노드·API·프록시)
- [ ] 기본 알람 룰 (노드 offline 5분 이상, API 5xx ≥ 1%)

**Verification:** **N5 조건**: 대시보드에서 실시간 값 관찰, 임의 이벤트 1건 실행 후 반영 확인

**Dependencies:** 여러 — 각 서비스 완성 후
**Files (~8):** `infra/prometheus/*`, `infra/grafana/*`, 서비스별 메트릭 코드
**Size:** M

---

#### Task 10.3: 로그 수집 (Loki) + 구조화 로깅
**Description:** zerolog → stdout JSON → Promtail → Loki. Grafana에서 검색 가능.

**Acceptance:**
- [ ] 3개 서비스 모두 JSON 로그
- [ ] instance_id·user_id 필드 공통
- [ ] 로그 보존 14일

**Verification:** Grafana에서 특정 instance_id 필터링 시 해당 로그만 표시

**Dependencies:** 10.2
**Files (~4):** infra + 로깅 유틸
**Size:** S

---

#### Task 10.4: 런북 + 백업 + 롤백
**Description:** `docs/runbooks/` — (a) 노드 장애 (b) VM hang (c) ssh-proxy crash (d) DB 장애 (e) 전면 롤백. PostgreSQL 일일 dump → S3(또는 동등) + 복구 리허설.

**Acceptance:**
- [ ] 5개 런북 문서
- [ ] `scripts/backup.sh` + `scripts/restore.sh`
- [ ] 복구 리허설 1회 수행 기록

**Verification:** **O1–O4 조건** 모두 충족 문서

**Dependencies:** 전 Phase
**Files (~8):** 문서 + 스크립트
**Size:** M

---

**Checkpoint 10 — Operational Readiness**
- [ ] Success Criteria §8 전 항목 체크 통과
- [ ] 30일 운영 관찰 계획 수립
- [ ] 런칭 GO/NO-GO 인간 승인

---

## Parallelization Opportunities

| 병렬 가능 조합 | 이유 | 전제 |
|---|---|---|
| Phase 1.2 (proto) + 1.3 (schema) | 독립 | 1.1 완료 후 |
| Phase 3.1 (libvirt) + 3.2 (cloud-init) | 다른 파일 | 1.1 완료 후 |
| Phase 8 (frontend) 시작 ↔ Phase 7.3 (RBAC) 마무리 | API 계약 고정되면 가능 | 7.2 완료 후 |
| Phase 10.2 (metrics) ↔ 기능 Phase들 | observability 코드는 별도 축 | — |

| 반드시 순차 | 이유 |
|---|---|
| Phase 4 → 5 → 6 | 기술 가정 게이트, 실패 전파 방지 |
| Phase 1 → 2 | 모든 gRPC 서비스의 기반 |
| Phase 9.2 → 9.3 | 과금 워커가 게이트보다 먼저 |

---

## Risks and Mitigations

| # | Risk | Impact | Likelihood | Mitigation |
|---|------|--------|-----------|------------|
| R1 | GPU passthrough 호스트 BIOS·메인보드 호환 불가 | High | Med | **Phase 4 게이트**에서 즉시 검증. 실패 시 메인보드 교체 · 다른 워크스테이션 모델 재평가 전에 다음 Phase 진입 금지. |
| R2 | NVIDIA 드라이버 VFIO 차단 (GeForce 카드) | Med | Low–Med | 초기엔 RTX A6000·L40 등 프로 카드 중심 가정. GeForce 지원은 별도 연구 분기 |
| R3 | libvirt-go 라이브러리 CGo 의존으로 빌드 복잡 | Med | Low | `digitalocean/go-libvirt` (pure Go) 고정. CGo 대안 금지 |
| R4 | GPU 재할당 시 메모리 잔존 (A6) | High | Med | Phase 5.3 마커 테스트 · 실패 시 카드별 리셋 전략(nvidia-smi -r / PCI FLR / 전체 노드 재부팅) 계층화 |
| R5 | SSH 프록시 릴레이 지연 > 30ms 목표 초과 | Med | Low | Phase 6.4 부하 테스트에서 즉시 관측. agent 측 io copy 버퍼 조정 · zero-copy 대안 |
| R6 | Nested KVM CI 러너 불안정 | Med | Med | 통합 테스트는 전용 베어메탈 러너 1대 확보. CI 매트릭스에서 분리 |
| R7 | Phase 7 이후 스코프 크리프 (UI 기능 추가 요구) | Med | High | Boundaries "Ask first" 규칙 — 스펙 §7. 모든 추가 요청은 스펙 PR 먼저 |
| R8 | Assumption A1/A2 (사용자 인터뷰) 구현 시작 후에도 미완 | High | Med | Phase 1 병행으로 리서치 진행. Phase 5 완료 전 결과 확보 못하면 방향 재평가 |
| R9 | DB 스키마 v1 결정이 Phase 9 과금 요구와 충돌 | Med | Med | Phase 9 설계 시점에 스키마 마이그레이션 전략 재점검. append-only 원장 구조로 대비 |
| R10 | 멀티 노드 네트워크 격리 부족 (S1 위협) | High | Med | Phase 4 착수 시점에 libvirt 네트워크 격리 정책 결정. 게스트 간 L2 차단 기본 |

---

## Open Questions (스펙에서 이어짐 + 계획 수립 중 추가)

스펙의 Q1–Q8 외에 계획 수립 과정에서 다음이 떠올랐다:

| # | 질문 | 결정 필요 시점 | 차단 Phase |
|---|------|---------------|-----------|
| **P1** | compute-agent ↔ main-api 인증: **mTLS vs Bearer** | Phase 2 시작 전 | 2.1 |
| **P2** | Nested KVM CI 러너 호스팅 (자체 베어메탈 / 클라우드 인스턴스) | Phase 3 시작 전 | 3.1 |
| **P3** | 프로파일 변경 정책: 노드 재부팅 필요한가, 핫 리로드 가능한가 | Phase 5 시작 전 | 5.1 |
| **P4** | SSH 프록시 HA 요구 수준: 단일 인스턴스 vs Active-Active | Phase 6 시작 전 | 6.1 |
| **P5** | 사용자 SSH 키 관리 UX — 계정당 N개 vs 인스턴스당 N개 | Phase 8 시작 전 | 8.3 |
| **P6** | 요금표 단위 (분/시간), 최소 청구 단위 | Phase 9 시작 전 | 9.2 |
| **P7** | 게스트 OS 이미지 보안 베이스라인 (SSH 포트 · fail2ban · unattended-upgrades) | Phase 3 시작 전 | 3.2 |
| **P8** | 운영 DNS 공급자 + `*.hybrid-cloud.com` 관리 주체 | Phase 6 시작 전 | 6.4 |

---

## 리뷰·승인 체크리스트

구현 착수 전:

- [ ] 모든 태스크에 Acceptance · Verification · Dependencies · 파일·사이즈 기재됨
- [ ] XL 태스크 없음 (모두 S 또는 M)
- [ ] Checkpoint가 각 Phase 종료마다 존재
- [ ] 기술 리스크 게이트(A5·A6·A7)가 Phase 4·5·6에 배치
- [ ] Open Questions P1–P8이 해당 Phase 시작 전에 결정되도록 명시
- [ ] Assumption A1–A8 중 기술 가정(A4–A7)이 Phase 중 검증되도록 게이트 배치
- [ ] 비기술 가정(A1·A2·A3·A8)은 병행 리서치로 Phase 5 완료 전까지 결론
- [ ] 인간 프로젝트 오너 승인
