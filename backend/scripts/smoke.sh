#!/usr/bin/env bash
# YunNian 后端冒烟脚本：走通 docs/03 §14 的一期核心路径
# （登录 → 项目 → 构建 → 构建 SSE → 环境 → 服务实例 → 部署 → 部署 SSE → 回滚 → 审计）。
#
# 用法：bash scripts/smoke.sh [baseUrl]
#   默认 baseUrl=http://127.0.0.1:8080/api/v1
#
# 两个环境相关的注意点：
#   1. 一律加 --noproxy '*'：本机默认 HTTP 代理会把 127.0.0.1 也代理掉，
#      表现为"服务明明起来了却返回 502 upstream connect failed"。
#   2. 只依赖 curl 与 python3，不引第三方库，便于在任意环境复跑。
set -uo pipefail

BASE="${1:-http://127.0.0.1:8080/api/v1}"
ROOT="${BASE%/api/v1}"
USERNAME="ops"
PASSWORD="changeme"
CURL=(curl -s --noproxy '*' -H 'Content-Type: application/json')
FAILED=0

# 清掉上一轮的产物再开跑。
#
# 不清理会「假绿」：多数断言是「curl 落盘 → check 读文件比对 code」，
# 一旦 curl 因服务未启动/中途挂掉而失败，文件不会被写，
# check 读到的是**上一轮遗留的同名文件**，于是照样打印 ✓。
# 实测踩过：服务其实早就退出了，整个脚本仍报了满屏绿。
# 先 rm 掉，缺失文件会让 check 返回 PARSE_ERR → 如实报红。
rm -f /tmp/smoke-*.json

step() { printf '\n\033[1;34m== %s ==\033[0m\n' "$1"; }
ok()   { printf '  \033[32m✓\033[0m %s\n' "$1"; }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$1"; FAILED=$((FAILED + 1)); }
info() { printf '  · %s\n' "$1"; }

# check <描述> <期望code> <json文件>
check() {
  local desc="$1" want="$2" file="$3" got
  got=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["code"])' "$file" 2>/dev/null || echo PARSE_ERR)
  if [ "$got" = "$want" ]; then ok "$desc (code=$got)"; else bad "$desc 期望 code=$want 实际=$got"; fi
}

# get <json文件> <python表达式，d 为根对象>
get() { python3 -c "import json,sys;d=json.load(open(sys.argv[1]));print(eval(sys.argv[2]))" "$1" "$2" 2>/dev/null; }

# --------------------------------------------------------------------------- 1. 探针
step "探针"
# 预检：服务不通就别继续了。继续跑下去只会得到一堆"看起来通过"的假绿
# （断言的文件不存在时虽会报 PARSE_ERR，但后面的链路断言会刷出更长更难查的噪声）。
if ! curl -s --noproxy '*' --max-time 5 "$ROOT/healthz" -o /tmp/smoke-health.json; then
  printf '\033[1;31m服务不可达：%s/healthz\033[0m\n' "$ROOT"
  printf '请先启动后端（后台方式，注意 & 起的进程会随 shell 退出被回收）：\n'
  printf '  cd backend && ./bin/yunnian-server -config configs/config.yaml\n'
  exit 2
fi
check "GET /healthz" 0 /tmp/smoke-health.json
curl -s --noproxy '*' "$ROOT/readyz"  -o /tmp/smoke-ready.json  && check "GET /readyz"  0 /tmp/smoke-ready.json
info "DB=$(get /tmp/smoke-health.json 'd["data"]["dbDriver"]') Cache=$(get /tmp/smoke-health.json 'd["data"]["cacheImpl"]')"

# --------------------------------------------------------------------------- 2. 登录
step "登录"
"${CURL[@]}" -X POST "$BASE/auth/login" \
  -d "{\"username\":\"$USERNAME\",\"password\":\"$PASSWORD\"}" -o /tmp/smoke-login.json
check "POST /auth/login" 0 /tmp/smoke-login.json
TOKEN=$(get /tmp/smoke-login.json 'd["data"]["accessToken"]')
if [ -z "${TOKEN:-}" ]; then bad "未拿到 accessToken，后续用例无法继续"; exit 1; fi
ok "拿到 accessToken（${#TOKEN} 字符）"
C=("${CURL[@]}" -H "Authorization: Bearer $TOKEN")

# 错误密码必须失败，且不能泄露"账号是否存在"
"${CURL[@]}" -X POST "$BASE/auth/login" -d "{\"username\":\"$USERNAME\",\"password\":\"wrong-password\"}" -o /tmp/smoke-login-bad.json
check "错误密码 → 40100" 40100 /tmp/smoke-login-bad.json

# --------------------------------------------------------------------------- 3. 鉴权
step "鉴权"
"${C[@]}" "$BASE/auth/me" -o /tmp/smoke-me.json && check "GET /auth/me" 0 /tmp/smoke-me.json
info "角色=$(get /tmp/smoke-me.json 'd["data"]["role"]["name"]') 权限数=$(get /tmp/smoke-me.json 'len(d["data"]["permissions"])')"
"${CURL[@]}" "$BASE/auth/me" -o /tmp/smoke-anon.json
check "未带令牌 → 40100" 40100 /tmp/smoke-anon.json
"${C[@]}" -X POST "$BASE/users/invite" -d '{"username":"probe","name":"probe","email":"p@x.cn","role":"dev"}' -o /tmp/smoke-forbidden.json
check "ops 建用户 → 40300" 40300 /tmp/smoke-forbidden.json

# --------------------------------------------------------------------------- 3b. 夹具发现
step "夹具发现"
#
# 夹具**自动发现**，不再写死项目标识。
#
# 写死过一次 `PKEY=user-web`：那个项目在某次重建后不存在了，于是
#   · 第 4/5 节的 6 条断言全红（40400 项目不存在）—— 至少还看得见；
#   · 更糟的是第 7/8 节的部署与回滚：它们靠 `${SID:-}` 兜底，SID 取不到就
#     **整段静默跳过**。一次"跑完了"的冒烟其实既没验部署、也没验回滚，
#     输出里只有几条红，读起来像是"夹具又过期了"的老毛病，实则是覆盖度塌了一半。
# 写死的夹具必然随数据漂移，改成从接口现查，取不到就如实说取不到。
#
# ⚠️ 构建夹具与部署夹具是**两件事**，不能共用一个项目：
#   · 构建要求"平台能构建"；
#   · 部署要求"(test 环境, 该项目) 有服务实例"，因为 POST /deployments 要的正是这两个键。
"${C[@]}" "$BASE/projects?page=1&pageSize=200" -o /tmp/smoke-projects.json && check "GET /projects" 0 /tmp/smoke-projects.json
info "项目总数=$(get /tmp/smoke-projects.json 'd["data"]["total"]')"

# ① 构建夹具：必须是 build_method != jenkins 的项目。
#    Jenkins 项目的 POST /builds 被后端明确拒绝（40901「平台不会代为执行 docker 构建」），
#    拿它去测构建必然是红的 —— 那不是接口坏了，是选题错了。
#    （可用 SMOKE_PKEY=xxx 指定。）
BUILD_PKEY="${SMOKE_PKEY:-$(get /tmp/smoke-projects.json 'next((p["key"] for p in d["data"]["list"] if p.get("buildMethod") != "jenkins"), "")')}"

# ② 部署/服务夹具：test 环境里**有服务实例**、且项目**有镜像**的第一个。
#
#    这里要区分**读夹具**与**写夹具**，两者条件不同：
#      · 读（第 4/6 节）：有服务实例 + 项目有镜像即可 —— 这些接口都是只读的，随便挑；
#      · 写（第 7/8 节）：还要求"该服务**至少有两个不同镜像**"。不带目标版本的回滚要
#        "该服务上一次**不同镜像**的成功部署"（cd.go resolveRollbackTarget），
#        只有一个镜像的服务必然回 40901「找不到可回滚的历史版本」——
#        那不是接口坏了，是这个服务本来就没有上一版。
#        实测踩过：夹具挑中只有一个镜像的项目，部署段全绿、回滚段恒红。
#
# ⚠️ **第 7/8 节是真实的集群写入，不是干跑。**
#
# 这一点之前被写错过，记在这里免得再犯：`environment.kubeconfig_credential_id` 为 NULL
# 并**不代表**这个环境的部署是空转 —— `clusterRefOf`（cd.go:830）只在环境**自带**凭据时
# 才填 EnvID，否则凭据解析会回退去查平台集群表（store/cluster_cred.go resolveClusterCredID）。
# 实测：env `test` 的 cluster=`cluster-local`，平台集群表里那一条是有凭据的，
# 于是对 test 发版会**真的改掉集群里那个工作负载的镜像**。
#
# 所以第 7/8 节默认**不跑**，必须显式 SMOKE_WRITE=1 开启，且动手前会把"改谁、改成什么"打出来：
#   SMOKE_WRITE=1 bash scripts/smoke.sh
"${C[@]}" "$BASE/environments/test/services" -o /tmp/smoke-envsvc.json && check "GET /environments/test/services" 0 /tmp/smoke-envsvc.json
"${C[@]}" "$BASE/images?pageSize=200" -o /tmp/smoke-images-all.json && check "GET /images" 0 /tmp/smoke-images-all.json
CANDS=$(python3 -c '
import json
svcs = json.load(open("/tmp/smoke-envsvc.json"))["data"]["list"]
keys = {i["projectKey"] for i in json.load(open("/tmp/smoke-images-all.json"))["data"]["list"]}
print(" ".join("%s:%s" % (s["id"], s["projectKey"]) for s in svcs if s["projectKey"] in keys))
' 2>/dev/null)
READ_PICK=""
WRITE_PICK=""
for c in $CANDS; do
  cid="${c%%:*}"
  ckey="${c##*:}"
  "${C[@]}" "$BASE/services/$cid/versions" -o /tmp/smoke-versions-probe.json
  nimg=$(python3 -c '
import json
try:
    d = json.load(open("/tmp/smoke-versions-probe.json"))["data"]
except Exception:
    print(0)
    raise SystemExit
lst = d["list"] if isinstance(d, dict) else d
print(len({r.get("image") for r in lst if r.get("image")}))
' 2>/dev/null)
  if [ -z "$READ_PICK" ]; then READ_PICK="$cid|$ckey"; fi
  # 注意用 if 而不是 `[ ... ] && VAR=...`：bash 3.2 下把赋值放在 && 右边，
  # 一旦左侧为假就会让整个语句返回非 0，在 set -e 的场景里会静默改变控制流。
  if [ -z "$WRITE_PICK" ] && [ "${nimg:-0}" -ge 2 ] 2>/dev/null; then WRITE_PICK="$cid|$ckey"; fi
done
READ_SID="${READ_PICK%%|*}"
READ_PKEY="${READ_PICK##*|}"
if [ "${SMOKE_WRITE:-0}" = "1" ]; then
  SID="${WRITE_PICK%%|*}"
  DEP_PKEY="${WRITE_PICK##*|}"
else
  SID=""
  DEP_PKEY=""
fi

if [ -z "${BUILD_PKEY:-}" ]; then
  info "构建夹具：（无）—— 纳管项目全部是 Jenkins 构建，第 5 节只验「平台拒绝代跑」这条契约"
else
  info "构建夹具：${BUILD_PKEY}（平台可构建）"
fi
if [ -z "${READ_SID:-}" ]; then
  bad "test 环境里没有「有服务实例且有镜像」的项目，服务相关接口无法验证"
else
  info "服务读夹具：${READ_PKEY}@test serviceId=${READ_SID}"
fi
if [ "${SMOKE_WRITE:-0}" != "1" ]; then
  info "写夹具：（未启用）—— 第 7/8 节默认跳过，它们会真的改集群"
  info "      （要跑请用 SMOKE_WRITE=1；当前可用的写夹具：${WRITE_PICK:-无}）"
elif [ -z "${SID:-}" ]; then
  bad "test 环境里没有「有两个以上不同镜像」的服务，第 8 节回滚无法执行"
else
  info "部署夹具：${DEP_PKEY}@test serviceId=${SID} —— 本节会**真实写入集群**"
fi

# --------------------------------------------------------------------------- 4. 项目与镜像
step "项目与镜像"
# 详情/分支只要求"项目真实存在"，构建夹具有就用它。
PKEY="${BUILD_PKEY:-$READ_PKEY}"
"${C[@]}" "$BASE/projects/$PKEY" -o /tmp/smoke-project.json && check "GET /projects/$PKEY" 0 /tmp/smoke-project.json
"${C[@]}" "$BASE/projects/$PKEY/branches" -o /tmp/smoke-branches.json && check "GET /projects/$PKEY/branches" 0 /tmp/smoke-branches.json
"${C[@]}" "$BASE/images?projectKey=$READ_PKEY&pageSize=1" -o /tmp/smoke-images.json && check "GET /images?projectKey=$READ_PKEY" 0 /tmp/smoke-images.json
IMGID=$(get /tmp/smoke-images.json 'd["data"]["list"][0]["id"]')
info "$READ_PKEY 的镜像数=$(get /tmp/smoke-images.json 'd["data"]["total"]')，取 imageId=$IMGID"

# --------------------------------------------------------------------------- 5. 构建
step "构建（一期核心）"
if [ -z "${BUILD_PKEY:-}" ]; then
  # 没有可构建的项目时**不静默跳过**：至少把这条真实存在的契约钉住。
  # 平台若替 Jenkins 项目跑一次 docker 构建，会落一条与 Jenkins 毫无关系的"成功"记录
  # ——这是本模块最容易出、也最难看出来的错（详见 ci.go TriggerBuild 的注释）。
  # 注意用的是"提交必然被拒"的形态：请求被后端挡下，不会真的触发任何构建。
  "${C[@]}" -X POST "$BASE/builds" \
    -d "{\"projectKey\":\"$READ_PKEY\",\"branch\":\"master\",\"idempotencyKey\":\"smoke-jenkins-guard-$(date +%s)\"}" \
    -o /tmp/smoke-build-jenkins.json
  check "Jenkins 项目 POST /builds → 40901（平台不代跑 docker 构建）" 40901 /tmp/smoke-build-jenkins.json
  info "拒绝原文：$(get /tmp/smoke-build-jenkins.json 'd["message"]')"
  info "跳过：完整构建链路（提交 → 幂等 → 详情 → SSE）需要一个 build_method != jenkins 的项目，"
  info "      当前一个都没有。构建的**读路径**仍在下面验证，不留整段空白。"

  # 读路径：库里已有 Jenkins 落库的构建记录，列表 / 详情 / 日志流三个接口都要能读。
  "${C[@]}" "$BASE/builds?page=1&pageSize=3" -o /tmp/smoke-builds.json && check "GET /builds" 0 /tmp/smoke-builds.json
  HID=$(get /tmp/smoke-builds.json 'd["data"]["list"][0]["id"]')
  if [ -n "${HID:-}" ]; then
    "${C[@]}" "$BASE/builds/$HID" -o /tmp/smoke-build-read.json && check "GET /builds/${HID}（读历史构建）" 0 /tmp/smoke-build-read.json
    info "构建 #$(get /tmp/smoke-build-read.json 'd["data"]["buildNo"]') 状态=$(get /tmp/smoke-build-read.json 'd["data"]["status"]') 镜像=$(get /tmp/smoke-build-read.json 'd["data"]["image"]')"
    info "SSE 前 3 行："
    "${C[@]}" -N --max-time 4 "$BASE/builds/$HID/logs/stream" 2>/dev/null | head -3 | sed 's/^/      /'
  else
    bad "库里没有任何构建记录，构建读路径未能验证"
  fi
else
  IDEM="smoke-$BUILD_PKEY-$(date +%s)"
  "${C[@]}" -X POST "$BASE/builds" \
    -d "{\"projectKey\":\"$BUILD_PKEY\",\"branch\":\"master\",\"params\":{\"BUILD_MODE\":\"release\"},\"idempotencyKey\":\"$IDEM\"}" \
    -o /tmp/smoke-build.json
  check "POST /builds" 0 /tmp/smoke-build.json
  BID=$(get /tmp/smoke-build.json 'd["data"]["id"]')
  info "构建 id=$BID buildNo=$(get /tmp/smoke-build.json 'd["data"]["buildNo"]') 流=$(get /tmp/smoke-build.json 'd["data"]["logStreamUrl"]')"

  # 幂等：同一 idempotencyKey 再提交，必须复用同一条记录
  "${C[@]}" -X POST "$BASE/builds" \
    -d "{\"projectKey\":\"$BUILD_PKEY\",\"branch\":\"master\",\"idempotencyKey\":\"$IDEM\"}" -o /tmp/smoke-build2.json
  BID2=$(get /tmp/smoke-build2.json 'd["data"]["id"]')
  if [ "$BID" = "$BID2" ]; then ok "幂等命中，复用构建 id=$BID2"; else bad "幂等失效：$BID vs $BID2"; fi

  "${C[@]}" "$BASE/builds/$BID" -o /tmp/smoke-build-detail.json && check "GET /builds/$BID" 0 /tmp/smoke-build-detail.json
  info "状态=$(get /tmp/smoke-build-detail.json 'd["data"]["status"]') 镜像=$(get /tmp/smoke-build-detail.json 'd["data"]["image"]')"
  info "SSE 前 6 行："
  "${C[@]}" -N --max-time 6 "$BASE/builds/$BID/logs/stream" 2>/dev/null | head -6 | sed 's/^/      /'
fi

# --------------------------------------------------------------------------- 6. 环境与服务实例
step "环境与服务实例"
"${C[@]}" "$BASE/environments" -o /tmp/smoke-envs.json && check "GET /environments" 0 /tmp/smoke-envs.json
"${C[@]}" "$BASE/environments/test/resources" -o /tmp/smoke-envres.json && check "GET /environments/test/resources" 0 /tmp/smoke-envres.json
"${C[@]}" -X POST "$BASE/environments/test/test-connection" -o /tmp/smoke-envconn.json
check "POST /environments/test/test-connection" 0 /tmp/smoke-envconn.json

if [ -n "${READ_SID:-}" ]; then
  info "${READ_PKEY}@test 服务实例 id=${READ_SID}"
  "${C[@]}" "$BASE/services/$READ_SID" -o /tmp/smoke-svc.json && check "GET /services/$READ_SID" 0 /tmp/smoke-svc.json
  "${C[@]}" "$BASE/services/$READ_SID/pods" -o /tmp/smoke-pods.json && check "GET /services/$READ_SID/pods" 0 /tmp/smoke-pods.json
  "${C[@]}" "$BASE/services/$READ_SID/versions" -o /tmp/smoke-versions.json && check "GET /services/$READ_SID/versions" 0 /tmp/smoke-versions.json
  "${C[@]}" "$BASE/services/$READ_SID/params/history" -o /tmp/smoke-paramhist.json && check "GET /services/$READ_SID/params/history" 0 /tmp/smoke-paramhist.json
fi

# --------------------------------------------------------------------------- 7. 部署
step "部署（一期核心）"
if [ "${SMOKE_WRITE:-0}" != "1" ]; then
  info "跳过：本段会对 test 环境发起**真实部署**，直接改掉集群里那个工作负载的镜像。"
  info "      默认不跑。要跑请显式开启，并在开启前确认它的靶点："
  info "        SMOKE_WRITE=1 bash scripts/smoke.sh"
  info "      当前可用的写夹具：${WRITE_PICK:-（无）}"
elif [ -z "${SID:-}" ]; then
  bad "没有可用的写夹具，第 7/8 节无法执行"
else
  DKEY="smoke-deploy-$(date +%s)"
  if [ -n "${IMGID:-}" ]; then
    DDATA="{\"envKey\":\"test\",\"projectKey\":\"$DEP_PKEY\",\"imageRecordId\":\"$IMGID\",\"remark\":\"冒烟测试部署\",\"idempotencyKey\":\"$DKEY\"}"
  else
    DDATA="{\"envKey\":\"test\",\"projectKey\":\"$DEP_PKEY\",\"remark\":\"冒烟测试部署\",\"idempotencyKey\":\"$DKEY\"}"
  fi
  # 动手之前先把"改谁、改成什么"打出来。写集群的操作不该靠读脚本才知道它要做什么。
  info "即将对集群写入：env=test（cluster=cluster-local）project=$DEP_PKEY serviceId=$SID"
  info "                 镜像 → $(get /tmp/smoke-images.json 'd["data"]["list"][0]["image"]')"
  "${C[@]}" -X POST "$BASE/deployments" -d "$DDATA" -o /tmp/smoke-deploy.json
  check "POST /deployments" 0 /tmp/smoke-deploy.json
  DID=$(get /tmp/smoke-deploy.json 'd["data"]["id"]')
  info "部署单 id=$DID status=$(get /tmp/smoke-deploy.json 'd["data"]["status"]')"
  "${C[@]}" "$BASE/deployments/$DID" -o /tmp/smoke-deploy-detail.json && check "GET /deployments/$DID" 0 /tmp/smoke-deploy-detail.json
  info "部署详情状态=$(get /tmp/smoke-deploy-detail.json 'd["data"]["status"]')"
  info "SSE 前 6 行："
  "${C[@]}" -N --max-time 5 "$BASE/deployments/$DID/logs/stream" 2>/dev/null | head -6 | sed 's/^/      /'
  "${C[@]}" "$BASE/deployments?page=1&pageSize=3" -o /tmp/smoke-deploys.json && check "GET /deployments" 0 /tmp/smoke-deploys.json
  "${C[@]}" "$BASE/deployments/pending-approval" -o /tmp/smoke-pending.json
  check "GET /deployments/pending-approval（ops 有 deploy:*）" 0 /tmp/smoke-pending.json

  # 等部署走到终态。
  #
  # 超时给到 5 分钟，因为**这是真实集群的滚动更新**，不是模拟：实测 env `test`
  # 一次发版约 100 秒（拉镜像 + 滚动 + 就绪），test-152 上更慢的到过 345 秒。
  # 原来写 30×2s＝60 秒，在真集群上必然误报「未在预期时间内收敛」。
  # 并发上限是 1，不等它结束就去回滚会（正确地）撞上"该服务有部署进行中"的 40901。
  info "等待部署 $DID 进入终态（真集群滚动更新，最长等 5 分钟）…"
  DST=""
  for _ in $(seq 1 150); do
    "${C[@]}" "$BASE/deployments/$DID" -o /tmp/smoke-deploy-wait.json
    DST=$(get /tmp/smoke-deploy-wait.json 'd["data"]["status"]')
    case "$DST" in
      # interrupted（平台重启打断）也算终态：它不会自行恢复，继续等只会空转到超时，
      # 最后报一句"未在预期时间内收敛"——把"平台重启过"说成了"部署卡住了"。
      success | failed | canceled | rolled_back | interrupted) break ;;
    esac
    sleep 2
  done
  info "部署终态：$DST"
  case "$DST" in
    success) ok "部署成功完成" ;;
    failed) bad "部署失败" ;;
    interrupted) bad "部署被中断（status=interrupted）—— 本次运行期间平台重启过，请重跑冒烟" ;;
    *) bad "部署未在预期时间内收敛（status=${DST}）" ;;
  esac
fi

# 越权：dev 角色只能发 test/staging，试发 prod 必须被拒。
# 这一条**不受 SMOKE_WRITE 门控**：请求必然被权限层挡下（40300），不产生任何写操作，
# 是纯粹的权限契约验证 —— 放进写门控里会因为"默认不写集群"而白白丢掉。
DEVTOK=$("${CURL[@]}" -X POST "$BASE/auth/login" -d '{"username":"dev01","password":"changeme"}' | get /dev/stdin 'd["data"]["accessToken"]')
if [ -n "${DEVTOK:-}" ]; then
  "${CURL[@]}" -H "Authorization: Bearer $DEVTOK" -X POST "$BASE/deployments" \
    -d "{\"envKey\":\"prod\",\"projectKey\":\"${DEP_PKEY:-$READ_PKEY}\",\"remark\":\"越权探测\"}" -o /tmp/smoke-deploy-prod.json
  # 这里断言的是**环境级权限**先于一切资源校验生效：即使该项目在 prod 根本没有服务实例，
  # 也必须先返回 40300，而不是 40400 —— 否则探测结论会随数据变化而失去意义。
  check "dev 发正式区部署 → 40300（环境级权限）" 40300 /tmp/smoke-deploy-prod.json
fi

# --------------------------------------------------------------------------- 8. 回滚
step "回滚"
if [ "${SMOKE_WRITE:-0}" != "1" ]; then
  info "跳过：回滚会真的把集群里的镜像换回上一版，和部署段同属写操作。"
  info "      SMOKE_WRITE=1 时才执行；夹具需「该服务至少有两个不同镜像」（见夹具发现的说明）。"
elif [ -n "${SID:-}" ]; then
  "${C[@]}" -X POST "$BASE/rollbacks" \
    -d "{\"serviceId\":\"$SID\",\"reason\":\"冒烟测试：人工介入回滚\",\"triggerType\":\"manual\"}" \
    -o /tmp/smoke-rollback.json
  check "POST /rollbacks" 0 /tmp/smoke-rollback.json
  info "回滚 id=$(get /tmp/smoke-rollback.json 'd["data"]["id"]') 目标镜像=$(get /tmp/smoke-rollback.json 'd["data"]["toImage"]')"
  "${C[@]}" "$BASE/rollbacks?page=1&pageSize=3" -o /tmp/smoke-rollbacks.json && check "GET /rollbacks" 0 /tmp/smoke-rollbacks.json
else
  bad "没有可用的写夹具，第 8 节回滚无法执行"
fi

# --------------------------------------------------------------------------- 9. 审计与系统
step "审计与系统"
"${C[@]}" "$BASE/audit-logs?page=1&pageSize=5" -o /tmp/smoke-audit.json && check "GET /audit-logs" 0 /tmp/smoke-audit.json
info "审计总数=$(get /tmp/smoke-audit.json 'd["data"]["total"]')"
"${C[@]}" -X POST "$BASE/audit-logs/export" -d '{"limit":50}' -o /tmp/smoke-export.json
check "POST /audit-logs/export" 0 /tmp/smoke-export.json
"${C[@]}" "$BASE/system/stats" -o /tmp/smoke-stats.json && check "GET /system/stats" 0 /tmp/smoke-stats.json
"${C[@]}" "$BASE/system/integrations" -o /tmp/smoke-integrations.json && check "GET /system/integrations" 0 /tmp/smoke-integrations.json
"${C[@]}" "$BASE/notify-channels" -o /tmp/smoke-notify.json
check "ops 读通知渠道 → 40300" 40300 /tmp/smoke-notify.json
info "metrics 前 4 行："
curl -s --noproxy '*' "$ROOT/metrics" | head -4 | sed 's/^/      /'

# --------------------------------------------------------------------------- 9b. 管理员视角
step "管理员视角（admin 账号）"
ATOK=$("${CURL[@]}" -X POST "$BASE/auth/login" -d '{"username":"admin","password":"changeme"}' | get /dev/stdin 'd["data"]["accessToken"]')
if [ -z "${ATOK:-}" ]; then
  bad "admin 账号登录失败"
else
  ok "admin 登录成功"
  AC=("${CURL[@]}" -H "Authorization: Bearer $ATOK")
  "${AC[@]}" "$BASE/auth/me" -o /tmp/smoke-me-admin.json && check "GET /auth/me（admin）" 0 /tmp/smoke-me-admin.json
  "${AC[@]}" "$BASE/users?page=1&pageSize=3" -o /tmp/smoke-users.json && check "GET /users" 0 /tmp/smoke-users.json
  info "用户总数=$(get /tmp/smoke-users.json 'd["data"]["total"]')"
  "${AC[@]}" "$BASE/roles" -o /tmp/smoke-roles.json && check "GET /roles" 0 /tmp/smoke-roles.json
  "${AC[@]}" "$BASE/permissions" -o /tmp/smoke-perms.json && check "GET /permissions" 0 /tmp/smoke-perms.json
  info "权限点数量=$(get /tmp/smoke-perms.json 'len(d["data"])')"
  "${AC[@]}" "$BASE/notify-channels" -o /tmp/smoke-notify-admin.json && check "GET /notify-channels（admin）" 0 /tmp/smoke-notify-admin.json
  "${AC[@]}" "$BASE/models" -o /tmp/smoke-models.json && check "GET /models" 0 /tmp/smoke-models.json
  "${AC[@]}" "$BASE/system/config" -o /tmp/smoke-config.json && check "GET /system/config" 0 /tmp/smoke-config.json
  "${AC[@]}" -X POST "$BASE/projects/$PKEY/validate" -o /tmp/smoke-validate.json
  check "POST /projects/$PKEY/validate（需 project:write）" 0 /tmp/smoke-validate.json

  AID=$("${AC[@]}" "$BASE/audit-logs?page=1&pageSize=1" | get /dev/stdin 'd["data"]["list"][0]["id"]')
  "${AC[@]}" "$BASE/audit-logs/$AID" -o /tmp/smoke-audit-one.json && check "GET /audit-logs/$AID" 0 /tmp/smoke-audit-one.json

  # 管理动作的不变量：不能改 admin 角色（避免把自己锁死）
  "${AC[@]}" -X PUT "$BASE/roles/admin/permissions" -d '{"perms":["project:read"]}' -o /tmp/smoke-role-admin.json
  check "改 admin 角色权限 → 40300" 40300 /tmp/smoke-role-admin.json

  # 不能停用当前登录账号。注意：这里必须用「自己」的 id，
  # 随便挑一个用户停用是合法操作，而且会连带吊销那个账号的会话
  # （实测踩过：停用了演示账号，后面的登出用例因此拿到 40100）。
  AMEID=$(get /tmp/smoke-me-admin.json 'd["data"]["id"]')
  "${AC[@]}" -X PATCH "$BASE/users/$AMEID" -d '{"enabled":false}' -o /tmp/smoke-self-disable.json
  check "停用当前登录账号 → 40000" 40000 /tmp/smoke-self-disable.json
fi

# --------------------------------------------------------------------------- 10. 登出与会话吊销
step "登出（会话吊销）"
"${C[@]}" -X POST "$BASE/auth/logout" -o /tmp/smoke-logout.json && check "POST /auth/logout" 0 /tmp/smoke-logout.json
"${C[@]}" "$BASE/auth/me" -o /tmp/smoke-me-after.json
check "登出后 /auth/me → 40100" 40100 /tmp/smoke-me-after.json

# --------------------------------------------------------------------------- 汇总
printf '\n'
if [ "$FAILED" -eq 0 ]; then printf '\033[1;32m全部通过\033[0m\n'; exit 0; fi
printf '\033[1;31m%d 项失败\033[0m\n' "$FAILED"; exit 1
