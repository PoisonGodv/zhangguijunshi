# 新人第一天清单（Quickstart）

欢迎加入掌柜军师后端！本文件帮你用半天时间把环境跑通、把关键文档读完。目标：本地能 `build` + `test`，能起 HTTP 服务并命中健康端点，理解多租户护栏。

---

## 0. 前置

- Go **1.25+**（`go version` 确认）。
- （可选）Docker，用于起本地 PG/Redis/MinIO；没有也能开发，见 [第 3 步](#3-启动-http-服务) 说明。
- （可选）`golangci-lint` **v2.x**（跑 lint 用）：`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2`。

---

## 1. 克隆与定位

仓库根：`zhangguijunshi/`（monorepo）。唯一 Go module 在 `backend/`（`module github.com/zhangguijunshi/backend`）。

> CI 与所有 `go` 命令都在 `backend/` 内执行，或用根 `make` 目标自动转发。

---

## 2. 起本地依赖（可选）

```bash
# 在仓库根
make dev          # = docker compose -f infra/compose/docker-compose.yml up -d
                  # 起 postgres(pgvector/pg16) / redis7 / minio 三服务
```

无 Docker？跳过本步，直接进入下一步——服务能在「无 DB 模式」启动，`/readyz` 会报 `db: skipped`。

---

## 3. 应用迁移（仅在起了 PG 时需要）

```bash
cd backend
go run ./cmd/migrate up        # 读 migrations/ 目录，应用 000001_init_tenant_user_store
```

`migrate` 与 `api` 共用同一套分层配置（`ZG_ENV` + `ZG_POSTGRES_*`），定位数据库的方式一致。

---

## 4. 启动 HTTP 服务

```bash
cd backend
go run ./cmd/api               # 默认监听 :8080
```

冒烟验证：

```bash
curl localhost:8080/healthz    # {"code":0,"data":{"status":"alive"}}
curl localhost:8080/readyz     # {"code":0,"data":{"db":"ok"}}（无 DB 时 data.db 为 "skipped"）
curl localhost:8080/metrics    # Prometheus 指标
```

验证多租户护栏（`/whoami` 不带租户必被拒）：

```bash
# 不带 mock 头 → 403 / 40302（ErrTenantCross）
curl -i localhost:8080/v1/boss/whoami

# dev 演示：用 X-Mock-Tenant 头注入租户（仅 dev 生效）
curl -H 'X-Mock-Tenant: 1001' localhost:8080/v1/boss/whoami
# {"code":0,"data":{"tenant_id":1001,"source":"mock-tenant-header"}}
```

---

## 5. 跑测试

```bash
# 在仓库根
make test

# 或在 backend/ 内
cd backend && go test -race -count=1 ./...
```

M0-S1 单测一律 mock（`pgxmock` / `sqlmock`），不连真实 DB/Redis，本机无需任何外部依赖即可全绿。

本地检查清单（提 PR 前全过）：

```bash
make fmt && make vet && make lint-tenant && make lint && make build
```

---

## 6. 必读文档（按顺序）

1. **[ADR-0001](../adr/0001-modular-monolith-and-tenant-isolation.md)**——两个奠基决策：为何选 Go 模块化单体、为何 Day1 内建多租户隔离（含三层防护）。
2. **[backend/CONTRACT.md](../../backend/CONTRACT.md)**——后端集成硬约束总规范：硬约束、包所有权地图、依赖白名单、编码约定、各包 DoD。**写代码前必读**。
3. **[CONTRIBUTING.md](../../CONTRIBUTING.md)**——分支策略、提交规范、PR 流程、多租户硬约定、本地检查。
4. **[README.md](../../README.md)**——项目总览、技术栈、端点表、Roadmap。

---

## 7. 常见问题

- **`make lint` 报 schema 不识别 / 配置错误**：装的 golangci-lint 不是 v2.x。本仓库 `.golangci.yml` 用 v2 schema，必须装 `v2.12.2` 等 v2 版本。
- **`/readyz` 报 `db: skipped`**：正常。未起 PG 或连库失败时，服务降级为「无 DB 模式」继续运行。
- **新增仓储被 `make lint-tenant` 报错**：`tenantcheck` 发现「裸查询未先取租户」。在查询前调 `multitenancy.EnforceTenant(ctx)`（或 `TenantGuard.Wrap(ctx)`），见 [CONTRIBUTING.md 第 4 节](../../CONTRIBUTING.md#4-多租户硬约定不可违背)。
- **环境变量不生效**：前缀必须是 `ZG_`（不是 `ZGJS_`），下划线嵌套如 `ZG_POSTGRES_HOST`；`ZG_ENV` 选 `dev` / `staging` / `prod`。
