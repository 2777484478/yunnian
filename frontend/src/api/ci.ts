import { http, streamUrl } from './client';
import { unwrapList, unwrapPage, params } from './shape';
import type {
  BuildRecord,
  DeployRecord,
  ImageRecord,
  LogLineRecord,
  PageResult,
  Project,
} from '@/types';

// ---------------------------------------------------------------------------
// CI 模块：项目 / 构建 / 镜像（对应后端 handler_ci.go）
// ---------------------------------------------------------------------------

export interface ProjectQuery {
  keyword?: string;
  type?: string;
  team?: string;
  enabled?: boolean;
  page?: number;
  pageSize?: number;
}

/**
 * 测试仓库连接的结果。
 *
 * `message` 是后端把 git 的原始报错翻译过的**人话**（"账号或密码不正确" 而不是
 * `fatal: Authentication failed`），界面直接展示它即可。
 * `detail` 是原始输出尾部，只作折叠展开的定位用。
 */
export interface GitConnectionResult {
  ok: boolean;
  message: string;
  detail?: string;
  /** 分支名，**最多 100 个** —— 用 branchTotal 说"远端共多少个"，别用它的长度。 */
  branches: string[];
  /** 远端**实际**分支数。branches 被截断时靠它说实话。 */
  branchTotal: number;
  defaultBranch?: string;
  elapsedMs: number;
}

export const projectApi = {
  list: async (q: ProjectQuery = {}): Promise<PageResult<Project>> =>
    unwrapPage<Project>(await http.get('/projects', params(q))),

  get: (key: string) => http.get<Project>(`/projects/${key}`),

  create: (body: Partial<Project>) => http.post<Project>('/projects', body),

  update: (key: string, body: Partial<Project>) => http.put<Project>(`/projects/${key}`, body),

  remove: (key: string) => http.del<null>(`/projects/${key}`),

  setEnabled: (key: string, enabled: boolean) =>
    http.patch<Project>(`/projects/${key}/enabled`, { enabled }),

  /** 分支列表（走本地/真实 GitLab 实现，视 integration 配置） */
  branches: async (key: string): Promise<string[]> =>
    unwrapList<string>(await http.get(`/projects/${key}/branches`)),

  /** 校验项目配置（连通性 + 路径），需要 project:write */
  validate: (key: string) =>
    http.post<{ valid: boolean; messages: string[] }>(`/projects/${key}/validate`),

  /**
   * 真去读一次仓库，验证"这个地址 + 这套凭据现在能不能拉到代码"。
   *
   * 不传 gitPassword 时后端回落到已保存的凭据 —— 界面上"改完地址点一下测试"
   * 不该逼用户重敲一遍密码。传了就用传的（覆盖保存）。
   */
  testConnection: (body: {
    projectKey?: string;
    gitUrl?: string;
    gitUsername?: string;
    gitPassword?: string;
    branch?: string;
  }) => http.post<GitConnectionResult>('/projects/test-connection', body),

  /**
   * 取平台内置的默认构建脚本。
   *
   * 由后端下发而不是前端硬编码：默认脚本要和构建机上的镜像地址、注入的变量名
   * 保持一致，前端各留一份副本，迟早会出现"界面给的默认脚本跑不通"。
   */
  defaultBuildScript: async (type: string): Promise<string> => {
    const d = await http.get<{ script?: string }>('/projects/default-build-script', { type });
    return d?.script ?? '';
  },

  builds: async (key: string, q: { status?: string; branch?: string; page?: number; pageSize?: number } = {}) =>
    unwrapPage<BuildRecord>(await http.get(`/projects/${key}/builds`, params(q))),

  images: async (key: string, q: { env?: string; page?: number; pageSize?: number } = {}) =>
    unwrapPage<ImageRecord>(await http.get(`/projects/${key}/images`, params(q))),

  deployments: async (key: string, q: { env?: string; status?: string; page?: number; pageSize?: number } = {}) =>
    unwrapPage<DeployRecord>(await http.get(`/projects/${key}/deployments`, params(q))),
};

export interface BuildTriggerInput {
  projectKey: string;
  branch?: string;
  /** 构建参数覆盖，键名与项目 buildParams 对齐 */
  params?: Record<string, string>;
  /**
   * 幂等键：同一次用户点击必须复用同一个值。
   * 重复提交时后端返回 40902 并带上已有构建单，页面据此提示"已有相同构建"。
   */
  idempotencyKey?: string;
  commitId?: string;
  commitMsg?: string;
  triggerType?: 'manual' | 'webhook' | 'schedule' | 'retry';
}

export interface BuildQuery {
  projectKey?: string;
  projectType?: 'frontend' | 'backend' | string;
  /** 支持逗号分隔的多值，如 `running,queued`（后端已支持，单值同样合法） */
  status?: string;
  branch?: string;
  keyword?: string;
  page?: number;
  pageSize?: number;
  sortBy?: string;
  order?: 'asc' | 'desc';
}

/**
 * `POST /builds`（触发）的返回。
 *
 * 它**不是** BuildRecord：后端刻意只回"发起者需要立刻知道"的字段
 * （单号、分支、状态、日志流地址）。早期把它声明成 BuildRecord 是个谎——
 * 页面写 `created.imageSize` 会拿到 undefined，而且不会报错。
 * 要完整记录（镜像、耗时、日志）请再 `GET /builds/{id}`。
 */
export interface BuildCreated {
  id: string;
  buildNo: number;
  projectKey: string;
  branch: string;
  commitId: string;
  status: string;
  stage: string;
  progress: number;
  /** 相对路径（不是完整 URL）：要订阅请用 buildApi.stream(id) 拼令牌 */
  logStreamUrl: string;
  startedAt: string;
}

/**
 * 构建失败归因结果（`GET /builds/{id}/ai-diagnosis`）。
 *
 * `source` 必须按原样展示：用户有权知道这段结论是模型读了日志说的，
 * 还是平台错误码查表查出来的。把规则结果说成"模型置信度 92%"，
 * 会让人以为日志已经被看过一遍。
 */
export interface BuildDiagnosis {
  source: 'model' | 'rule';
  /** source=model 时的模型通道名（如 qwen-plus） */
  model?: string;
  /** 需要如实告知的附加说明，如"模型这次为什么没参与" */
  notice?: string;
  reason: string;
  suggestion: string;
  confidence: number;
  errorCode?: string;
}

export const buildApi = {
  list: async (q: BuildQuery = {}): Promise<PageResult<BuildRecord>> =>
    unwrapPage<BuildRecord>(await http.get('/builds', params(q))),

  get: (id: string) => http.get<BuildRecord>(`/builds/${id}`),

  trigger: (body: BuildTriggerInput) => http.post<BuildCreated>('/builds', body),

  /** 取消只回 `{canceled:true}`——没有"取消后的构建单"这回事 */
  cancel: (id: string) => http.post<{ canceled: boolean }>(`/builds/${id}/cancel`),

  // 这里刻意没有 retry：平台不会替 Jenkins 项目重新构建（后端会对这类项目返回
  // 40901"构建方式为 Jenkins Job，平台不会代为执行 docker 构建"），而一期纳管的
  // 三个项目全部是 Jenkins。留一个必然失败的入口就是假承诺，所以入口已整体移除。
  // 后端 `POST /builds/{id}/retry` 仍在（对平台构建项目有效），将来要放开再说。

  /**
   * 历史日志（一次性拉全量，用于已完成/失败构建）。
   *
   * 与详情接口的 `logs` 不同——那边是 `string[]`（只有正文），
   * 这里返回**结构化行** `{seq, level, line, ts}`，可直接喂给 LogPanel 着色。
   * 后端默认 limit=1000（上限 5000），afterSeq 用于增量续拉。
   */
  logs: async (id: string, q: { afterSeq?: number; limit?: number } = {}): Promise<LogLineRecord[]> =>
    unwrapList<LogLineRecord>(await http.get(`/builds/${id}/logs`, params({ limit: 1000, ...q }))),

  aiDiagnosis: (id: string) => http.get<BuildDiagnosis>(`/builds/${id}/ai-diagnosis`),

  /**
   * 实时日志流地址（SSE）。
   * 用 streamUrl 而不是 http——EventSource 不能自定义请求头，令牌走查询串（后端已支持）。
   */
  stream: (id: string) => streamUrl(`/builds/${id}/logs/stream`),
};

export interface ImageQuery {
  projectKey?: string;
  env?: string;
  keyword?: string;
  page?: number;
  pageSize?: number;
}

export const imageApi = {
  list: async (q: ImageQuery = {}): Promise<PageResult<ImageRecord>> =>
    unwrapPage<ImageRecord>(await http.get('/images', params(q))),

  get: (id: string) => http.get<ImageRecord>(`/images/${id}`),

  /**
   * 该镜像被哪些环境的服务实例使用（删除前的引用检查）。
   * 注意后端返回的是**对象** `{imageId,inUse,services}` 而不是列表——
   * 用 unwrapList 会拿到空数组，从而误判"没人用"。
   */
  usedBy: (id: string) =>
    http.get<{
      imageId: string;
      inUse: boolean;
      services: { envKey: string; service: string }[];
    }>(`/images/${id}/used-by`),

  remove: (id: string) => http.del<null>(`/images/${id}`),

  /** 批量清理（按策略），需要 image:delete */
  cleanup: (body: { projectKey?: string; keepLatest?: number; beforeDays?: number; dryRun?: boolean }) =>
    http.post<{ deleted: number; freed: number; items?: string[] }>('/images/cleanup', body),
};
