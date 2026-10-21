#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# YunNian 云辇 · 离线包导出（**在有外网的机器上执行**）
#
#   ./scripts/export-offline.sh [输出目录，默认 ./offline]
#
#   PLATFORM=linux/amd64 ./scripts/export-offline.sh   # 按目标架构交叉构建（推荐）
#
# ⚠️ **服务器是 x86_64 就一定要设 PLATFORM=linux/amd64**：
#    在 Apple Silicon（arm64）上导出、拿到 x86 服务器上跑，镜像能 load 成功，
#    但一启动就 `exec format error` —— 那个报错里完全看不出是"架构不对"。
#
# 做一些什么：
#   ① 构建应用镜像 yunnian:latest
#   ② 把「应用 + 依赖 + 平台自建构建要用的基础镜像」全部 save 成一个 tar
#   ③ 连同源码与部署文件一起打包
#
# 为什么要把基础镜像也带上：
#   平台的「自建构建」是真跑 docker build，而项目 Dockerfile 里的 FROM
#   （golang / node / alpine…）在无外网的服务器上拉不到 —— 构建会卡在第一步。
#   把它们一起 load 进服务器，Docker 会优先用本地已有的层。
#
# ⚠️ 目标服务器与这台机器的 **CPU 架构必须一致**（都是 x86_64 或都是 arm64）。
#    不一致时镜像导入后启动会报 exec format error，而报错位置跟"架构不对"毫无关系。
# ---------------------------------------------------------------------------
set -euo pipefail

OUT_DIR="${1:-./offline}"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

# 应用镜像 + 运行依赖 + 平台自建构建的常用基础镜像
APP_IMAGE="yunnian:latest"
BASE_IMAGES=(
  "mysql:8.0"           # 依赖：数据库
  "redis:7-alpine"      # 依赖：缓存 / 队列 / 部署锁
  "node:22-alpine"      # Dockerfile 前端构建阶段
  "golang:1.26-alpine"  # Dockerfile 后端构建阶段
  "alpine:3.20"         # 运行时基础镜像
)

info() { printf '\033[36m›\033[0m %s\n' "$*"; }
ok()   { printf '\033[32m✓\033[0m %s\n' "$*"; }
warn() { printf '\033[33m!\033[0m %s\n' "$*"; }

info "输出目录：$OUT_DIR"
mkdir -p "$OUT_DIR/images"

# ---------------------------------------------------------------- ① 先准备好基础镜像
# ⚠️ **必须在构建之前**。buildx 交叉构建时，BuildKit 会自己去 registry 解析
# 基础镜像的元数据（`load metadata`）—— 这一步走的是 BuildKit 自己的网络路径，
# 在代理不稳的机器上经常报 `failed to fetch oauth token: Bad Gateway`。
# 而**只要镜像已经在本地 store，BuildKit 就直接用本地层，不再解析远端**。
# 所以顺序是：先把 amd64 的几个基础镜像拉下来 → 再 build。
PLATFORM="${PLATFORM:-}"

for img in "${BASE_IMAGES[@]}"; do
  if [ -n "$PLATFORM" ]; then
    # 指定了架构就必须按该架构检查，不能只看"本地有没有这个名字"——
    # arm64 的 golang:1.26-alpine 已经在本机，但 amd64 版并不在
    if docker image inspect "$img" >/dev/null 2>&1 \
       && docker image inspect "$img" --format '{{.Os}}/{{.Architecture}}' | grep -qx "linux/${PLATFORM#linux/}"; then
      ok "已有 ${img}（${PLATFORM}）"
      continue
    fi
    info "拉取 ${img}（${PLATFORM}）"
    docker pull --platform "$PLATFORM" "$img"
  else
    if docker image inspect "$img" >/dev/null 2>&1; then
      ok "已有 $img"
    else
      info "拉取 $img"
      docker pull "$img"
    fi
  fi
done

# ---------------------------------------------------------------- ② 构建应用镜像
# 为什么必须能交叉：开发机多半是 Apple Silicon（arm64），服务器多半是 x86_64。
if [ -n "$PLATFORM" ]; then
  info "构建应用镜像 ${APP_IMAGE}（目标 ${PLATFORM}，交叉构建会比较久）"
  docker buildx build --platform "$PLATFORM" -t "$APP_IMAGE" --load .
else
  info "构建应用镜像 ${APP_IMAGE}（多阶段，首次会比较久）"
  docker build -t "$APP_IMAGE" .
fi
ok "应用镜像构建完成"

# ---------------------------------------------------------------- ③ 导出一个 tar
TAR="$OUT_DIR/images/yunnian-offline.tar"
info "导出镜像到 ${TAR}（实测约 450 MB；共享层会去重，所以比各镜像大小之和小很多）"
docker save -o "$TAR" "$APP_IMAGE" "${BASE_IMAGES[@]}"
ok "镜像已导出：$(du -h "$TAR" | cut -f1)"

# 记下导出时的架构：导入端靠它挡住"包导错了"——架构不符时镜像能 load、
# 但一启动就 exec format error，而那个报错完全看不出是包的问题
if [ -n "$PLATFORM" ]; then
  echo "${PLATFORM#linux/}" > "$OUT_DIR/images/ARCH"
else
  uname -m > "$OUT_DIR/images/ARCH"
fi
ok "记录架构：$(cat "$OUT_DIR/images/ARCH")"

# ---------------------------------------------------------------- ④ 打包源码
# 不打包 node_modules / bin / .git：前两个能重建，.git 在服务器上用不到且体积大
SRC="$OUT_DIR/yunnian-src.tar.gz"
info "打包源码到 $SRC"
tar czf "$SRC" \
  --exclude='.git' \
  --exclude='node_modules' \
  --exclude='frontend/dist' \
  --exclude='backend/bin' \
  --exclude='offline' \
  --exclude='.workbuddy' \
  -C "$(dirname "$ROOT_DIR")" "$(basename "$ROOT_DIR")"
ok "源码已打包：$(du -h "$SRC" | cut -f1)"

# ---------------------------------------------------------------- ⑤ 带走部署脚本
cp scripts/import-offline.sh "$OUT_DIR/" 2>/dev/null || true
cp docker-compose.yml "$OUT_DIR/" 2>/dev/null || true
cp .env.example "$OUT_DIR/" 2>/dev/null || true

cat > "$OUT_DIR/README-离线部署.txt" <<'TXTEOF'
YunNian 云辇 · 离线部署包
=========================

包含：
  images/yunnian-offline.tar   应用镜像 + MySQL + Redis + 构建用基础镜像
  yunnian-src.tar.gz           源码（不含 .git / node_modules / 二进制）
  docker-compose.yml           编排文件
  .env.example                 环境变量模板
  import-offline.sh            在目标服务器上执行的导入脚本

在目标服务器上（假设离线包解到 /tmp/yunnian-offline）：
  1. cd /tmp/yunnian-offline && ./import-offline.sh        # 先导入镜像
  2. tar xzf yunnian-src.tar.gz -C /opt && cd /opt/yunnian
  3. cp /tmp/yunnian-offline/.env.example .env             # 填三个密钥（见 README「一键部署」）
  4. docker compose up -d --no-build

注意：
  · 目标机器必须已装 Docker（20.10+）与 docker compose v2
  · 架构必须与本机一致，否则镜像起来会 exec format error
  · 平台「自建构建」用的基础镜像已包含在内；项目自带的其它 FROM 镜像需另外补
TXTEOF

ok "离线包完成：$OUT_DIR"
echo
echo "下一步：把整个 $OUT_DIR 目录拷到目标服务器"
echo "  scp -r $OUT_DIR user@server:/tmp/"
echo "或（内网更快）先压成一个包："
echo "  tar czf offline-bundle.tar.gz -C \"$(dirname "$OUT_DIR")\" \"$(basename "$OUT_DIR")\""
