# Pull Request

## 变更说明

<!-- 这条 PR 做了什么、为什么做。关联 issue / 需求号(如 closes #123)。 -->



## 测试方式

必须在 `backend/` 跑通下列命令后再请求 review,勾选确认:

- [ ] `cd backend && go vet ./...` 通过
- [ ] `cd backend && go test -race -count=1 ./...` 通过(强制开启竞态检测)
- [ ] (若改动涉及 lint 规则或被 lint 覆盖的代码) `cd backend && golangci-lint run ./...` 通过
- [ ] (若改动接线 / 路由 / 中间件 / 多租户护栏) 真实二进制冒烟通过:`/healthz` `/readyz` `/metrics` 以及三端 `/v1/{wxshop,boss,admin}/whoami` 多租户护栏生效

<!-- 其他手工验证步骤 / 复现路径 / 截图: -->


## 多租户影响

> `tenant_id` 必须从 JWT claims / ctx 取(dev 用 `X-Mock-Tenant` 头)。仓储唯一入口是 `pkg/multitenancy` 的 `EnforceTenant(ctx)` / `TenantID(ctx)`,无 tenant 即 `ErrNoTenant`。**串户是最高优先级事故。**

- [ ] 本 PR 新增 / 修改了仓储调用或 SQL
- [ ] 本 PR 未涉及仓储或 SQL

若勾选了「新增 / 修改了仓储或 SQL」,请在下面回答(否则填 N/A):

- 所有仓储入口是否都经 `EnforceTenant(ctx)` / `TenantID(ctx)` 取 `tenant_id`(强制非空)?
- 是否存在绕过 `EnforceTenant` 的裸查询(尤其 `internal/**`)? 若有,说明原因。
- 贴出关键调用链(请求 → handler → service → repository):



## 可观测影响

<!-- 新增 / 修改了 metrics、日志字段(`trace_id` / `tenant_id` / `merchant_id` / `store_id` / `user_id`)、trace span?
     告警阈值是否需要调整? 是否新增了 PII / 敏感字段(注意:日志不输出密码 / secret,连接串须 mask)? -->



## 回滚方案

<!-- 出问题时如何快速回滚:
     - 直接 revert 本 PR 即可? 还是需要先执行 down migration(列出 `make migrate-down` 或对应 revert SQL)?
     - 是否有破坏性变更(DB schema / 配置 key / 对外 API 契约)需要协调上线 / 灰度? -->
