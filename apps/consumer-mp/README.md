# consumer-mp · 掌柜军师消费者微信小程序 (FE-MP-01 骨架)

> 里程碑 **FE-MP-01**（见 `掌柜军师-技术方案/00_总索引与WBS排期.md` + `09_前端三端架构.md` §9.2）。
> 骨架 = 路由/页面/组件/状态/请求全链路可跑通；鉴权真实，业务数据 mock。

## 技术栈与约束

- **原生微信小程序 + TypeScript**，零 npm 依赖，2 空格缩进。
- **无 tsconfig.json**：TS 编译靠微信开发者工具 `project.config.json` 的 `useCompilerPlugins:["typescript"]`（同 merchant-mp）。
- 暖色零售主题：主橙 `#FF6B35` / 背景 `#FFF9F5` / 辅黄 `#FFB627` / 成功绿 `#2ECC71`。
- 全局样式在 `app.wxss`（基础 class 沿用 merchant-mp + C 端独有 class）。
- libVersion `3.0.0`，`lazyCodeLoading:"requiredComponents"`。

## 目录结构

```
apps/consumer-mp/
├── app.json / app.ts / app.wxss      # 路由+tabBar / 入口(Page劫持埋点) / 全局样式
├── project.config.json / sitemap.json
├── assets/                            # tabBar 图标
├── types/api.d.ts                     # C 端共享类型 (镜像后端 json tag,骨架期手写校准)
├── utils/    request(JWT+401续期) / auth / tracker(埋点) / format
├── stores/   store(轻量响应式内核) / user / tenant
├── services/ auth(真实) / member / coupon / order / group-buy (后四 mock fallback)
├── pages/    index(首页) / join(领券入会) / member-code(会员码) / coupons(券包) / profile(我的)
└── components/ member-qr / coupon-card / empty-state
```

## 页面与 tabBar

| tabBar | 页面 | 说明 |
|---|---|---|
| 首页 | `pages/index` | hero 两态（未登录=领券 CTA / 已登录=欢迎+会员码入口）、今日福利、拼团、附近门店、进群 |
| 券包 | `pages/coupons` | 三 tab（未使用/已使用/已过期）+ coupon-card + 下拉刷新 |
| 我的 | `pages/profile` | member-header 两态 + 积分/储值/手机号 + 九宫格菜单 |
| — | `pages/join` | 领券入会（转化核心，见下） |
| — | `pages/member-code` | 动态会员码 + 30s 倒计时刷新 + 截屏检测 |

## 鉴权（真实）

- `POST /v1/wxshop/auth/login`：`wx.login` 拿 code → `{code, tenant_id}` → 返回 `access_token/refresh_token`（后端 M0-S2 已就绪）。
- 请求层 `utils/request.ts` 统一注入 `Authorization: Bearer`、`X-Client: consumer-mp`、`X-Trace-Id`。
- **401 / errcode 10001/10002 → 静默 refresh（`POST /v1/wxshop/auth/refresh`，`X-Refresh-Token` 头）→ 只重发一次**（`retried` 闭包标志防死循环）；refresh 也失败则清空 token 强制重新登录。
- token 存 storage：`zgjs_access` / `zgjs_refresh`。
- 并发去重：多请求同时触发 login/refresh 复用同一 promise。
- 幂等 GET 网络失败自动重试 2 次（800ms 退避）；写操作不重试。

## 领券入会转化漏斗（join 页）

`join_view`（曝光）→ `join_phone_auth`（手机号授权通过）→ `join_success`（全链路完成）。
全链路：协议勾选校验 → `getPhoneNumber` → `wx.login` → `auth.login` → `getMyProfile` → `claimNewMemberCoupon` → `requestSubscribeMessage`（模板 ID 占位 TODO）→ 成功态。

## 状态管理

`stores/store.ts` 提供零依赖 `createStore` 响应式内核（`get/set/subscribe`，`subscribe` 返回取消函数）。
对齐技术方案 §9.2.4 接口形状——后续 npm 构建就绪后可平滑替换为 `mobx-miniprogram`，页面订阅范式不变。

## 后端 C 端接口待补清单（FE-MP-01 之后）

| 待补端点 | 当前状态 | 切换方式 |
|---|---|---|
| `POST /v1/wxshop/member/code`（动态会员码） | mock | `services/member.ts USE_MOCK=false` |
| `GET /v1/wxshop/member/profile`（会员聚合） | mock（identity 取自真实 `auth/me`） | 同上 |
| `POST /v1/wxshop/member/claim-newcomer`（领新人券） | mock | 同上 |
| `GET /v1/wxshop/coupons?status=` | mock | `services/coupon.ts USE_MOCK=false` |
| `GET /v1/wxshop/groupbuys/active` | mock | `services/group-buy.ts USE_MOCK=false` |
| `GET /v1/wxshop/orders` | mock（空） | `services/order.ts USE_MOCK=false` |
| `POST /v1/track/batch`（埋点批量上报） | 后端未实现；失败回队列不丢 | 后端补接口即可，前端无需改 |

> 各 mock service 已写好真实分支（`http.get/post(...)`），置 `USE_MOCK=false` 即切换，零成本。

## 骨架期显式延后（后续里程碑）

- 订阅消息模板 ID（join 页 `tmplIds: []` 占位，待申请）。
- 二维码图片渲染（`member-qr` 组件现以 nonce 文本占位，后端返回图片 URL 后改 `<image>`）。
- 订单/储值/积分/拼团详情/进群等页（FE-MP-01 仅 5 页骨架，九宫格菜单其余项 toast 占位）。
- 多租户：进店扫码/企微活码绑定 tenantID（当前开发态默认 `globalData.tenantID=1`）。

## 本地预览

1. 微信开发者工具 → 导入 `apps/consumer-mp/`（appid 用测试号 / `touristappid`）。
2. 后端起在 `http://localhost:8080`（与 `app.ts globalData.baseURL` 一致），或改 `app.ts`。
3. 详情设置勾选「不校验合法域名」（`urlCheck:false` 已配）。
4. 首页游客可看；点「领券入会」走通登录链路即可看到会员态/会员码/券包 mock 数据。
