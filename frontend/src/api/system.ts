import { http } from './client';
import { unwrapList, params } from './shape';
import type {
  IntegrationStatus,
  ModelProvider,
  NotifyChannel,
  SystemConfigItem,
  SystemStats,
} from '@/types';

// ---------------------------------------------------------------------------
// 系统管理：统计 / 集成状态 / 平台配置 / 通知渠道 / 模型通道
// （对应后端 handler_system.go）
// ---------------------------------------------------------------------------

export const systemApi = {
  stats: () => http.get<SystemStats>('/system/stats'),

  /** 各下游集成（Harbor/GitLab/K8s）的实现方式与就绪状态 */
  integrations: async (): Promise<IntegrationStatus[]> =>
    unwrapList<IntegrationStatus>(await http.get('/system/integrations')),

  config: async (group?: string): Promise<SystemConfigItem[]> =>
    unwrapList<SystemConfigItem>(await http.get('/system/config', params({ group }))),

  /**
   * 更新平台参数（覆盖式，但只需传要改的项）。
   *
   * ⚠️ **必须把 `valueType` 一起带上**。后端 `validateConfigValue` 用的是
   * **请求体里的 valueType**，不是库中该行的类型：
   *   - 带上 `number` → 非数字会被拒（"应为数字，收到 \"abc\""）；
   *   - 不带（空串）→ switch 直接落空，**任何值都通过**。
   * 也就是说漏传 valueType 不会报错，只会让服务端校验静默失效。
   * 这里把它列为必填，正是为了防止"提交成功但校验被绕过"。
   */
  updateConfig: (items: { configKey: string; configValue: string; valueType: string }[]) =>
    http.put<SystemConfigItem[]>('/system/config', { items }),
};

// ---------------------------------------------------------------------------
// GitLab 接入配置（后端 handler_system.go § GitLab 接入配置）
//
// 这几个接口解决一个具体问题：GitLab 的地址与令牌原先只能写进
// configs/config.yaml，改一次要重启一次平台；而"平台读不到仓库"
// （列分支、校验 Dockerfile、取提交号）恰恰是配项目时最先撞上的。
//
// 三条与页面写法直接相关的约定：
//   1. **令牌只进不出**。读取永远只回 `tokenMasked`，明文没有出口；
//      因此保存时 `token` 留空表示"不修改"，而不是"清空"。
//   2. **清空要显式**。想删掉已存令牌必须传 `clearToken: true`，
//      否则任何一次只想改地址的保存都会顺手删掉凭据。
//   3. **`ok=false` 不是请求失败**。测试连接永远返回 200，
//      通不通看 `ok` 与 `message`——不能只看有没有抛异常。
// ---------------------------------------------------------------------------

/** 当前生效的 GitLab 连接配置（不含令牌明文）。 */
export interface GitLabSettings {
  enabled: boolean;
  baseUrl: string;
  timeout: number;
  hasToken: boolean;
  /** 形如 glp****************a3f9，仅用于确认"填的是哪一把"。 */
  tokenMasked: string;
  /**
   * 这份配置来自哪里：`db` = 界面上保存过；`config` = 尚未保存，
   * 用的是 configs/config.yaml 的值。页面上必须显示出来，
   * 否则运维会对着配置文件改半天，其实值早被界面覆盖了。
   */
  source: string;
  /** 上次测试识别到的令牌账号（可能为空：还没测过）。 */
  identity: string;
  /** 令牌自身声明的到期日（可能为空）。 */
  tokenExpiresAt: string;
  updatedAt: string;
  /** 当前实际干活的实现：gitlab / local。与 enabled 是两回事。 */
  impl: string;
  /** 由地址派生的"新建访问令牌"页面，地址为空时为空串。 */
  tokenPageUrl: string;
}

/** 保存入参。token 留空 = 不修改；clearToken = 显式删除。二者互斥。 */
export interface GitLabSettingsInput {
  enabled: boolean;
  baseUrl: string;
  token?: string;
  clearToken?: boolean;
  timeout?: number;
}

/** 连通性测试结果。 */
export interface GitLabTestResult {
  ok: boolean;
  message: string;
  latency: number;
  version: string;
  username: string;
  name: string;
  isAdmin: boolean;
  /** 老版本 GitLab 读不到令牌元数据时是 null（不是空数组）。 */
  scopes: string[] | null;
  expiresAt: string;
  /** 本次测的是库里那份（true）还是表单里还没保存的值（false）。 */
  usedSavedValues: boolean;
  /**
   * "现在能连上、但迟早出事"的提醒：作用域不足、令牌即将到期。
   * 这两类问题不表现为连接失败，而是过一阵子突然全挂，必须原样展示。
   */
  warnings: string[] | null;
}

export const gitlabApi = {
  /** 读取当前配置。需要 user:manage——响应里带凭据元信息，不是普通状态。 */
  settings: () => http.get<GitLabSettings>('/system/integrations/gitlab'),

  /** 保存。token 留空表示保持原令牌不变（见上方约定 1、2）。 */
  save: (body: GitLabSettingsInput) =>
    http.put<GitLabSettings>('/system/integrations/gitlab', body),

  /**
   * 测试连接。不带参数表示测已保存的配置；
   * 带上 baseUrl/token 则测表单里还没保存的值，支持"先测再存"。
   */
  test: (body?: { baseUrl?: string; token?: string; timeout?: number }) =>
    http.post<GitLabTestResult>('/system/integrations/gitlab/test', body ?? {}),
};

export const notifyApi = {
  list: async (): Promise<NotifyChannel[]> => unwrapList<NotifyChannel>(await http.get('/notify-channels')),

  create: (body: Partial<NotifyChannel>) => http.post<NotifyChannel>('/notify-channels', body),

  update: (id: string, body: Partial<NotifyChannel>) =>
    http.put<NotifyChannel>(`/notify-channels/${id}`, body),

  remove: (id: string) => http.del<null>(`/notify-channels/${id}`),

  /**
   * 发送一条测试消息。
   *
   * ⚠️ 后端当前只做**可达性预检**（目标格式/地址是否可解析），并不真的投递：
   * 投递需要邮件网关 / 钉钉机器人 / 企微应用的具体凭据，平台还没拿到。
   * 所以返回 `ok=false` 是常态，message 里会写明"配置格式校验通过，但真实投递尚未接入"。
   * 页面必须原样展示这句话，不要包装成"测试成功"。
   */
  test: (id: string) => http.post<{ ok: boolean; message: string }>(`/notify-channels/${id}/test`),

  setEnabled: (id: string, enabled: boolean) =>
    http.patch<NotifyChannel>(`/notify-channels/${id}/enabled`, { enabled }),
};

export interface ModelQuery {
  keyword?: string;
  vendor?: string;
  enabled?: boolean;
}

/**
 * 连通性测试结果（后端 service.ModelTestResult）。
 *
 * 注意字段名是 `latency` 而不是 `latencyMs`：后端直接复用 DTO 序列化，
 * 写错字段名不会报错，只会让耗时永远显示不出来。
 * 另外 `ok=false` 也是**正常响应**（HTTP 200）——表示"通了但模型侧不通"，
 * 页面必须读 message，不能只看请求有没有抛异常。
 */
export interface ModelTestResult {
  providerId: string;
  name: string;
  ok: boolean;
  message: string;
  latency: number;
}

/**
 * 模型通道写入入参（对应后端 service.ModelInput）。
 *
 * ⚠️ `PUT /models/{id}` 是**全量语义**，不是 PATCH：后端对 name / vendor / baseUrl
 * 用了 `binding:"required"`，只传 `{enabled:false}` 会直接 40000
 * （实测报错：`name`、`vendor`、`base_url` 该字段必填）。
 * 所以改任何一个字段都要把这三个必填项一起带上——页面里统一由 `toModelBody()`
 * 生成完整 body，不要手写"只传变化字段"的对象。
 *
 * `apiKey` 留空（或不传）表示**保留原密钥**：接口只回掩码，前端拿不到明文，
 * 若每次保存都补一个空串，就等于把已有密钥清掉。
 */
export interface ModelInput {
  name: string;
  vendor: string;
  baseUrl: string;
  apiKey?: string;
  models?: string[];
  defaultModel?: string;
  enabled?: boolean;
  isDefault?: boolean;
  timeout?: number;
  maxConcurrency?: number;
  temperature?: number;
  maxTokens?: number;
  desc?: string;
}

export const modelApi = {
  // 路径同时注册了 /models 与 /model-providers，用前者与前端导航一致
  list: async (q: ModelQuery = {}): Promise<ModelProvider[]> =>
    unwrapList<ModelProvider>(await http.get('/models', params(q))),

  create: (body: ModelInput) => http.post<ModelProvider>('/models', body),

  /** 全量更新，务必传完整的 name/vendor/baseUrl（见 ModelInput 注释） */
  update: (id: string, body: ModelInput) => http.put<ModelProvider>(`/models/${id}`, body),

  remove: (id: string) => http.del<null>(`/models/${id}`),

  /** 连通性测试：后端会真的发一次最小请求，返回耗时 */
  test: (id: string) => http.post<ModelTestResult>(`/models/${id}/test`),

  setDefault: (id: string) => http.post<ModelProvider>(`/models/${id}/default`),
};
