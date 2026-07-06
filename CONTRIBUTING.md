# 贡献指南（CONTRIBUTING）

感谢参与掌柜军师！本文件规定协作约定与本地检查清单。**动手前请先读 [README.md](README.md) 与 [backend/CONTRACT.md](backend/CONTRACT.md)**——后者是后端集成的硬约束总规范。

---

## 1. 分支策略

- `main`：始终可发布、`go build ./... && go test -race ./...` 全绿。
- `feature-*` / `fix-*` / `docs-*`：从 `main` 拉出，完成后合回 `main`。
- 禁止向 `main` 直接 push；一律走 Pull Request（见下）。

```bash
git checkout main
git pull
git checkout -b feature-<short-topic>
```

---

## 2. 提交规范（Conventional Commits）

提交信息用英文 Conventional Commits 格式，便于自动生成变更日志：

```
<type>(<scope>): <subject>

<可选 body>
<可选 footer>
```

- **type**：`feat` / `fix` / `docs` / `refactor` / `test` / `chore` / `perf` / `build` / `ci`
- **scope**（可选）：受影响包或域，如 `multitenancy`、`config`、`router`、`catalog`
- **subject**：祈使句、现在时、首字母小写、不加句号

示例：

```
feat(multitenancy): add TenantGuard.Wrap helper for repositories
fix(db): mask password in pool DSN log
docs(adr): record modular-monolith + tenant-isolation decisions
chore(ci): pin golangci-lint to v2.12.2
```

> 重大破坏性变更在 footer 加 `BREAKING CHANGE:` 一行说明。

---

## 3. Pull Request 流程

1. 确保分支基于最新 `main`，本地检查（见 [第 5 节](#5-本地检查)）全部通过。
2. 在 GitHub 上发起 PR，目标分支 `main`。发起时自动套用 [.github/PULL_REQUEST_TEMPLATE.md](.github/PULL_REQUEST_TEMPLATE.md)，按模板逐项填写：
   - **变更说明**：做了什么、为什么（关联 Issue 号，如 `Closes #12`）。
   - **测试方式**：勾选确认 `go vet` / `go test -race` / `golangci-lint` 通过；若改动接线/路由/中间件/多租户护栏，须真实二进制冒烟（`/healthz` `/readyz` `/metrics` 与三端 `/v1/{wxshop,boss,admin}/whoami` 护栏生效）。
   - **多租户影响**：若触碰仓储或 SQL，**显式说明如何保证不串户**（见 [第 4 节](#4-多租户硬约定不可违背)），贴出关键调用链。
   - **可观测影响**：是否新增/修改 metrics、日志字段（`trace_id`/`tenant_id`/`merchant_id`/`store_id`/`user_id`）、trace span；是否新增 PII/敏感字段（日志不得输出密码/secret，连接串须 mask）。
   - **回滚方案**：出问题如何回滚（直接 revert / 需先 `make migrate-down` / 是否有破坏性变更需灰度）。
3. 至少一人 Code Review 通过、CI 全绿后方可合并；优先 `Squash merge`。
4. 合并后删除特性分支。

---

## 4. 多租户硬约定（不可违背）

多租户串户是本项目**第一号风险**。任何触及数据访问的改动必须遵守：

- **`tenant_id` 必从已校验上下文取**：仓储调用前先 `pkg/multitenancy.EnforceTenant(ctx)`（或 `TenantGuard.Wrap(ctx)`），取不到返回 `ErrNoTenant`，**绝不放行查询**。
- **单一 chokepoint**：`EnforceTenant` 是仓储取租户的唯一入口；不得用请求体、URL 参数或前端传值当作 `tenant_id`。
- **dev 用 mock 头，生产用 JWT**：`X-Mock-Tenant` 头**仅在 dev** 经 `MockTenant` 中间件注入（`interfaces/http/middleware/tenant.go`）；staging/prod 必从已校验 JWT claims 注入（JWT 鉴权 M0-S2 交付）。
- **静态闸门**：`tenantcheck` 分析器（`pkg/multitenancy/lint.go`）扫描 `internal/**`，标记「裸查询（`Query` / `QueryRow` / `Exec`）未先取租户」的仓储函数。新增仓储若绕过 `EnforceTenant`，**`make lint-tenant` 会报错、CI 拦截**。

```go
// 正确：先取租户，再查询
tid, err := multitenancy.EnforceTenant(ctx)
if err != nil {
    return fmt.Errorf("resolve tenant: %w", err)
}
// 用 tid 拼接 WHERE tenant_id = $1 ...

// 错误：直接裸查询，会被 tenantcheck 标记
// rows, _ := tx.Query(ctx, "select ... from orders")  // ❌ 无租户
```

> 接触数据访问层却不调 `EnforceTenant` 的 PR，一律 **Request Changes**。

---

## 5. 本地检查

提 PR 前在仓库根全部跑通：

```bash
make fmt            # go fmt ./...
make vet            # go vet ./...
make lint-tenant    # 多租户隔离闸门（含 tenantcheck）
make lint           # golangci-lint run ./...（需 v2.x）
make test           # go test -race ./...
make build          # go build ./...
```

**lint 前置**：`.golangci.yml` 用 v2 schema，须装 golangci-lint v2.x：

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
```

---

## 6. 代码风格要点（详见 CONTRACT.md）

- 第一参数必传 `ctx context.Context`。
- 错误用 `fmt.Errorf("xxx: %w", err)` 包装，保留错误链；哨兵错误用 `errors.New`。
- 日志固定字段名：`trace_id` / `tenant_id` / `merchant_id` / `store_id` / `user_id`；zap 用强类型字段（`zap.Int64("tenant_id", ...)`）。
- TTL / Timeout 用 `time.Duration`，YAML 写人类可读（`"30s"` / `"720h"`）。
- 每个包暴露 `New(cfg ...) (*T, error)`，失败返回 error 不 panic（`Must` 前缀除外）。
- **不向日志输出密码/secret**；连接串打日志要 mask。
- 每个 `package xx` 上方写 `// Package xx ...` 文档注释；交付代码须 `gofmt` 干净。
- 第三方依赖须在 `backend/CONTRACT.md` 白名单内；如需引入白名单外依赖，在 PR 里标注理由，由维护者决定。
