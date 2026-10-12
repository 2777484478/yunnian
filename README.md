# YunNian 云辇 · AI 运维管理发布平台

内部 CI/CD 管理平台。第一阶段目标：**CI 镜像构建管理** + **CD 环境部署管理**，
统一纳管公司 20 个项目（前端 10 / 后端 10）。

> **名称**：`YunNian` 对应中文 **云辇**（「辇」为载运之器，取"把构建产物运到各环境"之意）。
> Logo 为橙色渐变圆角方块 + 白色云（矢量），用于页面标识与浏览器标签页图标；
> 详见 `docs/04-前端设计.md` 的「品牌标识与 Logo」。

---

## 当前进度

| 模块 | 状态 |
| --- | --- |
| 前端（React 18 + TS + Vite + AntD 5，18 个业务页面） | ✅ 已实现并接入真实接口（`src/mock` 已删除） |
| 后端（Go 1.26 + Gin + GORM + MySQL 8 + Redis 7） | ✅ 已实现，冒烟 50/50 通过（详见 [`backend/README.md`](./backend/README.md)） |
| 设计文档（架构 / DDL / API / 前端 / 时序 / 排期） | ✅ 已随实现同步修订；**排期见 [`docs/06`](./docs/06-MVP排期建议.md)** |

后端实现范围：`docs/03` 的 13 个模块全部落地（认证/用户角色/项目/构建/镜像/环境/服务实例/
部署/回滚/审计/通知/模型通道/AI/Webhook），含多条 SSE 实时流（构建、部署、回滚、Pod 日志、AI 问答）。
共 **24 张表**；未落地的少数项在 `backend/README.md` 的「已知的未落地项」逐条说明。

> 前端先做出来是为了确认交互与视觉方向，随后逐页替换为 `@/api` —— `frontend/src/mock/`
> 已整体删除，页面上没有残留的示例数据。
> ⚠️ 后端 DTO 以 `frontend/src/types/index.ts` 为对齐基准（例如项目统计是扁平的
> `totalBuilds` / `successRate`，不是 `docs/03` 示例里的 `stats` 对象），改接口时两边一起改。

---

## 目录结构

```
YunNian/
├── frontend/               # React 18 + TS + Vite + Ant Design 5
│   ├── src/
│   │   ├── api/            # 真实接口层（请求封装、登录态、SSE 订阅）
│   │   ├── pages/          # 18 个业务页面
│   │   ├── components/     # PageHeader / StatCard / StatusTag / LogPanel / charts / KVFields
│   │   ├── layouts/        # 顶栏 + 侧边菜单
│   │   ├── types/          # 领域模型（与后端 DTO 对齐）
│   │   └── styles/         # 设计 Token + 全局样式
│   └── dist/index.html     # 构建产物：自包含单文件，双击即可打开
├── backend/                # Go 后端（详见 backend/README.md）
│   ├── cmd/server/         # 入口
│   ├── configs/            # 配置
│   ├── scripts/smoke.sh    # 冒烟脚本（50 项断言）
│   └── internal/           # api / service / store / model / auth / cache / queue / integration …
├── docker-compose.dev.yml  # 本地依赖：MySQL 8 + Redis 7
└── docs/                   # 设计文档
```

---

## 快速开始

### 前端原型

```bash
cd frontend
npm install
npm run dev      # http://127.0.0.1:5273
```

产出自包含单文件（便于发给同事评审，无需服务器）：

```bash
npm run build    # → frontend/dist/index.html
```

### 后端

```bash
docker compose -f docker-compose.dev.yml up -d   # MySQL 8 + Redis 7
cd backend
go run ./cmd/server -config configs/config.yaml  # 首次自动建表 + 灌种子数据
bash scripts/smoke.sh                            # 另开终端跑冒烟
```

演示账号 `ops` / `changeme`（运维）、`admin` / `changeme`（管理员）。
后端默认监听 `:8080`，接口前缀 `/api/v1`，并会托管 `frontend/dist` 作为静态资源
（即 `npm run build` 之后直接开 `http://127.0.0.1:8080` 就能看到前端）。

---

## 页面清单

| 分组 | 页面 |
| --- | --- |
| 概览 | 运维发布总览（指标卡、构建趋势、结果分布、最近构建、环境健康、待处理） |
| CI 镜像构建 | 项目管理 · 项目详情 · Jenkins Job（只读）· 构建记录（实时日志 + AI 诊断）· 镜像版本 |
| CD 环境部署 | 发布拓扑（按阶段排序）· 环境管理（卡片/表格双视图）· 环境详情（服务/部署历史/回滚/环境变量）· 部署记录（含审批）· 回滚记录 |
| AI | AI 运维助手（自然语言查询 + 只读工具取数 + 结构化结果卡片） |
| 治理 | 操作审计 |
| 系统管理 | 用户与权限 · 通知渠道 · 第三方集成 · 集群接入 · 模型设置 |

交互亮点：一键编译弹窗（分支 / 参数覆盖 / 历史构建）、**实时流式日志面板**（终端观感、关键字过滤、跟随滚动、复制导出）、
部署向导（三步 + 进度 + 步骤 + 日志）、回滚弹窗（历史版本单选 + 必填原因）、服务操作菜单 8 项（部署/日志/重新部署/回滚/设置参数/扩缩容/重启/下线）。

---

## 文档

见 [`docs/README.md`](./docs/README.md) —— 含架构设计、数据库 DDL、API 设计、前端设计、时序图、MVP 排期，
以及**需要拍板的关键决策清单**。
