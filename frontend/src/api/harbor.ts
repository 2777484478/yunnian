import { http } from './client';

// ---------------------------------------------------------------------------
// Harbor 接入（/system/integrations/harbor）
//
// ⚠️ 这个集成有**两条互不相通的通道**，类型里刻意把它们分成两个字段：
//
//   baseUrl    registry 组件地址（走 /v2/，OCI Distribution）→ 读 manifest/digest，
//              用来回答"这个 tag 到底存不存在"
//   coreApiUrl Core 管理接口地址 → 读 tag 列表（v2 还带推送时间）
//
// 两者**通常就在同一个入口上**（Harbor v2 一个端口同时挂 /v2/ 与 /api/v2.0/；
// 实测那台 Harbor v1.1.2 的 /v2/ 与 /api/ 也都在 :5000），所以 coreApiUrl **留空即可**
// —— 平台按 baseUrl 探。它只是给"registry 走独立端口/镜像站"的部署留的覆盖项。
//
// 至于接口路径（v1 是 /api/、v2 是 /api/v2.0/）由平台按版本自动识别，
// 这里只填**入口地址**，不要带 /api 或 /api/v2.0 后缀。
// ---------------------------------------------------------------------------

/** Harbor 接入设置。口令只回 hasPassword，明文没有读出口。 */
export interface HarborSettings {
  enabled: boolean;
  baseUrl: string;
  coreApiUrl: string;
  /** 镜像地址里出现的仓库部分（如 es-repo.example.com:5000），**不能带 http:// 前缀**。 */
  registry: string;
  username: string;
  hasPassword: boolean;
  insecure: boolean;
  timeout: number;
  /** 当前实际干活的实现（harbor-registry-v2 / local）。 */
  impl: string;
  /** 这份值来自 db 还是 config.yaml —— 让用户知道自己在改哪一份。 */
  source: 'db' | 'config';
  /** 上次连通性测试识别到的账号。 */
  identity: string;
  updatedAt: string;
}

/** 保存入参。password 空 = 不修改；要清空必须显式 clearPassword。 */
export interface HarborSettingsInput {
  enabled: boolean;
  baseUrl: string;
  coreApiUrl: string;
  registry: string;
  username: string;
  password?: string;
  clearPassword?: boolean;
  insecure: boolean;
  timeout: number;
}

/** 测试入参：全空 = 测库里已保存的那份；填了哪些就用哪些（支持先测再存）。 */
export interface HarborTestInput {
  baseUrl?: string;
  coreApiUrl?: string;
  registry?: string;
  username?: string;
  password?: string;
  insecure?: boolean;
  timeout?: number;
}

/** 测试结果：两条通道**各自的**结论 + 一句总述。ok=false 也是 200 响应。 */
export interface HarborTestResult {
  ok: boolean;
  message: string;
  latency: number;
  usedSavedValues: boolean;
  registryOk: boolean;
  registryMessage: string;
  coreOk: boolean;
  coreMessage: string;
  coreVersion: string;
  /** 探测到的 Core 接口版本："v1" / "v2"（v1 的能力与 v2 不同）。 */
  coreApiVersion: string;
  /** 这台 Harbor 给不出 tag 推送时间（v1 就是）—— 界面必须据此改措辞。 */
  coreNoPushTime: boolean;
  coreProjects: number;
  /** 可见的镜像仓库数（只有 v1 匿名探测给得出）。 */
  coreRepos: number;
}

export const harborApi = {
  settings: () => http.get<HarborSettings>('/system/integrations/harbor'),
  save: (body: HarborSettingsInput) => http.put<HarborSettings>('/system/integrations/harbor', body),
  test: (body?: HarborTestInput) =>
    http.post<HarborTestResult>('/system/integrations/harbor/test', body ?? {}),
};

export default harborApi;
