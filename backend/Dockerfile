# syntax=docker/dockerfile:1.7
# 掌柜军师 Go 后端生产级容器镜像。
#
# 构建命令（在仓库根 /Users/liningwei/Selfproj/zhangguijunshi 执行）：
#   docker build -f backend/Dockerfile -t zhanggui-junshi-backend:latest backend/
#
# 构建上下文是 backend/，所以本文件内的路径都相对 backend/。
#
# 关键决策：
#   - 配置加载（见 backend/pkg/config/config.go）：config.Load 读取
#     configs/config.<ZG_ENV>.yaml（目录默认 "configs"，相对 CWD），缺失文件不算
#     错误（落到 defaults + ZG_ 环境变量）。没有命令行 flag 指定配置文件路径，
#     也没有 ZG_APP_CONFIG_FILE；唯一的全局开关是 ZG_ENV（dev|staging|prod）。
#     因此 runtime 把工作目录设为 /app，并把 configs/ 与 migrations/ 拷到 /app
#     下，使 api 与 migrate 默认就能找到各自目录（migrate 的 migrations dir 默认
#     "migrations"，同样相对 CWD）。
#   - 监听端口：http.port 默认 8080（pkg/config/config.go SetDefaults），EXPOSE 8080。
#   - health 端点：/healthz（liveness，无依赖检查）与 /readyz（readiness，含 DB
#     ping，见 interfaces/http/router.go）。HEALTHCHECK 用 /healthz，因为它在 DB
#     未就绪时仍返回 200——容器自身存活与依赖就绪分离，避免编排器误杀。
#   - 五个二进制（api / migrate / worker / cron / llmbench）一并构建到 /out/，
#     方便同一镜像以不同 entrypoint 复用（migrate 作为 init 容器等）。
#   - CGO_ENABLED=0：纯静态，可跑 distroless static；pgx 等纯 Go 依赖无需 cgo。

ARG GO_VERSION=1.25
ARG ALPINE_VERSION=3.20

############################
# builder
############################
FROM golang:${GO_VERSION}-alpine AS builder

# 构建时注入版本号（可被 --build-arg 覆盖），便于镜像溯源。
ARG APP_VERSION=0.0.0-container
ENV APP_VERSION=${APP_VERSION}

# alpine 上 git 被 go module 可能需要（私服/vanity import）；本仓为本地 module，
# 不需要网络拉取，但安装 git 可避免在更新依赖时失败。
RUN apk add --no-cache git ca-certificates

WORKDIR /src

# 先拷 go.mod/go.sum 单独下载依赖，最大化层缓存。
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# 拷全部源码（.dockerignore 已排除无关项）。
COPY . .

# 静态构建全部五个二进制到 /out/。ldflags 去符号缩体积；不注入版本到 main
# （源码未定义 ldflags -X 入口，version 来自 config，故只 strip）。
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" \
      -o /out/api       ./cmd/api && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" \
      -o /out/migrate  ./cmd/migrate && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" \
      -o /out/worker   ./cmd/worker && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" \
      -o /out/cron     ./cmd/cron && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" \
      -o /out/llmbench ./cmd/llmbench

############################
# runtime
############################
FROM alpine:${ALPINE_VERSION} AS runtime

# ca-certificates：LLM 网关 / 微信 code2session / 微信支付 V3 均 HTTPS 出站。
# wget：用于容器内 HEALTHCHECK（alpine 自带 busybox wget，但显式装 wget 兜底）。
# tzdata：微信支付回调时间戳 / cron 时区敏感（北京时间）。
RUN apk add --no-cache ca-certificates tzdata wget && \
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone && \
    addgroup -S zgjs && adduser -S -G zgjs -u 10001 zgjs

# 非 root 运行，工作目录设为 /app 以匹配 config/migrate 的相对目录默认值。
WORKDIR /app

# 拷贝二进制与运行期资产。
COPY --from=builder /out/api       /out/api
COPY --from=builder /out/migrate  /out/migrate
COPY --from=builder /out/worker   /out/worker
COPY --from=builder /out/cron     /out/cron
COPY --from=builder /out/llmbench /out/llmbench
COPY configs/ /app/configs/
COPY migrations/ /app/migrations/

# 权限归非 root 用户。
RUN chown -R zgjs:zgjs /app /out

USER 10001:zgjs

EXPOSE 8080

# 用 /healthz 做存活探测（liveness，无 DB 依赖；DB 就绪走 /readyz，由编排器单独
# probe，避免 DB 慢起把容器杀掉重启循环）。interval/timeout 与 http 默认 15s
# 超时匹配，start-period 给 DB/Redis 慢起 30s 缓冲。
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=3 \
    CMD wget -qO- --tries=1 --timeout=3 http://127.0.0.1:8080/healthz || exit 1

# 默认启动 api。复用镜像跑 migrate/worker/cron/llmbench：
#   docker run --rm --entrypoint /out/migrate zhanggui-junshi-backend:latest up
ENTRYPOINT ["/out/api"]
