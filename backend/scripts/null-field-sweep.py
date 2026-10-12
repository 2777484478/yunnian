#!/usr/bin/env python3
"""扫描后端各接口返回，找出「值为 null 但前端声明成数组」的字段。

为什么需要这个脚本（2026-09-29 真实事故）：
Go 的 nil 切片会序列化成 JSON `null`，而前端把这些字段声明成 `string[]` 并直接
`.length` / `.map`。**一个字段是 null 就会在渲染期间抛 TypeError，React 随即卸载
整棵组件树 → 整个平台白屏**。当时的表现是：集群接入页在"有集群但没被环境引用"时
白屏（`usedByEnvs` 是 null），侧边栏和顶栏一起消失，用户连"点别处逃出去"都做不到。

这类缺陷有几个讨厌的性质：
  - **类型检查抓不到**：`tsc` 认为 `usedByEnvs` 是 `string[]`，代码写 `.length` 完全合法；
  - **只在特定数据下出现**：有引用时是数组，没引用时才是 null —— 空库上跑永远看不到；
  - **冒烟容易漏**：原来的断言只检查"内容对不对"，没检查"形状对不对"。

所以它只能靠"跑起来、看真实响应"来发现。本脚本就是干这件事：
把主要 GET 接口拉一遍，递归找出所有 null 字段，再回查前端类型定义 ——
若该字段在 `types/index.ts` 里声明成数组（`x: T[]`），就判定为**缺陷**并报红。

用法：
    python3 null-field-sweep.py                 # 默认 http://127.0.0.1:8080
    YN_BASE=... python3 null-field-sweep.py

退出码：0 = 无缺陷，1 = 发现"声明成数组却返回 null"的字段，2 = 预检失败。
"""
import json
import os
import re
import sys
import urllib.error
import urllib.request

BASE = os.environ.get("YN_BASE", "http://127.0.0.1:8080")
API = BASE + "/api/v1"
# 前端类型定义文件，用来判断"这个字段是不是被声明成数组"
TYPES_FILE = os.environ.get(
    "YN_TYPES_FILE",
    os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "..", "frontend", "src", "types", "index.ts"),
)

_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))

# 要扫的接口。列表类为主 —— 列表字段就是出事的那一类。
PATHS = [
    "/clusters",
    "/projects",
    "/builds",
    "/images",
    "/environments",
    "/deployments",
    "/rollbacks",
    "/audit-logs",
    "/system/integrations",
    "/system/stats",
    "/ai/conversations",
]

ACCOUNT = ("admin", "changeme")


def call(method, path, body=None, token=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(API + path, data=data, method=method)
    r.add_header("Content-Type", "application/json")
    if token:
        r.add_header("Authorization", "Bearer " + token)
    try:
        with _OPENER.open(r, timeout=30) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as e:
        try:
            return json.loads(e.read().decode())
        except Exception:  # noqa: BLE001
            return {"code": -1, "message": f"HTTP {e.code}"}


def array_typed_fields():
    """从 types/index.ts 里读出「被声明成数组」的字段名。

    只看形如 `  fieldName: SomeType[];` 的行 —— 够用且不会误判：
    本项目里数组字段都写成 `T[]`（没有 `Array<T>` 写法）。
    另外把 `x: T[] | null` 这种**显式声明可空**的排除掉：那是作者明知并处理过的。
    """
    out = set()
    try:
        src = open(TYPES_FILE, encoding="utf-8").read()
    except OSError as e:
        print(f"⚠️  读不到前端类型文件（{TYPES_FILE}）：{e}\n    只能列出 null 字段，无法判定哪些是缺陷。")
        return None
    for m in re.finditer(r"^\s*(\w+)\??:\s*[^;\n]*\[\]\s*(\|\s*null)?\s*;", src, re.M):
        if not m.group(2):  # 显式 `| null` 的不算
            out.add(m.group(1))
    return out


def walk(node, path, found):
    if isinstance(node, dict):
        for k, v in node.items():
            p = f"{path}.{k}" if path else k
            if v is None:
                found.append((k, p))
            else:
                walk(v, p, found)
    elif isinstance(node, list):
        for v in node[:5]:  # 每类抽前 5 条，够暴露形状问题
            walk(v, path + "[]", found)


def main():
    print(f"== 预检 {BASE}/healthz ==")
    try:
        with _OPENER.open(BASE + "/healthz", timeout=5) as resp:
            if resp.status != 200:
                print(f"预检失败：/healthz = {resp.status}")
                return 2
    except Exception as e:  # noqa: BLE001
        print(f"预检失败：{e!r}\n（后端没起？先按 backend/README.md 起服务）")
        return 2
    print("  /healthz = 200")

    tok = (call("POST", "/auth/login", {"username": ACCOUNT[0], "password": ACCOUNT[1]}).get("data") or {}).get("accessToken")
    if not tok:
        print("登录失败，无法扫描")
        return 2

    array_fields = array_typed_fields()
    found = []
    scanned = 0
    for p in PATHS:
        r = call("GET", p, token=tok)
        if r.get("code") == 0:
            scanned += 1
            walk(r.get("data"), "", found)
        else:
            print(f"  [skip] {p}  code={r.get('code')} {r.get('message')}")
    print(f"\n== 扫了 {scanned} 个接口，发现 {len(found)} 处 null ==")

    if not found:
        print("✅ 没有任何字段返回 null")
        return 0

    by_field = {}
    for k, p in found:
        by_field.setdefault(k, []).append(p)

    bad = []
    print("\n字段名                 声明成数组?  出现位置")
    for k in sorted(by_field):
        is_arr = array_fields is not None and k in array_fields
        flag = "❌ 是" if is_arr else ("显式可空" if array_fields is not None else "—")
        print(f"  {k:20s} {flag:11s} {sorted(by_field[k])[:2]}")
        if is_arr:
            bad.append(k)

    if bad:
        print(f"\n❌ 有 {len(bad)} 个字段「前端声明成数组、后端却返回 null」，会白屏：{bad}")
        print("   修法优先改后端（列表字段的空值就是 []，见 service/cluster.go 的 nonNilSlice），")
        print("   前端再在 api 边界收口一道（见 api/cluster.ts 的 asList）。")
        return 1

    print("\n✅ 没有「声明成数组却返回 null」的字段（其余 null 都是显式可空或标量）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
