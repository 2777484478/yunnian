# 集群接入 E2E 夹具

验证「系统管理 › 集群接入」七条接口的**真实**行为。因为集群接入的全部价值就是
"真拨通一次集群 API"，所以这里不放"假数据"，而是**复现一个真集群在该协议上的行为**：
真 HTTPS、真校验 `Authorization: Bearer`、真返回 K8s 风格的 `Status` 对象。
在它上面得到的结论（接入成功 / 鉴权失败 / 不可达）与对真集群的结论走**同一套代码路径**。

## 文件

| 文件 | 作用 |
| --- | --- |
| `fake_k8s.py` | 最小假 K8s API Server。无 token → **401**；`/version` → `v1.29.6`；3 个节点（node-03 未就绪）；14 个命名空间；存在 `forbidden.flag` 时节点接口 → **403** |
| `start_fake_k8s.py` | double-fork 启动（`&` 起的进程会随 shell 调用结束被 SIGHUP 回收） |
| `kubeconfig.yaml` | 3 个 context：`yn-test-ctx`（正常）/ `yn-prod-ctx`（正常）/ `yn-broken-ctx`（**引用不存在的集群**，用来验证解析阶段就能识别出不可接入） |
| `smoke.py` | 70 条断言 |

`cert.pem` / `key.pem` 缺失时 `fake_k8s.py` 会自动生成自签证书（kubeconfig 里用了
`insecure-skip-tls-verify`，故不需要 CA）。

## ⚠️ 清理只动自己的命名空间

本脚本建的所有集群标识都在 **`yn-smoke-`**（手工复现用 `yn-repro-`）前缀下。
起跑与收尾的清理**只删这些前缀的**，并且会断言"外部集群未被删除"。

这不是洁癖：开发机上常已接入真实集群（`cluster.local` 这类，**凭据不可再生**）。
早先的实现是 `for stale in 全部集群: DELETE` —— 在这样一台机器上跑一次就造成
不可逆的数据丢失。**别把清理改回"清空列表"。**

同理，「恰好 N 个」这类断言要按**自己的**前缀计数（`_is_mine`），不能按列表总数，
也不要取 `[0]`：真实数据混进来时下标会指到别人的行上，脚本会红得莫名其妙。

## 跑法

```bash
# 1) 依赖与后端（见 backend/README.md），确认探针通
curl -s --noproxy '*' -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/healthz

# 2) 起假 K8s（默认 16443）
python3 start_fake_k8s.py

# 3) 跑断言
python3 smoke.py
```

停止：`lsof -nP -iTCP:16443 -sTCP:LISTEN` 拿 PID 后 kill。

## 断言覆盖

- **鉴权**：未登录 40100 / `ops` 无 `user:manage` 时写接口 40300、有 `env:read` 时读接口放行
- **解析**：`problem` 非空（引用的集群不存在、缺 client-key…）不被凭据判定覆盖
- **真探测五类结论**：连通（v1.29.6 / 3 节点 / 2 就绪 / 14 命名空间）、
  错凭据 → `unauthorized`、不可达 → `unreachable`、连通但无权列节点 → 走 `warnings`
  且 `nodes` 为 **null**（不用 0 冒充）、命名空间仍读得到
- **凭据无出口**：接入与列表响应里都不含 token 明文与 `insecure-skip-tls-verify`
- **删除守卫**：被环境引用时返 40901 并点出引用者；列表 `usedByEnvs` 标出引用环境
- **软删复活**：删掉后用同标识重建成功（`uk_cluster_key` 只含 key，不复用会撞 40900）
- **集成状态口径**：`/system/integrations` 的 kubernetes 项不再拿 `LocalCluster` 冒充
- **列表字段必须是 `[]` 而非 `null`**：`usedByEnvs` / `warnings` / `errors` / `contexts`。
  这条是**白屏事故之后补的** —— 原先的断言只检查"内容对不对"，没检查"形状对不对"
  （详见本目录上层 `scripts/null-field-sweep.py` 的文件头）。
- **解析失败的契约**：`ok=false + errors`（HTTP 200），不是抛 40000

## 防「假绿」的三条自保

这是本项目踩过的坑（详见技能 `api-smoke-false-green-hardening`）：

1. **预检**：后端 `/healthz` 与本地假 K8s（未鉴权应返 401）都必须先探通，否则**直接中止** ——
   不探通就往下跑，会把"请求全失败"变成一堆看似通过的断言。
2. **不落盘再读**：全部断言在内存里做，不存在"读到上一轮旧产物"的可能。
3. **断言具体内容**：不只看 `code==0`，还看 nodes / version / 是否真的区分了 401 与不可达。

> ⚠️ `forbidden.flag` 由 `smoke.py` 与 `fake_k8s.py` 按**各自脚本目录**解析，
> 两者必须在同一个目录下运行；把其中一个换成别处的副本（路径不一致）会得到 3 条**假红**。

## 与真实集群的差别

本夹具**不是**真实集群，因此以下未被验证：真实 TLS 证书链、真实 RBAC、
`kubeconfig` 里的 `exec` / `auth-provider` 凭据插件、多 API Server 的 context。
接入真实集群前，需要一份真 kubeconfig 再跑一遍同样的链路。
