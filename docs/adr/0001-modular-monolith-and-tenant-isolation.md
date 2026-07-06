# ADR-0001：选 Go 模块化单体，并 Day1 内建 tenant_id 行级隔离

- **状态（Status）**：Accepted（已采纳）
- **日期（Date）**：2026-07-06（M0-S1 地基阶段）
- **决策者（Deciders）**：掌柜军师后端主线程
- **关联（Related）**：[backend/CONTRACT.md](../../backend/CONTRACT.md) §0 不可违背的硬约束；技术方案 `02_后端架构总纲.md`、`01_工程地基与DevOps.md`

## 背景（Context）

掌柜军师定位「面向三四线个体零售的 AI 营销军师 + 私域 SaaS」，目标客户是水果店、母婴店、便利店、服装店、生鲜店等个体商户。0→1 阶段团队规模 6–8 人，需要在有限人力下快速交付一个**多租户 SaaS**（一商户=一租户，租户间数据物理同行库、逻辑行级隔离）。

两个奠基问题必须在 Day1 定下来，否则后续返工成本极高：

1. **系统形态**：单体还是微服务？要不要上 K8s？
2. **多租户隔离**：何时引入？以什么机制保证不串户？

多租户串户在计划书的风险清单里被列为 **#1 风险**——一旦 A 商户看到 B 商户的会员/订单，信任崩塌、合规出问题，对小商户是致命的。

## 决策（Decision）

### 决策一：选 Go 模块化单体，0→1 禁微服务 / 禁 K8s

采用 **Go 模块化单体**（module `github.com/zhangguijunshi/backend`，go 1.25）：单进程、单二进制、单 Go module，内部按业务域分包（`internal/<domain>`），通过明确的接口边界（`pkg/` + `interfaces/`）保持模块独立，为未来拆分留出**接缝**。

理由：

- **人力匹配**：6–8 人团队，单体把部署、调试、可观测、配置的复杂度压到最低；微服务的运维/链路追踪/服务网格成本在这个规模下不划算。
- **招聘友好**：单体 Go 工程师市场供给远大于「会 K8s + 多服务网格」的稀缺岗，三四线团队更现实。
- **微信生态 SDK 全在 Go 生态可用**（微信小程序登录、支付、企微、消息推送），单体聚合更顺手。
- **1→10 预留拆分点**：域包按 DDD 风格隔离（`internal/catalog`、`internal/crm`、`internal/marketing`…），域间只走显式接口；当某域流量/团队规模够大，可沿接缝切成独立服务，而无需大改业务代码。

显式**不采用**：0→1 阶段上微服务、上 K8s。docker-compose 仅用于本地起依赖（PG/Redis/MinIO），生产用更轻量的部署方式起步。

### 决策二：Day1 即内建 `tenant_id` 行级隔离，三层防护

多租户隔离不延后、不「先单租户后补」。从第一行业务代码起，`tenant_id`（== `merchant_id`）就是所有业务表的行级隔离键（见 `backend/migrations/000001_init_tenant_user_store.up.sql`：`users` / `stores` 显式带 `tenant_id BIGINT NOT NULL REFERENCES tenants(id)`）。

防护分三层，互为冗余：

1. **运行时单一入口（chokepoint）**：`pkg/multitenancy.EnforceTenant(ctx)`（与 `TenantID` 同义、`TenantGuard.Wrap` 同效）是仓储取租户的**唯一**入口；上下文无租户即返回 `ErrNoTenant`，**绝不放行查询**。HTTP 层把租户注入 `context.Context`，仓储只认 ctx。
2. **静态分析闸门**：`tenantcheck` 分析器（`pkg/multitenancy/lint.go`，`go/analysis` 实现）扫描 `internal/**`，标记「函数体内出现裸查询（`Query` / `QueryRow` / `Exec`）却未先调 `EnforceTenant` / `TenantID` / `MustFromContext` / `TenantGuard.Wrap`」的仓储函数，CI 经 `make lint-tenant`（`go vet ./...`）执行——**在 CI 阶段就拦下疏漏**，而不是等运行时。
3. **串户测试矩阵**：`pkg/multitenancy/guard_test.go` 覆盖「无 ctx / 有 ctx / system 上下文」等路径，确保护栏在各种调用姿态下都正确拒绝。

租户身份来源：**dev** 用 `X-Mock-Tenant` 头经 `MockTenant` 中间件注入（仅 dev，staging/prod 禁用）；**staging/prod** 必从已校验的 JWT claims 注入（JWT 鉴权 M0-S2 交付）。无论哪种来源，仓储层只认 ctx 里的租户，handler 传值不被信任。

## 备选方案（Alternatives Considered）

- **微服务 + K8s**：拒绝。理由见上（决策一）。
- **schema-per-tenant 或 database-per-tenant**：拒绝。个体商户客单价低、租户密度高，schema/db 数量爆炸、迁移成本高；行级隔离 + 单 chokepoint 在本规模下更可控。
- **延后做隔离（先单租户 MVP 再补）**：拒绝。串户是 #1 风险，事后补意味着全量回溯改写数据访问层，且期间随时可能串户；Day1 内建成本最低。
- **仅靠运行时 guard，不做静态分析**：拒绝。guard 只在「代码真的执行到」时生效，新写的漏网约束能跑过测试却埋雷；`tenantcheck` 在 CI 静态阶段就把「忘记取租户」的仓储函数揪出来，与 guard 互补。

## 后果（Consequences）

**正面**：

- 0→1 团队专注业务，不被基础设施拖住；单二进制部署简单，本地一键起。
- 多租户隔离从第一天就是「默认安全」：新增仓储不调 `EnforceTenant` 直接编译/CI 失败，串户风险被结构性压低。
- 域包边界清晰，未来沿接缝拆服务时业务代码改动小。

**负面 / 权衡**：

- 单体内部必须靠纪律维持域边界（无进程隔离），需要 Code Review + 分析器共同把关。
- `tenantcheck` 是 AST 级启发式分析（不做完整类型推断），存在漏报/误报可能；靠 guard 的运行时兜底与测试矩阵补强。
- 行级隔离意味着**每条**业务 SQL 都要带 `tenant_id` 过滤，开发者负担略增——这是「默认安全」的必要代价。
- 未来若某域真的拆出，需要补 RPC/契约层，但接口接缝已预留，迁移面可控。

## 备注（Notes）

- 本 ADR 记录的是**奠基决策**，状态 Accepted，不轻易推翻；如需变更（例如决定拆服务、或改用 database-per-tenant），请新写 ADR 覆盖并标注本 ADR 被 Superseded。
- 实现细节（接口签名、`ErrNoTenant`、`tenantcheck` 规则等）以代码与 `backend/CONTRACT.md` 为准。
