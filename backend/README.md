# YunNian 云辇 · 后端

Go 1.26 + Gin + GORM + MySQL 8 + Redis 7。一期范围：**CI 镜像构建 + CD 环境部署**。

对齐 `docs/02-数据库设计.md`（24 张表，含后加的 `k8s_cluster`）与 `docs/03-API设计.md`（RESTful v1，统一响应体 `{code,message,data,traceId}`）。

---

## 快速开始

```bash
# 1. 起依赖（仓库根目录）：MySQL 8 + Redis 7
cd ..            # 回到 YunNian/
docker compose -f docker-compose.dev.yml up -d

# 2. 起后端（首次会自动建表 + 灌种子数据）
cd backend
go run ./cmd/server -config configs/config.yaml

# 3. 冒烟（另开一个终端）
bash scripts/smoke.sh
```

默认监听 `:8080`，接口前缀 `/api/v1`。演示账号：

| 账号 | 密码 | 角色 |
| --- | --- | --- |
| `ops` | `changeme` | 运维（ops） |
| `admin` | `changeme` | 超级管理员 |
| `guest` | `changeme` | 只读 |
| `dev01` … `dev16` | `changeme` | 开发（各项目负责人） |

> 冒烟脚本里的所有请求都带 `--noproxy '*'`。本机默认 HTTP 代理会把 `127.0.0.1`
> 也代理掉，表现为"服务明明起来了却返回 502 upstream connect failed"。

### 让后端常驻后台（一条经验，踩过坑）

**不要用 `命令 &` 起后端。** 交互式 shell 退出时会向子进程发 SIGHUP，
后端跟着一起死——而后续命令各自在独立 shell 里执行，你看到的现象是
"服务刚探活还 200，下一分钟就 connection refused"，并且中间那些请求根本没落日志。

用下面任一方式（让进程脱离控制终端，SIGHUP 就送不到它身上）：

```bash
# 方式一：Linux —— setsid 一站式解决
cd backend
setsid ./bin/yunnian-server -config configs/config.yaml > /tmp/yunnian.log 2>&1 < /dev/null &
disown
curl -s --noproxy '*' http://127.0.0.1:8080/healthz   # 自检

# 方式二：macOS —— **没有 setsid 命令**（`zsh: command not found: setsid`），
#   且 `nohup 命令 &` 在本项目用到的受限环境里实测仍会被回收。
#   用 Python double-fork：fork → 父退 → os.setsid() → 再 fork → 重定向 0/1/2 → os.execv。
#   **仓库里已有现成脚本**（会自动清代理变量、先停旧进程、日志落 bin/yunnian-server.log）：
python3 scripts/start-local.py            # 启动
python3 scripts/start-local.py --status   # 看进程 + 探针
python3 scripts/start-local.py --stop     # 停掉监听 8080 的 yunnian-server

#   需要自己写的话，骨架如下：
python3 - <<'PY'
import os
pid = os.fork()
if pid > 0: os._exit(0)          # 父进程退出，子进程被 init 收养
os.setsid()                      # 脱离控制终端
if os.fork() > 0: os._exit(0)    # 再 fork 一次，确保不是会话首进程
for k in ("HTTP_PROXY","HTTPS_PROXY","http_proxy","https_proxy","ALL_PROXY"):
    os.environ.pop(k, None)      # 见下方代理那段，务必清掉
fd = os.open("/tmp/yunnian.log", os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
os.dup2(fd, 1); os.dup2(fd, 2)
os.dup2(os.open(os.devnull, os.O_RDONLY), 0)
os.chdir("backend")
os.execv("./bin/yunnian-server", ["./bin/yunnian-server"])
PY

# 方式三：交给进程管理器跑（生产形态）
#   systemd / supervisord / k8s Deployment 均可，注意探针走 /healthz
```

> ⚠️ **同样重要的是清代理环境变量再启动**。若 shell 里带着 `HTTP_PROXY`/`HTTPS_PROXY`
> （如 SnailLink `127.0.0.1:7890`），后端会继承它们，而 `integration_jenkins.go` 用的是
> `&http.Transport{Proxy: http.ProxyFromEnvironment}` —— **真会走那个代理**。
> 会话一结束代理端口就没了，表现为「只有 Jenkins 全面 502、报错耗时 0.3~3ms、日志无 cause」。
> 启动前清掉这几个变量，并设
> `NO_PROXY=localhost,127.0.0.1,::1,es-repo.example.com`。

> `ps` 在部分受限环境里被禁用（报 `operation not permitted`），
> 此时用 `lsof -nP -iTCP:8080 -sTCP:LISTEN` 判断服务是否还在。

### 冒烟脚本的两道自保

`scripts/smoke.sh` 的断言大多是「curl 落盘 → 读 JSON 比对 `code`」。这个模式有个
**假绿陷阱**：curl 一旦失败就不写文件，比对会读到**上一轮遗留的同名文件**，于是照样打印 ✓
（实测踩过——后端其实早就退了，脚本仍报满屏绿，唯一的红恰好是那条走管道取值的 admin 登录）。
所以脚本开头做了两件事：

1. `rm -f /tmp/smoke-*.json`：先清干净，文件缺失时 `check()` 返回 `PARSE_ERR`，如实报红；
2. 预检 `/healthz`，不通就直接 `exit 2` 并提示启动命令，不再往下刷噪声。

改动这类脚本时请保留这两条——它们不是装饰。

### 冒烟脚本的部署 / 回滚段默认不跑（`SMOKE_WRITE=1` 才开）

第 7、8 节**是真实的集群写入**：平台对 `test` 环境发版会真的改掉集群里那个工作负载的镜像。

> ⚠️ 一个曾经写错、后来被实测推翻的前提：`environment.kubeconfig_credential_id` 为 `NULL`
> **不代表**部署会空转。`clusterRefOf`（`internal/service/cd.go`）只在环境**自带**凭据时才填
> `EnvID`，否则凭据解析会回退查平台集群表（`store/cluster_cred.go` 的 `resolveClusterCredID`）。
> 实测 `env test` 的 `cluster=cluster-local`，而 `k8s_cluster` 表里那条有
> `kubeconfig_credential_id=23` → 对 test 发版**真的会改集群**。

因此这两段默认跳过，并在跳过时打印「有哪几个可用的写夹具」。要跑就显式：

```bash
SMOKE_WRITE=1 bash scripts/smoke.sh     # 会真的改集群，且动手前会打印「改谁、改成什么」
```

写夹具（`SID` / `DEP_PKEY`）要求**至少两个不同镜像**，否则回滚段会恒红
（报 `40901 找不到可回滚的历史版本`）。终态等待超时给到 150×2s：真集群滚动更新实测 100–345 秒，
60 秒是不够的，会误报「未在预期时间内收敛」。

### 夹具自动发现

`PKEY` 之类**不许写死**。写死一次的代价不只是「6 条 401 可见」，而是
**部署/回滚整段静默跳过、覆盖度塌一半**——脚本照样报绿（2026-10-09 修）。

现在构建夹具取「第一个 `buildMethod != jenkins` 的项目」，服务夹具分读/写两套；
若纳管项目**全部是 Jenkins 构建**（当前就是），第 5 节不静默跳过，改为断言那条真实契约：
`POST /builds` 对 Jenkins 项目返回 `40901`（平台不代跑 docker 构建），并补上构建读路径。

---

## 目录结构

```
backend/
├── cmd/server/             # 入口：配置 → 日志 → DB → 缓存 → 下游 → 加密 → 队列 → 服务 → HTTP
├── configs/config.yaml     # 配置（默认值 → 文件 → 环境变量三层覆盖）
├── scripts/smoke.sh        # 冒烟：走通一期核心路径的 50 项断言
├── scripts/cluster-e2e/    # 集群接入的 70 项 E2E（假 K8s + 断言，见其 README）
├── scripts/null-field-sweep.py  # 扫「声明成数组却返回 null」的字段（会白屏，见文件头）
└── internal/
    ├── api/                # HTTP 层：路由表 + handler + SSE 写出（不写业务判断）
    ├── service/            # 业务层：auth / ci / cd / system / ai + DTO
    ├── store/              # 迁移、命名策略、种子数据
    ├── model/              # GORM 模型（24 张表）
    ├── auth/               # JWT 签发解析 + 权限点通配匹配
    ├── cache/              # Redis 封装：会话键、失败计数、分布式锁、日志流
    ├── logstream/          # 日志流接口 + 进程内实现（SSE 的数据底座）
    ├── queue/              # 并发受限任务池 + 部署互斥锁接口
    ├── integration/        # 下游抽象：Harbor / GitLab / K8s / 构建执行器（均含 local 实现）
    ├── crypto/             # AES-256-GCM 凭证加密
    ├── llm/                # OpenAI 兼容大模型客户端（流式）
    ├── middleware/         # TraceID / Recovery / AccessLog / CORS / Auth / 权限
    ├── httpx/ errs/        # 统一响应体、分页、错误码
    └── config/             # 配置加载与校验
```

分层约定：**handler 不写业务判断**（只绑定入参、组装 Caller、写响应），
业务规则、事务与审计全部在 `service`，且 service 不依赖 gin。

---

## 设计上的几个关键取舍

| 决定 | 原因 |
| --- | --- |
| 日志通道用 **SSE** 而非 WebSocket | 单向推送、纯 HTTP 免网关改造、`Last-Event-ID` 原生支持断线续传 |
| JWT + **Redis 会话键双校验** | 签名有效的令牌无法主动吊销；管理员停用账号、改角色需要"立即失效" |
| **Redis 可选**（`redis.enabled=false` 仍可运行） | 日志流 / 分布式锁 / 失败计数自动退回进程内实现，本地零依赖可跑 |
| 下游全部有 **local 实现** | 没有 Harbor / GitLab / K8s 也能把编排闭环跑通；响应里带 `source` 标明"当前为本地模拟" |
| 表名**单数**（`user` / `build_record`） | `store.NamingStrategy()` 固定 `SingularTable`，与 docs/02 的 DDL 及手写 JOIN 三方一致 |
| `firstOrNotFound(db, id, …)` 主键**必填** | 曾经漏传主键导致"查 #136 拿到 #1"却返回 200，把 id 放进签名让漏传编译不过 |
| 环境级权限在 handler 内动态判定 | `deploy:test` / `deploy:prod` 这类权限点由环境名拼出，路由表上声明不了 |
| 失败归因**降级而不报错** | 模型通道不可用时退回错误码规则，`source` 如实标注、`notice` 说明原因——让用户拿不到任何结论，比拿到一个标了来源的结论更糟 |
| **列表字段的空值一律发 `[]`，绝不发 `null`** | Go 的 nil 切片会序列化成 `null`，而前端把这类字段声明成 `string[]` 并直接 `.length`/`.map` → **渲染期抛异常，React 卸载整棵树，整个平台白屏**（2026-09-29 实测：`usedByEnvs` 为 null 让集群接入页白屏）。`null` 只用来表达"没读到/未知"（如 `nodes *int`），不用来表达"空"。见 `service/cluster.go` 的 `nonNilSlice` 与 `scripts/null-field-sweep.py` |
| 解析类接口的失败走 **`ok=false`** 而不是 4xx | 校验失败是"输入有问题、请改内容"，不是服务端故障；4xx 会让前端只能弹一句 toast，而那些原因往往有好几条，需要一个能留存、能逐条读的展示位置 |

---

## 配置

优先级：**内置默认值 → `configs/config.yaml` → 环境变量（`YUNNIAN_` 前缀）**。
未定义的环境变量会原样保留（不会静默变成空串），便于发现"忘了注入"。

生产必须覆盖的项（否则 `Validate` 会拒绝以 `app.env=prod` 启动）：

```bash
export YUNNIAN_ENV=prod
export YUNNIAN_JWT_SECRET='<32 位以上随机串>'
export YUNNIAN_MASTER_KEY='<32 位以上随机串>'     # 凭证与模型密钥加密用
export YUNNIAN_DB_DSN='yunnian:<pw>@tcp(<host>:3306)/yunnian?charset=utf8mb4&parseTime=true&loc=Local'
export YUNNIAN_REDIS_ADDR='<host>:6379'
```

下游接入（默认全 `false`，走 local 实现）：

| 开关 | 说明 |
| --- | --- |
| `YUNNIAN_HARBOR_ENABLED` + `_URL/_REGISTRY/_USERNAME/_PASSWORD` | 镜像仓库 |
| `YUNNIAN_GITLAB_ENABLED` + `_URL/_TOKEN` | 代码托管 |
| `YUNNIAN_K8S_ENABLED` + `integration.kubernetes.clusters` | 目标集群 |
| `YUNNIAN_WEBHOOK_GITLAB_SECRET` / `_GITHUB_SECRET` | 推送回调密钥。**留空则回调接口直接 403**，不允许无签名触发构建 |

AI 通道不在配置文件里：模型密钥需要脱敏回显与运行时变更，统一由
「系统管理 › 模型设置」落库（`ai_model_config`，密钥 AES-256-GCM 加密存储）。

---

## 已实现的接口

按 `docs/03` 的模块分组，权限点紧随路径。⭐ = 一期核心路径。

| 模块 | 接口 |
| --- | --- |
| 认证 | `POST /auth/login` ⭐、`POST /auth/refresh`、`POST /auth/logout`、`GET /auth/me` ⭐、`GET /auth/oidc/authorize`、`GET /auth/oidc/callback` |
| 用户角色 | `GET /users`、`POST /users/invite`、`PATCH /users/{id}`、`POST /users/{id}/reset-password`、`GET /roles`、`GET /permissions`、`PUT /roles/{code}/permissions` |
| 项目 | `GET /projects` ⭐、`GET/POST/PUT/DELETE /projects[/{key}]`、`PATCH /projects/{key}/enabled`、`GET /projects/{key}/branches`、`POST /projects/{key}/validate`、`GET /projects/{key}/builds·images·deployments` |
| 构建 | `POST /builds` ⭐、`GET /builds`、`GET /builds/{id}`、`GET /builds/{id}/logs`、`GET /builds/{id}/logs/stream` ⭐（SSE）、`POST /builds/{id}/cancel·retry`、`GET /builds/{id}/ai-diagnosis` |
| 镜像 | `GET /images`、`GET /images/{id}`、`GET /images/{id}/used-by`、`DELETE /images/{id}`、`POST /images/cleanup`（支持 dryRun） |
| 环境 | `GET /environments` ⭐、`POST/PUT/DELETE /environments[/{key}]`、`GET /environments/{key}/project-candidates·vars·services ⭐·resources·events`、`PUT /environments/{key}/vars`、`POST /environments/{key}/sync·test-connection` |
| 服务实例 | `GET /services/{id}·pods·versions ⭐·params/history`、`GET /services/{id}/pods/{podName}/logs/stream`（SSE）、`PUT /services/{id}/params`、`POST /services/{id}/scale·restart·redeploy`、`DELETE /services/{id}` |
| 部署 | `POST /deployments` ⭐、`GET /deployments`、`GET /deployments/{id}`、`GET /deployments/{id}/logs[/stream]` ⭐（SSE）、`POST /deployments/{id}/approve·reject·cancel·retry`、`GET /deployments/{id}/ai-diagnosis`、`GET /deployments/pending-approval` |
| 回滚 | `POST /rollbacks` ⭐、`GET /rollbacks`、`GET /rollbacks/{id}`、`GET /rollbacks/{id}/logs/stream` |
| 审计 | `GET /audit-logs` ⭐、`GET /audit-logs/{id}`、`POST /audit-logs/export`、`GET /audit-logs/export/{taskId}` |
| 通知 | `GET/POST /notify-channels`、`PUT/DELETE /notify-channels/{id}`、`POST /notify-channels/{id}/test`（**真实投递**）、`PATCH /notify-channels/{id}/enabled` |
| 模型 | `GET/POST /models`、`PUT/DELETE /models/{id}`、`POST /models/{id}/test·default`（`/model-providers` 为同义别名） |
| AI | `POST /ai/chat`（SSE 流式）、`GET /ai/conversations[/{id}]`、`POST /ai/analyze-build/{buildId}`、`POST /ai/analyze-deploy/{deployId}` |
| Webhook | `POST /webhooks/gitlab`、`POST /webhooks/github`（共享密钥 / HMAC 签名校验） |
| 系统 | `GET /system/stats`、`GET /system/integrations`、`GET/PUT /system/config`、`GET/PUT/POST /system/integrations/gitlab[/test]`、`GET/PUT/POST /system/integrations/jenkins[/test]`、`GET /system/integrations/jenkins/jobs·params·param-values`、`POST /system/integrations/jenkins/trigger`、`GET /healthz`、`GET /readyz`、`GET /metrics` |

### Jenkins 接入（复用已有 Jenkins）

复用而非重新建模：`project.build_method=jenkins` + `project.jenkins_job`（Job 的 fullName）
把平台项目和 Jenkins 上的 Job 绑起来，平台按绑定关系触发、拉日志、同步状态。
实现在 `internal/integration/integration_jenkins*.go`（客户端）、`internal/service/jenkins.go`（编排）。
配置与凭据走 DB 优先、逐键回退 `config.yaml`（`store/jenkins.go`），口令 AES-256-GCM 加密。

踩过的坑，改这块之前先看：

| 坑 | 表现 | 正解 |
| --- | --- | --- |
| CSRF crumb 按**会话**加盐 | 单独请求 crumb 再去 POST，403 | 客户端必须带 cookie jar（`cookiejar.New(nil)`），crumb 与 cookie 同源 |
| `param-values` 的查询串 | 写成 `name=` 时 **200 + `values: []`**，静默空 | 必须 `param=<参数名>`；且先读参数定义，参数名对不上主动回 40400 |
| 触发只给队列项 | 拿不到构建号就编一个 | 触发后轮询 `queue/item/{id}` 的 `executable.number`；认不出来就留空 |
| Job 不存在被归成 502 | 让人去查"Jenkins 是不是挂了" | 客户端独立哨兵 `ErrJobNotFound` → **40400**，提示重新绑定 |
| Jenkins 自报的 `rootUrl` 是内网 IP | 响应里的链接点不开 | 一律改写成配置的 baseUrl |
| 项目的 `registry` 可能带 scheme（`https://host:5000`） | `splitImageRef` 按第一个斜杠切分得到 `registry='https:'`、`repo='//host:5000/…'`，`image_record` 里存出一条坏记录（**已真实发生过**） | 切分前剥掉 `scheme://` 与首尾多余斜杠 |
| Jenkins 构建的产物版本没有接口可查 | 只能从日志的 `docker push` 行读，读错就登记错版本 | `ParsePushedImage` 只认确定证据，读不到留空（见下） |
| **`POST /builds` 不看 `build_method`** | Jenkins 项目被平台跑了一次 docker 构建，还"成功"了 | `TriggerBuild` 对 `build_method=jenkins` 直接 40901（见下方"已知未落地"） |

**构建日志落库（`persistBuildLogs` / `flushBuildLogs`）的两条硬约束**，2026-10-10 实测踩到：

| 坑 | 表现 | 正解 |
| --- | --- | --- |
| 日志行按**字节**截断（`content[:2000]`） | 一个中文 3 字节，切点落在字符中间 → 非法 UTF-8 → MySQL utf8mb4 列拒收整条 INSERT（`Error 1366 Incorrect string value`） | `clampLogLine` 先退到字符边界再截，最后过一遍 `strings.ToValidUTF8`（**上游也会给半截**：docker 输出按块读，多字节字符会被切在两块之间） |
| 批量写失败直接 `return` | 一条坏行让**整批 200 行**一起被拒，那次构建的日志**一行都没落库**——"构建失败了、日志是空的"，恰恰是最需要证据的时候没有证据 | `flushBuildLogs` 批量失败 → **逐条重试**，损失限制在真正有问题的那几行 |

> 这类缺陷的可怕之处是**静默**：它只打一条 WARN，构建状态一切正常，界面上就是"日志为空"。
> 看到"构建失败但日志是空的"，先去看后端日志里有没有 `落库构建日志失败`。

> **构建入口只有一条**：都从「项目管理 › 编译」出发。Jenkins Job 列表页是只读的——
> 平台要靠项目登记这次构建属于谁、镜像登记到哪、日志挂在哪，未绑定项目的 Job 后端
> 本来就会拒绝（40901），与其让人点了再失败，不如只留下能成的那条路。
> 批量编译向导同样不接收 Jenkins 项目（后端会拒，前端也不给勾）。

> **两套"新建项目"入口**（前端「新建项目 ▾」菜单）：
>
> - **新建项目**（Dockerfile / 自定义脚本）：平台自己拉代码构建，`gitUrl` 必填；
> - **新建 Jenkins Job 项目**：只填基本信息 + 构建方式（Jenkins Job）+ 要绑定的 Job，
>   仓库地址、构建参数、镜像地址全由 Job 自己管。
>
> 后端只有 `POST /projects` 一条路径，必填规则按 `build_method` 判
> （`validateProjectRepo`，新建与编辑共用）：`buildMethod=jenkins` 时 `jenkinsJob` 必填、
> `gitUrl` 可空；其余方式反过来。**不要**把 `gitUrl` 改回无条件 `binding:"required"`——
> 那样 Jenkins 项目会被逼着填一个没有任何代码去读的地址。

### 「自定义脚本」构建（`buildMethod=script`，2026-10-10）

构建流程**整个由项目上的 `build_script` 定义**：拉代码、编译、打包、推送都写在里面。
与 `dockerfile` 模式的唯一差别是**谁来描述打包过程**。平台只做三件事：

1. 把仓库凭据配好（`GIT_ASKPASS` 预置，脚本里写普通 clone 地址，**不出现凭据**）；
2. 注入 `YUNNIAN_*` 环境变量 + 该项目的「构建参数」；
3. 把输出收进构建日志。

| 约定 | 为什么 |
| --- | --- |
| 脚本**落盘成文件**再 `sh <file>`，不用 `sh -c` | `sh -c` 下脚本里的报错行号全指向那一整行，定位不了 |
| 平台先 `cd` 到空白工作目录 | 脚本不该操心"我在哪"；也让两次构建互不干扰 |
| 默认脚本由 `internal/buildscript` 用 `go:embed` 下发 | 默认脚本必须和构建机上的镜像地址、注入的变量名一致。前端各留一份副本迟早会出现"界面给的默认脚本跑不通"。`store` 的种子数据也要它，所以抽成**没有下游的叶子包**（否则 store → service 成环） |
| 空脚本 → 补 `buildscript.DefaultScript(type)`，不是报错 | 用户切到脚本模式还没编辑就保存，应该得到一份能跑的，而不是一个必然失败的入口 |
| `docker pull` 编译镜像**必须容忍失败** | `docker pull` 只看远端、**不看本地缓存**。构建机上已预装该镜像时（离线/内网没同步）它会报 not found 并因 `set -e` 中断整个构建 —— 而镜像明明在本地。真拉不到也不要紧：下一步 `docker run` 会立刻报"镜像不存在"，错误位置更清楚 |
| `docker build` **必须带 `--provenance=false`** | BuildKit 默认挂 attestation，产物变成 manifest list（多一个 `unknown/unknown` 平台条目）。本地看不出异常，`docker push` 到 Harbor 报 `error from registry: manifest invalid`。**实测踩到过**，与 `docker_builder.go` 里同一处参数同理 |
| `docker build` 强制 `--platform linux/amd64` | 开发机 arm64、目标机 x86_64。不指定会构建出 arm64 镜像，推到生产 `exec format error`，要等容器起不来才发现 |
| **`ErrCodeScriptNoImage`（`SCRIPT_NO_IMAGE`）** | 一段只做 `git clone` 的脚本退出码是 0，构建记录会写"成功"、日志一片祥和，而镜像列表什么都没有。平台唯一能查证的事实是「构建机上有没有 `$YUNNIAN_IMAGE`」，查不到就判失败 |

**默认 Tag 规则 `v{buildNo}`**：`buildNo` 是项目级构建序号（`build_record.build_no`），
每构建一次 +1 → v1、v2、v3…，天然唯一。`renderTag` 另支持 `{branch}` / `{commitId}` /
`{timestamp}` / `{date}`。

> ⚠️ **失败的构建也占号**（序列有空洞），这是刻意的。改成"只数成功次数"就要在成功之后
> 回写 tag，而那时镜像已经推出去了，回写失败会导致下次成功构建用同一个 tag **覆盖上一个**——
> 比序号有空洞严重得多。

**项目级 Git 凭据（`git_username` + `git_credential_id`）**，取代早先"一份平台级 GitLab 通道"：
那条通道一旦 `enabled=false`，**所有项目会静默退化成匿名检出**（表现为"昨天还好好的，今天全 401"）。
`git_username` 明文（它会出现在 `git remote`、CI 日志、GitLab 提交记录里，不是秘密）；
密码走 `secret_credential` 加密，**出参一律不回显**，编辑时留空 = 不修改。
凭据写入与项目行更新**必须在同一事务**：分开写会出现"项目建好了、密码没存上"，
首次构建报 401，而用户刚点过「测试连接成功」—— 他会去怀疑凭据本身而不是平台没存。

**测试连接（`POST /projects/test-connection`）真跑 `git ls-remote`**，不走 GitLab REST API：
构建用的是 git 协议 + 凭据助手，探测必须**测同一件事**，否则就是在给"测试通过但构建 401"
的假承诺。同理 `GitProbe` 与真正的构建**共用同一份 `prepareAskpass`**。
`message` 必须是**翻成人话的结论**，分类依据是"用户下一步该改什么"（地址写错 / 账号密码错 /
分支不存在 / 域名解析不了），不是 git 的错误码。原始输出放 `detail`（已打码），只作定位用。

端到端实测脚本在 `/tmp/yn-buildtest/e2e-script.py`（平台外，不入库）：默认脚本跑通、
注入变量逐一断言、Tag 规则产出 `v{buildNo}`、只 clone 的脚本必须判 `SCRIPT_NO_IMAGE`、
产出镜像 `docker run` 打出预期内容。

### 发单前镜像预检（2026-10-08 落地）

围绕一条纪律：**别让用户等一个注定失败的发布**。

**`CDService.preflightImage`**

`POST /deployments` 在建单前问一次仓库：这个 tag 在不在？判定刻意**保守**——

| 仓库的回答 | 平台的动作 | 理由 |
| --- | --- | --- |
| HTTP 404（`ErrTagNotFound`） | **拒绝建单**，返回 `40000 镜像 X:Y 不在仓库中，这次发布必然失败` | 仓库**明确**说了没有，这是确定性答案，发布必然失败 |
| 200 | 放行 | — |
| 超时 / 401 / 5xx / 不是本平台管的 registry | **放行 + WARN 日志** | 仓库抖动不该变成"不能发版" |

只有 404 能判定"必然失败"，其余一律不能拿来做拒绝的理由 —— 否则一次网络抖动就会
挡掉一次正常发布。"是不是本平台管的仓库"由 `RegistryV2.Accepts()` 判定，
避免拿别人的仓库地址去问、静默挂上不相干的 digest。

桩式单测在 `service/cd_preflight_test.go`（4 个分支）；打**真仓库**的只读验证在
`service/cd_preflight_live_test.go`（`go test -tags live`）。

⚠️ 预检**只能拦"仓库里没有"**。镜像存在但应用起不来（缺配置、依赖连不上）拦不住 ——
那要等运行期才暴露，见下一节。

### 部署失败后**不回滚**（2026-10-08 用户拍板；此前实现过，当天移除）

失败后平台**只写失败终态，不动集群**。工作负载就留在"新镜像 + 卡住的 rollout"上。

为什么不回滚：

- `RollingUpdate` 在 1 副本时 `maxUnavailable` 向下取整为 0，**旧 Pod 不会被杀**，
  服务通常不中断；此时"谁在跑、跑的是哪一版"是确定的，不需要平台去猜。
- 回滚**本身也是一次发版**，它同样会失败。届时集群被改到第三个状态，平台只能再报一次错，
  比不动作更糟 —— 详见技能 `failure-auto-rollback` 里"什么时候不该做"那一节。
- 恢复动作交给人，走的是**早已实现的手工回滚链路**（`POST /rollbacks`，
  以及 AI 诊断抽屉里的「按建议回滚」）：那一路有权限校验、有审批环境判定、有审计。

**代价必须知情**：失败的 spec 会留在集群上，直到有人回滚或重新发布。
所以失败时会往日志流补一行 `warn` 明说这件事，避免用户以为平台替他兜了底。

> 历史：2026-10-08 上午实现过 `autoRollback`（含落 `trigger_type=auto` 的回滚记录），
> 当天下午按用户要求整体移除，`runDeploy` 的失败分支不再读变更前镜像、也不发起任何写集群动作。
> `rollback_record.trigger_type` 的 `auto` 取值**保留**（docs/02 的枚举仍是
> `manual/ai_suggested/auto`），因为库里已有一条真实产生过的 `auto` 记录，前端也照常
> 如实标注「自动」—— 把渲染删掉会把历史数据谎报成「人工」。

### 已知的未落地项（如实说明，不是"忘了"）

- **失败原因已落库** —— **已修**（2026-10-10）。此前的问题：`deploy_record` 只有 `error_code`、
  **没有人类可读的原因列**，`DeployDTO` 也**不暴露 `errorCode`**；而真因只进了**易失的日志流**
  （进程内 / 带 TTL），**平台一重启，那条失败单的日志与原因就一起没了**。不做自动回滚之后这条更要紧：
  现场只剩"集群停在新镜像"这一个事实，解释它的线索却没了。
  **构建侧是同一个毛病**（`finishBuild` 的 `errMsg` 只打 WARN 日志），所以两侧一起修：
  `build_record` / `deploy_record` 各加一列 `error_message`（`varchar(512)`），
  `finishBuild` / `failDeploy` 以及三处「队列拒收」、两处「启动补偿标记 `interrupted`」
  共 **7 个写点**一并写入，`BuildDTO` / `DeployDTO` 同时下发 `errorCode` 与 `errorMessage`
  （机器码用来筛选/统计，文本用来解释）。
  两个实现细节：① 只在**非成功终态**写（成功时 `errMsg` 可能是"构建完成"这类正常文案，
  写进去会被误读成异常）；② 落库前按**字符**截断到 512（`clipRunes`）——
  MySQL 严格模式下超长会直接报 1406 把整条 UPDATE 打回去，变成"失败时写终态也失败"，
  比丢一段文本糟糕得多；按字节截又会在库里留下半个汉字。
- ~~**部署「AI 诊断」只是错误码查表**~~ —— **2026-10-08 已落地**，见
  「部署失败归因：模型优先、带集群侧证据」一节。落地前的实情是：`source` 恒为 `rule`，
  结论是按 `error_code` 从写死的 switch 里取一段固定文案，**没有读日志、更没有调模型**，
  所以给不出"这次到底是缺哪个配置 / 哪个探针没过"。
- **operator 不做"滚动更新失败"的判定**（平台侧已自行兜住，见下）：`reconcileRollout` 只会把 CR 置成
  `Succeeded` 或 `Updating`，没有对 Deployment `ProgressDeadlineExceeded` 的处理。
  后果是：镜像拉到了但容器起不来时，CR 会一直停在 `Updating`。
  注：CR 置 `Failed` 的那几条路径（目标不存在 / spec 非法 / patch 被拒）平台走的是
  `terminalFailure()`，能**立刻**失败；唯独"卡住的 rollout"没有这条快路。
  **平台侧补偿（2026-10-08 落地）**：`integration.waitRollout` 现在自己读 Deployment 的
  `Progressing=False / reason=ProgressDeadlineExceeded`，命中即返回 `ErrRolloutStuck`，
  映射成错误码 `ROLLOUT_STUCK`。判据只认这一个 reason —— `Progressing=False` 还有别的
  reason（如 `DeploymentPaused`），放进来会把"暂停"报成"失败"。
  效果：失败从等满 `runner.deployTimeout`（15m）缩到约 `progressDeadlineSeconds`
  （实测 demobasic = 600s，k8s 未显式设置时的默认值），并且**失败原因与 `TIMEOUT` 分开**
  （超时要查"为什么慢"，卡死要查"为什么起不来"）。
  operator 侧的正解仍是把 `ProgressDeadlineExceeded` 映射成 `phase=Failed`（`PhaseFailed` 已存在），
  届时平台这条兜底可留作双保险。
- **`/ai/conversations` 不持久化**：docs/02 的 22 张表里没有会话表，一期按一次性上下文处理，
  接口返回空列表 + `usable:false`，前端可据此隐藏入口。`POST /ai/chat` 本身可用（需先在模型设置里配通道）。
- ~~**`/ai/chat` 只喂一份提问前拼好的快照，模型无法自己取数**~~ —— **2026-10-09 已落地**，
  见「AI 助手：从『复述快照』改成『自己取数』」。配套问题一起消失：
  - ~~`ai.go` 把 `service_instance.liveness_ok / readiness_ok` 当"健康"喂给模型（假前提）~~
    —— 这两列是种子数据写死的，当集群健康用是错的。该路径已整体删除，模型改调 `get_service_status`。
  - ~~用户在聊天框里用口语提环境/服务名（"测试区192"、"demouser"）时必然答"上下文没有"~~
    —— 现在靠名录 + `list_environments`/`list_projects` 对标识。
- **AI 工具未按环境做权限收敛（待决定）**：有 `ai:query` 的用户可通过工具读到任意环境
  （含 prod）的集群实时状态。加工具之前就存在（旧快照同样不区分环境），但工具把可读内容从
  "平台登记值"扩到了"集群实时值"。`docs/03` §12 已记同一条。
- **环境表单的负责人 / 审批人选择器，`ops` 打开时是空的（待决定）**：这两个下拉的候选来自
  `GET /users`（`EnvironmentList.tsx` 的 `userApi.list`），而该路由要求 `user:manage`
  —— `ops` 调它返 `40300 缺少权限点：user:manage`，前端不弹错、`userOptions` 恒为 `[]`。
  后果：`ops` 是持有 `env:*` 的环境维护者，能打开编辑抽屉，却**一个候选都选不到**
  （审批人只能删、加不了；负责人换不了），改由必填规则拦住保存。**实测复现（2026-10-09）**：
  ops 登录下审批人显示裸用户名 `ops | admin`，切 admin 后同一字段显示
  `运维示例（ops） | 系统管理员（admin）`。两个修法都涉及权限模型，故留给用户选：
  **① 新增轻量选择器接口**（只回 `id/name/username`）、权限对齐 `env:edit` —— 推荐，
  不动 `/users` 既有语义；**② 放宽 `/users` 的读权限** —— 省事，但把全量用户名单开放给 ops。
- **AI 助手页的预置提问依赖 `/environments` 可读**：取不到环境名时自动退化为不含实体的两条
  （「最近 24 小时有哪些构建失败了？」），不会指向不存在的服务。这是刻意的 ——
  原先写死的 `payment-service` / `order-service` / `user-web` 在库里**都不存在**，
  点了必然得到一个"查不到"的回答，看着像 AI 不好使，实际是提问本身指向了不存在的东西。
- **通知「只配不发」已于 2026-10-10 补上发送链路**（`internal/service/notify.go`）。
  此前 `/notify-channels` 只有 6 条 CRUD、`TestNotifyChannel` 明确回 `ok=false`
  「真实投递尚未接入」，全仓搜不到任何发送实现 —— 界面上那个「测试」是诚实的空壳，
  但整个通知能力等于没有。现在补齐了这几块：

  - **发送器**：邮件（SMTP，`smtps`/`starttls` 两种协议）与钉钉（自定义机器人，**支持加签**）。
    入参枚举同步收敛为这两个 —— 企微 / 飞书 / 自定义 Webhook **从枚举里去掉**了，
    因为配得出来却发不出去，那种"安静的假承诺"没有任何地方会亮红灯。
    库里若残留这些类型的历史记录，投递会明确失败并写进 `last_error`。
  - **订阅矩阵生效**：`notify_channel.events` 真的决定"哪个事件发到哪个渠道"。
    五个触发点：`finishBuild`（构建失败）、`failDeploy`（部署失败）、
    `CreateDeployment` / `ExecuteRollback` 落待审批，以及**两个启动补偿**
    （`CIService` / `CDService` 的 `ReconcileInterrupted`，见下面「启动补偿」一节）。
    ⚠️ 待审批是在**建单那一刻**通知的。
  - **「被中断」单列两个事件码**（`build_interrupted` / `deploy_interrupted`，2026-10-10 补）。
    它曾经是**唯一没有任何告警的失败态**：终态、不会自行恢复，而列表页的筛选器与重试按钮
    都是"等人来看"的被动入口。**没有并进 `*_failed`** —— 成因（平台重启 / 本身写错）
    与该做的事（重新触发 / 改代码）都不同，订阅方应该能只要其中一个。
    两条实现约束值得记住：**同类合并成一条汇总消息**（队列容量是并发数 ×20，
    一次重启理论上能中断几十条，逐条发会把告警群刷屏，结果是该看的那条被划过去）；
    **投递放在改库之后**（反过来的话，通知已发出、改库却失败，就成了
    "群里说中断了、界面上还在转圈"）。回归测试 `notify_interrupted_test.go`
    用桩钉钉做端到端：既断言"发了什么"，也断言"**没发给未订阅的渠道**"。
  - **`events` 加了白名单校验**。以前 `defaultEvents` 放行任意字符串：界面上 8 个事件码里
    有 5 个（`build_success` / `deploy_success` / `rollback_executed` / `env_unhealthy`…）
    没有任何代码会触发。用户勾上、看到勾上了、以为配好了 —— 然后永远收不到。
    现在白名单与前端选项都读 `SupportedNotifyEvents` 一份清单，另由
    `TestSupportedNotifyEventsAreComplete` 钉住"每个码都有中文标签、且都登记了触发点"。
    2026-10-15 成功侧以**带触发点**的方式回归（`build_succeeded` / `deploy_succeeded`）：
    用户要求"发布成功要通知到群里"。⚠️ 判定点必须挂在 rollout **真正完成**之后
    （构建侧是镜像已登记，部署侧是 `ApplyImage` 返回 nil），提前到"spec 提交完"
    会变成一条比没有还糟的通知 —— 它在 rollout 失败之前就报成功。
  - **`TestNotifyChannel` 从"格式预检"改成真投递**，`ok=false` 意味着这次真的没发出去，
    失败原因是**对端的原话**（如钉钉 `errcode=310000 机器人发送签名不匹配`）。
  - **结果回写 `last_sent_at` / `fail_count` / `last_error`**（`last_error` 是新增列），
    并在 DTO 里下发 —— 这三个字段是"配了渠道却没收到消息"唯一的自查入口。
  - **发信身份在平台配置里**（`config.yaml` 的 `notify.email`），不在渠道上；
    邮件渠道的 `target` 只填收件人。
  - **种子一并收掉了**（`store/seed_sys.go`，2026-10-10）。原先种子里还种 `wecom` / `webhook`
    两条 —— 只删库里的行是**半截修法**：`Seed` 以"user 表为空"为判据，空库重装会把它们
    再造出来；而 webhook 那条 `enabled=true` 且订阅 `build_failed`，**每次构建失败都会去投递、
    每次必然失败**。现在种子只种 `email` / `dingtalk` 两种类型 + 五个真事件码（2026-10-10 补上
    `build_interrupted` / `deploy_interrupted`，否则全新部署上线就带着同一个告警缺口），并由
    `TestSeedNotifyChannelsAreDeliverable` 钉住：它会拒绝"没有发送器的类型"与
    "触发不了的事件码"，且已用临时坏种子验证过**两条都真的会变红**。

  改这块之前先读 `notify.go` 文件头列的五条约束（不阻塞调用方、不用请求的 ctx
  起后台 goroutine、不静默成功、HTTP 200 不等于送达、重试要有分寸），
  每一条都对应一个真实踩过的坑。
- **凭证管理 `/credentials`（docs/03 §11）未开放**：`secret_credential` 表与加解密能力已就绪
  （`crypto.Sealer`），但没有对应的 service/handler。Harbor 目前**只读且匿名可读**（public 项目），
  所以暂时不需要凭据；等要接 private 项目或做镜像清理时，这条是前置依赖。
- **Harbor 没有界面配置**：GitLab / Jenkins 都有 `/system/integrations/{key}` 的 GET/PUT/test，
  Harbor 只看 `config.yaml` 的 `integration.harbor.*`，**改完要重启**。
  补 UI 时照 `store/gitlab.go` + `system.go` 那一套抄。
- **镜像清理未做**：`POST /images/cleanup` 只改平台自己的记录，**不会真的去仓库删 tag**
  （那需要 push/delete 权限）。要做之前必须先查 `image_record.deployed_envs` 防误删正在用的版本。
- **`image_record#1` 已于 2026-09-28 清除**（曾经的 `registry='https:'` 坏记录）。
  成因是**平台侧手工构建**（`build_record#2`，`trigger_type=manual`、`runner_id=runner-local`
  即 `runner.mode=inline`）产出的伪造登记，`splitImageRef` 把带 scheme 的镜像串切成了残骸。
  **原来记的"digest / 大小是种子数据编的"不准确**：实测该 tag `1.2.0.4551` 在 Harbor 里
  **根本不存在**（`/v2/escloud/demowarning/tags/list` 共 16 个 tag、最大 `1.2.0.1058`；
  `manifests/1.2.0.4551` 返回 404），所以整条记录都是无源数据，不是"引用了真实镜像但字段错"。
  **两条成因都已在代码里堵住，不会再产生**：`splitImageRef` 会剥掉 `scheme://` 并裁掉首尾斜杠
  （见上方"刻意的取舍"表）；`POST /builds` 对 `build_method=jenkins` 直接返回 40901。
  删除前已确认只被 `build_record#2.image_record_id` 引用（`service_instance` / `deploy_record`
  均为 0 引用），该指针一并置空，界面上 build#2 现在显示"无镜像"。
  用的是**物理删除**而非软删：`image_record.deleted_at` 的语义是"镜像已从 Harbor 清理"
  （见 `docs/02`），写它等于宣称清理了一张从未存在过的镜像。备份（含原行与 build#2 快照）：
  `backups/image-record-1-removed-20260928.sql`。
- **`runner.mode=agent` 已决定不做**（2026-10-10 用户拍板：平台是**单体单机**形态，构建 / 发布 / 部署
  只用一台机器）。配成 agent 时**构建会明确失败** —— `integration.UnavailableBuilder` 返回
  `RUNNER_UNAVAILABLE`，**不会**退回平台进程内执行：退回的结果是构建记录写「成功」，
  而用户以为自己用的是构建机，于是他把一次假成功当成了真成功。
  ⚠️ 此前这里写的是「任务仍在平台进程内执行」，那是 `UnavailableBuilder` 引入**之前**的行为，
  文档没跟着改 —— 运维看到会以为"配了 agent 也照跑不误"，直到第一次发版才发现。已更正。
  `model/runner.go` 保留为**无引用的预留**（表由 AutoMigrate 建出但一直为空，无路由、无页面）。
- **启动补偿已落地**（2026-10-09）。此前的问题：构建/部署队列是进程内的，平台重启后
  中断的任务会永远停在 `progressing`/`queued`（实测库里留下 3 条孤儿）。
  现在 `cmd/server/main.go` 在**开始对外服务之前**跑一次 `CIService.ReconcileInterrupted`
  与 `CDService.ReconcileInterrupted`（20s 超时，失败只告警不阻断启动），把它标记为
  **`interrupted`**（独立状态码，不并进 `failed`/`canceled`，理由见 `docs/01` §4.1）。
  两条边界：**`pending_approval` 绝不算中断**（合法长期态，标成中断等于替用户撤单）；
  `duration_sec` 显式写「`started_at` → `updated_at`」，不让 DTO 用「扫描时刻」兜底
  （否则平台挂 8 小时，一个跑 30 秒的构建会显示「耗时 8 小时」）。
  收尾还会**发一条汇总通知**（`build_interrupted` / `deploy_interrupted`，2026-10-10 补）——
  这两个状态不会自行恢复，唯一该做的事是"重新触发"，而在此之前没有任何地方会主动说出来。
  通知在**改库 + `duration_sec` + 日志流收口全部完成之后**才发。实测：真实造了一次
  "构建途中重启平台"，构建记录落 `interrupted`，钉钉与邮件两条真实渠道均收到。
  仍**未做**的是反向那半：按幂等键**重新入队**。
- **`/system/stats` 的 `abnormalServices` 已删除**（2026-10-09）：它原本是
  `service_instance.status` 的库内计数（纳管时写入的「平台期望值」），不是集群实况，
  实测会把集群里 running 的服务报成异常。字段从 DTO 与 handler 一并移除，**不是留着不读**
  ——留着不读，后来者会以为它还能用。
- **AI 只读**：`/ai/chat` 背后没有任何变更能力，模型只能输出 `attachment.kind=action` 建议卡片，
  由用户点击后走正常 REST（带权限与审计）。
- **删掉的项目标识重建不了** —— **已修**（2026-10-09）。成因：`project` 同时有 `deleted_at` 与
  `uk_project_key`（该索引**只含 `project_key`**），删除是软删 → 列表里已经看不到的那一行仍占着标识，
  用同一个标识再建会返回 `40900 项目标识已被占用：xxx`，而用户翻列表只会看到「这个标识没人用」
  （实测复现两次，第二次是用户重建 `demoitms`）。
  修法是**建单时捞回软删行复活**：`ci.go:620` 的 `reviveProject`（`environment` 有同源的
  `reviveEnvironment`）。两条必须守住的边界，都有单测（`cd_rollback_guard_test.go`）：
  ① 复活前**必须判 `DeletedAt.Valid`**，否则会把一条**活跃行原地改写**还回「创建成功」；
  ② **真并发占用不能伪装成"已复活"**，真冲突仍要报 `40900`。
  另一条路（把唯一键改成 `(project_key, deleted_at)` 复合）没走，因为那要动 DDL。
- **团队没有主数据；负责人候选已改为读接口**（2026-10-09 部分收口）：后端**没有团队表、
  也没有 `/teams` 接口**，`project.team` 只是个自由字符串（用户的数据权限按它反查）；
  负责人则是**真校验**——`resolveOwner` 拿名字去 `user` 表查，查不到报 `40000 负责人 "xxx" 不存在`。
  ~~前端下拉只有 11 个写死的名字，用户表里另外 11 个用户（含用户管理里新邀请的人）选不到~~
  → 已新增轻量选择器 **`GET /users/options`**（权限 `env:write`，只回 `id/username/name`），
  环境编辑抽屉与项目表单的负责人候选都走它（实测 ops 拿到 22 个）。
  为什么不是放宽 `GET /users`：那个接口要 `user:manage`，回的是完整用户行（含角色/状态/手机号），
  给「选个人」用既越权又过量。
  **`team` 仍是写死的 6 个字符串**（新建默认「基础架构组」），二期做团队主数据时换掉。
- **新建项目的默认值（一期）**：类型/团队/负责人/启用状态四个字段没有维护入口，
  新建表单直接预填 `type=backend`、`team=基础架构组`、`owner=当前登录用户`、`enabled=true`。
  ⚠️ 负责人默认值是**当前登录用户**，所以候选列表必须包含它 —— `ownerOptions()` 要显式把
  当前登录用户补进候选，否则默认值会指向一个下拉里选不到的项。
- **Jenkins 项目的镜像版本只能从日志里读**（已实现，见下）：Jenkins 没有"这次产出是什么"的接口，
  平台逐行解析日志里的 `docker push`，构建成功时登记成 `image_record`。
  读不到就**留空**（前端显示"—"），不猜版本号。
- **Jenkins 构建的取消只到"中止"**：`StopBuild` 中止的是 Jenkins 侧那一次构建；
  平台侧记录随日志同步一起收口，两者之间没有事务保证（极端情况需要人工核对）。
- **`service_instance.liveness_ok` / `readiness_ok` 不是集群的探针状态**：
  两列由 `seed_ci.go` 从平台的 `status` 反推（`liveness = status != "error"`、
  `readiness = status == "running"`），`cd.go` 新建服务实例时更是直接写死 `true`，
  表定义同样是 `default:true`。它们描述"平台认为它该怎么跑"，与集群里探针**实际过没过**无关。
  实测 demobasic：两列都是 `true`，而集群里新 Pod 正在 `CrashLoopBackOff` ——
  前端曾据此渲染出「Liveness 通过 / Readiness 通过」，与同一个展开行里的
  `CrashLoopBackOff` 自相矛盾（2026-10-08 已改为读集群 `live`，见前端 `liveHealthy`）。
  **2026-10-09 核对后剩两处，当天两处都已处理**：
  ① ~~`ai.go` 拼服务实例上下文时把这两个值喂给模型（`健康 liveness=%v readiness=%v`）~~
  —— **已整体删除**（连同 `crossFacts`），模型改调 `get_service_status` 去集群读真的，
  见「AI 助手：从『复述快照』改成『自己取数』」一节。
  ② ~~**仍未处理**：`dto.go:451` 的**服务详情接口**仍在暴露 `health: {liveness, readiness}`~~
  —— **2026-10-09 已删除**（`EnvServiceDTO.Health` 整块退役，前端 `types` 里的 `health` 同步删掉）。
  理由：留着它等于在**同一个响应**里给两个互相矛盾的健康口径 ——
  实测 demobasic 库内两项皆 `true`、界面显示"通过"，而集群里新 Pod 正在 `CrashLoopBackOff`。
  要健康状态就看 `live`（读不到就如实说读不到）。`service_instance` 那两列本身仍在库里
  （`model/deploy.go` 上本就是 `json:"-"`，种子与建服务/复活时照旧写入），
  但**已无任何接口或页面读它** —— 全仓搜 `LivenessOK|ReadinessOK` 只剩写入点。

### Jenkins 产物镜像的识别（`integration.ParsePushedImage`）

日志是唯一的事实来源，因此解析器只认**确定的证据**，每条规则都对应一个"会把错误版本当成
正确版本"的情形（完整表格见 `docs/03-API设计.md` §13.2.1）：`set -x` 的 `+ ` 前缀要容忍、
变量没展开（`$TAG`）不算、不带 tag 不算（那是 `latest`）、冒号是端口不算、
digest 推送不算、Job 中途推的辅助镜像要靠**项目仓库名**排除、
最后**读不到就留空**。登记时 `digest`/大小/架构日志里没有，但接下来会去**镜像仓库**补（见下条）。

- 构建进行中逐行喂给解析器（不等日志落库，落库那份受 `MaxLogBytes` 限制可能被截断）；
- **只在 `status=success` 时登记**：日志里出现 `push` 只说明执行过这条命令；
- **启动补偿第一步** `BackfillJenkinsImages`：解析发生在收尾那一刻，进程重启就错过。
  启动时把 `trigger_type=jenkins AND status=success AND image_record_id IS NULL` 的构建捞出来，
  读**已落库的日志**补登记（纯本地、幂等、不连 Jenkins）。
- **启动补偿第二步** `EnrichMissingImageMeta`：给 `digest` 还空的镜像记录补仓库元数据 ——
  上一步刚补出来的记录顺手在这一步一起补上。这一步要连镜像仓库（见下条）。
- 登记走 `upsertImageRecord`，与平台构建共用一份去重口径：唯一键 `(registry, repo, tag)` 撞上
  **软删行**时要 `Unscoped` 把这行复活，否则会出现"删过的镜像再也推不回来"，
  且报错长成"构建成功但写库失败"，完全看不出真因。

### 从镜像仓库读回 digest / 大小 / 架构（`integration/registry_v2.go`）

日志里没有 digest，所以登记完 `image_record` 后，拿 `repo:tag` 去镜像仓库问一次。
**走 `/v2/`（OCI Distribution），不是 Harbor 的 `/api/v2.0` 管理接口。**

```bash
curl -s -o /dev/null -w '%{http_code}\n' -k "https://<reg>/api/v2.0/systeminfo"
# 200 → 有 Core 管理接口；404 → 只有 registry 组件，本实现就是为这种形态写的
```

实测本项目的仓库（`es-repo.example.com:5000`）属于后者：`service="harbor-registry"` 说明
确实是 Harbor，但 `/api/v2.0/systeminfo` 在 `:5000`/`:443`/`:80` 上**全是 nginx 404**。
按 Core API 写的客户端会原样 404 —— 表现为"接上了但什么都读不到"，最难查的一类。

| 步骤 | 请求 | 拿到什么 |
| --- | --- | --- |
| 1 | `GET {host}/v2/` | 401 + `Www-Authenticate` 里的 `realm` / `service` |
| 2 | `GET {realm}?service=harbor-registry&scope=repository:{repo}:pull` | bearer token（public 项目匿名就给） |
| 3 | `GET /v2/{repo}/manifests/{tag}` | 响应头 `Docker-Content-Digest`；`sum(layers[].size)` = 镜像大小 |
| 4 | `GET /v2/{repo}/blobs/{config.digest}` | config blob 的 `os` / `architecture` |

要点（每条都有单测锁着，见 `registry_v2_test.go`）：

- **只认配置里那一个仓库**（`Registry.Accepts`）：记录里的 `registry` 来自日志，未必是同一个。
  不同仓库里同名 repo 内容可能完全不同，问错了会把别人的 digest 挂到这次构建上 ——
  **不报任何错，数据直接变假**。比对前按 `host[:port]` 归一（`https://host:5000`、
  `host:5000`、`host:5000/proj/repo:tag` 三种写法必须判成同一个）。
- **`escapeRepoPath` 保留斜杠**，与 Harbor Core 相反：Core 把「项目/仓库」塞进一个路径参数、
  斜杠要编成 `%2F`；`/v2/{name}/` 里斜杠就是路径分隔符，编掉反而 404。同名 repo 两套接口两种转义。
- **多架构**：清单列表本身不算镜像层，必须逐个拉子清单；大小按各平台之和（与 Harbor 界面口径一致），
  `arch` 写 `linux/amd64,linux/arm64`。
- **`Ping` 把 401 当连通**：401 恰恰证明仓库在正常工作、只是要换 token；判成失败会让状态页永远报红。
- **不提供"取最新 tag"**：`tags/list` 不保证顺序（实测 Harbor 返回乱序），
  能判"最新"的只有 Core 的 `sort=-push_time`。做不到的能力就不做，免得给出看着像时间排序的假"最新"。
- `Resolve` 失败只记 warn、digest 留空，**不影响构建终态**；反过来，日志里解析出的 tag 在仓库里
  404 是**有价值的交叉验证**（Jenkins 报 SUCCESS 不等于 push 成功）。

### 构建失败归因：模型优先、规则兜底（`CIService.BuildAIDiagnosis`）

`GET /builds/{id}/ai-diagnosis` 有**两级来源**，`source` 字段如实下发，前端按它措辞——
把规则结论渲染成"模型分析"，比不给结论更糟：它会让人以为日志已经被看过一遍。

| 来源 | 触发条件 | 做法 |
| --- | --- | --- |
| `model` | 默认模型通道可用且输出能解析 | 取构建日志**尾部**（`seq desc` 取 120 行、上限 16KB）拼进提示词，要求只输出 `{reason, suggestion, confidence}`；结论落库缓存 |
| `rule` | 未配置通道 / 调用失败 / 输出无法解析 / 没有日志 | 退回按 `error_code` 的确定性结论，`notice` 写明模型为什么没参与 |

**降级而不是报错**：错误码本身（如 `NPM_INSTALL`）已经指明了原因，让用户拿不到任何结论，
比拿到一个标注了来源的结论更糟。所以模型挂了仍是 200，只是 `source=rule` + 一句实话。

几个刻意的选择：

- **只取日志尾部**：根因几乎总在最后几十行，从头截只会留下"正在拉取依赖…"这类噪音。
- **提示词第一条就是"不要编造日志里没有的包名/版本号"**：模型在信息不足时最自然的动作是补一个
  像样的版本号，而运维看到"`@yunnian/shared@2.6.1` 不存在"这种具体到版本号的假原因，
  会真的去查那个版本——比不给结论更费时间。
- **解析不假设模型听话**：剥 ```` ```json ```` 围栏、容忍前后寒暄、从第一个 `{` 起用 `json.Decoder`
  解**一个**对象（按最后一个 `}` 截取，会在 `reason` 里含 `}` 时截错）；`confidence` 只钳制不换算
  （模型常把 0.9 写成 90，钳成 1 仍是"高置信"，而擅自除以 100 会把真的 0.9 变成 0.009）。
- **只缓存模型结论**：规则结论是纯函数、重算零成本；模型结论重算要花钱要等，
  而同一条失败构建的日志与错误码不会再变。
- **模型调用有独立的 45s 上限**，短于通道自身 timeout（60~120s）：诊断是"点一下就要等"的交互，
  宁可早点降级，也不让抽屉一直转圈；通道 timeout 留给流式问答那种能看着输出的场景。

**前提**：通道要在「系统管理 › 模型设置」里配好 Key。该菜单按 `user:manage` 权限显示，
`ops` 角色**看不到它**（接口对 ops 也直接 404）——用 admin 登录才配得到。没配不会报错，
只会一直走 `rule` 并在 `notice` 里说明。三种情形（模型成功 / 缓存命中 / 模型下线降级）
已用本地 OpenAI 兼容桩实测，单测覆盖解析与降级，见 `ci_diagnosis_test.go`。

### 部署失败归因：模型优先、带集群侧证据（`CDService.DeploymentAIDiagnosis`）

`GET /deployments/{id}/ai-diagnosis`。两级来源、如实标注 `source`、降级不报错、只缓存模型结论
——**与上面 CI 那节的四条口径完全一致**，共用 `parseModelDiagnosis` / `stripCodeFence` /
`modelDiagnosisTimeout(45s)` 同一份实现。

**为什么不直接复用 CI 那个函数**：两者的**证据完全不同**。CI 的证据是一份构建日志（一段文本）；
CD 的证据是三块，其中最关键的一块平台自己并不拥有：

| 证据块 | 来源 | 缺失时的表现 |
| --- | --- | --- |
| 部署上下文 | `deploy_record` + `service_instance` + `environment`（含"变更前在跑哪一版"） | 必有 |
| 平台部署日志尾部 | 日志流快照，**易失**（平台重启后取不回） | 提示词里写"（无。日志流是易失的…）" |
| **集群侧证据** | `Cluster.DiagnoseFailure`（只读）：容器状态 / 退出码 / 崩溃前那一轮日志 / Pod 事件 | 见下面"读不到 ≠ 不存在" |

抽成共用函数就得让调用方先把证据压成一段文本，那等于把"结论是从哪块证据读出来的"这件事藏起来
—— 而这恰恰是决定结论对不对的地方。共用的部分在下游：提示词骨架、解析、落库形状。

**提示词里 CD 特有的四条规则**（`deployDiagnosisSystemPrompt`），全部来自实测踩到的坑：

1. **平台日志没写，不等于没发生**：平台只看得到"下发成功 / 等待滚动 / 超时"三个动作。
   实测一次 demobasic 失败，平台日志只有三行进度，真因在容器侧。模型很容易因为"平台日志没有报错"
   就回一句"未发现异常"——那是把"我们没看到"说成了"没问题"。
2. **分清"容器起不来"与"平台等超时"**：前者有 `CrashLoopBackOff`/`OOMKilled`/`ImagePullBackOff`/
   明确退出码，原因要从那里读；后者只是慢，不许硬编一个崩溃原因。
3. 判断"是不是本次发布引入的"要给依据（旧版本仍在服务 → 集群与依赖整体是好的）。
4. 取证说明里写了哪块缺失，要在原因里说明，并给出"拿到那块证据后该看什么"。

**几个刻意的选择**：

- **`previous=true` 的容器日志优先于当前实例。** `CrashLoopBackOff` 时当前实例往往一行都没输出
  （还没跑起来就挂了），真因在**上一轮**的日志里。`PodEvidence.LogsFrom` 明确区分
  `previous`/`current`，渲染成"崩溃前那一轮"/"当前这一轮"——只写"容器日志"会让人以为是这次的。
- **Pod 事件按 `involvedObject.name` 逐个查，不 List 整个 namespace**：`default` 里有 47 个
  与本服务无关的负载，List 回来一大半是噪音，还会把别的服务的报错混进证据。
- **判断 Pod 是否就绪不能只看 `phase`**：`CrashLoopBackOff` 的 Pod，`phase` 仍然是 `Running`。
  要用 `Ready` condition。只看 phase 会把最需要诊断的那类失败判成"没问题"。
- **容器状态按"信息量"打分选**：`Waiting`(4) > `Terminated`(3) > `LastTermination`(2) >
  `Running 但未就绪`(1)，`RestartCount>0` 加分 —— 一个 Pod 上可能同时挂着几种状态，
  要挑最能说明问题的那一个。
- **"读不到" ≠ "不存在"**：`DiagnoseFailure` 拿不到证据时**必须返回 error**（进 `Notes`），
  绝不能返回一个空结构体。下游对这两者的结论正好相反：一个是"我们没看到"，一个是"应用没报错"。
  未接入集群时 `integration_local.go` 同样返回错误，不返回空证据。
- **取证用 `context.WithoutCancel(ctx)` + 独立 20s 预算**：这次诊断很可能就是因为"进来的 ctx
  已经超时"才触发的，拿那个 ctx 去取证必然立刻失败。
- ⚠️ **日志截断必须保尾部，行数上限要装得进字节上限**（2026-10-08 事故，已实测复现与回归）：
  取日志是 `TailLines=80`（最后 80 行）**加上**一个字节上限，两步的**方向必须一致**。
  原实现用 `io.LimitReader(rc, 8<<10)` —— 它是从**流的开头**读的，语义是"留下最早的 8KB、
  丢掉最后几行"。实测一次 Java 崩溃：80 行共 14854 字节，8KB 边界落在第 44 行，而最内层那条
  `Caused by: java.lang.IllegalArgumentException: Could not resolve placeholder
  'messagelibrary.mq.username'` 在第 47 行（累计 8437 字节）—— **正好被切掉**。
  后果不是报错，而是模型拿着满屏 `at org.springframework...` 帧行，编出一个
  "应用进程在端口 9629 上未监听" 这类**看似合理但错误**的结论（同一条部署单在修好取证后
  改为"缺少配置项 `messagelibrary.mq.username`"）。
  现在的取法：`evidenceLogReadCap`(1MB) 只防"超长单行撑爆内存" → `tailLogText` 保**尾部**
  `evidenceLogBytes`(32KB) 并对齐行边界。32KB 是刻意留的余量（80 行真实 Java 崩溃约 14.8KB），
  目的是让"行数"成为**唯一**的截断依据、字节上限退化成安全网 —— 否则会出现
  "按行数取回来、又被字节数悄悄削掉几行"，而削掉的恰好是根因。
  回归测试：`integration_k8s_diagnosis_test.go` 的 `TestTailLogText_KeepsTailNotHead` /
  `TestEvidenceLogLimits_CoverARealJavaCrash`。
- **缓存只认 `source=model` 的那条**：失败时 `failDeploy` 写进 `ai_diagnosis` 的是**规则**结论，
  把它当模型结论返回就是谎报来源 —— 会出现"标签写着本地规则、文案却是模型口气"。

**卡住的 rollout 提前判失败（同批落地）**：`integration.waitRollout` 现在自己读 Deployment 的
`Progressing=False / reason=ProgressDeadlineExceeded`，命中即返回 `ErrRolloutStuck` → 错误码
`ROLLOUT_STUCK`。判据**只认这一个 reason**（`Progressing=False` 还有 `DeploymentPaused` 等，
放进来会把"暂停"报成"失败"）。失败从等满 15m 缩到约 600s（k8s 未设 `progressDeadlineSeconds`
时的默认值，实测 demobasic 即此值）。显示时写明"600s（k8s 默认值）"——显示 `0s` 会被读成"立即超时"。

**端到端实测（2026-10-08）**：本机 4 条模型通道全部 `api_key_cipher IS NULL`（NO-KEY），
真通道不可用，所以起了一个**零依赖的本地 OpenAI 兼容桩**（`/v1/chat/completions` 返回
`{reason,suggestion,confidence}`，并把收到的 prompt 落盘）。桩把"收到的证据指纹"回显进 `reason`
——只回固定文案只能证明"模型被调用了"，回显证据才能同时证明**证据确实被组装进了提示词**。
结果：`source=model`、`model` 回显通道名、四块证据齐全（部署上下文 / 平台日志 / 集群侧证据 /
取证说明）；二次调用 5.6ms 命中缓存、模型请求数不增；停掉桩后 `source=rule` + `notice` 带真实
`connection refused`，接口仍 200。**验完立即还原**了 `ai_model_config` 与 `deploy_record.ai_diagnosis`
（原值从 binlog 的 UPDATE 前镜像找回，不是猜的）。

相关文件：`service/cd_diagnosis.go`、`integration/integration_k8s_diagnosis.go`、
单测 `cd_diagnosis_test.go` / `integration_k8s_writes_test.go`。

### 部署失败时，把集群侧证据**也写进部署日志流**（2026-10-08 落地）

`CDService.appendDeployEvidenceLogs`（`service/cd_diagnosis.go`），在 `runDeploy` 的失败分支里调用。

**为什么要有这一步**：上面那节的 AI 归因给的是**结论**，而且按需触发（用户点开抽屉才拉）、
还可能降级（通道没配 Key 就退回错误码规则，那几句是套话）。用户真正卡住的地方是
"平台日志翻来覆去只说'等待就绪探针'，我到底怎么知道为什么起不来" —— 以前只能去开终端
跑 `kubectl get events / describe`。现在失败那一屏的日志流里直接给出**原始证据**：

```
[error] 滚动更新卡死：新 Pod 在进度截止时间内始终没有就绪（工作负载 default/demobasic，
        progressDeadlineSeconds=600s）：ReplicaSet "demobasic-59b46b9bfb" has timed out progressing.
[warn ] 平台不会自动回滚，工作负载当前停在本次变更后的镜像上。…
[info ] ———— 以下为集群侧证据（只读采集，原文未加工）————
[error] Pod demobasic-59b46b9bfb-bxrzw：phase=Running，重启 15 次，容器 master
[error]   容器状态：CrashLoopBackOff（退出码 1）
[error]   kubelet 说明：back-off 5m0s restarting failed container=master pod=…
[warn ]   事件 [BackOff] Back-off restarting failed container
[warn ]   事件 [Unhealthy] Readiness probe failed: Get "http://10.233.90.102:9629/digiwin/info": dial tcp …
[warn ]   事件 [Pulled] Container image "…:1.2.0.2761" already present on machine
[warn ]   事件 [Created] / [Started] / [Scheduled] …
[info ] Pod … 的容器日志（崩溃前那一轮，末 20 行；完整原文见「AI 诊断」）：
[info ]   Caused by: java.lang.IllegalArgumentException: Could not resolve placeholder 'messagelibrary.mq.username'
```

**几个刻意的取舍**：

- **取的是原始证据，不是结论**。事件原文、容器退出原因、崩溃日志都是集群给的，
  平台一个字没加工 —— 结论可能降级，证据不会。两者并存，排查顺序是"先看证据，再信结论"。
- **写给人的那一份与给模型的那一份刻意不共用**（`deployEvidenceLogLines` vs
  `renderFailureEvidence`）：读者不同，取舍就不同。人这份要短、先给结论（哪个 Pod、坏在哪）、
  容器日志**只回放 20 行**（80 行 Java 栈会把这一屏冲走，而根因在最后几行）；
  模型那份要完整铺开 —— 它自己会挑重点，替它截等于替它丢证据。
- **取证说明排在证据之前**，不是之后：它决定下面这些证据能信几分（哪块没拿到、为什么），
  压到末尾一定被长日志冲掉。
- **取不到证据只写一行 `warn`，不报错、也不静默**。静默会让人把"我们没看到"读成
  "集群里没问题"，那是相反结论；而让"取证失败"变成"部署又失败一次"更是本末倒置。
- **独立 20s 预算 + `context.WithoutCancel`**：同上面的归因，失败分支里的 ctx 很可能已经 done
  （部署超时就是最常见的失败原因之一）。
- **全程只读**：取证发生在一个已经失败的发布之后，此时任何写动作都是把事故扩大。

回归测试：`cd_diagnosis_test.go` 的 `TestDeployEvidenceLogLines_NotesFirstAndLogsFromTail`
（钉死"取证说明在最前 + 日志只留尾部"）、`_EmptyLogsAreSaidOut`、`_AllReadyPointsAtTimeout`。

> ⚠️ **一个必须知情的边界**：`waitRollout` 判"卡死"读的是 Deployment **当前**的
> `ProgressDeadlineExceeded` 条件，它可能来自上一次失败。所以对同一个镜像再发一次
> （比如部署记录里点「重试」）会**立刻**报 `ROLLOUT_STUCK`，而不是等 600s。
> 这是自洽的：集群当下确实处于"有 Pod 起不来"的状态，重试同一镜像并不会让它好起来 ——
> 立刻失败 + 给出证据，比让用户干等十分钟更有用。要真正恢复需改配置后换个 tag 发布，
> 或按「AI 诊断」的建议回滚。

### 副本数：期望数与实际存在数**必须分开**（`WorkloadState.ActualReplicas`）

`integration.WorkloadState.ActualReplicas` / `ServiceLiveDTO.actualReplicas`
（JSON `actualReplicas`），取自 `Deployment.status.replicas`。

界面上「副本」列按 `readyReplicas / max(replicas, actualReplicas)` 渲染
（前端统一口径在 `utils/live.ts` 的 `liveReplicaTotal`）。

**为什么不能只用 `spec.replicas` 当分母**：滚动更新卡住时新 Pod 反复崩溃、旧 Pod 因
1 副本下 `maxUnavailable` 向下取整为 0 而**不会被杀**，于是集群里实际有 2 个 Pod、
期望仍是 1 个、就绪是 1 个 —— 实测 demobasic 就是 `spec=1 / status=2 / ready=1`。
只看比值会渲染成绿色的 `1/1`，把"有一个实例正在崩"整个抹掉；用户在自己的运维面板里
看到的是 `1/2`，两边对不上反而显得平台在骗人。取 `max` 是为了两头都不漏：
期望高（刚扩容、还在拉起）与实际高（有残留实例）都要能看出来。

回归测试：`integration_k8s_state_test.go`。

### AI 助手：从「复述快照」改成「自己取数」（2026-10-09 落地）

**问题**：用户在聊天框问「帮我看看测试区192环境 demouser 服务的状态」，
助手回答「当前上下文没有关于『测试区192环境』或『demouser』服务的任何数据」，
然后列了三条"请你自己去页面里点"的建议。功能没坏 —— 但它答的不是用户问的。

**根因**：`AIService` 只做一次性事实拼装 —— 提问前把一份快照塞进提示词，
模型只能在这张快照里找。而快照的内容取决于前端传的 `context.envKey / projectKey`，
聊天框里问的这两句**不在任何页面上下文里**；更关键的是，平台**从没有把口语名
（"测试区192"）映射到标识（`env_key=test`）的能力**。它只有查询能力，没有取数能力。

**改法**：给模型挂 8 个**只读工具**，让它自己查（`internal/service/ai_tools.go`）。

| 工具 | 数据源 |
| --- | --- |
| `list_environments` / `list_projects` | 平台库（把口语名对到标识） |
| `list_env_services` | 平台库（**登记值**，可能滞后，返回里显式标注） |
| `get_service_status` | **集群实时** `Cluster.Describe` |
| `diagnose_service` | **集群实时** `Cluster.DiagnoseFailure` |
| `get_cluster_resources` | **集群实时** `Cluster.Nodes` |
| `list_deployments` / `list_builds` | 平台库 |

实测效果（同一个问题）：

```
[tool] 读取环境名录            args={}
[tool] 读取项目名录            args={"keyword":"demouser"}
[tool] 查询集群实时状态        args={"envKey":"test","projectKey":"demouser"}
→ 「测试区192环境（env_key=test）中 demouser：实际存在副本 1 / 就绪 1，
    镜像 demouser:1.2.0.1471，最近部署单 DP202610090007（系统管理员，成功）」
```

**六个刻意取舍**：

1. **只读是结构性保证，不是提示词承诺**。工具注册表里根本没有
   apply / scale / restart / rollback ——模型说"我帮你重启了"也没有东西可调。
   `ai_tools_test.go` 用**精确集合**断言钉住这份清单（不是 Contains）：将来有人顺手
   加一个写工具，测试就红，而那就该是它在评审里被拦下来的时刻。
2. **不把集群凭证交给模型**。工具在平台进程内执行，用平台自己那套 K8s 客户端
   （环境自带 kubeconfig / 集群接入凭据，见 `KubeconfigSource`）。模型只看到返回值。
   用户提的"给他集群密钥让他自己查"，正解是**把能力包成工具**，不是把凭证递过去 ——
   提示词里的凭证会被一段可能被注入的文本操纵。
3. **每轮都流式，不是"探路+收尾"**。前几轮非流式会让**不需要工具**的普通提问
   （占多数）退化成"等模型全生成完再一次性吐出来"，打字机与首字延迟全赔。
   实测百炼 compatible-mode 的流式响应会按 `index` 分片下发 `tool_calls`，所以可行。
4. **工具执行期必须推进度事件**。查集群要几百毫秒到几秒，这期间**一个 token 都没有**。
   SSE 因此多了一类 `tool` 事件（`handler_ai.go`），前端渲染成「查询过程 · 共 N 次」。
   不推它，用户看到的就是"发出去没反应"。
5. **工具失败回灌 `ok=false`，不抛错**。抛错整条对话就崩；而**静默返回空对象最糟** ——
   模型会读成"查了，没问题"，与事实正好相反。参数不是合法 JSON 也照样回灌：
   那是模型的锅，但只有它能改。
6. **「没读到」与「不存在」分开**。`Describe` 的三种情形（读到且存在 / 读到不存在 /
   没读到）不能混为一谈，否则权限不足会被说成"集群里没这个服务"。

**顺带修掉的一个既有缺陷**：`ai.go` 原来在拼服务实例上下文时把
`service_instance.liveness_ok / readiness_ok` 当"健康状态"喂给模型 —— 那两列是
**种子数据写死的**（`status != "error"` / `status == "running"`），拿它当集群健康
是个假前提（此前已记录在待修清单）。现在这条路径整体删掉了：模型要健康状态就调用
`get_service_status` 去集群读真的。`crossFacts` 与随之失去调用点的 `userNameOf` 一并移除。

**模型通道的能力探测**：`cmd/llmprobe` 是可重跑的诊断工具，一分钟回答
"当前通道支持不支持 `tools` / `role:\"tool\"` / 流式 `tool_calls`"。
**新增模型通道时应该跑一次** —— 厂商名字不能决定行为，API 路径才能：

| 端点 | `role:"tool"` | `tools` | 流式 `tool_calls` |
| --- | --- | --- | --- |
| `.../compatible-mode/v1/chat/completions` | ✅ 200 | ✅ 原生 tool_calls | ✅ 分片下发 |
| 百炼 Responses API | ❌ 400 InvalidParameter | — | — |

> ⚠️ **一个必须知情的边界（待用户决定）**：工具**没有**按环境做权限收敛 ——
> 有 `ai:query` 的用户可以通过工具读到任意环境（含 prod）的集群实时状态。
> 这一点在加工具之前就存在（旧快照逻辑同样不区分环境），但工具把可读内容从
> "平台登记值"扩到了"集群实时值"。要不要按环境门控，等你拍板。

**回归测试**：`internal/llm/llm_stream_tools_test.go`（**真实抓包帧**，5 条：分片拼接、
前言+工具调用共存、普通回答不受影响、并行调用归位、残片丢弃）、
`internal/service/ai_tools_test.go`（8 条：只读集合钉死、schema 非 null、
未知工具回灌、坏 JSON 回灌、参数解析、失败形态）。
