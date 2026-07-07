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

---

## 7. M0-S2 增量契约（ARCH-02 / WX-02 / AI-01 / INFRA-07，2026-07-07）

M0-S2 在 M0-S1 地基上落地四块，全部沿用「可选依赖 + 接口 + 内存实现兜底 + mock 测试」原则：本机无 Redis/无 wx appid/无 LLM key/无腾讯云账号时，`go build ./... && go test -race ./...` 仍全绿。

### 7.1 新增依赖（go.mod）

| 依赖 | 用途 | 备注 |
|---|---|---|
| `github.com/golang-jwt/jwt/v5` | ES256 签发/验签 | 白名单内 |
| `github.com/redis/go-redis/v9` | refresh token / jti 黑名单 / AI 预算 | 白名单内 |
| `github.com/alicebob/miniredis/v2` | Redis 实现 SessionStore/Budget 的单测 | **test-only** |

> POC 刻意未引入 `sashabaranov/go-openai`（AI 网关用纯 net/http 直连 OpenAI 兼容端点）与 `silenceper/wechat`（code2session 一个 GET，自写更轻）。需要时由主线程评估再加。

### 7.2 包所有权与契约

- **`pkg/jwt`**（ARCH-02）：ES256 自签。`Claims` 对齐技术方案 §6.2.4（scene/tenant_id/store_id/role/openid/ver/jti）。`Signer.Sign`/`Verifier.Verify`/`Manager`。密钥管理：`DevKey(seed)` **确定性派生** P-256 私钥（注：go 1.25 `ecdsa.GenerateKey` 文档明确其输出不依赖于读取的字节，故不能用它做确定性 key——改直接派生标量 D 再 `ScalarBaseMult`）；staging/prod 用 `LoadPrivateKeyFile`。`SessionStore` 接口（`IssueRefresh/GetRefresh/RevokeRefresh/BlockJTI/IsBlocked`）+ `MemorySessionStore`（stdlib，惰性过期+后台 sweep）+ `RedisSessionStore`（`redis.Cmdable`）。refresh token 为 opaque，access token 走 ES256。
- **`pkg/redisx`**：`New(ctx, cfg.Redis)` 返回惰性 `*Client`（内嵌 `*redis.Client`，满足 `redis.Cmdable`），`Ping` 决定可用性；镜像 `pkg/db` 的「缺失降级」语义。
- **`pkg/wxsdk`**（WX-02）：纯 net/http 封装 `jscode2session`（`Code2Session(ctx, appID, secret, code)`→`{OpenID,SessionKey,UnionID}`），`WithBaseURL` 便于 httptest。`Code2SessionError.IsWellKnownInvalid()` 区分调用方错误(40029/40163…)与上游故障。
- **`internal/auth`**：`AuthService` 编排 login/refresh/logout；`ConsumerRepo` 接口 + `MemoryConsumerRepo`。**session_key 永不下发前端**（缓存于服务端 Consumer 记录；真实 DB 落地后迁 `wx_sessions` 表）。`Code2Sessioner` 接口便于注入 stub。
- **`interfaces/http/middleware/auth.go`**：`Auth(verifier, blocker)` 中间件——Bearer→验签→查 jti 黑名单→注入 `multitenancy.Context`（**tenant 只从 JWT claims 取**）。`MockTenant` 保留为 dev-only，由 `multitenancy.dev_mock_header` 开关。
- **`interfaces/http/handler/auth.go`**：`/v1/wxshop/auth/{login,refresh,me,logout}`。`httpx.FailError` 把 service 返回的 `errcode.Err` 透明映射。
- **`internal/aiengine/gateway`**（AI-01）：`Provider` 接口 + `OpenAIProvider` + `NewQwen/NewDeepSeek/NewDoubao`（baseURL/key/model，均标注「需核实」）。`Pricing` 表 + `Budget`（memory/redis，月¥2/户熔断 `ErrBudgetExceeded`）。`Gateway.Chat`（tier 路由+预算+计费）+ `ChatWithFallback`（lite→V3 降级链）。**Dify 按 §7.5 决策不引入**（`DifyStub` 承载架构理由）。`cmd/llmbench` 成本压测 CLI（需真实 key，不入 CI）。
- **`infra/terraform`**（INFRA-07）：腾讯云 VPC+CVM+CLB+COS+TDSQL-C(CynosDB)+Redis+CLS 的 staging 栈。密钥全走变量（`*.tfvars` 已入 .gitignore，仅提交 `*.tfvars.example`）。本机无 terraform 二进制，README 文档化 `init/validate/plan` 验证门。

### 7.3 配置变更（types.go / config.go）

- `JWTConfig` 增 `KeyFile`/`KeyID`（`Secret` 保留但 ES256 不依赖它）。
- 新增 `WXConfig`（app_id/secret/code2session_url/timeout）、`AIConfig`（providers map/request_timeout/monthly_budget_cny）。
- `MultitenancyConfig` 增 `DevMockHeader`（默认 false；`config.dev.yaml` 置 true 保住 curl 演示）。
- **已修 staging `${VAR}` 占位缺口**：`config.Load` Layer 2 改为「自行定位 `config.<env>.{yaml,yml}` → `os.ExpandEnv` → `v.ReadConfig`」。

### 7.4 DoD（M0-S2）

- `go build ./... && go vet ./... && gofmt -l .`（空）`&& go test -race ./...` 全绿。✅
- 真实二进制冒烟（`go run ./cmd/api` + curl）：`/v1/boss/whoami`+`X-Mock-Tenant`→200（dev mock 保留）；`/v1/wxshop/auth/me` 无 token→40101（JWT 强制）；login 无 tenant→40001。✅
- 串户证明（HTTP 层）：tenant-50021 的 token 携 `X-Mock-Tenant:9999` 访问 `/auth/me`→仍返回 50021（tenant 只来自 JWT）。✅
- Terraform：`infra/terraform` 结构完整；`terraform init/validate/plan` 为首次 apply 前的验证门（本机无二进制）。⚠️ 待有 terraform 的机器上跑通。

### 7.5 显式延后（不在 M0-S2 范围）

- **BE-A-01 商家注册/登录**（boss 手机号+验证码、admin 账号密码+TOTP）：boss/admin 目前走 dev MockTenant；真实登录随商家域落地。
- **Dify 编排**：§7.5 决策「0→1 不引入」，作远期实验场。
- **真实 LLM 成本压测**：`cmd/llmbench` 已就位，需真实 key 跑（凭据由运营提供）。
- **`internal/tenant` 仍空**（`.gitkeep` 已清，目录保留）：商家域/计费域落地时填充。
