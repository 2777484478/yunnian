// ---------------------------------------------------------------------------
// YunNian 云辇 · 领域模型类型定义
// 第一阶段（MVP）：CI 镜像构建 + CD 环境部署
// ---------------------------------------------------------------------------

/** 项目类型：前端 / 后端 */
export type ProjectType = 'frontend' | 'backend';

/** 构建方式 */
export type BuildMethod = 'dockerfile' | 'jenkins' | 'script';

/**
 * 构建状态机。
 *
 * `interrupted`（平台重启时中断）是独立一档，**不是** failed / canceled 的马甲：
 * 构建队列在进程内，平台一重启它连同执行协程一起消失，那条记录再也没人会去写终态。
 * 判成 failed 会引着人去查"这次构建哪里写错了"，判成 canceled 会把"平台重启"
 * 谎报成"有人点了取消"——两者都会让人往错的方向排查。
 */
export type BuildStatus = 'queued' | 'running' | 'success' | 'failed' | 'canceled' | 'interrupted';

/**
 * 环境**资源状态**（不是服务健康）。
 *
 * 取值只由该环境所在集群的节点 CPU / 内存用量决定
 * （后端 `SyncEnvironment`：>=90 → error、>=75 → warning、读不到 → unknown），
 * 所以界面上读作「资源紧张 / 资源告警」。
 */
export type EnvStatus = 'healthy' | 'warning' | 'error' | 'unknown';

/** 服务运行状态 */
export type ServiceStatus = 'running' | 'deploying' | 'error' | 'stopped' | 'pending';

/**
 * 部署状态。
 *
 * `pending_approval` 是**合法长期态**（审批人可能明天才点），不是"卡住了"；
 * `interrupted` 则相反 —— 它是平台重启留下的终态，不会再自行推进。
 * 两者都不能与 `progressing` 混为一谈。
 */
export type DeployStatus =
  | 'pending_approval'
  | 'queued'
  | 'progressing'
  | 'success'
  | 'failed'
  | 'rolled_back'
  | 'canceled'
  | 'interrupted';

/** RBAC 角色 */
export type UserRole = 'admin' | 'dev' | 'qa' | 'ops' | 'readonly';

/** Git 托管平台 */
export type GitProvider = 'gitlab' | 'github' | 'gitee';

/** 构建参数（Key-Value，支持密文） */
export interface BuildParam {
  key: string;
  value: string;
  desc?: string;
  /** 密文参数（如 Maven 私服密码），前端脱敏展示 */
  secret?: boolean;
}

// --------------------------------------------------------------------------- CI

export interface Project {
  id: string;
  /** 项目标识，全局唯一（英文小写+中划线） */
  key: string;
  name: string;
  type: ProjectType;
  gitProvider: GitProvider;
  gitUrl: string;
  /**
   * 拉代码用的账号。**明文下发**：它不是秘密（git remote 里就有），
   * 而且"这个项目用哪个账号拉的"必须能被人看到。
   * 密码不在这里，也永远不会下发（见 api/ci.ts 的 testConnection）。
   */
  gitUsername: string;
  defaultBranch: string;
  buildMethod: BuildMethod;
  /** buildMethod=jenkins 时使用 */
  jenkinsJob?: string;
  dockerfilePath: string;
  buildContext: string;
  /**
   * buildMethod=script 时的构建脚本正文。
   *
   * 无条件下发（不按 buildMethod 过滤）：切换构建方式时要能回填已写过的内容，
   * 过滤掉会让用户一切换就白写一遍。
   */
  buildScript: string;
  /** 镜像仓库地址，如 harbor.yunnian.internal */
  registry: string;
  /** 镜像名称，如 frontend/user-web */
  imageName: string;
  /** Tag 规则模板 */
  tagPattern: string;
  buildParams: BuildParam[];
  owner: string;
  team: string;
  description: string;
  enabled: boolean;
  createdAt: string;
  updatedAt: string;
  /** 最近一次构建（列表页直接展示，避免 N+1） */
  lastBuild?: BuildRecord;
  totalBuilds: number;
  successRate: number;
  avgDuration: number;
}

export interface BuildRecord {
  id: string;
  buildNo: number;
  projectKey: string;
  projectName: string;
  projectType: ProjectType;
  branch: string;
  commitId: string;
  commitMsg: string;
  /** 完整镜像地址 repo:tag */
  image: string;
  imageDigest: string;
  /** 镜像大小（字节）。**null = 读不到**（仓库元数据没取到），不是 0 ——
   *  后端下发的是 null，别用 `|| 0` 把它变成"0 字节"。 */
  imageSize: number | null;
  status: BuildStatus;
  /** 耗时（秒） */
  duration: number;
  progress: number;
  stage: string;
  trigger: string;
  triggerType: 'manual' | 'webhook' | 'schedule';
  startedAt: string;
  finishedAt?: string;
  /**
   * 失败原因：`errorCode` 是机器码（TIMEOUT / ROLLOUT_STUCK / QUEUE_REJECTED），
   * `errorMessage` 是给人看的真因。
   *
   * 为什么两个都要：机器码用来筛选与统计，但它解释不了现场；真因以前**只活在日志流里**，
   * 而日志流是进程内 + TTL 的 —— 平台一重启，那条失败记录就只剩一个机器码。
   * 加这列之前的旧记录两者都可能是空的，展示时要如实说"未记录"，不要留白。
   */
  errorCode?: string;
  errorMessage?: string;
  /**
   * 构建失败时的 AI 归因。
   *
   * `source` 标明结论从哪来：`model`=模型读了日志得出，`rule`=平台错误码规则匹配。
   * 列表页不展示细节，但任何展示它的地方都必须按 source 措辞——
   * 一律说成"AI 分析"就是在替一条查表结果冒充模型输出。
   */
  aiDiagnosis?: {
    reason: string;
    suggestion: string;
    confidence: number;
    source?: 'model' | 'rule';
    model?: string;
  };
  /**
   * 日志正文行（**纯字符串**，不含级别与时刻）。
   *
   * 注意三个接口的形状差异，这是实测确认的后端契约：
   *  1. 本字段（`/builds/{id}` 详情）是 `string[]`，只有正文；
   *  2. `/builds/{id}/logs` 返回 `{list:[{seq,level,line,ts}]}` —— **结构化**；
   *  3. SSE `log` 事件也是结构化行。
   * 结构化的那两路走 `LogLineRecord[]`，本字段保持字符串。
   */
  logs: string[];
}

/** 结构化日志行：构建 / 部署 / 回滚 / Pod 日志接口与 SSE `log` 事件共用同一形状 */
export interface LogLineRecord {
  seq: number;
  level: string;
  /** 日志正文（不含时间戳前缀） */
  line: string;
  ts: string;
}

export interface ImageRecord {
  id: string;
  projectKey: string;
  projectName: string;
  tag: string;
  image: string;
  digest: string;
  /** 字节。null = 仓库元数据没读到，**不是 0**。渲染走 humanSize()，它会把 null 显示成 `-`。 */
  size: number | null;
  branch: string;
  commitId: string;
  buildNo: number;
  registry: string;
  pushedAt: string;
  pullCount: number;
  /** 已部署到哪些环境 */
  deployedEnvs: string[];
}

// --------------------------------------------------------------------------- CD

export interface EnvVar {
  key: string;
  value: string;
  secret?: boolean;
}

export interface Environment {
  id: string;
  /** 环境标识，唯一，如 test / staging / prod */
  key: string;
  name: string;
  cluster: string;
  clusterVersion: string;
  namespace: string;
  /**
   * 部署方式。后端仍会返回这个字段（原型期留下的列，可能残留 helm/compose 的历史值），
   * 但**前端不再消费**：平台改集群只有一条路 —— 写 AppRelease CR，由 operator 下发到
   * Deployment，后端没有任何代码读它。曾经这里是个四选一下拉 + 列表列 + 标签，
   * 全是假选项（选 Helm 和选 K8s 行为完全一样，2026-10-15 收敛掉）。
   *
   * 故意标成 `string` 而不是联合类型：不给"再按它分支"留余地。
   */
  deployMethod: string;
  /** 关联项目 key 列表 */
  projectKeys: string[];
  vars: EnvVar[];
  registry: string;
  ingressDomain: string;
  owner: string;
  description: string;
  enabled: boolean;
  /** 正式区部署需要审批 */
  needApproval: boolean;
  approvers: string[];
  /**
   * 与 `approvers` **一一对应**的登录名（用户名），后端专门为编辑表单回的这一份。
   *
   * 编辑表单提交的必须是用户名，而 `approvers` 是展示名 —— 只回展示名时表单没法预填，
   * 于是「打开编辑不重选」就等于把审批人清空（且 needApproval 下会被必填规则挡住，
   * 逼用户每次重新点一遍）。展示点继续用 `approvers`，只有表单读这里。
   */
  approverUsernames: string[];
  createdAt: string;
  updatedAt: string;
  status: EnvStatus;
  cpuUsage: number;
  memUsage: number;
  nodes: number;
  lastSyncAt: string;
  /**
   * 发布阶段：拓扑横向排序的依据（越小越靠左）。
   *
   * 由**环境自带**（用户 2026-09-29 定）：这样自定义环境（如「华东压测区」）
   * 也能排进链路，而不是被扔到最后。后端 2026-09-29 已补上 `stage_label` /
   * `stage_order` 两列；没填时回退到按 key 的默认映射（见 `stageOf`）。
   */
  stageOrder?: number;
  stageLabel?: string;
  /**
   * 集群凭据来源：`platform` = 引用「系统管理 › 集群接入」里已接入集群的凭据；
   * `own` = 该环境自带一份 kubeconfig（用户 2026-09-29 定为「两者都允许」）。
   * 缺省按 `platform` 处理。
   */
  clusterSource?: 'platform' | 'own';
  /** 该环境是否配了自带的 kubeconfig（**只回「有没有」，内容永不出库**） */
  hasKubeconfig?: boolean;
  /**
   * 自带 kubeconfig 时提交的凭据原文。**只写不读**：
   * 接口只回 `hasKubeconfig` 布尔，明文没有任何读出口。
   * 服务端按该 kubeconfig 的 current-context 最小化后加密存储。
   */
  kubeconfig?: string;
}

export interface ServicePod {
  name: string;
  status: 'Running' | 'Pending' | 'CrashLoopBackOff' | 'Terminating' | 'Completed';
  node: string;
  restarts: number;
  age: string;
  ip: string;
  cpu: string;
  mem: string;
}

export interface DeployVersion {
  tag: string;
  image: string;
  deployedAt: string;
  operator: string;
  status: 'success' | 'failed';
  note?: string;
}

export interface EnvService {
  id: string;
  envKey: string;
  projectKey: string;
  projectName: string;
  projectType: ProjectType;
  kind: 'Deployment' | 'StatefulSet';
  image: string;
  tag: string;
  replicas: number;
  readyReplicas: number;
  status: ServiceStatus;
  cpuRequest: string;
  cpuLimit: string;
  memRequest: string;
  memLimit: string;
  envVars: EnvVar[];
  configMaps: string[];
  secrets: string[];
  entrypoint: string;
  updatedAt: string;
  updatedBy: string;
  /**
   * 这里曾有 `health: { liveness, readiness }`，2026-10-09 随后端一起退役。
   *
   * 它读的是平台库里的 `liveness_ok / readiness_ok` —— **静态派生值**，
   * 由 `seed_ci.go` 按平台 status 反推（新建服务实例时直接写死 true），
   * 与集群里的探针状态**无关**。实测 demobasic：库里两项都是 true、界面显示"通过"，
   * 而集群里新 Pod 正在 CrashLoopBackOff、Deployment 已报 ProgressDeadlineExceeded
   * —— 两个"通过"恰好把异常盖住，正是"只展示成功的一面"。
   * 界面一律以 `live` 为准（见 `utils/live.ts` 的 `liveHealthy`）；
   * 读不到集群就说读不到，**不回退**这些库值。
   */
  pods: ServicePod[];
  history: DeployVersion[];
  /**
   * 集群实时状态。**界面上应当优先相信这一份**。
   *
   * 上面的 replicas / readyReplicas / status / tag / image 是平台库里存的值，
   * 含义是"平台打算让它跑成什么样"（纳管时按默认值写入，之后只有部署成功才更新），
   * 不代表集群里现在是什么样。此前界面直接显示它们，于是出现
   * "平台说 2/2 运行中、集群里其实只有 1/1" 这类假数据。
   *
   * 三种情形要分清，混起来就是对用户撒谎：
   * - `available && exists` → 读到了，字段即事实；
   * - `available && !exists` → **确认**集群里没有这个工作负载；
   * - `!available` → **没读到**（未接入/权限/超时），`message` 说明原因。
   */
  live?: ServiceLive | null;
}

/** 工作负载在集群里的实时状态。 */
export interface ServiceLive {
  available: boolean;
  exists: boolean;
  image: string;
  tag: string;
  replicas: number;
  /**
   * 集群里**实际存在**的实例数（`status.replicas`）。
   *
   * 与 `replicas`（期望数）分开：滚动更新卡住时新 Pod 反复崩溃、旧 Pod 不被杀，
   * 实际有 2 个而期望仍是 1 个 —— 只按期望渲染会写成 `1/1`，
   * 把"有一个实例正在崩"抹掉。显示口径见 `utils/live.ts` 的 `liveReplicaTotal`。
   *
   * 可选：老后端不下发时退化为 `replicas`，与旧行为一致。
   */
  actualReplicas?: number;
  readyReplicas: number;
  status: string;
  revision: number;
  /** 容器声明的资源规格（**不是使用量**）。空串表示容器没写，界面显示 "—"。 */
  cpuRequest: string;
  cpuLimit: string;
  memRequest: string;
  memLimit: string;

  // ---------------------------------------------------------------- 实时用量
  /**
   * 是否读到了用量（集群装了 metrics-server 且凭据可读）。
   *
   * false 时下面的用量字段是空的、百分比是 -1 —— 界面显示 "—"，
   * **不能当 0**：那会被读成"这个服务几乎不占资源"，与事实正好相反。
   */
  metricsAvailable?: boolean;
  /** 该服务名下所有 Pod 的**合计**用量（CPU 毫核口径、内存 Mi/Gi） */
  cpuUsage?: string;
  memUsage?: string;
  /**
   * 合计用量占**基准**的百分比（可能 >100）。-1 表示算不出来（没有基准），
   * 此时不画进度条 —— 画一根 0% 的条会被读成"几乎没用"。
   */
  cpuUsagePercent?: number;
  memUsagePercent?: number;
  /**
   * 上面百分比按什么算的：`limit` / `request` / `''`。
   *
   * 两者含义**完全不同**：超 limit 会被限流/OOM Kill，超 request 只是调度层面的事、
   * 很常见。所以基准必须跟着数字一起显示，不能合成一句"占用率"。
   */
  cpuUsageBase?: 'limit' | 'request' | '';
  memUsageBase?: 'limit' | 'request' | '';
  /** 真的有用量数据的 Pod 数 / 匹配到的 Pod 总数（刚起的 Pod 要等几十秒才有数据） */
  usagePods?: number;
  usagePodTotal?: number;
  /**
   * 该服务名下所有 Pod 的容器重启次数**之和**（kubectl 的 RESTARTS 列）。
   *
   * 与 `metricsAvailable` 无关：这是 Pod 对象自带的字段，
   * 没装 metrics-server 时也有值 —— 而那时它恰恰是最该看的东西。
   */
  restarts?: number;
  /** 最新 Pod 的创建时间（RFC3339）。空 = 没有 Pod。口径是**实例**年龄，不是 Deployment 的。 */
  podStartedAt?: string;
  message: string;
}

export interface DeployStep {
  name: string;
  status: 'pending' | 'running' | 'success' | 'failed';
  at?: string;
  detail?: string;
}

export interface DeployRecord {
  id: string;
  deployNo: string;
  envKey: string;
  envName: string;
  projectKey: string;
  projectName: string;
  service: string;
  image: string;
  /**
   * 这次部署实际用的那份镜像的摘要（sha256:…）。
   *
   * 从「仓库标签」发起的部署没有 image_record 可查，这个字段就是"当时发的是哪一份"
   * 的唯一答案（同一个 tag 被重新推过就指向别的内容了）。仓库读不到时后端留空。
   */
  imageDigest?: string;
  replicas: number;
  status: DeployStatus;
  progress: number;
  steps: DeployStep[];
  operator: string;
  approver?: string;
  startedAt: string;
  finishedAt?: string;
  duration: number;
  /** 该次部署是否为回滚产生，以及回滚来源单号 */
  rollbackOf?: string;
  /** 失败原因：`errorCode` 机器码 / `errorMessage` 人类可读真因。见 BuildRecord 同名注释。 */
  errorCode?: string;
  errorMessage?: string;
  /**
   * **判失败之后**集群自己就绪的时间。
   *
   * 只有 `ROLLOUT_STUCK` 会有：K8s 到 progressDeadlineSeconds 就宣布卡死，
   * 而镜像拉得慢时 Pod 常在宣布之后才起来 —— 于是"后台其实成功了、页面说失败"。
   * 状态仍记为失败（失败是当时的事实），这个字段只用来标「失败后已就绪」。
   */
  recoveredAt?: string;
  /** 部署异常时的 AI 诊断建议 */
  aiDiagnosis?: { reason: string; suggestion: string; confidence: number };
  logs: string[];
}

export interface RollbackRecord {
  id: string;
  /** 关联的部署单主键，用于把回滚日志流接到部署记录上 */
  deployId?: string;
  envKey: string;
  envName: string;
  projectKey: string;
  service: string;
  fromImage: string;
  toImage: string;
  reason: string;
  operator: string;
  /**
   * 回滚状态。
   *
   * `interrupted`（平台重启时中断）与部署单同一来源：回滚复用部署状态机，
   * 收尾写入点是 `finishRollbackOf`（部署执行器跑完时调用）—— 平台一重启那次调用就没了，
   * 记录会永远停在 `progressing`。启动补偿会把它改写成 `interrupted`。
   * 刻意**不通写成 failed**：回滚本身没出错，是平台重启把它打断了。
   */
  status: 'success' | 'failed' | 'progressing' | 'interrupted';
  startedAt: string;
  duration: number;
  deployNo: string;
  /**
   * 触发方式：manual（人工）/ ai_suggested（AI 诊断建议）/ auto（发布失败后的自动回滚）。
   * `auto` 由后端在部署失败时自己写入，接口不接受客户端传这个值。
   */
  triggerType: 'manual' | 'ai_suggested' | 'auto';
}

// --------------------------------------------------------------------------- 通用能力

export interface AuditLog {
  id: string;
  user: string;
  userName: string;
  role: UserRole;
  module: 'CI' | 'CD' | '系统' | '认证';
  action: string;
  targetType: string;
  target: string;
  detail: string;
  ip: string;
  result: 'success' | 'failed';
  at: string;
}

export interface SysUser {
  id: string;
  username: string;
  name: string;
  email: string;
  phone: string;
  role: UserRole;
  teams: string[];
  enabled: boolean;
  lastLoginAt: string;
  createdAt: string;
}

export interface SysRole {
  key: UserRole;
  name: string;
  desc: string;
  users: number;
  color: string;
  perms: string[];
}

export interface NotifyChannel {
  id: string;
  /**
   * 渠道类型。**只有两个字面量**是刻意的：后端入参枚举已收敛成邮件与钉钉，
   * 因为平台只实现了这两种发送器。前端如果多留几种，用户就能配出一个
   * "看起来成功了、但永远发不出去"的渠道 —— 那比报错难查得多。
   *
   * 后面那截 `(string & {})` 是为了**历史数据**：库里可能还留着 wecom/webhook
   * 的记录，界面对认不出的类型会原样把类型名显示出来，而不是当成邮件去渲染。
   */
  type: 'email' | 'dingtalk' | (string & {});
  name: string;
  target: string;
  enabled: boolean;
  events: string[];
  /** 最近一次**成功**投递的时间；为空表示从未成功发过（失败会体现在 failCount/lastError 上） */
  lastSentAt?: string;
  /** 连续失败次数。成功一次即清零 —— 它回答的是"现在正常吗"，不是"历史上错过几次" */
  failCount: number;
  /** 最近一次失败的原因。"配了渠道却没收到消息"唯一的自查线索 */
  lastError?: string;
  updatedAt: string;
}

export interface AiMessage {
  id: string;
  role: 'user' | 'assistant';
  content: string;
  at: string;
  /** 助手回复时携带的结构化卡片 */
  attachments?: { kind: 'table' | 'kv' | 'action'; title: string; data: any }[];
}

// --------------------------------------------------------------------------- 批量任务
//
// 批量任务的状态、任务定义与执行句柄**不放在这里**，而是与 BatchRunner 同处一文件
// （见 components/BatchRunner.tsx）。
//
// 原因：它们是「组件与调用方之间的执行契约」，不是平台数据模型。
// 早期这里放了一份带 `frames`（前端预生成日志帧）的 BatchTaskDef，
// 现在日志来自后端 SSE，那个字段已经不存在了——两份同名类型留在仓库里，
// 下一个人 import 错一份就会写出 `task.frames` 这种永远为 undefined 的代码。

// --------------------------------------------------------------------------- AI 模型配置

/** 模型接入渠道标识 */
export type ModelVendor = 'dashscope' | 'qwen' | 'deepseek' | 'openai' | 'custom';

/** 模型通道连通性状态 */
export type ModelConnStatus = 'connected' | 'failed' | 'unchecked' | 'testing';

/** 大模型接入配置（对应后端 ai_model_config 表） */
export interface ModelProvider {
  id: string;
  /** 展示名称，如「阿里云百炼」 */
  name: string;
  vendor: ModelVendor;
  /**
   * OpenAI 兼容的 API Base 地址。
   * 注意是 /v1 级别的根地址（不含 /chat/completions），
   * 后端拼接时统一补 `chat/completions`，避免各厂商路径写法不一致。
   */
  baseUrl: string;
  /** 密钥（前端仅回显掩码，明文不下发） */
  apiKeyMasked: string;
  /** 该通道可选模型列表 */
  models: string[];
  /** 当前选中的模型 */
  defaultModel: string;
  enabled: boolean;
  /** 平台默认通道：AI 助手未指定模型时使用 */
  isDefault: boolean;
  /** 请求超时（秒） */
  timeout: number;
  /** 最大并发请求数（影响 AI 助手的并发分析） */
  maxConcurrency: number;
  /** 采样温度 */
  temperature: number;
  /** 单次最大输出 token */
  maxTokens: number;
  status: ModelConnStatus;
  /** 最近一次连通性测试耗时（毫秒） */
  latency?: number;
  checkedAt?: string;
  /** 近 7 天调用次数 */
  callCount: number;
  /** 失败次数（近 7 天），用于判断通道健康度 */
  failCount: number;
  desc: string;
  updatedAt: string;
}

// --------------------------------------------------------------------------- 通用响应

export interface PageResult<T> {
  list: T[];
  total: number;
  page: number;
  pageSize: number;
}

export interface ApiResult<T> {
  code: number;
  message: string;
  data: T;
  traceId: string;
}

// --------------------------------------------------------------------------- 会话与权限

/**
 * 当前登录用户（GET /auth/me）。
 *
 * 注意后端同时下发 `role`（嵌套对象）与 `roleCode`/`roleName`（扁平字段）两种写法，
 * 后者是为了兼容页面里大量 `user.roleName` 的直接引用，不要当冗余删掉。
 */
export interface CurrentUser {
  id: string;
  username: string;
  name: string;
  email: string;
  role: { code: UserRole; name: string };
  roleCode: UserRole;
  roleName: string;
  teams: string[];
  /** 权限点，支持通配（如 `deploy:*`、`*`），匹配规则见后端 auth.Match */
  permissions: string[];
}

export interface LoginResult {
  accessToken: string;
  refreshToken: string;
  tokenType: string;
  /** accessToken 有效期（秒） */
  expiresIn: number;
  user: CurrentUser;
}

/**
 * 权限点定义（GET /permissions）。
 *
 * 注意：`/permissions` 返回的是**对象数组**（带中文名与所属模块，供权限编辑页展示），
 * 而 `SysRole.perms` 里装的是**权限 code 字符串**。两者不要混用。
 */
export interface PermissionPoint {
  code: string;
  name: string;
  module: string;
  description: string;
}

/** 集成状态（GET /system/integrations） */
export interface IntegrationStatus {
  key: string;
  name: string;
  /** 实现方式：local / 真实下游类型 */
  impl: string;
  enabled: boolean;
  ready: boolean;
  detail: string;
}

/** 平台配置项（GET /system/config） */
export interface SystemConfigItem {
  configKey: string;
  configValue: string;
  valueType: string;
  groupName: string;
  description: string;
  updatedAt: string;
}

/** 平台概览统计（GET /system/stats） */
export interface SystemStats {
  projects: number;
  todayBuilds: number;
  /** 今日已结束的成功构建数；与 todayFailed 相加不等于 todayBuilds（差值是进行中/排队中） */
  todaySuccess: number;
  todayFailed: number;
  successRate: number;
  environments: number;
  runningBuilds: number;
  runningDeploys: number;
  pendingApproval: number;
  totalImages: number;
  totalDeploys: number;
  totalRollbacks: number;
  activeServices: number;
  // ⚠️ 这里**曾经**有 `abnormalServices`（`service_instance.status` 的库内计数）。
  // 2026-10-09 连同后端字段一起退役：那个 status 是"平台期望"、会与集群漂移，
  // 实测报 1 而真实是 0。要数"需要关注的服务"请用 `/environments/{key}/services`
  // 的集群实时状态（`utils/live.ts` 的 `isServiceAbnormal`）。
}

// ---------------------------------------------------------------------------
// K8s 集群接入（对应后端 /clusters）
//
// 这一组类型的存在本身就说明了一件事：**平台原先没有"集群"这个实体**。
// environment.cluster 只是一段自由文本（后端只校验 required,max=128），
// 手输一个拼错的集群名也能存进去；执行侧走的是 LocalCluster 模拟器
// （Ping 恒真、Nodes 按集群名哈希编数），于是「测试连接」永远全绿。
// 接入之后，集群才有凭据、有真实探测结果，环境才是在**引用**它。
// ---------------------------------------------------------------------------

/** 集群连接状态。unprobed = 接入后还没探过（不是"连不上"） */
export type ClusterStatus = 'connected' | 'unreachable' | 'unauthorized' | 'unprobed';

export interface K8sCluster {
  /** 集群标识，environment.cluster 存的就是它 */
  key: string;
  name: string;
  /** kubeconfig 里该 context 指向的 API Server */
  apiServer: string;
  /** 选定的 context 名 */
  context: string;
  /** kubeconfig 内部的集群名（与 context 未必同名，排障时用得上） */
  kubeconfigCluster: string;
  /** 探测得到的 K8s 版本；未探测为空串 */
  k8sVersion: string;
  /** 节点总数；未探测为 null */
  nodes: number | null;
  /** Ready 节点数；未探测为 null */
  nodesReady: number | null;
  /** 命名空间数量；未探测为 null */
  namespaceCount: number | null;
  status: ClusterStatus;
  /** 上次探测时间，未探测为空串 */
  lastProbedAt: string;
  /** 上次探测的原始结论：成功是「集群连通」，失败是真实错误原文 */
  lastProbeMessage: string;
  /** 上次探测耗时（毫秒） */
  lastProbeMs: number;
  /** 是否已保存 kubeconfig（**只回布尔，内容没有出口**） */
  hasKubeconfig: boolean;
  /** 引用该集群的环境 key 列表。删除时靠它拦：非空即不许删 */
  usedByEnvs: string[];
  remark: string;
  createdAt: string;
  updatedAt: string;
}

/** kubeconfig 里的一个 context（解析结果，供选择接入哪些） */
export interface ClusterContextOption {
  context: string;
  /** 该 context 引用的集群名（kubeconfig 内部名） */
  cluster: string;
  apiServer: string;
  /** context 自带的默认命名空间，常见为空 */
  namespace: string;
  user: string;
  /**
   * 凭据形态，取值与后端 `authKindOf` 一致：
   * token / token-file / client-cert / basic / exec / auth-provider / none。
   * 只用来告诉用户"这个 context 靠什么鉴权"，不是安全判定。
   */
  credentialKind: string;
  /**
   * **非空即接入不了**：引用的集群不存在、user 不存在、证书缺一半、什么都没有…
   * 由后端在解析时就判定（`integration.ParseKubeconfig`）。
   * 前端拿它做三件事：默认不勾选、禁止手动勾选、把原因挂在「凭据」列的 Tooltip 上。
   */
  problem: string;
  /** 平台是否已接入过同一个 apiServer + context（重复接入要提示） */
  alreadyImported: boolean;
}

export interface ClusterParseResult {
  ok: boolean;
  /** 解析失败的原因（kubeconfig 格式错、缺 context、证书字段不完整…） */
  errors: string[];
  /** kubeconfig 声明的 current-context，可能不在 contexts 里 */
  currentContext: string;
  contexts: ClusterContextOption[];
}

/** 连通性探测结果。ok=false 也是 HTTP 200（与 GitLab/Jenkins 的测试接口同约定） */
export interface ClusterTestResult {
  key: string;
  ok: boolean;
  message: string;
  latencyMs: number;
  k8sVersion: string;
  nodes: number | null;
  nodesReady: number | null;
  namespaceCount: number | null;
  /**
   * 「能连上但迟早出事」类提醒：权限只够只读、证书快过期等。与 ok 无关，必须原样展示。
   *
   * **永远是数组**：后端的列表字段一律发 `[]` 而非 `null`（`nonNilSlice`），
   * api 层也会兜一层折平。别在这里写回 `string[] | null` ——
   * 那会让每个读取点都得加 `?.`，漏掉一个就是一次渲染崩溃。
   */
  warnings: string[];
}

/** 接入/编辑入参。kubeconfig 留空 = 不修改已存凭据（原文没有出口） */
export interface ClusterInput {
  key: string;
  name: string;
  context: string;
  /** 仅接入或更换凭据时传；留空表示保持原 kubeconfig 不变 */
  kubeconfig?: string;
  remark?: string;
}

/**
 * 批量接入的结果。
 *
 * 之所以是"部分成功"结构而不是一个数组：一次导入常含多个 context，
 * 其中某个集群网络不通是常事。若整批回滚，用户得挨个重试；
 * 若只返回成功项，用户又不知道另外几个为什么没出现。
 * 所以失败的必须带 context 名与原因单独回来，页面逐条展示。
 */
export interface ClusterCreateResult {
  clusters: K8sCluster[];
  /** 探测未通过而未接入的项；全成功时为 null */
  errors: { context: string; message: string }[] | null;
}
