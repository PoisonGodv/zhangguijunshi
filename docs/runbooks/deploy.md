# 生产部署 Runbook

掌柜军师生产环境部署操作手册。面向有 Docker + compose 的服务器；本机（开发机）无 Docker，compose / Dockerfile 作为可交付产物，实际部署在有 Docker 的服务器上执行。

> 关联文件：
> - 生产编排：`infra/compose/docker-compose.prod.yml`（由部署 agent 维护）
> - 后端镜像：`backend/Dockerfile`（构建上下文 `backend/`，五二进制 `api/migrate/worker/cron/llmbench`）
> - 后台镜像：`apps/admin-web/Dockerfile`（nginx 静态站 + `/v1/` 同源反代，避免 CORS）
> - 生产配置：`backend/configs/config.prod.yaml`（由部署 agent 维护）
> - 环境占位：`.env.example`（**仅占位，不含真实密钥**）

---

## 0. 前置条件

| 项 | 要求 |
|---|---|
| 服务器 | Linux，已装 Docker Engine + compose v2 插件（`docker compose version` 可用） |
| 域名 / 证书 | 商家侧访问域名（如 `boss.example.com`）、C 端 API 域名；TLS 证书（Let's Encrypt 或腾讯云 SSL） |
| 数据库 | PostgreSQL 16 + pgvector（生产用 TDSQL-C 或自建 RDS），账号库已建好，`CREATE EXTENSION vector` 权限 |
| Redis | Redis 7 实例（生产可用腾讯云 Redis） |
| 对象存储 | 腾讯云 COS Bucket 已建（用于商品图/券图/素材） |
| 密钥来源 | 腾讯云 SSM / KMS 运行时注入；**密钥不入仓库、不进 .env 提交** |
| 镜像 | 仓库根执行 `docker build -f backend/Dockerfile -t zhanggui-junshi-backend:<tag> backend/` 与 `docker build -f apps/admin-web/Dockerfile -t zhanggui-junshi-admin:<tag> apps/admin-web/`，推到镜像仓库或在服务器本地构建 |

> 镜像构建在仓库根执行（Dockerfile 内路径相对 `backend/` 与 `apps/admin-web/` 上下文）。

---

## 1. 配置：填 `.env`

```bash
cp .env.example .env
vi .env
```

逐项填入真实值（变量前缀 `ZG_`，下划线嵌套，见 `.env.example` 与 `backend/pkg/config/types.go`）：

| 变量 | 说明 |
|---|---|
| `ZG_ENV=prod` | 选 prod 配置（读 `configs/config.prod.yaml`） |
| `ZG_POSTGRES_HOST/PORT/USER/PASSWORD/DATABASE/SSL_MODE` | 生产 PG 连接；`SSL_MODE` 建议 `require` 或 `verify-full` |
| `ZG_REDIS_HOST/PORT/PASSWORD/DB` | 生产 Redis |
| `ZG_JWT_SECRET` | **ES256 密钥**（PEM 或 SSM 托管）；prod 必填，不能留 `__FROM_SSM__` 占位 |
| `ZG_JWT_ISSUER` | `zhanggui-junshi` |
| LLM provider key | AI 网关 provider 密钥（豆包 / DeepSeek 等），按 `ai.providers` 结构以 `ZG_AI_PROVIDERS_*` 注入 |
| `ZG_WX_APP_ID` / `ZG_WX_SECRET` | 微信小程序 code2session 用（C 端登录） |
| `ZG_COS_SECRET_ID/SECRET_KEY/REGION/BUCKET/ENDPOINT` | 腾讯云 COS |
| `ZG_OTEL_ENDPOINT` | OpenTelemetry OTLP 收集器地址；prod `enable=true`、`insecure=false` |
| `ZG_MULTITENANCY_STRICT=true` | 行级隔离闸门，恒 true |

> **安全红线**：prod 配置必须 `dev_mock_header=false`（见 `config.dev.yaml` 注释，dev 才开 `X-Mock-Tenant` 头）。prod 租户身份**只能**从已校验的 JWT claims 注入，**绝不**开 mock 头，否则等同绕过多租户隔离。确认 `config.prod.yaml` 中 `multitenancy.dev_mock_header` 为 `false` 或缺省。

`.env` 已被 `.gitignore`，**切勿提交**。

---

## 2. 起依赖 + 迁移（一次性）

`docker-compose.prod.yml` 用 `--profile migrate` 起一次性 migrate 任务（init 容器语义），跑完退出。

```bash
docker compose -f infra/compose/docker-compose.prod.yml --profile migrate up
```

观察要点：
- migrate 容器退出码必须为 `0`（`docker compose ps -a` 看 migrate 服务 `Exited (0)`）。
- 日志出现 `migrate up` 成功应用 16 个迁移（`000001_init_tenant_user_store` … `000016_store_cside`）。
- `migrate` 读 `ZG_MIGRATIONS_DIR`（默认 `migrations`，相对 CWD=`/app`），与 api 共用同一套分层配置（`ZG_ENV` + `ZG_POSTGRES_*`）定位数据库。
- 若 migrate 报 `ErrNoChange` 是正常的（无待应用迁移）。

> migrate 二进制在镜像内为 `/out/migrate`（见 `backend/Dockerfile`），prod compose 通过 `entrypoint: /out/migrate` + `command: ["up"]` 复用同一镜像。

---

## 3. 起服务

```bash
docker compose -f infra/compose/docker-compose.prod.yml up -d
docker compose -f infra/compose/docker-compose.prod.yml ps
docker compose -f infra/compose/docker-compose.prod.yml logs -f backend
```

预期：
- `backend` 容器健康（HEALTHCHECK 走 `/healthz`，`interval=15s`，`start-period=30s`）。
- 后台 `admin-web` 容器（nginx）监听容器 80，宿主映射 **8088**（访问 `http://<host>:8088`；TLS 由前置网关/反代终结）。
- 后端 HTTP 默认监听 **8080**（`http.port`，`EXPOSE 8080`）。

---

## 4. 冒烟验证

```bash
# 后端存活 / 就绪 / 指标
curl -i http://<host>:8080/healthz     # 200 {"code":0,"data":{"status":"alive"}}
curl -i http://<host>:8080/readyz      # 200，data.db = "ok"（DB 通）
curl -i http://<host>:8080/metrics     # 200，Prometheus 文本

# 后台可访问
curl -i http://<host>:8088/            # 200，返回 admin-web index.html

# 登录链路（C 端微信登录）
curl -i -X POST http://<host>:8080/v1/wxshop/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"code":"<wx.login code>"}'
```

串户隔离验证（**不带租户上下文必被拒**）：

```bash
# prod 不开 mock 头，无 JWT 直接打业务端点 → 多租户护栏拒绝
curl -i http://<host>:8080/v1/boss/whoami
# 期望 403，code 40302（ErrTenantCross）

# 拿合法 JWT 后带 Authorization 头访问（租户从 JWT claims 注入，不从请求体）
curl -i -H 'Authorization: Bearer <jwt>' http://<host>:8080/v1/boss/whoami
```

> 若 `whoami` 在 prod 仍接受 `X-Mock-Tenant` 头，说明 `dev_mock_header` 没关，**立即回滚**并检查 `config.prod.yaml`。

---

## 5. 日常运维

### 5.1 滚动更新

```bash
# 构建新镜像
docker build -f backend/Dockerfile    -t zhanggui-junshi-backend:<newtag> backend/
docker build -f apps/admin-web/Dockerfile -t zhanggui-junshi-admin:<newtag>  apps/admin-web/

# 改 compose image tag 后重建
docker compose -f infra/compose/docker-compose.prod.yml up -d --build
```

### 5.2 单独跑迁移

```bash
docker compose -f infra/compose/docker-compose.prod.yml --profile migrate up
# 或直接复用镜像
docker run --rm --env-file .env --network <prod-net> \
  --entrypoint /out/migrate zhanggui-junshi-backend:<tag> up

# 回滚最近一次迁移（先确认 down SQL 安全）
docker run --rm --env-file .env --network <prod-net> \
  --entrypoint /out/migrate zhanggui-junshi-backend:<tag> down 1

# 查看 schema 版本
docker run --rm --env-file .env --network <prod-net> \
  --entrypoint /out/migrate zhanggui-junshi-backend:<tag> version

# dirty 状态解卡
docker run --rm --env-file .env --network <prod-net> \
  --entrypoint /out/migrate zhanggui-junshi-backend:<tag> force <v>
```

### 5.3 日志与排障

```bash
docker compose -f infra/compose/docker-compose.prod.yml logs -f backend
docker compose -f infra/compose/docker-compose.prod.yml logs -f admin-web
docker compose -f infra/compose/docker-compose.prod.yml logs --tail=200 backend
```

### 5.4 进入容器

```bash
docker compose -f infra/compose/docker-compose.prod.yml exec backend sh
# 镜像 runtime 是 alpine，工作目录 /app
```

### 5.5 备份 PG

```bash
# 从能连 PG 的主机（或 pg 容器 sidecar）执行
pg_dump --host=<pg-host> -U zg -d zhanggui -F c -f zhanggui-$(date +%F).dump
# 恢复：pg_restore -d zhanggui zhanggui-YYYY-MM-DD.dump
```

### 5.6 回滚（镜像 tag + 迁移 down）

1. compose `image:` tag 回退到上一个已知可用 tag，`docker compose ... up -d`。
2. 若新版本带了 schema 迁移，按 5.2 执行 `migrate down 1`（**先核对 down SQL 与数据兼容性**）。
3. 冒烟（第 4 步）复核。

---

## 6. 故障排查清单

| 现象 | 排查 |
|---|---|
| healthcheck 不通（容器反复 restart） | `docker compose logs backend`；先看 `/healthz` 是否 200（liveness 不依赖 DB）。若 `/healthz` 通但编排仍杀，多半是 `readyz` 被误用做 liveness——prod compose 的 liveness 应指向 `/healthz`，readiness 指向 `/readyz`。 |
| `/readyz` 报 `db: down` | PG 连接失败：查 `ZG_POSTGRES_HOST/PORT/USER/PASSWORD`、网络可达、SSL_MODE 是否对。后端 `db pool init failed` 时会降级为无 DB 模式继续服务（`db: skipped`），prod 不允许 `skipped`。 |
| migrate 失败退出码非 0 | 看 migrate 日志；常见：PG 不可达、`vector` 扩展未装（`000001` 之前的 init.sql `CREATE EXTENSION vector` 需 superuser）、迁移脏状态（`migrate force <v>` 解卡）。 |
| CORS 报错（admin-web 调 API） | admin-web 走 nginx 同源反代 `/v1/ → backend:8080`（见 `apps/admin-web/nginx.conf`），正常无 CORS。出现 CORS 说明请求没走 nginx 反代（直连了 backend:8080 跨域），改走 `:8088` 同源。 |
| 502 from nginx | nginx 转发 `/v1/` 到 `backend:8080` 不可达：`docker compose ps` 看 backend 是否 healthy、`docker compose exec admin-web wget -qO- http://backend:8080/healthz` 验证服务间网络。 |
| 串户未拒绝（`whoami` 直返 200） | `dev_mock_header` 在 prod 没关。回滚，修 `config.prod.yaml`。 |
| 微信登录 401 | `ZG_WX_APP_ID/SECRET` 错或 code2session 网络不通；后端 `wx.code2session_url` 为空走官方端点，需服务器出网。 |
| AI 管线全降级 | LLM provider key 缺失或额度耗尽；查 `ZG_AI_PROVIDERS_*`、月预算 `ai.monthly_budget_cny`。 |
