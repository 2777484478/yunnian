import { http } from './client';

// ---------------------------------------------------------------------------
// Jenkins 接入（已接通真实后端）
//
// 与 GitLab 接入的三点差异，直接决定了下面的类型形状：
//
//   1. **多一个「用户名」**。Jenkins 走 HTTP Basic 认证，要「用户名 + 密钥」两个值一起
//      提交，只给密钥无法完成认证。这里的"密钥"**既可以是 API Token，也可以是账号密码**
//      ——Basic 认证本身两种都收。平台推荐令牌（可单独吊销、不受改密码/双因素影响），
//      但不阻拦密码：实测目标 Jenkins（2.150.2）用账号密码即可 200。
//      所以入参比 GitLab 多一项，且这一项是"用户名+密钥"的组合而非单令牌。
//
//   2. **job 有文件夹嵌套**。用了 Folders 插件（几乎必用）之后，job 的唯一标识是
//      `fullName`（如 `backend/pay/ci-payment`），不是裸名。拿裸名去触发会 404，
//      而 `/api/json` 默认又只返回顶层——这是拉列表最容易翻车的地方。
//
//   3. **触发构建拿不到构建号**。`POST /job/{x}/build` 只返回 201 和一个队列项地址
//      （`queue/item/{id}`），构建号要再轮询一次队列项才拿得到。所以触发接口
//      返回的是 `queueId` 而不是 `buildNumber`，别指望一次拿到。
//
//   4. **"选版本号"那个下拉不是 job 的属性，要单独问 Jenkins**。参数化 job 触发时
//      走 `buildWithParameters`；参数**定义**在 `property[parameterDefinitions]` 里，
//      但 GitParameter 的**可选值（分支/标签列表）不在定义里**，要再调一次
//      `descriptorByName/<GitParameterDefinition>/fillValueItems`。而那个接口
//      每次都会去 `git ls-remote` 真读一遍仓库（实测 0.6s 起）。
//      → 结论：**只能按需拉（点开某个 job 的构建弹窗时），绝不能进列表页批量拉**。
//      531 个 job 若每个都拉一次，就是 5 分钟起步。
//
// 演示回退已移除（后端 7 个接口全部落地）。**不要再把它加回来**：
// 原来的 `withDemoFallback` 把 404 当作"后端还没实现"，于是真实错误
// （例如 Job 已被删除时后端返回的 40400）会被静默替换成一屏演示数据，
// 用户看到的是"一切正常"，而实际上什么都没拉到。
// 宁可显示一条错误，也不要显示一份假的成功。
// ---------------------------------------------------------------------------

// --------------------------------------------------------------------------- 连接配置

export interface JenkinsSettings {
  enabled: boolean;
  baseUrl: string;
  /** HTTP Basic 的用户名。与密钥一起提交，缺一不可。 */
  username: string;
  timeout: number;
  hasToken: boolean;
  /** 形如 ****3456 的掩码，仅用于确认"填的是哪一把"。 */
  tokenMasked: string;
  /** db = 界面保存过；config = 尚未保存，用的是 configs/config.yaml 的值。 */
  source: string;
  /** 上次测试识别到的账号名（Jenkins 侧显示名）。 */
  identity: string;
  updatedAt: string;
  /** 当前实际干活的实现：jenkins / local。与 enabled 是两回事。 */
  impl: string;
  /** 是否开启了 CSRF 保护。开启则触发构建必须先取 crumb，平台会自动处理。 */
  crumbEnabled: boolean;
  /** 由地址派生的"用户设置 › API Token"页面。 */
  tokenPageUrl: string;
}

/** 保存入参。token 留空 = 不修改；clearToken = 显式删除。二者互斥。 */
export interface JenkinsSettingsInput {
  enabled: boolean;
  baseUrl: string;
  username: string;
  token?: string;
  clearToken?: boolean;
  timeout?: number;
}

export interface JenkinsTestResult {
  ok: boolean;
  message: string;
  latency: number;
  /** Jenkins 版本（取自 /api/json 的 X-Jenkins 头）。 */
  version: string;
  /** 令牌对应的账号名。 */
  username: string;
  /** 账号在 Jenkins 里的权限点，用于判断"能连上但没权限拉 job"。 */
  permissions: string[] | null;
  crumbEnabled: boolean;
  /** 探测到的 job 总数（含文件夹内），用于确认权限是否足够。 */
  jobCount: number;
  warnings: string[] | null;
  usedSavedValues: boolean;
}

// --------------------------------------------------------------------------- Job 列表

/** Jenkins 里一次构建的引用。 */
export interface JenkinsBuildRef {
  number: number;
  /** null = 还在跑（Jenkins 只有构建结束后才填 result）。 */
  result: string | null;
  building: boolean;
  timestamp: number;
  duration: number;
  url: string;
}

export interface JenkinsJob {
  /** 触发时使用的完整名，含文件夹前缀，如 `backend/pay/ci-payment`。 */
  fullName: string;
  /** 展示名（不含文件夹）。 */
  name: string;
  /** 所在文件夹路径；顶层 job 为空串。 */
  folder: string;
  /**
   * Jenkins 的 job 状态色（运维习惯的语义）：
   * blue 成功 / red 失败 / yellow 不稳定 / grey 未构建 / notbuilt，
   * 后缀 `_anime` 表示正在构建（如 `blue_anime`）。
   */
  color: string;
  url: string;
  /** 是否参数化 job。参数化的要用 buildWithParameters，否则用 build。 */
  parameterized: boolean;
  /**
   * 参数名列表（仅名字，**不含可选值**）。
   *
   * 列表页只用来显示"有哪几个参数"，不用它渲染表单：可选值要另外去仓库读，
   * 531 个 job 全读一遍是不可能的。真正的参数定义 + 可选值在点开
   * 构建弹窗时才拉（见 `params` / `paramValues`）。
   * 拉列表时顺手带上名字几乎没有开销（纯配置，不碰 git）。
   */
  paramNames?: string[];
  /**
   * 下一次构建的编号。触发前告知"这将产生 #4462"，比触发后再去查更符合直觉，
   * 也和 Jenkins 页面上的标题一致。注意它只是**预期值**：并发触发时以实际为准。
   */
  nextBuildNumber?: number;
  lastBuild?: JenkinsBuildRef | null;
  /**
   * 已绑定到哪个平台项目（未绑定为 null）。
   * 注意：这一项由**平台侧**补，不是 Jenkins 的字段——Jenkins 不知道平台的存在。
   */
  boundProjectKey: string | null;
}

export interface JenkinsJobList {
  jobs: JenkinsJob[];
  /**
   * 后端固定返回 `real`（这些数据真的来自 Jenkins）。
   * 保留 `demo` 是为了让契约稳定——但接口层已经不回退演示数据了，
   * 见文件头"演示回退已移除"。
   */
  source: 'real' | 'demo';
  fetchedAt: string;
  /** 是否因超出上限被截断（job 很多时）。 */
  truncated: boolean;
  /** 因权限不足被跳过的文件夹数量，>0 时要在界面上提示。 */
  skippedFolders: number;
}

// --------------------------------------------------------------------------- 构建参数

/**
 * 归一化后的参数类型。
 *
 * **为什么不直接用 Jenkins 返回的 Java 类名**：类名是插件实现细节
 * （`net.uaznia.lukanus.hudson.plugins.gitparameter.GitParameterDefinition`），
 * 换个插件版本就可能变。后端负责把类名归一化成下面这组稳定值，前端只认这些，
 * 这样插件升级不会把前端打散。
 *
 * `git_*` 这 6 种的选择项**不在参数定义里**，要再调一次 `paramValues` 去仓库读；
 * 其余类型的选择项是静态的，随定义一起返回（`choices`）。
 */
export type JenkinsParamKind =
  /** 分支 + 标签都列（GitParameter 的 PT_BRANCH_TAG，实测最常见） */
  | 'git_branch_tag'
  /** 只列分支 */
  | 'git_branch'
  /** 只列标签——"选版本号"通常就是这种 */
  | 'git_tag'
  | 'git_revision'
  | 'git_pull_request'
  | 'git_build'
  /** 预置的固定选项 */
  | 'choice'
  | 'boolean'
  | 'string'
  | 'password'
  | 'text'
  /** 平台还不认识的类型：**只读展示，不允许提交**——猜着渲染成文本框会送错值。 */
  | 'unknown';

/** 是否属于"可选值要去仓库读"的类型 */
export const isGitParam = (k: JenkinsParamKind) => k.startsWith('git_');

export interface JenkinsParamDef {
  name: string;
  kind: JenkinsParamKind;
  /** 原始 Java 类名。排障用，界面不展示。 */
  className: string;
  /** 参数说明。GitParameter 常把"默认用哪个分支"写在这里（如 `默认master`）。 */
  description: string;
  /**
   * 默认值。
   *
   * ⚠️ **GitParameter 这一项恒为 null**：老版本插件不通过 API 暴露配置页里的
   * defaultValue（用 `tree=*` 拿全字段也只有 `defaultParameterValue: null`）。
   * 真实默认值只出现在 `description` 的文字里。所以界面**不要假装知道默认值**，
   * 统一按"不选则不传该参数 → Jenkins 用自己配置的默认值"处理。
   */
  defaultValue: string | null;
  /** 静态选项（kind=choice 时才有）。 */
  choices: string[] | null;
  /** 是否多选。GitParameter 可配多选，多选时同名参数提交多次。 */
  multiple: boolean;
}

/**
 * GitParameter 的可选值。
 *
 * **tag 与分支分开返回**是刻意的：实测 `镜像打包-demoitms` 共 311 个值，
 * 其中 84 个是 `v_aiops_1.16.0109` 这类版本标签，另外 227 个是
 * `origin/Release_aiops_3.8.3_1.52` 这类**远端分支引用**。
 * 混成一个列表的话，"选版本号"要在 311 条里翻，而找版本号的人根本不想看分支。
 * 分成两组、默认展开标签组，才符合这个界面的用途。
 *
 * ⚠️ Jenkins 这一侧有个**静默失败**的坑，后端已按 `param=<参数名>` 实现并兜住：
 * 查询串写成 `name=<参数名>` 时 Jenkins 返回 **HTTP 200 + `values: []`**——
 * 不报错、不警告，就是空的；参数名根本不存在时也是同样表现。
 * 所以后端会先读一次参数定义，参数名对不上就**主动回 40400**，
 * 于是这份响应里"一个值都没有"只剩一种含义：仓库里确实没有匹配的引用。
 */
export interface JenkinsParamValues {
  param: string;
  /** 版本标签（不含 `origin/` 前缀的纯引用），构建镜像时最常用。 */
  tags: string[];
  /** 分支引用（`origin/xxx` 这类）。 */
  branches: string[];
  /** 既不像标签也不像分支的原值，兜底展示，避免**静默丢值**。 */
  other: string[];
  /** 总条数 = tags + branches + other。 */
  total: number;
  /**
   * Jenkins 报出的错误。注意：**参数名写错时这个也是空的**，
   * 所以"空值"不能靠 errors 判断（后端已用另一种方式兜住，见接口注释）。
   */
  errors: string[];
  fetchedAt: string;
  /** 拉取耗时（毫秒）。这个接口每次真的去读仓库，耗时波动大，值得显示。 */
  elapsedMs: number;
  /** 后端固定返回 `real`。理由同 `JenkinsJobList.source`。 */
  source: 'real' | 'demo';
}

/** 提交给 Jenkins 的一个参数。 */
export interface JenkinsBuildParam {
  name: string;
  value: string;
}

/** 触发构建的结果。注意这里**没有构建号**——只有队列项。 */
export interface JenkinsTriggerResult {
  /** 队列项 id，用它换构建号。 */
  queueId: number;
  fullName: string;
  /** 平台据此建单的构建记录 id（后端接好后返回）。 */
  buildId: number;
  /**
   * 平台构建单号。批量提交后要在任务列表里显示 `项目#单号`，所以后端一并下发。
   * 注意它不是 Jenkins 的 buildNumber——那个要等执行器空闲才出现。
   */
  buildNo: number;
  /**
   * 平台**这条构建记录**的状态（queued / running …），不是 Jenkins 侧的状态。
   *
   * 批量提交时构建池可能已满，后端会把任务排队；面板要据此显示"排队中"，
   * 不能一律显示"执行中"——那会让用户以为已经在跑了。
   */
  status: string;
  /** 与 status 配套的阶段说明，可直接展示。 */
  stage: string;
  message: string;
  /** 回显本次提交的参数，便于在通知里写清"用哪个版本构建的"。 */
  parameters?: JenkinsBuildParam[];
}

export const jenkinsApi = {
  /** 读取当前配置。需要 user:manage——响应里带凭据元信息。 */
  settings: () => http.get<JenkinsSettings>('/system/integrations/jenkins'),

  /** 保存。token 留空表示保持原凭据不变；clearToken 显式删除。二者互斥。 */
  save: (body: JenkinsSettingsInput) =>
    http.put<JenkinsSettings>('/system/integrations/jenkins', body),

  /**
   * 测试连接。
   *
   * 不传参表示"用库里已保存的配置测"（后端会回填 baseUrl/用户名/凭据）；
   * 传参则测表单里尚未保存的值。后端**连不上时返回 200 + ok=false**，
   * 不是错误码——所以调用方必须看 `ok`，不能靠 catch 判断成败。
   */
  test: (body?: { baseUrl?: string; username?: string; token?: string; timeout?: number }) =>
    http.post<JenkinsTestResult>('/system/integrations/jenkins/test', body ?? {}),

  /** 拉取 job 列表（含文件夹内）。带 refresh 时后端会跳过缓存。 */
  jobs: (opts?: { refresh?: boolean }) =>
    http.get<JenkinsJobList>('/system/integrations/jenkins/jobs', opts?.refresh ? { refresh: '1' } : undefined),

  /**
   * 读取某个 Job 的参数定义（**不含 GitParameter 的可选值**）。
   *
   * 轻量：纯读 job 配置，不碰仓库，适合构建弹窗一打开就调。
   * 可选值要另外调 `paramValues`——那个会真去读仓库，不能随便调。
   *
   * Job 不存在时后端返回 40400（不是空数组），文案已说明"可能已被删除或改名"。
   */
  params: (fullName: string) =>
    http.get<JenkinsParamDef[]>('/system/integrations/jenkins/params', { fullName }),

  /**
   * 读取 GitParameter 的可选值（版本标签 / 分支）。
   *
   * ⚠️ 后端实现必须用 `param=<参数名>`，用 `name=<参数名>` 会**静默**返回空数组
   *    （实测 HTTP 200 + `values: []`，不报错，极难排查）。后端已按 param 实现，
   *    并且会在参数名不存在时主动返回 40400。
   * ⚠️ 这个接口**每次都会去仓库拉一遍**（实测 0.6~1.1s，且看不出有缓存），
   *    所以只在用户点开构建弹窗、且该 job 确实有 git 参数时才调。
   *    531 个 job 批量调会直接把这台 Jenkins 拖慢。
   */
  paramValues: (fullName: string, param: string) =>
    http.get<JenkinsParamValues>('/system/integrations/jenkins/param-values', { fullName, param }),

  /**
   * 触发一次构建。
   *
   * ⚠️ 返回的是**队列项**不是构建号：Jenkins 接受请求后只给 `queue/item/{id}`，
   * 真正的构建号要等执行器空闲、job 真正开跑后才出现。平台的后台任务会把
   * buildNumber 补齐并写进构建记录，前端不需要关心这一步。
   *
   * ⚠️ 后端只受理**已绑定平台项目**的 Job：未绑定会返回 40901。
   *    按钮的禁用条件必须与它一致，否则就是"勾得动、发不出"。
   *
   * `parameters` **只传用户明确选择过的**参数。这不是偷懒，是正确性：
   * GitParameter 的真实默认值平台读不到（见 `JenkinsParamDef.defaultValue`），
   * 不传 → Jenkins 用它自己配置的默认值，与在 Jenkins 页面上直接点 Build 完全一致；
   * 传一个平台猜的默认值过去 → 才可能真的构建错版本。
   *
   * 幂等：配合 `idempotencyKey` 使用时，同一个键重复提交返回已有构建记录
   * （`message` 里会写明"没有重复排队"），不会再排一次队。
   */
  trigger: (fullName: string, parameters: JenkinsBuildParam[] = [], idempotencyKey?: string) =>
    http.post<JenkinsTriggerResult>('/system/integrations/jenkins/trigger', {
      fullName,
      parameters,
      idempotencyKey,
    }),
};

