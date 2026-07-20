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

---

## 8. M1-S1 增量契约（DB-02 / BE-A-02 / BE-A-05 / BE-A-06，2026-07-20）

M1-S1 把「商品 + 订单 + 当日流水」这条 MVP 核心链路打通（WBS §M1-S1）。沿用铁律：「接口 + 内存实现兜底 + mock 测试」，本机无 PG 时 `go build ./... && go test -race ./...` 仍全绿。**仓储接口已定死，实现可换（内存↔pgx）而不动 service/handler。**

### 8.1 范围与裁剪（对照 ch05）

- **纳入**：`internal/catalog`（商品 CRUD）、`internal/billing`（开单 + 当日流水汇总）。
- **裁剪**：`payment_ledger`、`reconciliation_diffs`、`product_pending` **不纳入 M1-S1**（支付对账属 M4，多源采集属 M1-S3）。`inventory_movements` 表建了但服务层暂不落流水（库存域 M1-S1 后接 BE-A-04）。
- **分区降级**：ch05 里 `orders`/`order_items`/`inventory_movements` 设计为 `PARTITION BY RANGE`。M1-S1 为可迁移性建**普通表**（不带 `PARTITION BY`），月分区滚动脚本留待后续迁移（线上数据量未到分区阈值前普通表即够）。在迁移注释里标注此决策。

### 8.2 迁移编号（DB-02）

| 文件 | 内容 |
|---|---|
| `migrations/000002_catalog_inventory.up/.down.sql` | `products` + `inventory_batches` + `inventory_movements`（普通表非分区）。复用 000001 的 `trg_set_updated_at()`（**勿重复 CREATE FUNCTION**，可用 `CREATE OR REPLACE`）。 |
| `migrations/000003_billing_order.up/.down.sql` | `orders` + `order_items`（普通表非分区）。 |

DDL 逐字段对齐 ch05 §5.3.2/§5.3.3；金额一律 `BIGINT` 分；`orders`/`order_items`/`inventory_movements` **禁软删**（强审计）。`down.sql` 逆序 drop（先子后父）。触发器：`products`/`inventory_batches`/`orders` 挂 `trg_set_updated_at`；`order_items`/`inventory_movements` 无 `updated_at`，不挂。

### 8.3 领域类型与仓储接口（**已固定，agent 必须一字不差实现签名**）

#### `internal/catalog`

```go
type Product struct {
    ID, TenantID, StoreID       int64
    Barcode, Name, Category, Unit string
    CostCents, PriceCents       int64
    ImageURL                    string
    ShelfLifeDays               int   // 0 = 未设置
    Tags                        []string
    Status                      string // "active" | "archived"
    CreatedAt, UpdatedAt        time.Time
}

var ErrNotFound = errors.New("catalog: product not found")

type ProductFilter struct {
    StoreID  int64    // >0 时按店过滤
    Query    string   // 名称/条码模糊
    Category string
    Limit    int      // <=0 用默认 50
    Offset   int
}

type ProductRepo interface {
    Create(ctx context.Context, p *Product) (*Product, error)
    Update(ctx context.Context, p *Product) (*Product, error)          // 全字段更新(按 ID+tenant)
    GetByID(ctx context.Context, tenantID, id int64) (*Product, error) // ErrNotFound
    GetByBarcode(ctx context.Context, tenantID, storeID int64, barcode string) (*Product, error) // ErrNotFound
    List(ctx context.Context, tenantID int64, f ProductFilter) ([]*Product, error)
}

// Service 方法(全部先 EnforceTenant,再用 ctx 里的 StoreID 兜底):
//   Create(ctx, in CreateInput) (*Product, error)        // 校验 name/unit 非空、price>=cost>=0; 默认 Status=active/Unit=个
//   UpdatePrice(ctx, id, costCents, priceCents int64) (*Product, error) // 价格校验
//   GetByID(ctx, id int64) (*Product, error)
//   GetByBarcode(ctx, barcode string) (*Product, error)
//   List(ctx, f ProductFilter) ([]*Product, error)
```

#### `internal/billing`

```go
type OrderItem struct {
    ProductID      int64
    NameSnapshot   string
    Unit           string
    Qty            int64
    PriceCents     int64 // 成交单价(快照)
    CostCents      int64 // 成本快照(归因利润)
}
type Order struct {
    ID, TenantID, StoreID, MemberID int64 // MemberID 可 0(散客)
    OrderNo                         string
    Channel                         string // in_store|wxshop|wework|delivery
    Status                          string // pending|paid|refunded|voided
    TotalCents, PaidCents, DiscountCents int64
    PaymentMethod                   string
    OperatorID                      int64
    Items                           []OrderItem
    PaidAt                          *time.Time
    CreatedAt, UpdatedAt            time.Time
}

var ErrNotFound = errors.New("billing: order not found")

type OrderRepo interface {
    Create(ctx context.Context, o *Order) (*Order, error)              // 连 Items 一起落
    GetByID(ctx context.Context, tenantID, id int64) (*Order, error)   // ErrNotFound
    ListByDate(ctx context.Context, tenantID, storeID int64, day time.Time) ([]*Order, error) // day 任一时刻,按自然日
}

#### `internal/billing`（注意：**禁止 import `internal/catalog`**，用本包镜像类型做防腐）

```go
// PricedProduct 是 billing 对商品视图的最小镜像(防腐层),与 catalog.Product 字段同构,
// 由主线程在接线时做适配转换;billing 单测用 fake pricer 返回它。
type PricedProduct struct {
    ID, TenantID, StoreID int64
    Name, Unit            string
    PriceCents, CostCents int64
    Status                string
}

type ProductPricer interface {
    GetByID(ctx context.Context, tenantID, id int64) (*PricedProduct, error)
}
```

type CreateOrderItem struct{ ProductID, Qty int64 }
type CreateOrderInput struct {
    StoreID       int64
    MemberID      int64
    Channel       string // 空默认 in_store
    PaymentMethod string // 空默认 cash
    Items         []CreateOrderItem
}

// Service:
//   CreateOrder(ctx, in CreateOrderInput) (*Order, error)
//     — 逐项查 ProductPricer 取 name/unit/price/cost 快照; total=Σqty*price; paid=total; discount=0
//     — 校验: items 非空、qty>0、商品必须属于本 tenant(跨租户商品→ErrForbidden)
//     — OrderNo 由注入的 OrderNoFunc 生成; Status=paid; PaidAt=now
//   GetOrder(ctx, id int64) (*Order, error)
//   DailySummary(ctx, storeID int64, day time.Time) (*DailySummary, error)
type DailySummary struct {
    TenantID, StoreID    int64
    Date                 string // YYYY-MM-DD
    OrderCount           int
    GrossCents           int64 // Σ total
    NetCents             int64 // Σ paid
    CostCents            int64 // Σ 各项 qty*cost
    ProfitCents          int64 // Net - Cost
    TopProducts          []TopProduct // 按销量 desc, 上限 5
}
type TopProduct struct{ ProductID int64; Name string; Qty int64; GrossCents int64 }
```

### 8.4 包所有权（文件集互斥）

| Agent | 只准写这些文件 |
|---|---|
| **M1-migrations** | `migrations/000002_catalog_inventory.up.sql` + `.down.sql` + `migrations/000003_billing_order.up.sql` + `.down.sql` |
| **M1-catalog** | `internal/catalog/{catalog.go, memory_repo.go, service.go, service_test.go}` |
| **M1-billing** | `internal/billing/{billing.go, memory_repo.go, service.go, service_test.go}` |

> 主线程负责：`interfaces/http/handler/{catalog.go,billing.go}`、`interfaces/http/router.go`、`cmd/api/main.go`、`internal/billing` 与 `catalog` 的接线（`billing.ProductPricer = catalog.Service`）。**agent 不写这些路径。**

### 8.5 硬约束（M1-S1 全员）

1. **每个 repo 方法第一行 `tenantID, err := multitenancy.EnforceTenant(ctx)`**（或 `guard.Wrap`）；service 同样先取 tenant。**禁止从请求体/参数信任 tenant_id**——tenant 永远来自 ctx（JWT/Mock）。
2. 内存实现须**并发安全**（`sync.RWMutex`）+ **返回防御性拷贝**（仿 `auth.MemoryConsumerRepo`），并**强制 tenant 隔离**：`GetByID(tenantID, id)` 对别的 tenant 的 id 返回 `ErrNotFound`（串户测试矩阵必覆盖）。
3. 金额一律 `int64` 分，禁浮点。
4. 不引入白名单外依赖；不连真实 DB/Redis。
5. service 对 `billing` 只依赖 `ProductPricer` 接口（_test 里用 fake pricer_），**不 import catalog 具体类型做断言**——类型只用于字段搬运。
6. `OrderNoFunc` 注入（便于测试确定性）：`func(tenantID int64, now time.Time) string`；默认实现形如 `ZG<tenant><yyyymmdd><seq>`。

### 8.6 DoD（M1-S1）

- `go build ./... && go vet ./... && gofmt -l .`（空）`&& go test -race ./...` 全绿。
- catalog/billing service 单测覆盖：CRUD 闭环、串户拒绝（tenant A 读不到 tenant B 的商品/订单）、价格校验（price<cost 拒绝）、当日流水汇总数值正确（毛利=净收-成本）。
- 迁移文件 golang-migrate 命名合法、up/down 对称。
- HTTP 冒烟（主线程）：dev mock 下 `POST /v1/boss/catalog/products` 建品 → `POST /v1/boss/billing/orders` 开单 → `GET /v1/boss/billing/summary/today` 出汇总；带别的 `X-Mock-Tenant` 读不到前述数据（串户证明）。

---

## 9. M1-S2 增量契约（BE-A-04 库存 / BE-B-01 会员 / BE-A-07 会员码核销 / BE-A-08·09 数据质量 jobs，2026-07-20）

M1-S2 把 MVP 闭环补全：库存批次 + 会员档案 + 动态会员码核销 + 两个数据质量 cron。沿用全部铁律：「接口 + 内存实现兜底 + mock 测试」「tenant 只从 ctx 取，首行 EnforceTenant」「金额 int64 分」「不连真实 DB/Redis」「内存实现并发安全 + 防御性拷贝 + 跨租户 ErrNotFound」。

### 9.1 范围与裁剪（对照 ch05）

- **纳入**：`internal/inventory`（批次入库/出库/FIFO 消耗 + inventory_movements 落流水）、`internal/membership`（会员档案 + 储值账户 + 积分 + 动态会员码核销）、`internal/dataquality`（BE-A-08 销售反推 / BE-A-09 标签标记，纯函数 job，主线程配 cron）。
- **裁剪**：`coupons`/`member_coupons`（券核销）**不纳入**（属营销域 M2）；`payment_ledger`/`sub_merchants`（支付/反二清真实资金）**不纳入**（M4）。会员「扣储值」只动账面余额 + 落 `member_balance_log`，不接真实资金清结算。
- **分区降级**：ch05 里 `member_balance_log` 设计为 `PARTITION BY RANGE`，本次建**普通表**（同 §8.1 决策），注释标注。
- **0.5s 闭环**：本里程碑单进程内存 repo 天然满足；不做 Redis 锁/分布式优化（有真实流量再上 `t:{tid}:lock:*`）。

### 9.2 迁移编号（DB-03）

| 文件 | 内容 |
|---|---|
| `migrations/000004_membership.up/.down.sql` | `members` + `member_balance_accounts` + `member_balance_log`（普通表）+ `member_points_log`。复用 000001 的 `trg_set_updated_at()`（**勿重复 CREATE FUNCTION**，`CREATE OR REPLACE` 或直接挂 trigger）。 |

DDL 逐字段对齐 ch05 §5.3.4。`members`/`member_balance_accounts` 挂 `trg_set_updated_at`；`member_balance_log`/`member_points_log` 为审计流水无 `updated_at` 不挂、禁软删。金额一律 `BIGINT` 分。`down.sql` 逆序 drop（先子后父：log → account → member）。`inventory_batches`/`inventory_movements` 已在 000002 建好，**本次不重复建**。

### 9.3 领域类型与仓储接口（**已固定，agent 必须一字不差实现签名**）

#### `internal/inventory`（BE-A-04）

```go
// 批次
type Batch struct {
    ID, TenantID, StoreID, ProductID int64
    Quantity       int64     // 当前余量
    InPriceCents   int64
    ExpiryDate     *time.Time // nil = 未设保质期
    ExpiryWarnDate *time.Time
    Status         string // active|nearing|expired|cleared
    CreatedAt, UpdatedAt time.Time
}
// 流水(强审计,禁软删)
type Movement struct {
    ID, TenantID, StoreID, ProductID int64
    BatchID       int64  // 0 = 无关联批次
    Type          string // purchase_in|sale_out|stocktake_adj|return_in|loss|transfer
    Quantity      int64  // 正入负出
    RefOrderID    int64  // 0 = 无关联订单
    OperatorID    int64
    Note          string
    CreatedAt     time.Time
}
var ErrNotFound = errors.New("inventory: batch/product not found")
var ErrInsufficientStock = errors.New("inventory: insufficient stock")

type BatchRepo interface {
    CreateBatch(ctx context.Context, b *Batch) (*Batch, error)
    GetBatch(ctx context.Context, tenantID, id int64) (*Batch, error)                 // ErrNotFound/跨租户
    UpdateBatch(ctx context.Context, b *Batch) (*Batch, error)                        // 按 ID+tenant 更新余量/状态
    ListActiveBatchesFIFO(ctx context.Context, tenantID, storeID, productID int64) ([]*Batch, error) // status=active 且 quantity>0, 按 CreatedAt 升序(FIFO)
    AppendMovement(ctx context.Context, m *Movement) (*Movement, error)
    ListMovements(ctx context.Context, tenantID int64, f MovementFilter) ([]*Movement, error)
}
type MovementFilter struct {
    StoreID, ProductID int64
    Type               string
    Day                *time.Time // 非 nil 时按自然日过滤
    Limit, Offset      int
}
// Service:
//   PurchaseIn(ctx, in PurchaseInInput) (*Batch, error)         // 校验 product 属于本 tenant(经 ProductLookuper)+qty>0; 建批次+purchase_in 流水
//   ConsumeFIFO(ctx, storeID, productID, qty int64, refOrderID int64) ([]*Batch, error) // 按 FIFO 扣减+逐项 sale_out 流水; 不足→ErrInsufficientStock(已扣的回滚)
//   Stock(ctx, storeID, productID int64) (int64, error)          // 本 tenant 该商品 active 批次余量合计
//   ListMovements(ctx, f MovementFilter) ([]*Movement, error)
type PurchaseInInput struct {
    StoreID, ProductID, Quantity, InPriceCents int64
    ExpiryDate *time.Time
    Note       string
}
// ProductLookuper 是 inventory 对商品视图的最小依赖(防腐,不 import catalog):
type ProductLookuper interface {
    Exists(ctx context.Context, tenantID, productID int64) (bool, error)
}
```

> **接线说明**：billing 的 FIFO 扣库存由主线程在 `cmd/api/main.go` 通过 billing 的 `Option`（如 `WithStockConsumer(inventorySvc)`）注入；**billing 不 import inventory**，用 billing 本包定义的 `StockConsumer` 接口（`ConsumeFIFO(ctx, storeID, productID, qty, refOrderID) error`）。M1-S2 主线程接线时给 billing 加这个可选 Option；注入后 CreateOrder 在开单前对每个商品调 ConsumeFIFO，任一失败则整单拒绝（不落单）。**本契约不改 §8.3 已定的 billing 签名**，仅新增可选 Option。

#### `internal/membership`（BE-B-01 + BE-A-07）

```go
type Member struct {
    ID, TenantID, StoreID  int64
    PhoneHash, WxOpenID    string  // 至少一个非空
    Name, Level            string  // level: normal|vip|blackcard
    Points                 int64
    TotalSpentCents        int64
    LastVisitAt            *time.Time
    Status                 string  // active|blocked
    CreatedAt, UpdatedAt   time.Time
}
type BalanceAccount struct {
    ID, TenantID, MemberID int64
    BalanceCents, GiftCents int64
    CreatedAt, UpdatedAt    time.Time
}
var ErrNotFound = errors.New("membership: member not found")
var ErrInsufficientBalance = errors.New("membership: insufficient balance")
var ErrMemberBlocked = errors.New("membership: member blocked")
var ErrCodeInvalid = errors.New("membership: member code invalid or expired")

type MemberRepo interface {
    CreateMember(ctx context.Context, m *Member) (*Member, error)
    GetMember(ctx context.Context, tenantID, id int64) (*Member, error)              // ErrNotFound/跨租户
    UpdateMember(ctx context.Context, m *Member) (*Member, error)
    UpsertAccount(ctx context.Context, a *BalanceAccount) (*BalanceAccount, error)
    GetAccount(ctx context.Context, tenantID, memberID int64) (*BalanceAccount, error) // 无则 ErrNotFound
    AppendPointsLog(ctx context.Context, tenantID, memberID int64, typ string, delta, pointsAfter, refOrderID int64) error
    AppendBalanceLog(ctx context.Context, tenantID, memberID, accountID int64, typ string, amountCents, balanceAfter, refOrderID int64) error
}
// MemberCoder 动态会员码(防腐,不依赖具体签发实现):签发/解析短时效会员码。
type MemberCoder interface {
    Issue(ctx context.Context, tenantID, memberID int64, ttl time.Duration) (string, error)
    Resolve(ctx context.Context, tenantID int64, code string) (memberID int64, err error) // 过期/伪造→ErrCodeInvalid
}
// Service:
//   Join(ctx, in JoinInput) (*Member, error)                 // 校验 phone/openid 至少其一; 建会员+开户(余额0); 幂等(同 phone_hash 复用)
//   GetMember(ctx, id int64) (*Member, error)
//   IssueCode(ctx, memberID int64) (string, error)            // 会员存在且 active; ttl 60s
//   Redeem(ctx, in RedeemInput) (*RedeemResult, error)        // BE-A-07 闭环
type JoinInput struct{ StoreID int64; PhoneHash, WxOpenID, Name string }
type RedeemInput struct {
    StoreID, OperatorID, RefOrderID int64
    Code             string
    ConsumeCents     int64  // >0 时扣储值
    EarnPoints       int64  // >0 时累积分
}
type RedeemResult struct {
    MemberID, Points, BalanceCents int64
    ConsumedCents                  int64
}
```

> `Redeem` 流程：`Resolve(code)`→member 属于本 tenant 且 active→（ConsumeCents>0 时）扣账户余额+落 consume 流水，余额不足→`ErrInsufficientBalance`→（EarnPoints>0 时）累积分+落 earn 流水→更新 LastVisitAt。储值扣减与积分累计在内存实现里用单 repo 锁保证原子（生产 pgx 用事务）。**MemberCoder 的内存实现**：HMAC 签 `tenantID.memberID.expiryUnix` + 服务端 map 存 code→member 防重放，ttl 到期即 ErrCodeInvalid（注入 clock 便于测试）。

#### `internal/dataquality`（BE-A-08 + BE-A-09）

纯函数 job 包，**不 import catalog/billing/inventory 具体类型**，只依赖本包定义的窄接口（主线程适配）：

```go
// BE-A-08 库存反推日销售: 由当日 sale_out 流水汇总每商品销量(三源融合的库存源)。
type SalesReverser interface {
    ListSaleOut(ctx context.Context, tenantID, storeID int64, day time.Time) ([]SaleQty, error)
}
type SaleQty struct{ ProductID, Qty int64 }
func ReverseDailySales(ctx context.Context, r SalesReverser, tenantID, storeID int64, day time.Time) ([]SaleQty, error)

// BE-A-09 临期/滞销/高毛利 tag 标记: 输入商品视图+批次视图,输出每商品应打的 tag 集合。
type TagProductView struct {
    ProductID, ShelfLifeDays int
    CostCents, PriceCents    int64
    DaysSinceLastSale        int   // 滞销判定
    EarliestExpiry           *time.Time // 最近到期批次(临期判定)
}
type TagRule struct {
    NearingDays      int   // 距到期 <=N 天打「临期」
    StaleDays        int   // >=N 天未售打「滞销」
    HighMarginMinPct int   // (price-cost)/price*100 >=N 打「高毛利」
}
func ComputeTags(v TagProductView, rule TagRule, now time.Time) []string
```

> `ComputeTags` 为**纯函数**（无副作用、可单测）；`ReverseDailySales` 只做聚合。主线程负责适配真实 repo 并配 cron（`@daily` / 每小时），agent 不写 cron 调度器。

### 9.4 包所有权（文件集互斥）

| Agent | 只准写这些文件 |
|---|---|
| **M2-migrations** | `migrations/000004_membership.up.sql` + `.down.sql` |
| **M2-inventory** | `internal/inventory/{inventory.go, memory_repo.go, service.go, service_test.go}` |
| **M2-membership** | `internal/membership/{membership.go, memory_repo.go, coder.go, service.go, service_test.go}` |
| **M2-dataquality** | `internal/dataquality/{dataquality.go, dataquality_test.go}` |

> 主线程负责（agent 不写）：`interfaces/http/handler/{inventory.go,membership.go}`、`interfaces/http/router.go`、`cmd/api/main.go` 接线（含 billing 的 FIFO StockConsumer Option、catalog→inventory.ProductLookuper 适配、membership MemberCoder 接线）、`interfaces/http/m1s2_test.go`、数据质量 cron 挂载。

### 9.5 硬约束（M1-S2 全员）

1. 每个 repo/service 方法首行 `tenantID, err := multitenancy.EnforceTenant(ctx)`；tenant **永不**从请求体/参数取。
2. 内存实现 `sync.RWMutex` + 防御性拷贝 + 跨租户 `ErrNotFound`（串户测试矩阵必覆盖：tenant A 读/扣 tenant B 的批次/会员→ErrNotFound/拒绝）。
3. 金额 int64 分，禁浮点。
4. 库存扣减**不得透支**（`quantity >= 0`，不足→ErrInsufficientStock 且回滚已扣）；储值**不得透支**（balance>=0，不足→ErrInsufficientBalance）。
5. 防腐：`inventory` 不 import `catalog`（用 `ProductLookuper`）；`membership` 不 import `billing`；`dataquality` 不 import 任何领域包。所有跨域依赖走窄接口，主线程在 main.go 适配。
6. 不引入白名单外依赖；不连真实 DB/Redis；测试用 fake 接口 + 注入 clock。
7. 每个 `package xx` 上方 `// Package xx ...` 文档注释；`gofmt` 干净。

### 9.6 DoD（M1-S2）

- `go build ./... && go vet ./... && gofmt -l .`（空）`&& go test -race ./...` 全绿。
- inventory service 单测：入库→FIFO 消耗顺序正确、批次余量扣减正确、不足拒绝且回滚、流水正确落、串户拒绝。
- membership service 单测：入会开户、会员码签发/解析、过期码拒绝、核销扣储值+累积分+落流水、余额不足拒绝、串户拒绝。
- dataquality 单测：ReverseDailySales 聚合正确、ComputeTags 临期/滞销/高毛利规则正确（注入 now）。
- 迁移 golang-migrate 命名合法、up/down 对称。
- HTTP 冒烟（主线程）：dev mock 下「建品→入库→开单扣库存→会员入会→签发会员码→核销累积分→当日流水」全链路；串户证明（别的 X-Mock-Tenant 读不到批次/会员、核销别人的码→拒绝）。

---

## 10. M2-S1 增量契约（BE-B-02 券域 / BE-B-04 频次熔断 / BE-B-05 内容过滤 / AI-02-seed 毛利护栏，2026-07-20）

M2-S1 落地营销域发券闭环 + 三道合规护栏（频次熔断 / 内容过滤 / 毛利护栏），为 M3 AI 一键执行（BE-B-11 ActionExecutor）铺数据与安全轨。沿用全部铁律：「接口 + 内存实现兜底 + mock 测试」「tenant 只从 ctx 取，首行 EnforceTenant」「金额 int64 分」「不连真实 DB/Redis」「内存实现并发安全 + 防御性拷贝 + 跨租户 ErrNotFound」。

### 10.1 范围与裁剪（对照 ch04 §4.2 / §4.3 / §4.5）

- **纳入**：`internal/marketing`（券模板 CRUD + 会员券发券/核销 + source/strategy_id 归因）、`internal/compliance/freqcircuit`（企微群发/1v1/订阅消息/短信 四通道频次熔断，内存计数器）、`internal/compliance/contentfilter`（DFA 敏感词 + 广告法绝对化替换，纯函数）、`internal/margin`（毛利护栏纯函数，AI-02 seed）。
- **裁剪**：`groupbuys`/`groupbuy_participants`（拼团，BE-B-03）不纳入（依赖小程序码，留 M2-S2）；`wework_living_codes`/`community_groups`/`reach_tasks`（企微活码/社群/触达任务，BE-B-06/07）不纳入（依赖企微 SDK，留 M2-S2）；`ActionExecutor`（BE-B-11）不纳入（依赖 AI-05 五段式，留 M3-S1）；营销策略表 `ai_strategies`/`strategy_items`（DB-04）不纳入（留 M3-S1）。
- **分区降级**：ch05 无分区设计，coupons/member_coupons 均普通表。
- **频次熔断内存化**：Counter 接口对齐 Redis INCR/DECR/EXPIRE 语义，内存实现（map + sync.Mutex + 惰性过期）；生产换 Redis 实现不改 service（同 pkg/db/pkg/redisx 缺失降级范式）。
- **毛利护栏纯函数化**：`internal/margin` 不 import 任何领域包（同 dataquality 范式），主线程适配 marketing.Coupon → margin.CouponRule。marketing 通过可选 `MarginGuard` 接口注入，nil 时不校验（手动发券可选，AI 发券 M3 必走）。

### 10.2 迁移编号（DB-03 部分）

| 文件 | 内容 |
|---|---|
| `migrations/000005_marketing.up/.down.sql` | `coupons`（券模板，软删除）+ `member_coupons`（会员券实例，禁软删审计）。复用 000001 的 `trg_set_updated_at()`（`CREATE OR REPLACE` 或直接挂 trigger）。 |

DDL 逐字段对齐 ch05 §5.3.5。`coupons` 挂 `trg_set_updated_at` 且有 `deleted_at`；`member_coupons` 为审计实例无 `updated_at` 不挂触发器、禁软删。金额一律 `BIGINT` 分；`params` 用 `JSONB`。`down.sql` 逆序 drop（先 member_coupons 后 coupons）。`groupbuys`/`groupbuy_participants` 本次**不建**（留 M2-S2）。

### 10.3 领域类型与仓储接口（**已固定，agent 必须一字不差实现签名**）

#### `internal/marketing`（BE-B-02）

```go
type Coupon struct {
    ID, TenantID, StoreID int64
    Name                  string
    Type                  string // full_reduction|discount|cash|gift
    Params                CouponParams
    ValidFrom, ValidTo    time.Time
    TotalQuota            int64 // 0 = 无限发行
    Issued                int64
    Status                string // draft|active|paused|ended
    CreatedBy             int64
    CreatedAt, UpdatedAt  time.Time
}

// CouponParams 是券模板的弹性参数(JSONB 解析后)。字段按 Type 取用:
//   full_reduction: ThresholdCents + OffCents (满 threshold 减 off)
//   discount:       DiscountPermille (‰, 850=85%=8.5折)
//   cash:           OffCents (代金直减, 无门槛)
//   gift:           GiftProductID (赠品, M2 不校验赠品成本)
type CouponParams struct {
    ThresholdCents   int64 `json:"threshold_cents,omitempty"`
    OffCents         int64 `json:"off_cents,omitempty"`
    DiscountPermille int64 `json:"discount_permille,omitempty"` // 0-1000
    GiftProductID    int64 `json:"gift_product_id,omitempty"`
}

var ErrNotFound       = errors.New("marketing: coupon not found")
var ErrQuotaExhausted = errors.New("marketing: coupon quota exhausted")
var ErrCouponInactive = errors.New("marketing: coupon not active")
var ErrCouponExpired  = errors.New("marketing: member coupon expired")
var ErrCouponUsed     = errors.New("marketing: member coupon already used")

type CouponFilter struct {
    StoreID int64
    Status  string // 空不过滤
    Type    string // 空不过滤
    Limit   int    // <=0 默认 50
    Offset  int
}

type MemberCoupon struct {
    ID, TenantID, MemberID, CouponID int64
    Status     string // unused|used|expired|revoked
    Source     string // ai_strategy|manual|groupbuy|newmember
    StrategyID int64  // 归因(0=非AI来源)
    IssuedAt   time.Time
    UsedAt     *time.Time
    ExpireAt   *time.Time // nil=用 coupon.ValidTo
    RefOrderID int64
}

type CouponRepo interface {
    CreateCoupon(ctx context.Context, c *Coupon) (*Coupon, error)
    GetCoupon(ctx context.Context, tenantID, id int64) (*Coupon, error)                  // ErrNotFound/跨租户
    GetByName(ctx context.Context, tenantID int64, name string) (*Coupon, error)         // ErrNotFound/跨租户
    UpdateCoupon(ctx context.Context, c *Coupon) (*Coupon, error)                        // 按 ID+tenant 更新 status/issued
    ListCoupons(ctx context.Context, tenantID int64, f CouponFilter) ([]*Coupon, error)
    // IssueToMember 原子: 校验 coupon active+在有效期+未超发(TotalQuota=0 无限) → issued++ → 建 member_coupon(unused)。
    // source/strategyID 落归因; expireAt nil 时用 coupon.ValidTo。任一不满足返回对应哨兵错误, 状态不变。
    IssueToMember(ctx context.Context, tenantID, memberID, couponID int64, source string, strategyID int64, expireAt *time.Time, now time.Time) (*MemberCoupon, error)
    GetMemberCoupon(ctx context.Context, tenantID, id int64) (*MemberCoupon, error) // ErrNotFound/跨租户
    ListMemberCoupons(ctx context.Context, tenantID, memberID int64, status string) ([]*MemberCoupon, error) // status 空不过滤
    // RedeemAtomic 原子: unused→used + 设 UsedAt/RefOrderID; 非 unused→ErrCouponUsed; ExpireAt 已过→ErrCouponExpired
    RedeemAtomic(ctx context.Context, tenantID, id, refOrderID int64, now time.Time) (*MemberCoupon, error)
}

// MemberLookuper 防腐: marketing 不 import membership(同 inventory.ProductLookuper 范式)。
type MemberLookuper interface {
    Exists(ctx context.Context, tenantID, memberID int64) (bool, error)
}

// MarginGuard 可选: 发券前毛利护栏(AI 发券必走, 手动发券可选)。nil 时不校验。
type MarginGuard interface {
    ValidateCoupon(ctx context.Context, c *Coupon) error
}

type CreateCouponInput struct {
    StoreID    int64
    Name       string
    Type       string
    Params     CouponParams
    ValidFrom  time.Time
    ValidTo    time.Time
    TotalQuota int64
    CreatedBy  int64
}
type IssueInput struct {
    MemberID   int64
    CouponID   int64
    Source     string // 空=manual
    StrategyID int64
    ExpireAt   *time.Time // nil=用 coupon.ValidTo
}
type RedeemInput struct {
    MemberCouponID int64
    RefOrderID     int64
}
// Service:
//   CreateCoupon(ctx, in CreateCouponInput) (*Coupon, error)   // name 非空+租户内唯一; type ∈ 四类; valid_from<valid_to; 默认 status=draft
//   UpdateStatus(ctx, id int64, status string) (*Coupon, error) // draft→active; active↔paused; *→ended; 非法迁移→ErrParam
//   GetCoupon(ctx, id int64) (*Coupon, error)
//   ListCoupons(ctx, f CouponFilter) ([]*Coupon, error)
//   Issue(ctx, in IssueInput) (*MemberCoupon, error)           // 校验 member 存在(via MemberLookuper)+coupon active+quota+有效期; 可选 MarginGuard
//   Redeem(ctx, in RedeemInput) (*MemberCoupon, error)         // 核销 unused→used(经 RedeemAtomic)
//   ListMemberCoupons(ctx, memberID int64, status string) ([]*MemberCoupon, error)
```

> **接线说明**：marketing 的 MemberLookuper / MarginGuard 由主线程在 `cmd/api/main.go` 适配（membership → MemberLookuper，margin.Guard → MarginGuard）。marketing **不 import** membership/margin/catalog。

#### `internal/margin`（AI-02 seed — 毛利护栏纯函数）

```go
// 纯函数包, 不 import 任何领域包(同 dataquality 范式)。主线程适配 marketing.Coupon → margin.CouponRule。
type Guard struct {
    MarginFloorPct int64 // 最低毛利率(%, 10=10%); 0=不校验
}

type CouponRule struct {
    Type             string // full_reduction|discount|cash|gift
    ThresholdCents   int64
    OffCents         int64
    DiscountPermille int64 // 0-1000
}

type ProductView struct {
    ID                    int64
    CostCents, PriceCents int64
}

var ErrBelowMarginFloor = errors.New("margin: below floor")

// ValidateCoupon 校验: 对 products 中每个商品应用 rule 后, 实付金额的毛利率是否 >= floor。
// 实付计算(price=PriceCents, cost=CostCents):
//   full_reduction: price>=ThresholdCents → price-OffCents; 否则 price(不触发满减)
//   discount:       price * DiscountPermille / 1000
//   cash:           max(price-OffCents, 0)
//   gift:           price(赠品成本 M2 不计)
// 毛利率 = (实付 - cost) / 实付 * 100; 实付<=0 时若 cost>0 → ErrBelowMarginFloor。
// 任一商品不达标 → ErrBelowMarginFloor; products 为空 → nil(无可校验商品, 调用方负责)。
func ValidateCoupon(g Guard, rule CouponRule, products []ProductView) error

// ValidatePriceAdj 校验改价后毛利率: newPrice>0 且 (newPrice-cost)*100/newPrice >= floor。
func ValidatePriceAdj(g Guard, costCents, newPriceCents int64) error
```

#### `internal/compliance/freqcircuit`（BE-B-04）

```go
type Channel string
const (
    ChannelWeworkGroup  Channel = "wework_group"
    ChannelWeworkOneV   Channel = "wework_1v1"
    ChannelSubscribeMsg Channel = "subscribe_msg"
    ChannelSMS          Channel = "sms"
)

type Period string
const (
    PeriodDaily   Period = "daily"
    PeriodWeekly  Period = "weekly"
    PeriodMonthly Period = "monthly"
)

// Limit 是某通道的频次上限。Monthly=0 表示按"当月天数"动态算(计划书口径)。
type Limit struct {
    Daily, Weekly, Monthly int64
}

// Counter 抽象 Redis INCR/DECR/EXPIRE 语义, 内存实现 + 未来 Redis 实现都满足。
type Counter interface {
    // Incr 原子 +1 并返回 incr 后的值; 若 key 不存在或已过期则建并设 expireAt(首次)。
    Incr(ctx context.Context, key string, expireAt time.Time) (int64, error)
    // Decr 原子 -1(回退被拒的计数), 不低于 0。
    Decr(ctx context.Context, key string) error
    // Get 返回当前计数(不增); 过期/不存在 → 0。
    Get(ctx context.Context, key string) (int64, error)
}

var ErrFreqExceeded = errors.New("freqcircuit: frequency exceeded")

type Service struct {
    counter Counter
    now     func() time.Time
    limits  map[Channel]Limit
}

type Option func(*Service)
func WithCounter(c Counter) Option      // 默认 MemoryCounter
func WithNowFunc(f func() time.Time) Option
func WithLimits(m map[Channel]Limit) Option // 覆盖默认 limits

// New 构造 Service。默认 limits(对齐 ch04 §4.3.3):
//   WeworkGroup/WeworkOneV: {Daily:1, Weekly:7, Monthly:0(=当月天数)}
//   SubscribeMsg/SMS:       {0,0,0}(不限, Allow 恒 true)
func New(opts ...Option) *Service

// Allow 检查 ch 在当前 ctx tenant 的三档频次。流程: 逐档 Incr → 若超限则 Decr 全部已 Incr 的回退 → 返回 (false, reason)。
// reason 形如 "freq_daily"/"freq_weekly"/"freq_monthly"; 全通过 → (true, "")。
// 无 limit 的通道恒 (true, "")。key 格式: t:{tid}:freq:{ch}:{period}。
func (s *Service) Allow(ctx context.Context, ch Channel) (allowed bool, reason string, err error)

// Remaining 返回 ch 当前三档剩余配额(不 Incr); 无 limit 通道返回 -1(无限)。
func (s *Service) Remaining(ctx context.Context, ch Channel) (daily, weekly, monthly int64, err error)
```

> Counter 的内存实现 `MemoryCounter`（map + sync.Mutex + 惰性过期）须随包交付，供 dev/test 直接用；生产 Redis 实现留待有 Redis 后（接口已对齐）。

#### `internal/compliance/contentfilter`（BE-B-05）

```go
// 纯函数包(核心 Do 无副作用, 可单测)。DFA 敏感词树 + 广告法绝对化词库。
type Filter struct {
    // 未导出: dfa *DFA, absolutism []string, replacement string
}

type Result struct {
    Text     string   // 过滤/替换后的文本(命中敏感词时为空)
    Passed   bool     // true=可发, false=命中敏感词被拦截
    HitWords []string // 命中的敏感词(DFA)
    Replaced []string // 被替换的绝对化词(广告法)
}

type Option func(*Filter)
func WithSensitiveWords(words []string) Option  // 覆盖默认敏感词库
func WithAbsolutismWords(words []string) Option // 覆盖默认绝对化词库
func WithReplacement(r string) Option           // 默认 "*"

// New 构造 Filter, 内建默认敏感词+绝对化词库(最小可用集, 主线程可覆盖)。
func New(opts ...Option) *Filter

// Do 执行: DFA 命中敏感词 → Passed=false + Text=""; 绝对化词 → 替换为 Replacement(默认"*")。
// 两者独立: 命中敏感词直接拦截(不再做绝对化替换); 未命中敏感词才做绝对化替换。
func (f *Filter) Do(text string) Result
```

> 默认词库为最小可用集（如绝对化词含「最/第一/国家级/顶级/唯一/极致」等，敏感词含最小演示集）；主线程/配置可覆盖。DFA 实现 rune 级匹配，支持中文。

### 10.4 包所有权（文件集互斥）

| Agent | 只准写这些文件 |
|---|---|
| **M2-migrations** | `migrations/000005_marketing.up.sql` + `.down.sql` |
| **M2-marketing** | `internal/marketing/{marketing.go, memory_repo.go, service.go, service_test.go}` |
| **M2-margin** | `internal/margin/{margin.go, margin_test.go}` |
| **M2-freqcircuit** | `internal/compliance/freqcircuit/{freqcircuit.go, memory_counter.go, service.go, service_test.go}` |
| **M2-contentfilter** | `internal/compliance/contentfilter/{contentfilter.go, dfa.go, contentfilter_test.go}` |

> 主线程负责（agent 不写）：`interfaces/http/handler/marketing.go`、`interfaces/http/router.go`、`cmd/api/main.go` 接线（membership→MemberLookuper 适配、margin.Guard→MarginGuard 适配、freqcircuit/contentfilter 暂不挂路由——留 M2-S2 reach/send）、`interfaces/http/m2s1_test.go`、`pkg/errcode/errcode.go`（如需新增码）。

### 10.5 硬约束（M2-S1 全员）

1. 每个 repo/service 方法首行 `tenantID, err := multitenancy.EnforceTenant(ctx)`；tenant **永不**从请求体/参数取。
2. 内存实现 `sync.RWMutex`/`sync.Mutex` + 防御性拷贝 + 跨租户 `ErrNotFound`（串户测试矩阵必覆盖：tenant A 读/发/核 tenant B 的券→ErrNotFound/拒绝）。
3. 金额 int64 分，禁浮点；折扣用千分比(‰)整数。
4. 券发行**不得超发**（issued < TotalQuota 或 TotalQuota=0）；会员券核销**不得重复**（unused→used 原子，非 unused→ErrCouponUsed）。
5. 防腐：`marketing` 不 import `membership`/`margin`/`catalog`（用 `MemberLookuper`/`MarginGuard` 接口）；`margin` 不 import 任何领域包；`freqcircuit`/`contentfilter` 不 import 任何领域包。所有跨域依赖走窄接口，主线程在 main.go 适配。
6. 不引入白名单外依赖；不连真实 DB/Redis；测试用 fake 接口 + 注入 clock/counter。
7. 纯函数包（`margin`、`contentfilter` 的 `Do`、`freqcircuit` 的逻辑）无副作用、可单测；`contentfilter` 的 DFA 匹配须 rune 级（支持中文）。
8. 每个 `package xx` 上方 `// Package xx ...` 文档注释；`gofmt` 干净。

### 10.6 DoD（M2-S1）

- `go build ./... && go vet ./... && gofmt -l .`（空）`&& go test -race ./...` 全绿。
- marketing service 单测：建券(四类)→状态迁移(draft→active)→发券(active+quota+有效期校验)→超发拒绝→核销(unused→used)→重复核销拒绝→过期券核销拒绝→归因(strategy_id/source)正确→串户拒绝。
- margin 单测：满减/折扣/代金/赠品四类毛利率计算正确、低于护栏拒绝、改价校验、空 products 不报错。
- freqcircuit 单测：三档计数正确、超额硬拒+回退计数、monthly=当月天数、无 limit 通道恒通过、跨租户 key 隔离。
- contentfilter 单测：DFA 敏感词命中拦截、绝对化词替换、中文匹配、默认词库可覆盖。
- 迁移 golang-migrate 命名合法、up/down 对称。
- HTTP 冒烟（主线程）：dev mock 下「建券→上线→手动发券给会员→核销」全链路；串户证明（别的 X-Mock-Tenant 读不到券、核销别人的会员券→拒绝）；毛利护栏可选演示（margin 注入后低毛利券被拒）。
