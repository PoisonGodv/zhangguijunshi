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

---

## 13. M3-S2 增量契约（AI-06 语义缓存 + AI-07 凌晨预生成 + AI-08 gateway 分层 + AI-09 成本熔断 + membership/inventory List 补充，2026-07-28）

> M3-S2 是「AI 降本」sprint：语义缓存（PGVector）+ 凌晨离线预生成 cron + gateway lite/V3/R1 分层路由 + 成本熔断分层计费 + signal 真实枚举（membership/inventory List）。设计依据 ch07 §7.6-7.8 + ch05 §5.3.9。**本机无 PG/Redis/Docker**（§0 硬约束）：AI-06/07 涉及外部 infra 的部分用**双实现**（Memory dev/test + pgx/redis 生产，pgxmock 测试），生产部署配 PG/Redis/embedding API 时启用。AI-09 在 M3-S1 已基本落地（gateway.Budget + pricing），本期补分层计费 + 全局告警。

### 13.1 范围与裁剪（对照 ch07 §7.6-7.8 / ch00 WBS §M3-S2）

**必做**：AI-06 语义缓存（Embedder 接口 + MemorySemanticCache + PGVectorSemanticCache + migration 000008 ai_cache + 命中率 metrics）；AI-07 凌晨预生成 cron（robfig/cron + BatchGenerate + 增量重算内存 channel + 失败重试）；AI-08 gateway 分层（routes 配置化 lite/V3/R1 + ChatWithFallback 降级链已就绪）；AI-09 成本熔断分层计费（pricing 已按 tier，补全局告警 log）；membership 加 ListMembers + Member.Birthday + migration 000009；inventory 加 ListNearingBatches；signal adapter 真实枚举（替换 M3-S1 best-effort 空）；Pipeline 集成 L2 语义缓存（L1 精确 hash → L2 语义 → 生成）。

**显式延后**：真实 PGVector 检索（需 PG，本期 pgx 实现/pgxmock 测试，生产配 PG 启用）；真实 Redis Stream 增量（本期内存 channel，生产配 Redis）；真实 embedding API（本期 fake/HTTP stub，生产配通义/bge-m3）；AI-10/11/12 归因/bandit/剧本库（M3-S3）；FE-ADMIN-03 归因看板（M3-S3）；pgx repo 替换内存（有 PG 后，全局）。

### 13.2 迁移编号（DB-05）

- `migrations/000008_ai_cache.{up,down}.sql`：`ai_cache` 表（ch07 §7.6.3 权威，注意 ch05 §5.3.9 的 `strategy_cache` 是旧命名，统一用 `ai_cache`）：id(tidentity PK)/tenant_id/business/key_text/embedding vector(1024)/advice_json(jsonb)/created_at/expires_at/hit_count int default 0。`CREATE EXTENSION IF NOT EXISTS vector`（幂等）。HNSW 索引 `ON ai_cache USING hnsw (embedding vector_cosine_ops) WITH (m=16, ef_construction=64)` + btree 索引 `(tenant_id, business, expires_at)`。down 逆序 drop 表 + `DROP EXTENSION IF EXISTS vector`（仅当无其他依赖；保守起见 down 不 drop extension，只 drop 表）。
- `migrations/000009_member_birthday.{up,down}.sql`：`ALTER TABLE members ADD COLUMN birthday DATE`（可空，加性扩展，M3-S2 signal 用）。down `ALTER TABLE members DROP COLUMN birthday`。

### 13.3 领域类型与接口（**已固定，agent 必须一字不差实现签名**）

#### 13.3.1 `internal/membership` 扩展（A1，加性，不改现有契约）

```go
// Member 加 Birthday 字段（M3-S2 signal 用；migration 000009）
type Member struct {
    // ... 现有字段 ...
    Birthday *time.Time // 可空，YYYY-MM-DD
}
// MemberRepo 加 ListMembers（加性，不改现有方法）
type MemberRepo interface {
    // ... 现有方法 ...
    // ListMembers 按 filter 枚举租户内会员（M3-S2 signal 真实枚举；filter 空字段不过滤）。
    ListMembers(ctx context.Context, tenantID int64, f MemberFilter) ([]*Member, error)
}
type MemberFilter struct {
    SleepBefore *time.Time // 30天未到店（LastVisitAt < 此值）；nil 不过滤
    MinPoints   int64      // 高价值最小积分；0 不过滤
    BirthdayOn  *time.Time // 当天生日；nil 不过滤
    Limit       int        // <=0 默认 500
}
// Service 加 ListMembers(ctx, f MemberFilter) ([]*Member, error)（首行 EnforceTenant）
```
> Member 需有 LastVisitAt 字段支撑 SleepBefore。若 M1-S2 Member 无此字段，A1 在 migration 000009 一并加 `last_visit_at TIMESTAMPTZ`（加性）。A1 自查现有 Member struct + migration 000004，按需补字段。

#### 13.3.2 `internal/inventory` 扩展（A1，加性）

```go
// BatchRepo 加 ListNearingBatches（跨品临期枚举，M3-S2 signal 用）
type BatchRepo interface {
    // ... 现有方法 ...
    // ListNearingBatches 返回 store 内所有 active 且 ExpiryDate <= now+withinDays 的批次（跨产品）。
    ListNearingBatches(ctx context.Context, tenantID, storeID int64, withinDays int) ([]*Batch, error)
}
// Service 加 ListNearingBatches(ctx, storeID, withinDays int) ([]*Batch, error)（首行 EnforceTenant）
```

#### 13.3.3 `internal/aiengine/gateway` 扩展（A2，加性，不改 Provider 接口）

```go
// Embedder 是独立的 embedding 接口（不改现有 Provider；M3-S2 语义缓存用）。
// 实现一：gateway 内置 HTTP embedder（通义/bge-m3，OpenAI 兼容 /v1/embeddings）；
// 实现二：主线程 fake embedder（dev/test，固定向量或确定性 hash→[1024]float32）。
type Embedder interface {
    Embed(ctx context.Context, texts []string) ([][]float32, error)
    Dim() int // 1024
}
// NewEmbedderFromConfig(cfg config.AIConfig) Embedder —— 无 API key 时返回 FakeEmbedder（确定性 hash→1024维，dev/test 用）
// gateway.Gateway 不变（routes 已支持 WithRoute；NewFromConfig 从 cfg.AI.Routes 配 routes）
// config.AIConfig 加 Routes map[TaskTier]AIProviderConfig? —— 见 §13.3.7
```

#### 13.3.4 `internal/aiengine/semantic`（A3，新包，import strategy + gateway Embedder）

```go
package semantic

import (
    "context"
    "github.com/zhangguijunshi/backend/internal/aiengine/gateway"
    "github.com/zhangguijunshi/backend/internal/strategy"
)

// SemanticCache 是 L2 语义缓存（M3-S1 的 cache.Cache 是 L1 精确 hash）。
// TryHit 按 signalText 的 embedding 做 PGVector 余弦近邻检索（相似度>0.85 且未过期且同业态）。
type SemanticCache interface {
    TryHit(ctx context.Context, tenantID int64, business, signalText string) (*strategy.Strategy, bool, error)
    Put(ctx context.Context, tenantID int64, business, signalText string, s *strategy.Strategy, ttl time.Duration) error
    // Invalidate 主动失效某 tenant 当日缓存（执行动作后调）
    Invalidate(ctx context.Context, tenantID int64) error
}

// MemorySemanticCache：dev/test 用。fake embedding（确定性 hash→[1024]float32）+
// 内存切片线性扫描（余弦相似度）。无 PG 依赖。命中率埋点。
type MemorySemanticCache struct { /* embedder gateway.Embedder; mu sync.RWMutex; entries []entry; metrics */ }
func NewMemoryCache(emb gateway.Embedder, opts ...Option) *MemorySemanticCache

// PGVectorSemanticCache：生产用。pgx + pgvector-go，余弦距离 <0.15 检索。
// 测试用 pgxmock（不连真实 PG）。主线程在无 PG 时注入 MemorySemanticCache。
type PGVectorSemanticCache struct { /* pool *pgxpool.Pool; emb gateway.Embedder; metrics */ }
func NewPGVectorCache(pool /* pgxpool.Pool 接口 */, emb gateway.Embedder, opts ...Option) *PGVectorSemanticCache

// Metrics 命中率埋点（§7.6.5）：IncHit/IncMiss（按 tenant + global），Prometheus 暴露。
type Metrics interface {
    IncHit(tenantID int64)
    IncMiss(tenantID int64)
}
func NewMetrics() Metrics // 内存计数 + prometheus counter

// SignalText 构造缓存 key 文本（§7.6.2）：{业态 | 信号组合}，如 "fruit|weather=rain|action=clearance|days_to_expiry=2|sleep_30=18|holiday=none"。
// 由 Pipeline 在 TryHit 前构造（或 semantic 包提供 BuildSignalText(bundle) string）。
func BuildSignalText(b /* *signal.Bundle */ any) string
```

#### 13.3.5 `internal/aiengine/pregen`（A4，新包，import pipeline + cache + semantic + strategy）

```go
package pregen

import (
    "context"
    "github.com/robfig/cron/v3"
    "github.com/zhangguijunshi/backend/internal/aiengine"
    "github.com/zhangguijunshi/backend/internal/aiengine/cache"
    "github.com/zhangguijunshi/backend/internal/aiengine/semantic"
)

// Scheduler 凌晨预生成 + 增量重算（§7.7）。
type Scheduler struct {
    pipeline *aiengine.Pipeline
    cache    cache.Cache
    semantic semantic.SemanticCache
    repo     TenantEnumerator // 防腐：枚举活跃租户
    log      *zap.Logger
    cron     *cron.Cron
    incr     chan IncrementalJob // 内存 channel（dev）；生产换 Redis Stream（接口同）
}
type IncrementalJob struct {
    TenantID int64
    Reason   string // inventory_change|weather_change|manual_refresh|holiday_rollover
}
type TenantEnumerator interface {
    ActiveTenantIDs(ctx context.Context) ([]int64, error)
}
// Start 启动 cron：0 3 * * * BatchGenerate（全量）+ */10 * * * * IncrementalRetry（增量重试）。
// BatchGenerate 按 tenant 分批 50，worker pool 8 并发，调 pipeline.Run + cache.Put + semantic.Put（TTL 24h）。
// IncrementalRetry 消费 incr channel，调 pipeline.Run 重算覆盖缓存。
// 失败重试 ≤3（指数退避）；全量批次失败 >20% 告警 + 降级纯规则兜底。
func (s *Scheduler) Start() error
func (s *Scheduler) Stop() // cron.Stop()
func (s *Scheduler) BatchGenerate(ctx context.Context) (int, error) // 返回处理条数
func (s *Scheduler) EnqueueIncremental(job IncrementalJob)           // 非阻塞投递
func NewScheduler(p *aiengine.Pipeline, ch cache.Cache, sem semantic.SemanticCache, repo TenantEnumerator, opts ...Option) *Scheduler
```

#### 13.3.6 `internal/aiengine/cache` L1 不变 + Pipeline 集成 L2（A5）

```go
// cache.Cache（L1 精确 hash）签名不变（M3-S1 §12.3.4）。
// Pipeline 扩展：新增 semantic SemanticCache 字段（可选，nil 时跳过 L2）。
// Pipeline.Run 集成顺序：
//   0a) cache.TryHit（L1 精确 by tenant+date）→ 命中返 cache_hit
//   0b) semantic.TryHit（L2 语义 by signalText embedding）→ 命中返 semantic_hit
//   1-5) 生成 → cache.Put（L1）+ semantic.Put（L2，TTL 24h）
type Pipeline struct { /* 现有字段 + semantic semantic.SemanticCache */ }
func NewPipeline(..., ch cache.Cache, sem semantic.SemanticCache, opts ...Option) *Pipeline
// WithSemanticCache(semantic.SemanticCache) Option（nil 跳过 L2，dev 兼容）
// Degrade 新增值：semantic_hit（advice.degrade 字段透传前端）
```

#### 13.3.7 config AI routes（A2，加性）

```go
// config.AIConfig 加 Routes（lite/V3/R1 → provider 配置）
type AIConfig struct {
    // ... 现有字段 ...
    Routes map[string]AIRouteConfig `mapstructure:"routes"` // key: "lite"|"v3"|"r1"
}
type AIRouteConfig struct {
    Provider string `mapstructure:"provider"` // qwen|deepseek|doubao
    Model    string `mapstructure:"model"`
}
// gateway.NewFromConfig 从 Routes 配 WithRoute(TierLite/V3/R1, Route{...})
// config.dev.yaml 加 ai.routes 节（dev 可空→全 defaultRoute/lite）
```

### 13.4 包所有权（文件集互斥）

| Agent | 只准写这些文件 |
|---|---|
| **M3S2-membership-inventory**（A1） | `migrations/000009_member_birthday.{up,down}.sql`；`internal/membership/{membership.go(加 Birthday+LastVisitAt+MemberFilter+ListMembers 方法签名), memory_repo.go(加 ListMembers 实现), service.go(加 ListMembers), service_test.go(加测试)}`；`internal/inventory/{inventory.go(加 ListNearingBatches 接口签名), memory_repo.go(加实现), service.go(加方法), service_test.go(加测试)}` |
| **M3S2-gateway-config**（A2） | `internal/aiengine/gateway/{embedder.go(Embedder 接口+FakeEmbedder+HTTPEmbedder+NewEmbedderFromConfig), gateway.go(NewFromConfig 从 Routes 配 routes, 加性不改现有), embedder_test.go}`；`pkg/config/types.go(加 AIRouteConfig+Routes 字段)`；`configs/config.dev.yaml(ai.routes 节)` |
| **M3S2-semantic**（A3） | `migrations/000008_ai_cache.{up,down}.sql`；`internal/aiengine/semantic/{cache.go(SemanticCache 接口+Metrics), memory.go(MemorySemanticCache), pgvector.go(PGVectorSemanticCache), signal_text.go(BuildSignalText), service.go, service_test.go}` |
| **M3S2-pregen**（A4） | `internal/aiengine/pregen/{scheduler.go, batch.go, incremental.go, service.go, service_test.go}` |
| **主线程**（A5 集成） | `internal/aiengine/cache/`（不变，只 Pipeline 集成）；`internal/aiengine/pipeline.go`（加 semantic 字段+L2 集成+WithSemanticCache）；`cmd/api/main.go`（实例化 Embedder+SemanticCache[Memory dev]+Scheduler+signalLookuperAdapter 真实枚举）；`cmd/api/aiengine_adapters.go`（signalLookuperAdapter 改真实枚举+tenantEnumeratorAdapter）；`interfaces/http/m3s2_test.go`；`go.mod`（加 pgvector-go + cron 依赖） |

> **波次**：Wave 0（本 §13）→ Wave 1（A1‖A2，无跨 import）→ Wave 2（A3，依赖 A2 Embedder）→ Wave 3（A4‖A5，A4 依赖 A3；A5 依赖 A1/A3）→ 主线程集成。A1 改 membership/inventory 是**加性扩展**（不改现有方法签名），不破坏 M2 契约。

### 13.5 硬约束（M3-S2 全员）

1. 同 M0-M3-S1（tenant 只从 ctx/int64 分/接口+内存兜底/跨租户 ErrNotFound）。
2. **加性扩展**：membership/inventory/gateway 的现有方法签名**不动**，只新增方法 + 字段。现有测试必须继续通过。
3. **双实现**：semantic.SemanticCache 有 Memory + PGVector 两实现。主线程无 PG 时注入 Memory（dev/test）。PGVector 实现用 pgxmock 测试（不连真实 PG）。
4. **Embedder 独立接口**（不破坏 gateway.Provider）：semantic 包定义 Embedder 需求，主线程注入 gateway.Embedder（fake/HTTP）。
5. **新依赖白名单**（§3 加）：`github.com/pgvector/pgvector-go v0.3.0`、`github.com/robfig/cron/v3 v3.0.1`。其他不引。
6. **embedding 维度固定 1024**（bge-m3/text-embedding-v3）。FakeEmbedder 返确定性 hash→[1024]float32（同文本同向量，可命中）。
7. **语义缓存阈值**：余弦距离 <0.15（相似度 >0.85），同业态，未过期（TTL 24h）。Memory 实现用线性扫描余弦相似度。
8. **预生成不阻断**：cron 失败/超时降级纯规则兜底，保证白天有建议。BatchGenerate 单户失败 ≤3 重试。
9. **成本熔断**：M3-S1 已有 gateway.Budget（MonthlyLimitCNY=2.0），M3-S2 补全局告警（log + pkg/notify 若接入）。pricing 已按 tier（lite/V3/R1 不同价）。
10. package 注释 + gofmt 干净 + `-race` 全绿。

### 13.6 DoD（M3-S2）

- `go build/vet/gofmt/test -race ./...` 全绿（预计 ~38 包）。
- membership 单测：ListMembers（SleepBefore/MinPoints/BirthdayOn 过滤）+ Birthday 字段 + 串户。
- inventory 单测：ListNearingBatches（跨品临期，withinDays 过滤）+ 串户。
- gateway 单测：Embedder（fake 确定性 + HTTP stub）+ NewFromConfig 从 Routes 配 lite/V3/R1 + ChatWithFallback 降级链。
- semantic 单测：MemoryCache（fake embedding，同 signalText 命中/相似命中/不相似 miss/TTL 过期/Invalidate）+ PGVectorCache（pgxmock 验证 SQL + 参数）+ Metrics（IncHit/IncMiss）+ BuildSignalText。
- pregen 单测：Scheduler（fake pipeline+cache+semantic+TenantEnumerator）BatchGenerate 分批 + 增量 channel + 失败重试 ≤3。
- HTTP 冒烟（主线程 m3s2_test.go）：①语义缓存命中（同 signalText 二次 /today → degrade=semantic_hit，pipeline 不重算）②预生成 cron（手动触发 BatchGenerate → cache 填充 → /today 命中）③gateway 分层降级（fake provider lite 返错→V3 成功）④真实信号（membership/inventory List → rule 命中真实建议）⑤串户。

### 13.7 配置 + 依赖（主线程）

- `go.mod` 加 `github.com/pgvector/pgvector-go v0.3.0` + `github.com/robfig/cron/v3 v3.0.1`（`go mod tidy`）。
- `config.dev.yaml` 加 `ai.routes` 节（dev 空→全 defaultRoute/lite）。
- main.go：`emb := gateway.NewEmbedderFromConfig(cfg.AI)`（dev→FakeEmbedder）；`semCache := semantic.NewMemoryCache(emb)`（dev，无 PG）；`pipeline := aiengine.NewPipeline(..., cacheL1, semCache, aiengine.WithSemanticCache(semCache))`；`scheduler := pregen.NewScheduler(pipeline, cacheL1, semCache, tenantEnumeratorAdapter{...})` + `scheduler.Start()`（goroutine）；signalLookuperAdapter 改真实枚举（用 membership.ListMembers + inventory.ListNearingBatches）。
- HTTP：不新增端点（M3-S1 的 /aiengine/today 等不变，degrade 新增 semantic_hit 值）。

### 13.8 显式延后（不在 M3-S2）

真实 PGVector 检索（需 PG，本期 pgx 实现+pgxmock 测试，生产配 PG 启用 PGVectorCache）；真实 Redis Stream 增量重算（本期内存 channel，生产配 Redis Stream）；真实 embedding API（本期 FakeEmbedder，生产配通义 text-embedding-v3/bge-m3）；AI-10/11/12 归因/bandit/剧本库（M3-S3）；FE-ADMIN-03 归因看板（M3-S3）；pgx repo 全局替换内存（有 PG 后）；白天增量重算的真实触发器接入（inventory/weather 事件 → pregen.EnqueueIncremental，本期接口留好，触发器接 M3-S3）。

## 14. M3-S3 增量契约（AI-10 效果归因回填 + AI-11 冷启动剧本库 + AI-12 规则权重 bandit + FE-ADMIN-03 归因看板读 API，2026-07-28）

### 14.1 范围与裁剪（对照 ch07 §7.9-7.10 / ch00 WBS §M3-S3）

M3-S3 是「自学习 + 冷启动」sprint。三个**全新** Go 包，互斥，互不 import（无环）：

- **AI-10 attribution**（`internal/aiengine/attribution/`）：策略执行效果归因回填。把 strategy_items 执行后 T+24h/T+72h 的代理指标（券核销率/到店反推/拼团成团率）写入 **已存在**的 `strategy_attribution` 表（migration 000007，本期建表空，本 sprint 回填）。提供归一化 reward 给 bandit，提供读 API 给看板。
- **AI-11 playbook**（`internal/aiengine/playbook/`）：5 业态冷启动剧本库（fruit/convenience/maternity/fresh/apparel）。每业态一份 `Playbook`（默认规则集 + 文案模板 + 社群 SOP + 损耗率/现金系数 + ColdPriority 冷启动优先动作）。新店首日无销售数据时只推 ColdPriority。
- **AI-12 bandit**（`internal/aiengine/bandit/`）：epsilon-greedy 规则权重自学习。`Arm/Select/Update/CreditItem`，消费 attribution 的 reward + strategy_feedback 反哺，回写 `ai_rule_weight` 表（**本期新增** migration 000010）。

**裁剪（对照 ch07）**：
- ch07 §7.9.1 提议的 `ai_advice_outcome` 宽表**不建**——migration 000007 的 `strategy_attribution`（metric/value/measured_at EAV 形，5 metric CHECK）是权威，本期回填它。
- 5 metric 中 **coupon_redeem / visit_uplift / groupbuy_success 本期真实计算**；**repurchase / moments_engage 无数据源，本期不产出**（Compute 不 emit，看板显示「—」），显式延后。
- reward 归一化权重：ch07 `0.5*coupon+0.3*visit+0.2*revenue` → 本期 `0.7*coupon_rate + 0.3*visit_delta_norm`（revenue 延后），clip [0,1]。
- bandit arm 粒度：以 **reason_type（7 值）为 arm key**（cold-start-safe，每业态必命中），非 rule.ID（触发稀疏）。`ItemResolver.Resolve` 返回 `item.ReasonType` 作 ruleID。
- playbook 剧本**本期 in-code seed**（`NewStore()` 硬编码 5 业态）；DB `ai_rule` 表 + 后台 React 可视化编辑延后（M4）。
- FE-ADMIN-03 本期交付**后端读 API**（`GET /v1/boss/aiengine/strategies/:id/attribution`）+ handler；admin-web React 看板本身延后（M4，admin-web 目录仍空）。

### 14.2 迁移编号（DB-06）

新增 `migrations/000010_ai_rule_weight.{up,down}.sql`（**bandit agent 拥有**，其它 agent 不动 migrations/）：

```sql
-- 000010_ai_rule_weight.up.sql
-- 规则权重 bandit 自学习持久化 (M3-S3, AI-12). 依据 ch07 §7.9.2.
-- arm key = reason_type (7 值) 或 rule.ID; tenant_id=0 留作平台默认(本期不用).
CREATE TABLE ai_rule_weight (
    id          BIGSERIAL    PRIMARY KEY,
    tenant_id   BIGINT       NOT NULL REFERENCES tenants(id),
    rule_id     TEXT         NOT NULL,                       -- reason_type (7 值) 或 rule.ID
    weight      DOUBLE PRECISION NOT NULL DEFAULT 0.5,       -- 软更新后的 Q 值, 初值 0.5 中性先验
    trials      INT          NOT NULL DEFAULT 0,
    reward_sum  DOUBLE PRECISION NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, rule_id)
);
CREATE INDEX idx_ai_rule_weight_tenant ON ai_rule_weight(tenant_id);
COMMENT ON TABLE  ai_rule_weight              IS '规则权重 bandit 自学习(M3-S3); arm=reason_type; UNIQUE(tenant_id,rule_id)';
COMMENT ON COLUMN ai_rule_weight.weight      IS '软更新 Q 值, 初值 0.5 中性先验(未见规则 cold-start-safe)';
-- down: DROP TABLE ai_rule_weight;
```
挂 `trg_set_updated_at`（bandit 每次更新 weight → 刷新 updated_at，沿用 000001-000009 触发器约定）。`strategy_attribution` 表**不动**（000007 已建）。

### 14.3 领域类型与接口（**已固定，agent 必须一字不差实现签名**）

#### 14.3.0 包依赖 DAG（**无环，跨包只走声明接口**）
```
rule        → (无 internal 依赖)
strategy    → (无 internal 依赖)
playbook    → rule
attribution → strategy
bandit      → (无 internal 依赖; RewardSource/ItemResolver 自声明, 由主线程接线)
pipeline/pregen/handler → rule + playbook + attribution + bandit + strategy (主线程集成)
```
**禁止**：attribution 不 import bandit；bandit 不 import attribution（reward 走 bandit 自声明的 `RewardSource` 接口，attribution.Computer 满足之）；playbook 不 import strategy/attribution/bandit。

#### 14.3.1 attribution 包（AI-10）
```go
package attribution // import github.com/zhangguijunshi/backend/internal/aiengine/attribution

import (
    "context"
    "time"
    "github.com/zhangguijunshi/backend/internal/strategy"
)

// metric 常量 — 一一对应 strategy_attribution.metric CHECK (migration 000007).
const (
    MetricCouponRedeem    = "coupon_redeem"     // 本期真实计算
    MetricVisitUplift     = "visit_uplift"      // 本期真实计算
    MetricGroupbuySuccess = "groupbuy_success"  // 本期真实计算(按 strategy_id 归因, §14.9)
    MetricRepurchase      = "repurchase"        // 本期 best-effort 产出(coupon 类 item; 触达会员在 [executedAt,measuredAt] 内复购率, §14.9)
    MetricMomentsEngage   = "moments_engage"    // 本期不产出(无企微互动数据源, 延后 M4)
)

// Measurement 一条 (item, metric, value, measured_at) 事实, 1:1 映射 strategy_attribution.
type Measurement struct {
    ID             int64     `json:"id"`
    TenantID       int64     `json:"tenant_id"`
    StrategyItemID int64     `json:"strategy_item_id"`
    Metric         string    `json:"metric"`
    Value          float64   `json:"value"`
    MeasuredAt     time.Time `json:"measured_at"`
}

type MeasurementFilter struct {
    ItemIDs []int64  // 空=不限
    Metrics []string // 空=不限
    Limit   int      // <=0 默认 50
}

var (
    ErrParam    = errors.New("attribution: invalid param")
    ErrNotFound = errors.New("attribution: not found")
)

// Repo 持久化 Measurement (strategy_attribution 表). Memory dev/test + pgx 生产(pgxmock 测).
// 跨租户记录视为不存在. Record 的 UNIQUE(tenant,item,metric,measured_at) 由实现层保证 upsert.
type Repo interface {
    Record(ctx context.Context, tenantID, itemID int64, metric string, value float64, measuredAt time.Time) (*Measurement, error)
    List(ctx context.Context, tenantID int64, f MeasurementFilter) ([]Measurement, error)
    LatestPerMetric(ctx context.Context, tenantID, itemID int64) (map[string]float64, error) // 缺失 metric 省略(非 0)
}
type MemoryRepo struct{ /* ... */ }
func NewMemoryRepo() *MemoryRepo
type PGRepo struct{ /* 窄 Pool 接口: QueryRow/Exec, 同 semantic.PGVectorSemanticCache 范式 */ }
func NewPGRepo(pool PGPool) *PGRepo
type PGPool interface {
    QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
    Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// CouponStats 反腐端口 → marketing. 返回 strategyID 下发出/已核销券数.
type CouponStats interface {
    SentRedeemed(ctx context.Context, tenantID, strategyID int64) (sent, redeemed int, err error)
}
// VisitCounter 反腐端口 → billing. SinceCount=store 自 t 起支付笔数; BaselineCount=同时段历史均值.
type VisitCounter interface {
    SinceCount(ctx context.Context, tenantID, storeID int64, since time.Time) (int, error)
    BaselineCount(ctx context.Context, tenantID, storeID int64, window time.Duration, before time.Time) (float64, error)
}
// GroupbuyStats 反腐端口 → groupbuy. 返回 strategyID 关联的发起/成团数(无 linkage 返 0,0,nil → 不 emit).
type GroupbuyStats interface {
    StartedSucceeded(ctx context.Context, tenantID, strategyID int64) (started, succeeded int, err error)
}
// RepurchaseCounter 反腐端口 → marketing+billing. 返回 strategyID 触达会员在 [executedAt, measuredAt] 的复购率
// (去重会员中 ≥1 笔已付订单占比, clip [0,1]); 触达集空/上游 err → 0,nil → 不 emit. §14.9.
type RepurchaseCounter interface {
    Rate(ctx context.Context, tenantID, strategyID int64, executedAt, measuredAt time.Time) (float64, error)
}
type Sources struct {
    Coupons    CouponStats
    Visits     VisitCounter
    Groupbuy   GroupbuyStats
    Repurchase RepurchaseCounter
}

// Computer 由一项已执行 strategy_item + 其 store 产出 Measurement 事实. 拥有 metric 语义与 reward 归一化.
type Computer struct { /* Sources + WithNowFunc */ }
func NewComputer(src Sources, opts ...Option) *Computer
type Option func(*Computer) // WithNowFunc(func() time.Time)

// Compute 在 measuredAt 评估 item, 返回可算 metric 的事实(无可算→空切片, 不报错).
//   coupon_redeem    = redeemed/sent (sent==0 不 emit)
//   visit_uplift     = SinceCount(executedAt) − BaselineCount(window=measuredAt−executedAt, before=executedAt)
//   groupbuy_success = succeeded/started (started==0 不 emit)
// tenantID 显式传(沿用 strategy memory_repo 约定); item 只读.
func (c *Computer) Compute(ctx context.Context, tenantID int64, item *strategy.StrategyItem, storeID int64, executedAt, measuredAt time.Time) ([]Measurement, error)

// Reward 返回 item 的归一化 [0,1] reward, 供 bandit 消费.
// reward = 0.7*coupon_rate + 0.3*visit_delta_norm, clip [0,1]. 无 coupon 数据→coupon_rate=0.
// 满足 bandit.RewardSource.Reward(ctx, tenantID, itemID, strategyID, storeID) → (float64, error).
func (c *Computer) Reward(ctx context.Context, tenantID, itemID, strategyID, storeID int64) (float64, error)
```
**注意**：coupon/visit/groupbuy 三端口在 Compute 里**按 item 的 action_type 选择性调用**——非 coupon 类 item 不查 CouponStats（避免无意义查询）。reward 计算复用 Compute 的 coupon/visit 结果（不重复查库）；itemID→item 的解析由主线程接线时给 Computer 注入一个 `ItemLoader`（**或** Reward 接收已解析的 item，二选一，agent 选 Reward(ctx,...,storeID) 签名为主、内部用 strategy.Service.GetItem 解析；主线程把 strategy.Service 适配成 Computer 的 item 来源）。**为避免环依赖，Computer 不 import strategy.Service**；item 来源走 Computer 自声明的窄接口：
```go
type ItemLoader interface { GetItem(ctx context.Context, tenantID, itemID int64) (*strategy.StrategyItem, error) }
```
`NewComputer(src Sources, loader ItemLoader, opts ...Option)` —— **签名以此为准**（loader 必传，主线程用 strategy.Service 适配）。Compute 的 item 参数仍由调用方传入（backfill job 已持 item）；Reward 内部用 loader 自解析。

#### 14.3.2 playbook 包（AI-11）
```go
package playbook // import github.com/zhangguijunshi/backend/internal/aiengine/playbook

import "github.com/zhangguijunshi/backend/internal/aiengine/rule"

// 5 业态常量 — 与 rule.Business 取值一致.
const (
    Fruit       = "fruit"
    Convenience = "convenience"
    Maternity   = "maternity"
    Fresh       = "fresh"
    Apparel     = "apparel"
)

// SOPStage 社群四阶段之一(拉群/促活/转化/裂变, ch07 §7.10.1).
type SOPStage struct {
    Name string `json:"name"`
    Play string `json:"play"`
}

// Playbook 一个业态的冷启动运营手册(ch07 §7.10.1).
type Playbook struct {
    Business     string            `json:"business"`
    Rules        []rule.Rule       `json:"rules"`         // 该业态默认规则集(可含 DefaultSeeds 子集 + 业态特化)
    Prompts      map[string]string `json:"prompts"`        // action_type → 文案模板
    SOP          [4]SOPStage       `json:"sop"`            // 拉群/促活/转化/裂变
    LossRate     float64           `json:"loss_rate"`      // 损耗率(fruit≈0.10, convenience≈0.015)
    CashCoef     float64           `json:"cash_coef"`      // 现金系数(客流反推, 默认 1.2)
    ColdPriority []string          `json:"cold_priority"`  // 冷启动优先 action_type(不依赖销售反推)
}

// IsColdPriority 报告 actionType 是否在 ColdPriority 中.
func (p *Playbook) IsColdPriority(actionType string) bool

// Store 持有 5 业态剧本库(本期 in-code seed; DB/后台延后).
type Store struct{ /* ... */ }
func NewStore() *Store                  // 种入 5 业态
func (s *Store) Get(business string) (*Playbook, bool)
func (s *Store) List() []*Playbook

// ColdStartFilter 冷启动期过滤: 移除非 ColdPriority 的 item. 若过滤后为空(全非冷启 或 pb 无 ColdPriority),
// 原样返回(老板永远 ≥1 条, 计划书 6.6 / CONTRACT §12.3.8). 纯函数, 不写库.
func ColdStartFilter(items []rule.RoutedItem, pb *Playbook) []rule.RoutedItem
```
**5 业态填充**（ch07 §7.10.2 表，损耗率/现金系数为待核实默认值）：fruit(LossRate 0.10/CashCoef 1.2, ColdPriority: moments_post/group_seckill/groupbuy), convenience(0.015/1.2, festival→coupon_wake/moments_post), maternity(0.02/1.2, birthday_care→coupon_wake/recharge_promo), fresh(0.10/1.2, rain→groupbuy/group_seckill), apparel(0.03/1.3, festival→coupon_wake/groupbuy)。Rules 至少含每业态 2 条（可复用 rule.DefaultSeeds 同 ID 规则 + Prompts 文案模板，模板变量用 `{product}/{price}/{member_tag}` 占位）。

#### 14.3.3 bandit 包（AI-12）
```go
package bandit // import github.com/zhangguijunshi/backend/internal/aiengine/bandit

import "context"

// Arm 一条规则的习得权重(ch07 §7.9.2). rule_id = reason_type(7 值, cold-start-safe).
type Arm struct {
    RuleID    string  `json:"rule_id"`
    Weight    float64 `json:"weight"`      // 软更新后 Q 值, 初值 0.5
    Trials    int     `json:"trials"`
    RewardSum float64 `json:"reward_sum"`
}

// Config epsilon-greedy 超参(ch07 §7.11.1).
type Config struct {
    Epsilon float64 // 探索率, 默认 0.1
    LR      float64 // 学习率, 默认 0.1
}
func DefaultConfig() Config // {0.1, 0.1}

// RewardSource 反腐端口 → attribution. 返回 item 归一化 [0,1] reward.
type RewardSource interface {
    Reward(ctx context.Context, tenantID, itemID, strategyID, storeID int64) (float64, error)
}
// ItemResolver 反腐端口 → strategy. 返回 item 的 (ruleID=ReasonType, strategyID, storeID).
type ItemResolver interface {
    Resolve(ctx context.Context, tenantID, itemID int64) (ruleID string, strategyID int64, storeID int64, err error)
}

// Repo 持久化 Arm (ai_rule_weight 表, migration 000010). Memory dev/test + pgx 生产(pgxmock 测).
// 未见规则 Get 返零值 Arm{Weight:0.5}(中性先验, cold-start-safe). 跨租户=不存在.
type Repo interface {
    Get(ctx context.Context, tenantID int64, ruleID string) (*Arm, error)
    Put(ctx context.Context, tenantID int64, arm *Arm) error
    All(ctx context.Context, tenantID int64) ([]Arm, error)
}
type MemoryRepo struct{ /* map + sync.Mutex, -race 安全 */ }
func NewMemoryRepo() *MemoryRepo
type PGRepo struct{ /* 窄 Pool, 同 attribution.PGPool 范式 */ }
func NewPGRepo(pool PGPool) *PGRepo
type PGPool interface { // bandit 自声明, 与 attribution.PGPool 同构(主线程可共用一个 pgxpool)
    QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
    Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Learner epsilon-greedy 权重学习器(ch07 §7.9.2).
type Learner struct{ /* repo + cfg + rand */ }
func NewLearner(repo Repo, cfg Config, opts ...Option) *Learner
type Option func(*Learner) // WithRand(func() float64) 注入确定性随机源(测试用), 默认 rand.Float64

// Select 1−epsilon 选 Weight 最高 arm, epsilon 随机探索. arms 空→nil.
func (l *Learner) Select(ctx context.Context, tenantID int64, arms []Arm) (*Arm, error)
// Update 应用一次 reward 到 ruleID(增量均值+软更新). 未见 arm 先 seed Weight=0.5. reward clip [0,1].
func (l *Learner) Update(ctx context.Context, tenantID int64, ruleID string, reward float64) (*Arm, error)
// CreditItem 一站式反馈: Resolve item → 读 Reward → Update arm. pregen bandit-update 步 + feedback 反哺共用.
func (l *Learner) CreditItem(ctx context.Context, tenantID, itemID int64) (*Arm, error)
```
**rand 注入**：`WithRand(func() float64)` 让 Select/Update 的探索可确定性测试（项目禁全局态，见 §4）。Reward clip 用 `math.Max(0, math.Min(1, r))`。

### 14.4 包所有权（文件集互斥，每个 agent 拥有互斥的**包**）

| agent | 拥有（**只准写这些文件**） | 禁动 |
|---|---|---|
| **A1 attribution** | `internal/aiengine/attribution/*.go`（含 `*_test.go`） | 其它一切；migrations/ |
| **A2 playbook** | `internal/aiengine/playbook/*.go`（含 `*_test.go`） | 其它一切；migrations/ |
| **A3 bandit** | `internal/aiengine/bandit/*.go`（含 `*_test.go`）**+** `migrations/000010_ai_rule_weight.up.sql` + `migrations/000010_ai_rule_weight.down.sql` | 其它包的 .go |
| **主线程** | `CONTRACT.md` §14 / `internal/aiengine/pipeline.go` / `internal/aiengine/pregen/scheduler.go`(+`batch.go`/`incremental.go` 如需) / `cmd/api/main.go` / `cmd/api/aiengine_adapters.go` / `interfaces/http/router.go` / `interfaces/http/handler/aiengine.go` / `interfaces/http/m3s3_test.go` / `go.mod`(如需) / `configs/config.dev.yaml`(如需) | 三 agent 的包内文件 |

**跨包编译约束**：agent 本地只跑 `go test ./internal/aiengine/<本包>/...`（**不要** `go build ./...`——集成前整体必编译失败，那是主线程收尾职责）。

### 14.5 硬约束（M3-S3 全员）

1. **签名一字不差**：§14.3 的类型/方法签名是冻结的，主线程集成按此接线。返回 error 不 panic。
2. **多租户行级隔离**：所有 Repo 方法第一业务参 `tenantID int64`，跨租户=不存在（ErrNotFound/零值）。agent 测试须含跨租户用例（assert 返空/零值）。
3. **反腐端口**：attribution/bandit/playbook **只 import 声明的窄接口 + rule/strategy**，不直 import marketing/billing/groupbuy/membership（端口由主线程适配）。无环（§14.3.0）。
4. **双实现**：三包 Repo 均 Memory（dev/test）+ PG（pgxmock 测，生产配 PG）。pgxmock 用 `github.com/pashagolub/pgxmock/v4`（白名单 §3）。
5. **-race 全绿**：Memory 实现用 `sync.Mutex` 保护 map；测试 `go test -race ./internal/aiengine/<本包>/...` 必须通过。
6. **确定性测试**：bandit 用 `WithRand` 注入确定性随机源；attribution `Reward`/`Compute` 用固定 `measuredAt`/`executedAt` 输入。禁 `time.Now()`（用 `WithNowFunc` 或显式参）。
7. **gofmt + go vet 干净**；包注释 `// Package xx ...`（§4 约定 7）。
8. **reward clip [0,1]**；arm Weight 初值 0.5（cold-start-safe，未见规则不 panic）。
9. **不引白名单外依赖**（§3）。三包均只需 stdlib + pgx/pgconn/pgxmock（PG repo）+ rule/strategy（internal）。`go.mod` 预期**无需新增**（pgxmock/pgx 已在）。

### 14.6 DoD（M3-S3 各 agent）

- **A1 attribution**：① `MemoryRepo` Record/List/LatestPerMetric + 跨租户用例；② `PGRepo` pgxmock 测（INSERT ... ON CONFLICT upsert + SELECT LatestPerMetric）；③ `Computer.Compute` coupon/visit/groupbuy 三 metric 计算（mock 端口）+ sent==0/started==0 不 emit + 跨租户；④ `Computer.Reward` 归一化 + clip + 满足 bandit.RewardSource；⑤ `-race` 全绿。**至少 18 测试**。
- **A2 playbook**：① `NewStore` 种入 5 业态，Get/List 齐 + 未知业态 bool false；② 每 `Playbook` 字段齐（Rules≥2/Prompts/SOP[4]/LossRate/CashCoef/ColdPriority≥2）；③ `IsColdPriority`；④ `ColdStartFilter` 过滤非冷启 + 过滤后为空原样返回 + pb nil 安全；⑤ `-race` 全绿。**至少 12 测试**。
- **A3 bandit**：① `MemoryRepo` Get(未见→0.5)/Put/All + 跨租户；② `PGRepo` pgxmock 测；③ `Select` 利用(最高 Weight)+探索(WithRand 注入 epsilon 阈值上下)+空→nil；④ `Update` 增量均值+软更新+clip+未见先 seed；⑤ `CreditItem` Resolve→Reward→Update 一站式(mock 端口)；⑥ migration 000010 up/down SQL 合法 + 挂 trg_set_updated_at；⑦ `-race` 全绿。**至少 16 测试**。

### 14.7 集成 + 配置（主线程，workflow 收尾后）

- **pipeline.go**：① 冷启动——`Run` 内 rule.Route 后，若 `len(routed.Items)==0` 或 data_quality<阈值，用 `playbook.Store.Get(business)` 取剧本，`ColdStartFilter` 注入剧本 ColdPriority 占位 item（替换/增强现有 `injectColdStart`，保留 degrade="cold_start"）；② bandit 加权——route 前，用 `bandit.Learner`/Repo 读当 tenant 7 reason_type arm Weight，对 seeds 按 reason_type 加 `Weight*priorityBoost` 偏置（best-effort，无 arm 时原 priority）。`NewPipeline` 加 Option `WithPlaybookStore(*playbook.Store)` + `WithBanditLearner(*bandit.Learner)`，**向后兼容**（nil 时走原 injectColdStart，现有 m3s1/m3s2 测试不破）。
- **pregen/scheduler.go**：新增两条 cron——① `0 4 * * *` attribution 回填（`AttributionBackfill`：扫 executed≥24h 且无 attribution 的 items → Computer.Compute → Repo.Record；72h 二次回填）；② `0 2 * * *` bandit-update（`BanditUpdate`：扫昨日 executed items → Learner.CreditItem → Repo.Put）。`NewScheduler` 加参 `attrComputer *attribution.Computer` + `banditLearner *bandit.Learner`（窄接口/可 nil，向后兼容）。`Scheduler` 暴露 `AttributionBackfill(ctx)` + `BanditUpdate(ctx)` 供 m3s3_test 手动触发（同 BatchGenerate 范式）。
- **strategy_feedback 反哺**：`RecordFeedback` 后异步 `banditLearner.CreditItem`（useful=true→reward+1 boost，best-effort，失败仅日志）。或由 `BanditUpdate` cron 统一吃 feedback——**主线程选 cron 统一吃**（避免改 strategy.Service 签名）。
- **HTTP**：`interfaces/http/handler/aiengine.go` 加 `GetAttribution(c *gin.Context)`（`GET /v1/boss/aiengine/strategies/:id/attribution` → 拉 strategy.items → attribution.Repo.LatestPerMetric 聚合 → 返回 `{items:[{rank,action_type,metrics:{coupon_redeem:..,visit_uplift:..,groupbuy_success:..},revenue_cny:..}]}`，缺失 metric 显示 null）。router.go 挂载（deps 注入 attribution Repo/Computer）。
- **main.go**：构造 `attrRepo := attribution.NewMemoryRepo()`（dev，无 PG）+ `attrComputer := attribution.NewComputer(src, strategySvc适配ItemLoader)` + `banditRepo := bandit.NewMemoryRepo()` + `banditLearner := bandit.NewLearner(banditRepo, bandit.DefaultConfig())` + `playbookStore := playbook.NewStore()`；接线 `pipeline`（WithPlaybookStore/WithBanditLearner）+ `scheduler`（新参）+ `handler`（attribution Repo）。src 的 CouponStats/VisitCounter/GroupbuyStats 端口由主线程写适配器（marketing/billing/groupbuy Service → 端口），**dev 可 nil**（Computer 对 nil 端口安全跳过该 metric）。
- **HTTP 冒烟（主线程 `interfaces/http/m3s3_test.go`）**：① 冷启动（新店/空 signal → playbook ColdPriority 注入 ≥1 条，degrade=cold_start）；② 归因回填（执行 coupon_wake item → 造 marketing 券核销数据 → 手动触发 scheduler.AttributionBackfill → GET attribution 见 coupon_redeem）；③ bandit 更新（手动触发 scheduler.BanditUpdate → ai_rule_weight 权重变化 → 路由偏置变化）；④ 看板读 API（GET attribution 返回聚合 JSON）；⑤ 串户（跨 tenant GET attribution → 空/404）。

### 14.8 显式延后（不在 M3-S3）

bandit arm 粒度细化到 rule.ID（本期 reason_type 7 arm）；playbook DB 化（`ai_rule` 表 + 后台 React 可视化编辑，admin-web 仍空，M4）；FE-ADMIN-03 admin-web React 看板（本期仅后端读 API，M4）；strategy_feedback 实时反哺（本期 cron 统一吃，实时异步 M4）；pgx repo 全局替换内存（有 PG 后）；真实 PG/Redis 接入（生产部署前）；moments_engage metric（无企微互动数据源，需朋友圈互动 API 接线 + exec.execMomentsPost 实发，M4）。

**M3-S3 收尾交付（见 §14.9）**：groupbuy→strategy linkage（本期 migration 000011 加 `groupbuys.strategy_id` 列 + `groupbuyStatsAdapter` 实装 `StatsByStrategy` → `groupbuy_success` 按 `strategy_id` 真实 emit）；revenue_cny best-effort 估算（看板 handler 调 `marketing.MembersByStrategy` + `billing.OrderTotalByMembers`，归因窗口 `[item.ExecutedAt, now]`，缺失/未执行/null 端口 → null）；repurchase metric best-effort 产出（`RepurchaseCounter` 端口，coupon 类 item 触达会员复购率）。三项从本期延后项中移出，落入"已交付 best-effort"。

### 14.9 M3-S3 收尾（2026-07-28）

M3-S3 收尾三项遗留已交付（集成核心 + 主线程收尾）：

1. **groupbuy → strategy linkage（A1 域）**：migration `000011_groupbuy_strategy` 给 `groupbuys` 表加 `strategy_id BIGINT NOT NULL DEFAULT 0`（0=非AI来源，与 `reach_tasks.strategy_id` nullable 语义对齐：groupbuy 无 NULL 归因场景，0 即未归因）+ `idx_groupbuys_strategy(tenant_id, strategy_id)` 索引。`GroupbuyRepo.ListByStrategy(ctx, tenantID, strategyID)` 按 (tenantID, strategyID) 过滤返回全状态 groupbuy（strategyID<=0 → 空 slice）。`Service.StatsByStrategy(ctx, strategyID)` 内部 `EnforceTenant` 取租户，返 (started, succeeded)；`groupbuyStatsAdapter.StartedSucceeded` 代理到 `StatsByStrategy`（`withTenantCtx` 对齐 ctx 租户与显式 tenantID），故 `groupbuy_success` 归因 metric 按 `strategy_id` 真实计算（不再 no-op）。归因 key 是 **strategy_id**（非 strategy_item_id）—— 端口签名冻结在 `strategy_id` 粒度（§14.3.1 `GroupbuyStats.StartedSucceeded(ctx, tenantID, strategyID)`）。
2. **billing → revenue/repurchase 反腐能力（A2 域）**：`OrderRepo.OrderTotalByMembers(ctx, tenantID, memberIDs, since, until)` 返 `total_cents`（仅 paid、按 `PaidAt` 闭区间、跨租户排除、空 memberIDs/逆序区间 → 0,nil）；`OrderRepo.RepurchaseMembers(...)` 返去重复购人数（map 去重）。`Service.OrderTotalByMembers/RepurchaseMembers` 先 `EnforceTenant` 再调 repo，无 tenant → error（不 panic）。billing 域不 import marketing/attribution（DAG 干净，纯被动按会员汇总）。
3. **marketing → MembersByStrategy 归因 feed（A3 域）**：`Service.MembersByStrategy(ctx, tenantID, strategyID)` 返去重（first-seen wins，保 ListByStrategy 的 ID-desc 序）的触达会员 id 列表；strategyID<=0 → nil,nil；跨租户（ctx tenant ≠ 显式 tenantID）→ nil,nil；无 ctx tenant → error（不 panic 不返空）。复用 `ListByStrategy(ctx, strategyID, "")`。
4. **集成核心（aiengine_adapters.go + computer.go + attribution.go）**：`attribution.Computer.Sources` 加 `Repurchase RepurchaseCounter` 端口（`Rate(ctx, tenantID, strategyID, executedAt, measuredAt)(float64,error)`）；`Compute` 在 `groupbuy_success` 后对 coupon 类 item 且端口非 nil 时调 `Rate`，err==nil → append `Measurement{Metric:MetricRepurchase, Value: clip01(rate)}`。`repurchaseCounterAdapter` 组合 `marketing.MembersByStrategy` + `billing.RepurchaseMembers`，全链路 degrade（nil mkt/bill / strategyID<=0 / 空会员集 / 上游 err → 0,nil，不 panic）。`groupbuyOpenerAdapter.Launch` 第8参 `_ int64` → `strategyID int64`，`CreateInput` 字面量写 `StrategyID: strategyID`（归因链路通了）。
5. **revenue_cny 看板 best-effort（主线程 handler）**：`GetAttribution` 对每个 item 在 `mkt/bill` 均 wired、`it.StrategyID>0`、`it.ExecutedAt != nil` 时，调 `marketing.MembersByStrategy(mctx, tenantID, strategyID)` 拿触达会员，再 `billing.OrderTotalByMembers(mctx, memberIDs, *ExecutedAt, now)` 拿已付订单 total_cents，`revenue_cny = float64(totalCents)/100.0`；任一未满足/err → `revenue_cny` 字段省略（JSON null）。`mctx` 用 `multitenancy.WithContext` 注入 system-sourced 租户，对齐两个反腐端口内部 `EnforceTenant`。handler 字段加 `mkt *marketing.Service` + `bill *billing.Service`，`NewAIEngineHandler` 第5/6参挂载；router.go `Deps.Marketing/Billing` 透传（Deps 字段无新增）；main.go 调用处补 `marketingSvc`/`billingSvc`。

**moments_engage 仍延后**：本期不产出（无企微朋友圈互动 API 数据源；`exec.execMomentsPost` 当前只 audit 不实发）。归因 `Computer` 不 emit `MetricMomentsEngage`，看板 `revenue_cny` 同款 best-effort 逻辑对其不适用（无触达会员集），看板字段 null。CONTRACT §14.3.1 `MetricMomentsEngage` 注释仍标"本期不产出(无企微互动数据源, 延后 M4)"。延后项见 §14.8。

**best-effort 语义**：repurchase/revenue_cny/groupbuy_success 三项均为 best-effort —— 端口未 wired（dev 最小 fixture）、strategyID<=0、无触达会员、无已付订单、上游 error 任一发生 → 对应 metric/字段省略（JSON null），不 panic、不串租、不误导看板（boss 见"—"）。归因窗口口径：repurchase = `[executedAt, measuredAt]`（Computer 调用方 pregene cron 传入）；revenue_cny = `[item.ExecutedAt, now]`（handler 用请求时刻 now）。真实 PG/Redis 接入与 pgx repo 的 `ListByStrategy/OrderTotalByMembers/RepurchaseMembers` 实装为 M4 落地项（本期 Memory 双实现）。

---

## 15. M4-S1 增量契约（WX-07 微信支付服务商 + BE-A-10 支付回调幂等 + BE-B-12 储值充值反二清 + BE-B-13 订阅付费墙 + DB-06，2026-07-28）

M4-S1 是「商业化」sprint。**两个全新 Go 包**（`internal/payment`、`internal/subscription`，当前为空 .gitkeep）+ **两个已有包加性扩展**（`internal/billing`、`internal/membership`），互斥，跨包只走声明接口（无环）。本机无 wx appid/无支付商户号/无 PG → 沿用「接口 + 内存实现 + Fake/mock 测试」原则：微信支付 V3 走自声明反腐端口（dev FakeWxPay 确定性实现），签名/验签是确定性纯函数（真实可测），HTTP 调用降级。`go build ./... && go test -race ./...` 全绿不连任何真实外部依赖。

### 15.1 范围与裁剪（对照 ch05 §5.3.3 / §5.3.7 / ch03 §3.x / ch00 WBS §M4-S1）

- **payment（WX-07 + BE-A-10 + DB-06 payment 部分）**：特约商户进件（sub_merchants）+ 支付单（payments）+ 支付流水（payment_ledger，反二清审计）+ 微信支付 V3 三反腐端口（进件 Apply / 统一下单 Prepay / 回调验签 ParseNotify）+ FakeWxPay（dev）+ V3 签名/验签纯函数。回调幂等（UNIQUE 交易号 + 状态机 pending→success 不可逆 + 重复回调不重入账不重履约）。
- **subscription（BE-B-13 + DB-06 subscription 部分）**：订阅三层（free/pro/flagship）功能矩阵 + 付费墙门禁（Can/Require）+ 订阅状态（subscriptions 表）。tenants.plan 字段已存（000001）但全代码无读取，本期 subscription 自带 SubscriptionStore（内存 dev + subscriptions 表生产），主线程种默认 free。
- **billing 扩展（BE-A-10 订单状态推进）**：Order 状态机加 pending（现仅创建 paid）+ CreatePendingOrder + MarkOrderPaid（pending→paid 幂等推进，关联 PaymentRef）。**加性扩展，现有 CreateOrder/测试不破**。
- **membership 扩展（BE-B-12 储值充值 + 地区合规）**：Recharge（反二清记账：余额入账 + recharge 流水）+ BalanceAccount 加 RegionCapCents 字段 + 地区单卡余额上限校验（超额→errcode.ErrBalanceOver 60003）+ rechargeAtomic repo 原子方法 + 储值档位规则（充200送20/充500送80，in-code seed）。**加性扩展，现有 Join/Redeem/测试不破**。

**裁剪（对照 ch03/ch05/ch06）**：
- ch05 §5.3.3 `payment_ledger` 原设计**按月分区**（PARTITION BY RANGE created_at）+ UNIQUE 含 created_at → **本期不分区**（单机/dev），UNIQUE 简化为 `(tenant_id, provider, provider_tx_id)`（同一微信交易号全局唯一，幂等核心）。分区延后生产部署。
- ch05 `reconciliation_diffs`（对账差异表）**本期不建**（M4-S2 BE-A-11 供应商/外卖对账）。
- 支付宝 ISV（WX-01 支付宝侧）**本期不实现**（provider=alipay 留接口位，FakeWxPay 只实装 wxpay；alipay 进件/下单延后 M4-S2/生产）。
- 微信支付 V3 **真实 HTTP 调用降级**：`SubMerchantApplier/Preparer/NotifyParser` 是接口，FakeWxPay 给确定性返回（dev/测试）；真实 V3 实现（调 api.mch.weixin.qq.com）可写但本机不连，签名/验签纯函数真实可测。平台证书下载/轮换延后生产。
- 储值**资金流与入账分离**（反二清）：充值 = payment 创建 recharge 支付单（资金经微信直清特约商户 sub_merchant，**不沉淀平台**）→ 回调 success → membership.Recharge 入账（BalanceAccount += amount+gift）。平台不持有资金，payment_ledger 仅审计记账。
- 储值赠送（gift）**由 membership 内部档位规则算**（RechargeTier：amount→gift 映射 in-code seed），payment/Fulfiller 只传 amountCents。计划书 §7.4「充200送20、充500送80送鸡蛋」。
- 订阅价格**本期 in-code 映射**（pro=3900 月/flagship=9900 月，待核实计划书 §8.1 L2=39元月）；微信支付订阅收款走 payment(Subject=subscription)→ConfirmPayment 激活。
- 「不做交易抽成」（计划书 §8.1）→ payment 域**无平台抽佣逻辑**，仅服务商模式（特约商户进件 + 微信支付分润由微信侧结算，平台不碰资金）。
- WX-01 微信支付服务商**资质申请**（行政）不在本期；本期代码就绪，资质到位后配置 mchid/apikey 即可上线。

### 15.2 迁移编号（DB-06）

WBS §M4-S1 写的「000007 payment / 000004 membership」编号**已过期**（000007=ai_strategy、000004=membership 已用）。本期新增 **000012 + 000013**：

- **A1 payment 拥有** `migrations/000012_payment_submerchant.{up,down}.sql`（sub_merchants + payments + payment_ledger 三表）：
```sql
-- 000012_payment_submerchant.up.sql
-- 支付服务商进件 + 支付单 + 支付流水(反二清审计) (M4-S1, WX-07/BE-A-10). 依据 ch05 §5.3.3/§5.3.7.
-- 本期不分区(payment_ledger 生产部署时按 created_at 月分区, 见 §15.8).

-- 特约商户(子商户)进件记录
CREATE TABLE sub_merchants (
    id           BIGSERIAL    PRIMARY KEY,
    tenant_id    BIGINT       NOT NULL REFERENCES tenants(id),
    store_id     BIGINT       NOT NULL REFERENCES stores(id),
    provider     TEXT         NOT NULL,                         -- wxpay|alipay
    sub_mch_id   TEXT,                                          -- 进件成功后的特约商户号
    applyment_id TEXT,                                          -- 微信申请单号
    status       TEXT         NOT NULL DEFAULT 'pending'        -- pending|reviewing|active|rejected|frozen
                 CHECK (status IN ('pending','reviewing','active','rejected','frozen')),
    rate_bps     INT,                                            -- 费率基点 38=0.38%
    materials    JSONB,                                          -- 进件材料快照(脱敏)
    applied_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    approved_at  TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uniq_submch_provider_mchid ON sub_merchants(tenant_id, provider, sub_mch_id) WHERE sub_mch_id IS NOT NULL;
CREATE INDEX idx_submch_tenant_status ON sub_merchants(tenant_id, status);
COMMENT ON TABLE sub_merchants IS '特约商户进件(M4-S1); 服务商模式批量进件; sub_mch_id NULL=未进件成功';

-- 支付单(业务侧; 状态机 pending→success)
CREATE TABLE payments (
    id             BIGSERIAL    PRIMARY KEY,
    tenant_id      BIGINT       NOT NULL REFERENCES tenants(id),
    store_id       BIGINT       NOT NULL REFERENCES stores(id),
    sub_merchant_id BIGINT      NOT NULL,                        -- 清算到的特约商户(反二清)
    out_trade_no   TEXT         NOT NULL,                        -- 商户订单号(业务侧生成, UNIQUE)
    provider       TEXT         NOT NULL,                        -- wxpay|alipay
    provider_tx_id TEXT,                                         -- 微信/支付宝交易号(回调填, 对账键)
    subject        TEXT         NOT NULL,                        -- order|recharge|subscription (履约分发)
    ref_id         BIGINT,                                       -- 按 subject 解释: orderID/memberID/0(subscription)
    amount_cents   BIGINT       NOT NULL,
    type           TEXT         NOT NULL DEFAULT 'pay'           -- pay|refund
                 CHECK (type IN ('pay','refund')),
    status         TEXT         NOT NULL DEFAULT 'pending'       -- pending|success|failed|refunded
                 CHECK (status IN ('pending','success','failed','refunded')),
    paid_at        TIMESTAMPTZ,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, out_trade_no)
);
CREATE INDEX idx_payments_tenant_status ON payments(tenant_id, status);
CREATE INDEX idx_payments_txid ON payments(tenant_id, provider, provider_tx_id) WHERE provider_tx_id IS NOT NULL;
COMMENT ON COLUMN payments.subject IS '履约分发键: order→billing.MarkOrderPaid, recharge→membership.Recharge, subscription→subscription.ConfirmPayment';

-- 支付流水(反二清审计; 禁软删; UNIQUE 交易号=回调幂等核心)
CREATE TABLE payment_ledger (
    id             BIGSERIAL    PRIMARY KEY,
    tenant_id      BIGINT       NOT NULL REFERENCES tenants(id),
    store_id       BIGINT       NOT NULL REFERENCES stores(id),
    sub_merchant_id BIGINT      NOT NULL,
    order_id       BIGINT,                                       -- 可 0(非订单支付)
    provider       TEXT         NOT NULL,
    provider_tx_id TEXT,
    amount_cents   BIGINT       NOT NULL,
    fee_cents      BIGINT       NOT NULL DEFAULT 0,              -- 手续费
    type           TEXT         NOT NULL CHECK (type IN ('pay','refund')),
    status         TEXT         NOT NULL CHECK (status IN ('success','pending','failed','refunded')),
    raw_payload    JSONB,                                        -- 回调原始报文(审计)
    paid_at        TIMESTAMPTZ,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, provider, provider_tx_id)                 -- 同一交易号全局唯一=幂等
);
CREATE INDEX idx_ledger_tenant_created ON payment_ledger(tenant_id, created_at);
COMMENT ON TABLE payment_ledger IS '支付流水(M4-S1); 反二清: 仅审计记账, 资金经微信直清特约商户, 不沉淀平台';
-- trg_set_updated_at 挂 sub_merchants/payments (有 updated_at); payment_ledger 无 updated_at 不挂。
-- down: DROP TABLE payment_ledger; DROP TABLE payments; DROP TABLE sub_merchants;
```

- **A2 subscription 拥有** `migrations/000013_subscription.{up,down}.sql`（subscriptions 订阅流水 + 当前态）：
```sql
-- 000013_subscription.up.sql
-- 订阅状态与流水(M4-S1, BE-B-13). tenants.plan(000001)是冗余快照, 本表是权威流水.
CREATE TABLE subscriptions (
    id              BIGSERIAL    PRIMARY KEY,
    tenant_id       BIGINT       NOT NULL REFERENCES tenants(id),
    plan            TEXT         NOT NULL                        -- free|pro|flagship
                    CHECK (plan IN ('free','pro','flagship')),
    period_started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    period_ends_at  TIMESTAMPTZ,                                  -- NULL=永久(仅 free)
    status          TEXT         NOT NULL DEFAULT 'active'       -- active|expired|cancelled
                    CHECK (status IN ('active','expired','cancelled')),
    source          TEXT         NOT NULL DEFAULT 'manual'       -- manual|wxpay|alipay|admin
                    CHECK (source IN ('manual','wxpay','alipay','admin')),
    payment_ref     TEXT,                                         -- 关联支付单 out_trade_no(可选)
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now()
);
CREATE INDEX idx_subscriptions_tenant ON subscriptions(tenant_id, status);
-- 每租户至多一条 active 订阅(切换 plan 时旧记录置 cancelled)
CREATE UNIQUE INDEX uniq_subscriptions_active_one ON subscriptions(tenant_id) WHERE status='active';
COMMENT ON TABLE subscriptions IS '订阅流水(M4-S1); 三层蛋糕 L1 free/L2 pro 39元月/L3 flagship; UNIQUE active 保证单租户单有效订阅';
-- 挂 trg_set_updated_at; down: DROP TABLE subscriptions;
```

### 15.3 领域类型与接口（**已固定，agent 必须一字不差实现签名**）

#### 15.3.0 包依赖 DAG（**无环，跨包只走声明接口**）
```
payment       → (无 internal 依赖; SubMerchantApplier/Preparer/NotifyParser/Fulfiller 自声明, 主线程适配)
subscription  → (无 internal 依赖; 自带 SubscriptionStore 内存实现)
billing       → (无新 internal 依赖; 现有 + MarkOrderPaid 加性扩展, 不 import payment)
membership    → (无新 internal 依赖; 现有 + Recharge 加性扩展, 不 import payment)
handler/main  → payment + subscription + billing + membership (主线程集成, 写 Fulfiller/adapter)
```
**禁止**：payment 不 import billing/membership/subscription（履约走自声明 `Fulfiller` 端口）；billing/membership 不 import payment（支付单号作 int64/string 参数传入）；subscription 不 import payment。四包两两不互 import（除 stdlib）。

#### 15.3.1 payment 包（A1，新包，零 internal 依赖）
```go
package payment // import github.com/zhangguijunshi/backend/internal/payment

import (
    "context"
    "errors"
    "time"
)

// provider 常量
const (
    ProviderWxPay  = "wxpay"
    ProviderAlipay = "alipay"
)

// SubMerchantStatus 特约商户进件状态 (sub_merchants.status).
type SubMerchantStatus string
const (
    SubMerchantPending   SubMerchantStatus = "pending"
    SubMerchantReviewing SubMerchantStatus = "reviewing"
    SubMerchantActive    SubMerchantStatus = "active"
    SubMerchantRejected  SubMerchantStatus = "rejected"
    SubMerchantFrozen    SubMerchantStatus = "frozen"
)

// SubMerchant 特约商户进件记录 (sub_merchants 表).
type SubMerchant struct {
    ID, TenantID, StoreID int64
    Provider              string             // wxpay|alipay
    SubMchID              string             // 进件成功后的特约商户号(空=未成功)
    ApplymentID           string             // 微信申请单号
    Status                SubMerchantStatus
    RateBPS               int32              // 费率基点 38=0.38%
    Materials             map[string]any     // 进件材料快照(脱敏, JSONB)
    AppliedAt             time.Time
    ApprovedAt            *time.Time
    UpdatedAt             time.Time
}

// PaymentStatus 支付单状态机.
type PaymentStatus string
const (
    PaymentStatusPending  PaymentStatus = "pending"
    PaymentStatusSuccess  PaymentStatus = "success"
    PaymentStatusFailed   PaymentStatus = "failed"
    PaymentStatusRefunded PaymentStatus = "refunded"
)

// PaymentType.
type PaymentType string
const (
    PaymentTypePay    PaymentType = "pay"
    PaymentTypeRefund PaymentType = "refund"
)

// subject 履约分发键 (payments.subject). 决定回调成功后调哪个 Fulfiller.
const (
    SubjectOrder       = "order"        // → billing.MarkOrderPaid(RefID=orderID)
    SubjectRecharge    = "recharge"     // → membership.Recharge(RefID=memberID, AmountCents=实付)
    SubjectSubscription = "subscription" // → subscription.ConfirmPayment(RefID 忽略, tenantID 从 ctx)
)

// Payment 一笔支付单 (payments 表).
type Payment struct {
    ID, TenantID, StoreID, SubMerchantID int64
    OutTradeNo   string        // 商户订单号(业务侧生成, UNIQUE per tenant)
    Provider     string        // wxpay|alipay
    ProviderTxID string        // 微信交易号(回调填, 对账键)
    Subject      string        // order|recharge|subscription
    RefID        int64         // 按 Subject 解释
    AmountCents  int64
    Type         PaymentType
    Status       PaymentStatus
    PaidAt       *time.Time
    CreatedAt    time.Time
    UpdatedAt    time.Time
}

// PaymentLedger 支付流水 (payment_ledger 表, 反二清审计, 禁软删).
type PaymentLedger struct {
    ID, TenantID, StoreID, SubMerchantID int64
    OrderID      int64         // 可 0
    Provider     string
    ProviderTxID string        // UNIQUE(tenant,provider,tx_id) 幂等核心
    AmountCents  int64
    FeeCents     int64
    Type         PaymentType
    Status       string        // success|pending|failed|refunded
    RawPayload   []byte        // 回调原始报文(审计, JSONB 序列化)
    PaidAt       *time.Time
    CreatedAt    time.Time
}

// ===== 反腐端口：微信支付 V3 服务商 SDK (主线程适配; dev FakeWxPay) =====

// ApplySubMerchantInput 进件请求参数.
type ApplySubMerchantInput struct {
    TenantID, StoreID int64
    Provider          string // wxpay
    BusinessName      string
    LicenseNo         string // 营业执照号
    LicenseType       string // individual|enterprise
    ContactName       string
    ContactPhone      string
    RateBPS           int32
}

// SubMerchantApplier 进件端口 → 微信支付 V3 applyment. 真实实现调 api.mch.weixin.qq.com;
// FakeWxPay 返回确定性 applyment_id + 即时 reviewing 状态(dev/测试).
type SubMerchantApplier interface {
    Apply(ctx context.Context, in ApplySubMerchantInput) (applymentID string, err error)
    Query(ctx context.Context, tenantID int64, applymentID string) (status SubMerchantStatus, subMchID string, err error)
}

// PrepayInput 统一下单请求.
type PrepayInput struct {
    TenantID, StoreID, SubMerchantID int64
    OutTradeNo  string
    Subject     string
    RefID       int64
    AmountCents int64
    OpenID      string // 付款方 openid (JSAPI 小程序支付; 空=Native 扫码)
    Description string
}

// PrepayResult 统一下单返回 (JSAPI 调起支付参数).
type PrepayResult struct {
    PrepayID  string
    TimeStamp string
    NonceStr  string
    Package   string // "prepay_id=xxx"
    PaySign   string
    SignType  string // RSA
}

// Preparer 统一下单端口 → 微信支付 V3 prepay. FakeWxPay 返回确定性 PrepayResult.
type Preparer interface {
    Prepay(ctx context.Context, in PrepayInput) (PrepayResult, error)
}

// NotifyResult 回调验签后的标准化结果.
type NotifyResult struct {
    OutTradeNo   string
    ProviderTxID string
    AmountCents  int64
    Status       PaymentStatus // success|failed
    PaidAt       time.Time
    RawPayload   []byte
}

// NotifyParser 回调验签端口 → 微信支付 V3 notify. 验签失败返 ErrSignInvalid.
// FakeWxPay 从 body JSON 反序列化(不验签); 真实实现 V3 验签+AES-GCM 解密.
type NotifyParser interface {
    ParseNotify(ctx context.Context, headers map[string]string, body []byte) (NotifyResult, error)
}

// Fulfiller 履约端口 (支付 pending→success 后回调业务侧; 主线程写复合 adapter 按 Subject 分发).
// 仅在状态真推进时调用(重复回调不重履约). nil=不履约(dev/test 安全).
type Fulfiller interface {
    Fulfill(ctx context.Context, p *Payment) error
}

// 哨兵错误. 跨租户=ErrNotFound(ARCH-01).
var (
    ErrNotFound            = errors.New("payment: not found")
    ErrSignInvalid         = errors.New("payment: notify signature invalid")
    ErrDuplicateOutTradeNo = errors.New("payment: duplicate out_trade_no")
    ErrSubMerchantNotActive = errors.New("payment: sub-merchant not active")
    ErrStatusConflict      = errors.New("payment: status conflict") // 非 pending 推进 success
)

// ===== Repo (各 Memory + PG pgxmock) =====

type SubMerchantRepo interface {
    Create(ctx context.Context, sm *SubMerchant) (*SubMerchant, error)
    GetByID(ctx context.Context, tenantID, id int64) (*SubMerchant, error)
    GetActiveByStore(ctx context.Context, tenantID, storeID int64, provider string) (*SubMerchant, error) // 返 status=active 的; 无→ErrNotFound
    UpdateStatus(ctx context.Context, tenantID, id int64, status SubMerchantStatus, subMchID string, approvedAt time.Time) error
    ListByTenant(ctx context.Context, tenantID int64) ([]*SubMerchant, error)
}
type MemorySubMerchantRepo struct{ /* map + sync.Mutex, -race 安全 */ }
func NewMemorySubMerchantRepo() *MemorySubMerchantRepo

type PaymentRepo interface {
    Create(ctx context.Context, p *Payment) (*Payment, error)
    GetByID(ctx context.Context, tenantID, id int64) (*Payment, error)
    GetByOutTradeNo(ctx context.Context, tenantID int64, outTradeNo string) (*Payment, error)
    // MarkSuccess 幂等推进 pending→success:
    //   pending → success, 填 ProviderTxID/PaidAt/UpdatedAt, 返 (p, changed=true, nil)
    //   success → no-op, 返 (p, changed=false, nil)  ← 重复回调幂等, 不重入账
    //   其它    → (nil, false, ErrStatusConflict)
    MarkSuccess(ctx context.Context, tenantID, id int64, providerTxID string, paidAt time.Time) (p *Payment, changed bool, err error)
    UpdateStatus(ctx context.Context, tenantID, id int64, status PaymentStatus) error
}
type MemoryPaymentRepo struct{ /* ... */ }
func NewMemoryPaymentRepo() *MemoryPaymentRepo

type LedgerRepo interface {
    // Append 幂等写流水: UNIQUE(tenant,provider,provider_tx_id) 已存在→返已存记录(nil error);
    // provider_tx_id 空→直接写(不去重). 返写入(或已存)的记录.
    Append(ctx context.Context, l *PaymentLedger) (*PaymentLedger, error)
    ListByTenant(ctx context.Context, tenantID int64, limit int) ([]*PaymentLedger, error)
}
type MemoryLedgerRepo struct{ /* ... */ }
func NewMemoryLedgerRepo() *MemoryLedgerRepo

// PGRepo 各自窄 Pool 接口 (同 attribution.PGPool 范式, 主线程共用一个 pgxpool).
// pgxmock 测 ON CONFLICT upsert + SELECT. type-assert 到 queryer 读多行(同 bandit.PGRepo).
type PGPool interface {
    QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
    Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}
type PGSubMerchantRepo struct{ pool PGPool }
func NewPGSubMerchantRepo(pool PGPool) *PGSubMerchantRepo
type PGPaymentRepo struct{ pool PGPool }
func NewPGPaymentRepo(pool PGPool) *PGPaymentRepo
type PGLedgerRepo struct{ pool PGPool }
func NewPGLedgerRepo(pool PGPool) *PGLedgerRepo

// ===== Service =====
type Service struct { /* smRepo, payRepo, ledgerRepo, applier, preparer, parser, fulfiller, now, outTradeNoFunc */ }
func NewService(smRepo SubMerchantRepo, payRepo PaymentRepo, ledgerRepo LedgerRepo, opts ...Option) *Service
type Option func(*Service)
func WithApplier(a SubMerchantApplier) Option
func WithPreparer(p Preparer) Option
func WithNotifyParser(p NotifyParser) Option
func WithFulfiller(f Fulfiller) Option
func WithNowFunc(f func() time.Time) Option
func WithOutTradeNoFunc(f func(tenantID int64, now time.Time) string) Option

// ApplySubMerchant 提交特约商户进件. status=reviewing(Fake 即时)/pending(真实异步). 写库 + 调 applier.
func (s *Service) ApplySubMerchant(ctx context.Context, in ApplySubMerchantInput) (*SubMerchant, error)
// QuerySubMerchant 查进件状态回写(成功→active+sub_mch_id+approved_at). applier nil→仅读库.
func (s *Service) QuerySubMerchant(ctx context.Context, id int64) (*SubMerchant, error)
// ListSubMerchants 列 ctx 租户特约商户.
func (s *Service) ListSubMerchants(ctx context.Context) ([]*SubMerchant, error)

// Prepay 建支付单(pending)+ 调统一下单. 需 store 有 active 特约商户(provider 匹配).
// outTradeNo 唯一(同 outTradeNo 重复→ErrDuplicateOutTradeNo). 返 (payment, prepayResult).
func (s *Service) Prepay(ctx context.Context, in PrepayInput) (*Payment, *PrepayResult, error)
// GetPayment 查支付单.
func (s *Service) GetPayment(ctx context.Context, id int64) (*Payment, error)

// HandleNotify 回调入口(幂等核心):
//  1. parser.ParseNotify 验签(失败→ErrSignInvalid, 不动库)
//  2. GetByOutTradeNo 找 payment(未找到→ErrNotFound; 跨租户=不存在)
//  3. ledgerRepo.Append 幂等写流水(UNIQUE tx_id 去重)
//  4. payRepo.MarkSuccess 推进 pending→success, 拿 changed 标志
//  5. changed==true 且 fulfiller!=nil → fulfiller.Fulfill(payment) 履约(失败仅日志 best-effort, 不回滚 payment)
// 重复回调: 步3 Append 返已存, 步4 changed=false → 不重履约. 返最终 payment.
func (s *Service) HandleNotify(ctx context.Context, headers map[string]string, body []byte) (*Payment, error)

// ListLedger 读流水(boss 对账看板用).
func (s *Service) ListLedger(ctx context.Context, limit int) ([]*PaymentLedger, error)
```
**V3 签名纯函数**（agent 同包内 `sign.go`，真实可测，不连网）：
```go
// SignV3Authorization 构造微信支付 V3 请求签名 Authorization 头 (APIv3).
// 真实算法: SHA256withRSA(method\nurl\ntimestamp\nnonce\nbody\n) → base64, header "WECHATPAY2-SHA256-RSA2048".
// 本期实现: 输入确定性 → 输出确定性(用注入私钥 *rsa.PrivateKey); 无私钥→返空串+err(不 panic).
func SignV3Authorization(method, url, body, nonce string, ts int64, mchID, serial string, priv *rsa.PrivateKey) (string, error)
// VerifyV3Notify 验签回调: 用微信平台证书公钥验 Authorization 头 + 时间戳偏移±5min.
// 验签是确定性纯函数(注入 *rsa.PublicKey). 失败返 ErrSignInvalid.
func VerifyV3Notify(headers map[string]string, body []byte, pub *rsa.PublicKey) error
```
**注意**：SignV3/VerifyV3 是 stdlib `crypto/rsa + crypto/sha256 + crypto/x509` 实现，**不引第三方微信 SDK**（白名单 §3 不含）。FakeWxPay 的 ParseNotify 不调 VerifyV3（直接 JSON 反序列化），真实 NotifyParser 实现调 VerifyV3。两者同包，agent 都写。

#### 15.3.2 subscription 包（A2，新包，零 internal 依赖）
```go
package subscription // import github.com/zhangguijunshi/backend/internal/subscription

import (
    "context"
    "errors"
    "time"
)

// plan 常量 (与 tenants.plan CHECK 一致).
const (
    PlanFree     = "free"
    PlanPro      = "pro"      // L2 营销版 39元/月或365元/年
    PlanFlagship = "flagship" // L3 旗舰版
)

// 功能常量 (FeatureGate 检查 key; 计划书 §8.1 三层蛋糕功能矩阵).
const (
    FeatureAIAdvice       = "ai_advice"        // 今日3件事 (pro+)
    FeatureMarketingAuto  = "marketing_auto"   // 自动化营销 (pro+)
    FeatureGroupbuy       = "groupbuy"         // 拼团裂变 (pro+)
    FeatureGroupSOP       = "group_sop"        // 社群SOP (pro+)
    FeatureAttributionBI  = "attribution_bi"   // 归因看板 (flagship)
    FeatureMultiStore     = "multi_store"      // 多店 (flagship)
    FeatureAdvancedReport = "advanced_report"  // 高级报表 (flagship)
)

// PlanLevel plan 等级 free=0/pro=1/flagship=2, 用于 >= 比较. 未知→0.
func PlanLevel(plan string) int

// PlanPriceCents plan 月价(分). pro=3900, flagship=9900, free=0 (in-code, 待核实计划书 §8.1).
func PlanPriceCents(plan string) int64

// Subscription 租户订阅状态 (subscriptions 表).
type Subscription struct {
    ID, TenantID          int64
    Plan                  string        // free|pro|flagship
    PeriodStartedAt       time.Time
    PeriodEndsAt          *time.Time    // nil=永久(仅 free)
    Status                string        // active|expired|cancelled
    Source                string        // manual|wxpay|alipay|admin
    PaymentRef            string        // 关联支付单 out_trade_no
    CreatedAt, UpdatedAt  time.Time
}

// ErrPlanUpgradeRequired 功能门禁不通过(需升级 plan).
var ErrPlanUpgradeRequired = errors.New("subscription: plan upgrade required")

// featureMatrix feature→最低 PlanLevel (free 默认满足不列).
var featureMatrix = map[string]int{
    FeatureAIAdvice:       PlanLevel(PlanPro),
    FeatureMarketingAuto:  PlanLevel(PlanPro),
    FeatureGroupbuy:       PlanLevel(PlanPro),
    FeatureGroupSOP:       PlanLevel(PlanPro),
    FeatureAttributionBI:  PlanLevel(PlanFlagship),
    FeatureMultiStore:     PlanLevel(PlanFlagship),
    FeatureAdvancedReport: PlanLevel(PlanFlagship),
}

// SubscriptionStore 持久化订阅状态. Memory dev/test + PG 生产.
type SubscriptionStore interface {
    // CurrentPlan 返租户当前 active 订阅 plan (无记录/expired → free).
    CurrentPlan(ctx context.Context, tenantID int64) (string, error)
    // Get 返当前 active 订阅记录 (无→nil, 不报错).
    Get(ctx context.Context, tenantID int64) (*Subscription, error)
    // Upsert 激活/续期: 旧 active 置 cancelled, 新建 active 记录. 返新建记录.
    Upsert(ctx context.Context, sub *Subscription) (*Subscription, error)
    // ListHistory 订阅流水(全部状态).
    ListHistory(ctx context.Context, tenantID int64) ([]*Subscription, error)
}
type MemoryStore struct{ /* map + sync.Mutex, -race 安全 */ }
func NewMemoryStore() *MemoryStore

// FeatureGate 功能门禁 (业务侧注入检查; 纯逻辑读 store).
type FeatureGate struct { /* store */ }
func NewFeatureGate(store SubscriptionStore) *FeatureGate
// Can 报告 ctx 租户当前 plan 是否允许 feature. 未知 feature→false. 读 store 失败→false+err.
func (g *FeatureGate) Can(ctx context.Context, feature string) (bool, error)
// Require 检查, 不允许→ErrPlanUpgradeRequired.
func (g *FeatureGate) Require(ctx context.Context, feature string) error

// Service 订阅管理.
type Service struct { /* store, gate, now */ }
func NewService(store SubscriptionStore, opts ...Option) *Service
type Option func(*Service) // WithNowFunc
// CurrentPlan ctx 租户当前 plan.
func (s *Service) CurrentPlan(ctx context.Context) (string, error)
// GetSubscription ctx 租户当前订阅记录.
func (s *Service) GetSubscription(ctx context.Context) (*Subscription, error)
// Gate 暴露 FeatureGate 给业务侧(handler/中间件注入检查).
func (s *Service) Gate() *FeatureGate
// Subscribe 切换/续期 plan (manual/admin). periodMonths>0 算 PeriodEndsAt; plan=free→PeriodEndsAt=nil.
func (s *Service) Subscribe(ctx context.Context, in SubscribeInput) (*Subscription, error)
// ConfirmPayment 支付成功激活订阅 (Fulfiller 适配口): 按 amountCents 映射 plan, source=provider, paymentRef 记录.
// amountCents 不匹配任一 PlanPriceCents→ErrParam. 返新建 active 订阅.
func (s *Service) ConfirmPayment(ctx context.Context, tenantID int64, amountCents int64, provider, paymentRef string) (*Subscription, error)

type SubscribeInput struct {
    Plan          string
    PeriodMonths  int    // >0 续期; <=0 用默认(pro/flagship=1月, free=永久)
    Source        string // manual|admin
    PaymentRef    string
}
```

#### 15.3.3 billing 扩展（A3，加性扩展，不 import payment）
```go
// 在 billing.Order struct 加字段 (不破坏现有字段顺序):
//   PaymentRef string `json:"payment_ref,omitempty"` // 关联支付单 out_trade_no (pending→paid 推进时填; 现金单空)

// 新增状态常量 (与 orderStatusPaid 并列):
const (
    orderStatusPending = "pending"
    // orderStatusPaid 已有; refunded/voided 仅 schema, 后续 sprint.
)

// OrderRepo 加方法 (现有方法签名不动, 现有测试全绿):
type OrderRepo interface {
    // ... 现有 6 方法一字不动 ...

    // UpdateStatus 推进订单状态 + 填支付字段(pending→paid 用).
    //   tenantID 注入(ARCH-01); 跨租户订单→ErrNotFound.
    //   status=paid 时填 paidCents/paidAt/paymentRef; 其它状态 paid* 传零值不改.
    //   返更新后的订单(含 Items).
    UpdateStatus(ctx context.Context, tenantID, id int64, status string, paidCents int64, paidAt *time.Time, paymentRef string) (*Order, error)
}

// Service 加方法 (现有方法不动):
// CreatePendingOrder 建待支付订单: total=Σqty*price, paid=0, status=pending, PaymentRef 空.
//   复用 CreateOrder 的计价/快照/stock 逻辑(stock 在 pending 阶段不扣, MarkOrderPaid 时扣 ——
//   或简化: pending 不扣 stock, paid 推进时 best-effort 扣, 同 CreateOrder 范式; agent 选其一, 注释说明).
func (s *Service) CreatePendingOrder(ctx context.Context, in CreateOrderInput) (*Order, error)

// MarkOrderPaid 推进 pending→paid, 填 PaidCents=TotalCents, PaidAt, PaymentRef.
//   pending → paid, 返 (order, nil)
//   paid    → 幂等 no-op, 返 (current, nil)  (重复回调安全)
//   refunded/voided/未知 → (nil, ErrStatusConflict)
//   跨租户/不存在 → (nil, ErrNotFound)
func (s *Service) MarkOrderPaid(ctx context.Context, orderID int64, paymentRef string, paidAt time.Time) (*Order, error)

// 新增哨兵 (与 ErrNotFound 并列):
var ErrStatusConflict = errors.New("billing: order status conflict")
```
**注意**：billing 包 A3 agent 拥有整个包，可改 `billing.go`/`service.go`/`memory_repo.go`/`service_test.go`。**现有 CreateOrder/GetOrder/DailySummary/OrderTotalByMembers/RepurchaseMembers 签名与行为一字不动**，现有测试全绿。仅加 Order.PaymentRef 字段 + orderStatusPending 常量 + OrderRepo.UpdateStatus + Service.CreatePendingOrder/MarkOrderPaid + ErrStatusConflict。

#### 15.3.4 membership 扩展（A4，加性扩展，不 import payment）
```go
// 在 membership.BalanceAccount struct 加字段 (ch05 §5.3.4 member_balance_accounts.region_cap_cents):
//   RegionCapCents int64 `json:"region_cap_cents"` // 地区单卡余额上限(0=不限); 上海/江苏等

// 储值档位规则 (in-code seed, 计划书 §7.4 "充200送20、充500送80"): amount→gift.
var rechargeTiers = []RechargeTier{
    {AmountCents: 20000, GiftCents: 2000},   // 充200送20
    {AmountCents: 50000, GiftCents: 8000},   // 充500送80
    {AmountCents: 100000, GiftCents: 20000}, // 充1000送200
}
type RechargeTier struct{ AmountCents, GiftCents int64 }
// GiftForAmount 返充值 amountCents 对应赠送(无匹配档位→0). 纯函数.
func GiftForAmount(amountCents int64) int64

// RechargeInput 储值充值请求.
type RechargeInput struct {
    MemberID     int64
    AmountCents  int64  // 实付(资金经微信直清特约商户, 反二清; 平台不持有)
    RefPaymentID int64  // 关联 payment.ID(反二清审计链路; 可 0=手动)
    OperatorID   int64
}

// RechargeResult 充值结果.
type RechargeResult struct {
    MemberID       int64
    BalanceCents   int64 // 入账后实付余额
    GiftCents      int64 // 入账后赠送余额
    RechargedCents int64 // 本次实付
    GiftedCents    int64 // 本次赠送(档位规则算)
}

// Service 加方法 (现有方法不动):
// Recharge 储值充值(反二清记账):
//   - EnforceTenant; member 存在且 active(否则 ErrNotFound/ErrMemberBlocked)
//   - gift = GiftForAmount(amountCents)
//   - rechargeAtomic: load account → 校验 balance+amount+gift ≤ RegionCapCents(0=不限; 超→errcode.ErrBalanceOver) → += amount+gift → append recharge log
//   - 返 RechargeResult
//   amountCents<=0 → errcode.ErrParam. 不 import payment.
func (s *Service) Recharge(ctx context.Context, in RechargeInput) (*RechargeResult, error)

// SetRegionCap 设置会员账户地区单卡余额上限(admin/合规; 主线程按租户 province 种入).
func (s *Service) SetRegionCap(ctx context.Context, memberID, regionCapCents int64) error

// MemberRepo 加方法 (现有方法签名不动):
type MemberRepo interface {
    // ... 现有方法一字不动 ...

    // rechargeAtomic 原子入账: load account(不存在→ErrNotFound) → check cap(超→errcode.ErrBalanceOver, 不改状态)
    //   → BalanceCents+=amount, GiftCents+=gift → UpsertAccount → append recharge log → 返更新后 account.
    rechargeAtomic(tenantID, memberID int64, now time.Time, amountCents, giftCents, refPaymentID, operatorID int64) (*BalanceAccount, error)

    // UpdateAccountRegionCap 设置账户 RegionCapCents(跨租户→no-op 或 ErrNotFound, agent 选后者).
    UpdateAccountRegionCap(ctx context.Context, tenantID, memberID, regionCapCents int64) error
}
```
**注意**：membership 包 A4 agent 拥有整个包。**现有 Join/GetMember/ListMembers/IssueCode/Redeem + MemberRepo 现有方法 + redeemAtomic 签名一字不动**，现有测试全绿。仅加 BalanceAccount.RegionCapCents 字段 + rechargeTiers/GiftForAmount + Service.Recharge/SetRegionCap + MemberRepo.rechargeAtomic/UpdateAccountRegionCap。memory_repo.go 的 UpsertAccount/GetAccount 实现要读写新 RegionCapCents 字段（默认 0=不限，Join 开零余额账户时 RegionCapCents=0）。

### 15.4 包所有权（文件集互斥，每个 agent 拥有互斥的**包**）

| agent | 拥有（**只准写这些文件**） | 禁动 |
|---|---|---|
| **A1 payment** | `internal/payment/*.go`（含 `*_test.go`，新包全建）**+** `migrations/000012_payment_submerchant.up.sql` + `migrations/000012_payment_submerchant.down.sql` | 其它一切 |
| **A2 subscription** | `internal/subscription/*.go`（含 `*_test.go`，新包全建）**+** `migrations/000013_subscription.up.sql` + `migrations/000013_subscription.down.sql` | 其它一切；不碰 payments/000012 |
| **A3 billing** | `internal/billing/*.go`（含 `*_test.go`，加性扩展） | migrations/；其它包 |
| **A4 membership** | `internal/membership/*.go`（含 `*_test.go`，加性扩展） | migrations/；其它包 |
| **主线程** | `CONTRACT.md` §15 / `cmd/api/main.go` / `cmd/api/payment_adapters.go`(新) / `interfaces/http/router.go` / `interfaces/http/handler/payment.go`(新) / `interfaces/http/handler/subscription.go`(新) / `interfaces/http/handler/billing.go`(加支付端点) / `interfaces/http/handler/membership.go`(加充值端点) / `interfaces/http/m4s1_test.go`(新) / `pkg/errcode/*.go`(加错误码) / `go.mod`(如需) / `configs/config.dev.yaml`(如需) | 四 agent 的包内文件 |

**跨包编译约束**：agent 本地只跑 `go test ./internal/<本包>/...`（**不要** `go build ./...`——集成前整体必编译失败，那是主线程收尾职责）。

### 15.5 硬约束（M4-S1 全员）

1. **签名一字不差**：§15.3 的类型/方法签名是冻结的，主线程集成按此接线。返回 error 不 panic。
2. **多租户行级隔离**：所有 Repo/Service 方法 EnforceTenant 取 tenantID，跨租户=不存在（ErrNotFound）。agent 测试须含跨租户用例（assert 返 ErrNotFound/零值/false）。
3. **反二清（命门）**：payment 域**不持有资金**——PaymentLedger 仅审计记账，资金由微信支付直清特约商户 sub_merchant（payments.sub_merchant_id 标记清算方）。平台账户**无余额沉淀**。储值充值资金流经 payment(Subject=recharge) → 微信直清 → membership.Recharge 仅入账（book）。代码中**不得**出现「平台收款」「平台余额」概念。
4. **回调幂等（命门）**：HandleNotify 重复回调同一 provider_tx_id：① ledger.Append 返已存记录（UNIQUE 去重，不重复入流水）；② MarkSuccess changed=false（不重复推进状态）；③ 不重 Fulfill（不重复发券/扣款/激活）。幂等性必须有用例证明（同一回调调两次，ledger/payment 各一条，履约一次）。
5. **状态机不可逆**：payment pending→success 单向；order pending→paid 单向；已 success/paid 的重复推进返 changed=false/幂等 nil，非 refunded→paid 等逆推进返 ErrStatusConflict。
6. **反腐端口**：payment 不 import billing/membership/subscription（Fulfiller 自声明）；billing/membership 不 import payment（支付单号作参数）；subscription 不 import payment。四包两两不互 import（无环，§15.3.0）。
7. **双实现**：payment 三 Repo + subscription Store 均 Memory（dev/test）+ PG（pgxmock 测，生产配 PG）。pgxmock 用 `github.com/pashagolub/pgxmock/v4`（白名单 §3）。PGRepo 窄 Pool 接口（同 attribution/bandit 范式，type-assert queryer 读多行）。
8. **V3 签名纯函数**：SignV3Authorization/VerifyV3Notify 用 stdlib crypto（rsa/sha256/x509），**不引第三方微信 SDK**（白名单不含）。确定性可测（注入私/公钥）。FakeWxPay.ParseNotify 不验签（JSON 反序列化），真实 NotifyParser 调 VerifyV3。
9. **-race 全绿**：Memory 实现用 sync.Mutex 保护 map；`go test -race ./internal/<本包>/...` 通过。
10. **确定性测试**：用 WithNowFunc/WithOutTradeNoFunc 注入确定性时钟/单号生成；禁 time.Now()（签名或 Option 注入）。V3 签名测试用固定 nonce/ts/key。
11. **gofmt + go vet 干净**；包注释 `// Package xx ...`（§4 约定 7）。
12. **加性扩展不破现有**：billing/membership 现有方法签名与行为不动，现有测试全绿（agent 跑 `go test ./internal/billing/... ./internal/membership/...` 验证）。
13. **不引白名单外依赖**（§3）。payment 需 stdlib crypto/encoding/json；subscription 纯 stdlib；billing/membership 无新依赖。`go.mod` 预期**无需新增**。

### 15.6 DoD（M4-S1 各 agent）

- **A1 payment**：① 三 MemoryRepo（SubMerchant/Payment/Ledger）CRUD + MarkSuccess(changed 标志) + Append(UNIQUE 幂等) + 跨租户；② 三 PGRepo pgxmock 测（INSERT...ON CONFLICT upsert + SELECT）；③ FakeWxPay（Applier/Preparer/NotifyParser 三端口确定性实现）；④ SignV3Authorization/VerifyV3Notify 纯函数（注入 rsa key，确定性，验签失败返 ErrSignInvalid）；⑤ Service.ApplySubMerchant/QuerySubMerchant/ListSubMerchants；⑥ Service.Prepay（active 特约商户校验 + outTradeNo 唯一）；⑦ **Service.HandleNotify 幂等**（重复回调 ledger/payment 各一条、履约一次，用例证明）+ Fulfiller nil 安全 + 验签失败不动库；⑧ migration 000012 up/down 合法（三表 + 索引 + trg_set_updated_at）；⑨ `-race` 全绿。**至少 22 测试**。
- **A2 subscription**：① MemoryStore CurrentPlan(无→free)/Get/Upsert(旧 active 置 cancelled)/ListHistory + 跨租户；② PlanLevel/PlanPriceCents/GiftForAmount 纯函数；③ FeatureGate.Can/Require（free→pro feature 拒、pro→flagship feature 拒、pro→pro feature 过、未知 feature→false）；④ Service.CurrentPlan/GetSubscription/Gate/Subscribe/ConfirmPayment(amountCents→plan 映射)；⑤ migration 000013 up/down 合法（UNIQUE active one）；⑥ `-race` 全绿。**至少 14 测试**。
- **A3 billing**：① Order.PaymentRef 字段；② OrderRepo.UpdateStatus（pending→paid 填支付字段、跨租户 ErrNotFound）；③ Service.CreatePendingOrder（total 计算 + status=pending + paid=0）；④ **Service.MarkOrderPaid**（pending→paid、已 paid 幂等 nil、refunded/voided→ErrStatusConflict、跨租户 ErrNotFound）；⑤ 现有 CreateOrder/Redeem 等测试**全绿不破**；⑥ `-race` 全绿。**至少 +6 新测试**（累计不降）。
- **A4 membership**：① BalanceAccount.RegionCapCents 字段 + memory_repo 读写；② rechargeTiers/GiftForAmount（档位匹配 + 无匹配→0）；③ MemberRepo.rechargeAtomic（入账 + cap 校验超→errcode.ErrBalanceOver 不改状态 + recharge log）；④ UpdateAccountRegionCap；⑤ **Service.Recharge**（active 校验 + gift 内部算 + cap 拦截 + 跨租户）；⑥ Service.SetRegionCap；⑦ 现有 Join/Redeem 测试**全绿不破**；⑧ `-race` 全绿。**至少 +8 新测试**（累计不降）。

### 15.7 集成 + 配置（主线程，workflow 收尾后）

- **errcode 新增**（`pkg/errcode/errcode.go`）：`ErrPlanUpgradeRequired = Err{40303, "当前套餐不支持该功能，请升级", http.StatusForbidden}`；`ErrSignInvalid = Err{40103, "支付回调验签失败", http.StatusUnauthorized}`；`ErrPaymentDuplicate = Err{40902, "支付单号重复", http.StatusConflict}`；`ErrSubMerchantNotActive = Err{60023, "特约商户未进件或未激活", http.StatusOK}`；复用 `ErrBalanceOver`(60003)/`ErrIdempotent`(40901)。
- **main.go**：构造 `paymentSvc := payment.NewService(payment.NewMemorySubMerchantRepo(), payment.NewMemoryPaymentRepo(), payment.NewMemoryLedgerRepo(), payment.WithApplier(fakeWxPay), payment.WithPreparer(fakeWxPay), payment.WithNotifyParser(fakeWxPay), payment.WithFulfiller(paymentFulfiller{bill:billingSvc, mbr:membershipSvc, sub:subscriptionSvc}))`（`fakeWxPay := payment.NewFakeWxPay()`，dev 确定性）；`subscriptionSvc := subscription.NewService(subscription.NewMemoryStore())`（默认 free，主线程可种 demo 租户 plan）。Deps 加 `Payment *payment.Service` + `Subscription *subscription.Service`。
- **payment_adapters.go（新）**：① `type fakeWxPay struct{}` + `NewFakeWxPay()` 实装三端口（确定性 applymentID/prepayID/notify 反序列化）；② `paymentFulfiller{bill, mbr, sub}` 实装 `Fulfill(ctx, p)` 按 `p.Subject` switch：`SubjectOrder`→`bill.MarkOrderPaid(p.RefID, p.OutTradeNo, *p.PaidAt)`；`SubjectRecharge`→`mbr.Recharge(RechargeInput{MemberID:p.RefID, AmountCents:p.AmountCents, RefPaymentID:p.ID})`；`SubjectSubscription`→`sub.ConfirmPayment(tenantID, p.AmountCents, p.Provider, p.OutTradeNo)`；默认 best-effort（err 仅日志不回滚 payment）。
- **router.go**：Deps 加 `Payment`/`Subscription` 字段。挂载：
  - `/v1/boss/payment/sub-merchants`（POST 进件 / GET 列表 / POST :id/query 查状态）
  - `/v1/boss/payment/prepay`（POST 统一下单）
  - `/v1/boss/payment/payments/:id`（GET 查支付单）
  - `/v1/boss/payment/ledger`（GET 流水）
  - `/v1/wxshop/payment/notify`（POST 微信回调，**无鉴权**——验签入口；handler 调 paymentSvc.HandleNotify，成功 200 返微信要求格式 `{"code":"SUCCESS","message":"成功"}`，验签失败 401）
  - `/v1/boss/subscription`（GET 当前 / POST subscribe）
  - `/v1/boss/billing/orders/pending`（POST 建 pending 订单）+ `/v1/boss/billing/orders/:id/pay`（GET/POST 模拟支付成功回调——dev 用，生产走 wxshop/notify）
  - `/v1/boss/membership/members/:id/recharge`（POST 储值充值）+ `/v1/boss/membership/members/:id/region-cap`（POST 设置地区上限）
  handler/payment.go（新）+ handler/subscription.go（新）+ handler/billing.go 加 CreatePendingOrder/SimulatePay + handler/membership.go 加 Recharge/SetRegionCap。
- **HTTP 冒烟（主线程 `interfaces/http/m4s1_test.go`）**：① 进件（POST sub-merchant → Fake 即时 reviewing → query → active，sub_mch_id 非空）；② 下单+支付（prepay order → 模拟微信回调 POST notify → payment success + order paid + ledger 一条）；③ **回调幂等**（同一 notify POST 两次 → ledger 仍一条、payment 仍一条、order 不重复 paid）；④ 储值充值（prepay recharge → 回调 → member BalanceCents += amount、GiftCents += 档位 gift、balance_log recharge 一条）；⑤ 地区上限（SetRegionCap 后 Recharge 超 cap → 60003 ErrBalanceOver，余额不变）；⑥ 订阅付费墙（free 租户调 pro 功能 → 40303；Subscribe pro → 同功能放行）；⑦ 串户（跨 tenant GET payment/order/subscription → 404/40302）。

### 15.8 显式延后（不在 M4-S1）

- 支付宝 ISV 进件/下单（provider=alipay 留接口位，FakeWxPay 只实装 wxpay；alipay 延后 M4-S2/生产）。
- payment_ledger 按月分区（PARTITION BY RANGE created_at，生产部署加；本期单表 + UNIQUE(tenant,provider,tx_id)）。
- 微信支付 V3 真实 HTTP（api.mch.weixin.qq.com）+ 平台证书下载/轮换（本期签名纯函数 + FakeWxPay；生产配 mchid/apikey/平台证书后启用真实 NotifyParser）。
- reconciliation_diffs 对账差异表 + 供应商/外卖对账（BE-A-11，M4-S2）。
- 供应链联合采购 / 品牌广告（M4-S2 BE-A-12/13）。
- 真实 PG 接入（pgx repo 生产实装 payment 三表 / subscriptions；本期 Memory 双实现）。
- 订阅自动续费 / 到期降级 cron（本期 ConfirmPayment 激活即 active；过期检测 cron 延后）。
- 储值退款（type=refund 流水类型留位，退款流延后）。
- WX-01 微信支付服务商资质申请（行政，M0-S2 已启动；资质到位前代码就绪不收款）。
- 多店功能门禁落地（FeatureMultiStore 定义在矩阵，但本期单店为主，多店 RBAC 校验延后）。

## 16. MVP C端业务接口契约（2026-07-29）

本期补 **C 端（消费者小程序）业务接口**，挂在 `/v1/wxshop` 的 authed 组（JWT）之下，解锁 consumer-mp 前端从 mock 转真实（FE-MP-01 骨架的 `USE_MOCK` 开关可置 false）。**纯加性**：不改任何已存在的共享类型/域服务签名，只新增 `interfaces/http/handler/wxshop.go` + 路由装配 + `interfaces/http/wxshop_test.go`。所有接口从 verified JWT claims 取 openid、从 verified ctx 取 tenant/store，**绝不从 body 信 tenant/member**（ARCH-01）；串户访问与"不存在"不可区分（40404 / 空列表），绝不返回他租户资源。

### 16.1 路由装配（router.go `wireWxshop` authed 组）

仅当 `Membership && Marketing && Groupbuy` 均非 nil 时挂载（`Billing` 可 nil → `/orders` 降级空列表）：

```go
h := handler.NewWxshopHandler(deps.Membership, deps.Marketing, deps.Groupbuy, deps.Billing)
authed.GET("/coupons", h.Coupons)
authed.GET("/member/profile", h.Profile)
authed.POST("/member/code", h.MemberCode)
authed.POST("/member/claim-newcomer", h.ClaimNewcomer)
authed.GET("/groupbuys/active", h.GroupbuysActive)
authed.POST("/groupbuys/:id/join", h.GroupbuyJoin)
authed.GET("/orders", h.Orders)
```

### 16.2 memberID 约定（核心）：Join 幂等拿 member

C 端 JWT 不带 memberID（只带 openid/tenant/store）。**每个 C 端接口都先调 `membership.Join(ctx, JoinInput{WxOpenID: claims.OpenID, PhoneHash: ...})` 幂等拿 member**：Join 按 (tenant, wx_openid) 去重，首次访问自动开户（含零余额储值账户），后续复用同一 member 行。这是拿到 memberID 的**唯一推荐方式**——不改 MemberRepo 私有方法（`findMemberIDByOpenID` 等），不新增"按 openid 查 member"的公开方法。openid 为空 → 40101。

### 16.3 端点契约（7 + 5 = 12 个；FE-MP-02 补 5 个）

统一响应信封 `{code,message,data,trace_id}`。字段全部 snake_case，与 `apps/consumer-mp/types/api.d.ts` 对齐。

**1) `GET /v1/wxshop/coupons?status=` — 我的券包**
- query `status` 缺省 `unused`；可 `unused|used|expired`。
- 流程：Join 幂等得 member → `ListMemberCoupons(member.ID, status)` → 逐张补模板渲染前端 `Coupon` DTO。
- 响应 `data: Coupon[]`：
```json
[{"id":1,"title":"新人见面礼 满30减10","type":"cash","value_text":"¥10","threshold_text":"满¥30可用","scope_text":"全场通用","expire_text":"2026-08-05 到期","status":"unused"}]
```
- 类型映射：后端 `full_reduction|cash`→前端 `cash`，`discount`→`discount`，`gift`→`gift`。`value_text`=¥N（cash）/ N折（discount）/ 赠品（gift）。仍 `unused` 但已过期的券渲染为 `expired`（C 端不展示过期可用券）。

**2) `GET /v1/wxshop/member/profile` — 我的会员档案**
- 响应 `data`：
```json
{"member_id":1,"phone_masked":"","level":"normal","level_name":"普通会员","points":0,"balance_cents":0,"total_spent_cents":0,"tenant_id":50021,"store_id":9}
```
- `level_name`：normal→普通会员 / vip→银卡会员 / blackcard→金卡会员。

**3) `POST /v1/wxshop/member/code` — 生成动态会员码**
- 无 body。流程：Join 幂等得 member → `IssueCode(member.ID)`。
- 响应 `data: {"code":"<hmac-nonce>","expire_seconds":60}`（ttl 对齐 membership `codeTTL`=60s）。
- errcode：40402 会员不存在 / 42302 会员已冻结。

**4) `POST /v1/wxshop/member/claim-newcomer` — 领新人券**
- body `{"phone":"138..."}`（可选；非空时 hash 后作为 Join 的 phone_hash 幂等键之一，不存明文）。
- **幂等**：member 已持有 `source=newmember` 的券 → 返回该券且 `already_claimed:true`，不重复发。
- 模板：先找本租户 active 且标题含「新人/新客/见面礼/新会员」的券；找不到则**自动建**一张默认「新人见面礼 满30减10」（full_reduction 满3000减1000，1年有效，不限量）并激活。
- 发行：`Issue(member, coupon, source=newmember, expire_at=now+7d)`（领取后 7 天有效，含毛利护栏）。
- 响应 `data: {"coupon":Coupon, "already_claimed"?:true}`。
- errcode：60005 券已发完 / 60001 毛利护栏拦截。

**5) `GET /v1/wxshop/groupbuys/active` — 进行中拼团**
- store 取 ctx（fallback tenant 全店）。流程：`ListActive(storeID)`，joined 用 `ListParticipants` 计数（权威）。
- 响应 `data: GroupBuyBrief[]`：
```json
[{"id":1,"title":"3人团 新鲜草莓 2斤装","price_cents":1990,"original_cents":1990,"required":3,"joined":2}]
```
- `original_cents`：Groupbuy 无原价字段，本期降级=团购价（前端可渲染；TODO 补原价）。

**6) `POST /v1/wxshop/groupbuys/:id/join` — 参团**
- 无 body（memberID 从 Join 幂等得，**不从 body 信**）。流程：`groupbuy.Join(id, member.ID)`。
- 响应 `data: {"participant":Participant, "groupbuy":Groupbuy}`。
- errcode：40404 拼团不存在（含串户）/ 60013 已满 / 60014 已结束 / 60015 已参团。

**7) `GET /v1/wxshop/orders` — 我的订单/交易记录**
- **本期降级**：billing 无"按 member 查询"公开方法，返回空数组 `data: []`（不硬塞散客/全店订单，防越权）。TODO §16.8。
- 仍先 Join 幂等拿 member 以证明身份+租户。

**8) `GET /v1/wxshop/groupbuys/:id` — 拼团详情（FE-MP-02）**
- 流程：`groupbuy.Get(ctx, id)` + `ListParticipants(ctx, id)` 计权威 joined。memberID 不取(参团前浏览,仅 verified tenant 从 ctx)。
- 响应 `data`（隐藏 tenant_id/store_id/product_id/strategy_id/joined_count）：
```json
{"id":1,"title":"3人团 新鲜草莓 2斤装","desc":"3人团 新鲜草莓 2斤装","price_cents":1990,"original_cents":1990,"required":3,"joined":2,"status":"active","expire":"2026-07-30T10:00:00+08:00"}
```
- `status`: active|success|failed|cancelled; `expire`=EndAt(倒计时用); `original_cents` 仍降级=团购价(无原价字段)。
- errcode：40404 拼团不存在(含串户)/ 40001 id 非法。

**9) `GET /v1/wxshop/coupons/:id` — 券详情（FE-MP-02）**
- 流程：`marketing.GetCoupon(ctx, id)` 取模板; member 经 memberFromClaims 幂等取; 若该 member 持有此券(ListMemberCoupons 匹配 CouponID) 则带真实 status+expire, 否则 status="available"+available=true。
- 响应 `data`（隐藏 quota/issued/strategy_id）：
```json
{"id":1,"title":"新人见面礼 满30减10","type":"cash","value_text":"¥10","threshold_text":"满¥30可用","scope_text":"全场通用","expire_text":"2027-01-05 到期","status":"unused","detail":{"threshold_text":"满¥30可用","scope_text":"全场通用","value_text":"¥10","valid_from":"2026-07-29","valid_to":"2027-01-05"},"available":false,"expire":"2026-08-05"}
```
- errcode：40403 券不存在(含串户)/ 40001 id 非法。

**10) `GET /v1/wxshop/member/points?limit=` — 积分明细（FE-MP-02）**
- 流程：member 经 memberFromClaims 取; `membership.ListPointsLog(ctx, member.ID, limit)` 读 member_points_log(memory+pg 双实现)。limit 默认 50, 上限 200。
- 响应 `data`：
```json
{"points":50,"logs":[{"type":"earn","delta":50,"points_after":50,"ref_order_id":700,"created_at":"2026-07-29T18:25:08+08:00"}]}
```
- `type`: earn|redeem|expire|adjust; logs 按 created_at DESC; 串户→空 logs+0 points(member 行 tenant 隔离)。

**11) `GET /v1/wxshop/stores` — 门店列表（FE-MP-02, 独立装配）**
- 新建轻量 `internal/store/` 域: Store{id,tenant_id,name,address,phone,group_qr,active}; MemoryStoreRepo(种子 tenant_id=1 两条 demo) + PGStoreRepo(读 000001 stores 表 + 000016 phone/group_qr/active 列); service.ListStores(ctx) 按 tenant 过滤。
- 流程：`store.ListStores(ctx)`(EnforceTenant; 无 tenant→40302)。store 服务独立装配(`deps.StoreSvc`), 不依赖 Membership+Marketing+Groupbuy。
- 响应 `data`（隐藏 tenant_id/active/created_at）：
```json
[{"id":7001,"name":"总店 · 阳光水果行","address":"上海市浦东新区阳光路1号","phone":"021-50000001","group_qr":"group_qr_seed_1"}]
```
- 串户→空数组(非 404); store 服务未装配(nil)→空数组降级(不 404)。

**5-修) `GET /v1/wxshop/member/profile` — balance_cents 修复（FE-MP-02, 退役 §16.4 TODO）**
- balance_cents 从硬编码 0 改为调 `membership.GetAccount(ctx, m.ID)`: account 不存在返 0(与 Balance handler 一致), 存在返真实 BalanceCents。phone_masked 仍空。其余字段不变。

### 16.4 已知 TODO 降级（不阻塞接通）
- ~~**`balance_cents` 恒 0**~~: **FE-MP-02 退役** —— Profile 已接 `membership.GetAccount(ctx, m.ID)` 返回真实储值余额(§16.3(5-修))。
- **`GET /orders` 空数组**：见 §16.3(7)。MVP 已接 billing 加 `ListByMember`（M4-S2 §17.4），返回真实订单（MVP 冒烟已证）。
- **`original_cents` = 团购价**：Groupbuy 无原价字段；拼团详情(§16.3(8))继承同降级。
- **`phone_masked` 空**：member 只存 phone_hash，无法回显明文/掩码；C 端档案不展示手机号。

### 16.5 默认新人券模板
`新人见面礼 满30减10`：type=full_reduction，params `{threshold_cents:3000, off_cents:1000}`，valid 1 年，TotalQuota=0（不限量），auto-create + activate。领取时 `expire_at=now+7d`（领取后 7 天有效）。

### 16.6 串户隔离（ARCH-01）
- 每个接口 tenant 取自 verified ctx（`EnforceTenant`/FromContext），member 用 claims.OpenID 在本租户内 Join 幂等——**同一 openid 在不同租户映射到不同 member 行**（member 行带 tenant_id）。
- 串户结果：券包/档案/拼团均只见本租户数据；跨租户参团/读资源 → 40404（与不存在同口径）。绝不返回他租户资源。

### 16.7 测试（`interfaces/http/wxshop_test.go`，memory repo + 真实 dev-key JWT）
- `TestWxshop_FullConsumerFlow`：领新人券（auto-create 模板）→ 幂等再领（already_claimed）→ 券包 1 张 unused → 档案（普通会员/tenant/store 正确）→ 动态码非空+expire_seconds=60 → 建拼团 → 进行中列表 → 参团 → 重复参团 60015 → 订单空数组。
- `TestWxshop_CrossTenantIsolation`：tenant A 领券 → tenant B（同 openid）券包为空、member_id 不同（每租户独立 member 行）→ B 参 A 的团 40404。
- `TestWxshop_NoToken`：7 端点无 token 全部 401/40101。
- 退出码：`go build ./...`=0；`go test ./interfaces/... ./internal/... -race`=0（不破坏 M0→M4-S1 全部现有测试）。

### 16.8 后续（解锁 mock→真实后）
- membership 加 `GetAccount` 公开读 → profile 真实储值余额。
- billing 加 `ListByMember` → orders 真实交易记录。
- consumer-mp 各 service `USE_MOCK` 置 false（types/services 字段已对齐本期契约）。


### 16.9 商家端 MVP 前端闭环（billing 开单 + me 我的，2026-07-29）
- **billing 开单页**(`apps/merchant-mp/pages/billing/`,原占位 → 真实): 商品搜索(query 模糊 + 分页触底)+ `wx.scanCode` → `GET /catalog/products/barcode/:barcode` 入车; 购物车步进器/删除/清空; 两段式开单 `POST /billing/orders/pending` → 确认收款 `POST /billing/orders/:id/pay`(SimulatePay)→ 回执。库存由后端 MarkOrderPaid best-effort FIFO 扣,前端不调 consume。
- **me 我的页**(`apps/merchant-mp/pages/me/`,原占位 → 真实): `GET /billing/summary/today`(营业额 NetCents/订单数 OrderCount/客单价前端算)+ `GET /subscription`(plan/到期)+ `GET /whoami`(tenant_id+source)。
- **响应字段大小写约定(关键)**: `catalog.Product` / `billing.Order` / `billing.DailySummary` / `subscription.Subscription` 结构体**未声明 json tag**,Go encoding/json 默认按字段名输出 **PascalCase**(`ID/Name/PriceCents/TotalCents/NetCents/OrderCount/Plan/...`);仅 handler 的 DTO(`createOrderRequest` 等)有显式 snake_case tag。前端 `types/api.d.ts` 据此按 PascalCase 镜像这些类型(文件顶部有醒目注释)。**历史接口测试(m1s1 等)大量断言 PascalCase 键,故本期不改共享类型 json tag,保持 PascalCase 契约**;若未来统一 snake_case 需同步改全部历史测试。
- GetStock(`GET /inventory/stock`)返回 `gin.H{"product_id","store_id","quantity"}` → **小写 `quantity`**(与 batch 实体的 PascalCase `Quantity` 不同,注意区分)。
- **会员总数**: B 端 `/v1/boss/membership` 无列表/计数接口(仅 Join/GetMember/IssueCode/Redeem/Recharge/SetRegionCap),me 页会员数卡片显示「—」占位。TODO: 补 `GET /membership/members?count` 或 summary。

### 16.10 MVP 一体化冒烟（`interfaces/http/mvp_smoke_test.go`，同一进程双 BFF 共享 in-memory 状态）
- `TestMVP_EndToEnd`: B 端 建品→扫码查品→入库20→现金开单2件(扣库存20→18)→挂单3件→收款(→15)→当日流水(2单/NetCents=9950); C 端 JWT→领新人券(幂等)→券包1→档案→动态码→建拼团→列表→参团→重复参团60015→订单空。双端在同一 fixture(同一 tenant 50021/store 9)共享 catalog/inventory/billing/membership/marketing/groupbuy 状态,端到端证明 MVP 闭环。
- `TestMVP_CrossTenant`: tenant A 建品+开单+领券后, tenant B(同 openid)商品列表空/流水0/券包空/拼团空 —— ARCH-01 串户隔离端到端证明。
- 退出码: `go test ./interfaces/http/ -run TestMVP_ -count=1`=0(连续5次稳定)。

### 16.11 FE-MP-02 / FE-ADMIN 后端配套（2026-07-29）：C 端缺口接口 + boss 会员列表

本期补齐 C 端消费者缺口接口 + boss 端会员列表, 端到端真实(不再降级 mock)。**纯加性**：不改既有域服务签名(memory_repo.go/service.go 仅加性新增方法); 新建 `internal/store/` 域 + migration 000016(stores 表加 phone/group_qr/active 列)。

**新增接口路径清单（C 端 `/v1/wxshop/*` authed 组 + boss `/v1/boss/*`）**:
- `GET /v1/wxshop/groupbuys/:id` — 拼团详情(§16.3(8))。
- `GET /v1/wxshop/coupons/:id` — 券详情(§16.3(9))。
- `GET /v1/wxshop/member/points?limit=` — 积分明细(§16.3(10))。
- `GET /v1/wxshop/stores` — 门店列表(§16.3(11), 独立装配 deps.StoreSvc)。
- `GET /v1/boss/membership/members?page=&size=&level=&q=` — boss 会员列表(§17.11)。
- 修 `GET /v1/wxshop/member/profile` balance_cents(§16.3(5-修), 退役 §16.4 TODO)。
- boss `GET /v1/boss/membership/members/:id` 调整为 memberDTO 形态(隐藏 phone hash/openid, 加 balance_cents)。

**新建域 `internal/store/`**:
- `store.go`: Store struct{id,tenant_id,name,address,phone,group_qr,active,created_at} + StoreRepo 接口{ListStores(ctx,tenantID)}。
- `memory_repo.go`: MemoryStoreRepo(种子 tenant_id=1 两条 demo: 总店+分店; StoreID 从 7000 起; Seed() 测试用); ListStores 按 tenant 过滤 + active 过滤 + id 升序; 防御性拷贝。
- `pg_repo.go`: PGStoreRepo(读 000001 stores 表 + 000016 phone/group_qr/active 列; `SELECT id,tenant_id,COALESCE(address,''),COALESCE(phone,''),COALESCE(group_qr,''),status='active',created_at FROM stores WHERE tenant_id=$1 AND deleted_at IS NULL ORDER BY id`; nil-pool→空; queryer type-assert)。
- `service.go`: Service.ListStores(ctx)(EnforceTenant; 无 tenant→40302)。
- `pg_repo_test.go` + `service_test.go`: pgxmock happy/empty/null-cols/nil-pool/query-error + service happy/cross-tenant/inactive-hidden/no-tenant/clone。

**membership 新增（加性, 不改既有方法签名）**:
- `PointsLog` struct(public 读 shape) + `ListMembersResult{Items,Total}` struct。
- `MemberFilter` 加字段: `Level string`(equality) + `Query string`(name/phone_hash 子串, pg 用 ILIKE) + `Offset int`(分页)。既有 SleepBefore/MinPoints/BirthdayOn/Limit 不变 → M3-S2 signal 调用方零值不受影响。
- `MemberRepo` 加方法: `ListPointsLog(ctx,tenantID,memberID,limit)`(memory 倒序扫 pointsLog, limit 默认 50; pg `SELECT ... FROM member_points_log WHERE tenant_id=$1 AND member_id=$2 ORDER BY created_at DESC,id DESC LIMIT $3`) + `ListMembersPage(ctx,tenantID,f)`(memory countLocked + 分页; pg COUNT(*) + 分页 SELECT, total=0 短路)。
- `Service` 加方法: `ListPointsLog(ctx,memberID,limit)` + `ListMembersPage(ctx,f)`(均 EnforceTenant)。
- `pg_repo_test.go` 加测: ListMembersPage happy/level-filter/empty + ListPointsLog happy/empty + NilPool 降级扩展。
- `service_test.go` 加测: ListMembersPage 服务层(Level/Query/pagination/total/cross-tenant) + ListPointsLog 服务层(earn log + cross-tenant)。

**boss membership list handler**(`handler/membership.go`):
- `ListMembers`: query `?page=&size=&level=&q=`; page 默认 1, size 默认 20(上限 200); 构 MemberFilter{Level,Query,Limit:size,Offset:(page-1)*size}; `svc.ListMembersPage` → `{items: memberDTO[], total}`; 逐 member best-effort `GetAccount` 取 balance_cents。
- `memberDTO` 字段: `id/level/level_name/name?/points/balance_cents/total_spent_cents/created_at/block`(status=='blocked'→block=true)。**不暴露** phone_hash/wx_openid/tenant_id/store_id。
- `GetMember`(:id) 调整为 memberDTO 形态(原返 raw *Member PascalCase; 现返 memberDTO snake_case, 隐藏敏感字段; balance_cents best-effort)。POST `/membership/members`(Join) 仍返 raw *Member(PascalCase, boss 域约定 + m1s2/m2s1 测试依赖)。

**migration 000016_store_cside**: `ALTER TABLE stores ADD COLUMN IF NOT EXISTS phone TEXT / group_qr TEXT / active BOOLEAN NOT NULL DEFAULT TRUE`(加性, 不改 000001 既有列/约束/触发器; active 与既有 status 并存, 互不耦合)。down 删三列。

**串户隔离（ARCH-01）**: 全部新接口 tenant 取自 verified ctx(EnforceTenant); member 用 claims.OpenID 在本租户内 Join 幂等。串户结果: 拼团详情→40404; 券详情→40403; 积分明细→空 logs+0 points; 门店→空数组; boss 会员列表→空+total 0。绝不返回他租户资源。

**测试（`interfaces/http/fe_mp02_test.go`, memory repo + 真实 dev-key JWT + DevMock boss）**:
- `TestWxshop_GroupbuyDetail`: 详情 happy + bad-id 400 + cross-tenant 40404 + 无内部字段泄漏。
- `TestWxshop_CouponDetail`: held 券 status=unused/available=false + absent 40403 + bad-id 400。
- `TestWxshop_Points`: fresh 0/empty + earn log(delta=50/after=50) + cross-tenant 空。
- `TestWxshop_Stores`: seeded tenant 1 → 2 店(无内部字段泄漏) + 非 seeded tenant → 空。
- `TestWxshop_ProfileBalance`: fresh 0 + Recharge 后 20000(退役 §16.4 TODO)。
- `TestBoss_MembersList`: 分页(page1/2 size2 total4) + level 过滤(normal=4 / vip=0) + q 过滤(Member1=1) + cross-tenant 空 + memberDTO 无 phone_hash/openid 泄漏。
- `TestWxshop_NoToken` 扩展: 新 5 端点无 token 全部 401/40101。
- 退出码: `go build ./...`=0; `go test -race ./...`=0(41 包全绿, 含 10 域 pgxmock + store 新包 + membership 新测)。

### 17.11 FE-MP-02 / FE-ADMIN boss membership list 增量（2026-07-29）
- boss `GET /v1/boss/membership/members?page=&size=&level=&q=` 列表(分页+过滤+total), 见 §16.11 详契。
- membership 加 `ListMembersPage(ctx, MemberFilter)`(repo 接口加性新增, §17.10 表 membership 方法数 13→15); `MemberFilter` 加 `Level/Query/Offset` 字段(加性, M3-S2 signal 零值调用方不受影响); memory + pg 双实现, pg 用 COUNT(*) + 分页 SELECT, total=0 短路跳过分页 SELECT。
- memberDTO 形态给后台用(隐藏 phone_hash/wx_openid), GetMember(:id) 同步调整为 memberDTO(Join 仍返 raw *Member PascalCase)。

## 17. M4-S2 增量契约（pgx 全局替换：10 域 memory→pg，2026-07-29）

### 17.1 范围与目标
把 10 个仍只有 memory 实现的域补齐 pgx 实现 + pgxmock 测试，达到 production-ready（配置 PG 后可直接切换，无需改 service/handler）。已有 pg 实现的 3 域（payment / aiengine.attribution / aiengine.bandit）**不动**。**纯加性**：不改任何域的 Repo 接口签名、不改 memory_repo.go、不改 service.go、不改 handler、不改现有 migration 000001–000013。唯一新增 migration：**000014 wx_sessions**（仅 auth 域需要，见 §17.7）。main.go 的 memory↔pg 切换装配由**主线程统一**做（§17.9），pgx agent **不碰 main.go**。

### 17.2 唯一权威样板（动手前必读）
**先完整读这两个文件，它们是本节的权威实现，本节只是其摘要**：
- `internal/payment/pg_repo.go`（472 行，PG 实现样板）
- `internal/payment/pg_repo_test.go`（240 行，pgxmock 测试样板）

每域 pgx 实现必须**一字不差遵循**以下 idiom（全部摘自 payment 样板）：

1. **PGPool 接口**（每域在本域包内定义，结构各域一致；main 用一个 `*pgxpool.Pool` 共享，它同时满足所有域的 PGPool + queryer）：
   ```go
   type PGPool interface {
       QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
       Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
   }
   // 多行读：PGPool 故意不含 Query，list 方法对 pool 做 type-assert；缺 Query 返回空 slice 而非报错
   type queryer interface{ Query(ctx context.Context, q string, args ...any) (pgx.Rows, error) }
   type rowScanner interface{ Scan(dest ...any) error }
   ```
2. **SQL 常量**：`const ( sqlXxxCreate = \`...\` ... )`，**每条读写语句的 WHERE 都带 `tenant_id = $N`**（租户行级隔离，ARCH-01）。INSERT 用 `RETURNING id` 取分配的主键。
3. **scan helpers**：`func scanXxx(s rowScanner) (*Xxx, error)`。nullable 列用 `database/sql.NullString` / `sql.NullTime`；JSONB 列 scan 进 `[]byte`，`json.Unmarshal` 到 map/struct；枚举列 scan 进 `string` 再转本域枚举类型。
4. **PGxxxRepo 结构**：`type PGxxxRepo struct{ pool PGPool }` + `func NewPGxxxRepo(pool PGPool) *PGxxxRepo`。
5. **nil-pool 安全降级**（关键）：每个方法首行 `if r == nil || r.pool == nil { return ErrNotFound / 空 slice / 零值 }`——配置缺失时优雅降级，绝不 panic。写方法（Create/Update）nil pool 时按 payment 样板：要么返回 ErrNotFound，要么（如 ledger Append）返回克隆的入参作 no-op——**照该域 memory 在"无持久化"时的语义选最贴近的**。
6. **错误映射**：`errors.Is(err, pgx.ErrNoRows) → 本域 ErrNotFound`；其他 `fmt.Errorf("domain: pg xxx: %w", err)`（保留 errors.Is/As 链）。
7. **不连真实 DB**：所有验证用 pgxmock（见 §17.3）。

### 17.3 pgxmock 测试规范（照 payment/pg_repo_test.go）
- 文件名 `pg_repo_test.go`，**同包**（`package xxx`，非 `xxx_test`），以便引用未导出的 `sqlXxx` 常量。
- `newMockPool(t)`：`pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))`（SQL 含正则元字符，必须等值匹配）+ `t.Cleanup(mock.Close)`。
- **每个 repo 方法至少 2 个 case**：happy（含 RETURNING id / scan 字段往返）+ 关键 error（ErrNoRows→ErrNotFound / 跨租户不可见）。
- 列名 helper：`func xxxColumns() []string { return []string{...} }`，顺序与 `scanXxx` 的 Scan 顺序**完全一致**——这是 pgxmock 行映射能成立的前提。
- WithArgs 的参数值要和实现真正传给 SQL 的值**类型+顺序一致**（如 `nullTime(t)`、`int64(1)`、枚举的底层 string）。
- **必加 `TestPGRepo_NilPoolIsSafe`**：覆盖 nil repo/nil pool 的降级返回。
- 每个 case 末尾 `require.NoError(t, mock.ExpectationsWereMet())`。

### 17.4 10 域映射表（每域 agent 的开工清单）
| 域 | migration | 主要表 | repo 方法数 | 参考文件（必读） |
|---|---|---|---|---|
| auth | **新 000014** | wx_sessions | 2 | internal/auth/{consumer.go,memory_repo.go} + §17.7 |
| billing | 000003 | orders, order_items | 7（**含本期 A2 新增 ListByMember**） | internal/billing/{billing.go,memory_repo.go} + migrations/000003 |
| catalog | 000002 | products | 5 | internal/catalog/{catalog.go,memory_repo.go} + 000002 |
| crm | 000006 | community_groups, wework_living_codes | 8 | internal/crm/{crm.go,memory_repo.go} + 000006 |
| groupbuy | 000006 | groupbuys, groupbuy_participants | 9（含 M3-S3 ListByStrategy） | internal/groupbuy/{groupbuy.go,memory_repo.go} + 000006 |
| inventory | 000002 | inventory_batches, inventory_movements | 8 | internal/inventory/{inventory.go,memory_repo.go} + 000002 |
| marketing | 000005 | coupons, member_coupons | 10 | internal/marketing/{marketing.go,memory_repo.go} + 000005 |
| membership | 000004 | members, member_balance_accounts, member_balance_log, member_points_log | 13（接口实际 13；另 5 是 memory 测试 helper，非接口方法） | internal/membership/{membership.go,memory_repo.go} + 000004 |
| reach | 000006 | reach_tasks, subscribe_auth, unsubscribe_records (+ living_codes/community_groups 若 memory 操作) | 10 | internal/reach/{reach.go,memory_repo.go} + 000006 |
| strategy | 000007 | strategies, strategy_items, strategy_feedback | 7 | internal/strategy/{strategy.go,memory_repo.go} + 000007 |

> 注：crm 与 reach 共处 migration 000006，但操作**不同表**（crm=community_groups/living_codes，reach=reach_tasks/subscribe_auth/unsubscribe_records）；以各自 memory_repo 实际操作的字段为准。strategy_attribution 表归属 aiengine.attribution（已有 pg，本期不动）。

### 17.5 硬约束（M4-S2 全员，违反即 reject）
1. **接口冻结**：本域 Repo 接口签名一字不改；新 pg 实现必须满足现有接口（编译期保证）。
2. **文件所有权**：每域只新建 `internal/<domain>/pg_repo.go` + `internal/<domain>/pg_repo_test.go`。**绝不改** memory_repo.go / service.go / handler / 其他域文件 / main.go / 现有 migration。
3. **唯一例外**：auth 域新建 `migrations/000014_wx_sessions.{up,down}.sql`（§17.7）。
4. **租户隔离**：每条 SQL 的 WHERE 带 tenant_id；跨租户查询返回 ErrNotFound / 空，与"不存在"不可区分。
5. **金额 int64 分**；时间用 `time.Time` / `sql.NullTime`；不改任何 PascalCase json 输出契约。
6. **不连真实 DB/Redis**；本机无 PG，验证只用 pgxmock。
7. **不 git commit**（主线程统一提交）。

### 17.6 DoD（每域 pgx agent）
- `internal/<domain>/pg_repo.go` 实现该域 Repo 接口**全部**方法（含 billing 的 ListByMember）。
- `internal/<domain>/pg_repo_test.go` 覆盖每方法 happy + error/not-found + nil-pool。
- `cd backend && go build ./...` 通过；`go test ./internal/<domain>/ -count=1` 全绿。
- 现有 `service_test.go` / `memory_repo_test.go` **不被破坏**（pg 实现是加性的，service 仍测 memory）。

### 17.7 auth 新增 migration 000014（唯一新表）
auth 的 ConsumerRepo 当前无表（memory only）。新建 `migrations/000014_wx_sessions.up.sql` + `.down.sql`，表结构对齐 `auth.Consumer`：
```sql
CREATE TABLE wx_sessions (
  id           BIGSERIAL PRIMARY KEY,
  tenant_id    BIGINT      NOT NULL,
  openid       TEXT        NOT NULL,
  unionid      TEXT        NOT NULL DEFAULT '',
  member_id    BIGINT      NOT NULL DEFAULT 0,
  session_key  TEXT        NOT NULL DEFAULT '',   -- SERVER-SIDE ONLY
  version      BIGINT      NOT NULL DEFAULT 0,    -- token version
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, openid)
);
CREATE INDEX idx_wx_sessions_tenant_openid ON wx_sessions (tenant_id, openid);
-- down: DROP TABLE IF EXISTS wx_sessions;
```
`PGConsumerRepo.UpsertByOpenID` = `INSERT ... ON CONFLICT (tenant_id, openid) DO UPDATE SET session_key=EXCLUDED.session_key, unionid=EXCLUDED.unionid, updated_at=now() RETURNING *`（等价 memory 的 upsert 语义）；`GetByID` = `SELECT ... WHERE tenant_id=$1 AND id=$2`。MemberID 在 auth 层先留 0（membership Join 时回填属另一期，本期不实现跨域回填）。

### 17.8 显式延后（不在 M4-S2）
- 真实 PG / Redis 接入联调（仍 pgxmock）。
- membership.Join 时回填 wx_sessions.member_id 的跨域写。
- main.go 切换装配后的端到端真 PG 冒烟。
- consumer-mp 前端 pg 相关改造（无，前端只认 HTTP 契约）。

### 17.9 集成（主线程，全部 10 域 pgx agent 收尾后）
- main.go 加 memory↔pg 切换：当 `database`（`*db.DB`，boot 时 `db.New` 返回）非 nil 且 Ping 通过时，各域用 `NewPGXxxRepo(database.Pool())`，否则 fallback 现有 `NewMemoryXxxRepo()`（dev 无 PG 行为不变）。
- 切换点集中在 main.go 现有 repo 装配段（约 128–262 行），逐域 `if database != nil { pg } else { mem }`。
- 验证：`go build ./...` + `go test -race ./... -count=1` 全绿（含 m4s1/m3s3/mvp 冒烟）；pgxmock 测试随各自包一起跑。

### 17.10 集成落地（2026-07-29，主线程收尾）
- **main.go 切换装配完成**：`cmd/api/main.go` 的 15 个 repo 装配点（本期 10 新 pgx 域 + payment 3 + attribution/bandit 2）改为 `var x <域接口> = NewMemoryXxx(); if database != nil { x = NewPGXxx(database.Pool()) }`；`*pgxpool.Pool` 隐式满足各域 `PGPool`+`queryer`。**dev 无 PG（database nil）行为不变**（全走 memory）；配置 PG 后整链路切 pg。subscription 保持 memory（无 pg 实现）。
- **两个新 migration**：000014 wx_sessions（auth，§17.7）+ **000015 orders.payment_ref**（M4-S1 给 `Order.PaymentRef` 加了领域字段但当时 billing 全 memory、无人补 DB 列；pgx 化 billing 暴露此 schema gap，pgxmock 不校验 schema 故测试不可见，必须由主线程补列）。
- **方法数校正**（§17.4 已回填实际接口方法数）：membership 13（非 18，另 5 是 memory 测试 helper）、strategy 7（非 8）、groupbuy 9（非 10）。
- **验证**：`go build ./...` 通过；`go test -race ./...` **40 包全绿**（含 MVP 冒烟 TestMVP_EndToEnd/CrossTenant、10 域 pgxmock pg_repo_test、现有 memory/service 测试、aiengine 3 个日期测试 A1 已修）。
- **pgx 实现亮点**：跨表原子（inventory `ConsumeFIFOAtomic`、membership `Redeem`/`recharge`、groupbuy `JoinAtomic`）用各域包内本地 `txBeginner` interface（领域包不耦合 pkg/db）+ 条件 UPDATE / `SELECT...FOR UPDATE`；并发原子（payment `MarkSuccess`、marketing 配额自增、reach 订阅计数、billing 状态机）用条件 UPDATE 的 `ErrNoRows` 检测；哨兵优先级严格对齐 memory；nil-pool 安全降级（审计写返回 nil 对齐 memory "审计不可失败"语义）；显式 RETURNING 列（非 `*`，对齐 payment 样板 + 兼容 pgxmock）。
- **固有特性（非 bug，已记录）**：JSONB 数字经 `map[string]any` 往返 int64→float64（strategy 测试记录）；memory↔pg 切换时这是固有不对称，真接 PG 时下游需注意。
- **显式延后**（仍不在本期）：真 PG/Redis 联调（验证均 pgxmock）；membership.Join 回填 wx_sessions.member_id；切换后真 PG 端到端冒烟。

---

## 18. FE-MP-02 / FE-ADMIN-01+02 双端落地（消费者小程序业务页补全 + 管理后台，2026-07-29）

本轮在 M4-S2 pgx 全局替换基础上，**双端并行**补全消费者小程序业务页与新建管理后台，配套补齐后端 C端缺口接口与 boss 端会员列表接口（端到端真实，不再降级 mock）。技术栈决策：admin-web 用 **Vite + React 18 + TS strict + Ant Design 5 + @ant-design/pro-components + ECharts**（技术方案 09 §9.4 原定 Umi Max，计划书指定 Vite，本轮采 Vite 更轻）。

### 18.1 后端配套（B1，见 §16.11 / §17.11 详契）

**C端新增 4 接口**（挂在 `/v1/wxshop` authed 组，JWT 鉴权，member 由 claims 经幂等 `membership.Join` 取，ARCH-01）：
- `GET /v1/wxshop/groupbuys/:id` — 拼团详情（groupbuyDetailDTO：id/title/desc/price_cents/original_cents/required/joined/status/expire）。跨租户 40404。
- `GET /v1/wxshop/coupons/:id` — 券详情（couponDetailDTO：couponDTO + detail{threshold_text/scope_text/value_text/valid_from/valid_to} + available + expire；member 持有时合并真实 status）。跨租户 40403。
- `GET /v1/wxshop/member/points?limit=` — 积分明细（{points, logs:[PointLog]}，读 member_points_log；limit 默认 50/上限 200）。
- `GET /v1/wxshop/stores` — 门店列表（storeDTO[]；新建轻量 internal/store 域：Store struct + StoreRepo + Service.ListStores + MemoryStoreRepo 种子 tenant_id=1 两条 + PGStoreRepo 读 stores 表 + migration 000016_store_cside）。

**修复**：`GET /v1/wxshop/member/profile` 的 `balance_cents` 从硬编码 0 改为调 `membership.GetAccount` 返真实值（退役 §16.4 TODO）。

**boss 端新增**（给 admin-web 会员页用）：`GET /v1/boss/membership/members?page=&size=&level=&q=` — 会员分页列表（{items:memberDTO[], total}）；`GET /v1/boss/membership/members/:id` 改返 memberDTO（隐藏 phone_hash/wx_openid，加 balance_cents）。memberDTO 全 snake_case。

**新增 repo 方法双实现**（memory + pgxmock）：`ListPointsLog` / `ListMembersPage` / `countMembersLocked`（membership）；`ListStores`（store 新域）。migration 000016 给 stores 表加 phone/group_qr/active 列。

**验证**：`go build ./...` + `gofmt`/`go vet` clean + `go test -race ./...` **41 包全绿**（新增 internal/store 包、membership 新测、6 新 HTTP 测 GroupbuyDetail/CouponDetail/Points/Stores/ProfileBalance/Boss_MembersList、10 域 pgxmock）。

### 18.2 消费者小程序 consumer-mp（FE-MP-02，原生小程序 + TS）

骨架 FE-MP-01 原有 5 页（index/coupons/profile/join/member-code）基础上**新增 6 业务页**，全部接通真实后端接口（services 层 USE_MOCK=false，失败降级占位但写操作不降级）：
- `pages/orders` — 我的订单（GET /v1/wxshop/orders，billing.ListByMember 真实返单；status 中文映射；下拉刷新；空态）
- `pages/balance` — 储值余额（GET /v1/wxshop/member/balance，balance_cents+gift_cents；充值入口占位）
- `pages/group-buy` — 拼团列表（GET /v1/wxshop/groupbuys/active）
- `pages/group-buy-detail` — 拼团详情+参团（GET /v1/wxshop/groupbuys/:id + POST /:id/join；errcode 60013/60015 映射）
- `pages/coupon-detail` — 券详情（GET /v1/wxshop/coupons/:id）
- `pages/points` — 积分明细（GET /v1/wxshop/member/points）
- `pages/stores` — 门店列表+进群二维码（GET /v1/wxshop/stores）

services 层新增：`getBalance/getGroupBuyDetail/getCouponDetail/getPoints`（group-buy/coupon/member.ts）+ 新建 `services/store.ts`（listStores）。types/api.d.ts 补 GroupBuyDetail/CouponDetail/PointsResult/PointLog/Order/OrderItem/Balance/Store 扩展。app.json pages 共 **15 页**。profile 九宫格 orders/balance/points/stores 接通跳转；首页拼团入口→group-buy 列表；coupons 券卡→coupon-detail（coupon-card bindtap→catchtap 防双触发）。

**验证**：临时装 typescript@5.5.4 + miniprogram-api-typings，strict noEmit 类型检查全 32 个 .ts **0 错**（含注入错误测试验证活跃度）。

### 18.3 管理后台 admin-web（FE-ADMIN-01 骨架 + FE-ADMIN-02 业务页，Vite + React 18 + TS + AntD5 + ProComponents + ECharts）

从空目录新建。**骨架（A1）**：package.json/vite.config.ts（dev proxy /v1→:8080）/tsconfig(strict, @/ 别名)/index.html/src/main.tsx/App.tsx。src/services/request.ts（axios baseURL /v1，dev 注入 X-Mock-Tenant=1/X-Mock-Store=0/X-Mock-User=dev/X-Mock-Role=owner，解 envelope code===0 取 data，errcode 中文映射；prod 预留 Authorization 注入位，BE-A-01 真登录延后）。src/layouts/BasicLayout.tsx（ProLayout 侧边栏）。src/routes/menu.tsx（8 菜单）。src/types/api.ts（全域类型骨架，PascalCase/snake_case 已标注）。src/components/{PageLoading,ErrorBoundary,EmptyState,Money,PlaceholderCard}。src/utils/format.ts。10 占位页。

**业务页（A2a + A2b 并行填充）**：
- `pages/catalog/products` — 商品 ProTable + 新增 ModalForm + 改价 PATCH + 条码精确查/名称模糊（Product PascalCase）。
- `pages/inventory/stock` — 批次库存|出入库流水双 Tab + 入库 ModalForm + FIFO 消耗扣减 + 临期标记（从 Movement.Note 解析；后端无 list-batches 接口，流水侧面还原批次，临期看板待后端补）。
- `pages/membership/members` — 会员 ProTable 后端真分页（B1 ListMembers）+ 画像 Drawer（getMember）+ 储值充值 ModalForm + 会员码下发/核销（memberDTO snake_case）。
- `pages/dashboard` — 经营看板：DatePicker + 4 Statistic 卡（营业额 GrossCents/订单数 OrderCount/实收 NetCents/客单价）+ ECharts 热销 Top5 条形图 + 近 7 日营业额趋势（并发 7 次 dailySummary）。
- `pages/marketing/coupons` — 券模板 ProTable + 新建券 ModalForm（按 Type 动态字段）+ 上线/下线/终止（PATCH status）+ 发券（issue；毛利护栏 60001 toast）。
- `pages/aiengine/strategies` — 今日 3 件事（today）+ 策略 ProTable + 策略 Drawer（items）+ 一键执行（execute，200-as-data 按 ExecResult.status 三分支 executed/blocked[reason 中文归因]/failed）+ 归因看板 ECharts（核销率/成团率/复购率/到店增量）。

services 层：catalog.ts/inventory.ts/membership.ts（A2a）+ billing.ts/marketing.ts/aiengine.ts（A2b），各 service 局部类型自包含（不改公共 types/api.ts 避免并行冲突）。

**验证**：`npx tsc --noEmit` **0 错**（strict + noUnusedLocals/Parameters 全开）+ `npm run build` 成功（4725 模块）。A2a/A2b 合并后 tsc 0 错（A2a 报告的 Money import 隐患在 A2b 最终文件中已 import 齐全）。

### 18.4 端到端冒烟（主线程）

后端 `go run ./cmd/api/` 起服（dev，无 PG/Redis 走 memory）：
- `GET /healthz` → {status:alive}
- `GET /v1/boss/whoami`（X-Mock-Tenant:1）→ source=merchant, tenant_id=1
- `POST /v1/boss/membership/members`（创建会员）→ memberID 9001
- `GET /v1/boss/membership/members?page=1&size=10` → {items:[memberDTO], total:1}（B1 新接口闭环）
- `GET /v1/boss/catalog/products` → {count:0, items:[]}（admin-web 商品页用）
- `GET /v1/boss/billing/summary/today` → DailySummary（GrossCents/NetCents/TopProducts，admin-web 看板字段对齐）

### 18.5 显式延后（不在本轮）
- ~~**BE-A-01 真实登录 + RBAC**~~：**已在 M5 落地（§19）** — boss 手机号+密码 / admin 账号+密码 bcrypt 登录、`GET /v1/admin/me` 下发 permissions.menus、`RequireScene/RequireRole` RBAC 中间件、admin-web 改读 localStorage admin_token 注入 Authorization（dev 未登录回落 X-Mock-* 头）。
- **平台运营跨租户管理后台**：`/v1/admin/*` M5 已加 login/me/logout（§19），但跨租户商户入驻/审核/封禁/对账仍缺；`internal/tenant/` 空目录；需新建独立 `/v1/admin/*` 业务路由 + 跨租户 service（不能简单绕过 EnforceTenant，破坏 ARCH-01）。
- **后端缺口**：inventory `GET /boss/inventory/batches`（批次列表，临期看板真闭环）；groupbuy 独立 desc/原价字段（现降级 desc=title、original=团购价）；C端储值充值接口（balance 页充值入口）；C端订单详情单条接口；catalog 真分页 total（现 count=本次条数）。
- **真 PG/Redis 联调**：全 pgxmock + memory，main.go 切换已就位。
- **admin-web 构建优化**：单 chunk 3.2MB（echarts+antd 全量），待 manualChunks 拆分。
- consumer-mp `member-qr` 组件仍文本占位 nonce（后端返二维码图片 URL 后改 image）。

## 19. M5 生产化三缺口：真登录+RBAC / 真实密钥接入 / worker+cron（2026-07-30）

一次性补齐三类生产化硬缺口。执行模式：**主线程独占共享文件**（config / bootstrap / main.go / router / Dockerfile / compose / CONTRACT），**agent 独占隔离包**（Phase1 四并行：auth/V3payment/cos/jobqueue；Phase3 两并行：cron/worker）。`go build ./... && go test -race ./...` 全绿（45+ 包），gofmt/vet clean。

### 19.1 共享地基（Phase 0，主线程）
- `pkg/config/types.go`：新增 `PaymentConfig`(+`Configured()`)、`JobQueueConfig`、`SchedulerConfig{RunInAPI}`；`MultitenancyConfig` 加 `PlatformTenantID int64`（admin 账号所在平台租户，默认 1）。根 `Config` 挂 `Payment/JobQueue/Scheduler` 字段。
- `pkg/config/config.go`：`applyAIProvidersEnv` —— viper AutomaticEnv 填不了 map，故自定义 `os.Environ()` 扫描注入：`ZG_AI_PROVIDERS_<NAME>_<BASE_URL|API_KEY|MODEL>` → `cfg.AI.Providers`、`ZG_AI_ROUTES_<TIER>_<PROVIDER|MODEL>` → `cfg.AI.Routes`（name/tier 小写化；yaml 已有值保留、env 只补/覆盖单字段）。修掉 config.prod.yaml「声称 env 注入但无代码」的缺口。`SetDefaults` 补 payment.timeout / job_queue.* / scheduler.run_in_api=true / platform_tenant_id=1。
- `migrations/000017_user_password.{up,down}.sql`：`users` 加 `password_hash TEXT`（nullable，兼容存量；非空校验在 service 层）。bcrypt 存储。

### 19.2 BE-A-01 真登录 + RBAC（Phase 1 A1，密码 bcrypt）
- `internal/auth`：`Account` 模型 + `AccountRepo`（`FindByPhone`/`FindByUsername`/`TouchLogin`）memory+pg 双实现；`password.go`（`HashPassword`/`VerifyPassword`，bcrypt cost=10）；`boss_login.go`/`admin_login.go`（`AuthService.BossLogin/AdminLogin`，经 `WithAccountRepo` 注入账号库，复用 `jwt.Manager.Sign` + `SessionStore.IssueRefresh`，签 `Scene=boss|admin`、`Subject="u_<id>"`、带 TenantID/StoreID/Role/Version 的 JWT，返回既有 `LoginResult`）；`rbac.go`（`RoleMenus`/`MenusFor`/`MeResult`/`BuildMeResult`）。
- **登录失败三态折叠**：wrong-password / not-found / 空 hash 全部 → `ErrLoginFailed`（40102/401），不泄漏用户存在性。登录端点从 body 取凭证三元组（此时无已认证身份）；登录后所有业务请求 tenant 一律来自 JWT（`EnforceTenant`），绝不取 body（与 ARCH-01 串户证明一致）。AdminLogin 强制 StoreID=0。
- **约定**：`phone_hash = hex(SHA256("zgjs-phone-v1" + phone))`（users 无行级盐，用全局 pepper；PG 写/查与 memory seed 同函数）。admin 用 `name` 列做 platform_ops 查找（users 无 username 列；`FindByUsername` 是后续「加唯一 username 列」migration 的接缝）。users 暂无 version 列 → `Account.Version` 恒 0（JWT ver 校验当前 no-op，文件头已标注）。
- `interfaces/http/handler/{boss_auth,admin_auth}.go`：`POST /v1/boss/auth/login{tenant_id,phone,password}`、`POST /v1/{boss,admin}/auth/logout`、`POST /v1/admin/auth/login{username,password}`、`GET /v1/admin/me`（返 MeResult：user + role + permissions.menus）。
- `interfaces/http/middleware/rbac.go`：`RequireScene(scene)`（校验 `c.Get("claims").(*jwt.Claims).Scene`，不符 40101/Abort）、`RequireRole(roles...)`（不符 40301/Abort）。
- **dev demo 账号**（memory seed）：boss owner tenant 1 / phone `13800000001` / 密码 `boss12345`；platform ops tenant 1 / name `ops` / 密码 `ops12345`。

### 19.3 router 拆组 + RBAC 接线（Phase 2，主线程）
`interfaces/http/router.go`：`wireBoss`/`wireAdmin` 拆为「登录公开组（注册在 `Use` 之前 → public）+ authed 组」。authed 中间件 `bossAdminAuthMW(deps, scene)`：`DevMock || JWT==nil` → `MockTenant`（dev 兜底）；否则 `Auth` + `RequireScene(scene)` 组合（prod 强制真 JWT + RBAC）。boss 组挂 `Scene=boss`、admin 组挂 `Scene=admin`；`GET /v1/admin/me`、`POST /v1/boss/assets`（COS 上传）挂入 authed 组。

### 19.4 微信支付 V3 完整真客户端（Phase 1 B1）
- `internal/payment/v3{wire,client,cert,notify}.go`：`NewV3Client(cfg PaymentConfig, hc) (*V3Client, error)` 装私钥（PEM 内联优先于文件；PKCS#1/PKCS#8）；`*V3Client` 实现 `SubMerchantApplier`(Apply→POST `/v3/ecommerce/applyments/`、Query→GET)、`Preparer`(Prepay→POST `/v3/pay/transactions/jsapi` 服务商特约 + JSAPI 调起参数 paySign)、`NotifyParser`(ParseNotify)。接上已写好但零调用方的 `sign.go` `SignV3Authorization`(line 61)/`VerifyV3Notify`(line 93)——不重写签名。
- **NotifyParser**：`VerifyV3Notify` 验四头 + ±5min 防重放 + RSA 验签（平台证书按 serial 缓存、惰性拉取 `/v3/certificates` + APIv3 AES-GCM 解密证书 pem）→ 通过后 AES-256-GCM（key=`APiv3Key` 32 字节）解 `resource.ciphertext`。失败一律 `ErrSignInvalid`（复用既有 sentinel），DB 不动。
- **装配**（bootstrap `payment_ports.go`）：`cfg.Payment.Configured()` → 真 V3Client（`vc.AppID = cfg.WX.AppID`，PaymentConfig 不带 appid）三端口 `WithApplier/WithPreparer/WithNotifyParser`；否则 / 初始化失败 → `FakeWxPay`（dev/测试永不联网）。
- **已知 gap（代码已标注，上线前补）**：PrepayInput.SubMerchantID 是 DB 行 id（service.go 覆写），真 V3 需 WeChat `sub_mch_id` 字符串；Apply 敏感字段未做平台证书 RSA-OAEP 字段级加密（与 FakeWxPay 平级）。
- 测试：httptest 假上游验 Apply/Prepay/Query 签名头 + 响应解析、AES-GCM 已知向量、固定 pem 证书解析、notify 全链路（自签平台证书 + 篡改/过期 → ErrSignInvalid）。17 测试。

### 19.5 COS 客户端（Phase 1 B2，纯 stdlib）
- `pkg/cos/{sign,client,upload}.go`：腾讯云 COS（S3 兼容）裸 `net/http` + HMAC-SHA1（`q-sign-algorithm=sha1`），不引 `tenc-cos-go-sdk`（与 `pkg/wxsdk` 纯 HTTP 范式一致）。`PutObject(ctx,key,contentType,r) (url,error)`（host+content-type 参与签名）、`SignedURL(ctx,key,expiry)`（临时 URL 方案）。`Client.WithBaseURL/WithHTTPClient` 便于 httptest。
- `interfaces/http/handler/asset.go`：`POST /v1/boss/assets`（multipart `file`，≤10MB）→ COS PutObject → `{url}`；cos 为 nil（未配置）→ `ErrInternal`「COS 未配置」（nil-safe）。
- 测试：httptest 假对象存储验 PUT 签名头自洽往返 + 非 2xx 报错 + SignedURL 自洽 + baseURL 解析 + nil client。

### 19.6 Redis Stream jobqueue + worker + cron（Phase 1 C1 / Phase 3）
- `pkg/jobqueue/queue.go`：`Producer.Enqueue(ctx, Job)`（XADD，MAXLEN ~ N）+ `Consumer.Run(ctx, Handler)`（XREADGROUP consumer group，先读 id="0" 本 consumer pending 做重投递、再 ">" 阻塞读新；成功 XAck+XDel，失败不 ACK 留 pending 下轮重投；腐坏 entry 丢弃防毒丸热循环）。`Job{Type,Payload,TenantID,RunAt}`；`JobPregenIncremental`/`JobReachDueSend`。**nil-safe**：client==nil → Producer no-op 日志、Consumer 不启动（dev 无 Redis）。收 `redis.Cmdable`（prod 注入 `redisx.Client`，测试注入 miniredis）。
- **reach scheduled-send**（Phase 2）：`reach.SendInput.ScheduledAt`；`Send` 早分支——未来时间 + 已接 `ScheduledSendProducer` → 入队 `JobReachDueSend`（payload=`reach.ScheduledSendPayload{In}`，tenant 取自 ctx）并返回 transient pending task（**不落库**，queue 的 pending 重投递即 at-least-once 保证）；无 producer（dev）→ 回落即时发。worker 解码后清空 ScheduledAt 即时发。bootstrap `reachScheduledProducer` 适配 `jobqueue.Producer`→`reach.ScheduledSendProducer`（仅 rdb!=nil 时接）。**设计取舍**：定时发送由 jobqueue worker 消费驱动，cron 不做 DB sweep（故未加 `reach.ListDue`，避免 dead code）。
- **worker**（`cmd/worker/main.go`）：`bootstrap.BuildServices` → `jobqueue.NewConsumer(cmdable, cfg.JobQueue, dispatcher)`；dispatcher 分发：`pregen_incremental` → `svc.Scheduler.RefreshTenant(ctx, job.TenantID)`（新公开方法，跑单租户 pipeline + 覆写双缓存，带重试）；`reach_due_send` → 解码 `ScheduledSendPayload`、清 ScheduledAt、tenant ctx → `reach.Send`；未知 type → 日志 + nil（丢毒丸）。无 Redis → 日志 + 干净退出码 0。SIGINT/SIGTERM 优雅退出。
- **cron**（`cmd/cron/main.go`）：`bootstrap.BuildServices` → pregen 4 条经 `go svc.Scheduler.Start()`（Scheduler 自有 cron loop，含 batch/incremental/attribution/bandit）；另起一个 `*cron.Cron`（Asia/Shanghai）注册：groupbuy.finalize `*/5 * * * *`（per-tenant `ActiveTenantIDs`→SourceSystem ctx→`Groupbuy.FinalizeExpired`）、subscription.expire `0 6 * * *`（全局 `Subscription.ExpireDue`，新增方法：interface+Memory+PG+Service，`period_ends_at<now` 的 active→expired）、dataquality `0 1 * * *`（per-tenant BE-A-08 `ReverseDailySales` over inventory sale_out + BE-A-09 `ComputeTags`，**compute+log，落库为后续**）。
- **subscription.ExpireDue**（Phase 3，主线程）：`SubscriptionStore` 接口加性新增 `ExpireDue(ctx, now) (int, error)`（全局，不按租户）；MemoryStore 遍历、PGStore `UPDATE ... WHERE status='active' AND period_ends_at < $1`；`Service.ExpireDue(ctx)` 用 service clock。
- **真 tenant 枚举器**（Phase 3，主线程）：bootstrap `dbTenantEnumerator` 读 `tenants WHERE status='active' AND deleted_at IS NULL`（DB 在场）；dev 回落空 stub。驱动 pregen 03:00 batch + cron per-tenant jobs。

### 19.7 共享 bootstrap 组合根（Phase 2，主线程）
- 新 `internal/bootstrap/`：`BuildInfra(ctx,cfg,log)`（db pool + redis client + ES256 JWT manager + session store，均可 nil 降级）+ `BuildServices(ctx,cfg,log,infra) (*Services,error)`（全量 repo/service/AI pipeline/pregen scheduler/payment ports/COS/auth+AccountRepo/jobqueue producer）。`cmd/api/{aiengine_adapters,payment_adapters}.go` 的反腐适配器全部迁入 `bootstrap/adapters.go`。`Services` 暴露 HTTP Deps 全字段 + `Scheduler/Tenants/JobProducer/COS/RDB`。
- `cmd/api/main.go` 瘦身为：config → BuildLogger → BuildServices → `cfg.Scheduler.RunInAPI` 时 `StartScheduler()` → `httpserver.New(Deps{...})` → serve + graceful shutdown（`svc.Close()`）。
- **prod 不双跑 pregen**：`config.prod.yaml` `scheduler.run_in_api=false` + compose backend 设 `ZG_SCHEDULER_RUN_IN_API=false`，cron 二进制独占。

### 19.8 部署 + 前端 + 配置（Phase 4，主线程）
- `backend/Dockerfile`：构建五二进制（api/migrate/worker/cron/llmbench）到 `/out/` 并 COPY 到 runtime。
- `infra/compose/docker-compose.prod.yml`：加 `backend-worker`（entrypoint `/out/worker`）、`backend-cron`（entrypoint `/out/cron`）服务，同镜像、`env_file ../../.env`、`ZG_ENV=prod`、`depends_on postgres/redis service_healthy`、无端口/healthcheck（纯循环）、`restart: unless-stopped`；backend(api) 加 `ZG_SCHEDULER_RUN_IN_API=false`。
- `configs/config.prod.yaml`：加 `payment`/`job_queue`/`scheduler`/`multitenancy.platform_tenant_id` 段；修正 ai providers/routes 注释为真实 env 格式（`ZG_AI_PROVIDERS_<NAME>_<FIELD>` / `ZG_AI_ROUTES_<TIER>_<FIELD>`）。
- `.env.example`：prod 段加 `ZG_PAYMENT_*`/`ZG_JOBQUEUE_CONSUMER`/`ZG_SCHEDULER_RUN_IN_API`/`ZG_MULTITENANCY_PLATFORM_TENANT_ID` + AI routes env 示例。
- `apps/admin-web`：`request.ts` 改 token 优先（localStorage `admin_token` → `Authorization: Bearer`）、dev 未登录回落 `X-Mock-*`（`import.meta.env.DEV`）、401/40101 清 token + 跳 `/login`；`pages/login/index.tsx` 真登录表单 → `POST /v1/admin/auth/login` → 存 token → 跳看板（tsc 0 错）。

### 19.9 显式延后（不在 M5）
- **pregen 单店解析缺口（既有，非 M5 引入）**：`pregen.runAndCache` 只往 ctx 注入 TenantID（无 StoreID），而 `pipeline.Run` 从 ctx 取 storeID（缺省 0）、`strategy.SaveAdvice` 要求 StoreID>0（否则 ErrParam）→ 按租户的 pregen（batch/incremental cron 与 worker 的 `pregen_incremental`）端到端会报 `strategy: invalid param`。此前未暴露：pregen 测试用 fake PipelineRunner、dev tenant 枚举为空。worker dispatcher 行为正确（报错→重投递）；`reach_due_send` 端到端可用。修法：pregen 按 (tenant, store) 枚举门店并把 StoreID 注入 pipeline ctx（或解析主门店）——属 AI 引擎设计变更，独立里程碑处理。当前无自动 producer 投递 `pregen_incremental`，故不会实际空转。
- WX V3：PrepayInput 透传真 WeChat `sub_mch_id`（现为 DB 行 id）；Apply 敏感字段平台证书 RSA-OAEP 字段级加密。
- reach：定时发送的「落库可见 + cron DB sweep 兜底」（现为 jobqueue 驱动、enqueue 不落库；queue PEL 即重投递保证）。
- dataquality：fused daily-sales 落库、tag 落产品、`DaysSinceLastSale`/`EarliestExpiry` 精确化（现 compute+log，仅高毛利 tag 有效）。
- users `version` 列（改密/踢线 ver 失效）；admin 唯一 username 列；`internal/tenant` 跨租户平台运营后台。
- 真 PG/Redis/WX/COS/LLM 密钥联调（本机构建真实集成代码 + env 注入，dev/测试用 Fake/httptest/miniredis/pgxmock）。

## 20. M6 五缺口收尾：pregen 单店解析 / 密钥诊断 / reach 定时落库 / dataquality 落库 / users version+username（2026-07-30）

关闭 §19.9 显式延后清单中的五项（多 agent 并行实现 + 主线程集成；`go build ./...` 54 包、`go test -race ./...` 全绿、gofmt/vet clean）。新增 migration **000018**（users）与 **000019**（daily_product_sales）。

### 20.1 pregen 单店解析缺口（§19.9 第 1 项，修复）
- 根因：`pregen.runAndCache` 只往 ctx 注入 `TenantID`，`pipeline.Run` 从 ctx 取 `StoreID`（缺省 0），`strategy.SaveAdvice → storeScope` 拒绝 `StoreID==0`（ErrParam）→ 按租户的 pregen 端到端失败。
- 修法（`internal/aiengine/pregen/`）：新增反腐端口 `StoreEnumerator{ ActiveStoreIDs(ctx, tenantID) ([]int64, error) }` + `WithStoreEnumerator` Option（**nil = 旧行为**：单次 storeID=0 跑，向后兼容 M3-S2/S3 调用方）。`runAndCache(ctx, tenantID, storeID)` 现把 `multitenancy.Context{TenantID, StoreID, Source: SourceSystem}` 注入 ctx；新 `runTenantStores` 按店枚举逐店跑（各带 retry），枚举器 nil/空/报错时回落单次 storeID=0（弹性）。`BatchGenerate`/`IncrementalRetry`/`RefreshTenant` 全走 `runTenantStores`；`IncrementalJob` 加 `StoreID`（0=全店，>0=定向单店）。缓存键仍用管线输出的 `strat.StoreID`。
- 装配（主线程）：`bootstrap/adapters.go` 新 `storeEnumeratorAdapter{ *store.Service }`（`ListStores` 从 ctx 取租户，适配器注入 system ctx）；`buildAIEngine` 签名增 `storeSvc *store.Service`，`NewScheduler` 增 `pregen.WithStoreEnumerator(...)`。worker `RefreshTenant(ctx, tenantID)` 无需改，自动按店扇出。
- 行为变化：接入后 pregen 每租户跑 N 次（每活跃店一次），批量耗时/LLM 调用随店数线性增长（worker 上限仍约束并发）。测试：6 个新用例（per-store 跑、nil 回落 store0、零店回落、RefreshTenant per-store、定向/全店 incremental）。

### 20.2 真密钥联调诊断（§19.9 第 6 项，可观测性补强）
- 配置加载与 real-vs-fake 门控在 M5 已就绪（`PaymentConfig.Configured()`→V3Client 否则 FakeWxPay；COS 缺凭证→nil；AI 无 api_key→placeholder gateway 降级 rule-only）。本项补**启动诊断**：新 `internal/bootstrap/diagnostics.go` `LogClientDiagnostics(log, cfg, database, rdb)` + 纯函数 `ClientStatus(cfg, hasDB, hasRedis) map[string]bool`，对 postgres/redis/wx_miniprogram/cos/llm/wxpay_v3 各输出一条 `active=true/false + detail` 结构化日志（**不打密钥**，仅在场与否 + provider 名）。主线程在 `BuildServices` infra 就绪后调用一次，覆盖 api/worker/cron 三二进制。`.env.example` 补全 wx/ai/cos/payment/jwt/job_queue/scheduler 的 `ZG_*` 占位。
- 已知小不一致（保留）：cos `active` 按 SecretID+SecretKey+Bucket 判定，`buildCOS` 亦接受 Endpoint 代替 Bucket（极少数有 endpoint 无 bucket 时诊断报 inactive 但仍建客户端）。

### 20.3 reach 定时发送落库可见（§19.9 第 3 项，修复）
- 根因：`Send` 的 deferred 分支入队 Redis 但**不落 ReachTask 行**，定时发送在投递前 `ListTasks` 不可见。
- 修法（`internal/reach/`）：deferred 分支**先** `CreateTask` 一条 `status=pending`（带 `ScheduledAt`）行，再入队；`ScheduledSendPayload` 加 `TaskID`，payload 携带该行 id。入队失败则 best-effort 把该行翻 `failed`（不留悬挂 pending）并返回入队错误。新 `Service.SendFinalize(ctx, in, taskID)`：`taskID==0`→CreateTask 新行（用户侧 `Send` 与旧 job），`taskID>0`→`UpdateTask` 收尾既有行；`Send == SendFinalize(ctx,in,0)`。`ReachRepo` 加 `UpdateTask(ctx, *ReachTask)`（Memory+PG 双实现，`UPDATE ... WHERE id AND tenant_id` 保隔离）。worker `handleReachDueSend` 解码 `payload.TaskID`→`SendFinalize(..., taskID)` 更新同一行不重复。主线程 `reachScheduledProducer.EnqueueScheduledSend` 增 `taskID int64` 参并写 `payload.TaskID`（**破坏性接口变更**，必须同步改否则 worker/api 链接失败）。
- 设计取舍（有意）：合规/freq/unsub/quota 仅在**预约时**校验一次，worker 只记投递结果（sent/failed）；若产品要求投递时复检（如 honoring 迟到退订）属后续决策。旧 `TaskID==0` 队列 job 回落 CreateTask 新行。

### 20.4 dataquality 落库（§19.9 第 4 项，修复）
- migration **000019** `daily_product_sales`（`tenant_id` FK + `UNIQUE(tenant_id,store_id,product_id,sale_date)` 用 `CREATE UNIQUE INDEX`、`store_id DEFAULT 0`=全店融合口径不加 FK、`CHECK qty>=0`、`updated_at` 触发器）。
- `internal/dataquality/`（保持纯函数包，不引 pgx 于既有文件）：新端口 `SalesRepo{ UpsertDailySales; LastSaleDate }` + `TagWriter{ SetProductTags }`；`MemorySalesRepo`（dev/测试）+ `PGSalesRepo`（`daily_sales_pg_repo.go`，pgxmock 测试，`ON CONFLICT` upsert 幂等）。
- `internal/catalog/`：`ProductRepo` 加 `UpdateTags`，`Service.SetProductTags(ctx, tenantID, productID, tags)`（**直接满足 `dataquality.TagWriter`**，无需适配器；`products.tags` 为 `TEXT[]` 直存）。
- `cmd/cron/jobs.go`：`dataqualityTenant` 反推后 `UpsertDailySales` 落库；`DaysSinceLastSale` 改为从刚落库的融合销量经 `LastSaleDate` 实算（**滞销 tag 现可真实触发**）；tag 集变化时 `SetProductTags` 写回。jobs struct 新 `dailySales/tagWriter` 字段（nil-guarded，未接则 compute+log no-op）；registerJobs 按 memory↔pg 惯例自 `svc.DB`/`svc.Catalog` 装配。
- 文档化限制：`临期`（EarliestExpiry）暂未触发——`inventory.ListNearingBatches` 按店、全店 storeID=0 sweep 拿不到逐商品最早批期，需全店批期端口；无销售记录的商品 `DaysSinceLastSale=0`（保守，不误报滞销）；tag 集整体替换（tags 列即自动 BE-A-09 集）；融合销量尚未喂 AI 管线（既有后续）。

### 20.5 users version + 唯一 username（§19.9 第 5 项，修复）
- migration **000018**：`users.version BIGINT NOT NULL DEFAULT 0`（token 失效版本号）+ `users.username TEXT` + 部分唯一索引 `idx_users_tenant_username ON (tenant_id, username) WHERE deleted_at IS NULL AND username IS NOT NULL`（`CREATE UNIQUE INDEX`，与 000001 phone_hash 一致）。
- `internal/auth/`：`Account` 加 `Username`；PG repo `sqlAccountByPhone/Username` 改 SELECT 真 `version`（不再 `0 AS version`），`FindByUsername` 改匹配 `username` 列（不再复用非唯一 `name`）；`AccountRepo` 加 `BumpVersion(ctx, id) (int64, error)`（`UPDATE ... version+1 RETURNING version`，Memory+PG 双实现，为改密踢线提供钩子）；memory repo 加 username 索引 + `SeedAccountWithUsername`（平台运营 seed 用户名 `ops`，`AdminLogin("ops",...)` dev 不破）。
- 后续：调用 `BumpVersion` 的改密流程；JWT 中间件比对 `ver` claim（Version 现为活值，校验仍 no-op 待中间件接入）；PG 平台运营 seed 行需填 username。

### 20.6 仍延后
- **reach 定时发送时机强制**（新发现，非本次范围）：`jobqueue.Job.RunAt` 仅为观测性 hint，Redis Stream 消费端 `XREADGROUP` 立即读取、无 ZSET 延迟投递——"定时"消息 worker 一拿到即发。20.3 解决"落库可见"，"按时投递"需给 `pkg/jobqueue` 加延迟队列（共享件，独立里程碑）。
- §19.9 余项：WX V3 真 `sub_mch_id` 透传 + Apply 字段级 RSA-OAEP；`internal/tenant` 跨租户平台运营后台。
