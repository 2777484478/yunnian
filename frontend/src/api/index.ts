// ---------------------------------------------------------------------------
// src/api —— 后端接口统一出口
//
// 分两层：
//   - client.ts / shape.ts / sse.ts：传输与形状归一化（不含任何业务语义）
//   - 各业务模块：与后端 handler_*.go 一一对应，页面只 import 这里的模块
//
// 页面不要直接使用 fetch 或 client.request——那会把信封拆解、错误码分支、
// 令牌注入这些事散落到各处，改一次契约要翻遍全仓库。
// ---------------------------------------------------------------------------

export {
  ApiError,
  Code,
  http,
  request,
  streamUrl,
  tokenStore,
  setUnauthorizedHandler,
  API_BASE,
} from './client';

export type { ApiEnvelope } from './client';
export { unwrapList, unwrapPage, params } from './shape';
export { openSSE, subscribeLogStream } from './sse';
export type { SSEFrame, OpenSSEOptions, LogStreamHandle, LogStreamHandlers } from './sse';

export { authApi, userApi, roleApi } from './auth';
export type { UserQuery } from './auth';

export { projectApi, buildApi, imageApi } from './ci';
export { registryApi } from './registry';
export type { RegistryTag, RegistryTags } from './registry';
export type {
  ProjectQuery,
  BuildQuery,
  BuildTriggerInput,
  BuildCreated,
  ImageQuery,
  GitConnectionResult,
} from './ci';

export { environmentApi, serviceApi, deployApi, rollbackApi } from './cd';
export type {
  EnvQuery,
  DeployQuery,
  CreateDeployInput,
  DeployCreated,
  RollbackQuery,
  CreateRollbackInput,
  RollbackCreated,
} from './cd';

export { auditApi } from './audit';
export type { AuditQuery, AuditExportResult } from './audit';

export { systemApi, notifyApi, modelApi, gitlabApi } from './system';
export type {
  ModelQuery,
  ModelTestResult,
  ModelInput,
  GitLabSettings,
  GitLabSettingsInput,
  GitLabTestResult,
} from './system';

export { jenkinsApi, isGitParam } from './jenkins';
export type {
  JenkinsSettings,
  JenkinsSettingsInput,
  JenkinsTestResult,
  JenkinsJob,
  JenkinsJobList,
  JenkinsBuildRef,
  JenkinsTriggerResult,
  JenkinsParamDef,
  JenkinsParamKind,
  JenkinsParamValues,
  JenkinsBuildParam,
} from './jenkins';

export { aiApi } from './ai';
export type { ChatInput, ChatHandlers, ChatAttachment, ConversationsResult, ToolStep } from './ai';

export { clusterApi } from './cluster';
