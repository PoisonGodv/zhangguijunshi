# 掌柜军师（zhanggui-junshi）

> 面向三四线个体零售的 **AI 营销军师 + 私域 SaaS**：帮水果店、母婴店、便利店、服装店、生鲜店等个体商户，用一部手机完成「营销内容生成 + 会员私域运营 + 企微/小程序带货」。

本仓库是掌柜军师的 monorepo，当前处于 **M0-S1 地基阶段**：模块化单体后端已跑通工程地基（配置 / 日志 / 多租户隔离 / 数据库 / 三端 BFF 路由骨架 / 健康与指标端点 / 迁移），并通过 `go build`、`go vet`、`go test -race` 全绿与真实二进制端到端冒烟验证。商品、订单、会员、支付、营销、企微、AI 引擎等业务能力在后续 Sprint 交付（见文末 [Roadmap](#roadmap)）。

---

## 技术栈

| 层 | 选型 |
|---|---|
| 后端 | **Go 模块化单体**（module `github.com/zhangguijunshi/backend`，go 1.25），Gin HTTP 路由 |
| 数据库 | **PostgreSQL 16 + pgvector**（向量检索，AI 推荐用） |
| 缓存 | **Redis 7** |
| 对象存储 | 腾讯云 COS（本地开发用 MinIO 作为 S3 兼容替身） |
| 可观测 | OpenTelemetry + Prometheus（`/metrics`），结构化日志 zap |
| 前端三端 | **微信小程序**（消费者端 / 商家端）+ **React 后台**（平台运营端） |
| 部署 | 单体先行，1→10 阶段预留拆分点（见 [ADR-0001](docs/adr/0001-modular-monolith-and-tenant-isolation.md)） |

> 架构决策理由详见 [docs/adr/0001-modular-monolith-and-tenant-isolation.md](docs/adr/0001-modular-monolith-and-tenant-isolation.md)。

---

## 仓库结构

```
zhangguijunshi/                 # monorepo 根
├── backend/                    # 唯一 Go module（模块化单体后端）
│   ├── cmd/                    # 入口：api（HTTP 服务）、migrate（迁移）、worker/cron（占位，后续 Sprint）
│   ├── internal/               # 业务域（tenant/catalog/crm/marketing/... 当前为占位骨架，见 Roadmap）
│   ├── interfaces/http/        # 三端 BFF 路由 + 中间件 + handler
│   ├── pkg/                    # 共享基础库：config/logger/db/multitenancy/jwt/...
│   ├── migrations/             # SQL 迁移（golang-migrate，当前 000001 租户/用户/门店域）
│   └── configs/                # config.<env>.yaml（dev / staging）
├── apps/                       # 三端前端：admin-web（React 后台）+ merchant-mp/consumer-mp/merchant-app（小程序，占位骨架）
├── ai/                         # AI 能力：gateway-adapters / playbooks / dify-workflows（占位骨架，后续 Sprint）
├── infra/                      # 基础设施：compose（docker-compose）/ docker / terraform
├── docs/                       # 文档：adr / onboarding / api / runbooks
├── .github/                    # CI 工作流与 Issue 模板（占位，后续 Sprint 接入）
├── Makefile                    # 根 Makefile，转发到 backend 模块
└── .golangci.yml               # golangci-lint v2 配置
```

---

## 快速开始

前置：Go 1.25+。若要连真实 PG/Redis，需 Docker。

```bash
# 1. 起本地依赖（postgres16+pgvector / redis7 / minio 三服务）
make dev                      # = docker compose -f infra/compose/docker-compose.yml up -d

# 2. 应用数据库迁移（在 backend/ 内，默认读 migrations/ 目录）
cd backend && go run ./cmd/migrate up

# 3. 启动 HTTP 服务（默认 :8080）
go run ./cmd/api

# 4. 冒烟验证
curl localhost:8080/healthz    # {"code":0,"data":{"status":"alive"}}
curl localhost:8080/readyz     # {"code":0,"data":{"db":"ok"}}
curl localhost:8080/metrics    # Prometheus 指标文本
```

### 无 Docker / 无 Postgres 也能起

`cmd/api` 启动时若 `ZG_POSTGRES_HOST` 为空或连库失败，会降级为「无 DB 模式」继续服务，此时 `/readyz` 报 `db: skipped`：

```bash
cd backend && ZG_POSTGRES_HOST= go run ./cmd/api   # 不连库启动
curl localhost:8080/readyz                          # {"code":0,"data":{"db":"skipped"}}
```

> M0-S1 阶段所有单测均用 mock（`pgxmock` / `sqlmock`），不依赖真实 DB/Redis。

---

## 环境变量

配置分层加载（低 → 高优先级）：**defaults → `configs/config.<env>.yaml` → 环境变量（前缀 `ZG_`，下划线嵌套）**。

- `ZG_ENV`：选环境，`dev` / `staging` / `prod`（决定读哪个 yaml，缺省 `dev`）。
- `ZG_POSTGRES_*`：`HOST` / `PORT` / `USER` / `PASSWORD` / `DATABASE` / `SSL_MODE` 等。
- `ZG_REDIS_*`、`ZG_HTTP_*`、`ZG_LOG_*`、`ZG_JWT_*`、`ZG_OTEL_*`、`ZG_COS_*`、`ZG_MULTITENANCY_STRICT` 等。

完整占位清单见 [.env.example](.env.example)（**仅占位，不含真实密钥**；生产密钥运行时由腾讯云 SSM/KMS 注入）。

---

## 三端 BFF 前缀与端点

三端各自独立路由前缀，租户身份**永远从已校验上下文取**，不从请求体取。

| 端 | 路由前缀 | 说明 |
|---|---|---|
| 消费者（微信小程序） | `/v1/wxshop` | C 端用户触达 |
| 商家（小程序/后台） | `/v1/boss` | 店主/店长/店员操作 |
| 平台运营（React 后台） | `/v1/admin` | 平台侧运营管理 |

基础设施端点（不挂租户、不鉴权）：

| 方法 | 路径 | 返回 |
|---|---|---|
| GET | `/healthz` | 进程存活，恒 200，`{"status":"alive"}` |
| GET | `/readyz` | 就绪探针，`data.db` = `ok` / `down` / `skipped` |
| GET | `/metrics` | Prometheus 指标 |

每端含一个 `/whoami` 演示端点，验证多租户护栏：**不带租户上下文必被拒**（`ErrTenantCross`，HTTP 403，code 40302）。

```bash
# 不带 mock 头 → 被多租户护栏拒绝（403 / 40302）
curl -i localhost:8080/v1/boss/whoami

# dev 演示：用 X-Mock-Tenant 头注入租户（仅 dev 生效，staging/prod 禁用）
curl -H 'X-Mock-Tenant: 1001' -H 'X-Mock-Store: 2001' localhost:8080/v1/boss/whoami
# {"code":0,"data":{"tenant_id":1001,"source":"mock-tenant-header"}}
```

---

## 多租户隔离

多租户串户是本项目**第一号风险**。Day1 即内建行级隔离，三层防护：

1. **运行时单一入口**：所有仓储调用必须先经 `pkg/multitenancy.EnforceTenant(ctx)`（或 `TenantGuard.Wrap(ctx)`）取 `tenant_id`，否则返回 `ErrNoTenant`；上下文里没租户就**绝不放行任何查询**。
2. **静态分析闸门**：`tenantcheck` 分析器（`pkg/multitenancy`）扫描 `internal/**`，标记「裸查询未先取租户」的仓储函数，CI 经 `make lint-tenant`（`go vet ./...`）执行。
3. **串户测试矩阵**：`guard_test.go` 覆盖「无 ctx / 有 ctx / system 上下文」等路径。

`tenant_id`（== `merchant_id`）的来源：**dev** 用 `X-Mock-Tenant` 头演示；**staging/prod** 必从已校验的 JWT claims 注入（JWT 鉴权在 M0-S2 交付，当前 dev 用 mock 头占位）。

> 新增任何仓储代码，**必须**先调 `EnforceTenant`，否则 `tenantcheck` 直接报错、CI 拦截。详见 [CONTRIBUTING.md](CONTRIBUTING.md)。

---

## Makefile 目标速查

根目录 `make <target>`（等价于 `cd backend && go ...`）：

| 目标 | 作用 |
|---|---|
| `make dev` | 用 docker-compose 起本地三依赖（postgres/redis/minio） |
| `make build` | `go build ./...` |
| `make test` | `go test -race ./...` |
| `make lint` | `golangci-lint run ./...`（需 v2.x，见下） |
| `make vet` | `go vet ./...` |
| `make fmt` | `go fmt ./...` |
| `make tidy` | `go mod tidy`（仅编排脚本跑，禁止 agent 并发跑） |
| `make migrate-up` | 应用迁移：`go run ./cmd/migrate up` |
| `make migrate-down` | 回滚最近一次迁移 |
| `make lint-tenant` | 多租户隔离闸门（`go vet ./...`，含 `tenantcheck`） |
| `make help` | 列出所有目标 |

> **lint 前置**：`.golangci.yml` 用 v2 schema，必须装 golangci-lint **v2.x**（如 `v2.12.2`），否则 lint 会因 schema 不识别失败：
> ```bash
> go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
> ```

---

## 测试

```bash
make test        # go test -race ./...（根目录）
# 或在 backend/ 内：
cd backend && go test -race -count=1 ./...
```

M0-S1 单测一律 mock（`pgxmock` / `sqlmock` / 标准库），不连真实 DB/Redis；覆盖 config 分层加载、logger、多租户 guard + 串户矩阵、`tenantcheck` analyzer、`db.InTx`、router 端到端。

---

## Roadmap

**当前 M0-S1 仅地基**。以下为后续 Sprint 计划（详见计划书与技术方案），**尚未实现**：

- 业务域：商品（catalog）、订单/交易、会员（membership / crm）、支付（payment）、营销（marketing）、计费（billing）、消息通知（notify）、数据分析（analytics）
- 鉴权：JWT + RBAC（替换当前 dev 的 mock 头）、微信小程序登录
- 异步：`cmd/worker`（消费者）/ `cmd/cron`（定时任务）
- 微信生态：企业微信集成、小程序数据采集
- AI 引擎：五段式管线 + 毛利护栏（`ai/` 目录），向量检索（pgvector）驱动的推荐与内容生成
- 前端三端：`apps/` 下 React 后台与微信小程序
- CI/CD：`.github/workflows` 流水线、Issue 模板（PR 模板已就位于 [.github/PULL_REQUEST_TEMPLATE.md](.github/PULL_REQUEST_TEMPLATE.md)）

---

## 相关文档

- 项目计划书：[../掌柜军师-计划书.md](../掌柜军师-计划书.md)
- 技术实施方案（10 章 + WBS）：[../掌柜军师-技术方案/](../掌柜军师-技术方案/)
- 后端集成契约：[backend/CONTRACT.md](backend/CONTRACT.md)
- 架构决策记录：[docs/adr/0001-modular-monolith-and-tenant-isolation.md](docs/adr/0001-modular-monolith-and-tenant-isolation.md)
- 新人第一天清单：[docs/onboarding/quickstart.md](docs/onboarding/quickstart.md)
- 贡献指南：[CONTRIBUTING.md](CONTRIBUTING.md)
