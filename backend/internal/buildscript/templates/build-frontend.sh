#!/bin/sh
# =============================================================================
# 云辇 · 自建构建脚本（前端 / Node · npm）
#
# 执行环境、可用变量、约定 均与「后端」模板一致，详见 build-backend.sh 顶部的说明。
# 这里只列前端特有的两处。
# =============================================================================
set -e

# ---------------------------------------------------------------- 按项目改这几行
# ⚠️ 这个镜像是 Docker Hub 上的。构建机（或其 docker daemon）拉不到 Docker Hub 时，
#    把它换成内网仓库里的同名镜像，例如 es-repo.example.com:5000/escloud/node:20-alpine。
BUILD_IMAGE="node:20-alpine"            # 编译环境镜像（Node 20）
BUILD_CMD="npm ci && npm run build"     # 编译命令
DOCKERFILE="Dockerfile"                 # 打包用哪个 Dockerfile（通常是 nginx 那份）
# -----------------------------------------------------------------------------

echo "===== [1/5] 拉取代码 ====="
git clone --depth 1 --branch "$YUNNIAN_BRANCH" "$YUNNIAN_REPO" src
cd src
git log -1 --format='提交 %h  %an  %s'

echo "===== [2/5] 准备编译镜像 ====="
# 与后端模板同理，而且这里更要紧：node 镜像若在 Docker Hub 上，
# 不先拉到本地的话，BuildKit 解析远程元数据那一步很可能直接失败。
# 同样容忍失败：`docker pull` 不看本地缓存，构建机上已装好这个镜像时
# 它照样报 not found，不该因此中断整个构建。
docker pull --platform "$YUNNIAN_PLATFORM" "$BUILD_IMAGE" >/dev/null 2>&1 ||
  echo "（未能从仓库拉取 $BUILD_IMAGE，改用构建机本地已有的同名镜像）"

echo "===== [3/5] 编译 ====="
# node_modules 放在具名卷里而不是构建目录里：一来不会被随后的
# `docker build .` 当成构建上下文捎进去（那会让上下文从几 MB 涨到几百 MB），
# 二来容器内装的二进制不会被写回宿主的工作目录。
# 代价是每次都要重新 npm ci —— 换来的是构建可重复。
docker run --rm \
  --platform "$YUNNIAN_PLATFORM" \
  -v "$PWD":/src -w /src \
  -v yunnian-npm:/src/node_modules \
  "$BUILD_IMAGE" \
  sh -c "$BUILD_CMD"

echo "===== [4/5] 打包镜像 ====="
# `--provenance=false` 不能删，理由见后端模板同一行的注释（BuildKit 的
# attestation 会让产物变成 manifest list，推到 Harbor 报 manifest invalid）。
docker build --platform "$YUNNIAN_PLATFORM" --provenance=false -f "$DOCKERFILE" -t "$YUNNIAN_IMAGE" .

echo "===== [5/5] 推送镜像 ====="
docker push "$YUNNIAN_IMAGE"

echo "构建完成：$YUNNIAN_IMAGE"
