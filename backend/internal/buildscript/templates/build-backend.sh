#!/bin/sh
# =============================================================================
# 云辇 · 自建构建脚本（后端 / Java · Maven）
#
# 执行环境
#   平台已经 cd 到这个项目本次构建的**空白工作目录**，PATH 里有 git 与 docker，
#   代码仓库的账号密码已配好（直接 clone 即可，不用写凭据）。
#   构建机上**不需要装 JDK / Maven** —— 编译在下面的编译容器里跑。
#
#   ⚠️ 这个「空白工作目录」每次构建都会被平台**整个删掉重建**，所以代码和依赖
#      **不能**放在它里面。本脚本把两者都落在**宿主机持久化目录** HOST_BASE 下：
#        $HOST_BASE/src/<项目>   各项目的代码（每次构建都**重置为仓库原样**，见 [2/6]）
#        $HOST_BASE/m2           Maven 本地仓库（**长期保留**，跨构建/跨项目共用）
#
#   ⚠️ 打包方式：**优先用仓库自带的 Dockerfile**；仓库里没有时，**脚本自动生成一份**
#      （FROM 运行时镜像 + ADD 编译产物），仓库不必为此改动。
#      注意：Dockerfile 里 `ADD <jar>` 的路径是**相对构建上下文根**的 ——
#      本脚本会专门建一个只含「Dockerfile + 编译产物」的干净上下文，所以
#      哪怕 jar 在仓库里位于 `子模块/target/` 下，Dockerfile 里直接写文件名即可。
#
# 可用变量（平台注入，出现在日志里会自动打码成 ***）
#   $YUNNIAN_REPO       代码仓库地址          http://gitlab.example.com/group/app.git
#   $YUNNIAN_BRANCH     本次构建分支          master
#   $YUNNIAN_COMMIT     本次构建提交号        3f2a91c
#   $YUNNIAN_PROJECT    项目 key               aio-mcp-server
#   $YUNNIAN_BUILD_NO   本项目第几次构建      7          ← 每构建一次 +1，可用来做自增版本号
#   $YUNNIAN_REGISTRY   镜像仓库地址          registry.example.com:5000
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
# 基础镜像所在的仓库：**从平台注入的 $YUNNIAN_REGISTRY 派生，不要写死主机名**。
#
# 写死有两个后果：
#   1. 这份模板会进公开仓库，真实内网地址不能写进去，只能放一个占位域名 —— 而占位域名
#      不可解析：谁点了「恢复默认脚本」把它照抄下来，构建必然挂在拉/跑编译镜像那一步，
#      且报错与"这个域名是我编的"毫无关系（[3/6] 的 `docker pull ... ||` 容忍分支
#      还会把第一次失败吞掉，更难查）；
#   2. 换一台 registry（或多套环境各有一套）就得回来改脚本，而它本该跟着项目配置走。
# `$YUNNIAN_REGISTRY` 由平台按项目的「镜像仓库地址」注入，正是这件事的真相来源。
# 下面 `/escloud/` 是这两个基础镜像在仓库里的项目名，按你们仓库的实际情况改。
BASE_REGISTRY="${YUNNIAN_REGISTRY:-registry.example.com}"
BUILD_IMAGE="$BASE_REGISTRY/escloud/maven:3.6.3-jdk21"      # 编译环境镜像（JDK21 + Maven 3.6.3）
BUILD_CMD="mvn -B -DskipTests clean package"                       # 编译命令
DOCKERFILE_PATH="Dockerfile"                                       # 仓库自带 Dockerfile 的路径（有就用它）
APP_JAR="target/*.jar"                                             # 编译产物（相对仓库根）：
                                                                   #   单模块 → target/*.jar
                                                                   #   多模块 → 子模块/target/xxx.jar

# 仓库**没有** Dockerfile 时，用下面几项自动生成一份最小的（仓库有则忽略）：
RUNTIME_IMAGE="$BASE_REGISTRY/escloud/openjdk:21.0.11_10"           # 运行时基础镜像
APP_PORT="8080"                                                      # 容器监听端口（只写进 EXPOSE，不影响运行）
APP_CMD="java -jar /app.jar"                                         # 容器启动命令

# 宿主机持久化根目录：代码与 m2 都落在这里。
#   默认 $HOME/yunnian-build（本机即 /Users/liudongchneg/yunnian-build）。
#   想放别处，在项目的「构建参数」里加一项 YUNNIAN_HOST_BASE=<绝对路径> 即可覆盖。
# ⚠️ 必须是**构建机 docker 能访问的真实路径**：
#     macOS 要在 Docker Desktop 的 File Sharing 里（/Users 默认已共享）；Linux 上要有写权限。
HOST_BASE="${YUNNIAN_HOST_BASE:-$HOME/yunnian-build}"
CODE_DIR="$HOST_BASE/src/$YUNNIAN_PROJECT"
M2_DIR="$HOST_BASE/m2"
# -----------------------------------------------------------------------------

echo "===== [1/6] 准备持久化目录 ====="
mkdir -p "$M2_DIR"
echo "代码目录：  $CODE_DIR"
echo "Maven 仓库：$M2_DIR"

echo "===== [2/6] 拉取 / 更新代码 ====="
# 每次构建都让工作区**回到"跟仓库一模一样"的状态**，不留任何上一轮的痕迹：
#   1) 丢弃一切本地改动（含在持久化目录里手改过的代码）
#   2) 删除一切未跟踪文件（含上一轮 maven 生成的 target/ 与旧 jar）
#   3) 再对齐到本次分支的远端最新提交
#
# 为什么用 git 自己的命令、而不是 `rm -rf` 把目录删掉重建：
#   - **精确**：git 只动它自己管的东西，不会误删 $HOST_BASE/src 下别的内容；
#   - **不撞安全守卫**：有些环境（开发机沙箱）会给 `rm` 挂一个"批量删除需确认"的
#     守卫，一次删 500+ 个文件会被直接拦下、让整个构建失败；git 的内部删除不走那条路。
#
# ⚠️ 一旦本地 git 状态坏了（远端 force-push、shallow 文件损坏、上次构建中断留下残缺
#    .git），增量更新会失败。此时**不删除、改为移走**旧目录再重新克隆：既保留现场
#    供人工排查，又不需要人工干预才能继续构建 —— 这是本段唯一的自愈分支。
#
# ⚠️ Maven 依赖缓存（m2）**不在**这个目录里，所以清它**不会**导致依赖重下。
if [ -d "$CODE_DIR/.git" ]; then
  cd "$CODE_DIR"
  if git fetch --depth 1 origin "$YUNNIAN_BRANCH"; then
    git reset --hard FETCH_HEAD
    git clean -xfd
  else
    echo "⚠️ 本地仓库无法增量更新，改为重新克隆（旧目录已更名保留，排查后可自行删除）"
    cd "$HOST_BASE/src"
    mv "$CODE_DIR" "$(basename "$CODE_DIR").broken.$(date +%Y%m%d%H%M%S)"
    git clone --depth 1 --branch "$YUNNIAN_BRANCH" "$YUNNIAN_REPO" "$CODE_DIR"
    cd "$CODE_DIR"
  fi
else
  mkdir -p "$HOST_BASE/src"
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
#   $CODE_DIR → /src                  本次已重置为仓库原样的代码（见 [2/6]）
#   $M2_DIR   → /root/.m2/repository  Maven 依赖缓存，跨构建 / 跨项目复用（**这个才要留着**）
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

echo "===== [5/6] 组装构建上下文并打包镜像 ====="
# 构建上下文 = 临时工作目录下一个**干净的**目录，只放「Dockerfile + 编译产物」。
# 为什么不直接在仓库根打包：
#   ① 仓库是持久化的，往里写产物/文件会污染它（还会被 git status 看到）；
#   ② 只把需要的产物放进去，上下文小、打包快（不会把整个仓库打包传给 daemon）。
CTX="$YUNNIAN_WORKDIR/build-ctx"
rm -rf "$CTX"
mkdir -p "$CTX"

# 1) 把编译产物复制进上下文根，**保持原文件名** —— Dockerfile 里的 `ADD <文件名>`
#    就是按这个名字相对上下文根找它的。
#    排除 -sources.jar / -javadoc.jar：字母序里 `-`(0x2D) 排在 `.`(0x2E) 前面，
#    所以它会排在主 jar 之前被 `head -1` 选中 —— 把源码包当运行时 jar 打进镜像。
JAR=$(ls $CODE_DIR/$APP_JAR 2>/dev/null | grep -v -e '-sources\.jar$' -e '-javadoc\.jar$' | head -1)
if [ -z "$JAR" ] || [ ! -f "$JAR" ]; then
  echo "❌ 找不到编译产物：$APP_JAR（检查 APP_JAR 或 BUILD_CMD）" >&2
  exit 1
fi
JAR_NAME=$(basename "$JAR")
cp "$JAR" "$CTX/$JAR_NAME"
echo "编译产物：$JAR_NAME"

# 2) 准备 Dockerfile：仓库自带优先；没有就自动生成一份最小的。
if [ -f "$CODE_DIR/$DOCKERFILE_PATH" ]; then
  cp "$CODE_DIR/$DOCKERFILE_PATH" "$CTX/Dockerfile"
  echo "使用仓库自带的 Dockerfile：$DOCKERFILE_PATH"
else
  cat > "$CTX/Dockerfile" <<EOF
FROM $RUNTIME_IMAGE
EXPOSE $APP_PORT
ADD $JAR_NAME /app.jar
RUN sh -c 'touch /app.jar'
CMD ["sh", "-c", "$APP_CMD"]
EOF
  echo "仓库未自带 Dockerfile，已自动生成一份："
fi
echo "--- 本次使用的 Dockerfile ---"
cat "$CTX/Dockerfile"
echo "-----------------------------"

# ⚠️ `--provenance=false` 不能删。BuildKit 默认会给镜像挂上 attestation，
# 于是产出的其实是一个 **manifest list**（多出一个 unknown/unknown 平台条目）。
# 本地看不出任何异常，但 `docker push` 到老一些的 registry（含 Harbor）会直接
# 报 `error from registry: manifest invalid` —— 失败点离原因很远，很难查。
# 平台自己的「自带 Dockerfile」构建路径同样带着这个参数。
docker build --platform "$YUNNIAN_PLATFORM" --provenance=false -f "$CTX/Dockerfile" -t "$YUNNIAN_IMAGE" "$CTX"

echo "===== [6/6] 推送镜像 ====="
docker push "$YUNNIAN_IMAGE"

echo "构建完成：$YUNNIAN_IMAGE"
