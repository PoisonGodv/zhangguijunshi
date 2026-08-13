# 部署操作指南（照着敲即可）

> 面向：拿到代码、要在有 Docker 的服务器上把 MVP（无支付版）跑起来并验证「能登录、能开单、能看小结」的部署者。
>
> 这是一份**操作流**文档：按顺序执行，每步带「期望输出」与「出问题怎么办」。参数手册（每个变量释义、故障排查大表）见 [deploy.md](./deploy.md)。
>
> 全程约 30–60 分钟（首次构建镜像约 5–8 分钟，不含微信资质申请等外部周期）。

---

## 你将得到什么

部署完成后：

- 后端 API（`:8080`）+ worker + cron + admin-web 后台（`:8088`）全栈 healthy。
- 一个可登录的商家账号，能在 admin-web 录商品、开单、看当日小结；店员在 merchant-mp 开单/挂会员/核销。
- C 端顾客能在小程序领券入会、亮会员码（**前提：你已申请真实微信小程序**，否则这段跑不通，见 §9）。

**MVP 不含**：在线支付、企微、对账、订阅付费墙——代码保留但配置层下架，不用管。

---

## 0. 前置条件清单（开工前逐项确认）

| 项 | 要求 | 达标 |
|---|---|:---:|
| 服务器 | Linux，已装 Docker Engine + compose v2（`docker compose version` 有输出） | ☐ |
| 端口 | 8080（API）、8088（admin-web）对外可达；或前置反代/LB | ☐ |
| 代码 | 已 clone 仓库到服务器，`cd` 到仓库根（`zhangguijunshi/`） | ☐ |
| LLM API key | 至少一个：通义千问 / DeepSeek / 豆包（AI 今日 3 件事要用） | ☐ |
| 微信小程序 AppID/Secret | C 端登录 + 订阅消息要用；**没有则 C 端和一键执行群发跑不通**（见 §9） | ☐ |

> 数据库密码、JWT 密钥、boss 登录手机号/密码都在下面用 `openssl` 现场生成或现场填，无需提前准备。微信小程序资质是外部周期（注册→实名认证→开发设置拿 AppID/Secret→申请订阅消息模板），建议先起栈、用 admin-web 验证后端，同步去申请小程序。

---

## 1. 生成密钥 + 创建 JWT 挂载文件（一次性，别提交）

```bash
# 1.1 PostgreSQL 应用密码（强随机，填进 .env 的 ZG_POSTGRES_PASSWORD）
openssl rand -base64 24

# 1.2 JWT ES256 私钥（P-256），写到 infra/compose/secrets/（已 gitignore）
openssl ecparam -genkey -name prime256v1 -noout -out infra/compose/secrets/jwt-key.pem
```

> `infra/compose/secrets/jwt-key.pem` 已被 `.gitignore`（`*.pem` + `secrets/`），不会进仓库。若你在别处生成，scp 到这个路径即可。

**1.3 创建 JWT 挂载 override 文件**（自托管部署必需，见下方说明）：

```bash
cat > infra/compose/docker-compose.jwt.yml <<'EOF'
# 部署方本地覆盖：把 ES256 JWT 私钥挂进 backend/worker/cron。
# docker-compose.prod.yml 故意不内置密钥挂载（真实 prod 走 SSM/KMS 注入），
# 自托管 Docker 部署用本文件补上这一层。路径相对本文件目录 = infra/compose/secrets/。
services:
  backend:
    volumes:
      - ./secrets/jwt-key.pem:/app/configs/jwt-key.pem:ro
    environment:
      ZG_JWT_KEY_FILE: /app/configs/jwt-key.pem
  backend-worker:
    volumes:
      - ./secrets/jwt-key.pem:/app/configs/jwt-key.pem:ro
    environment:
      ZG_JWT_KEY_FILE: /app/configs/jwt-key.pem
  backend-cron:
    volumes:
      - ./secrets/jwt-key.pem:/app/configs/jwt-key.pem:ro
    environment:
      ZG_JWT_KEY_FILE: /app/configs/jwt-key.pem
EOF
```

> **为什么需要这个文件**：`backend/internal/bootstrap/infra.go` 在非 dev 环境且 `jwt.key_file` 为空或文件不存在时**直接拒绝启动**（防止用公开的 DevKey 伪造任意租户 token）。`prod.yml` 不挂密钥（留给平台注入），所以自托管必须用 override 补上。compose 的 `volumes` 跨文件追加、`environment` 跨文件按键合并，此文件只补不覆盖。
>
> 不想新建文件也可直接用现成的 `infra/compose/docker-compose.local.yml`（同样挂了这把 key），但它会把 API 宿主端口重映射到 18080（为本机 dev api 让位）。服务器上 8080 空闲就用上面的 `jwt.yml` 更干净。

---

## 2. 填 `.env`

```bash
cp .env.example .env
vi .env
```

`.env.example` 含 dev（上半）/prod（下半）两段，docker `--env-file` 取后者覆盖前者，prod 段生效；想干净可删掉上半段。下面只列**你要改**的 prod 段关键项（结构性 host/env 已由 compose `environment:` 强制，不用动）：

```bash
ZG_ENV=prod
ZG_APP_ENV=prod

ZG_POSTGRES_PASSWORD=<步骤1.1的输出>     # 必填，compose 启动 PG 要用
ZG_POSTGRES_USER=zg                       # 默认 zg，与 compose 一致
ZG_POSTGRES_DATABASE=zhanggui

ZG_JWT_KEY_FILE=/app/configs/jwt-key.pem  # 与 §1.3 override 挂载目标一致
ZG_JWT_ISSUER=zhanggui-junshi

ZG_BOOTSTRAP_BOSS_PHONE=139xxxxxxxx       # 你的 boss 登录手机号
# ZG_BOOTSTRAP_BOSS_PASSWORD 不写进 .env！§4 用 shell 临时 export

# LLM（至少填一个 provider 才有 AI 今日 3 件事）
ZG_AI_DEFAULT_PROVIDER=qwen
ZG_AI_PROVIDERS_QWEN_BASE_URL=https://dashscope.aliyuncs.com/compatible-mode/v1
ZG_AI_PROVIDERS_QWEN_API_KEY=<你的通义千问 key>
ZG_AI_PROVIDERS_QWEN_MODEL=qwen-turbo

# 微信小程序（有就填，没有先留空，后端照起、C 端跑不通，见 §9）
ZG_WX_APP_ID=
ZG_WX_SECRET=
```

其余 prod 段保持默认（`ZG_MULTITENANCY_STRICT=true`、`ZG_MULTITENANCY_DEV_MOCK_HEADER=false`、`ZG_POSTGRES_HOST=postgres`、`ZG_REDIS_HOST=redis`、`ZG_SCHEDULER_RUN_IN_API=false` 等）。

> Redis 默认无密码（compose 内网通信）；如要加密码，须同时改 `docker-compose.prod.yml` 的 redis `command` 加 `--requirepass` 并填 `ZG_REDIS_PASSWORD`，否则 backend 连不上。MVP 建议先不加。

**红线确认**：

- `ZG_ENV=prod`（大小写/空格已在代码里归一化，但请写规范值）。
- `dev_mock_header=false`（prod 绝不开 mock 头，否则等于绕过多租户隔离）。
- `.env` 已被 `.gitignore`，**切勿 `git add .env`**。

---

## 3. 定义命令别名 + 构建镜像 + 起依赖 + 建表

**3.0 在部署 shell 里定义一次别名**（后面所有 `$DC` 复用它）：

```bash
export DC="docker compose --env-file .env \
  -f infra/compose/docker-compose.prod.yml \
  -f infra/compose/docker-compose.jwt.yml"
```

> 关掉 shell 就失效；要持久化写进 `~/.bashrc` 或部署脚本里。

**3.1 构建镜像**（首次约 5–8 分钟，backend + admin-web 两个）：

```bash
$DC build
```

**3.2 起依赖**（PG + Redis，backend 等它们 healthy）：

```bash
$DC up -d postgres redis
$DC ps          # postgres / redis 都 healthy 再继续
```

**3.3 跑迁移**（建表，一次性）：

```bash
$DC --profile migrate run --rm migrate
# 退出码 0；首次会应用 24 个迁移
```

**3.4 确认迁移版本**：

```bash
$DC --profile migrate run --rm migrate version
# 期望: version=24 dirty=false
```

**出问题**：

- `vector 扩展未装` / `permission denied for extension` → `prod.yml` 用的 `pgvector/pgvector:pg16` 已内置 pgvector，不该报；若你换自建 PG，需先 `CREATE EXTENSION vector`（要 superuser）。
- `dirty=true` → 中途失败卡住，`$DC --profile migrate run --rm migrate force <上一个干净版本号>` 解卡（详见 [deploy.md §2.2](./deploy.md)）。
- 构建慢/卡 → 确认服务器能拉 Docker Hub 镜像（`golang`/`node` 基础镜像）；国内可配镜像加速。

---

## 4. 建首个租户 + boss 账号（seed-account，一次性）

迁移只建表，**不建任何租户/账号**，也没有注册接口。必须跑一次 `seed-account`，否则谁也登不进。

```bash
# 4.1 生成 boss 强密码（只进 shell env，绝不进命令行参数/脚本/git）
export ZG_BOOTSTRAP_BOSS_PASSWORD="$(openssl rand -base64 18)"

# 4.2 跑引导（复用 migrate 服务：同镜像/同二进制/同 PG env；-e 从 shell 继承密码，
#     command 覆盖 prod.yml 的默认 ["up"]）
$DC --profile migrate run -e ZG_BOOTSTRAP_BOSS_PASSWORD --rm migrate seed-account

# 4.3 立刻抹掉 shell 里的明文
unset ZG_BOOTSTRAP_BOSS_PASSWORD
```

> `ZG_BOOTSTRAP_BOSS_PHONE` 由 §2 写进 `.env`，经 `env_file` 自动注入容器，不用再 `-e`。密码强度强制 ≥10 位且含字母+数字，`openssl rand` 生成的必过。

**期望输出**（无密码）：

```
seed-account: 引导完成 tenant_id=1(新建=true) account_id=1 phone=139xxxxxxxx role=owner
```

**出问题**：

- `缺少 env ZG_BOOTSTRAP_BOSS_PHONE` → §2 没填 `ZG_BOOTSTRAP_BOSS_PHONE`，或 `.env` 没生效。
- `密码强度不足: 至少10位` → 你若手填了弱密码；用 `openssl rand` 不会触发。
- `PG 不可达` → postgres 没 healthy，或 `.env` 的 `ZG_POSTGRES_*` 不对。`$DC ps` 看 postgres 状态。
- `账号已存在, 跳过` → 重跑幂等正常，密码**不会被覆盖**。要改密加 `seed-account --reset-password`（旧 JWT 立即失效）：
  ```bash
  export ZG_BOOTSTRAP_BOSS_PASSWORD="$(openssl rand -base64 18)"
  $DC --profile migrate run -e ZG_BOOTSTRAP_BOSS_PASSWORD --rm migrate seed-account --reset-password
  unset ZG_BOOTSTRAP_BOSS_PASSWORD
  ```
  > `-e VAR` 和 `command`（`seed-account --reset-password`）的位置：`-e` 必须在 `run` 和 `--rm migrate` 之间，`seed-account --reset-password` 跟在服务名之后覆盖 `command:`。若报 `unknown command "seed-account"` 说明位置放错了。

> boss 账号 `store_id=NULL`（JWT 里 StoreID=0），登录和鉴权不受影响；后续若某接口强依赖 store_id 再单独建店。

---

## 5. 起全栈服务

```bash
$DC up -d
$DC ps
```

**期望**：`postgres / redis / backend / backend-worker / backend-cron / admin-web` 全部 `healthy`（admin-web 靠 `depends_on: backend: service_healthy` 等 backend ready 后才起）。

**出问题**：

- backend/worker/cron 启动即退出、日志含 `production requires an available PostgreSQL database` → **prod fail-fast 安全保护**，不是 bug。查 `.env` 的 `ZG_POSTGRES_*`、postgres 是否 healthy。**别**把 `ZG_ENV` 改成 `dev` 绕过——那样会回退内存库让 dev 口令 `boss12345` 在 prod 变活。
- backend 日志含 `jwt: prod env requires jwt.key_file` → §1.3 的 override 文件没建、或 `$DC` 没带 `-f docker-compose.jwt.yml`、或 `infra/compose/secrets/jwt-key.pem` 不存在。
- admin-web 起不来 → backend 没 healthy。先解决 backend。
- 详见 [deploy.md §6 故障排查](./deploy.md)。

---

## 6. 冒烟验证（确认「能登录、能开单」）

```bash
# 6.1 后端存活/就绪
curl -s http://localhost:8080/healthz   # {"code":0,"data":{"status":"alive"}}
curl -s http://localhost:8080/readyz      # data.db = "ok"

# 6.2 admin-web 可访问
curl -sI http://localhost:8088/          # HTTP/1.1 200

# 6.3 核心验收：用刚 seed 的账号真登录
curl -s -X POST http://localhost:8080/v1/boss/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"tenant_id":1,"phone":"139xxxxxxxx","password":"<你的boss密码>"}'
# 期望 200，返回 access_token + refresh_token
```

**期望**：正确密码 200 拿 token；错误密码 401；连续 5 次错密码 429（登录限流，见 §10）。

**进阶验证**（带 token 调业务接口，确认多租户隔离就位）：

```bash
TOKEN=<上面拿到的 access_token>
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:8080/v1/boss/whoami
# 期望 200，tenant_id=1 来自 JWT claims（不是请求头）

# 不带 token 打业务接口 → 必须被拒
curl -sI http://localhost:8080/v1/boss/whoami
# 期望 401/403（prod 不开 mock 头）
```

> 浏览器访问 `http://<服务器IP>:8088/` → admin-web 登录页 → 输「租户ID=1 + 手机号 + 密码」登录，即可在后台录商品、建券、看 dashboard。admin-web 登录走 `/v1/boss/auth/login`（scene=boss），业务页全部对接 `/v1/boss/*`。

---

## 7. （可选）灌演示数据，让 dashboard 有东西看

```bash
$DC exec -T postgres \
  psql -U zg -d zhanggui < backend/migrations_seed/seed_demo.sql
```

**期望**：插入 17 个商品、12 个会员、30 笔近 7 天订单、3 张券、1 个拼团；再跑一遍不翻倍（幂等）。灌完后 admin-web dashboard 能看到当日营业额、热销榜。详见 [seed-demo.md](./seed-demo.md)。

> seed_demo 要求 tenants 非空——你已在 §4 建了租户，没问题。

---

## 8. 日常运维速查

```bash
# 查日志
$DC logs -f backend
$DC logs -f backend-worker
$DC logs --tail=200 backend

# 改了代码/镜像后滚动更新
$DC build && $DC up -d

# 单独跑迁移（升级时）
$DC --profile migrate run --rm migrate up

# 备份 PG
$DC exec -T postgres pg_dump -U zg zhanggui | gzip > zhanggui-$(date +%F).sql.gz

# 进容器排障（alpine，工作目录 /app）
$DC exec backend sh
```

> 更完整的运维（回滚、dirty 解卡、down 带不带 `-v`）见 [deploy.md §5](./deploy.md)。

---

## 9. C 端与一键执行群发（需微信小程序资质）

§1–§7 只验了「后端 + admin/商家端」。要让 C 端顾客真能领券入会、店员一键执行的群发真能发到顾客微信，还差两样外部资质：

1. **注册微信小程序** → 实名认证 → 开发设置拿 `AppID` / `AppSecret` → 填进 `.env` 的 `ZG_WX_APP_ID` / `ZG_WX_SECRET`。
2. **申请订阅消息模板**：在小程序 MP 后台「订阅消息」申请模板，拿 `template_id`，按 reach 模板码填进 env（后缀大写，lower-case 后作 map 键）：
   ```bash
   ZG_WX_SUBSCRIBE_TEMPLATES_NEW_WELCOME=<template_id>
   ZG_WX_SUBSCRIBE_TEMPLATES_BIRTHDAY=<template_id>
   ZG_WX_SUBSCRIBE_TEMPLATES_RECALL_30=<template_id>
   ZG_WX_SUBSCRIBE_TEMPLATES_RECALL_60=<template_id>
   ZG_WX_SUBSCRIBE_TEMPLATES_RECALL_90=<template_id>
   ZG_WX_SUBSCRIBE_TEMPLATES_REPURCHASE=<template_id>
   ZG_WX_SUBSCRIBE_TEMPLATES_BALANCE_LOW=<template_id>
   ZG_WX_SUBSCRIBE_TEMPLATES_POINTS_EXPIRE=<template_id>
   ```

填完后**重建**（env 变了必须 recreate，不能只 restart）：

```bash
$DC up -d --force-recreate backend backend-worker backend-cron
$DC logs backend | grep -i 'integration.*active'
# 期望: integration=wx_miniprogram active=true ; integration=llm active=true ; integration=cos active=true（若填了 COS）
```

> 不填模板映射，一键执行群发会**静默假成功**（任务标"已发送"实则零投递）——务必填了再演示群发。详见 [real-keys.md](./real-keys.md)。

---

## 10. 安全注意事项（部署者必读）

| 风险 | 现状 | 你要做的 |
|---|---|---|
| boss 密码弱被爆破 | seed-account 强制 ≥10 位+字母+数字+弱密码块表；登录限流 IP 20次/分、账号 5次/分 | 用 `openssl rand` 生成，别手填弱口令；记牢或存密码管理器 |
| 登录爆破 | 限流器进程内，单副本有效 | **多副本前必须换 Redis 共享限流**，否则 N 副本放大 N 倍额度 |
| X-Forwarded-For 伪造绕过限流 | Gin 仅信任 `172.30.0.0/24`（compose 内网 nginx）；nginx 用 `$remote_addr` 覆盖 XFF | 若在 nginx 前再加外网 LB/CDN，把 LB 出网网段加进 `backend/interfaces/http/router.go` 的 `SetTrustedProxies` 白名单 |
| prod 误配无 PG 回退内存库 | `ZG_ENV=prod` 缺 DB 直接退出，不降级 Memory | 别改 env 绕过；修 PG 配置 |
| admin-web 演示凭据泄露 | 仅 `import.meta.env.DEV` 展示，prod build 后 dist 无 `boss12345` | 别在 prod 构建关掉 DEV 剔除逻辑 |
| 密钥进 git | `.env` / `secrets/` / `*.pem` 已 gitignore | 别 `git add -f`；`docker-compose.jwt.yml` 本身无密钥可提交，但建议也加进 `.gitignore` |
| JWT 私钥泄露 | ES256 P-256 真私钥，泄露即可伪造任意租户/admin token | 私钥只放 `infra/compose/secrets/`（gitignore）；CI/CD 用 secret manager 注入 |

> 完整安全红线见 [deploy.md §1 安全红线段](./deploy.md)。

---

## 11. 最小验收清单（部署完成的标志）

全部打勾即 MVP 部署成功：

- [ ] `$DC ps` 六个服务全 healthy
- [ ] `curl /healthz` 200、`curl /readyz` 200 且 `db=ok`
- [ ] `curl /v1/boss/auth/login` 正确密码 200 拿 token、错误密码 401、连续错 5 次 429
- [ ] admin-web `:8088` 能用「租户ID+手机号+密码」登录，看到 dashboard
- [ ] admin-web 能录一个商品、建一张券（核心 CRUD 通）
- [ ] （可选）灌 seed_demo 后 dashboard 有当日营业额
- [ ] （需微信资质）`integration=wx_miniprogram active=true`，C 端领券入会走通

前三项是硬验收——通了就证明后端 + 数据库 + 鉴权 + 多租户隔离 + 限流全部就位，别人可以接手测了。
