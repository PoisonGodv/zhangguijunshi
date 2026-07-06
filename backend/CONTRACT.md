# backend · M0-S1 集成契约 (CONTRACT)

> 本文件是 M0-S1 地基阶段多 agent **并行编码的共同规范**。所有 agent 必须先读本文件，再读自己负责的章节。目标是 `go build ./... && go test ./...` 全绿。

---

## 0. 不可违背的硬约束（来自计划书 + 技术方案）

- **Go 1.22+ 模块化单体**，禁止 0→1 上微服务/K8s。
- **Day1 内建 tenant_id 行级隔离**：每个仓储调用必须先取 tenant，否则报错。串户=灾难。
- **module path 固定**：`github.com/zhangguijunshi/backend`。
- **本机无 Docker / 无 Postgres / 无 migrate CLI**：所有测试用 `database/sql` 的 `sqlmock` 或纯标准库，**禁止写需要真实 DB/Redis 的集成测试**。docker-compose.yml 只产出文件不运行。
- 配置分层：`defaults → configs/<env>.yaml → 环境变量(前缀 ZG_，下划线嵌套，如 ZG_POSTGRES_HOST)`。

## 1. 已存在的共享类型（主线程已写好，**不要改**，直接 import 用）

### `pkg/config/types.go`（节选）
```go
type Config struct {
    App          AppConfig          // .Name .Env(dev|staging|prod) .Version
    HTTP         HTTPConfig         // .Host .Port .ReadTimeout .WriteTimeout .ShutdownTimeout (time.Duration)
    Postgres     PostgresConfig     // .Host .Port .User .Password .Database .SSLMode .MaxConns .MinConns .MaxConnLifetime .MaxConnIdleTime
    Redis        RedisConfig        // .Host .Port .Password .DB .PoolSize
    Log          LogConfig          // .Level .Encoding(json|console) .OutputPaths []string
    JWT          JWTConfig          // .Secret .AccessTTL .RefreshTTL .Issuer
    OTel         OTelConfig         // .Enable .Endpoint .ServiceName .SampleRatio .Insecure
    COS          COSConfig          // .SecretID .SecretKey .Region .Bucket .Endpoint
    Multitenancy MultitenancyConfig // .Strict bool
}
```
所有字段带 `mapstructure:"..."` tag，viper Unmarshal 用。

### `pkg/multitenancy/context.go`（节选）
```go
var ErrNoTenant = errors.New("multitenancy: tenant_id missing in context")

type Context struct {
    TenantID int64   // == merchant_id
    StoreID  int64
    UserID   int64
    Role     string
    Source   string  // consumer | merchant | admin | system
}
func WithContext(ctx context.Context, c Context) context.Context
func FromContext(ctx context.Context) (Context, bool)
func MustFromContext(ctx context.Context) Context
func TenantID(ctx context.Context) (int64, error)   // 仓储调用的唯一 chokepoint
const SourceConsumer/SourceMerchant/SourceAdmin/SourceSystem
```

## 2. 包所有权地图（agent 文件集互斥，禁止越界写文件）

| Agent | 拥有的文件（只准写这些） | 技术方案依据 |
|---|---|---|
| **A1 migrations** | `backend/migrations/000001_init_tenant_user_store.up.sql` + `.down.sql` | ch05 DDL |
| **A2 config+logger** | `backend/pkg/config/{config.go, config_test.go}` + `backend/pkg/logger/{logger.go, logger_test.go}` | ch01 §1.3 / §1.4 |
| **A3 multitenancy** | `backend/pkg/multitenancy/{guard.go, lint.go, lint_test.go, guard_test.go}`（context.go 已存在，禁改） | ch02 多租户强制隔离 |
| **A4 db** | `backend/pkg/db/{db.go, tx.go, db_test.go}` | ch02 / ch05 |
| **A5 infra+工具链** | `infra/compose/docker-compose.yml`、`backend/configs/config.dev.yaml`、`backend/configs/config.staging.yaml`、根 `Makefile`、根 `.gitignore`、`.editorconfig`、`.gitattributes`、根 `.golangci.yml`、`.env.example`、`backend/Makefile` | ch01 §1.1 / §1.5 / §1.6 |

> 主线程（非 agent）负责接线：`cmd/api/main.go`、`interfaces/http/{router.go, middleware/*.go}`、`go mod tidy`、编译修复。**agent 不要写这些路径。**

## 3. 允许的第三方依赖（import path 白名单，版本由 `go mod tidy` 解析）

```
github.com/gin-gonic/gin                 # HTTP 路由（技术方案 API-01 指定）
github.com/spf13/viper                   # 配置
github.com/jackc/pgx/v5                  # PG 驱动 (含 pgxpool)
github.com/jackc/pgx/v5/pgconn           # pgx 错误类型
github.com/redis/go-redis/v9             # Redis
go.uber.org/zap                          # 结构化日志
go.opentelemetry.io/otel                 # 可观测
go.opentelemetry.io/otel/sdk
go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp
go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin
github.com/golang-jwt/jwt/v5             # JWT（M0-S2 用，M0-S1 可暂不引）
github.com/go-playground/validator/v10   # 请求校验
github.com/golang-migrate/migrate/v4     # 迁移（作为库使用）
github.com/DATA-DOG/go-sqlmock           # repo 单测（本机无 DB 时用；注意 pgx 用 pgxmock 见下）
github.com/pashagolub/pgxmock/v4         # pgx 专用 mock（A4/db 测试首选）
github.com/stretchr/testify              # 断言
```
**不要引入白名单外的依赖**；如确有必要，在交付说明里标注理由，由主线程决定。

## 4. 编码约定（全员一致，避免接线失败）

1. **context 必传**：所有可能跨调用的函数第一参数 `ctx context.Context`。
2. **错误包装**：`fmt.Errorf("xxx: %w", err)`，保留错误链；定义哨兵错误用 `errors.New`。
3. **日志字段**：固定字段名 `trace_id` / `tenant_id` / `merchant_id` / `store_id` / `user_id`。zap 用 `zap.Int64("tenant_id", ...)`。
4. **time.Duration**：配置里的 TTL/Timeout 用 `time.Duration`，YAML 里写人类可读如 `"30s"` / `"720h"`（viper 解析）。
5. **构造函数**：每个包暴露 `New(cfg ...) (*T, error)`；失败返回 error 不 panic（除非 Must 前缀）。
6. **不输出密码/secret 到日志**：连接串打日志时 mask。
7. **包注释**：每个 `package xx` 上方 `// Package xx ...` 文档注释（`go vet`/golint 友好）。
8. **go fmt**：交付代码须 `gofmt` 干净。

## 5. 各 agent 的 DoD（完成判据）

- **A1**：两个 SQL 文件，`up.sql` 创建 tenant/user/store(含索引)；`down.sql` 逆序 drop；字段对齐 ch05（tenant_id bigint / created_at timestamptz 等）。DDL 须是合法 PostgreSQL 16 语法。
- **A2**：`config.Load()` 能从「默认值+yaml+env」分层加载并返回 `*Config`（用 fake yaml/env 单测验证，**不读真实文件系统以外**）；`logger.New(cfg.Log)` 返回 `*zap.Logger`，`FromCtx(ctx)` 能取出带 tenant 的 logger。单测全绿。
- **A3**：`guard.go` 提供 `EnforceTenant(ctx) (int64, error)`（无 tenant 返回 ErrNoTenant）+ 一个 `Scoped` 仓储包装器，串户测试矩阵（无ctx/有ctx/system上下文）绿；`lint.go` 是一个 `go/analysis` analyzer（名 `tenantcheck`），标记 `internal/**` 里绕过 `EnforceTenant` 的裸查询，自带 analyzer 单测。
- **A4**：`db.New(ctx, cfg.Postgres)` 返回 `*DB`（包 pgxpool，构造时不强制连真实 DB——**连接是惰性的或允许失败降级**，因为本机无 PG）；`InTx(ctx, fn)` 事务封装；一个查询辅助演示 tenant 注入。用 `pgxmock` 单测验证 InTx 行为，全绿，**不连真实库**。
- **A5**：`docker-compose.yml` 含 postgres16+pgvector、redis7、minio 三服务+健康检查；`config.dev.yaml` 字段完整可被 viper 加载；`Makefile` 含 `dev/build/test/lint/migrate-up/migrate-down/lint-tenant` 目标；`.golangci.yml` 启用 govet/gofmt/staticcheck/errcheck/gosec；`.env.example` 占位齐全（无真实密钥）。

## 6. 环境备注（agent 务必知晓）

- `go version` = go1.25.1 darwin/arm64。
- **无 Docker**：不要尝试 `docker compose up`；只产出 compose 文件。
- **无 Postgres/Redis**：单测一律 mock，禁止需要 live 连接。
- 已有目录树（不要 mkdir 已存在的）：见 `find zhangguijunshi -type d`。
- 技术方案文件路径：`~/Selfproj/掌柜军师-技术方案/05_数据库与存储.md`（A1）、`01_工程地基与DevOps.md`（A2/A5）、`02_后端架构总纲.md`（A3/A4）。**只读你需要的那一章**，不要把多章塞进上下文（会降低输出稳定性）。
