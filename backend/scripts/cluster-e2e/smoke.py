#!/usr/bin/env python3
"""集群接入七条接口的冒烟断言。

防「假绿」的三条自保（这是本项目踩过的坑，不是形式主义）：
  1. **预检**：后端 /healthz 与本地假 K8s 都必须先探通，否则直接 abort —— 
     不探通就往下跑，会把"请求全失败"变成一堆看似通过的断言。
  2. **不落盘再读**：全部断言都在内存里做，不存在"读到上一轮旧产物"的可能。
  3. **断言具体内容**：不只看 code==0，还看 nodes/version/是否真的区分了 401 与不可达 —— 
     "接口返回 200"与"接口干对了事"是两件事。

用法：
    # 1) 起依赖与后端（见 backend/README.md），确保 /healthz 通
    # 2) 起假 K8s：python3 fake_k8s.py &
    # 3) python3 smoke.py
"""
import json
import os
import ssl
import sys
import urllib.error
import urllib.request

# 路径按脚本所在目录解析，别写死 /tmp（它会被清理，且换机器就失效）
HERE = os.path.dirname(os.path.abspath(__file__))
BASE = os.environ.get("YN_BASE", "http://127.0.0.1:8080/api/v1")
KUBECONFIG = open(os.path.join(HERE, "kubeconfig.yaml")).read()
FORBIDDEN_FLAG = os.path.join(HERE, "forbidden.flag")

# 绕过沙箱代理：本机 curl 走代理会得到 502，urllib 同理。
_CTX = ssl.create_default_context()
_CTX.check_hostname = False
_CTX.verify_mode = ssl.CERT_NONE
_OPENER = urllib.request.build_opener(
    urllib.request.ProxyHandler({}),
    urllib.request.HTTPSHandler(context=_CTX),
)

passed, failed = [], []


def check(name, cond, detail=""):
    if cond:
        passed.append(name)
        print(f"  PASS  {name}")
    else:
        failed.append(name)
        print(f"  FAIL  {name}  {detail}")


def req(method, path, body=None, token=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(BASE + path, data=data, method=method)
    r.add_header("Content-Type", "application/json")
    if token:
        r.add_header("Authorization", "Bearer " + token)
    try:
        with _OPENER.open(r, timeout=30) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        return json.loads(e.read().decode())


def as_list(data):
    """列表响应有两种形态（裸数组 / {list:[...]}），本脚本两种都要能读。

    这不是多余的防御：同一个后端里确实两种都存在（/clusters 返裸数组、
    /system/integrations 返分页体），前端靠 unwrapList/unwrapPage 抹平。
    脚本若只认一种，就会把"形状不同"误报成"接口坏了"。
    """
    if isinstance(data, dict):
        return data.get("list") or []
    if isinstance(data, list):
        return data
    return []


def login(username, password):
    r = req("POST", "/auth/login", {"username": username, "password": password})
    return (r.get("data") or {}).get("accessToken"), r


def kubeconfig_with_token(token):
    return KUBECONFIG.replace("token: yn-test-token", "token: " + token)


def kubeconfig_with_server(server):
    return KUBECONFIG.replace("https://127.0.0.1:16443", server)


# ------------------------------------------------------------------ 预检
print("== 预检 ==")
preflight_ok = True
try:
    with _OPENER.open("http://127.0.0.1:8080/healthz", timeout=5) as resp:
        check("后端 /healthz 返回 200", resp.status == 200)
except Exception as e:  # noqa: BLE001
    check("后端 /healthz 返回 200", False, repr(e))
    preflight_ok = False

try:
    with _OPENER.open("https://127.0.0.1:16443/version", timeout=5) as resp:
        _ = resp.read()
        check("本地假 K8s 未鉴权返回 401（说明服务在跑）", False, "竟然返回 200")
except urllib.error.HTTPError as e:
    check("本地假 K8s 未鉴权返回 401（说明服务在跑）", e.code == 401, f"code={e.code}")
except Exception as e:  # noqa: BLE001
    check("本地假 K8s 未鉴权返回 401（说明服务在跑）", False, repr(e))
    preflight_ok = False

if not preflight_ok:
    print("\n预检未通过，中止（继续跑只会产出一堆假绿）")
    sys.exit(2)

# ------------------------------------------------------------------ 鉴权
print("== 鉴权与权限 ==")
r = req("GET", "/clusters")
check("未登录访问 /clusters 返回 40100", r.get("code") == 40100, r)

login = req("POST", "/auth/login", {"username": "admin", "password": "changeme"})
admin = (login.get("data") or {}).get("accessToken")
check("admin 登录拿到 accessToken", bool(admin), login)

ops_login = req("POST", "/auth/login", {"username": "ops", "password": "changeme"})
ops = (ops_login.get("data") or {}).get("accessToken")
check("ops 登录拿到 accessToken", bool(ops), ops_login)

# 先清掉上一轮**本脚本自己**的残留：脚本中途崩溃时收尾段不会执行，
# 残留项会让"列表只含 1 个"这类断言在本轮无故失败（假红）。
#
# ⚠️ 只删本脚本建的（见 SCRIPT_PREFIX），**绝不能删整个列表**。
# 早先这里是 `for stale in 全列表: DELETE`，那会在开发机上一把删掉用户
# 真实接入的集群（`cluster.local` 这类，凭据是不可再生的）—— 冒烟脚本被人在
# 有真实数据的库上跑一次就造成不可逆的数据丢失。清理必须按"自己建的"限定范围。
#
# 也不用 `?dryRun=` 之类的预览：前缀就是本脚本独占的命名空间，
# 而下面那条"外部集群未被删除"的断言会兜住任何手滑。
# 本脚本建的所有集群标识都必须落在 SCRIPT_PREFIX 下 ——
# 起跑清理与收尾清理都靠它界定"哪些是本脚本的"。**这是脚本能在有真实数据的库上
# 安全运行的前提**：不加前缀约束的清理会把用户真实接入的集群一起删掉。
SCRIPT_PREFIX = "yn-smoke-"
# 手工复现问题时建的键也用同一命名空间，好让清理能覆盖到
REPRO_PREFIX = "yn-repro-"


def _is_mine(k):
    return k.startswith(SCRIPT_PREFIX) or k.startswith(REPRO_PREFIX)


def _cluster_keys():
    return [c["key"] for c in as_list(req("GET", "/clusters", token=admin).get("data"))]

# 清理前先给"别人的集群"拍个快照 —— 下面要断言它们一个不少
EXTERNAL = [k for k in _cluster_keys() if not _is_mine(k)]

for k in [k for k in _cluster_keys() if _is_mine(k)]:
    req("DELETE", "/clusters/" + k, token=admin)
# 环境也按前缀清（历史遗留的固定名 + 带时间戳的都覆盖）
for env in as_list(req("GET", "/environments?pageSize=200", token=admin).get("data")):
    ek = str(env.get("key") or "")
    if ek.startswith(SCRIPT_PREFIX):
        req("DELETE", "/environments/" + ek, token=admin)

check("起跑时本脚本的残留集群已清空",
      [k for k in _cluster_keys() if _is_mine(k)] == [], _cluster_keys())
# 这一条是**保护真实数据的守卫**：开发机上常已接入真实集群（凭据不可再生），
# 清理只能动自己的命名空间。它不是"脚本用的"，是防脚本手滑的刹车。
missing = [k for k in EXTERNAL if k not in _cluster_keys()]
check("清理未波及外部集群（真实数据未被删除）", missing == [], missing)
print(f"  （库里另有 {len(EXTERNAL)} 个非本脚本的集群，本次不动它们：{EXTERNAL}）" if EXTERNAL
      else "  （库里无外部集群）")

r = req("POST", "/clusters", {"clusters": []}, token=ops)
check("ops 无 user:manage，POST /clusters 返回 40300", r.get("code") == 40300, r)

r = req("GET", "/clusters", token=ops)
check("ops 有 env:read，GET /clusters 返回 0", r.get("code") == 0, r)

# ------------------------------------------------------------------ 解析
print("== kubeconfig 解析 ==")
r = req("POST", "/clusters/parse", {"kubeconfig": KUBECONFIG}, token=admin)
d = r.get("data") or {}
check("解析成功", r.get("code") == 0 and d.get("ok") is True, r)
check("解析出 3 个 context", len(d.get("contexts") or []) == 3, d.get("contexts"))
check("currentContext 正确", d.get("currentContext") == "yn-test-ctx", d.get("currentContext"))
broken = [c for c in (d.get("contexts") or []) if c["context"] == "yn-broken-ctx"]
check("引用了不存在的集群的 context 被标出 problem",
      bool(broken) and broken[0].get("problem"), broken)
check("解析出的 apiServer 正确",
      all(c.get("apiServer") == "https://127.0.0.1:16443"
          for c in (d.get("contexts") or []) if c["context"] != "yn-broken-ctx"),
      d.get("contexts"))

# 解析失败的**行为在 2026-09-29 改过**：原先返回 40000 抛错，前端因此只能弹一句 toast，
# 而它那个内联错误面板（api/cluster.ts 注释里写明的 ok=false + errors 契约）
# 永远走不到（不会出现的分支 = 假承诺）。现在按契约返回 ok=false。
#
# 断言集中在下面「列表字段必须是 []」一节（那里同时校验 code=0 / ok=false / errors 非空），
# 这里只留这条注释指路，不再重复断言 —— 同一件事在两处断言，改行为时要改两遍，容易漏一处。


# ------------------------------------------------------------------ 接入（真探测）
print("== 接入：真探测 ==")
KEY = SCRIPT_PREFIX + "fake-test"
r = req("POST", "/clusters", {"clusters": [
    {"key": KEY, "name": "本地假集群", "context": "yn-test-ctx", "kubeconfig": KUBECONFIG}
]}, token=admin)
res = r.get("data") or {}
cl = (res.get("clusters") or [{}])[0]
check("接入返回 1 个集群", len(res.get("clusters") or []) == 1, res)
check("未通过探测的项为空", not res.get("errors"), res.get("errors"))
check("状态为 connected", cl.get("status") == "connected", cl)
check("读到 K8s 版本 v1.29.6", cl.get("k8sVersion") == "v1.29.6", cl)
check("读到节点数 3", cl.get("nodes") == 3, cl)
check("读到 Ready 节点数 2（node-03 未就绪）", cl.get("nodesReady") == 2, cl)
check("读到命名空间数 14", cl.get("namespaceCount") == 14, cl)
check("apiServer 落库正确", cl.get("apiServer") == "https://127.0.0.1:16443", cl)
check("kubeconfigCluster 取自 kubeconfig（fake-cluster）", cl.get("kubeconfigCluster") == "fake-cluster", cl)
check("hasKubeconfig 为 true", cl.get("hasKubeconfig") is True, cl)
check("响应里不含任何 kubeconfig 明文",
      "yn-test-token" not in json.dumps(r) and "insecure-skip-tls-verify" not in json.dumps(r), r)

r = req("POST", "/clusters", {"clusters": [
    {"key": "yn-bad-token", "name": "错凭据", "context": "yn-test-ctx",
     "kubeconfig": kubeconfig_with_token("wrong-token")}
]}, token=admin)
res = r.get("data") or {}
msg = ((res.get("errors") or [{}])[0]).get("message", "")
check("错凭据不落库（clusters 为空）", not res.get("clusters"), res)
check("错凭据被识别为 unauthorized（而不是含混的'连接失败'）",
      "凭据被集群拒绝" in msg or "401" in msg, msg)

r = req("POST", "/clusters", {"clusters": [
    {"key": "yn-unreachable", "name": "不可达", "context": "yn-test-ctx",
     "kubeconfig": kubeconfig_with_server("https://127.0.0.1:16999")}
]}, token=admin)
msg = (((r.get("data") or {}).get("errors") or [{}])[0]).get("message", "")
check("不可达集群被识别为 unreachable 且报出真实原因",
      "无法访问" in msg or "超时" in msg, msg)

r = req("POST", "/clusters", {"clusters": [
    {"key": KEY, "name": "重复", "context": "yn-test-ctx", "kubeconfig": KUBECONFIG}
]}, token=admin)
msg = (((r.get("data") or {}).get("errors") or [{}])[0]).get("message", "")
check("同标识重复接入被拒并说明原因", "已被占用" in msg, msg)

r = req("POST", "/clusters", {"clusters": [
    {"key": "Bad_Key!", "name": "非法标识", "context": "yn-test-ctx", "kubeconfig": KUBECONFIG}
]}, token=admin)
msg = (((r.get("data") or {}).get("errors") or [{}])[0]).get("message", "")
check("非法标识被拒（小写字母数字 . _ -）", "只能用小写字母" in msg, msg)

# ------------------------------------------------------------------ 读取与探测
print("== 读取 / 探测 / 命名空间 ==")
r = req("GET", "/clusters", token=admin)
lst = as_list(r.get("data"))
# 「恰好 1 个」要按**本脚本建的**数，不能按总数 —— 库里可能已有真实集群。
# 写成 `len(lst) == 1` 会在有真实数据的库上一跑就红，而红的原因是脚本自己的假设错了。
mine = [x["key"] for x in lst if _is_mine(x["key"])]
check("本脚本接入的集群恰好 1 个", mine == [KEY], mine)
check("外部集群仍在列表里（未被清掉）",
      all(k in [x["key"] for x in lst] for k in EXTERNAL), EXTERNAL)
check("列表不含 kubeconfig 明文", "yn-test-token" not in json.dumps(r), r)

r = req("POST", f"/clusters/{KEY}/test", token=admin)
t = r.get("data") or {}
check("探测返回 ok=true", r.get("code") == 0 and t.get("ok") is True, r)
check("探测复读到 3 节点", t.get("nodes") == 3, t)
check("探测耗时 > 0（真的拨了一次）", (t.get("latencyMs") or 0) > 0, t)

r = req("GET", f"/clusters/{KEY}/namespaces", token=admin)
ns = as_list(r.get("data"))
check("命名空间列表 14 条", len(ns) == 14, ns)
check("命名空间含 yunnian-prod", "yunnian-prod" in ns, ns)

r = req("PUT", f"/clusters/{KEY}", {"name": "改过名字", "remark": "冒烟用"}, token=admin)
check("编辑名称与备注成功", (r.get("data") or {}).get("name") == "改过名字", r)
check("编辑不动凭据（hasKubeconfig 仍为 true）",
      (r.get("data") or {}).get("hasKubeconfig") is True, r)

# ------------------------------------------------------------------ 列表字段不得为 null
print("== 列表字段必须是 []，不能是 null（曾因此整页白屏）==")
# 为什么单独立一节：Go 的 nil 切片会序列化成 JSON `null`，而前端把这些字段声明成
# `string[]` 并直接 `.length` / `.map`。**一个字段是 null 就会在渲染期间抛 TypeError，
# React 随即卸载整棵树 → 整个平台白屏**（2026-09-29 实测：有集群但没被环境引用时，
# `usedByEnvs` 为 null，集群接入页白屏，侧边栏和顶栏一起消失）。
#
# 本节三处都是"空"的情形 —— 也就是原来最容易漏测、恰恰会返回 null 的情形。
# 这条断言是在事故之后补的：原来的断言只看了内容对不对，没看形状对不对。
r = req("GET", "/clusters", token=admin)
rows = as_list(r.get("data"))
check("列表非空（本节才有意义）", len(rows) > 0, rows)
for row in rows:
    v = row.get("usedByEnvs")
    check(f"usedByEnvs 是数组而非 null（{row.get('key')}）", isinstance(v, list), v)

r = req("POST", f"/clusters/{KEY}/test", token=admin)
check("探测结果 warnings 是数组而非 null", isinstance((r.get("data") or {}).get("warnings"), list), r)

r = req("POST", "/clusters/parse", {"kubeconfig": KUBECONFIG}, token=admin)
d = r.get("data") or {}
check("解析成功时 errors 是数组而非 null", isinstance(d.get("errors"), list), d.get("errors"))
check("解析结果 contexts 是数组", isinstance(d.get("contexts"), list), d.get("contexts"))

# 契约：解析失败是 ok=false + errors（HTTP 200），**不是** 40000 抛错。
# 早先后端 return errs.Invalid(...)，与 api/cluster.ts 的注释契约不符，
# 导致前端那个内联错误面板永远走不到（不会出现的分支 = 假承诺）。
r = req("POST", "/clusters/parse", {"kubeconfig": "这不是一份 kubeconfig"}, token=admin)
d = r.get("data") or {}
check("非法 kubeconfig 仍是 code=0（不抛 40000）", r.get("code") == 0, r.get("code"))
check("非法 kubeconfig 返回 ok=false", d.get("ok") is False, d.get("ok"))
check("非法 kubeconfig 的 errors 非空且是数组",
      isinstance(d.get("errors"), list) and len(d["errors"]) > 0, d.get("errors"))

# ------------------------------------------------------------------ 删除守卫
print("== 删除守卫 ==")
# 环境标识用时间戳：**environment 表也有"软删后同标识重建不了"的缺陷**
# （软删行仍占着 uk_env_key，与 README 里 project 那条是同一个成因）。
# 那是 CD 侧的既有问题、不在本次集群接入的范围内，所以这里用一个每轮都不同的 key，
# 让本脚本只测"集群删除守卫"，不与无关缺陷耦合。该缺陷另行上报。
ENVKEY = "yn-smoke-env-%d" % int(__import__("time").time())
r = req("POST", "/environments", {"key": ENVKEY, "name": "冒烟环境", "cluster": KEY,
                                  "namespace": "yunnian-test"}, token=admin)
check("建一个引用该集群的环境", r.get("code") == 0, r)

r = req("DELETE", f"/clusters/{KEY}", token=admin)
check("被环境引用时删除返回 40901", r.get("code") == 40901, r)
check("拒绝消息里点出引用者", ENVKEY in (r.get("message") or ""), r.get("message"))

r = req("GET", "/clusters", token=admin)
# 按 key 找，**不能取 [0]** —— 库里可能还有别人真实接入的集群，
# 列表顺序由后端决定，取下标会让断言依赖"库里恰好只有我这一条"这个假前提
# （实测：用户接入 cluster.local 后，[0] 就成了集群 A，脚本无端报红）。
row = next((c for c in as_list(r.get("data")) if c.get("key") == KEY), None)
check("列表里 usedByEnvs 标出引用环境", row is not None and row.get("usedByEnvs") == [ENVKEY],
      row.get("usedByEnvs") if row else r.get("data"))

r = req("DELETE", f"/environments/{ENVKEY}", token=admin)
check("删掉该环境", r.get("code") == 0, r)

r = req("DELETE", f"/clusters/{KEY}", token=admin)
check("无引用后删除成功", r.get("code") == 0, r)

r = req("GET", "/clusters", token=admin)
check("删除后本脚本的集群已清空", [k for k in _cluster_keys() if _is_mine(k)] == [], _cluster_keys())

# ------------------------------------------------------------------ 软删后同标识重建
print("== 软删后同标识重建（唯一索引只含 cluster_key，不复用会撞 40900）==")
r = req("POST", "/clusters", {"clusters": [
    {"key": KEY, "name": "重建", "context": "yn-test-ctx", "kubeconfig": KUBECONFIG}
]}, token=admin)
res = r.get("data") or {}
check("同一个标识可以重建（软删行被复活）", len(res.get("clusters") or []) == 1, res)
check("重建后仍能读到节点数", ((res.get("clusters") or [{}])[0]).get("nodes") == 3, res)

# ------------------------------------------------------------------ 连通但无权列节点
print("== 连通但无权列节点 → warnings，而不是判失败 ==")
FORBIDDEN_KEY = SCRIPT_PREFIX + "forbidden"
open(FORBIDDEN_FLAG, "w").close()
try:
    r = req("POST", "/clusters", {"clusters": [
        {"key": FORBIDDEN_KEY, "name": "无权列节点", "context": "yn-test-ctx",
         "kubeconfig": KUBECONFIG}
    ]}, token=admin)
    res = r.get("data") or {}
    cl = (res.get("clusters") or [{}])[0]
    check("接入仍然成功（连通性是好的）", len(res.get("clusters") or []) == 1, res)
    check("状态为 connected", cl.get("status") == "connected", cl)
    check("节点数留空为 null（不用 0 冒充）", cl.get("nodes") is None, cl)
    check("结论里如实说明无权列节点",
          "无权列出节点" in (cl.get("lastProbeMessage") or ""), cl.get("lastProbeMessage"))
    check("命名空间仍读到了", cl.get("namespaceCount") == 14, cl)

    r = req("POST", f"/clusters/{FORBIDDEN_KEY}/test", token=admin)
    t = r.get("data") or {}
    check("单次探测也把提示放进 warnings",
          bool(t.get("warnings")) and "无权列出节点" in t["warnings"][0], t)
finally:
    if os.path.exists(FORBIDDEN_FLAG):
        os.remove(FORBIDDEN_FLAG)

# ------------------------------------------------------------------ 集成状态口径
print("== /system/integrations 的 kubernetes 口径 ==")
r = req("GET", "/system/integrations", token=admin)
k8s = next((x for x in as_list(r.get("data")) if x.get("key") == "kubernetes"), {})
check("kubernetes 项存在", bool(k8s), r)
check("不再用 LocalCluster 的实现名冒充集群接入", k8s.get("impl") != "local", k8s)
check("detail 说明已接入的集群数", "已接入" in (k8s.get("detail") or ""), k8s.get("detail"))
check("detail 如实说明部署执行仍是内置实现",
      "内置实现" in (k8s.get("detail") or ""), k8s.get("detail"))

# ------------------------------------------------------------------ 收尾清理
print("== 清理 ==")
# 同样只删本脚本建的。外部集群从进入脚本起就没被碰过，收尾也不碰。
for k in [k for k in _cluster_keys() if _is_mine(k)]:
    req("DELETE", f"/clusters/{k}", token=admin)
check("清理后无本脚本的残留集群",
      [k for k in _cluster_keys() if _is_mine(k)] == [], _cluster_keys())
missing = [k for k in EXTERNAL if k not in _cluster_keys()]
check("收尾后外部集群依旧完好", missing == [], missing)

print(f"\n通过 {len(passed)} 项，失败 {len(failed)} 项")
if failed:
    print("失败项：")
    for f in failed:
        print("  - " + f)
    sys.exit(1)
print("全部通过")
