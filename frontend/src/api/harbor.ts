import { http } from './client';

// ---------------------------------------------------------------------------
// Harbor 接入（/system/integrations/harbor）
//
// ⚠️ 这个集成有**两条互不相通的通道**，类型里刻意把它们分成两个字段：
//
//   baseUrl    registry 组件地址（走 /v2/，OCI Distribution）→ 读 manifest/digest，
//              用来回答"这个 tag 到底存不存在"
//   coreApiUrl Core 管理接口地址（走 /api/v2.0/）→ 读 tag 的**推送时间与推送人**
//
// 它们可能不在同一个地址上：实测目标 Harbor 只暴露了 registry 组件，
// /api/v2.0/* 在 :5000 / :443 / :80 上全是 404。合成一个字段的结果就是
// "某一类信息永远读不到，而界面显示已连通"。
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
  coreProjects: number;
}

export const harborApi = {
  settings: () => http.get<HarborSettings>('/system/integrations/harbor'),
  save: (body: HarborSettingsInput) => http.put<HarborSettings>('/system/integrations/harbor', body),
  test: (body?: HarborTestInput) =>
    http.post<HarborTestResult>('/system/integrations/harbor/test', body ?? {}),
};

export default harborApi;
