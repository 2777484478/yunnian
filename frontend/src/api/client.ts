// ---------------------------------------------------------------------------
// HTTP 客户端：统一请求封装
//
// 后端契约（docs/03）：所有接口返回 { code, message, data, traceId }，code=0 表示成功。
// 本文件负责把这层信封拆掉，让业务代码只拿到 data —— 业务代码里不该出现 `res.code === 0`。
//
// 三件事值得说明：
//   1. token 注入与 401 静默刷新（并发去重，避免十个请求同时刷新打爆后端）；
//   2. 错误一律抛 ApiError（带 code/message/traceId），页面按 code 分支，不解析字符串；
//   3. traceId 保留在错误对象上，排障时可以直接把它交给后端查日志。
// ---------------------------------------------------------------------------

/** 后端约定的业务码。0 成功；其余见 docs/03 附录。 */
export const Code = {
  OK: 0,
  /** 参数校验失败 */
  INVALID_PARAM: 40000,
  /** 未登录 / 令牌失效 */
  UNAUTHORIZED: 40100,
  /**
   * 令牌过期——**要和 40100 一起处理**。
   *
   * 后端刻意把这两个分开：40100 表示"没带令牌或令牌不是本平台签发的"（重登），
   * 40101 表示"令牌签名有效但已过期"（可以用 refreshToken 续期）。
   * 只认 40100 的话，一个正常过期（2 小时后）的会话永远走不到续期分支，
   * 页面会卡在"登录已过期"的报错里，既不刷新也不跳登录页。
   */
  TOKEN_EXPIRED: 40101,
  /** 无权限 */
  FORBIDDEN: 40300,
  /** 资源不存在 */
  NOT_FOUND: 40400,
  /** 状态冲突（如部署中不允许重复发布） */
  CONFLICT: 40900,
} as const;

/** 是否属于"会话不可用"——这两种码都要走一次静默续期 */
const isSessionInvalid = (code: number) =>
  code === Code.UNAUTHORIZED || code === Code.TOKEN_EXPIRED;

export interface ApiEnvelope<T> {
  code: number;
  message: string;
  data: T;
  traceId?: string;
}

/** 业务错误。页面可用 `e instanceof ApiError && e.code === Code.FORBIDDEN` 分支。 */
export class ApiError extends Error {
  readonly code: number;
  readonly traceId?: string;
  readonly status: number;

  constructor(code: number, message: string, opts?: { traceId?: string; status?: number }) {
    super(message);
    this.name = 'ApiError';
    this.code = code;
    this.traceId = opts?.traceId;
    this.status = opts?.status ?? 0;
  }
}

// --------------------------------------------------------------------------- 会话存储
//
// 说明：refreshToken 放 localStorage 只是**一期原型的妥协**——后端把它放在响应体里返回，
// 拿不到 httpOnly Cookie。生产上应改为 Cookie + SameSite，届时只需改本文件。
// 注意 token 只在 client/auth 之间流转，页面组件不直接读它。

const TOKEN_KEY = 'yunnian.token';
const REFRESH_KEY = 'yunnian.refresh';
const USER_KEY = 'yunnian.user';
const REMEMBER_KEY = 'yunnian.remember';

export const tokenStore = {
  get: () => localStorage.getItem(TOKEN_KEY) || '',
  getRefresh: () => localStorage.getItem(REFRESH_KEY) || '',
  set(accessToken: string, refreshToken?: string) {
    localStorage.setItem(TOKEN_KEY, accessToken);
    if (refreshToken) localStorage.setItem(REFRESH_KEY, refreshToken);
  },
  getRemember: () => localStorage.getItem(REMEMBER_KEY) === '1',
  setRemember(v: boolean) {
    if (v) localStorage.setItem(REMEMBER_KEY, '1');
    else localStorage.removeItem(REMEMBER_KEY);
  },
  getUser: (): Record<string, unknown> | null => {
    try {
      const raw = localStorage.getItem(USER_KEY);
      if (!raw) return null;
      const parsed = JSON.parse(raw);
      return parsed && typeof parsed === 'object' ? parsed : null;
    } catch {
      return null;
    }
  },
  setUser: (u: unknown) => localStorage.setItem(USER_KEY, JSON.stringify(u)),
  clear() {
    localStorage.removeItem(TOKEN_KEY);
    localStorage.removeItem(REFRESH_KEY);
    localStorage.removeItem(USER_KEY);
    // 刻意保留 REMEMBER_KEY：记住我是"下次回填表单"的偏好，不是会话本身。
  },
};

// --------------------------------------------------------------------------- 基础配置

/**
 * 接口前缀。默认相对路径 `/api/v1`：
 *   - 开发：vite devServer 的 proxy 转发到后端（见 vite.config.ts）；
 *   - 生产：后端直接托管前端产物，同源，无需 CORS。
 * 需要指向别的后端时用 VITE_API_BASE 覆盖。
 */
export const API_BASE = (import.meta.env?.VITE_API_BASE as string) || '/api/v1';

/** 会话失效时对外广播，由 auth 层挂监听做跳转，client 不直接操作路由 */
type UnauthorizedHandler = () => void;
let onUnauthorized: UnauthorizedHandler | null = null;
export const setUnauthorizedHandler = (fn: UnauthorizedHandler | null) => {
  onUnauthorized = fn;
};

// --------------------------------------------------------------------------- 请求实现

interface RequestOptions {
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';
  /** 查询参数，undefined/null/'' 会被剔除（避免 `?a=&b=` 这种噪声） */
  query?: Record<string, unknown>;
  body?: unknown;
  /** 内部使用：标记这是刷新令牌后的重放，避免无限递归 */
  _retried?: boolean;
  signal?: AbortSignal;
}

function buildUrl(path: string, query?: Record<string, unknown>): string {
  const url = `${API_BASE}${path}`;
  if (!query) return url;
  const parts: string[] = [];
  for (const [k, v] of Object.entries(query)) {
    if (v === undefined || v === null || v === '') continue;
    if (Array.isArray(v)) {
      v.forEach((item) => parts.push(`${encodeURIComponent(k)}=${encodeURIComponent(String(item))}`));
    } else {
      parts.push(`${encodeURIComponent(k)}=${encodeURIComponent(String(v))}`);
    }
  }
  return parts.length ? `${url}?${parts.join('&')}` : url;
}

/** 刷新令牌：同一时刻只允许一个在飞，其余请求排队复用其结果 */
let refreshing: Promise<boolean> | null = null;

async function doRefresh(): Promise<boolean> {
  const refreshToken = tokenStore.getRefresh();
  if (!refreshToken) return false;
  try {
    const res = await fetch(`${API_BASE}/auth/refresh`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ refreshToken }),
    });
    const env = (await res.json()) as ApiEnvelope<{ accessToken: string; refreshToken?: string }>;
    if (env.code !== Code.OK || !env.data?.accessToken) return false;
    tokenStore.set(env.data.accessToken, env.data.refreshToken);
    return true;
  } catch {
    return false;
  }
}

function refreshOnce(): Promise<boolean> {
  if (!refreshing) {
    refreshing = doRefresh().finally(() => {
      refreshing = null;
    });
  }
  return refreshing;
}

/**
 * 手动触发一次静默续期，供**不走 request() 的通道**使用。
 *
 * 目前只有 sse.ts 的 fetch 版 SSE 需要它：那条路径自己发 fetch、自己看响应，
 * 拿不到 request() 里的 40101 分支。不导出的话，页面挂机超过 accessToken 有效期后
 * 点"发送"只能收到一个 401，而 REST 请求却能自动续期——同一会话两种行为，很难排查。
 */
export const refreshAccessToken = refreshOnce;

/**
 * 发起请求并返回信封里的 data。
 * 非 0 的 code 一律抛 ApiError；网络/解析失败也抛 ApiError（code=-1），
 * 这样调用方只需要 catch 一种异常类型。
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { method = 'GET', query, body, signal } = options;
  const headers: Record<string, string> = {};
  const token = tokenStore.get();
  if (token) headers.Authorization = `Bearer ${token}`;
  if (body !== undefined) headers['Content-Type'] = 'application/json';

  let res: Response;
  try {
    res = await fetch(buildUrl(path, query), {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
    });
  } catch (e) {
    // AbortError 原样抛出：调用方用它做竞态取消，不该被当成业务错误提示给用户
    if ((e as Error)?.name === 'AbortError') throw e;
    throw new ApiError(-1, '网络异常，请检查后端服务是否可用');
  }

  let env: ApiEnvelope<T>;
  try {
    env = (await res.json()) as ApiEnvelope<T>;
  } catch {
    throw new ApiError(-1, `响应解析失败（HTTP ${res.status}）`, { status: res.status });
  }

  if (env.code === Code.OK) return env.data;

  // 会话失效：先尝试静默续期，成功则原请求重放一次。
  // `_retried` 保证只重放一次，避免 refresh 本身 401 时无限循环。
  if (isSessionInvalid(env.code) && !options._retried) {
    const ok = await refreshOnce();
    if (ok) return request<T>(path, { ...options, _retried: true });
    // 续期也失败 → 清本地会话并广播回登录页。
    // 只清不跳的话，路由守卫会因为"还有令牌"而继续放行，用户就卡在半死状态。
    tokenStore.clear();
    onUnauthorized?.();
  }

  throw new ApiError(env.code, env.message || '请求失败', {
    traceId: env.traceId,
    status: res.status,
  });
}

export const http = {
  get: <T>(path: string, query?: Record<string, unknown>, signal?: AbortSignal) =>
    request<T>(path, { method: 'GET', query, signal }),
  // 写方法也接受 query：后端有些"危险操作确认"开关走查询串（如 ?confirmEmpty=true），
  // 不是所有参数都在 body 里。
  post: <T>(path: string, body?: unknown, query?: Record<string, unknown>) =>
    request<T>(path, { method: 'POST', body, query }),
  put: <T>(path: string, body?: unknown, query?: Record<string, unknown>) =>
    request<T>(path, { method: 'PUT', body, query }),
  patch: <T>(path: string, body?: unknown, query?: Record<string, unknown>) =>
    request<T>(path, { method: 'PATCH', body, query }),
  del: <T>(path: string, body?: unknown, query?: Record<string, unknown>) =>
    request<T>(path, { method: 'DELETE', body, query }),
};

/**
 * 拼一个带 token 的事件流地址（SSE）。
 *
 * EventSource 不支持自定义请求头，浏览器只会带上 Cookie 与查询串，
 * 所以这里把 token 放进查询参数——后端 middleware.Auth 同时接受 `?token=`。
 * 只用于 GET 流式接口，不要拿它做普通请求（token 会进访问日志）。
 */
export function streamUrl(path: string, query?: Record<string, unknown>): string {
  const url = buildUrl(path, query);
  const token = tokenStore.get();
  if (!token) return url;
  return `${url}${url.includes('?') ? '&' : '?'}token=${encodeURIComponent(token)}`;
}
