import { http, streamUrl } from './client';
import { unwrapList, unwrapPage, params } from './shape';
import type {
  DeployRecord,
  DeployStatus,
  DeployVersion,
  Environment,
  EnvService,
  EnvVar,
  LogLineRecord,
  PageResult,
  ProjectType,
  RollbackRecord,
  ServicePod,
} from '@/types';

/**
 * 环境纳管项目候选（GET /environments/{key}/project-candidates）。
 *
 * 注意它不是完整的 Project —— 后端只下发挑选项目所需的最小字段：
 * `linked` 表示该项目已被某个环境纳管，`hasBuild` 表示它有过构建产物。
 * 早期直接把它声明成 `Project` 是错的，会让页面写出 `p.registry` 这种
 * 永远为 undefined 的字段访问。
 */
export interface EnvProjectCandidate {
  key: string;
  name: string;
  type: ProjectType;
  team: string;
  /** 是否已被某个环境纳管 */
  linked: boolean;
  /** 是否已有构建产物（没有产物的项目无法部署） */
  hasBuild: boolean;
}

/** 环境集群资源占用（GET /environments/{key}/resources） */
export interface EnvResources {
  cpuUsage: number;
  memUsage: number;
  nodes: number;
  /** 集群 K8s 版本，如 v1.29.6 */
  version: string;
  /**
   * 是否真的从 metrics.k8s.io 读到了用量。
   *
   * `false` 时 cpuUsage / memUsage 恒为 0——那是"没读到"，不是"用量为 0"。
   * 集群没装 metrics-server、或当前凭据无权读它，都会落到这里。
   * 界面据此分叉：读不到就别画进度条，更不要回退到环境快照里那个旧值。
   */
  metricsAvailable?: boolean;
  /**
   * 集群可分配的 CPU 核数 / 内存字节数（各节点 allocatable 之和）。
   *
   * 用 allocatable 而不是 capacity：后者含系统预留，拿它算百分比会系统性偏低、
   * 看着"很闲"其实已经吃满。配合 cpuUsage / memUsage（百分比）能换算出已用绝对值。
   */
  cpuCores?: number;
  memBytes?: number;
}

/** 部署单 AI 诊断（GET /deployments/{id}/ai-diagnosis） */
export interface DeployDiagnosis {
  /**
   * 结论来源。取值只有两个：
   * - `model` 模型分析（读了平台日志 + 集群侧证据：容器状态 / 退出码 / 崩溃前那一轮日志 / Pod 事件）
   * - `rule`  平台错误码规则查表
   *
   * 必须区分展示：两者文案长得一样，用户会把查表结论当成日志被看过一遍。
   * 未知值一律按 `rule` 展示，不猜。
   */
  source: string;
  /** 模型通道名，仅 source=model 时有值（如「阿里云百炼」） */
  model?: string;
  /**
   * 需要如实告知的附加说明 —— 目前用于"这次为什么没用上模型"。
   * 降级为规则结论时前端必须把它显示出来，否则用户无从知道结论是哪来的。
   */
  notice?: string;
  deployNo: string;
  currentImage: string;
  reason: string;
  suggestion: string;
  confidence: number;
  /** 是否存在可回滚的目标版本 */
  rollbackAvailable: boolean;
  /** 建议回滚到的镜像（rollbackAvailable 为 false 时为空） */
  rollbackImage: string;
  /** 目标版本对应的部署单号 */
  rollbackDeployNo: string;
  /** 该服务在 env_services 里的主键 —— 直接调回滚接口要用它 */
  serviceId: string;
}

/** 服务参数变更历史（GET /services/{id}/params/history） */
export interface ServiceParamSnapshot {
  id: string;
  /** 第几次变更，从 1 递增 */
  version: number;
  replicas: number | null;
  cpuRequest: string;
  cpuLimit: string;
  memRequest: string;
  memLimit: string;
  hpaEnabled: boolean | null;
  envVars: EnvVar[];
  /** 与上一版的差异摘要，首版为空数组 */
  diff: string[];
  changedBy: string;
  changedAt: string;
  note: string;
}

// ---------------------------------------------------------------------------
// CD 模块：环境 / 服务实例 / 部署 / 回滚（对应后端 handler_cd.go）
//
// 权限提示：本模块里所有"变更类"接口（改参数、扩缩容、重启、重建、删除、发布、回滚）
// 的权限点是**按环境动态判定**的（deploy:test / deploy:prod …）。后端在 handler 里判，
// 前端只能靠「用户权限点里是否有该环境的 deploy 权限」提前禁用按钮——见 auth.ts 的 canDeploy。
// ---------------------------------------------------------------------------

export interface EnvQuery {
  status?: string;
  enabled?: boolean;
  keyword?: string;
  page?: number;
  pageSize?: number;
}

export const environmentApi = {
  list: async (q: EnvQuery = {}): Promise<PageResult<Environment>> =>
    unwrapPage<Environment>(await http.get('/environments', params(q))),

  get: (key: string) => http.get<Environment>(`/environments/${key}`),

  create: (body: Partial<Environment>) => http.post<Environment>('/environments', body),

  update: (key: string, body: Partial<Environment>) => http.put<Environment>(`/environments/${key}`, body),

  remove: (key: string) => http.del<null>(`/environments/${key}`),

  /** 可纳入该环境的项目候选（未被其它环境占用 / 已启用的项目） */
  projectCandidates: async (
    key: string,
    q: { status?: string; type?: string; keyword?: string } = {},
  ): Promise<EnvProjectCandidate[]> =>
    unwrapList<EnvProjectCandidate>(await http.get(`/environments/${key}/project-candidates`, params(q))),

  /**
   * 覆盖式设置该环境纳管的项目。
   *
   * 为什么不复用 `update(key, { projectKeys })`：`PUT /environments/{key}` 是整实体覆盖
   * （name/cluster/namespace/审批人都直接写），只带一个字段去打它会把其余字段清空。
   * 后端为此单开了这个正交入口，语义与 `saveVars` 一致。返回保存后的完整候选集合。
   */
  setProjects: async (key: string, projectKeys: string[]): Promise<EnvProjectCandidate[]> =>
    unwrapList<EnvProjectCandidate>(
      await http.put(`/environments/${key}/projects`, { projectKeys }),
    ),

  vars: async (key: string): Promise<EnvVar[]> =>
    unwrapList<EnvVar>(await http.get(`/environments/${key}/vars`)),

  /** 覆盖式保存环境变量 */
  saveVars: (key: string, vars: EnvVar[]) => http.put<EnvVar[]>(`/environments/${key}/vars`, { vars }),

  /** 该环境下的服务实例（返回 {list} 信封，不是标准分页） */
  services: async (key: string): Promise<EnvService[]> =>
    unwrapList<EnvService>(await http.get(`/environments/${key}/services`)),

  /** 环境的集群资源占用（CPU/内存/节点） */
  resources: (key: string) => http.get<EnvResources>(`/environments/${key}/resources`),

  /** 同步集群状态（拉取 namespace 实际副本数等） */
  sync: (key: string) => http.post<Environment>(`/environments/${key}/sync`),

  events: async (key: string, limit = 1000): Promise<{ at: string; type: string; message: string }[]> =>
    unwrapList(await http.get(`/environments/${key}/events`, params({ limit }))),

  /** 连通性测试（k8s apiserver / 镜像仓库可达性） */
  testConnection: (key: string) =>
    http.post<{ ok: boolean; message: string; latencyMs: number }>(`/environments/${key}/test-connection`),
};

export const serviceApi = {
  get: (id: string) => http.get<EnvService>(`/services/${id}`),

  /** Pod 列表（返回 {list} 信封） */
  pods: async (id: string): Promise<ServicePod[]> =>
    unwrapList<ServicePod>(await http.get(`/services/${id}/pods`)),

  /** 历史版本（返回 {list} 信封），供回滚时选择目标版本 */
  versions: async (id: string): Promise<DeployVersion[]> =>
    unwrapList<DeployVersion>(await http.get(`/services/${id}/versions`)),

  paramHistory: async (id: string): Promise<ServiceParamSnapshot[]> =>
    unwrapList<ServiceParamSnapshot>(await http.get(`/services/${id}/params/history`)),

  /**
   * 改副本/资源/环境变量等。只传要改的键，后端按指针字段判断"是否修改"。
   *
   * 注意变更说明字段叫 `note`（对应 deploy_param 表的备注列），不是 `reason`——
   * 后端结构体上没有 reason，传了会被静默忽略，参数变更历史里就看不到原因。
   */
  updateParams: (
    id: string,
    body: {
      replicas?: number;
      cpuRequest?: string;
      cpuLimit?: string;
      memRequest?: string;
      memLimit?: string;
      /** HPA 自动扩缩容开关 */
      hpaEnabled?: boolean;
      envVars?: EnvVar[];
      configMaps?: string[];
      secrets?: string[];
      /** 变更说明，写入参数快照 */
      note?: string;
    },
  ) => http.put<EnvService>(`/services/${id}/params`, body),

  /**
   * 扩缩容。变更说明这次字段名是 `reason`（不是 updateParams 的 `note`）——
   * 两个 handler 各自定义了匿名请求体，字段名不统一，按各自的实际契约传。
   */
  scale: (id: string, replicas: number, reason?: string) =>
    http.post<EnvService>(`/services/${id}/scale`, { replicas, reason }),

  /** 重启 Deployment：无请求体 */
  restart: (id: string) => http.post<EnvService>(`/services/${id}/restart`),

  /** 用当前镜像重新滚动一次：无请求体，后端返回新建的部署单（同 create 的瘦载荷） */
  redeploy: (id: string) => http.post<DeployCreated>(`/services/${id}/redeploy`),

  remove: (id: string) => http.del<null>(`/services/${id}`),

  /** Pod 实时日志流地址（SSE），tailLines 控制回溯行数 */
  podLogStream: (id: string, podName: string, tailLines = 500) =>
    streamUrl(`/services/${id}/pods/${podName}/logs/stream`, params({ tailLines })),
};

export interface DeployQuery {
  envKey?: string;
  projectKey?: string;
  status?: string;
  keyword?: string;
  page?: number;
  pageSize?: number;
}

export interface CreateDeployInput {
  envKey: string;
  projectKey: string;
  /**
   * 镜像来源，**二选一**：
   *   platform（缺省）用 imageRecordId / tag 取平台自己构建过的镜像记录
   *   registry        用 registryTag 取镜像仓库里实际存在的标签（含别人推的）
   *
   * 后端对"两个都给"和"一个都不给"都会拒 —— 界面上也必须只能选一个
   * （见 components/ImageSourcePicker）。
   */
  imageSource?: 'platform' | 'registry';
  /** 指定镜像记录（优先）或直接给 tag；两者都给时后端以 imageRecordId 为准 */
  imageRecordId?: string;
  tag?: string;
  /** 仓库里的标签名（imageSource='registry' 时必填）。 */
  registryTag?: string;
  replicas?: number;
  strategy?: 'RollingUpdate' | 'Recreate';
  remark?: string;
  idempotencyKey?: string;
}

/**
 * `POST /deployments`、`POST /services/{id}/redeploy` 的返回。
 *
 * 与 BuildCreated 同理：它不是 DeployRecord，只是"发起者立刻要知道"的那几个字段。
 * 完整步骤与日志由 `GET /deployments/{id}` 给。
 */
export interface DeployCreated {
  id: string;
  deployNo: string;
  envKey: string;
  envName: string;
  projectKey: string;
  /** 本次实际部署的镜像（后端按环境与项目选定的版本，不是前端传的入参回显） */
  image: string;
  replicas: number;
  /** 创建时的状态：queued / progressing / pending_approval —— 不可能是终态 */
  status: DeployStatus;
  logStreamUrl: string;
  createdAt: string;
  /** 仅 status=pending_approval 时下发 */
  approvers?: string[];
}

export const deployApi = {
  list: async (q: DeployQuery = {}): Promise<PageResult<DeployRecord>> =>
    unwrapPage<DeployRecord>(await http.get('/deployments', params(q))),

  get: (id: string) => http.get<DeployRecord>(`/deployments/${id}`),

  create: (body: CreateDeployInput) => http.post<DeployCreated>('/deployments', body),

  /** 待我审批的部署单（正式区且开启审批的环境） */
  pendingApproval: async (): Promise<DeployRecord[]> =>
    unwrapList<DeployRecord>(await http.get('/deployments/pending-approval')),

  approve: (id: string, remark?: string) => http.post<DeployRecord>(`/deployments/${id}/approve`, { remark }),

  reject: (id: string, reason: string) => http.post<DeployRecord>(`/deployments/${id}/reject`, { reason }),

  cancel: (id: string, reason?: string) => http.post<DeployRecord>(`/deployments/${id}/cancel`, { reason }),

  retry: (id: string) => http.post<DeployRecord>(`/deployments/${id}/retry`),

  /**
   * 部署日志（历史）。与构建日志同形：返回**结构化**行 `{seq,level,line,ts}`。
   *
   * 别和 `/deployments/{id}` 详情里的 `logs: string[]` 搞混——那是纯文本正文。
   * 结构化这一路走 `LogLineRecord[]`，可以直接喂给 LogPanel 着色。
   */
  logs: async (id: string): Promise<LogLineRecord[]> =>
    unwrapList<LogLineRecord>(await http.get(`/deployments/${id}/logs`)),

  aiDiagnosis: (id: string) => http.get<DeployDiagnosis>(`/deployments/${id}/ai-diagnosis`),

  stream: (id: string) => streamUrl(`/deployments/${id}/logs/stream`),
};

export interface RollbackQuery {
  envKey?: string;
  projectKey?: string;
  status?: string;
  /** 触发方式：manual（人工）/ ai_suggested（AI 诊断建议）/ auto（发布失败后的自动回滚） */
  triggerType?: 'manual' | 'ai_suggested' | 'auto';
  keyword?: string;
  page?: number;
  pageSize?: number;
}

export interface CreateRollbackInput {
  serviceId: string;
  /** 目标版本：二选一。toImageId 指向镜像记录，toImage 是完整 repo:tag */
  toImageId?: string;
  toImage?: string;
  reason: string;
  triggerType?: 'manual' | 'ai_suggested';
  confidence?: number;
}

/**
 * `POST /rollbacks` 的返回。
 *
 * 路径参数用的是**回滚单 id**（RollbackCreated.id），日志流也从它拿；
 * 而历史日志（rollbackApi.deployLogs）要传 **部署单 id**（deployId）——
 * 回滚的执行实体就是那次部署。这两个 id 混用的坑见 deployLogs 的注释。
 */
export interface RollbackCreated {
  id: string;
  rollbackId: string;
  deployNo: string;
  envKey: string;
  projectKey: string;
  fromImage: string;
  toImage: string;
  status: string;
  logStreamUrl: string;
  createdAt: string;
}

export const rollbackApi = {
  list: async (q: RollbackQuery = {}): Promise<PageResult<RollbackRecord>> =>
    unwrapPage<RollbackRecord>(await http.get('/rollbacks', params(q))),

  get: (id: string) => http.get<RollbackRecord>(`/rollbacks/${id}`),

  create: (body: CreateRollbackInput) => http.post<RollbackCreated>('/rollbacks', body),

  /**
   * 回滚日志流（SSE）。
   * 路径参数是**回滚记录 id**（RollbackRecord.id），不是 deployId——
   * 后端 handler 会用回滚记录上的 deployId 去取日志，前端只需给回滚单自己的 id。
   */
  stream: (rollbackId: string) => streamUrl(`/rollbacks/${rollbackId}/logs/stream`),

  /**
   * 回滚那次部署的历史日志 —— **传部署单 id**（RollbackRecord.deployId），不是回滚单 id。
   *
   * 回滚本身没有 /logs 历史接口（只有 SSE 流），因为回滚的执行实体就是那次部署。
   * 拿回滚单 id 去打 /deployments/{id}/logs 会查不到记录并返回空列表，
   * 这个"看起来像没日志"的静默失败很难排查，所以单独给个名字把语义钉住。
   */
  deployLogs: async (deployId: string): Promise<LogLineRecord[]> => {
    if (!deployId) return [];
    return unwrapList<LogLineRecord>(await http.get(`/deployments/${deployId}/logs`));
  },
};
