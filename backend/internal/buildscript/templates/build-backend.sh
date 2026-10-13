#!/bin/sh
# =============================================================================
# 云辇 · 自建构建脚本（后端 / Java · Maven）
#
# 执行环境
#   平台已经 cd 到这个项目本次构建的**空白工作目录**，
#   PATH 里有 git 与 docker，代码仓库的账号密码已配好（直接 clone 即可，不用写凭据）。
#   构建机上**不需要装 JDK / Maven** —— 编译在下面的编译容器里跑。
#
#   ⚠️ 这个「空白工作目录」每次构建都会被平台**整个删掉重建**，所以代码和依赖
#      **不能**放在它里面。本脚本把两者都落在**宿主机持久化目录** HOST_BASE 下：
#        $HOST_BASE/src/<项目>   各项目的代码（按项目分目录，互不干扰）
#        $HOST_BASE/m2           Maven 本地仓库（所有项目共用，依赖只下一次）
#      好处：① 代码增量更新，不用每次整仓重拉；② m2 依赖跨构建 / 跨项目复用。
#
# 可用变量（平台注入，出现在日志里会自动打码成 ***）
#   $YUNNIAN_REPO       代码仓库地址          http://gitlab.example.com/group/app.git
#   $YUNNIAN_BRANCH     本次构建分支          master
#   $YUNNIAN_COMMIT     本次构建提交号        3f2a91c
#   $YUNNIAN_PROJECT    项目 key               aio-mcp-server
#   $YUNNIAN_BUILD_NO   本项目第几次构建      7          ← 每构建一次 +1，可用来做自增版本号
#   $YUNNIAN_REGISTRY   镜像仓库地址          es-repo.example.com:5000
#   $YUNNIAN_IMAGE_NAME 镜像名（不含仓库/tag） escloud/esc-mcp-bridge
#   $YUNNIAN_TAG        本次 tag（按 Tag 规则算出）v7
#   $YUNNIAN_IMAGE      完整镜像地址，= $YUNNIAN_REGISTRY/$YUNNIAN_IMAGE_NAME:$YUNNIAN_TAG
#   $YUNNIAN_PLATFORM   目标平台              linux/amd64
#   $YUNNIAN_WORKDIR    本次构建的工作目录（每次重建 —— **别往里放要留的数据**）
#   $YUNNIAN_GIT_USER / $YUNNIAN_GIT_PASS     仓库凭据（一般用不到，已配好）
#   以及「构建参数」里你自己定义的变量（可用来覆盖下面的 HOST_BASE）
#
# 约定
#   任何一步返回非 0 → 构建失败，失败原因取输出的最后几行。
#   平台最后会核验 $YUNNIAN_IMAGE 在构建机上**确实存在**，不存在就判为失败 ——
#   所以不能删掉 `docker build -t "$YUNNIAN_IMAGE"` 那一步。
# =============================================================================
set -e

# ---------------------------------------------------------------- 按项目改这几行
BUILD_IMAGE="es-repo.example.com:5000/escloud/maven:3.6.3-jdk21"  # 编译环境镜像（JDK21 + Maven 3.6.3）
BUILD_CMD="mvn -B -DskipTests clean package"                             # 编译命令
DOCKERFILE="Dockerfile"                                                  # 打包用哪个 Dockerfile

# 宿主机持久化根目录：代码与 m2 都落在这里。
#   默认 $HOME/yunnian-build（本机即 /Users/liudongchneg/yunnian-build）。
#   想放别处，在项目的「构建参数」里加一项 YUNNIAN_HOST_BASE=<绝对路径> 即可覆盖，
#   不用改这段脚本。
# ⚠️ 必须是**构建机 docker 能访问的真实路径**：
#     macOS 要在 Docker Desktop 的 File Sharing 里（/Users 默认已共享）；
#     Linux 上要有写权限。路径不存在也没关系，下面会 mkdir -p 建出来。
HOST_BASE="${YUNNIAN_HOST_BASE:-$HOME/yunnian-build}"
CODE_DIR="$HOST_BASE/src/$YUNNIAN_PROJECT"
M2_DIR="$HOST_BASE/m2"
# -----------------------------------------------------------------------------

echo "===== [1/6] 准备持久化目录 ====="
mkdir -p "$M2_DIR"
echo "代码目录：  $CODE_DIR"
echo "Maven 仓库：$M2_DIR"

echo "===== [2/6] 拉取 / 更新代码 ====="
if [ -d "$CODE_DIR/.git" ]; then
  # 目录里已有 git 仓库 → **增量更新**：只拉目标分支的最新提交，不再整仓重拉。
  cd "$CODE_DIR"
  git fetch --depth 1 origin "$YUNNIAN_BRANCH"
  git reset --hard FETCH_HEAD
else
  # 首次（或上次构建半途失败、留下了残缺目录）→ 清干净后浅克隆。
  # 只清本项目自己的目录：$YUNNIAN_PROJECT 由平台注入、必非空，
  # 这里再加一道判断，避免变量意外为空时误删 $HOST_BASE/src 整层。
  if [ -n "$YUNNIAN_PROJECT" ] && [ -d "$CODE_DIR" ]; then
    rm -rf "$CODE_DIR"
  fi
  git clone --depth 1 --branch "$YUNNIAN_BRANCH" "$YUNNIAN_REPO" "$CODE_DIR"
  cd "$CODE_DIR"
fi
git log -1 --format='提交 %h  %an  %s'

echo "===== [3/6] 准备编译镜像 ====="
# 这一句不是"保险起见"，是**实测必须的**。
#
# 基镜像不在本地时，docker build 会让 BuildKit 去注册表解析元数据，而那条路走的是
# daemon 自己的代理通道，在很多网络环境下会直接失败
# （典型报错：failed to fetch oauth token ... Bad Gateway）。
# 镜像一旦在本地，BuildKit 就不再去解析 —— 同一个 Dockerfile 从"必失败"变成"必成功"。
#
# ⚠️ 必须容忍失败，不能直接 `docker pull` 就完事：
#    `docker pull` **只看远端、不看本地缓存**。构建机上已经预装好这个镜像时
#    （离线构建、内网镜像没同步过来的常见情形），pull 会报 not found 并因 set -e
#    直接中断整个构建 —— 而它要用的那个镜像明明就在本地。方向正好反了。
#    真拉不到也不要紧：下一步 `docker run` 会立刻报出"镜像不存在"，错误位置更清楚。
#
# 这个编译镜像**装一次就一直留在构建机上**：本脚本全程不 rmi，后续所有项目都用它
# 编译，不必每次重拉。
docker pull --platform "$YUNNIAN_PLATFORM" "$BUILD_IMAGE" >/dev/null 2>&1 ||
  echo "（未能从仓库拉取 $BUILD_IMAGE，改用构建机本地已有的同名镜像）"

echo "===== [4/6] 编译（在编译容器里跑，构建机不用装 JDK/Maven）====="
# 两个挂载都指向**宿主机持久化目录**，不是临时工作目录、也不是匿名卷：
#   $CODE_DIR → /src                  编译的就是这份代码，构建完不删，下次接着更新
#   $M2_DIR   → /root/.m2/repository  Maven 依赖缓存，跨构建 / 跨项目复用
# ⚠️ 用**绝对路径**挂载（不是 "$PWD"）：构建机若通过挂 docker.sock 执行构建（DooD），
#    daemon 会在**宿主**上找这个路径；临时工作目录那种每次都变的路径它会找不到，
#    报的错还和"目录不存在"毫不相干。
# 容器用完即删（--rm），但**镜像不删** —— 下次继续用同一个 maven 镜像编译别的代码。
docker run --rm \
  --platform "$YUNNIAN_PLATFORM" \
  -v "$CODE_DIR":/src -w /src \
  -v "$M2_DIR":/root/.m2/repository \
  "$BUILD_IMAGE" \
  sh -c "$BUILD_CMD"

echo "===== [5/6] 打包镜像 ====="
# 如果你的 Dockerfile 里写死了产物文件名（比如 `ADD app-1.0.0.jar /app.jar`），
# 而 Maven 打出来的 jar 带版本号，就在这行前面加一句改名，例如：
#   cp target/app-*.jar target/app-1.0.0.jar
#
# ⚠️ `--provenance=false` 不能删。BuildKit 默认会给镜像挂上 attestation，
# 于是产出的其实是一个 **manifest list**（多出一个 unknown/unknown 平台条目）。
# 本地看不出任何异常，但 `docker push` 到老一些的 registry（含 Harbor）会直接
# 报 `error from registry: manifest invalid` —— 失败点离原因很远，很难查。
# 平台自己的「自带 Dockerfile」构建路径同样带着这个参数。
docker build --platform "$YUNNIAN_PLATFORM" --provenance=false -f "$DOCKERFILE" -t "$YUNNIAN_IMAGE" .

echo "===== [6/6] 推送镜像 ====="
docker push "$YUNNIAN_IMAGE"

echo "构建完成：$YUNNIAN_IMAGE"
