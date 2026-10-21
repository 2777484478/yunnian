#!/usr/bin/env bash
# ---------------------------------------------------------------------------
# YunNian 云辇 · 离线包导入（**在无外网的目标服务器上执行**）
#
#   ./import-offline.sh [离线包目录，默认脚本所在目录]
#
# 只做一件事：把 export-offline.sh 导出的 tar 灌进本机 Docker。
# 之后用 `docker compose up -d --no-build` 启动 —— **必须带 --no-build**，
# 否则 compose 看到 docker-compose.yml 里有 build: 段会尝试重新构建，
# 而无外网时那一步会因为拉不到基础镜像直接失败。
# ---------------------------------------------------------------------------
set -euo pipefail

DIR="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
TAR="$DIR/images/yunnian-offline.tar"

info() { printf '\033[36m›\033[0m %s\n' "$*"; }
ok()   { printf '\033[32m✓\033[0m %s\n' "$*"; }
die()  { printf '\033[31m✗\033[0m %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- 预检
command -v docker >/dev/null 2>&1 || die "没装 docker"
docker info >/dev/null 2>&1 || die "docker daemon 没起来（当前用户可能没权限，试试 sudo）"
[ -f "$TAR" ] || die "找不到 $TAR —— 目录传对了吗？"

# ---------------------------------------------------------------- 架构预检
# 架构不一致时，镜像能 load 成功、但一启动就 exec format error，
# 而那个报错里完全看不出是「包导错了」。所以在这里先挡住。
#
# ⚠️ 不能直接比字符串：Docker 说 amd64 / arm64，uname 说 x86_64 / aarch64。
#    `amd64` != `x86_64` 会让这段永远误判成「架构不对」，连正常的包也挡住。
#    先归一成规范名再比。
norm_arch() {
  case "$1" in
    amd64|x86_64|x64)  echo "amd64" ;;
    arm64|aarch64)     echo "arm64" ;;
    armv7l|armv7)      echo "arm/v7" ;;
    *)                 echo "$1" ;;
  esac
}

LOCAL_ARCH="$(norm_arch "$(uname -m)")"
info "本机架构：${LOCAL_ARCH}（uname 报 $(uname -m)）"
if [ -f "$DIR/images/ARCH" ]; then
  PKG_ARCH="$(norm_arch "$(cat "$DIR/images/ARCH")")"
  if [ "$PKG_ARCH" != "$LOCAL_ARCH" ]; then
    die "离线包是 ${PKG_ARCH} 架构，本机是 ${LOCAL_ARCH} —— 镜像能导入但一启动就会 exec format error。
     重新导出：PLATFORM=linux/${LOCAL_ARCH} ./scripts/export-offline.sh"
  fi
  ok "架构一致（${PKG_ARCH}）"
else
  printf '\033[33m!\033[0m 离线包里没有 images/ARCH，跳过架构校验（若 exec format error 就是架构不对）\n'
fi

# ---------------------------------------------------------------- 导入
info "导入镜像（实测约 450 MB，1-2 分钟）"
docker load -i "$TAR"

# ---------------------------------------------------------------- 核对
echo
info "导入结果："
for img in yunnian:latest mysql:8.0 redis:7-alpine node:22-alpine golang:1.26-alpine alpine:3.20; do
  if docker image inspect "$img" >/dev/null 2>&1; then
    ok "$img"
  else
    printf '\033[33m!\033[0m %s 不在本地（如果只是不用它，可以忽略）\n' "$img"
  fi
done

cat <<'TXTEOF'

下一步：
  1. cp .env.example .env     # 填 YUNNIAN_JWT_SECRET / YUNNIAN_MASTER_KEY / MYSQL_ROOT_PASSWORD
  2. docker compose up -d --no-build
  3. docker compose logs -f app

⚠️ 一定要带 --no-build —— 无外网时 compose 的隐式构建会卡在拉基础镜像。
TXTEOF
