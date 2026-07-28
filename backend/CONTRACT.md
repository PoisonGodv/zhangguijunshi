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

---

## 11. M2-S2 增量契约（BE-B-03 拼团 / BE-B-07 触达模板 / BE-B-08 订阅计数 / BE-B-09 新号限速 / BE-B-10 退订风控 / BE-B-06 社群活码，2026-07-27）

在 M2-S1 之上落地「触达闭环」——把营销券、毛利护栏、频次熔断、内容过滤串成一条可发送、可拦截、可归因、可退订的 reach 链路，并补齐拼团裂变 + 社群/活码私域沉淀。为 M3-S1 ActionExecutor(SendGroupMsg/IssueCoupon 走 reach.Send) 提供唯一的对外发送口子。

### 11.1 范围与裁剪（对照 ch04 §4.2.3 / §4.3 / §4.2.4 / ch05 §5.3.5-§5.3.6）

**纳入**：
- **BE-B-03 拼团+老带新**：groupbuys/groupbuy_participants 状态机，`JoinAtomic`（active+未满+未参团→插入 participant + joined++，满员→success）+ `FinalizeExpired`（active 且 end_at<now 且 joined<required→failed）。
- **BE-B-07 8 类触达模板**：`new_welcome / birthday / recall_30 / recall_60 / recall_90 / repurchase / balance_low / points_expire`，纯函数渲染（会员名/店名/弹性 vars）。
- **BE-B-08 订阅消息授权计数**：subscribe_auth `Grant`(granted++) / `Consume`(used<granted→used++ 原子)；subscribe_msg 通道发送前强制校验。
- **BE-B-09 新号加好友限速**：wework_user_id 维度，age<7 天 50/天、7-30 天 80/天、30+ 天 200/天，超限硬拒 + Decr 回退。
- **BE-B-10 退订入口 + 风控滑动窗口**：退订记录阻断后续发送（已退订→blocked）；租户级 blocked 率/突发发送滑动窗口超阈值→记录 RiskEvent（**告警不硬拒**，真实告警通道延后）。
- **BE-B-06 社群+活码数据模型**：community_groups(sop_stage 四阶段 recruit→activate→convert→refer) + wework_living_codes(活码分流 nearest_store/round_robin)。
- **reach.Send 编排**（核心枢纽）：render→contentfilter→freqcircuit→unsubscribe→subscribe quota→sender，任一拦截写 ReachTask(blocked+reason)，全程留痕归因 strategy_id。

**显式延后（不在 M2-S2，写进 §11.1 供后人知）**：
- WX-06 企微真实 API（活码创建/客户群群发 `add_msg_templates`）——**无服务商资质，做 Sender 接口 + 内存 stub**（Send 记录到 reach_tasks，status=sent，不真正触达用户）。
- 短信真实通道 —— stub 同上。
- 拼团支付/退款闭环 —— 仅状态机（joined→paid 标记），不接 payment 域。
- 风控告警真实通道（飞书/短信）—— 仅写 RiskEvent 内存记录。
- pgx repo —— 全部继续内存实现（接口已对齐未来 pgx）。
- jobs cron（FinalizeExpired/离线扫描）—— 提供 Service 方法，定时调度留 M3。

### 11.2 迁移编号（DB-03 续）

`migrations/000006_reach_community_groupbuy.{up,down}.sql`，7 张表，全部挂 tenant_id 行隔离，参照 §5.3 DDL（ch05 行 416-505）：
- `groupbuys` / `groupbuy_participants`（参 ch05 §5.3.5，coupons 之后）
- `wework_living_codes` / `community_groups`（§5.3.6）
- `reach_tasks`（§5.3.6，blocked_reason 取值 freq_circuit|content_filter|unsubscribed|subscribe_quota|new_account_guard|risk）
- `subscribe_auth`（§5.3.6，CHECK used_quota<=granted_quota）
- `unsubscribe_records`（新增：id/tenant_id/member_id/channel/reason/unsubscribed_at，INDEX tenant_id+member_id+channel）。UNIQUE(tenant_id, member_id, channel) —— 一会员一通道一条退订态，退订后重新订阅 UPDATE。
- groupbuys/community_groups 挂 `trg_set_updated_at`（同 §8/§10 范式）；reach_tasks/subscribe_auth/unsubscribe_records/wework_living_codes 无 updated_at 不挂触发器。down 反序 DROP。

### 11.3 领域类型与仓储接口（**已固定，agent 必须一字不差实现签名**）

#### 11.3.1 `internal/groupbuy`（BE-B-03）

```go
package groupbuy

type Groupbuy struct {
    ID, TenantID, StoreID, ProductID int64
    Title                            string
    Params                           GroupbuyParams
    RequiredCount                    int        // 3-4 成团
    Joined                           int
    Status                           string     // active|success|failed|cancelled
    StartAt, EndAt                   time.Time
    CreatedAt, UpdatedAt             time.Time
}
type GroupbuyParams struct {
    GroupSize  int   `json:"group_size,omitempty"`
    PriceCents int64 `json:"price_cents,omitempty"`
    ValidHours int   `json:"valid_hours,omitempty"`
}
type Participant struct {
    ID, TenantID, GroupbuyID, MemberID int64
    Status                             string // joined|paid|refunded
    JoinedAt                           time.Time
}
const (
    StatusActive    = "active"
    StatusSuccess   = "success"
    StatusFailed    = "failed"
    StatusCancelled = "cancelled"
    PStatusJoined   = "joined"
    PStatusPaid     = "paid"
    PStatusRefunded = "refunded"
)
var (
    ErrNotFound      = errors.New("groupbuy: not found")
    ErrGroupbuyFull  = errors.New("groupbuy: already full")
    ErrGroupbuyClosed = errors.New("groupbuy: not active")
    ErrAlreadyJoined = errors.New("groupbuy: member already joined")
)

type GroupbuyRepo interface {
    Create(ctx, g *Groupbuy) (*Groupbuy, error)
    GetByID(ctx, tenantID, id int64) (*Groupbuy, error)
    ListActive(ctx, tenantID, storeID int64) ([]*Groupbuy, error)
    UpdateStatus(ctx, tenantID, id int64, status string, now time.Time) (*Groupbuy, error)
    // JoinAtomic: 校验 active+joined<required+未参团 → 插 participant(joined) + groupbuy.Joined++；
    // 若 Join 后 Joined>=Required → groupbuy.Status=success。全成功或全回滚。
    JoinAtomic(ctx, tenantID, groupbuyID, memberID int64, now time.Time) (*Participant, *Groupbuy, error)
    GetParticipant(ctx, tenantID, groupbuyID, memberID int64) (*Participant, error)
    ListParticipants(ctx, tenantID, groupbuyID int64) ([]*Participant, error)
    // MarkPaid: participant joined→paid（拼团支付闭环留 payment 域，M2-S2 仅状态标记）
    MarkPaid(ctx, tenantID, groupbuyID, memberID int64, now time.Time) (*Participant, error)
}
// MemberLookuper 防腐：groupbuy 不 import membership。主线程适配（同 marketing 范式）。
type MemberLookuper interface { Exists(ctx, tenantID, memberID int64) (bool, error) }

type Option func(*Service)
func New(repo GroupbuyRepo, opts ...Option) *Service
func WithMemberLookuper(MemberLookuper) Option
func WithNowFunc(func() time.Time) Option

type CreateInput struct {
    StoreID, ProductID int64
    Title              string
    Params             GroupbuyParams
    RequiredCount      int    // <=0 时取 Params.GroupSize 或默认 3
    ValidHours         int    // 计算 EndAt = StartAt + ValidHoursh；<=0 默认 24
}
func (s *Service) Create(ctx, in CreateInput) (*Groupbuy, error)
func (s *Service) Get(ctx, id int64) (*Groupbuy, error)
func (s *Service) ListActive(ctx, storeID int64) ([]*Groupbuy, error)
func (s *Service) Cancel(ctx, id int64) (*Groupbuy, error)        // active→cancelled
func (s *Service) Join(ctx, groupbuyID, memberID int64) (*Participant, *Groupbuy, error)
func (s *Service) ListParticipants(ctx, groupbuyID int64) ([]*Participant, error)
// FinalizeExpired: 扫 active 且 EndAt<now 且 Joined<Required → failed；返回处理条数（jobs 钩子，cron 留 M3）
func (s *Service) FinalizeExpired(ctx, now time.Time) (int, error)
```

#### 11.3.2 `internal/reach`（BE-B-07+08+10，**核心枢纽包**）

```go
package reach

type ReachTask struct {
    ID, TenantID, StoreID, StrategyID int64
    Channel                           string // wework_group|wework_1v1|subscribe_msg|sms
    TemplateCode                      string
    TargetCount                       int
    Status                            string // pending|sent|blocked|failed
    BlockedReason                     string // 空=非拦截；freq_circuit|content_filter|unsubscribed|subscribe_quota|risk
    ScheduledAt, SentAt               time.Time
    CreatedAt                         time.Time
}
type SubscribeAuth struct {
    ID, TenantID, MemberID int64
    TemplateID             string
    GrantedQuota, UsedQuota int
    GrantedAt              time.Time
}
type UnsubscribeRecord struct {
    ID, TenantID, MemberID int64
    Channel, Reason        string
    UnsubscribedAt         time.Time
}
const (
    ChannelWeworkGroup = "wework_group"
    ChannelWeworkOneV  = "wework_1v1"
    ChannelSubscribeMsg = "subscribe_msg"
    ChannelSMS          = "sms"
    // 8 类触达模板
    TplNewWelcome   = "new_welcome"
    TplBirthday     = "birthday"
    TplRecall30     = "recall_30"
    TplRecall60     = "recall_60"
    TplRecall90     = "recall_90"
    TplRepurchase   = "repurchase"
    TplBalanceLow   = "balance_low"
    TplPointsExpire = "points_expire"
    StatusPending = "pending"
    StatusSent    = "sent"
    StatusBlocked = "blocked"
    StatusFailed  = "failed"
    BlockFreqCircuit    = "freq_circuit"
    BlockContentFilter  = "content_filter"
    BlockUnsubscribed   = "unsubscribed"
    BlockSubscribeQuota = "subscribe_quota"
    BlockRisk           = "risk"
)
var (
    ErrNotFound         = errors.New("reach: task not found")
    ErrTemplateUnknown  = errors.New("reach: unknown template code")
    ErrSubscribeExhausted = errors.New("reach: subscribe quota exhausted")
)

// Sender 是通道适配器（企微/订阅/短信真实实现延后；内存 stub 记录即返回 nil）
type Sender interface {
    Send(ctx context.Context, p *SendPayload) error
}
type SendPayload struct {
    TenantID, MemberID int64
    Channel            string
    TemplateCode       string
    Title, Body        string
}

// 三道防腐窄接口（reach 不直接 import compliance/freqcircuit/contentfilter）
type FreqGate interface {
    Allow(ctx context.Context, channel string) (allowed bool, reason string, err error)
}
type ContentFilter interface {
    Do(text string) ContentResult
}
type ContentResult struct {
    Text     string
    Passed   bool
    HitWords []string
    Replaced []string
}

type ReachRepo interface {
    CreateTask(ctx, t *ReachTask) (*ReachTask, error)
    GetTask(ctx, tenantID, id int64) (*ReachTask, error)
    ListTasks(ctx, tenantID, storeID int64, status string) ([]*ReachTask, error)
    UpdateTaskStatus(ctx, tenantID, id int64, status, blockedReason string, sentAt time.Time) (*ReachTask, error)
    // 订阅授权（原子）
    GrantSubscribe(ctx, tenantID, memberID int64, tplID string, quota int, now time.Time) (*SubscribeAuth, error) // 已存在则 GrantedQuota+=quota
    ConsumeSubscribe(ctx, tenantID, memberID int64, tplID string, now time.Time) (*SubscribeAuth, error)           // used<granted→used++；否则 ErrSubscribeExhausted
    GetSubscribe(ctx, tenantID, memberID int64, tplID string) (*SubscribeAuth, error)
    // 退订（一会员一通道一条；退订后重订阅 UPDATE 清态）
    Unsubscribe(ctx, tenantID, memberID int64, channel, reason string, now time.Time) (*UnsubscribeRecord, error)
    IsUnsubscribed(ctx, tenantID, memberID int64, channel string) (bool, error)
    Resubscribe(ctx, tenantID, memberID int64, channel string) error
}

type Option func(*Service)
func New(repo ReachRepo, opts ...Option) *Service
func WithSender(Sender) Option
func WithFreqGate(FreqGate) Option
func WithContentFilter(ContentFilter) Option
func WithNowFunc(func() time.Time) Option

type RenderInput struct {
    Code       string
    MemberName string
    StoreName  string
    Vars       map[string]string // 弹性: birthday/recall_days/balance/points/product_name 等
}
type Rendered struct{ Code, Title, Body string }
// Render 是纯函数：按 Code 选模板，填充 vars，返回标题+正文（口语化，正文≤120字）
func (s *Service) Render(in RenderInput) (Rendered, error)
// 即使无 Sender/FreqGate/ContentFilter 注入，Render 也可独立调用（零值守卫）

type SendInput struct {
    StoreID, MemberID, StrategyID int64
    Channel, TemplateCode         string
    Vars                          map[string]string
}
// Send 编排（**核心**，顺序固定，任一拦截写 blocked 并返回 ReachTask+nil error）：
//   1. Render 模板（未知码→ErrTemplateUnknown，硬错非 blocked）
//   2. ContentFilter.Do(body) → !Passed ⇒ blocked(content_filter)
//   3. FreqGate.Allow(channel) → !allowed ⇒ blocked(freq_circuit)
//   4. IsUnsubscribed(channel) ⇒ true ⇒ blocked(unsubscribed)
//   5. channel==subscribe_msg ⇒ ConsumeSubscribe ⇒ ErrSubscribeExhausted ⇒ blocked(subscribe_quota)
//   6. Sender.Send ⇒ 失败 status=failed；成功 status=sent
//   首行 EnforceTenant；写 ReachTask 留痕（含 strategy_id 归因）；blocked 不消耗 subscribe 配额之外的计数。
//   未注入的 guard（nil）跳过该步（dev 默认仅 Render+Sender）。
func (s *Service) Send(ctx, in SendInput) (*ReachTask, error)

func (s *Service) GrantSubscribe(ctx, memberID int64, tplID string, quota int) (*SubscribeAuth, error)
func (s *Service) Unsubscribe(ctx, memberID int64, channel, reason string) (*UnsubscribeRecord, error)
func (s *Service) Resubscribe(ctx, memberID int64, channel string) error
func (s *Service) ListTasks(ctx, storeID int64, status string) ([]*ReachTask, error)
func (s *Service) GetTask(ctx, id int64) (*ReachTask, error)
```

#### 11.3.3 `internal/compliance/newaccountguard`（BE-B-09）

```go
package newaccountguard
// 复用 freqcircuit 的计数语义但零领域依赖：自定义 Counter 接口（签名同 freqcircuit.Counter），
// 主线程注入 freqcircuit.MemoryCounter 实例（已实现该签名）。不 import freqcircuit。

type Account struct {
    WeworkUserID string
    CreatedAt    time.Time // 账号创建时间，决定 daily cap
}
type AccountStore interface {
    Get(ctx, tenantID int64, weworkUserID string) (*Account, error)
}
type Counter interface {
    Incr(ctx, key string, expireAt time.Time) (int64, error)
    Decr(ctx, key string) error
    Get(ctx, key string) (int64, error)
}
var ErrAccountNotFound = errors.New("newaccountguard: account not found")

type Option func(*Service)
func New(store AccountStore, opts ...Option) *Service
func WithCounter(Counter) Option     // 默认 NewMemoryCounter()（包内自带，同 freqcircuit 范式）
func WithNowFunc(func() time.Time) Option

// AllowAddFriend: age=daysSince(CreatedAt)；cap=50(age<7)/80(7-29)/200(>=30)；
// key=t:{tid}:freq:add_friend:{weworkUserID}:daily；Incr 后超 cap→Decr 回退返回 (false,"new_account_cap")
func (s *Service) AllowAddFriend(ctx, weworkUserID string) (allowed bool, reason string, err error)
func (s *Service) DailyCapForAge(ageDays int) int // 暴露给测试/观测
```

#### 11.3.4 `internal/compliance/riskwindow`（BE-B-10 风控部分）

```go
package riskwindow
// 租户级滑动窗口风控：1h 窗口内 blocked 次数 / 发送次数超阈值 → 记录 RiskEvent（告警不硬拒）。
// 滑动窗口用「桶列表 + now 裁剪」内存实现（不依赖 Redis），可单测。

type EventType string
const (
    EventHighBlockRate EventType = "high_block_rate" // blocked占比超阈值
    EventBurstSend     EventType = "burst_send"      // 窗口内发送绝对值超阈值
)
type RiskEvent struct {
    ID, TenantID int64
    Type         EventType
    Window       string  // "1h"
    Threshold    int64
    Observed     int64
    CreatedAt    time.Time
}
type Option func(*Service)
func New(opts ...Option) *Service
func WithWindow(time.Duration) Option       // 默认 1h
func WithBlockThreshold(int64) Option        // 默认 10（窗口内 blocked 次数）
func WithSendThreshold(int64) Option         // 默认 200（窗口内发送次数）
func WithNowFunc(func() time.Time) Option

// RecordBlock 记一次拦截事件，返回是否触发 high_block_rate 告警 + 事件（无触发 event=nil）
func (s *Service) RecordBlock(ctx, reason string) (alerted bool, event *RiskEvent, err error)
// RecordSend 记一次发送，返回是否触发 burst_send 告警
func (s *Service) RecordSend(ctx) (alerted bool, event *RiskEvent, err error)
// RecentEvents 返回租户窗口内已记录事件（观测/测试用）
func (s *Service) RecentEvents(ctx) ([]*RiskEvent, error)
```

#### 11.3.5 `internal/crm`（BE-B-06 社群+活码）

```go
package crm

type CommunityGroup struct {
    ID, TenantID, StoreID int64
    WeworkChatID, Name    string
    MemberCount           int
    SOPStage              string // recruit|activate|convert|refer（空=未启动SOP）
    CreatedAt, UpdatedAt  time.Time
}
type LivingCode struct {
    ID, TenantID, StoreID int64
    Code, Scene           string // add_friend|join_group
    Config                LivingCodeConfig
    CreatedAt             time.Time
}
type LivingCodeConfig struct {
    AutoAssignRule string // nearest_store|round_robin
    WelcomeMsg     string
}
const (
    SOPRecruit  = "recruit"
    SOPActivate = "activate"
    SOPConvert  = "convert"
    SOPRefer    = "refer"
    SceneAddFriend = "add_friend"
    SceneJoinGroup = "join_group"
    RuleNearestStore = "nearest_store"
    RuleRoundRobin   = "round_robin"
)
var ErrNotFound = errors.New("crm: not found")

type Repo interface {
    CreateGroup(ctx, g *CommunityGroup) (*CommunityGroup, error)
    GetGroup(ctx, tenantID, id int64) (*CommunityGroup, error)
    ListGroups(ctx, tenantID, storeID int64) ([]*CommunityGroup, error)
    UpdateGroupSOP(ctx, tenantID, id int64, stage string, now time.Time) (*CommunityGroup, error)
    AddGroupMembers(ctx, tenantID, id int64, delta int, now time.Time) (*CommunityGroup, error)
    CreateCode(ctx, lc *LivingCode) (*LivingCode, error)
    GetCode(ctx, tenantID int64, code string) (*LivingCode, error)
    ListCodes(ctx, tenantID, storeID int64) ([]*LivingCode, error)
}
type Option func(*Service)
func New(repo Repo, opts ...Option) *Service
func WithNowFunc(func() time.Time) Option
func WithStorePicker(StorePicker) Option // 活码分流选门店（nearest 需外部 LBS，M2-S2 注入内存 picker）

// 活码分流：按 code 查 LivingCode，按 Config.AutoAssignRule 选 store_id
type StorePicker interface {
    Pick(ctx, tenantID int64, rule string) (storeID int64, err error)
}
// ResolveCode 返回分流到的 storeID + 欢迎语；round_robin 内置轮询，nearest 走 StorePicker（缺省回退首个门店）
func (s *Service) ResolveCode(ctx, code string) (storeID int64, cfg LivingCodeConfig, err error)
// AdvanceSOP: recruit→activate→convert→refer（refer 为终态，再推进报错）
func (s *Service) AdvanceSOP(ctx, groupID int64) (*CommunityGroup, error)
func (s *Service) CreateGroup / GetGroup / ListGroups / AddGroupMembers / CreateCode / ListCodes（薄封装 repo）
```

### 11.4 包所有权（文件集互斥）

| Agent | 只准写这些文件 |
|---|---|
| **M2S2-migrations** | `migrations/000006_reach_community_groupbuy.up.sql` + `.down.sql` |
| **M2S2-groupbuy** | `internal/groupbuy/{groupbuy.go, memory_repo.go, service.go, service_test.go}` |
| **M2S2-reach** | `internal/reach/{reach.go, template.go, memory_repo.go, sender_memory.go, service.go, service_test.go}` |
| **M2S2-newaccountguard** | `internal/compliance/newaccountguard/{newaccountguard.go, memory_counter.go, memory_store.go, service.go, service_test.go}` |
| **M2S2-riskwindow** | `internal/compliance/riskwindow/{riskwindow.go, service.go, service_test.go}` |
| **M2S2-crm** | `internal/crm/{crm.go, memory_repo.go, service.go, service_test.go}` |

> 主线程负责（agent 不写）：`interfaces/http/handler/{reach.go, groupbuy.go, crm.go}`、`interfaces/http/router.go`（Deps 加 Reach/Groupbuy/CRM + 路由）、`cmd/api/main.go`（实例化 freqcircuit.New()+contentfilter.New()+reach/groupbuy/crm，适配 FreqGate/ContentFilter/Sender/MemberLookuper/StorePicker 窄接口）、`interfaces/http/m2s2_test.go`、`pkg/errcode/errcode.go`（新增 60009+ 码）。

### 11.5 硬约束（M2-S2 全员）

1. 每个 repo/service 方法首行 `tenantID, err := multitenancy.EnforceTenant(ctx)`；tenant **永不**从请求体/参数取。
2. 内存实现 `sync.Mutex`/`sync.RWMutex` + 防御性拷贝 + 跨租户 `ErrNotFound`（串户矩阵必覆盖：tenant A 读/发/参团 tenant B 的资源→ErrNotFound/拒绝）。
3. 金额 int64 分（groupbuy.PriceCents），禁浮点。
4. **原子临界区**：groupbuy `JoinAtomic`（未满+未参团→插+joined++，满员→success，全成功或全回滚）；reach `ConsumeSubscribe`（used<granted→used++，CHECK 约束语义）；newaccountguard `AllowAddFriend`（超 cap→Decr 回退）。
5. 防腐：`groupbuy`/`reach`/`crm` 不 import `membership`/`marketing`/`catalog`（走 MemberLookuper/Sender 等窄接口，主线程 main.go 适配）；`reach` 不直接 import `freqcircuit`/`contentfilter`（走 FreqGate/ContentFilter 窄接口）；`newaccountguard`/`riskwindow` 不 import 任何领域包。跨域依赖全走窄接口。
6. reach.Send **顺序固定**（render→content→freq→unsub→subscribe quota→sender），blocked 写 ReachTask 留痕且返回 `(task, nil)`（非 error，让上层区分「拦截」与「系统错」）。
7. 不引入白名单外依赖；不连真实 DB/Redis/企微；测试用 fake 接口 + 注入 clock/counter。
8. 每个 `package xx` 上方 `// Package xx ...` 文档注释；`gofmt` 干净。

### 11.6 DoD（M2-S2）

- `go build ./... && go vet ./... && gofmt -l .`（空）`&& go test -race ./...` 全绿。
- groupbuy 单测：建团→参团(joined++)→满员成团(success)→超员拒绝→重复参团拒绝→非 active 参团拒绝→FinalizeExpired(超时未满→failed)→串户拒绝。
- reach 单测：8 模板渲染正确；Send 全链路(渲染→频次拦截→内容拦截→退订拦截→订阅计数拦截→发送成功)；订阅 Grant/Consume 原子(超额拒绝)；退订阻断+重订阅恢复；blocked 返回 task 非 error；串户拒绝。
- newaccountguard 单测：三档 cap(50/80/200)正确、超限 Decr 回退、跨租户 key 隔离、AccountStore 缺失报 ErrAccountNotFound。
- riskwindow 单测：滑动窗口裁剪过期桶、blocked/send 双阈值触发告警、告警不阻断后续（不硬拒）。
- crm 单测：社群 SOP 推进(recruit→…→refer 终态报错)；活码分流(round_robin 轮询、nearest 回退)；串户拒绝。
- 迁移 golang-migrate 命名合法、up/down 对称、CHECK/UNIQUE 约束齐全。
- HTTP 冒烟（主线程）：dev mock 下「建团→参团→满员成团」；「发触达→频次熔断 blocked→换通道/退订 blocked」；订阅授权计数；串户证明（X-Mock-Tenant 隔离）。

---

## 12. M3-S1 增量契约（AI-02/03/04/05 + BE-B-11 + DB-04 + FE-MP-03，2026-07-28）

> M3-S1 是产品核心卖点「AI 今日营销军师：今日 3 件事 + 一键执行」。落地五段式管线 Signal→Rule→LLM→Converge→Schema + ActionExecutor。设计依据 ch07 §7.1-7.2（管线/规则）+ ch05 §5.3.8（strategies DDL）+ `~/Selfproj/zhanggui-ai-demo/`（Python 五段式范式）。**LLM 复用 `internal/aiengine/gateway`（已就绪 Chat/Budget），全程可降级**。金额 int64 cents / permille / integer pct，毛利护栏复用 `internal/margin`（纯函数）。

### 12.1 范围与裁剪（对照 ch05 §5.3.8 / ch07 §7.1-7.2）

**必做**：DB-04（strategies/strategy_items/strategy_attribution/strategy_feedback）；AI-02 规则引擎（struct+JSON，**非 DSL**）+ 5 业态 in-code 种子；AI-03 毛利护栏（复用 margin，三道校验）；AI-04 动作库（exec 6 handler）；AI-05 五段式管线；BE-B-11 ActionExecutor；FE-MP-03 商家小程序今日军师页。

**显式延后**：AI-06 语义缓存 PGVector（M3-S2，本期 Cache 仅内存 L1 精确 hash）；AI-07 凌晨预生成 cron（M3-S2）；AI-08 gateway lite/V3/R1 分层路由（M3-S2，本期全 lite）；AI-10 归因回填 + AI-12 bandit（M3-S3，表建好但空）；AI-11 剧本库后台编辑（M3-S3，本期 in-code 种子）；tool-calling LLM 循环（M3-S2+，本期 gateway.Chat 单轮 json_object）；moments 朋友圈实发（M3-S2，handler 生成文案 Sender=nil 只 audit）；recharge_promo 执行（M4，handler 返回 ErrActionNotExecutable）。

### 12.2 迁移编号（DB-04）

`migrations/000007_ai_strategy.{up,down}.sql`（ch05 规划名 000008，实际按落地顺序用 000007；payment 域 M4 再用 000008）。建 4 表，逐字段对齐 ch05 §5.3.8 行 536–595 + 本契约的有意增补（strategy_items 加 `reason_type` 列 + execution_status CHECK 加 `blocked` 值，服务于 action_type 双层设计）：

- `strategies`：id/uuid/tenant_id/store_id/for_date(unique w/ tenant+store)/signals(jsonb)/data_quality_score/generated_by(CHECK 4值)/llm_tier/total_cost_microcents/status(CHECK 4值)/created_at。无 updated_at（策略每日 upsert 覆盖，不挂 trg）。
- `strategy_items`：id/tenant_id/strategy_id(FK)/rank(CHECK 1-3)/action_type(CHECK **6 值**)/reason_type(CHECK **7 值**，本期增补)/target(jsonb)/product_id(FK 可空)/params(jsonb)/copy/reason/expected_effect(jsonb)/margin_ok(boolean)/execution_status(CHECK `pending|executed|skipped|failed|blocked`，本期增补 blocked)/executed_at。UNIQUE(tenant_id,strategy_id,rank)。
- `strategy_attribution`：id/tenant_id/strategy_item_id(FK)/metric(CHECK 5值)/value(double)/measured_at。UNIQUE(tenant_id,strategy_item_id,metric,measured_at)。本期建表空（M3-S3 回填）。
- `strategy_feedback`：id/tenant_id/strategy_item_id(FK)/useful(boolean)/created_at。UNIQUE(tenant_id,strategy_item_id)。

> `strategy_cache`/`marketing_cases`（pgvector）属 §5.3.9，本期**不建**（M3-S2）。down.sql 逆序 drop 4 表。

### 12.3 领域类型与仓储接口（**已固定，agent 必须一字不差实现签名**）

#### 12.3.0 全包共享常量 + 映射表（放 `rule` 包 `actions.go` 导出；exec import 取常量；strategy/output 用裸 string 不 import rule）

```go
// rule/actions.go —— ActionType 6 值执行通道（落 strategy_items.action_type，对齐 ch05 CHECK）
const (
    ActGroupSeckill  = "group_seckill"   // 群秒杀(临期/雨天/滞销捆绑)→发券+群发
    ActCouponWake    = "coupon_wake"     // 券唤醒私聊(沉睡/节日/生日/高价值)→发券+1v1触达
    ActMomentsPost   = "moments_post"    // 朋友圈(高毛利推送)→文案(M3-S1 audit 不实发)
    ActPriceAdj      = "price_adj"       // 改价(高危,需二次确认)→catalog.UpdatePrice
    ActGroupbuy      = "groupbuy"        // 拼团→groupbuy.Create
    ActRechargePromo = "recharge_promo"  // 储值促销→stub(M4)
)
// ReasonType 7 值规则语义（落 strategy_items.reason_type，规则库「为什么」）
const (
    ReasonClearanceCoupon = "clearance_coupon" // 临期清仓
    ReasonRainDelivery    = "rain_delivery"    // 雨天配送
    ReasonBundleSlow      = "bundle_slow"      // 滞销捆绑
    ReasonWakeDormant     = "wake_dormant"     // 沉睡唤醒
    ReasonFestival        = "festival_discount"// 节日满减
    ReasonBirthday        = "birthday_care"    // 生日关怀
    ReasonHighValue       = "high_value_push"  // 高毛利推送
)
// ReasonToAction 冻结映射（rule 产 ActionType 用；exec 禁看 ReasonType）
var ReasonToAction = map[string]string{
    ReasonClearanceCoupon: ActGroupSeckill,
    ReasonRainDelivery:    ActGroupSeckill,
    ReasonBundleSlow:      ActGroupSeckill,
    ReasonWakeDormant:     ActCouponWake,
    ReasonFestival:        ActCouponWake,
    ReasonBirthday:        ActCouponWake,
    ReasonHighValue:       ActCouponWake, // M3-S1 固定映射；服装业态可走 moments_post 的二次映射留 M3-S3
}
// 状态常量（StatusXxx/ItemXxx/GenXxx）定义在 strategy 包（持久化域状态），见 §12.3.1。
```

#### 12.3.1 `internal/strategy`（A1，持久化域，零领域 import）

```go
package strategy

import (
    "context"
    "encoding/json"
    "errors"
    "time"
)

// 状态常量（strategies.status / strategy_items.execution_status / strategies.generated_by）
const (
    StatusGenerated         = "generated"         // strategies.status
    StatusPartiallyExecuted = "partially_executed"
    StatusExecuted          = "executed"
    StatusDismissed         = "dismissed"
    ItemPending             = "pending"           // strategy_items.execution_status
    ItemExecuted            = "executed"
    ItemSkipped             = "skipped"
    ItemFailed              = "failed"
    ItemBlocked             = "blocked"           // 一键执行被合规拦截(本期增补)
    GenCacheHit             = "cache_hit"         // strategies.generated_by
    GenOfflinePregen        = "offline_pregen"    // M3-S2
    GenLiveLLM              = "live_llm"
    GenRuleOnly             = "rule_only"
)

type Strategy struct {
    ID, TenantID, StoreID   int64
    ForDate                 string          // YYYY-MM-DD
    Signals                 json.RawMessage // 当日 signal.Bundle 快照（output 落库时 marshal 注入；strategy 包不 import signal）
    DataQualityScore        int             // 0-100
    GeneratedBy             string          // cache_hit|offline_pregen|live_llm|rule_only
    LLMTier                 string          // rule_only 时空
    TotalCostMicroCents     int64           // 本次 token 成本（1元=10000微分）
    Status                  string          // generated|partially_executed|executed|dismissed
    Items                   []StrategyItem
    CreatedAt               time.Time
}

type StrategyItem struct {
    ID, TenantID, StrategyID int64
    Rank                     int            // 1-3
    ActionType               string         // 6 值（ActXxx）
    ReasonType               string         // 7 值语义（ReasonXxx）
    Target                   map[string]any // 受众筛选（落 JSONB）
    ProductID                int64          // 0=非商品绑定
    Params                   map[string]any // 折扣/满减/文案（落 JSONB）
    Copy                     string         // ≤40 字
    Reason                   string         // 推荐理由（老板可读）
    ExpectedEffect           map[string]any // 预期效果（落 JSONB）
    MarginOK                 bool           // 毛利护栏校验
    ExecutionStatus          string         // pending|executed|skipped|failed|blocked
    ExecutedAt               *time.Time
}

type StrategyFeedback struct {
    ID, TenantID, StrategyItemID int64
    Useful                       bool
    CreatedAt                    time.Time
}

type StrategyFilter struct {
    StoreID int64
    Date    string // 空不过滤
    Status  string // 空不过滤
    Limit   int    // <=0 默认 50
    Offset  int
}

var (
    ErrNotFound = errors.New("strategy: not found") // 跨租户/不存在统一返回此
    ErrParam    = errors.New("strategy: invalid param")
)

type StrategyRepo interface {
    // SaveAdvice 原子 upsert：按 (tenantID, storeID, forDate) 覆盖（同日重新生成覆盖草稿，不新增行）。
    // 写 strategies + strategy_items（事务语义；内存实现单 mutex）。落库后 Strategy.ID/Items[].ID 回填。
    SaveAdvice(ctx context.Context, s *Strategy) (*Strategy, error)
    Get(ctx context.Context, tenantID, id int64) (*Strategy, error)              // ErrNotFound/跨租户
    List(ctx context.Context, tenantID int64, f StrategyFilter) ([]*Strategy, error)
    GetToday(ctx context.Context, tenantID, storeID int64, date string) (*Strategy, error) // ErrNotFound/跨租户
    GetItem(ctx context.Context, tenantID, itemID int64) (*StrategyItem, error)            // ErrNotFound/跨租户
    // UpdateItemStatus 一键执行后回写 execution_status + executed_at；非法迁移→ErrParam。
    UpdateItemStatus(ctx context.Context, tenantID, itemID int64, status string, executedAt *time.Time, now time.Time) (*StrategyItem, error)
    RecordFeedback(ctx context.Context, tenantID, strategyItemID int64, useful bool, now time.Time) (*StrategyFeedback, error) // UNIQUE 语义：同 item 重复反馈覆盖
}

type Option func(*Service)
func New(repo StrategyRepo, opts ...Option) *Service
func WithNowFunc(func() time.Time) Option

// Service（首行 EnforceTenant；包装 repo，加业务校验）：
//   SaveAdvice(ctx, s *Strategy) (*Strategy, error)
//   Get(ctx, id int64) (*Strategy, error)
//   GetToday(ctx, storeID int64, date string) (*Strategy, error)   // date 空取今天
//   List(ctx, f StrategyFilter) ([]*Strategy, error)
//   GetItem(ctx, itemID int64) (*StrategyItem, error)
//   UpdateItemStatus(ctx, itemID int64, status string) (*StrategyItem, error)   // executedAt: status∈{executed,blocked,failed,skipped}→now；pending→nil
//   RecordFeedback(ctx, itemID int64, useful bool) (*StrategyFeedback, error)
```

#### 12.3.2 `internal/aiengine/signal`（A2，types + collector + 防腐 Lookuper 端口）

```go
package signal

import (
    "context"
    "time"
)

type Bundle struct {
    TenantID     int64         `json:"tenant_id"`
    StoreID      int64         `json:"store_id"`
    BusinessType string        `json:"business_type"` // fruit|convenience|maternity|fresh|apparel
    City         string        `json:"city"`
    Date         time.Time     `json:"date"`
    Weather      Weather       `json:"weather"`
    Holiday      Holiday       `json:"holiday"`
    Inventory    []InventorySig `json:"inventory"`
    Members      MemberStats   `json:"members"`
    DataQuality  int           `json:"data_quality"` // 0-100，仅 trace
}
type Weather struct {
    Condition string  `json:"condition"` // rain|hot|cold|snow|normal
    TempC     float64 `json:"temp_c"`     // 温度可 float（非金额）
    Forecast  string  `json:"forecast"`
}
type Holiday struct {
    Name       string `json:"name"`        // 端午|中秋|母亲节|无
    DaysTo     int    `json:"days_to"`     // 距今天数，-1=无
    IsFestival bool   `json:"is_festival"`
}
// InventorySig 金额一律 int64 cents（非 float）；库存数量/天数用 float64/int
type InventorySig struct {
    ProductID    int64   `json:"product_id"`
    Name         string  `json:"name"`
    CostCents    int64   `json:"cost_cents"`
    PriceCents   int64   `json:"price_cents"`
    Stock        float64 `json:"stock"`
    DaysToExpiry int     `json:"days_to_expiry"` // -1=非临期
    SlowMoving   bool    `json:"slow_moving"`
    HighMargin   bool    `json:"high_margin"`
}
type MemberStats struct {
    Total         int     `json:"total"`          // 会员总数（best-effort）
    Sleep30       int     `json:"sleep_30"`       // 30 天未到店人数（M3-S1 best-effort，可为 0）
    HighValue     []int64 `json:"high_value"`     // 高价值会员 ID（M3-S1 best-effort，可空）
    BirthdayToday []int64 `json:"birthday_today"` // 当天生日（M3-S1 best-effort，可空）
}
type TxnSig struct {
    RevenueCents int64 `json:"revenue_cents"` // 当日营收
    OrderCount   int   `json:"order_count"`
    NewMembers   int   `json:"new_members"`
}

// Lookuper 防腐端口（定义在 signal 包；主线程 main.go 适配 membership/inventory/billing/catalog/dataquality）。
// M3-S1 best-effort：底层无法枚举时返回零值/空切片 + nil error（触发上层冷启动降级）。
type Lookuper interface {
    Inventory(ctx context.Context, tenantID, storeID int64) ([]InventorySig, error) // 临期/滞销/高毛利（catalog.List + inventory + dataquality.ComputeTags 推导）
    Members(ctx context.Context, tenantID int64) (MemberStats, error)               // best-effort（M2 无 List，多为零值）
    DailyTxn(ctx context.Context, tenantID, storeID int64, date string) (TxnSig, error)
}

// Collector 聚合信号（errgroup 并发拉三路，任一失败→空子集不阻断；weather/holiday 内存 mock）。
type Collector interface {
    Collect(ctx context.Context, tenantID, storeID int64) (*Bundle, error)
}
// New 接口实现：func NewCollector(businessType, city string, lk Lookuper, opts ...Option) *Service
// （Service 实现 Collector；Collect 返回 *Bundle，永远非 nil——即使全空也返回零 Bundle 供冷启动判定）
```

#### 12.3.3 `internal/aiengine/rule`（A2，import signal + margin；**不 import strategy 域类型**，只 import strategy 取 ActionType/ReasonType 常量）

```go
package rule

import (
    "context"
    "github.com/zhangguijunshi/backend/internal/aiengine/signal"
    "github.com/zhangguijunshi/backend/internal/margin" // 纯函数 lib，直接 import（非领域包）
)
// ActionType/ReasonType 常量 + ReasonToAction 映射定义在本包 actions.go（见 §12.3.0）。
// rule 不 import strategy（保持 Wave 1 与 A1 无跨依赖）；strategy/output 只存/传裸 string。

type Routed struct {
    TenantID int64        `json:"tenant_id"`
    Items    []RoutedItem `json:"items"`
}
// RoutedItem 金额 int64 cents；ActionType 由 ReasonToAction 映射得出（不在 Rule 里手填）
type RoutedItem struct {
    RuleID         string     `json:"rule_id"`          // clearance_fruit_3d / cold_start_wake
    ReasonType     string     `json:"reason_type"`      // 7 值（ReasonXxx）
    ActionType     string     `json:"action_type"`      // 6 值（ActXxx，由 ReasonToAction 映射）
    Target         string     `json:"target"`           // member_segment|product|group
    ProductID      int64      `json:"product_id"`       // 0=非商品绑定
    Product        string     `json:"product"`          // "-"=非商品绑定
    CostCents      int64      `json:"cost_cents"`       // 0=满减类不绑商品
    PriceCents     int64      `json:"price_cents"`
    CostCeilCents  int64      `json:"cost_ceil_cents"`  // 护栏下限：cost*(100+floor)/100，0=不校验
    MarginFloorPct int64      `json:"margin_floor_pct"` // 5=5%
    Priority       int        `json:"priority"`
    Reason         string     `json:"reason"`           // 老板可读理由
    ParamRange     ParamRange `json:"param_range"`
}
// ParamRange：LLM 只能在区间内润色（全 int64，禁 float）
type ParamRange struct {
    DiscountMinPermille int64 `json:"discount_min_permille"` // 500=5 折
    DiscountMaxPermille int64 `json:"discount_max_permille"` // 700=7 折
    ThresholdCents      int64 `json:"threshold_cents"`       // 满 X
    OffCents            int64 `json:"off_cents"`             // 减 Y
    LimitQtyMax         int   `json:"limit_qty_max"`
    ExpireHours         int   `json:"expire_hours"`
}

type Rule struct {
    ID             string     `json:"id"`              // clearance_fruit_3d
    Business       string     `json:"business"`        // fruit|*（* 全业态）
    Priority       int        `json:"priority"`        // 越大越先
    Enable         bool       `json:"enable"`
    Conds          []Cond     `json:"conds"`           // AND
    Action         Action     `json:"action"`
    MarginFloorPct int64      `json:"margin_floor_pct"` // 5=5%，核心不可关闭
}
type Cond struct {
    Field string `json:"field"` // weather.condition|holiday.days_to|inventory.days_to_expiry|inventory.slow_moving|inventory.high_margin|members.sleep_30|members.total
    Op    string `json:"op"`    // eq|lt|gt|gte|lte|between|in|exists
    Value any    `json:"value"` // between=[lo,hi]；in/exists=判列表/字段非空
}
type Action struct {
    ReasonType string     `json:"reason_type"`   // 7 值（Rule 仅声明语义；ActionType 由映射表得出）
    Target     string     `json:"target"`
    ParamRange ParamRange `json:"param_range"`
}

// Engine：规则按 Priority 降序匹配；命中后过毛利护栏产出 RoutedItem。命中的 ActionType = strategy.ReasonToAction[ReasonType]。
type Engine interface {
    Route(ctx context.Context, b *signal.Bundle) (*Routed, error) // 永不返回 error（异常→空 Routed；命中 0 条由 Pipeline 注入冷启动）
}
// 构造：func NewEngine(rules []Rule, guard margin.Guard, opts ...Option) *service
//   （service 实现 Engine；rules 已按 Priority 降序、过滤 Enable=false）
// 种子：func DefaultSeeds() []Rule  // 5 业态 in-code 种子（fruit/convenience/maternity/fresh/apparel 各若干条，覆盖 7 个 ReasonType）

// ApplyGuardrail（纯函数，rule/guardrail.go）：对单个 (inv, rule) 校验护栏产出 RoutedItem。
//   推导 newPrice 下限 → margin.ValidatePriceAdj 二次确认 → 通过返回 (RoutedItem, true)；不达标 (zero, false)。
//   cost/price<=0 → 直接 (zero, false)（无成本数据不给出价动作，防幻觉亏钱）。
func ApplyGuardrail(inv signal.InventorySig, r Rule, guard margin.Guard) (RoutedItem, bool)
```

#### 12.3.4 `internal/aiengine/cache`（A2，L0 零依赖；本期内存 L1 精确 hash，PGVector 留 M3-S2）

```go
package cache

import (
    "context"
    "github.com/zhangguijunshi/backend/internal/strategy" // Cache 存 *strategy.Strategy（落库后的当日策略）
)

// Cache L1 精确 hash：key = (tenantID, storeID, date)。本期内存；M3-S2 加 PGVector 语义缓存实现同接口。
type Cache interface {
    TryHit(ctx context.Context, tenantID, storeID int64, date string) (*strategy.Strategy, bool)
    Put(ctx context.Context, tenantID, storeID int64, date string, s *strategy.Strategy) // 回写失败不阻断（调用方吞错）
}
// func NewMemoryCache() *MemoryCache // sync.RWMutex + map；TryHit 命中返回策略副本
```

#### 12.3.5 `internal/aiengine/agent`（A3，import rule + gateway；LLM 增强段，**永不返回 error**）

```go
package agent

import (
    "context"
    "github.com/zhangguijunshi/backend/internal/aiengine/gateway"
    "github.com/zhangguijunshi/backend/internal/aiengine/rule"
)

type Enhanced struct {
    TenantID int64          `json:"tenant_id"`
    Items    []EnhancedItem `json:"items"`
}
type EnhancedItem struct {
    Routed        rule.RoutedItem `json:"routed"`
    Copy          string          `json:"copy"`           // ≤40 字（模板兜底或 LLM 润色）
    Params        map[string]any  `json:"params"`         // 折扣/满减/限份（已被护栏 clamp 到 ParamRange 内）
    ExpectedEffect string         `json:"expected_effect"`// "核销35份 挽回损耗¥180"
    MarginOK      bool            `json:"margin_ok"`       // route 段已保证 true；LLM 后再校验
}

// Stage 依赖 gateway.Gateway。EnhanceOrSkip 永远返回 *Enhanced（非 nil），永不返回 error：
//   budget.Exceeded / ErrNoProvider / Chat err / JSON 解析失败 / 超时 → 全部降级为模板文案（copy 从 ActionType 模板表取，
//   params 取 ParamRange 下限，MarginOK 保持 route 段的 true）。至少返回 Routed-only Enhanced。
type Stage interface {
    EnhanceOrSkip(ctx context.Context, tenantID int64, r *rule.Routed, degrade *string) *Enhanced
    // degrade 是 Pipeline 维度的降级标记指针：若全程走模板，置 *degrade="llm" 或 "budget" 或 "no_provider"（仅当全程未命中一次 LLM 时）
}
// func New(gw *gateway.Gateway, opts ...Option) *service // gw 可为空 provider 网关（dev 无 key），EnhanceOrSkip 自动降级
```

#### 12.3.6 `internal/aiengine/converge`（A3，import agent；Top3 排序）

```go
package converge

import (
    "context"
    "github.com/zhangguijunshi/backend/internal/aiengine/agent"
)

// PickTop3 按预期增量价值加权排序取前 3（RoutedItem.Priority 为主，ExpectedEffect 估算为辅）。
// margin_ok=false 排尾不丢。不足 3 条按实际返回（≥1，冷启动保证）。异常→原序前 3。
type Top3 interface {
    PickTop3(ctx context.Context, e *agent.Enhanced) []*agent.EnhancedItem
}
// func NewTop3() *service
```

#### 12.3.7 `internal/aiengine/output`（A3，import agent + strategy + signal；编码 + 落库）

```go
package output

import (
    "context"
    "github.com/zhangguijunshi/backend/internal/aiengine/agent"
    "github.com/zhangguijunshi/backend/internal/aiengine/signal"
    "github.com/zhangguijunshi/backend/internal/strategy"
)

// Advice = 已落库的当日策略 + 管线运行时降级标记（透传前端）
type Advice struct {
    Strategy *strategy.Strategy `json:"strategy"`
    Degrade  string             `json:"degrade,omitempty"` // 空=live_llm；budget|llm|no_provider|cold_start|cache_hit
}

// Encoder 把 Top3 EnhancedItem 编码为 strategy.Strategy + 落库（via strategy.Service.SaveAdvice），返回 Advice。
//   rank 1-3 分配；copy≤40 字 + contentfilter；margin_ok 强制 true；reason_type/action_type 落 item；
//   signal.Bundle marshal 进 strategy.Signals 快照；GeneratedBy 由 degrade 推导（空→live_llm；cold_start/budget/...→rule_only）。
type Encoder interface {
    Encode(ctx context.Context, tenantID, storeID int64, top3 []*agent.EnhancedItem, bundle *signal.Bundle, degrade string) (*Advice, error)
}
// func NewEncoder(svc *strategy.Service, opts ...Option) *service
```

#### 12.3.8 `internal/aiengine` 根 `pipeline.go`（A3 独占 aiengine 包根；五段总控）

```go
package aiengine

import (
    "context"
    "github.com/zhangguijunshi/backend/internal/aiengine/agent"
    "github.com/zhangguijunshi/backend/internal/aiengine/cache"
    "github.com/zhangguijunshi/backend/internal/aiengine/converge"
    "github.com/zhangguijunshi/backend/internal/aiengine/output"
    "github.com/zhangguijunshi/backend/internal/aiengine/rule"
    "github.com/zhangguijunshi/backend/internal/aiengine/signal"
)

// Pipeline 五段总控。Run 是 handler 唯一调用面。
//   顺序：0)L1 cache TryHit(命中→Degrade=cache_hit 直返) ①signal.Collect ②rule.Route(0条→冷启动注入,cold_start) ③agent.EnhanceOrSkip ④converge.Top3 ⑤output.Encode+落库+cache.Put
//   全程 context.WithTimeout(8s)；段间 trace 打 log；任一段异常降级不报错（Encode 失败除外→返回 error）。
func (p *Pipeline) Run(ctx context.Context) (*output.Advice, error)

type Option func(*Pipeline)
func NewPipeline(collector signal.Collector, router rule.Engine, stage agent.Stage, conv converge.Top3, enc output.Encoder, ch cache.Cache, opts ...Option) *Pipeline
func WithBusinessType(string) Option // 默认 "fruit"（冷启动样板业态）
```

#### 12.3.9 `internal/aiengine/exec`（A4，独立子包；import strategy + margin + 自定义 4 端口；**不依赖 pipeline**）

```go
package exec

import (
    "context"
    "github.com/zhangguijunshi/backend/internal/aiengine/rule" // 取 ActionType 常量做 dispatch
    "github.com/zhangguijunshi/backend/internal/margin"
    "github.com/zhangguijunshi/backend/internal/strategy"
)

// === 防腐端口（定义在 exec/ports.go；主线程 main.go 适配 marketing/reach/catalog/groupbuy）===

// CouponRef 是发券结果（归因用）
type CouponRef struct {
    MemberCouponID int64
    MemberID       int64
}
type CouponIssuer interface {
    // Issue 适配 marketing.Service.Issue；source 固定 "ai_strategy"，strategyID 是归因主键。
    Issue(ctx context.Context, memberID, couponID, strategyID int64, expireAt *time.Time) (*CouponRef, error)
}

// SendRef 复制 reach.ReachTask 的拦截语义（归因用——blocked 非 error）
type SendRef struct {
    TaskID        int64
    Status        string // sent|blocked|failed
    BlockedReason string // freq_circuit|content_filter|unsubscribed|subscribe_quota（status=blocked 时非空）
}
type MessageSender interface {
    // Send 适配 reach.Service.Send；channel/template/vars 由 executor 按 ActionType 决定。
    Send(ctx context.Context, storeID, memberID, strategyID int64, channel, tplCode string, vars map[string]string) (*SendRef, error)
}

type PriceUpdater interface {
    // UpdatePrice 适配 catalog.Service.UpdatePrice（高危动作，executor 调用前已做 margin 预判 + Confirm 校验）。
    UpdatePrice(ctx context.Context, productID, costCents, priceCents int64) error
}

type GroupbuyRef struct{ GroupbuyID int64 }
type GroupbuyOpener interface {
    // Launch 适配 groupbuy.Service.Create；strategyID 落 groupbuy 归因（M2-S2 groupbuy 无 strategy_id 字段则仅返回 ID）。
    Launch(ctx context.Context, storeID, productID int64, title string, priceCents int64, requiredCount, validHours int, strategyID int64) (*GroupbuyRef, error)
}

// === 执行结果（blocked 归因靠这个结构）===
type ExecResult struct {
    StrategyItemID int64    `json:"strategy_item_id"`
    Status         string   `json:"status"`          // executed|blocked|failed（对齐 strategy.ItemXxx + ItemBlocked）
    Reason         string   `json:"reason,omitempty"`// margin_floor|freq_circuit|content_filter|unsubscribed|subscribe_quota|not_executable
    TaskID         int64    `json:"task_id,omitempty"`   // reach.ReachTask.ID（触达类）
    RefID          int64    `json:"ref_id,omitempty"`    // MemberCouponID / GroupbuyID（按 ActionType 语义）
}

// === Executor ===
type Executor struct {
    coupons  CouponIssuer
    reacher  MessageSender
    pricer   PriceUpdater
    groupbuy GroupbuyOpener
    guard    margin.Guard   // 执行前第三次护栏校验（与 rule/agent 同一 Guard config）
    svc      *strategy.Service
    now      func() time.Time
}

type ExecuteInput struct {
    StrategyItemID int64
    Confirm        bool // price_adj 高危动作需老板二次确认（前端传 true）
}
// Run 按 strategy_item_id 加载 item（从 repo，不信任调用方传参）→ 按 item.ActionType dispatch 到 6 handler：
//   ActGroupSeckill: couponIssuer 发券 + messageSender 群发(wework_group, tpl 按 ReasonType 选)
//   ActCouponWake:   couponIssuer 发券 + messageSender 1v1(wework_1v1, tpl=recall_30/birthday/repurchase 按 ReasonType)
//   ActMomentsPost:  messageSender 生成文案 audit（M3-S1 Sender 走 reach.Send wework_group 占位 或 仅 audit）
//   ActPriceAdj:     Confirm==false→Status=blocked(reason=confirm_required)；true→margin 预判→priceUpdater.UpdatePrice
//   ActGroupbuy:     groupbuyOpener.Launch
//   ActRechargePromo:Status=blocked(reason=not_executable)（M4 stub）
// 归因：messageSender 返回 SendRef.Status!=sent → ExecResult.Status=blocked + Reason=BlockedReason + item 落 ItemBlocked（不标 executed）。
// margin 预判破底 → Status=blocked + Reason=margin_floor。真实系统错 → Status=failed + 返回 error。
// 首行 EnforceTenant；item 跨租户→ErrNotFound→HTTP 404。
func (e *Executor) Run(ctx context.Context, in ExecuteInput) (*ExecResult, error)
func NewExecutor(svc *strategy.Service, guard margin.Guard, coupons CouponIssuer, reacher MessageSender, pricer PriceUpdater, groupbuy GroupbuyOpener, opts ...Option) *Executor
```

### 12.4 包所有权（文件集互斥，每个 agent 拥有互斥的**包**）

| Agent | 只准写这些文件 |
|---|---|
| **M3S1-strategy**（A1） | `migrations/000007_ai_strategy.up.sql` + `.down.sql`；`internal/strategy/{strategy.go, memory_repo.go, service.go, service_test.go}`；`internal/aiengine/cache/{cache.go, memory_cache.go, cache_test.go}`（cache 存 *strategy.Strategy，强耦合 strategy，故同归 A1） |
| **M3S1-signal-rule**（A2） | `internal/aiengine/signal/{types.go, collector.go, service.go, service_test.go}`；`internal/aiengine/rule/{actions.go, rule.go, engine.go, guardrail.go, seeds.go, service.go, service_test.go}` |
| **M3S1-pipeline**（A3） | `internal/aiengine/agent/{agent.go, stage.go, templates.go, service.go, service_test.go}`；`internal/aiengine/converge/{top3.go, service.go, service_test.go}`；`internal/aiengine/output/{types.go, encoder.go, service.go, service_test.go}`；`internal/aiengine/pipeline.go` + `pipeline_test.go`（A3 独占 aiengine 包根） |
| **M3S1-exec**（A4） | `internal/aiengine/exec/{ports.go, executor.go, service.go, service_test.go}` |

> 主线程负责（agent 不写）：`interfaces/http/handler/aiengine.go`、`interfaces/http/router.go`（Deps 加 Pipeline/Executor + 路由）、`cmd/api/main.go`（实例化全链 + 5 防腐适配器：signalLookuperAdapter/couponIssuerAdapter/msgSenderAdapter/priceUpdaterAdapter/groupbuyOpenerAdapter）、`interfaces/http/m3s1_test.go`、`pkg/errcode/errcode.go`（新增 60017+ 码）。

> **波次依赖**（严格）：Wave 0（本 §12）→ Wave 1（A1‖A2，无跨 import）→ Wave 2（A3‖A4，依赖 Wave 1 的 signal/rule/strategy/cache 类型）→ Wave 3（主线程）。`rule` import `signal` 故二者同属 A2（agent 内部串行 signal→rule）；`exec` 独立子包故 A3/A4 可并行（避免与 pipeline.go 同属 aiengine 根）。

### 12.5 硬约束（M3-S1 全员）

1. 每个 repo/service 方法首行 `tenantID, err := multitenancy.EnforceTenant(ctx)`；tenant **永不**从请求体/参数取。
2. 内存实现 `sync.Mutex`/`sync.RWMutex` + 防御性拷贝 + 跨租户 `ErrNotFound`（串户矩阵必覆盖）。
3. **金额全程 int64 cents / permille / integer pct，禁浮点**（温度/库存数量/天数除外）。护栏一律走 `margin.ValidateCoupon`/`ValidatePriceAdj`，**禁止在 aiengine 包重写 float 护栏**。
4. **三道毛利护栏同一来源**：rule.ApplyGuardrail（emit 时）/ agent（LLM 后 clamp）/ exec（执行前预判）共用同一 `margin.Guard{MarginFloorPct}`（config 注入，main.go 给 rule+exec 同一个值；marketing.Issue 内部 catalogMarginGuard 是最终权威，三道幂等 defense-in-depth）。
5. 防腐：`signal` 不 import membership/inventory/billing/catalog（走 Lookuper 窄接口）；`exec` 不 import marketing/reach/catalog/groupbuy（走 4 端口窄接口）；`rule` 直接 import margin（纯函数 lib，非领域包）+ strategy（仅取常量）。`strategy` 零领域 import（Signals 用 json.RawMessage 不引 signal 类型）。
6. **agent.EnhanceOrSkip 永不返回 error**（LLM 失败/无 provider/超时/坏 JSON 全降级模板）；`rule.Engine.Route` 永不返回 error（异常→空 Routed）。降级非失败：`output.Advice.Degrade` 标记 + HTTP 200。
7. **exec blocked 归因**：MessageSender 返回 `SendRef.Status!=sent` → `ExecResult.Status=blocked` + `Reason=BlockedReason` + strategy_item 落 `ItemBlocked`（**不**标 executed）。margin 破底 → blocked+margin_floor。
8. **strategy.SaveAdvice upsert by (tenantID,storeID,forDate)**：同日重新生成覆盖（不新增行，不重复 item）。内存 repo 并发安全（`-race` 全绿）。
9. **action_type 双层不漂移**：rule 产 ActionType 用 `rule.ReasonToAction` 映射（常量定义在 rule 包 actions.go）；exec **只按 ActionType dispatch，禁看 ReasonType**。
10. **gateway 无 provider（dev 无 key）**：agent 把 `gateway.ErrNoProvider` 等同 `gateway.ErrBudgetExceeded` → rule_only 降级（Degrade=no_provider/budget）。
11. 不引入白名单外依赖；不连真实 DB/Redis/LLM（gateway 可构建空 provider 网关，测试用 fake gateway.Provider）；测试用 fake 接口 + 注入 clock。
12. 每个 `package xx` 上方 `// Package xx ...` 文档注释；`gofmt` 干净；copy ≤40 字过 contentfilter（M2-S2 已落地，output 调用）。

### 12.6 DoD（M3-S1）

- `go build ./... && go vet ./... && gofmt -l .`（空）`&& go test -race ./...` 全绿（预计 ~28 包）。
- strategy 单测：SaveAdvice upsert（同日覆盖不重复）+ 并发安全（`-race`）+ 状态机迁移合法 + 串户 ErrNotFound。
- signal 单测：fake Lookuper 三路 → Bundle 字段映射正确；空 Lookuper → 零 Bundle（冷启动入口）。
- rule 单测：5 业态种子各跑一条命中；毛利护栏用例（cost=1200 cents, price=2580 cents, floor=5% → discount 致 newPrice<1260 被压回下限）；ReasonToAction 映射正确；不达标候选被丢弃。
- agent 单测：fake gateway.Provider 4 场景（好 JSON / 坏 JSON / ErrBudgetExceeded / ErrNoProvider）→ EnhanceOrSkip 永不 error、坏路径回退 Routed-only 模板。
- converge 单测：5 个 EnhancedItem 取 Top3 + 同 priority 稳定排序 + margin_ok=false 排尾不丢。
- output 单测：Encode 落 strategy repo（GeneratedBy 由 degrade 推导）+ ActionType/ReasonType 落 item + copy≤40 字 + Signals 快照 marshal。
- exec 单测：6 ActionType dispatch；**blocked 归因**（fake MessageSender 返 blocked(freq_circuit) → ExecResult.Status=blocked + item 落 ItemBlocked）；margin 破底拦截；price_adj 未 Confirm → blocked(confirm_required)；recharge_promo → blocked(not_executable)。
- pipeline 单测：8s 超时降级 + cache TryHit 短路 + signal 失败 rule_only + 冷启动（0 命中→cold_priority 注入 ≥1 条）。
- 迁移 golang-migrate 命名合法、up/down 对称、CHECK/UNIQUE 齐全（含本期增补的 reason_type/execution_status CHECK）。
- HTTP 冒烟（主线程 m3s1_test.go 8 场景）：①生成 happy ②毛利护栏拦截 ③预算熔断降级 ④LLM 坏 JSON 降级 ⑤冷启动 ⑥一键执行发券触达 ⑦一键执行 blocked 归因 ⑧串户 404。

### 12.7 HTTP 端点 + errcode（主线程）

**errcode 新增（`pkg/errcode/errcode.go`，60017 起；降级非失败走 200+data.degrade）**：
```
ErrStrategyNotFound   = Err{40404, "策略不存在", 404}
ErrMarginGuardExec    = Err{60017, "执行时毛利护栏拦截", 200}
ErrActionBlocked      = Err{60021, "动作被合规拦截，未执行", 200}
ErrActionNotExecutable= Err{60022, "该动作暂不可一键执行", 200}
```
（60018-60020 预留：ErrBudgetDegrade/ErrLLMDegrade/ErrColdStart；本期这些降级通过 `Advice.Degrade` 字段透传，不强制映射 errcode，handler 返回 200+data。）

**HTTP 端点（boss 面，`interfaces/http/handler/aiengine.go`，参照 reach.go）**：
| 方法 | 路径 | handler | 入参 → 出参 |
|---|---|---|---|
| POST | `/v1/boss/aiengine/today` | `Today` | body `{date?}` → `*output.Advice`（200，降级也 200 带 degrade） |
| GET | `/v1/boss/aiengine/strategies` | `ListStrategies` | `?date=&status=` → `[]*strategy.Strategy` |
| GET | `/v1/boss/aiengine/strategies/:id` | `GetStrategy` | → `*strategy.Strategy`（含 items） |
| POST | `/v1/boss/aiengine/strategies/:id/items/:itemID/execute` | `ExecuteItem` | body `{confirm?}` → `*exec.ExecResult`（blocked 也 200） |
| POST | `/v1/boss/aiengine/strategies/:id/items/:itemID/feedback` | `RecordFeedback` | body `{useful}` → `{ok:true}` |

### 12.8 显式延后（不在 M3-S1）

语义缓存 PGVector（AI-06，Cache 接口已留，M3-S2 实现）；凌晨预生成 cron（AI-07，M3-S2 加 pregen 包）；gateway lite/V3/R1 分层路由（AI-08，本期全 lite）；效果归因回填 + bandit（AI-10/12，表建好空，M3-S3）；剧本库后台编辑（AI-11，本期 in-code 种子，M3-S3 接 React 后台）；tool-calling LLM 循环（本期 gateway.Chat 单轮 json_object）；moments 朋友圈实发（M3-S2 依赖海报/COS）；recharge_promo 执行（M4 依赖 billing/反二清）；membership/inventory 的 List 方法补充（signal 会员/临期枚举 M3-S1 best-effort，真实枚举留 M3-S2 与 strategy_cache 同期）。
