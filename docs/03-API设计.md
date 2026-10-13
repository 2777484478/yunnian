# YunNian 云辇 · 后端 API 设计（RESTful v1）

> Base URL：`https://yunnian.internal/api/v1`
> 认证：`Authorization: Bearer <access_token>`（JWT，有效期 2h；`refresh_token` 14d）
> 内容类型：`application/json; charset=utf-8`；时间统一 `ISO 8601` 带时区 `2026-09-22T16:01:03+08:00`

---

## 0. 通用约定

### 0.1 统一响应体

```json
{
  "code": 0,
  "message": "ok",
  "data": { },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

| 字段 | 说明 |
| --- | --- |
| `code` | `0` 表示成功；非 0 为业务错误码（见 0.3） |
| `message` | 人类可读提示，可直接 toast |
| `data` | 业务数据；失败时为 `null` |
| `traceId` | 用于关联服务端日志与审计 |

### 0.2 分页

请求（Query）：

```
GET /projects?page=1&pageSize=10&keyword=user&type=frontend&enabled=1&sortBy=updated_at&order=desc
```

响应：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "list": [],
    "total": 20,
    "page": 1,
    "pageSize": 10
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

约束：`page ≥ 1`，`1 ≤ pageSize ≤ 200`，`sortBy` 白名单校验（防 SQL 注入）。

### 0.3 错误码

| code | HTTP | 含义 |
| --- | --- | --- |
| 0 | 200 | 成功 |
| 40000 | 400 | 参数校验失败（`data.errors` 给出字段级错误） |
| 40100 | 401 | 未登录 / Token 失效 |
| 40101 | 401 | Token 过期，请用 refresh_token 续期 |
| 40300 | 403 | 无权限（缺少权限点） |
| 40301 | 403 | 环境已停用，禁止部署 |
| 40400 | 404 | 资源不存在 |
| 40900 | 409 | 唯一键冲突（如 project_key 重复） |
| 40901 | 409 | 状态冲突（如该服务有部署进行中） |
| 40902 | 409 | 幂等冲突（重复提交返回已有任务） |
| 42900 | 429 | 触发频率超限 |
| 50000 | 500 | 服务端异常 |
| 50200 | 502 | 上游系统不可用（GitLab/Harbor/K8s） |

### 0.4 SSE 约定

日志类接口返回 `text/event-stream`，事件类型固定三种：

| event | data | 说明 |
| --- | --- | --- |
| `log` | `{seq,ts,level,line}` | 一行日志；`id:` 字段为 `seq`，用于断线续传 |
| `status` | `{status,progress,stage,duration,image?}` | 状态变化 |
| `end` | `{reason}` | 流结束（`finished`/`canceled`/`timeout`） |
| `ping` | `{}` | 每 15s 心跳，防代理断连 |

---

## 1. 认证与用户

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| POST | `/auth/login` | 公开 | 账号密码登录 |
| GET | `/auth/oidc/authorize` | 公开 | 跳转 SSO 授权 |
| GET | `/auth/oidc/callback` | 公开 | SSO 回调，签发 token |
| POST | `/auth/refresh` | 公开 | 刷新 access_token |
| POST | `/auth/logout` | 登录态 | 登出并吊销会话 |
| GET | `/auth/me` | 登录态 | 当前用户 + 权限点 |
| GET | `/users` | `user:manage` | 用户列表（**仅管理者用**） |
| GET | `/users/options` | `env:write` | 轻量用户选择器（只回 `id/username/name`） |
| POST | `/users/invite` | `user:manage` | 邀请用户 |
| PATCH | `/users/{id}` | `user:manage` | 改角色 / 启停 |
| POST | `/users/{id}/reset-password` | `user:manage` | 重置密码 |
| GET | `/roles` | `user:manage` | 角色与权限点 |
| PUT | `/roles/{code}/permissions` | `role:manage` | 调整角色权限 |

**POST /auth/login**

```json
// Request
{ "username": "ops", "password": "******" }
```

```json
// Response
{
  "code": 0,
  "message": "ok",
  "data": {
    "accessToken": "eyJhbGciOiJIUzI1NiIs...",
    "refreshToken": "rt_9f2c1a9b4e7d4c1e",
    "expiresIn": 7200,
    "user": {
      "id": 15,
      "username": "ops",
      "name": "运维示例",
      "email": "xufeng@yunnian.cn",
      "role": { "code": "ops", "name": "运维" },
      "teams": ["基础架构组"],
      "permissions": ["env:*", "deploy:*", "rollback:*", "project:read", "build:*", "audit:read"]
    }
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

---

## 2. 项目（CI 配置）

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| GET | `/projects` | `project:read` | 项目列表（含最近构建） |
| GET | `/projects/{key}` | `project:read` | 项目详情 |
| POST | `/projects` | `project:write` | 新建项目 |
| PUT | `/projects/{key}` | `project:write:own` | 编辑项目 |
| DELETE | `/projects/{key}` | `project:delete` | 删除项目（软删） |
| PATCH | `/projects/{key}/enabled` | `project:write:own` | 启用 / 停用 |
| GET | `/projects/default-build-script` | `project:read` | 取平台内置的默认构建脚本（`?type=backend|frontend`） |
| POST | `/projects/test-connection` | `project:write` | **测试连接**：真跑一次 `git ls-remote` |
| GET | `/projects/{key}/branches` | `project:read` | 拉取远端分支列表 |
| POST | `/projects/{key}/validate` | `project:write` | 校验仓库连通性与 Dockerfile 是否存在 |
| GET | `/projects/{key}/builds` | `build:read` | 项目构建历史 |
| GET | `/projects/{key}/images` | `image:read` | 项目镜像版本 |
| GET | `/projects/{key}/deployments` | `deploy:read` | 项目在各环境的部署情况 |

> `/projects/default-build-script` 与 `/projects/test-connection` 是**静态段**，注册在 `/:key` 之前。
> gin 的路由树本来就是静态优先，但这两条靠框架行为很容易被后人改坏，注释里写明了别动。

**GET /projects**（响应节选）

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "list": [
      {
        "id": 3,
        "key": "user-web",
        "name": "用户中心前端",
        "type": "frontend",
        "team": "用户增长组",
        "owner": { "id": 1, "name": "张伟" },
        "buildMethod": "dockerfile",
        "gitUrl": "https://gitlab.yunnian.internal/frontend/user-web.git",
        "defaultBranch": "master",
        "registry": "harbor.yunnian.internal",
        "imageName": "frontend/user-web",
        "tagPattern": "{branch}-{commitId}-{timestamp}",
        "enabled": true,
        "stats": { "totalBuilds": 412, "successRate": 96, "avgDuration": 96 },
        "lastBuild": {
          "id": 8821,
          "buildNo": 412,
          "branch": "master",
          "commitId": "a91f3c2",
          "status": "success",
          "duration": 96,
          "image": "harbor.yunnian.internal/frontend/user-web:master-a91f3c2-202609221600",
          "imageDigest": "sha256:9f21c4d8...a7e3b015",
          "startedAt": "2026-09-22T16:00:12+08:00",
          "triggerName": "yn-ci-bot"
        }
      }
    ],
    "total": 20,
    "page": 1,
    "pageSize": 10
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

**POST /projects**（请求）

```json
{
  "key": "report-web",
  "name": "报表前端",
  "type": "frontend",
  "gitProvider": "gitlab",
  "gitUrl": "https://gitlab.yunnian.internal/frontend/report-web.git",
  "gitUsername": "escloudapi",
  "gitPassword": "glpat-xxxxxxxx",
  "defaultBranch": "master",
  "buildMethod": "dockerfile",
  "dockerfilePath": "Dockerfile",
  "buildContext": ".",
  "registry": "es-repo.example.com:5000",
  "imageName": "escloud/report-web",
  "tagPattern": "v{buildNo}",
  "buildParams": [
    { "key": "AREA", "value": "test", "desc": "运行区域" }
  ],
  "team": "数据平台组",
  "owner": "张伟",
  "description": "报表中心前端",
  "enabled": true
}
```

> **仓库凭据按项目存（`gitUsername` + `gitPassword`）**
>
> 早先是"一份平台级 GitLab 通道配所有项目"，那条路有两个死结：不同仓库可能在不同的
> GitLab、用不同的机器人账号；而且那条通道一旦 `enabled=false`，**所有项目会静默退化成
> 匿名检出** —— 表现为"昨天还好好的，今天全 401"。
>
> - `gitUsername` **明文**存库。它不是秘密：会出现在 `git remote` 地址、CI 日志、GitLab 提交记录里。
>   空 = 匿名拉取（公开仓库是合法形态，不能当错误拦）。
> - `gitPassword` 只在**入参**里出现，落库走 `secret_credential`（AES-256-GCM，`scope=project`）；
>   出参**一律不回显**。
> - 编辑时 `gitPassword` 留空 = **不修改**。任何"把已保存的密码回填进输入框"的做法，都等于把明文
>   发到浏览器，被密码管理器、前端日志、截图各留一份。
> - 凭据写入与项目行更新在**同一个事务**里。分开写会出现"项目建好了、密码没存上"，
>   首次构建报 401，而用户刚点过「测试连接成功」—— 他会去怀疑凭据本身，而不是平台没存。
> - 触发构建时凭据来源是 `openProjectGitCredential`：记录**存在但解不开**（密钥换了、行被删了）
>   会当场失败，**不许悄悄退回匿名**——那会把"平台配置坏了"伪装成"仓库 401"。

> **Tag 规则**
>
> 支持 5 个变量：`{buildNo}` `{branch}` `{commitId}` `{timestamp}` `{date}`。
> 默认 `v{buildNo}` —— `buildNo` 是**项目级构建序号**（`build_record.build_no`），每构建一次 +1，
> 天然唯一，产出 v1、v2、v3…，一眼看得出哪个更新。
>
> **`{buildNo}` 支持内联偏移**：`{buildNo+N}` / `{buildNo-N}`（负值结果夹到 0）。
> 给序号加一个固定偏移，版本号就能从某个数起步 —— 自建流水线常见的「`2.2.0.1000` 起」
> 就是 `2.2.0.{buildNo+999}`（首次构建即 1000，之后每次 +1）。**自定义脚本项目的默认规则
> 就是这一条**；dockerfile / jenkins 仍是 `v{buildNo}`。
>
> ⚠️ 偏移是按**该项目的构建序号**算的，而序号会被「失败也占号」和「删了重建」推动，
> 所以实际产出的号**会漂移**（`2.2.0.1000` 之后可能直接跳到 1006）。
> 要严格连续的版本号需要独立的版本序号字段（一期未做）——**别承诺它连续**。
>
> ⚠️ **失败的构建也占号**（序列会有空洞）。这是刻意的：改成"只数成功次数"就要在成功之后
> 回写 tag，而那时镜像已经推出去了，回写失败会导致下一次成功构建用同一个 tag **覆盖上一个**——
> 比序号有空洞严重得多。
>
> ⚠️ **认不出的花括号写法会被原样拼进 tag**：`renderTag` 只替换已知占位符，
> 而 docker 不允许 tag 里有花括号 —— 失败发生在 `docker build -t` 那一步、报
> `invalid reference format`，与"Tag 规则写错了"毫无关系。
> **前端表单已加校验提前拦下**（`unknownTagTokens`）；后端刻意不自动抹掉花括号 ——
> 抹掉会让 `v{buildno}` 变成合法的 `vbuildno`、构建成功但版本号永不递增，那是更难查的静默错误。

> **Jenkins 绑定形态（`buildMethod=jenkins`）**
>
> 代码怎么拉、怎么构建、镜像推到哪儿，全部写在那个 Jenkins Job 里，平台侧**不读**这些字段。
> 因此创建/编辑一个 Jenkins 项目时：
>
> - `gitUrl` **可以留空**（其余构建方式必填——平台自己要拉代码，没地址必然构建失败，
>   所以「仓库地址必填」是按构建方式判的，不是无条件必填）；
> - `jenkinsJob` **必填**（否则平台不知道该触发哪个 Job，提交会返回 `40000`）；
> - `registry` / `imageName` / `tagPattern` 可省略，后端补平台默认值（`imageName` 默认 `<type>/<key>`）——
>   其中 `imageName` 只用来在构建日志里挑出属于本项目的 `docker push`，不参与构建；
> - `POST /projects/{key}/validate` 对这类项目**跳过仓库连通性检查**，
>   结论里会写明「仓库由 Jenkins Job 管理，未检查仓库连通性」，而不是报一个不存在的失败。
>
> 前端入口：**项目管理 › 新建项目** —— 只有一个入口，构建方式在表单里选（**默认 Jenkins Job**）；
> 选它时只渲染「基本信息 + 构建方式 + Jenkins Job」这一组字段。

> **自定义脚本形态（`buildMethod=script`）**
>
> 构建流程**整个由 `buildScript` 定义** —— 拉代码、编译、打包、推送都写在里面。
> 与 `dockerfile` 模式的差别只有一处：**谁来描述打包过程**。前者用户给 Dockerfile、平台做
> `-f + build`；后者用户给脚本，平台只负责三件事：
>
> 1. 把仓库凭据配好（以 `GIT_ASKPASS` 预置，**脚本里写普通 clone 地址即可，不出现凭据**）；
> 2. 注入 `YUNNIAN_*` 环境变量 + 该项目的「构建参数」；
> 3. 把输出收进构建日志。
>
> - 平台执行前已经 `cd` 到本次构建的**空白工作目录**，PATH 里有 `git` 与 `docker`。
> - `buildScript` 为空时，后端补 `buildscript.DefaultScript(type)`（`go:embed` 下发，**不是前端硬编码**：
>   默认脚本必须和构建机上的镜像地址、注入的变量名保持一致）。
> - `dockerfilePath` / `buildContext` 在 `script` 模式下**不参与构建**，界面上也不展示
>   （cd 到哪、在哪个目录 build 由脚本自己定）。切换构建方式时它们的值会保留，不会被清掉。
> - 脚本里可以改的默认值只有 3 行：`BUILD_IMAGE`（编译环境镜像）、`BUILD_CMD`（编译命令）、
>   `DOCKERFILE`。
> - ⚠️ **`SCRIPT_NO_IMAGE` 是本模式最关键的一道检查**：一段只做 `git clone` 的脚本退出码是 0，
>   构建记录会显示"成功"、日志一片祥和，但镜像列表里什么都没有。平台唯一能查证的事实是
>   「构建机上有没有 `$YUNNIAN_IMAGE` 这个镜像」，查不到就判失败。

**GET /projects/default-build-script**（Response）

```json
{
  "code": 0,
  "message": "ok",
  "data": { "script": "#!/bin/sh\n# 云辇 · 自建构建脚本（后端 / Java · Maven）\n…# ===== [5/5] 推送镜像 =====\ndocker push \"$YUNNIAN_IMAGE\"\n" },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

**POST /projects/test-connection**

```json
// Request
{
  "projectKey": "report-web",
  "gitUrl": "https://gitlab.yunnian.internal/frontend/report-web.git",
  "gitUsername": "escloudapi",
  "gitPassword": "",
  "branch": "master"
}
```

```json
// Response —— 成功
{
  "code": 0,
  "message": "ok",
  "data": {
    "ok": true,
    "message": "连接成功",
    "branches": ["master", "release/2.6", "feature/login"],
    "branchTotal": 229,
    "defaultBranch": "master",
    "elapsedMs": 348
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

```json
// Response —— 失败：message 必须是**人话**
{
  "code": 0,
  "message": "ok",
  "data": {
    "ok": false,
    "message": "账号或密码不正确",
    "detail": "remote: HTTP Basic: Access denied\nfatal: Authentication failed for 'http://…'",
    "branches": [],
    "branchTotal": 0,
    "elapsedMs": 512
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

> 三条硬约束：
>
> 1. **真跑 `git ls-remote`，不走 GitLab REST API。** 构建用的是 git 协议 + 凭据助手，
>    探测必须**测同一件事**，否则就是在给"测试通过但构建 401"的假承诺。
>    出于同样理由，`GitProbe` 与真正的构建**共用同一份 `prepareAskpass`**。
> 2. **`message` 是翻成人话的结论**，不是 git 的原始 stderr。用户看到
>    `fatal: Authentication failed for 'http://…'` 的第一反应是"平台坏了"，而不是"密码错了"。
>    分类依据是**"用户下一步该改什么"**（地址写错 / 账号密码错 / 分支不存在 / 域名解析不了），
>    不是 git 的错误码。原始输出放 `detail`，只作折叠展开的定位用（不含凭据，构造时已打码）。
> 3. **`projectKey` + 留空 `gitPassword` 时回落已保存的凭据** —— "改完地址点一下测试"
>    不该逼用户把密码重敲一遍。
>
> 分支一起带上：远端没有这个分支是"构建必失败"的第一大来源，而它恰恰是**保存前就能发现**的。
> `res.OK` 为真但分支不存在时，接口会把 `ok` 改成 `false` 并给人话结论。
>
> 4. **`branches` 会被截到 100 条，`branchTotal` 才是远端的真实分支数。**
>    两个字段必须分开，因为它们回答的是两个问题："这些分支能选"和"远端一共多少个"。
>    合成一个就会说出假话 —— 实测踩过：`demowarning` 远端有 **229** 个分支，
>    而**分支存在性判定曾经拿截断后的 100 条来做**，于是
>    `dev`、`feature_*`（大写开头的分支排前面，小写开头的全在第 100 名之外）
>    全被判成「不存在」，提示里的计数也谎报成「远端有 100 个分支」。
>    用户据此会把一个**正确的**分支名改掉 —— 而这正是他点"测试连接"要解决的问题。
>    现在的口径：**存在性拿完整列表判、计数用 `branchTotal`、只有回给界面的列表才截断**。
>    界面照此显示「读到 229 个分支，已列出前 100 个」。
>    （选分支的控件是 `AutoComplete` 而非 `Select`，所以第 100 名之外的分支**仍可直接输入**，
>    截断只是"少了个便利"，不构成功能缺失。）

```json
// 校验失败示例（400）
{
  "code": 40000,
  "message": "参数校验失败",
  "data": {
    "errors": [
      { "field": "key", "message": "项目标识只允许小写字母、数字与中划线" },
      { "field": "imageName", "message": "镜像名称必须以项目类型目录开头，如 frontend/report-web" }
    ]
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

**POST /projects/{key}/validate**（新建前的连通性预检）

```json
// Response
{
  "code": 0,
  "message": "ok",
  "data": {
    "repoReachable": true,
    "defaultBranchExists": true,
    "dockerfileExists": true,
    "dockerfilePath": "Dockerfile",
    "registryReachable": true,
    "registryPushGranted": true,
    "warnings": ["仓库体积 320MB，建议开启 .dockerignore 忽略 node_modules"]
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

---

## 3. 构建

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| POST | `/builds` | `build:trigger` | **一键编译**（创建构建任务） |
| GET | `/builds` | `build:read` | 构建记录列表（全局） |
| GET | `/builds/{id}` | `build:read` | 构建详情（含步骤与镜像） |
| GET | `/builds/{id}/logs` | `log:read` | 拉取全量日志（分页） |
| GET | `/builds/{id}/logs/stream` | `log:read` | **SSE 实时日志** |
| POST | `/builds/{id}/cancel` | `build:cancel` | 取消构建 |
| POST | `/builds/{id}/retry` | `build:trigger` | 重新构建（同分支同参数） |
| GET | `/builds/{id}/ai-diagnosis` | `ai:query` | AI 失败归因（失败构建） |
| POST | `/webhooks/gitlab` | 签名校验 | GitLab push 事件 |
| POST | `/webhooks/github` | 签名校验 | GitHub push 事件 |

**POST /builds**（请求）

```json
{
  "projectKey": "user-web",
  "branch": "master",
  "tagOverride": null,
  "params": {
    "NPM_REGISTRY": "https://registry.npmmirror.com",
    "BUILD_MODE": "release"
  },
  "notify": true,
  "idempotencyKey": "user-web:master:a91f3c2"
}
```

```json
// Response 201
{
  "code": 0,
  "message": "构建任务已创建",
  "data": {
    "id": 8822,
    "buildNo": 413,
    "projectKey": "user-web",
    "branch": "master",
    "status": "queued",
    "queuePosition": 2,
    "logStreamUrl": "/api/v1/builds/8822/logs/stream",
    "createdAt": "2026-09-22T16:02:11+08:00"
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

```json
// 重复提交（幂等命中）→ 200
{
  "code": 40902,
  "message": "该 commit 在 10 分钟内已提交构建，返回已有任务",
  "data": { "id": 8822, "buildNo": 413, "status": "queued" },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

> **这个接口只服务于"平台自己构建"的项目**（`build_method` = `dockerfile` / `script`）。
> `build_method=jenkins` 的项目会被直接拒绝（**40901**）：
>
> ```json
> { "code": 40901, "message": "项目 demoitms 的构建方式为 Jenkins Job（镜像打包-demoitms），平台不会代为执行 docker 构建；请到「项目管理」的项目编译里选择构建分支，由平台触发 Jenkins 执行" }
> ```
>
> 少这道校验的后果很隐蔽：调用方把一个 Jenkins 的**版本标签**当平台分支名传进来，
> 平台会去代码托管查一个提交、跑一次自己的 docker 构建，最后落一条与 Jenkins
> 毫无关系的"成功"记录——看起来完全正常。前端两条入口（项目编译）已按 `buildMethod`
> 分流到 `/system/integrations/jenkins/trigger`，这里是兜底。

**GET /builds/{id}/logs/stream**（SSE 原始报文）

```
HTTP/1.1 200 OK
Content-Type: text/event-stream; charset=utf-8
Cache-Control: no-cache
X-Accel-Buffering: no

retry: 3000

event: log
id: 1
data: {"seq":1,"ts":"2026-09-22T16:02:14+08:00","level":"cmd","line":"$ git clone --depth 1 -b master https://gitlab.../user-web.git ."}

event: log
id: 2
data: {"seq":2,"ts":"2026-09-22T16:02:26+08:00","level":"info","line":"Step 6/9 : RUN npm run build"}

event: status
data: {"status":"running","progress":62,"stage":"编译构建"}

event: log
id: 3
data: {"seq":3,"ts":"2026-09-22T16:03:48+08:00","level":"info","line":"✅ 构建成功，镜像已推送至镜像仓库。"}

event: status
data: {"status":"success","progress":100,"stage":"已完成","duration":96,"image":"harbor.yunnian.internal/frontend/user-web:master-a91f3c2-202609221600","imageDigest":"sha256:9f21c4d8...a7e3b015"}

event: end
data: {"reason":"finished"}
```

**GET /builds/{id}**（响应节选，含构建成功后的完整展示信息）

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "id": 8822,
    "buildNo": 413,
    "project": { "key": "user-web", "name": "用户中心前端", "type": "frontend" },
    "branch": "master",
    "commitId": "a91f3c2",
    "commitMsg": "fix: 修复订单超时未自动关闭的问题",
    "triggerType": "manual",
    "triggerName": "张伟",
    "status": "success",
    "stage": "已完成",
    "progress": 100,
    "durationSec": 96,
    "startedAt": "2026-09-22T16:02:11+08:00",
    "finishedAt": "2026-09-22T16:03:47+08:00",
    "runnerId": "runner-02",
    "paramsSnapshot": { "NPM_REGISTRY": "https://registry.npmmirror.com", "BUILD_MODE": "release" },
    "image": {
      "id": 5521,
      "image": "harbor.yunnian.internal/frontend/user-web:master-a91f3c2-202609221600",
      "tag": "master-a91f3c2-202609221600",
      "digest": "sha256:9f21c4d8...a7e3b015",
      "sizeBytes": 402653184,
      "pushedAt": "2026-09-22T16:03:45+08:00",
      "pullCommand": "docker pull harbor.yunnian.internal/frontend/user-web:master-a91f3c2-202609221600"
    },
    "artifactUrl": "/api/v1/builds/8822/artifacts",
    "logSizeBytes": 284120,
    "deployedEnvs": ["test"]
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

> **失败原因的两个字段（2026-10-10 起，构建与部署同口径）**
>
> | 字段 | 是什么 | 用途 |
> | --- | --- | --- |
> | `errorCode` | 机器码：`TIMEOUT` / `ROLLOUT_STUCK` / `QUEUE_REJECTED` / `INTERRUPTED` / `NPM_INSTALL` … | 筛选、统计、按码归类 |
> | `errorMessage` | **人类可读的真因原文**，如「滚动更新卡死：新 Pod 在进度截止时间内始终没有就绪（工作负载 default/demobasic，progressDeadlineSeconds=600s）」 | 解释现场 |
>
> 为什么两个都要：机器码解释不了现场，而真因在此之前**只活在日志流里** ——
> 日志流是进程内 + TTL 的，平台一重启那条记录就只剩一个机器码（构建侧、部署侧都是这个毛病）。
> 所以失败那一刻的原文必须落库。落这列之前产生的旧记录两者都可能是空，此时前端要**如实显示
> 「未记录失败原因」**，不要留白 —— 留白会被读成"这条没有失败原因"，而事实是"当时没记下来"，
> 是相反的结论。`errorMessage` 落库前按**字符**截断到 512（不是字节：按字节会把汉字切成乱码）。

**GET /builds/{id}/ai-diagnosis**（失败构建的 AI 归因）

归因有**两级来源**，`source` 标明这次实际走的是哪一级，前端必须按它措辞——
把规则结论说成"模型置信度 92%"，会让人以为日志已经被模型看过一遍：

| source | 何时出现 | 说明 |
| --- | --- | --- |
| `model` | 平台默认模型通道可用，且输出能被解析 | 把失败上下文 + 日志尾部（最多 120 行 / 16KB）交给模型；结论落库缓存，同一构建再次查询不再调用模型 |
| `rule` | 通道未配置 / 调用失败 / 输出无法解析 | 退回按错误码的确定性结论，`notice` 说明模型为什么没参与 |

模型成功时的返回：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "source": "model",
    "model": "qwen-plus",
    "reason": "npm 私服缺少内部包 @yunnian/shared@2.6.1（ETARGET），构建在依赖安装阶段中断。",
    "suggestion": "确认该版本是否已同步到私服；若尚未发布，把 package.json 依赖回退到已同步版本后重新构建。",
    "confidence": 0.88,
    "errorCode": "NPM_INSTALL"
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

模型不可用时**仍是 200**（降级不是错误，用户要的是结论而不是报错）：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "source": "rule",
    "notice": "本次未由模型分析（尚未配置可用的模型通道，请到「系统管理 › 模型设置」添加），以下结论来自平台错误码规则。",
    "reason": "Java 编译失败：`OrderController` 引用了不存在的 `PageQuery` 类……",
    "suggestion": "检查 `PageQuery` 是否已迁移到公共模块，并补上对应 import。",
    "confidence": 0.9,
    "errorCode": "COMPILE_ERROR"
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

> 早期设计稿里的 `evidence`（命中的日志行）与 `similarBuilds`（历史相似构建）**一期未实现**：
> 前者要日志检索、后者要历史召回，都不在本期范围内。

---

## 4. 镜像

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| GET | `/images` | `image:read` | 镜像列表（支持 `projectKey`/`env`/`keyword`） |
| GET | `/images/{id}` | `image:read` | 镜像详情 |
| GET | `/images/{id}/used-by` | `image:read` | 被哪些服务/环境使用（清理前校验） |
| DELETE | `/images/{id}` | `image:delete` | 清理镜像（被使用则拒绝） |
| POST | `/images/cleanup` | `image:delete` | 按保留策略批量清理（dry-run 支持） |

```json
// DELETE /images/5521  被使用时 → 409
{
  "code": 40900,
  "message": "该镜像正被使用，无法清理",
  "data": {
    "usedBy": [
      { "envKey": "test", "envName": "测试区", "serviceId": 31, "projectKey": "user-web" }
    ]
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

---

## 5. 环境

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| GET | `/environments` | `env:read` | 环境列表（含**集群资源水位**与资源用量；`status` 字段只由节点 CPU/内存决定，**不代表服务健康**）⚠️ 这里的 `status / cpuUsage / memUsage` 是**上次同步时写下的快照**，只在 `POST /sync` 时更新；界面展示"当前水位"必须改用下面那条 `/resources`（实时）。 |
| GET | `/environments/{key}` | `env:read` | 环境详情 |
| POST | `/environments` | `env:write` | 新建环境 |
| PUT | `/environments/{key}` | `env:write` | 编辑环境 |
| DELETE | `/environments/{key}` | `env:write` | 删除（有服务则拒绝） |
| GET | `/environments/{key}/project-candidates` | `env:read` | 可关联的项目（避免 N+1） |
| GET | `/environments/{key}/vars` | `env:read` | 环境变量（密文脱敏） |
| PUT | `/environments/{key}/vars` | `env:write` | 覆盖式保存环境变量 |
| GET | `/environments/{key}/services` | `deploy:read` | 环境内服务列表（含 `live` 集群实时状态） |
| GET | `/environments/{key}/resources` | `env:read` | 集群资源用量（CPU/内存/节点/版本，**实时读集群**）。`metricsAvailable=false` 时 `cpuUsage/memUsage` 恒为 0，含义是"没读到"而不是"用量为 0"，**不许回退到环境快照**。前端统一走 `useEnvResources` |
| POST | `/environments/{key}/sync` | `env:write` | 主动同步集群状态与 namespace 实际用量 |
| GET | `/environments/{key}/events` | `env:read` | 集群事件（Warning 类，辅助排障） |
| POST | `/environments/{key}/test-connection` | `env:write` | 连通性测试 |

> **服务列表里的 `live` 才是集群实况**（界面必须优先信它，不能拿平台库的 `replicas` 顶替）：
> `replicas` 是**期望**副本数（`spec.replicas`），`actualReplicas` 是集群里**实际存在**的实例数
> （`status.replicas`，含正在崩溃、还没被回收的那些），`readyReplicas` 是就绪数。
> 三者分开是有意的：滚动更新卡住时新 Pod 反复崩溃、旧 Pod 不被杀（1 副本下
> `maxUnavailable` 向下取整为 0），实际有 2 个而期望只有 1 个 —— 实测 demobasic
> 就是 `1 / 2 / 1`。界面按 `readyReplicas / max(replicas, actualReplicas)` 渲染，
> 否则会写成 `1/1`（还带个健康绿），把"有一个实例正在崩"整个抹掉。

**GET /environments**（响应节选）
```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "list": [
      {
        "id": 1,
        "key": "test",
        "name": "测试区",
        "cluster": "k8s-test-01",
        "clusterVersion": "v1.29.6",
        "namespace": "yunnian-test",
        "deployMethod": "k8s",
        "registry": "harbor.yunnian.internal",
        "ingressDomain": "*.test.yunnian.cn",
        "needApproval": false,
        "approvers": [],
        "owner": { "id": 3, "name": "王强" },
        "enabled": true,
        "description": "研发自测与联调环境，允许高频发布，不做审批",
        "health": { "status": "healthy", "cpuUsage": 42, "memUsage": 57, "nodeCount": 6 },
        "counts": { "projects": 10, "services": 10, "abnormalServices": 3 },
        "lastSyncAt": "2026-09-22T15:38:00+08:00"
      }
    ]
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

> ⚠️ 上面这段示例是**设计稿的写法，与当前实现已经不一致**：实现里 `owner` 是字符串、
> 没有 `health` 对象、`counts` 展开成了扁平的 `status / cpuUsage / memUsage / nodes`，
> 另有 `approverUsernames`（与 `approvers` 一一对应的登录名，专供编辑表单预填
> —— 只回展示名时表单没法预填，"打开编辑不重选"就等于把审批人清空）。
> 字段以 `backend/internal/service/dto.go` 的 `EnvironmentDTO` 为准。

**POST /environments/{key}/test-connection**
```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "apiServerReachable": true,
    "authGranted": true,
    "namespaceExists": true,
    "namespaceQuota": { "cpu": "20", "memory": "40Gi", "usedCpu": "8.4", "usedMemory": "22.8Gi" },
    "registryPullGranted": true,
    "latencyMs": 42,
    "warnings": ["Namespace 内存配额已使用 57%"]
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

---

## 6. 服务实例（环境内的 Deployment 操作）

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| GET | `/services/{id}` | `deploy:read` | 服务详情 |
| GET | `/services/{id}/pods` | `deploy:read` | Pod 列表与状态 |
| GET | `/services/{id}/versions` | `rollback:read` | 历史版本列表（含镜像、部署时间、部署人、状态） |
| GET | `/services/{id}/pods/{podName}/logs/stream` | `log:read` | **SSE Pod 实时日志** |
| PUT | `/services/{id}/params` | `deploy:*` | **设置参数**（副本/资源/环境变量/ConfigMap/Secret），写 deploy_param 版本 |
| POST | `/services/{id}/scale` | `deploy:*` | **扩缩容** |
| POST | `/services/{id}/restart` | `deploy:*` | **重启** Deployment（滚动重启） |
| POST | `/services/{id}/redeploy` | `deploy:*` | **重新部署**（当前镜像滚动更新） |
| DELETE | `/services/{id}` | `deploy:*` | **下线 / 删除** |
| GET | `/services/{id}/params/history` | `deploy:read` | 参数变更历史与 diff |

**GET /services/{id}/versions**

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "serviceId": 31,
    "projectKey": "user-web",
    "envKey": "test",
    "currentImageId": 5521,
    "list": [
      {
        "imageId": 5521,
        "image": "harbor.yunnian.internal/frontend/user-web:master-a91f3c2-202609221600",
        "tag": "master-a91f3c2-202609221600",
        "deployedAt": "2026-09-22T14:18:06+08:00",
        "operator": "张伟",
        "deployNo": "D2026092200031",
        "status": "success",
        "isCurrent": true,
        "note": "常规迭代发布"
      },
      {
        "imageId": 5480,
        "image": "harbor.yunnian.internal/frontend/user-web:master-7c2a91b-202609211430",
        "tag": "master-7c2a91b-202609211430",
        "deployedAt": "2026-09-21T14:32:10+08:00",
        "operator": "李静",
        "deployNo": "D2026092100018",
        "status": "success",
        "isCurrent": false,
        "isLastStable": true
      }
    ]
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

**PUT /services/{id}/params**（设置参数）

```json
// Request
{
  "replicas": 4,
  "cpuLimit": "2000m",
  "memLimit": "4Gi",
  "hpaEnabled": true,
  "envVars": [
    { "key": "SPRING_PROFILES_ACTIVE", "value": "test" },
    { "key": "DB_CONN_TIMEOUT", "value": "30s" }
  ],
  "configMaps": ["user-web-config", "common-config"],
  "secrets": ["user-web-secret", "registry-pull-secret"],
  "note": "压测前扩容并放宽 DB 连接超时"
}
```

```json
// Response
{
  "code": 0,
  "message": "参数已保存，将在下次部署/重启时生效",
  "data": {
    "paramVersion": 7,
    "diff": [
      { "field": "replicas", "before": "3", "after": "4" },
      { "field": "cpuLimit", "before": "1000m", "after": "2000m" },
      { "field": "envVars.DB_CONN_TIMEOUT", "before": null, "after": "30s" }
    ],
    "applyRequiresRestart": true
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

**POST /services/{id}/scale**

```json
// Request
{ "replicas": 5, "reason": "大促压测前扩容" }
```

```json
// Response
{
  "code": 0,
  "message": "已提交扩缩容",
  "data": { "replicas": 5, "previousReplicas": 3, "deployId": 9102, "status": "progressing" },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

**GET /services/{id}/pods/{podName}/logs/stream**（Query 参数）

```
?container=app&since=1h&tailLines=500&keyword=ERROR&follow=true
```

SSE 事件同 0.4 约定。

---

## 7. 部署

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| POST | `/deployments` | `deploy:{env}` | **创建部署单（= 部署）** |
| GET | `/deployments` | `deploy:read` | 部署记录列表 |
| GET | `/deployments/{id}` | `deploy:read` | 部署详情（步骤 + 日志） |
| GET | `/deployments/{id}/logs/stream` | `log:read` | **SSE 部署实时日志** |
| POST | `/deployments/{id}/approve` | `deploy:approve` | **审批通过** |
| POST | `/deployments/{id}/reject` | `deploy:approve` | 审批驳回 |
| POST | `/deployments/{id}/cancel` | `deploy:{env}` | 取消部署 |
| POST | `/deployments/{id}/retry` | `deploy:{env}` | 失败重试 |
| GET | `/deployments/{id}/ai-diagnosis` | `ai:query` | 部署异常 AI 诊断与回滚建议 |
| GET | `/deployments/pending-approval` | `deploy:approve` | 待我审批列表 |

**POST /deployments**（请求）

```json
{
  "envKey": "prod",
  "projectKey": "user-service",
  "imageId": 5521,
  "imageTag": null,
  "replicas": 4,
  "strategy": "RollingUpdate",
  "envOverrides": { "LOG_LEVEL": "INFO" },
  "remark": "修复订单超时问题，关联需求 REQ-2026-0912",
  "notify": true
}
```

> ⚠️ 上面是**设计稿的字段名**，一期实现与之有出入（以代码为准）：
> 镜像用 `imageRecordId`（字符串）或 `tag`，没有 `imageId` / `imageTag` / `envOverrides` / `notify`；
> 另加 `idempotencyKey`（连点两次只产生一个部署单）。见 `service/cd.go` 的 `CreateDeploymentInput`。

**发单前的拒绝理由**（`POST /deployments` 会在这里直接拦住，不建单）

| 拒绝理由 | 判定依据 |
| --- | --- |
| `40000 镜像 X:Y 不在仓库中，这次发布必然失败` | 仓库**明确回 404**（`ErrTagNotFound`）。只有 404 算"确定性失败"；超时 / 401 / 5xx / 非本平台管的 registry 一律**放行**并记 WARN |
| 已有进行中的部署单 | 同环境同服务的互斥锁 + 状态冲突 |
| 该环境未纳管此项目 / 没有服务实例 | `service_instance` 里没有对应行 |
| HPA 接管的工作负载 | 改副本数会被 HPA 拉回 |

```json
// Response 201 —— 受保护环境返回待审批
{
  "code": 0,
  "message": "已提交审批，等待 运维示例 / 林曦 确认",
  "data": {
    "id": 9103,
    "deployNo": "D2026092200048",
    "envKey": "prod",
    "envName": "正式区",
    "projectKey": "user-service",
    "image": "harbor.yunnian.internal/backend/user-service:master-b12c9f4-202609211805",
    "replicas": 4,
    "status": "pending_approval",
    "approvers": ["运维示例", "林曦"],
    "logStreamUrl": "/api/v1/deployments/9103/logs/stream",
    "createdAt": "2026-09-22T16:05:00+08:00"
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

```json
// 同一服务有部署进行中 → 409
{
  "code": 40901,
  "message": "该服务在测试区有部署进行中（D2026092200031），请等待完成或取消后再试",
  "data": { "conflictDeployId": 9080 },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

**GET /deployments/{id}/logs/stream**（SSE 报文节选）

```
event: log
id: 1
data: {"seq":1,"ts":"2026-09-22T16:06:02+08:00","level":"cmd","line":"$ kubectl -n yunnian-prod set image deployment/user-service app=harbor.../user-service:master-b12c9f4-202609211805"}

event: status
data: {"status":"progressing","progress":25,"stage":"拉取镜像"}

event: log
id: 2
data: {"seq":2,"ts":"2026-09-22T16:06:34+08:00","level":"info","line":"Readiness probe: GET /healthz:8080 成功（3/4）"}

event: status
data: {"status":"success","progress":100,"stage":"已完成","duration":112}

event: end
data: {"reason":"finished"}
```

> **失败时，这个流会自动追加一段「集群侧证据」**（`CDService.appendDeployEvidenceLogs`，2026-10-08 落地）。
> 理由是平台日志只会说"等待就绪探针"，说不清应用为什么起不来：部署失败后平台**只读**采集一次
> 集群侧事实并逐行写进本流 —— 容器状态（`CrashLoopBackOff` / `OOMKilled` / 退出码）、
> **Pod 事件**（`BackOff` / `Unhealthy` / `Pulled` / `Scheduled` …，与 `kubectl get events` 同级）、
> 以及**崩溃前那一轮**的容器日志尾部（最多 20 行；CrashLoop 时当前实例常一行都没输出，
> 真因在上一轮）。用户在失败那一屏就能排查，不必再开终端。
>
> 三条约束：**全程只读**（失败后任何写动作都是把事故扩大）；**取不到就写成一行 `warn`**
> （绝不静默 —— 静默会被读成"集群里没问题"，那是相反结论）；**独立 20s 预算 + 摘掉取消信号**
> （部署超时是最常见的失败原因之一，拿那个已经 done 的 ctx 去取证必然立刻失败）。
> 不截断的完整证据仍在 `/deployments/{id}/ai-diagnosis` 的依据里。

**GET /deployments/{id}/ai-diagnosis**（部署失败诊断 + 回滚建议）

> ⚠️ **这条接口给的是「结论」，不是「事实」**：它按需触发、可能走模型、还可能降级成错误码规则
> 的套话。而 `deploy_record.error_message`（2026-10-10 起下发到 `DeployDTO`）是失败那一刻记下的
> **原文**。界面上两者必须分开排布（事实在前、结论在后），排查顺序也应当是"先看事实，再信结论"。
> 字段口径见上文「失败原因的两个字段」——构建与部署同口径。

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "deployId": 9103,
    "errorCode": "PROBE_FAILED",
    "reason": "健康检查失败：Readiness probe GET /healthz 连续 3 次返回 503，新版本启动时数据库连接池初始化超时（默认 5s）。",
    "suggestion": "建议回滚至上一稳定版本 master-7c2a91b-202609211430，或将 DB_CONN_TIMEOUT 调整为 30s、initialDelaySeconds 调整为 20s 后重试。",
    "confidence": 0.91,
    "recommendedAction": {
      "type": "rollback",
      "targetImageId": 5480,
      "targetImage": "harbor.yunnian.internal/frontend/user-web:master-7c2a91b-202609211430",
      "label": "回滚至上一稳定版本"
    },
    "evidence": [
      { "seq": 88, "line": "Readiness probe failed: HTTP probe failed with statuscode: 503" },
      { "seq": 72, "line": "Caused by: java.sql.SQLTransientConnectionException: connect timed out" }
    ],
    "model": "yunnian-deploy-doctor-v1"
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

> ⚠️ 上面是**设计稿**。一期实现（`CDService.DeploymentAIDiagnosis`，`service/cd.go` + `service/cd_diagnosis.go`）
> 的实际响应字段如下，**以代码为准**：
>
> | 字段 | 说明 |
> | --- | --- |
> | `source` | 结论来源，只有两个取值：`model`（模型分析）/ `rule`（平台错误码规则）。**必须区分**——两者文案长得一样，不标出来用户会把查表结论当成"日志已经被看过一遍" |
> | `model` | 模型通道名，仅 `source=model` 时有值 |
> | `notice` | 附加说明，目前用于**"这次为什么没用上模型"**。降级时后端把真实失败原因（如 `dial tcp ...: connect: connection refused`）带在这里，前端必须展示 |
> | `deployNo` / `currentImage` / `reason` / `suggestion` / `confidence` | 与设计稿一致 |
> | `rollbackAvailable` / `rollbackImage` / `rollbackDeployNo` / `serviceId` | 取代设计稿的 `recommendedAction` 对象：只有 `rollbackAvailable=true` 时才给出目标镜像与对应部署单号 |
>
> 没有实现设计稿里的 `evidence`（逐行日志引用）与 `errorCode` 字段。
>
> **关键行为（与 CI 构建归因同一口径）**：
>
> 1. **模型优先、规则兜底，降级不报错。** 模型通道不可用（未配置 Key / 网络不通 / 超时 / 输出解析失败）
>    时接口仍返回 **200**，`source=rule` + `notice` 如实说明。
>    所以前端**不该**再出现"模型未配置密钥，请去模型设置填 Key 后重试"这类提示——
>    那个动作既不会让报错消失，也不影响结论正确性。
> 2. **结论来源如实标注**：`source` 由后端判定并下发，前端只负责分叉措辞，不得自行推断。
> 3. 部署失败后平台**不做自动回滚**（2026-10-08 起），`rollbackAvailable` 只是"能回滚到哪一版"的查询结果，
>    不代表平台已经回滚过。
>
> **证据来源**（模型看到的东西，`service/cd_diagnosis.go` 的 `deployEvidence`）：
>
> | 块 | 来源 | 缺失时的表现 |
> | --- | --- | --- |
> | 部署上下文 | `deploy_record` + `service_instance` + `environment` | 必有 |
> | 平台部署日志尾部 | 日志流快照（易失，平台重启后取不回） | 写明"（无。日志流是易失的…）" |
> | 集群侧证据 | `Cluster.DiagnoseFailure`：容器状态 / 退出码 / **`previous=true` 的崩溃前日志** / Pod 事件 | **取证失败必须报错或进 `Notes`，不能返回空结构体**——下游会把"我们没看到"读成"应用没问题" |
>
> 失败原因**不落库**（`deploy_record` 无 `reason` 列）；只有 `source=model` 的结论会写回
> `deploy_record.ai_diagnosis` 当缓存（规则结论是纯函数，重算零成本）。

---

## 8. 回滚

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| POST | `/rollbacks` | `rollback:execute` | **执行回滚** |
| GET | `/rollbacks` | `rollback:read` | 回滚记录列表 |
| GET | `/rollbacks/{id}` | `rollback:read` | 回滚详情 |
| GET | `/rollbacks/{id}/logs/stream` | `log:read` | SSE 回滚日志 |

**POST /rollbacks**（请求）

```json
{
  "envKey": "test",
  "projectKey": "user-web",
  "targetImageId": 5480,
  "reason": "新版本下单接口 P99 耗时从 180ms 升至 1.2s，人工介入回滚",
  "triggerType": "manual",
  "aiDiagnosisId": null,
  "notify": true
}
```

> `triggerType` 客户端只能传 `manual` / `ai_suggested`。第三个取值 **`auto` 是服务端专用的**：
> 只有平台自己发起的回滚才会写 `trigger_type=auto`（docs/02 的枚举即 `manual/ai_suggested/auto`）。
> 前端据此把触发方式显示成「自动」而不是「人工」——把自动回滚说成人工处理，
> 等于谎称"有人已经看过现场了"。
>
> ⚠️ **当前没有任何代码会写 `auto`**：部署失败后的自动回滚已按用户要求移除（2026-10-08，
> 见 `backend/README.md`「部署失败后不回滚」）。该取值与前端标注**保留**，理由有二：
> 一是库里已有一条真实产生过的 `auto` 记录，删掉渲染会把历史数据谎报成「人工」；
> 二是"服务端专用"这层语义本身仍然成立 —— 客户端**依旧不许**自封 `auto`。

```json
// Response 201
{
  "code": 0,
  "message": "回滚任务已提交",
  "data": {
    "id": 402,
    "rollbackId": 402,
    "deployNo": "D2026092200049",
    "envKey": "test",
    "projectKey": "user-web",
    "fromImage": "harbor.yunnian.internal/frontend/user-web:master-a91f3c2-202609221600",
    "toImage": "harbor.yunnian.internal/frontend/user-web:master-7c2a91b-202609211430",
    "status": "progressing",
    "logStreamUrl": "/api/v1/rollbacks/402/logs/stream",
    "createdAt": "2026-09-22T16:08:00+08:00"
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

---

## 9. 审计日志

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| GET | `/audit-logs` | `audit:read` | 检索（模块/用户/动作/结果/时间区间） |
| GET | `/audit-logs/{id}` | `audit:read` | 详情（含 diff） |
| POST | `/audit-logs/export` | `audit:read` | 异步导出 CSV/Excel，返回任务号 |
| GET | `/audit-logs/export/{taskId}` | `audit:read` | 查询导出进度与下载地址 |

```json
// GET /audit-logs?module=CD&action=执行部署&result=success&startTime=2026-09-01T00:00:00%2B08:00&endTime=2026-09-22T23:59:59%2B08:00
{
  "code": 0,
  "message": "ok",
  "data": {
    "list": [
      {
        "id": 12045,
        "createdAt": "2026-09-22T16:05:12+08:00",
        "user": { "id": 15, "name": "运维示例", "username": "ops", "role": "ops" },
        "module": "CD",
        "action": "执行部署",
        "target": { "type": "服务", "id": 44, "name": "user-service", "envKey": "prod" },
        "detail": "部署至正式区，副本数 3 → 4，需审批",
        "diff": [{ "field": "replicas", "before": "3", "after": "4" }],
        "result": "success",
        "ip": "10.8.12.37",
        "traceId": "8f2c1a9b4e7d4c1e",
        "durationMs": 112300
      }
    ],
    "total": 128,
    "page": 1,
    "pageSize": 20
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

---

## 10. 通知渠道

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| GET | `/notify-channels` | `notify:manage` | 渠道列表（含最近投递状态） |
| POST | `/notify-channels` | `notify:manage` | 新建渠道 |
| PUT | `/notify-channels/{id}` | `notify:manage` | 编辑 |
| DELETE | `/notify-channels/{id}` | `notify:manage` | 删除 |
| POST | `/notify-channels/{id}/test` | `notify:manage` | **真实投递**一条测试消息 |
| PATCH | `/notify-channels/{id}/enabled` | `notify:manage` | 启停 |

```json
// POST /notify-channels（请求）
{
  "name": "CI/CD 告警群（钉钉）",
  "type": "dingtalk",
  "target": "https://oapi.dingtalk.com/robot/send?access_token=***&secret=SEC***",
  "events": ["build_failed", "build_interrupted", "deploy_failed", "deploy_interrupted", "deploy_pending_approval"],
  "enabled": true
}

// 响应
{
  "code": 0, "message": "ok",
  "data": {
    "id": "1",
    "type": "dingtalk",
    "name": "CI/CD 告警群（钉钉）",
    "target": "https://oapi.dingtalk.com/robot/send?access_token=***&secret=SEC***",
    "enabled": true,
    "events": ["build_failed", "build_interrupted", "deploy_failed", "deploy_interrupted", "deploy_pending_approval"],
    "lastSentAt": "2026-10-10T14:38:17+08:00",
    "failCount": 0,
    "lastError": "",
    "updatedAt": "2026-10-10T14:38:17+08:00"
  }
}
```

**两个枚举里都只放「真能走通的组合」**：

- `type`：**`email` / `dingtalk`**。一期只实现了这两个发送器（`service/notify.go`）。
  企业微信 / 飞书 / 自定义 Webhook 曾经也在枚举里，但没有任何发送实现 ——
  配得出来却永远发不出去，所以从入参枚举里去掉。入库的历史记录仍会原样返回其 `type`，
  但投递会明确失败并把原因写进 `lastError`，**不会静默成功**。
- `events`：**`build_failed` / `build_interrupted` / `deploy_failed` / `deploy_interrupted` /
  `deploy_pending_approval`**。这是平台当前**真的会触发**的全部事件。传入其它码返回 `40000`
  并列出可选值 —— 以前这里放行任意字符串，于是界面上 8 个事件码里有 5 个永远不会推送，
  用户勾上了却永远收不到。

**「被中断」为什么与被「失败」分成两个码**（`build_interrupted` / `deploy_interrupted`，2026-10-10 补）：

| | `*_failed` | `*_interrupted` |
| --- | --- | --- |
| 成因 | 构建 / 部署**本身**出错（依赖拉不下来、滚动更新没就绪…） | **平台重启**，执行随进程中消失 |
| 该做什么 | 看真因，改代码或配置 | 原样**重新触发** —— 没有人做错任何事 |
| 库里状态 | `failed` | `interrupted`（独立一档，不并进 `failed` / `canceled`） |

两者是**可分开订阅**的：只关心"发布本身出问题"的群不该在凌晨被一条"平台自己重启了"叫醒；
反过来，把它们并进 `*_failed` 会把"平台重启"说成"这次构建哪里写错了"，引着人去翻代码。
这一点与状态机口径一致（见 `backend/internal/service/ci.go` 的 `ReconcileInterrupted`）。

触发点在**启动补偿**：平台启动时把重启那一刻还没结束的记录标为 `interrupted`，
并投递一条**汇总**消息（同一次重启中断的多条合并成一条，避免刷屏 —— 队列容量是并发数 ×20）。
投递在**改库之后**：反过来的话，通知已发出、改库却失败，就成了"群里说中断了、界面上还在转圈"。

> ⚠️ 这两个事件**只覆盖平台自身的重启**。因为 `runner.mode=inline`（任务跑在平台进程内），
> 平台一重启任务就必然断。`runner.mode=agent` 已于 2026-10-10 决定**不做**（单体单机），
> 所以这条判据在可预见期内不需要重新论证。

**`lastSentAt` / `failCount` / `lastError` 是"配了渠道却收不到消息"唯一的自查入口**：

| 字段 | 含义 |
| --- | --- |
| `lastSentAt` | 最近一次**成功**投递的时间。为空 = 还没有过一次成功（失败不会写进这里，否则会盖掉"上次成功是什么时候"这个有用信息） |
| `failCount` | **连续**失败次数，成功一次即清零。它回答的是"现在正常吗"，不是"历史上错过几次" |
| `lastError` | 最近一次失败的原因，**对端的原话**（如钉钉的 `errcode=310000 机器人发送签名不匹配`） |

**POST /notify-channels/{id}/test** 会**真的把消息投出去**，并同步返回结果：

```json
{ "code": 0, "message": "ok",
  "data": { "ok": true, "message": "已通过钉钉发出测试消息，请到该机器人所在的群确认收到" } }
```

`ok=false` 是**正常结果而不是接口错误**（对端拒收、签名不对、SMTP 认证失败都会走到它），
失败真因写在 `message` 里。测试结果同样会写回 `lastSentAt` / `failCount` / `lastError`。

**钉钉加签**：机器人安全设置选了「加签」时，光有 `access_token` 会被拒成 `310000`。
平台的约定是把加签密钥作为 `secret` 参数追在地址后面：

```
https://oapi.dingtalk.com/robot/send?access_token=xxx&secret=SECxxxxxxxx
```

发送前平台取出 `secret`、按官方算法生成 `timestamp` + `sign`，并把 `secret` 从请求 URL 里剔除
（钉钉不认这个参数，留着只会让它出现在访问日志里）。

**发信身份在平台配置（`notify.email`）里，不在渠道上** —— 邮件渠道的 `target` 只填收件人。

> **投递是旁路**：通知由构建/部署的收尾路径异步触发，**发不出去不会让业务操作失败或变慢**。
> 投递在进程内进行，平台重启时正在重试的通知会丢 —— 一期接受这个代价。
> 三个触发点：构建失败（`finishBuild`）、部署失败（`failDeploy`）、
> 建单/回滚落待审批（`CreateDeployment` / `ExecuteRollback`）。
> ⚠️ **待审批是在"建单这一刻"通知的**，不是等审批人打开平台才告诉他。

---

## 11. 凭证管理

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| GET | `/credentials` | `user:manage` | 列表（值脱敏） |
| POST | `/credentials` | `user:manage` | 新增（写入即加密） |
| PUT | `/credentials/{id}` | `user:manage` | 轮换（值留空表示不变） |
| DELETE | `/credentials/{id}` | `user:manage` | 删除 |
| POST | `/credentials/{id}/verify` | `user:manage` | 校验有效性（调上游 API 验证） |

---

## 12. AI 能力（只读）

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| POST | `/ai/chat` | `ai:query` | **自然语言查询（SSE 流式回答）** |
| GET | `/ai/conversations` | `ai:query` | 会话历史 |
| GET | `/ai/conversations/{id}` | `ai:query` | 会话详情 |
| POST | `/ai/analyze-build/{buildId}` | `ai:query` | 构建日志归因（同 /builds/{id}/ai-diagnosis） |
| POST | `/ai/analyze-deploy/{deployId}` | `ai:query` | 部署异常诊断（同 /deployments/{id}/ai-diagnosis） |

**POST /ai/chat**

```json
// Request
{
  "conversationId": "conv_8f2c1a9b",
  "message": "帮我看看测试区192 demouser 服务的状态",
  "context": { "envKey": "test", "projectKey": "demouser" }
}
```

> **`context` 只是省事的捷径**（用户在环境详情页提问时不必再打一遍环境名），
> **不是模型能查什么的前提**。模型手上有一组只读工具，会自己把口语名对到标识再查
> —— 详见下面「模型自己取数」。

SSE 报文：

```
event: tool
data: {"name":"list_environments","label":"读取环境名录","args":"{}","result":"{\"ok\":true,\"environments\":[…]} "}

event: tool
data: {"name":"get_service_status","label":"查询集群实时状态","args":"{\"envKey\":\"test\",\"projectKey\":\"demouser\"}","result":"{\"ok\":true,\"replicas\":1,…}"}

event: token
data: {"delta":"根据查询结果，"}

event: attachment
data: {"kind":"kv","title":"测试区 · user-web 部署详情","data":{"部署单号":"D2026092200031","镜像版本":"harbor.../user-web:master-a91f3c2-202609221600","副本":"3 / 3 Ready","部署人":"张伟","耗时":"1m12s"}}

event: attachment
data: {"kind":"action","title":"可执行操作","data":[{"label":"查看实时日志","action":"logs","target":"test/user-web"},{"label":"重新部署","action":"redeploy","target":"test/user-web"}]}

event: end
data: {"reason":"finished","conversationId":"conv_8f2c1a9b","tokensUsed":412}
```

事件共五类：`tool`（工具调用留痕）/ `token`（正文增量）/ `attachment`（结构化卡片）/ `error` / `end`。

> **`tool` 事件为什么必须有**：查集群的工具要几百毫秒到几秒，这期间**一个 token 都没有**，
> 前端若不显示这一步就是一片空白，看着像卡死。`label` 由后端给（"查询集群实时状态"），
> 前端不按函数名再映射一份——两处映射必然漂移。

**模型自己取数（2026-10-09 起）**

`/ai/chat` 背后挂了 8 个**只读**工具，模型会在回答前自行调用：

| 工具 | 用途 |
| --- | --- |
| `list_environments` / `list_projects` | 把用户口语（"测试区192"）对到 `env_key` / `project_key` |
| `list_env_services` | 某环境的服务清单（平台登记值，可能滞后） |
| `get_service_status` | **集群实时**状态：镜像 tag、期望/实际/就绪副本 |
| `diagnose_service` | **集群侧故障取证**：Pod 容器状态、事件、崩溃前那轮日志 |
| `get_cluster_resources` | 集群实时资源（含 `metricsAvailable` 语义） |
| `list_deployments` / `list_builds` | 部署与构建记录 |

> **安全约束（两条一起才成立）**：
> ① **结构性只读**——工具注册表里根本没有 apply / scale / restart / rollback 这些函数，
> 模型想说"我帮你重启了"也没有东西可调；写操作一律退化为 `attachment.kind=action` 卡片，
> 由用户点击后走**正常 REST 接口**（含权限与审计）。
> ② **不把集群凭证交给模型**——工具在**平台进程内**执行，用的是平台自己的 K8s 客户端
> （环境自带 kubeconfig / 集群接入凭据）。模型只看到返回值。
>
> ⚠️ **已知边界（待定）**：工具没有按环境做权限收敛 —— 有 `ai:query` 的用户可以通过
> 工具读到任意环境（含 prod）的集群实时状态。这一点在加工具之前就存在（旧的快照逻辑
> 同样不区分环境），但工具把可读内容从"平台登记值"扩到了"集群实时值"。是否要按环境门控
> 待评审。

---

## 13. 系统与健康

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| GET | `/system/stats` | 登录态 | 概览页统计（项目数/今日构建/成功率/环境数/待处理） |
| GET | `/system/integrations` | 登录态 | 第三方集成状态 |
| GET | `/system/integrations/gitlab` | `user:manage` | GitLab 接入配置（令牌只回掩码） |
| PUT | `/system/integrations/gitlab` | `user:manage` | 保存 GitLab 接入配置（地址/开关/超时/令牌） |
| POST | `/system/integrations/gitlab/test` | `user:manage` | GitLab 连通性测试（可测未保存的值） |
| GET | `/system/integrations/jenkins` | `user:manage` | Jenkins 接入配置（口令只回掩码） |
| PUT | `/system/integrations/jenkins` | `user:manage` | 保存 Jenkins 接入配置（地址/开关/账号/口令/超时） |
| POST | `/system/integrations/jenkins/test` | `user:manage` | Jenkins 连通性测试（可测未保存的值） |
| GET | `/system/integrations/jenkins/jobs` | `build:read` | 拉取 Jenkins Job 列表（含文件夹内，标出是否已绑定项目） |
| GET | `/system/integrations/jenkins/params` | `build:read` | 读取某个 Job 的参数定义（不含可选值） |
| GET | `/system/integrations/jenkins/param-values` | `build:read` | 读取 GitParameter 的可选值（版本标签 / 分支） |
| POST | `/system/integrations/jenkins/trigger` | `build:trigger` | **触发一次 Jenkins 构建**（仅限已绑定项目的 Job） |
| GET | `/system/config` | `user:manage` | 系统配置 |
| PUT | `/system/config` | `user:manage` | 更新系统配置 |
| GET | `/healthz` | 公开 | 存活探针 |
| GET | `/readyz` | 公开 | 依赖就绪（DB/Redis/Harbor/K8s） |
| GET | `/metrics` | 内网 | Prometheus 指标 |

**GET /system/stats**（概览页）

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "projects": { "total": 20, "frontend": 10, "backend": 10, "enabled": 19 },
    "builds": { "todayTotal": 37, "todaySuccess": 33, "todayFailed": 3, "running": 2, "successRate": 92.4, "avgDurationSec": 96 },
    "environments": { "total": 5, "healthy": 3, "warning": 1, "error": 1, "services": 29 },
    "todos": { "pendingApproval": 1, "failedBuilds24h": 6 },
    "trend": [{ "date": "09/09", "total": 31, "failed": 4, "success": 27 }]
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

> **`abnormalServices` 已退役**（2026-10-09）。它原本是 `service_instance.status` 的**库内计数**
> ——那是纳管时写入的「平台期望值」，不是集群实况，实测会把集群里 running 的服务报成异常。
> 「有几个服务需要关注」只能由集群实时值判定，前端走 `utils/live.ts` 的 `isServiceAbnormal`。
> 字段已从 DTO 与 handler 移除（不是留着不读），避免后来者以为它还能用。

### 13.1 GitLab 接入配置

> **为什么只有令牌、没有账号密码**：GitLab REST API v4 的认证方式只有 Personal Access Token
> （`PRIVATE-TOKEN` 头）/ OAuth Token / Job Token 三类，**没有账号密码认证**。早年唯一沾边的
> 「Resource Owner Password Credentials」密码模式已废弃：令牌只有 2 小时且**账号一旦开启 2FA 就直接失效**。
> 所以界面只收「地址 + 访问令牌」，不再提供账号密码输入框——提供一个注定不可用的入口比不提供更糟。
>
> **令牌不落明文**：地址/开关/超时写 `system_config`，访问令牌单独写 `secret_credential`
> 的 AES-256-GCM 加密列。所以库导出、只读副本、脱敏失败都不会泄露令牌。

**GET /system/integrations/gitlab** — 读取当前配置

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "enabled": true,
    "baseUrl": "http://47.100.239.15:22691",
    "timeout": 10,
    "hasToken": true,
    "tokenMasked": "glp****************3456",
    "source": "db",
    "identity": "xufeng",
    "tokenExpiresAt": "2026-12-31",
    "updatedAt": "2026-09-28T10:41:07+08:00",
    "impl": "gitlab",
    "tokenPageUrl": "http://47.100.239.15:22691/-/user_settings/personal_access_tokens"
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

| 字段 | 说明 |
| --- | --- |
| `hasToken` / `tokenMasked` | 令牌**只回掩码**（`glp****************3456`），管理员也拿不到明文；要换令牌就重填一次 |
| `source` | `db` = 界面保存过；`config` = 尚未保存，用的 `configs/config.yaml` 值。**必须显式暴露**，否则运维会对着配置文件改半天、其实值早被界面覆盖 |
| `impl` | 当前真正干活的实现（`gitlab` / `local`）。与 `enabled` 是两回事：开关打开但探活不通时它仍是 `gitlab`，故障由「集成状态」卡片体现 |
| `identity` / `tokenExpiresAt` | 上次连通性测试识别到的令牌账号与到期日（没测过则为空） |
| `tokenPageUrl` | 由地址派生的「新建访问令牌」页面，省得用户自己翻设置菜单 |

**PUT /system/integrations/gitlab** — 保存配置

```json
// Request —— 令牌留空 = 不修改；清空令牌要用 clearToken
{
  "enabled": true,
  "baseUrl": "http://47.100.239.15:22691",
  "timeout": 10,
  "token": "glpat-xxxxxxxxxxxxxxxxxxxx",
  "clearToken": false
}
```

- `token` 留空表示**保持原令牌不变**。前端只看得到掩码、回传不了明文，若把空串当成「清空」，
  每次改地址都会顺手把令牌删掉。
- `clearToken: true` 是**显式的删除意图**，必须与「留空」分开。两者同时提交会返回 `400`。
- `enabled: true` 但 `baseUrl` 为空 → `400`（提示先填地址），不允许开一个连不上的开关。
- 地址只接受 `http://` / `https://`；`git@host:group/repo.git` 与 `ssh://` 会被拒并给出明确提示
  （这里要的是站点根地址，不是仓库地址）。
- 保存成功**即时生效、无需重启**：运行期通过配置桥接重新解析，`impl` 当场切换。
- 审计日志记录「改了哪些字段」，**不记录令牌值**。

**POST /system/integrations/gitlab/test** — 连通性测试

```json
// Request —— 三个字段都可留空，留空则测已保存的配置
{ "baseUrl": "http://47.100.239.15:22691", "token": "glpat-xxx", "timeout": 10 }
```

```json
// Response —— 失败时 ok=false，但仍是 200
{
  "code": 0,
  "message": "ok",
  "data": {
    "ok": false,
    "message": "GitLab 拒绝了访问令牌（401 Unauthorized）",
    "latency": 231,
    "usedSavedValues": false,
    "warnings": []
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

- **`ok=false` 是 200 响应，不是错误码**：这是「测试」接口的**业务结论**，不是接口调用失败。
  客户端要把 `ok` 和 HTTP 状态分开判断，否则会把「连不上」误报成「后端挂了」。
- 允许带上**还没保存的值**，让「先测再存」走得通；此时 `usedSavedValues=false`。
  也**只有**测的是已保存配置时，才会把识别到的 `identity` / `tokenExpiresAt` 写回库。
- `warnings` 是「现在能连上、但迟早出事」的提醒：令牌**作用域不足**（缺 `api`，构建/读仓库会失败）、
  **即将到期**。能连不等于够用。

### 13.2 Jenkins 接入与 Job 触发

> **定位**：把"已经在用的 Jenkins"接进来复用，而不是把它的 job 重新建模成平台项目。
> 绑定关系是 `project.build_method=jenkins` + `project.jenkins_job`（存 Job 的 **fullName**，即文件夹内要带路径）。
>
> **凭据形态**：Jenkins 的 HTTP Basic 是「用户名 + 口令」，口令可以是密码，也可以是 API Token
> （`用户设置 › API Token`）。两者都支持，平台不区分——`hasToken` 只表示"有口令"。
> 地址/账号/超时写 `system_config`，口令单独写 `secret_credential`（AES-256-GCM）。
>
> **地址要填服务地址，不是 Job 链接**：`http://host:8080/jenkins` 这样（含 context path）。
> 传 `/job/xxx/` 会被拒（40000），因为拿它去拼 API 路径必然拼错。
> Jenkins 自报的 `rootUrl` 常是内网 IP，响应里所有 URL 都会**改写成配置的地址**，否则点开是打不开的。

**GET /system/integrations/jenkins/jobs** — Job 列表

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "source": "real",
    "total": 531,
    "truncated": false,
    "skippedFolders": 0,
    "elapsedMs": 1820,
    "jobs": [
      {
        "fullName": "镜像打包-demoitms",
        "name": "镜像打包-demoitms",
        "url": "http://es-repo.example.com:1143/jenkins/job/%E9%95%9C%E5%83%8F%E6%89%93%E5%8C%85-demoitms/",
        "color": "blue",
        "parameterized": true,
        "paramNames": ["git_branch"],
        "nextBuildNumber": 76,
        "lastBuild": { "number": 75, "result": "SUCCESS", "building": false },
        "boundProjectKey": "demoitms"
      }
    ]
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

- `boundProjectKey` 只代表**平台侧**有一条 `project.jenkins_job` 指向它；Jenkins 并不知道平台的存在。
- `skippedFolders > 0` 表示有文件夹因权限读不到，界面要提示，不能装作列表是完整的。

**GET /system/integrations/jenkins/param-values?fullName=&param=** — 版本标签 / 分支

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "param": "git_branch",
    "tags": ["v_aiops_1.16.0109", "v_kewei_2.5.0308"],
    "branches": ["origin/Release_aiops_1.63", "origin/master"],
    "other": [],
    "total": 311,
    "errors": [],
    "elapsedMs": 932,
    "source": "real"
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

- **查询串必须是 `param=<参数名>`**。写成 `name=` 时 Jenkins 返回 **HTTP 200 + `values: []`**——
  不报错、不警告，就是空的；参数名根本不存在时表现完全相同。所以后端会先读一次参数定义：
  参数名对不上直接回 **40400**，于是"空数组"只剩一种含义——仓库里确实没有匹配的引用。
- 这个接口**每次都会去读一遍仓库**（实测 0.6~1.1s，无明显缓存）。只在真的要用参数时调，
  不要为了预热列表而批量调——531 个 Job 全量拉会把这台 Jenkins 拖慢。
- `tags` / `branches` / `other` 分开返回是刻意的：混成一个 311 条的列表，找版本号的人要在里面翻分支。

**POST /system/integrations/jenkins/trigger** — 触发构建

```json
// Request —— parameters 只传用户"明确选择过"的参数
{
  "fullName": "镜像打包-demoitms",
  "parameters": [{ "name": "git_branch", "value": "origin/Release_aiops_1.63" }],
  "idempotencyKey": "ui-demoitms-1759044000000"
}
```

```json
// Response —— 只有队列项，没有构建号
{
  "code": 0,
  "message": "ok",
  "data": {
    "queueId": 8821,
    "fullName": "镜像打包-demoitms",
    "buildId": 12,
    "message": "已提交 Jenkins，正在等待执行器",
    "parameters": [{ "name": "git_branch", "value": "origin/Release_aiops_1.63" }]
  },
  "traceId": "8f2c1a9b4e7d4c1e"
}
```

- **返回的是队列项不是构建号**：Jenkins 接受请求时只给 `queue/item/{id}`，真正的构建号要等执行器
  空闲才出现。平台的后台任务把它补齐后写进 `build_record.jenkins_build_no`，前端不用关心这一步。
- **只受理已绑定平台项目的 Job**（未绑定 → 40901）。平台要靠项目登记这次构建属于谁、
  镜像登记到哪、日志挂在哪；没有项目就只能建一条无主记录，不如当场说清楚。
- **幂等**：带同一个 `idempotencyKey` 重复提交返回已有记录，不会排出第二次构建。
- **不传的参数 = 不覆盖**。GitParameter 的真实默认值插件不通过 API 暴露（`defaultValue` 恒为 null），
  平台猜一个值传过去就可能构建错版本，所以留空即"不传该参数"，由 Jenkins 用它自己配置的默认值——
  与在 Jenkins 页面上直接点 Build 完全一致。
- 触发失败（Job 已被改名/删除 → **40400**，不是 502）时**不会**留下构建记录；
  若排队成功但建单失败，平台会撤销那个队列项，做到"要么都成、要么都没发生"。
- 构建记录里 `commit_id` **刻意留空**：检出哪个提交由 Jenkins 侧 Job 配置决定，
  平台按 GitLab 查一个分支 tip 填进去会得到一个"看起来对、其实不对"的提交号。

#### 13.2.1 产物镜像版本：只能从日志里读

Jenkins 不提供任何接口能问出"这次构建产出了哪个镜像"，**日志是唯一的事实来源**。
平台在构建过程中逐行解析，构建成功时把读到的地址登记成一条 `image_record`。
日志里有**两种**可用的证据形态，都要认：

```
① 命令行回显（Job 里开了 set -x 才有）
+ docker push es-repo.example.com:5000/escloud/demowarning:1.2.0.1058

② 整行就是一个完整镜像引用（Job 主动把产物地址打印出来）
es-repo.example.com:5000/escloud/demowarning:1.2.0.1058
```

> ⚠️ **只认 ① 会漏掉一半的 Job**。实测踩到：某 Job 没开 `set -x`，日志里只有
> `docker push` 的**输出行**和一行自己 echo 的完整地址，平台两个都读不到 ——
> 界面上留下「成功 + 镜像 —」，用户直到要发版、发现一个版本都选不到才发现。
> 形态 ② 不依赖任何 shell 选项，是使用方更该采用的做法。

解析口径（每一条都对应一个会把错误版本当成正确版本的情形）：

| 情形 | 处理 |
| --- | --- |
| 形态 ①：`set -x` 回显命令（行首有 `+ `） | 不按行首匹配，按 token 找 `docker push` |
| 形态 ②：整行是一个完整镜像引用 | 行内不得有空白、不得含 `://`、第一个 `/` 之前须含 `.` 或 `:`（据此排除时间戳 / URL / 绝对路径 / 相对路径），且必须带合法 tag |
| 变量没展开（`docker push $REG/$IMG:$TAG`） | 不算——那是脚本原文，不是结果 |
| 不带 tag（`docker push host:5000/repo`） | 不算——推的是 `latest`，不是版本号 |
| 冒号是端口（`host:5000/repo`） | 不算——用"冒号在最后一个斜杠之后"区分端口与 tag |
| 按 digest 推送（`repo@sha256:…`） | 不算——没有 tag |
| Job 中途推了基础镜像 / 缓存镜像 | 优先取与**项目仓库同名**的那个；同名推多次取最后一次 |
| 项目仓库名匹配不上任何一条 | 退回最后一个候选（不因为项目配置有问题就什么都不登记） |
| 一个候选都读不到 | **留空，不猜** —— 也**不判失败**，见下 |
| 构建最终**失败 / 取消** | 不登记——日志里出现 push 只说明执行过这条命令 |

> ⚠️ **读不到产物时不判失败**（2026-10-13 试过一版"读不到就把构建改判 failed"，当天回退）。
> 理由：Jenkins Job 是使用方自己写的，**本来就存在只做编译、不推镜像的 Job**；
> 一刀切判失败会把它们全部染红，而那种"失败"没有任何可修的东西 —— 平台只是没读到
> 它本来就没产出的东西。构建状态以 Jenkins 的 result 为准，读不到就如实不登记，界面显示"—"。
>
> **代价要知情**：这时界面上会出现「成功 + 镜像 —」，而它在 CD 里选不到版本。
> 要避免它，靠的是让 Job 把产物地址打出来（见上面两种形态），**不是靠平台去猜一个结论**。

登记时 `digest`、镜像大小、架构日志里读不到，但**能从镜像仓库读到** ——
所以登记完会立刻去仓库问一次（见 13.2.2），问不到才留空。前端对应显示"—"。

**历史构建的补登记**：解析发生在构建收尾那一刻，进程重启/崩溃就会错过。因此平台启动时跑两步补偿：

1. `BackfillJenkinsImages`：把 `trigger_type=jenkins AND status=success AND jenkins_job<>'' AND image_record_id IS NULL`
   的构建捞出来，读**已落库的日志**（纯本地，不再连 Jenkins）补登记，幂等；
2. `EnrichMissingImageMeta`：给 `digest` 还空着的镜像记录补仓库元数据 —— 上一步刚补出来的记录
   顺手在这一步一起补上。这一步要连镜像仓库，所以只对 `registry` 与配置里那一个匹配的行生效。

#### 13.2.2 从镜像仓库回填 digest / 大小 / 架构

日志里没有 digest，所以平台在登记完 `image_record` 之后，拿 `repo:tag` 去**镜像仓库**问一次。

**走的是 `/v2/`（OCI Distribution 协议），不是 Harbor 的 `/api/v2.0` 管理接口。**
实测某内网 Harbor 只在 `:5000` 暴露 registry 组件，`/api/v2.0/systeminfo` 在 `:5000` / `:443` / `:80`
上**全是 404** —— 按 Core API 写的客户端会原样 404，表现为"接上了但什么都读不到"。
`/v2/` 这套是 OCI 规范，换仓库（原生 Registry / Nexus / 别家的 Harbor）都不用改代码。

调用顺序（public 项目匿名即可，不需要账号）：

| 步骤 | 请求 | 拿到什么 |
| --- | --- | --- |
| 1 | `GET {host}/v2/` | 401 + `Www-Authenticate: Bearer realm="…",service="harbor-registry"` |
| 2 | `GET {realm}?service=harbor-registry&scope=repository:{repo}:pull` | bearer token |
| 3 | `GET /v2/{repo}/manifests/{tag}` | 响应头 `Docker-Content-Digest`；body 里 `layers[].size` 求和 = 镜像大小 |
| 4 | `GET /v2/{repo}/blobs/{config.digest}` | config blob 里的 `os` / `architecture`（单架构时才需要） |

三条边界，都是"别编数据"：

- **只问配置里那一个仓库**：镜像记录里的 `registry` 来自构建日志，未必是配置的这个仓库。
  不同仓库里同名 repo 的内容可能完全不同，问错了会把别人的 digest 挂到这次构建上 ——
  这种错不报任何错，数据直接变假。所以比对前先按 `host[:port]` 归一，不是同一个一律不碰。
- **多架构镜像**：大小按各平台之和（与 Harbor 界面口径一致），`arch` 写成 `linux/amd64,linux/arm64`；
  清单列表本身不算镜像层，必须逐个拉子清单。
- **推送时间仍然留空**：只有 Harbor Core 的 artifacts 接口给得出，registry v2 拿不到。
  同理 `tags/list` **不保证顺序**（实测返回的是乱序），所以平台不提供"取最新 tag"这种能力 ——
  做不到的就不做，免得给出一个看着像时间排序、实际是随机的"最新"。

查不到的两种结果要分清：

- `Resolve` 返回错误（仓库不通 / tag 不存在）→ 记 warn 日志，digest 留空，**不影响构建终态**；
- 日志里解析出的 tag 在仓库里 404 → 说明那次推送其实没成功（Jenkins 报 SUCCESS 不等于 push 成功），
  这是**有价值的交叉验证**，不是平台故障。

---

## 14. 接口总览（按模块计数）

| 模块 | 接口数 | 备注 |
| --- | --- | --- |
| 认证与用户 | 12 | 含 SSO |
| 项目 | 11 | |
| 构建 | 10 | 2 个 SSE、2 个 Webhook |
| 镜像 | 5 | |
| 环境 | 11 | |
| 服务实例 | 10 | 1 个 SSE |
| 部署 | 10 | 1 个 SSE、含审批 |
| 回滚 | 4 | 1 个 SSE |
| 审计 | 4 | |
| 通知 | 6 | |
| 凭证 | 5 | |
| AI | 5 | 1 个 SSE |
| 系统 | 18 | 含 3 个 GitLab + 7 个 Jenkins 接入配置/Job/触发接口 |
| **合计** | **111** | 一期实现其中 **约 73 个**（标 ⭐ 的为核心路径） |

**一期核心路径（MVP 必做）**：
`POST /auth/login` → `GET /projects` → `POST /builds` → `GET /builds/{id}/logs/stream`
→ `GET /environments` → `GET /environments/{key}/services` → `POST /deployments`
→ `GET /deployments/{id}/logs/stream` → `GET /services/{id}/versions` → `POST /rollbacks` → `GET /audit-logs`
