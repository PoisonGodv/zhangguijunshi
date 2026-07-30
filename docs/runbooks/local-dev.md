# 本地开发 Runbook

掌柜军师本地开发环境手册。覆盖「无 Docker」与「有 Docker」两条路径，以及三端前端的本地起法。

> 后端唯一 Go module 在 `backend/`（`module github.com/zhangguijunshi/backend`，go 1.25）。所有 `go` 命令在 `backend/` 内执行，或用根 `make` 目标自动转发。

---

## 1. 前置

- Go **1.25+**（`go version` 确认）。
- （可选）Docker，用于起本地 PG/Redis/MinIO；**没有也能开发**（见第 2 节）。
- （可选）`golangci-lint` **v2.x**（跑 lint 用）：`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2`。
- Node 20+（admin-web 用）。
- 微信开发者工具（consumer-mp 用）。

> 本机无 Docker 是常态：`Makefile` 的 `make dev` 只定义目标、不强制执行；compose 与 Dockerfile 作为可交付产物，实际部署在有 Docker 的服务器上跑（见 [deploy.md](deploy.md)）。

---

## 2. 无 Docker 开发（纯 Go）

dev 配置默认走 **memory 实现**：`cmd/api` 启动时若 `ZG_POSTGRES_HOST` 为空或连库失败，所有仓储降级为内存实现（`main.go`：`database != nil` 才用 PG repo，否则 memory）。

```bash
# 在仓库根
make build              # = cd backend && go build ./...
make test               # = cd backend && go test -race ./...
make vet                # = cd backend && go vet ./...
make fmt                # = cd backend && go fmt ./...

# 起服务（无 DB 模式）
cd backend
ZG_POSTGRES_HOST= go run ./cmd/api     # 监听 :8080
```

冒烟：

```bash
curl localhost:8080/healthz           # {"code":0,"data":{"status":"alive"}}
curl localhost:8080/readyz            # {"code":0,"data":{"db":"skipped"}}  ← 无 DB 时
curl localhost:8080/metrics           # Prometheus 文本
```

dev 多租户护栏演示（dev 才开 `X-Mock-Tenant` 头，`config.dev.yaml` `dev_mock_header: true`）：

```bash
curl -i localhost:8080/v1/boss/whoami                                    # 403 / 40302
curl -H 'X-Mock-Tenant: 1001' -H 'X-Mock-Store: 2001' localhost:8080/v1/boss/whoami
# {"code":0,"data":{"tenant_id":1001,"source":"mock-tenant-header"}}
```

> M0-S1 起所有单测一律 mock（`pgxmock` / `sqlmock` / 标准库），不依赖真实 DB/Redis，本机无需任何外部依赖即可全绿。

---

## 3. 有 Docker 开发（起真实依赖）

```bash
# 起本地三依赖（postgres16+pgvector / redis7 / minio）
docker compose -f infra/compose/docker-compose.yml up -d

# 应用迁移（golang-migrate，读 backend/migrations/）
cd backend
go run ./cmd/migrate up

# 起 API，连本地 PG
ZG_POSTGRES_HOST=localhost go run ./cmd/api
```

依赖端口（与 `.env.example` / `config.dev.yaml` 对齐）：

| 服务 | 容器 | 端口 |
|---|---|---|
| PostgreSQL 16 + pgvector | `zg-postgres` | `5432`（user/pass/db：`zg`/`zg`/`zhanggui`） |
| Redis 7 | `zg-redis` | `6379` |
| MinIO（COS S3 兼容替身） | `zg-minio` | `9000`（S3 API）/ `9001`（控制台，root `minio`/`minio123`） |

迁移命令：

```bash
go run ./cmd/migrate up            # 应用全部
go run ./cmd/migrate down 1        # 回滚最近一次
go run ./cmd/migrate version       # 当前 schema 版本
go run ./cmd/migrate force <v>     # 解 dirty
```

收尾：

```bash
docker compose -f infra/compose/docker-compose.yml down      # 停并删容器（保留卷）
docker compose -f infra/compose/docker-compose.yml down -v   # 连数据卷一起删
```

---

## 4. admin-web（管理后台，React + Vite）

```bash
cd apps/admin-web
npm i
npm run dev          # vite dev server，:5173
```

- dev server `host=0.0.0.0`、`port=5173`，`/v1` 代理到 `http://localhost:8080`（见 `vite.config.ts`，**避免 CORS**）。
- dev 走 mock 鉴权头（`X-Mock-Tenant: 1` 等，见 `src/services/request.ts`），与后端 dev 的 `dev_mock_header: true` 配合；BE-A-01 真登录落地前用此方式。
- 类型检查 / 构建：

```bash
npm run typecheck    # tsc --noEmit
npm run build        # tsc -b && vite build → dist/
```

> 生产容器化见 `apps/admin-web/Dockerfile`（nginx 静态站 + `/v1/` 同源反代），部署细节见 [deploy.md](deploy.md)。

---

## 5. consumer-mp（消费者小程序）

用**微信开发者工具**打开 `apps/consumer-mp/`。

- 业务接口已接通真实后端（`USE_MOCK = false`，见 `services/*.ts`，CONTRACT §16）。`project.config.json` 的 `setting.urlCheck` 为 `false`，允许 dev 直连本地后端。
- mock 开关在每个 service 文件顶部，如 `apps/consumer-mp/services/member.ts`：
  ```ts
  const USE_MOCK = false   // 改 true 走 mock 占位（后端未起/离线兜底）
  ```
  真接口失败时，读操作会降级回 mock 占位（不阻断页面）；写操作（如参团 `join`）不降级，失败直接抛错。
- 登录链路：`wx.login` → `POST /v1/wxshop/auth/login` → JWT，401 自动续期（见 `services/auth.ts`）。
- 本地起后端后再用开发者工具预览：`ZG_POSTGRES_HOST=localhost go run ./cmd/api`（或无 DB 模式）。

---

## 6. 常用 make 目标

| 目标 | 作用 |
|---|---|
| `make dev` | docker compose 起本地三依赖（本机无 Docker 时跳过） |
| `make build` | `cd backend && go build ./...` |
| `make test` | `cd backend && go test -race ./...` |
| `make vet` | `cd backend && go vet ./...` |
| `make fmt` | `cd backend && go fmt ./...` |
| `make lint` | `cd backend && golangci-lint run ./...`（需 v2.x） |
| `make lint-tenant` | 多租户隔离闸门（`go vet ./...`，含 `tenantcheck`） |
| `make migrate-up` | `cd backend && go run ./cmd/migrate up` |
| `make migrate-down` | 回滚最近一次迁移 |
| `make tidy` | `go mod tidy`（仅编排脚本跑，禁止 agent 并发跑） |
| `make help` | 列出所有目标 |

提 PR 前本地检查：

```bash
make fmt && make vet && make lint-tenant && make lint && make build && make test
```

---

## 7. 常见问题

- **`make lint` 报 schema 不识别**：装的 golangci-lint 不是 v2.x。本仓 `.golangci.yml` 用 v2 schema，必须装 `v2.12.2` 等 v2 版本。
- **`/readyz` 报 `db: skipped`**：正常。未起 PG 或连库失败时降级为无 DB 模式。
- **新增仓储被 `make lint-tenant` 报错**：`tenantcheck` 发现「裸查询未先取租户」。查询前先调 `multitenancy.EnforceTenant(ctx)`（或 `TenantGuard.Wrap(ctx)`）。
- **环境变量不生效**：前缀必须 `ZG_`（不是 `ZGJS_`），下划线嵌套如 `ZG_POSTGRES_HOST`；`ZG_ENV` 选 `dev` / `staging` / `prod`。
- **admin-dev 调 API 跨域**：dev 必须走 vite 代理（`/v1` → `:8080`），不要在代码里写绝对地址直连 `:8080`。
