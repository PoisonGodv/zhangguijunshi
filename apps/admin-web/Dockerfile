# Dockerfile — 掌柜军师 管理后台 (apps/admin-web) 容器化产物
#
# NOTE: 不使用 `# syntax=docker/dockerfile:1.7` 显式前端指令。`# syntax=docker`
# 会被 BuildKit 解析为 docker.io/library/docker:latest 作为前端镜像，但它不是
# 合法的 BuildKit frontend，会以 "frontend grpc server closed unexpectedly"
# 崩溃；显式 `docker/dockerfile:1.7` 又依赖实时从 docker.io 拉取前端（在受限/
# flaky 网络下会 auth 超时）。Docker 29.x 内置的 BuildKit frontend 已支持本文件
# 用到的全部特性（多阶段、COPY --from），故去掉 syntax 指令、用内置前端，既
# 稳定义免去构建期网络拉取。
#
# 构建产物: nginx 静态站 + 同源反代 /v1/ → 后端 Go 单体 (避免 CORS)
# API base 现状: src/services/request.ts 硬编码 baseURL '/v1' (同源相对路径,
#   无 VITE_API_BASE 构建期注入, 无 window.__API_BASE__ 运行期注入), 故
#   prod 依赖 nginx 反向代理 /v1/ → backend:8080, 不需要 envsubst。
#
# 多阶段:
#   builder: node:20-alpine (npm v10 兼容 package-lock.json v3; node20 已稳定支持
#            项目所用全部依赖, 与本地 node24 产物一致), npm ci + npm run build → dist/
#   runtime: nginx:1.27-alpine, 拷 dist/ 到 /usr/share/nginx/html,
#            拷 nginx.conf 到 /etc/nginx/conf.d/default.conf, EXPOSE 80
#
# 权限模型说明: nginx 容器内 master 进程以 root 启动 (绑定 <1024 端口需要),
#   worker 以 nginx 用户 (非 root) 运行, 这是 nginx 官方镜像的常态与安全实践。
#   如需全非 root, 可换 nginxinc/nginx-unprivileged:1.27-alpine (监听 8080)。

# ---------- builder ----------
FROM node:20-alpine AS builder
WORKDIR /app

# 先拷依赖清单以最大化 layer 缓存命中
COPY package.json package-lock.json ./
# npm ci: 严格按 lockfile 安装, 可重复构建; node20-alpine 自带 npm10
RUN npm ci

# 拷源码 (node_modules 已被 .dockerignore 排除, 此处只拷源)
COPY . .

# 构建产物到 dist/ (tsc -b && vite build)
RUN npm run build

# ---------- runtime ----------
FROM nginx:1.27-alpine AS runtime

# 时区 (中文管理后台, 日志时间可读)
RUN apk add --no-cache tzdata && \
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime && \
    echo "Asia/Shanghai" > /etc/timezone

# 清空 nginx 默认配置占位 (避免与 default.conf 重复)
RUN rm -f /etc/nginx/conf.d/default.conf

# 自定义 server 配置 (SPA fallback + 静态缓存 + /v1 反代)
COPY nginx.conf /etc/nginx/conf.d/default.conf

# 构建产物
COPY --from=builder /app/dist /usr/share/nginx/html

# 健康检查: nginx 返回 index.html (200)
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD wget -qO- http://127.0.0.1/ >/dev/null 2>&1 || exit 1

EXPOSE 80

# nginx 默认 CMD: ["nginx", "-g", "daemon off;"]
# master root + worker nginx (非 root), 官方镜像默认行为, 无需覆盖
CMD ["nginx", "-g", "daemon off;"]
