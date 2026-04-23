# hybrid-cloud

하이브리드 GPU 클라우드 — 자체 데이터센터 + 개인 워크스테이션을 하나의 플랫폼으로 묶어 KVM 기반 GPU VM 인스턴스를 판매.

## 상태

Phase 1 MVP — 구현 중 (GPU 없는 단계까지). 전체 계획은 [tasks/plan.md](./tasks/plan.md), 스펙은 [docs/specs/phase-1-mvp.md](./docs/specs/phase-1-mvp.md).

## 레이아웃

```
services/
  compute-agent/   GPU 노드에 설치되는 에이전트
  main-api/        중앙 API · 스케줄러 · DB
  ssh-proxy/       도메인 기반 SSH 라우팅
web/frontend/      Next.js 15 웹 UI
proto/             gRPC 계약 (생성물은 shared/proto)
shared/            서비스 공통 코드 · 생성된 proto 스텁
infra/             docker-compose · prometheus · grafana 설정
docs/              ideas · specs · ADR · 런북
tasks/             plan.md · todo.md
```

## 개발 환경

필요한 도구:

- Go 1.24+
- Node 20+ / pnpm 10+
- Docker (로컬 postgres 용)
- `go install` 로 설치: `github.com/golangci/golangci-lint/cmd/golangci-lint@v1.62.2`, `github.com/pressly/goose/v3/cmd/goose@latest`, `github.com/sqlc-dev/sqlc/cmd/sqlc@latest`, `google.golang.org/protobuf/cmd/protoc-gen-go@latest`, `google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest`

## Makefile

| target | 설명 |
|---|---|
| `make dev-db` | postgres docker 기동 |
| `make build` | Go 서비스 + 프론트엔드 빌드 |
| `make test` | 유닛 테스트 전부 |
| `make lint` | golangci-lint + eslint |
| `make proto` | proto 스텁 재생성 |
| `make migrate-up` / `migrate-down` | goose 마이그레이션 |
| `make sqlc` | sqlc 코드 재생성 |

`make help` 로 전체 목록 확인.

## 환경 변수

`.env.example` 복사해서 `.env.local` 로 사용. 각 서비스는 자기 prefix (`MAIN_API_*`, `AGENT_*`, `SSH_PROXY_*`) 만 읽음.
