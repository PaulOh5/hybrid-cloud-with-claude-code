# Spec: 하이브리드 GPU 클라우드 — Phase 1 MVP

> 상태: Draft v1 — 구현 착수 전 리뷰·승인 필요
> 작성일: 2026-04-23
> 기반: [docs/ideas/hybrid-gpu-cloud.md](../ideas/hybrid-gpu-cloud.md)
> 커버리지: Phase 1 (1–4개월), DC 우선 출시 — Private Zone 베타 병행·커뮤니티 노드는 별도 spec

---

## 1. Objective

### 무엇을 만드나
자체 데이터센터의 멀티 GPU 워크스테이션 노드들을 하나의 **단일 zone 사설 GPU 클라우드**로 묶어 외부 유료 수요자에게 KVM 기반 GPU VM 인스턴스를 판매하는 플랫폼의 MVP. Phase 2(Private Zone 베타)·Phase 3(커뮤니티 노드)에서 그대로 확장 가능한 아키텍처로 구축한다.

### 왜
- 한국 GPU 클라우드 시장 인지도 확보 (6개월 목표의 Phase 1 담당분)
- 기존 워크스테이션 판매 고객에게 제공할 "수익화 옵션(Phase 3)"의 기술 기반 선행 구축
- 자체 DC 상용 운영으로 신뢰·SLA·운영 프로세스 검증

### 사용자 (Persona)
| 페르소나 | 설명 | Phase 1 관여 |
|---------|------|-------------|
| **Paid User** | 한국 내 AI 연구자·스타트업. RunPod/Vast.ai를 쓰다가 국내 저지연·원화 결제·한국어 지원 때문에 이동 | 인스턴스 생성·SSH 접속의 주 사용자 |
| **Internal Operator** | 회사 운영자. zone·노드·고객·크레딧 관리 | 관리자 대시보드의 주 사용자 |
| **Beta Partner** | 기존 워크스테이션 구매자 5–10명 (Phase 1 후반) | compute-agent를 자기 워크스테이션에 설치해보는 얼리 테스터 — 인프라 관점에서 `compute-agent`가 홈 네트워크에서 동작함을 검증하는 역할 |

### Success (상위 3개, 상세는 §8)
1. Paid User가 웹에서 GPU 인스턴스(1·2·4 GPU 티어)를 생성하여 60초 이내 SSH 접속 가능
2. 단일 4 GPU 노드에서 `1·1·1·1` / `2·2` / `4` 프로파일 각각 정상 동작 (격리·성능 손실 ≤ 5%)
3. 30일 상용 운영 중 인스턴스 가용성 ≥ 99.0%, 데이터 유실 0건

---

## 2. Tech Stack

### Backend (Go)
- **Go 1.22+**
- **libvirt-go (digitalocean/go-libvirt)** — VM 라이프사이클·XML 도메인 정의
- **gRPC + Protocol Buffers** — compute-agent ↔ main-api 영속 양방향 스트림 (agent 측 아웃바운드 연결)
- **golang.org/x/crypto/ssh** — ssh-proxy의 SSH 프로토콜 처리
- **pgx/v5** — PostgreSQL 드라이버
- **sqlc** — 스키마 기반 타입 안전 쿼리 생성
- **goose** — DB 마이그레이션
- **zerolog** — 구조화 로깅
- **prometheus/client_golang** — 메트릭

### Frontend
- **Next.js 15 (App Router)** + **React 19**
- **Tailwind CSS 4**
- **TanStack Query v5** — 서버 상태
- **Zod** — 폼·API 경계 검증
- **shadcn/ui** (Radix 기반) — 기본 UI 컴포넌트

### 인프라·운영
- **PostgreSQL 16**
- **Docker + Docker Compose** (main-api, ssh-proxy, frontend, postgres, prometheus, loki, grafana 번들)
- **systemd** (compute-agent; 각 GPU 노드에 직접 설치)
- **Let's Encrypt + wildcard DNS** (`*.hybrid-cloud.com` — ssh-proxy는 SSH이므로 TLS 아님; frontend는 HTTPS)
- **OS**: Ubuntu 24.04 LTS (호스트·게스트 양쪽 기본)

### 결정 사항(ADR로 기록 예정)
- [ADR-001] 하이퍼바이저: libvirt + QEMU/KVM (Firecracker 배제: GPU passthrough 지원 제한)
- [ADR-002] GPU 분할 전략: 정적 프로파일. 동적 패킹·MIG는 Phase 2+
- [ADR-003] 노드↔API 통신: agent 측 아웃바운드 gRPC 스트림 (Phase 2의 NAT 뒤 노드 호환 위한 선행 결정)
- [ADR-004] 이미지 provisioning: Ubuntu cloud image + cloud-init (SSH 키·호스트명 주입)
- [ADR-005] Monorepo (Go workspaces + pnpm workspaces)

---

## 3. Commands

```bash
# === 루트(Makefile) ===
make dev              # docker-compose up + compute-agent(dev-node) + frontend dev
make build            # 모든 Go 서비스 + frontend 빌드
make test             # Go test + frontend test (CI와 동일)
make lint             # golangci-lint + pnpm lint + buf lint
make proto            # proto/*.proto → Go stub + TS client 생성
make migrate-up       # goose postgres up
make migrate-down     # goose postgres down 1

# === 서비스별 ===
# services/compute-agent
go run ./cmd/compute-agent --config configs/dev.yaml
go test ./... -race -coverprofile=coverage.out

# services/main-api
go run ./cmd/main-api --config configs/dev.yaml
sqlc generate           # DB 쿼리 코드 생성

# services/ssh-proxy
go run ./cmd/ssh-proxy --config configs/dev.yaml

# web/frontend
pnpm dev                # Next.js 개발 서버 :3000
pnpm build              # 프로덕션 빌드
pnpm test               # Vitest
pnpm test:e2e           # Playwright
pnpm lint               # ESLint + Prettier --check

# === 통합 E2E ===
make e2e                # docker-compose.test.yml + Playwright (가짜 GPU 포함된 nested KVM 환경)
```

---

## 4. Project Structure

```
hybrid-cloud/
├─ services/
│  ├─ compute-agent/              # GPU 노드에 설치되는 에이전트
│  │  ├─ cmd/compute-agent/
│  │  ├─ internal/
│  │  │  ├─ libvirt/              # libvirt 래퍼·도메인 XML 빌더
│  │  │  ├─ gpu/                  # vfio-pci 바인딩·IOMMU 그룹 탐지
│  │  │  ├─ profile/              # 정적 프로파일 로더 (1·1·1·1 / 2·2 / 4)
│  │  │  ├─ slot/                 # GPU 슬롯 상태·할당 추적
│  │  │  ├─ cloudinit/            # cloud-init userdata 생성
│  │  │  ├─ stream/               # gRPC bidi stream 클라이언트
│  │  │  └─ metrics/              # Prometheus exporter
│  │  └─ configs/
│  ├─ main-api/                   # 중앙 API
│  │  ├─ cmd/main-api/
│  │  ├─ internal/
│  │  │  ├─ api/                  # HTTP handlers (REST)
│  │  │  ├─ grpc/                 # agent 쪽 gRPC 서버
│  │  │  ├─ scheduler/            # zone/노드/슬롯 선택 로직
│  │  │  ├─ instance/             # 라이프사이클 상태기계
│  │  │  ├─ billing/              # 크레딧 회계
│  │  │  ├─ auth/                 # 세션·토큰
│  │  │  └─ db/                   # sqlc 생성물 + 마이그레이션 적용자
│  │  └─ db/
│  │     ├─ queries/              # *.sql → sqlc
│  │     └─ migrations/           # goose
│  └─ ssh-proxy/                  # 도메인 기반 SSH 라우팅
│     ├─ cmd/ssh-proxy/
│     └─ internal/
│        ├─ resolver/             # {subdomain} → instance_id → target_agent
│        ├─ session/              # 세션 수명·멀티플렉싱
│        └─ relay/                # bytes 중계 + 로그·메트릭
├─ web/
│  └─ frontend/                   # Next.js 15 App Router
│     ├─ src/app/                 # 라우트
│     ├─ src/components/          # UI
│     ├─ src/lib/api/             # main-api 클라이언트
│     └─ src/lib/auth/
├─ proto/                         # *.proto 계약 (compute-agent ↔ main-api)
├─ infra/
│  ├─ docker-compose.yml
│  ├─ docker-compose.test.yml
│  ├─ prometheus/
│  ├─ grafana/
│  └─ systemd/                    # compute-agent.service 템플릿
├─ scripts/
│  ├─ node-bootstrap.sh           # GPU 노드 초기화 (IOMMU, vfio-pci 바인딩)
│  └─ release/
├─ docs/
│  ├─ ideas/
│  ├─ specs/
│  └─ adr/
├─ go.work                        # Go 워크스페이스
├─ pnpm-workspace.yaml
├─ Makefile
└─ README.md
```

**배치 원칙**
- `services/*`는 서비스 단위 독립 빌드 가능 (각자 `cmd/`, `internal/`, `go.mod` 없이 `go.work` 공유)
- `internal/` 밖으로 누출되는 공통 라이브러리는 신중히 — 필요 시 `pkg/`로 승격 후 합의
- `proto/`는 단일 진실 — Go와 TS 양쪽 스텁 자동 생성

---

## 5. Code Style

### Go
- **golangci-lint** strict 프로필 (`.golangci.yml`: errcheck, govet, staticcheck, gosec, revive, gocyclo max 15)
- `gofmt -s` 적용 (CI에서 강제)
- 에러는 `fmt.Errorf("%w", err)`로 래핑, 로그에서 최상위만 `log.Error().Err(err).Msg(...)`로 출력
- 컨텍스트 전파 의무: 외부 I/O 함수 첫 인자 `ctx context.Context`
- 인터페이스는 **사용하는 쪽**에서 선언 (소비자 지향)
- 패키지명 소문자 단일 단어, 함수명 단순·구체 (`NewInstance`가 아니라 `CreateForUser` 식으로 의도 드러내기)

**예시: compute-agent 인스턴스 생성 함수의 표준 형태**

```go
// services/compute-agent/internal/libvirt/domain.go
package libvirt

import (
    "context"
    "fmt"

    "github.com/digitalocean/go-libvirt"
    "github.com/rs/zerolog/log"

    "hybridcloud/services/compute-agent/internal/gpu"
    "hybridcloud/services/compute-agent/internal/profile"
)

// CreateInstance provisions a KVM VM occupying the given GPU slots of a node.
// The caller is responsible for reserving the slots before calling this function.
func (m *Manager) CreateInstance(
    ctx context.Context,
    spec InstanceSpec,
    slots []gpu.Slot,
    prof profile.Layout,
) (*Instance, error) {
    if err := prof.Validate(slots); err != nil {
        return nil, fmt.Errorf("profile mismatch: %w", err)
    }

    xml, err := m.xmlBuilder.Build(spec, slots, prof)
    if err != nil {
        return nil, fmt.Errorf("build domain xml: %w", err)
    }

    dom, err := m.virt.DomainDefineXML(xml)
    if err != nil {
        return nil, fmt.Errorf("define domain: %w", err)
    }
    if err := m.virt.DomainCreate(dom); err != nil {
        return nil, fmt.Errorf("start domain: %w", err)
    }

    log.Info().
        Str("instance_id", spec.ID).
        Int("gpu_count", len(slots)).
        Str("profile", prof.Name).
        Msg("instance created")

    return &Instance{ID: spec.ID, DomainID: dom.ID}, nil
}
```

### TypeScript / React
- **strict** + `noUncheckedIndexedAccess` + `noImplicitAny` (모두 true)
- 함수 컴포넌트 · 훅, 클래스 컴포넌트 금지
- API 경계에서 **Zod 스키마**로 검증 (런타임 안전)
- `TanStack Query`의 `useQuery`/`useMutation`을 `src/lib/api/` 아래 훅으로 묶음
- Tailwind: `clsx` + `tailwind-merge` 조합(`cn()` 유틸)

**예시: 인스턴스 목록 훅**

```tsx
// web/frontend/src/lib/api/instances.ts
import { z } from "zod";
import { useQuery } from "@tanstack/react-query";
import { apiFetch } from "./client";

export const InstanceSchema = z.object({
  id: z.string().uuid(),
  name: z.string(),
  gpuCount: z.enum(["1", "2", "4"]).transform(Number),
  status: z.enum(["pending", "running", "stopping", "stopped", "failed"]),
  sshHost: z.string().nullable(),
  createdAt: z.string().datetime(),
});
export type Instance = z.infer<typeof InstanceSchema>;

export function useInstances() {
  return useQuery({
    queryKey: ["instances"],
    queryFn: async (): Promise<Instance[]> => {
      const res = await apiFetch("/api/v1/instances");
      return z.array(InstanceSchema).parse(await res.json());
    },
    staleTime: 10_000,
  });
}
```

---

## 6. Testing Strategy

### Go
- **단위 테스트**: `testing` + `testify/require`. 테이블 드리븐 기본형.
- **통합 테스트**: 실제 PostgreSQL 컨테이너(`testcontainers-go`), 실제 libvirt는 **nested KVM CI 환경**에서만 실행 (태그 `//go:build integration`).
- **E2E**: `docker-compose.test.yml` 기동 후 Playwright로 전체 흐름 — 인스턴스 생성→SSH 접속→삭제.
- 커버리지 목표: 전체 ≥ 70%, 인스턴스 라이프사이클·GPU 슬롯 할당 경로 ≥ 90%.

### Frontend
- **Vitest + Testing Library** (컴포넌트·훅 단위)
- **Playwright** (E2E, Go 통합 E2E와 공유)
- 접근성: 핵심 페이지에 `@axe-core/playwright` 자동 검사

### 테스트 위치
```
services/<name>/internal/<pkg>/*_test.go        # 단위
services/<name>/test/integration/*_test.go      # 통합
web/frontend/src/**/__tests__/*.test.ts(x)      # 컴포넌트 단위
e2e/                                            # Playwright E2E (루트)
```

### Mandatory Prove-It 영역
다음 경로는 실제 가상화·libvirt를 상대로 한 통합 테스트 없이는 머지 금지:
- `libvirt.Manager.CreateInstance` / `DestroyInstance`
- `gpu.Slot` 할당·해제·재할당(청결성 포함)
- `scheduler`의 노드·프로파일·슬롯 선택
- ssh-proxy의 `resolver → relay` 전체 경로

---

## 7. Boundaries

### Always do
- PR 오픈 전 `make lint && make test` 녹색
- DB 스키마 변경은 **goose 마이그레이션 + 롤백 검증**까지 한 PR에
- 새 API 엔드포인트는 OpenAPI 또는 proto 계약을 먼저 갱신하고 구현
- 시크릿은 `.env.local`(git 제외) 또는 환경변수; 코드·커밋에 금지
- 사용자 입력은 Zod(프론트) / proto·validator(백엔드) 경계에서 검증
- 가정을 발견 즉시 문서화 (`docs/adr/`의 새 ADR 또는 스펙 업데이트)

### Ask first
- **새 외부 의존성** 추가 (Go 모듈·NPM 패키지 모두 — 보안 감사 때문에)
- **DB 스키마 호환성 깨는 변경** (컬럼 삭제·타입 변경)
- **공개 API(REST, gRPC, 프론트↔API) 파괴적 변경**
- **새 시스템·서비스 도입** (Redis, Kafka, K8s 등)
- **스코프 확장** — Phase 2/3 기능으로 분류된 것(마켓 정산·동적 패킹·MIG·컨테이너 등) 중 하나를 Phase 1에 넣고 싶을 때
- **가정 검증을 건너뛰고 구현 착수**하려 할 때

### Never do
- Phase 2/3 기능(동적 패킹, MIG, 컨테이너, 마켓 정산, 개인 노드 등록 플로우)을 Phase 1 코드베이스에 조용히 포함
- **"1 VM = 1 노드" 단순화** 제안·도입 (상품성 근간 훼손; 2026-04-23 명시 제약)
- 실제 GPU·libvirt 미테스트 상태로 라이프사이클 코드 머지
- 실패하는 테스트를 스킵/삭제하여 CI 통과시키기
- 시크릿(특히 DB 비번, LLM·결제 키) 커밋
- 프론트엔드에 관리자 전용 엔드포인트 노출 (권한 검사는 서버 권위)
- `vendor/`·생성물(`*.pb.go`, sqlc 생성물) 수동 편집

---

## 8. Success Criteria

Phase 1이 "완료"되려면 **전부** 충족:

### 기능(Functional)
- [ ] **F1. E2E 인스턴스 생성:** 인증된 사용자가 웹에서 크레딧 차감 확인 후 `Create` 클릭 → 60초 이내 `running` 상태 전환 및 SSH 정보 표시
- [ ] **F2. SSH 접속:** `ssh ubuntu@{random}.hybrid-cloud.com`로 접속 성공. 게스트 내 `nvidia-smi`로 할당된 GPU 개수·모델 확인 가능
- [ ] **F3. 멀티 GPU 분할 프로파일:** 단일 4-GPU 노드에 `1·1·1·1`, `2·2`, `4` 프로파일 각각 적용한 VM이 정상 기동·격리·벤치마크
- [ ] **F4. NVLink 존중:** 2 GPU VM 프로파일에서 페어 GPU가 같은 NVLink 도메인에 배치됨을 `nvidia-smi topo -m`로 확인
- [ ] **F5. 인스턴스 삭제 및 슬롯 회수:** VM 삭제 후 동일 슬롯에 새 VM 생성 시 이전 메모리 잔존 없음 (마커 테스트 통과)
- [ ] **F6. 크레딧 결제 흐름:** 내부 운영자가 사용자 크레딧을 수동 충전; 인스턴스 러닝 시간만큼 초당 크레딧 차감 로그 기록
- [ ] **F7. 관리자 대시보드:** 노드 상태·GPU 슬롯 점유율·사용자 크레딧·인스턴스 목록 확인·인스턴스 강제 종료

### 비기능(Non-functional)
- [ ] **N1. 가용성:** 30일 연속 관찰 기간 동안 인스턴스 가용성 ≥ 99.0% (`running → failed` 사고율 기준)
- [ ] **N2. 생성 레이턴시:** p50 45초 이하, p95 90초 이하 (`create` API 호출 → `running` 상태)
- [ ] **N3. SSH 프록시 처리량:** 동시 100 SSH 세션에서 추가 지연 p95 ≤ 30ms, 세션 드롭율 ≤ 0.1%
- [ ] **N4. 성능 손실:** 멀티 VM 공존 시 각 VM의 GPU 벤치마크가 바닥 수치 대비 ≤ 5% 하락
- [ ] **N5. 관찰성:** Prometheus·Grafana 대시보드에 노드 GPU 온도·전력·슬롯 점유율, API 요청 지연, SSH 세션 수가 실시간 노출

### 보안(Security)
- [ ] **S1. 테넌트 격리:** VM 간 직접 통신 불가 (네트워크 격리 확인)
- [ ] **S2. GPU 상태 누출 없음:** VM 종료 후 다음 VM이 이전 VM의 GPU 메모리·상태를 읽을 수 없음 (자동 리셋)
- [ ] **S3. 관리자 권한 분리:** 일반 사용자 토큰으로 관리자 엔드포인트 접근 시 403
- [ ] **S4. 비밀 관리:** 코드·로그에 API 토큰·DB 비번 하드코드 없음 (gitleaks CI 통과)

### 가정 검증(Assumption Closure)
one-pager의 검증 대상 가정이 구현 착수 **전**에 처리되어야 함:

- [ ] **A1. 구매자 수익화 의향** — 30명 인터뷰, 결과 문서화 (`docs/research/`)
- [ ] **A2. 국내 수요·가격** — 10명 수요자 인터뷰, 결과 문서화
- [ ] **A3. KVM VM 워크로드 적합성** — 내부 테스트 + 베타 파트너 NPS
- [ ] **A4. DC 가동률 48h 스트레스 테스트** 통과 문서
- [ ] **A5. GPU passthrough·분할 벤치마크** 결과 (2 VM 동시 기동)
- [ ] **A6. VM 간 GPU 재할당 청결성 테스트** 통과
- [ ] **A7. SSH 프록시 100 세션 부하 테스트** 결과
- [ ] **A8. 베타 파트너 풀 규모 확인** (구매 DB 세그먼트)

### 운영(Operational Readiness)
- [ ] **O1. 런북 문서:** 노드 장애·VM 행·SSH 프록시 크래시·DB 장애 대응 절차 `docs/runbooks/`
- [ ] **O2. 장애 훈련:** 각 런북 중 2개 이상 시뮬레이션 훈련 기록
- [ ] **O3. 백업:** PostgreSQL 일일 백업·복구 리허설 1회 이상
- [ ] **O4. 롤백 절차:** Docker Compose 서비스·compute-agent systemd 각각의 롤백 스크립트

---

## 9. Open Questions

결정 전·결정 시점을 명시:

| # | 질문 | 결정 필요 시점 | 참고 |
|---|------|--------------|------|
| Q1 | 상품명·도메인 확정 (`hybrid-cloud.com`은 예시인가?) | 프론트엔드 착수 전 | 브랜딩 |
| Q2 | 과금 모델 최종형: 선불 크레딧 단일 vs 월정액 옵션 | §8 F6 구현 전 | A2 인터뷰 결과에 연동 |
| Q3 | `compute-agent` ↔ `main-api` 인증: mTLS vs Bearer 토큰 vs 둘 다 | gRPC 계약 확정 전 | Phase 2 NAT 환경에 mTLS가 복잡할 수 있음 |
| Q4 | GPU 프로파일 정책: 관리자 고정 vs 사용자 요청 시 동적 전환 | 스케줄러 설계 시 | Phase 1은 관리자 고정 권장 |
| Q5 | NVLink 보장을 가격 티어로 구분할지 ("NVLink 페어 티어" 추가) | 요금 모델 확정 시 | A2 수요자 인터뷰에서 확인 |
| Q6 | 한 VM 내부에서 사용자가 root 권한 확보 가능 범위 (modprobe, kernel 빌드 등) 정책 | 게스트 이미지 설계 시 | 보안·지원 범위에 영향 |
| Q7 | 로그·메트릭 장기 보존 기간 & Loki/Grafana 호스팅(자체 vs 관리형) | 운영 준비 시 | 비용·규정 |
| Q8 | 데이터센터 네트워크 공인 IP 범위·대역폭 보장 | N1 검증 전 | 인프라 벤더 계약 |

---

## 10. 리뷰·승인

구현 착수 전 체크:
- [ ] 스펙 6개 코어 영역 커버 — Objective / Stack+Commands / Structure / Code Style / Testing / Boundaries
- [ ] Success Criteria가 구체적·테스트 가능
- [ ] Boundaries의 Always/Ask First/Never 명확
- [ ] 저장소에 커밋: `docs/specs/phase-1-mvp.md`
- [ ] 사용자(프로젝트 오너) 리뷰·승인

다음 Phase로 이행:
- Phase 2 (Plan): 본 스펙을 바탕으로 기술 구현 계획 — 모듈 의존성, 순서, 병렬화 가능성 → `/agent-skills:plan`
- Phase 3 (Tasks): 구현 단위 작업 분해 → `/agent-skills:plan` 산출물에서 이어 받음
- Phase 4 (Implement): 하나씩 실행 → `/agent-skills:build` (TDD 기반)
