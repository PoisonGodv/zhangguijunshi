# 真密钥联调 Runbook（LLM / WX / COS / WxPay）

> 目标：把后端从「全降级（FakeWxPay / placeholder LLM / 无上传）」切到「真实外部集成」，
> 并用启动日志的 **client diagnostic** 逐项确认 `active=true`。
>
> 适用：`infra/compose/docker-compose.prod.yml` 起栈（也适用 local override）。
> 所有密钥**只进 `.env`（已 gitignore）或 SSM/KMS**，绝不进 `configs/config.prod.yaml`
> （那里全部是 `${VAR}` 占位，见下文「配置链路」）。

---

## 0. 配置链路（env → config → 客户端，先理解再填）

加载优先级（低→高）：`defaults` → `configs/config.prod.yaml` → `ZG_` 环境变量
（`backend/pkg/config/config.go`）。prod yaml 里敏感项全是 `${VAR}` 占位，运行时由
`os.ExpandEnv` + viper 从 env 解析，因此**填 `.env` 即可，不用改 yaml**。

启动时 `internal/bootstrap/diagnostics.go` 的 `LogClientDiagnostics` 对 6 个集成各打一行：

```
{"level":"info","msg":"client diagnostic","integration":"<name>","active":<bool>,"detail":"..."}
```

`active` 的判定与真实客户端接线 gate **完全一致**（同一份真值，见下表），所以
`active=true` 就代表该集成真的接上了真实客户端，而不是误报。

| integration      | active=true 条件（= 真实接线 gate）                                             | 不满足时的降级行为                                   |
|------------------|----------------------------------------------------------------------------------|------------------------------------------------------|
| `postgres`       | pgxpool 连上（`hasDB`）                                                          | repo 全落 in-memory                                  |
| `redis`          | redis client 连上（`hasRedis`）                                                  | session store in-memory，job queue no-op             |
| `wx_miniprogram` | `wx.app_id` 且 `wx.secret` 非空                                                  | 消费者 wx.login 降级（code2session 不可用）          |
| `cos`            | `cos.secret_id` 且 `cos.secret_key` 且（`cos.bucket` 或 `cos.endpoint`）非空     | 上传禁用，asset handler 返回干净错误（不 panic）     |
| `llm`            | ≥1 个 AI provider 的 `api_key` 非空                                              | gateway 为 placeholder，AI 走 rules + cache（无 LLM）|
| `wxpay_v3`       | `PaymentConfig.Configured()`：mch_id + api_v3_key + cert_serial + 私钥(PEM/file) | 用 FakeWxPay（dev/test，绝不联网）                   |

> 注：`cos` 的 gate 是 `bucket OR endpoint`——用标准 COS 域名填 `bucket`，用自定义域名 /
> 私有部署 / 测试假对象存储填 `endpoint`。二者有一个即可（`buildCOS` 与 diagnostic 共用
> `cosConfigured`，单一真值）。

---

## 1. LLM（AI gateway）

**需要哪些 env**（provider/route 是 map 字段，viper 填不了 map，config.go 直接扫 env，
名字/层级会被小写化进 map）：

```bash
# 至少一个 provider 有 API_KEY 即 active=true。三选一或都填：
ZG_AI_PROVIDERS_QWEN_BASE_URL=https://dashscope.aliyuncs.com/compatible-mode/v1
ZG_AI_PROVIDERS_QWEN_API_KEY=sk-xxxx                 # ← 关键，非空才激活
ZG_AI_PROVIDERS_QWEN_MODEL=qwen-turbo

ZG_AI_PROVIDERS_DEEPSEEK_BASE_URL=https://api.deepseek.com/v1
ZG_AI_PROVIDERS_DEEPSEEK_API_KEY=sk-xxxx
ZG_AI_PROVIDERS_DEEPSEEK_MODEL=deepseek-chat

ZG_AI_PROVIDERS_DOUBAO_BASE_URL=https://ark.cn-beijing.volces.com/api/v3
ZG_AI_PROVIDERS_DOUBAO_API_KEY=xxxx
ZG_AI_PROVIDERS_DOUBAO_MODEL=<你的推理接入点>

# 可选：默认 provider（空 -> 取第一个有 key 的）与分层路由
ZG_AI_DEFAULT_PROVIDER=qwen
ZG_AI_ROUTES_LITE_PROVIDER=qwen        # lite 档
ZG_AI_ROUTES_V3_PROVIDER=deepseek      # v3 档
ZG_AI_ROUTES_R1_PROVIDER=qwen          # r1 档
```

**去哪申请**：
- 通义千问 Qwen / DashScope：<https://dashscope.console.aliyun.com/> → API-KEY 管理。
- DeepSeek：<https://platform.deepseek.com/> → API Keys。
- 豆包 Doubao / 火山方舟：<https://console.volcengine.com/ark/> → API Key（model 用「推理接入点」名）。

**验证**：`client diagnostic integration=llm active=true`，detail 列出有 key 的 provider 名。

**不填的降级**：gateway 为 placeholder，`agent.EnhanceOrSkip` 降级为 rule-only（`no_provider`），
管线仍产出冷启动 / 规则建议，只是没有 LLM 增强。语义缓存 embedder 同理降级为 FakeEmbedder。

---

## 2. WX（微信小程序，消费者登录 code2session）

**需要哪些 env**：

```bash
ZG_WX_APP_ID=wx1234567890abcdef       # 小程序 AppID
ZG_WX_SECRET=xxxxxxxxxxxxxxxxxxxxxxxx # 小程序 AppSecret
# 可选：ZG_WX_CODE2SESSION_URL（空 -> 官方 jscode2session 端点）、ZG_WX_TIMEOUT（默认 5s）
```

**去哪申请**：微信公众平台 <https://mp.weixin.qq.com/> → 开发管理 → 开发设置 →
AppID(小程序ID) / AppSecret(小程序密钥)。Secret 泄露就在该页重置。
（多租户下每商户小程序各有 AppID/Secret，auth 层按 tenant 解析；POC 用一对默认值即可。）

**验证**：`client diagnostic integration=wx_miniprogram active=true`，
detail = "wx mini-program AppID + Secret present; real code2session enabled"。

**不填的降级**：消费者 wx.login 降级（无法用真实 code 换 openid）。
注意：WxPay V3 客户端的 JSAPI 起跳参数 `appId` 也取自 `wx.app_id`（`payment_ports.go`），
所以做真实支付时 WX AppID 也要填。

---

## 3. COS（腾讯云对象存储，boss 图片上传）

**需要哪些 env**：

```bash
ZG_COS_SECRET_ID=AKIDxxxxxxxx          # 腾讯云 SecretID
ZG_COS_SECRET_KEY=xxxxxxxx             # 腾讯云 SecretKey
ZG_COS_REGION=ap-guangzhou             # 存储桶地域
ZG_COS_BUCKET=zhanggui-prod-1300000000 # 存储桶名 name-appid（标准域名时必填）
# 或自定义域名/私有部署：ZG_COS_ENDPOINT=https://cos.example.com（与 BUCKET 二选一）
```

**去哪申请**：腾讯云控制台 <https://console.cloud.tencent.com/>
- SecretID/SecretKey：访问管理 CAM → API密钥管理（**建议子账号密钥，仅授该桶 COS 权限**）。
- Region/Bucket：对象存储 COS → 存储桶列表。

**验证**：`client diagnostic integration=cos active=true`，
detail = "COS SecretID + SecretKey + (Bucket or Endpoint) present; uploads enabled"。

**不填的降级**：`buildCOS` 返回 nil，上传接口返回干净错误「COS 未配置」（不 panic）。

> ⚠️ 不要把占位串（如 `__REPLACE__`）留在 SecretID/SecretKey 里——非空即会把 active 误判为
> true。未接入就留**空串**。

---

## 4. WxPay V3（微信支付服务商，顺带）

**需要哪些 env**（四项齐才 `Configured()`）：

```bash
ZG_PAYMENT_MCH_ID=1900000001                 # 服务商商户号
ZG_PAYMENT_API_V3_KEY=<32字节 APIv3 密钥>     # AES-GCM 解密 resource + 证书
ZG_PAYMENT_CERT_SERIAL=<商户 API 证书序列号>
ZG_PAYMENT_PRIVATE_KEY_FILE=/run/secrets/apiclient_key.pem  # 挂载的商户 API 私钥 PEM
# 或内联：ZG_PAYMENT_PRIVATE_KEY_PEM="-----BEGIN PRIVATE KEY-----..."（与 FILE 互斥，PEM 优先）
ZG_PAYMENT_NOTIFY_URL=https://your.public.host/v1/wxpay/notify  # 公网回调地址
# 可选：ZG_PAYMENT_PLATFORM_CERT_URL（空 -> 官方 /v3/certificates）、ZG_PAYMENT_BASE_URL（空 -> api.mch.weixin.qq.com）
```

**去哪申请**：微信支付商户平台 <https://pay.weixin.qq.com/>（服务商）→ 账户中心 →
API安全：申请 API 证书（得到私钥 + 证书序列号）、设置 APIv3 密钥。

**验证**：`client diagnostic integration=wxpay_v3 active=true`，且日志另有
"payment: real WeChat Pay V3 client enabled"（带 mch_id）。

**不填的降级**：用 FakeWxPay（确定性假实现，dev/test，绝不联网）。

---

## 5. 填完重启 + grep 日志确认（命令示例）

```bash
cd <仓库根目录>

# 1) 把上面的值填进 .env（已 gitignore）。prod 栈用 ZG_ENV=prod（compose environment 已设）。
# 2) 重建并重启 backend（env 变了必须 recreate，不能只 restart）：
docker compose --env-file .env -f infra/compose/docker-compose.prod.yml up -d --force-recreate backend

# 3) 抓启动诊断行（backend 服务名 backend / 容器 zg-backend-prod）：
docker compose --env-file .env -f infra/compose/docker-compose.prod.yml logs backend \
  | grep '"client diagnostic"'

# 期望看到 6 行，目标四项 active=true：
#   integration=wx_miniprogram active=true
#   integration=cos            active=true
#   integration=llm            active=true   (detail 列出有 key 的 provider)
#   integration=wxpay_v3       active=true
#   integration=postgres / redis active=true  (基础设施)

# 只看是否全绿（应输出 6 个 true / 0 个 false，若都接上）：
docker compose --env-file .env -f infra/compose/docker-compose.prod.yml logs backend \
  | grep '"client diagnostic"' | grep -o '"active":[a-z]*' | sort | uniq -c
```

> local 起栈（`docker-compose.local.yml` override，backend 端口 18080）把 `-f` 换成对应文件、
> 服务名不变即可。改密钥后务必 `--force-recreate`（或先 `down` 再 `up -d`），否则容器沿用旧 env。

---

## 6. 常见排查

- **llm active=false 但明明填了 key**：检查变量名拼写 `ZG_AI_PROVIDERS_<NAME>_API_KEY`
  （NAME 任意，会被小写化；FIELD 必须是 `BASE_URL`/`API_KEY`/`MODEL`）。map 字段不走 viper，
  名字错了不会报错只是静默不生效。
- **cos active=true 但上传 403**：active 只校验「凭证非空 + 有 base URL」，不校验权限。
  去 CAM 确认子账号密钥对该桶有读写权限。
- **wx active=true 但登录报 errcode**：detail 会带 WeChat errcode/errmsg；常见 40029（code 无效/过期）、
  40125（AppSecret 错）。
- **改了 `.env` 没生效**：必须 `--force-recreate` 重建容器；`docker compose restart` 不重读 env。
