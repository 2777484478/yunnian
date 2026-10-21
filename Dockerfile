# ---------------------------------------------------------------------------
# YunNian 云辇 · 生产镜像（多阶段）
#
#   ① frontend  Node 构建前端单文件产物
#   ② backend   Go 静态编译后端
#   ③ runtime   运行时（Alpine），带 docker CLI —— 平台自己就是构建机
#
# ⚠️ 运行时**必须带 docker CLI**：平台的「自建构建」是真跑
#    docker build / docker push（runner.executor=docker），容器里没有 docker
#    就没有构建能力。构建时通过挂载宿主机的 /var/run/docker.sock 复用 daemon。
# ---------------------------------------------------------------------------

# ---------- ① 前端 ----------
FROM node:22-alpine AS frontend
WORKDIR /src
# 先只拷依赖清单：这层能命中缓存，代码改动时不必重装依赖
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
# vite-plugin-singlefile 会把 JS/CSS 内联进单个 index.html
RUN npm run build

# ---------- ② 后端 ----------
FROM golang:1.26-alpine AS backend
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
# CGO_ENABLED=0 → 静态二进制，可放进 Alpine；-trimpath 去掉构建机路径，-s -w 减小体积
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/yunnian-server ./cmd/server

# ---------- ③ 运行时 ----------
FROM alpine:3.20
# ca-certificates  访问 HTTPS 下游（Harbor / 模型通道 / 邮件）
# tzdata          容器内时区，否则日志时间是 UTC
# git/openssh     构建时要 clone 代码仓库（https 与 ssh 两种协议）
# docker-cli      平台自己就是构建机，必须能调 docker
RUN apk add --no-cache \
      ca-certificates tzdata git openssh-client \
      docker-cli docker-cli-compose \
    && ln -snf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone

WORKDIR /app
# 二进制
COPY --from=backend /out/yunnian-server /app/yunnian-server
# 前端产物（单文件，托管在 staticDir 指向的位置）
COPY --from=frontend /src/dist /app/frontend/dist
# 配置模板：敏感项一律走环境变量注入，**不要**把真实密码写进这个文件
COPY backend/configs/config.yaml.example /app/configs/config.yaml

# 头像等上传文件落在 data/，需要挂卷持久化
RUN mkdir -p /app/data/uploads/avatars \
    && addgroup -g 1000 -S yunnian \
    && adduser -u 1000 -S yunnian -G yunnian \
    && chown -R yunnian:yunnian /app
USER yunnian

EXPOSE 8080
VOLUME ["/app/data"]

HEALTHCHECK --interval=15s --timeout=3s --start-period=20s --retries=5 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/yunnian-server"]
