import type { UserRole } from '@/types';
import { authApi, setUnauthorizedHandler, tokenStore } from '@/api';

// ---------------------------------------------------------------------------
// 前端会话门面
//
// 分层：本文件不直接调 fetch，一切经 src/api/auth.ts；它只负责
// 「把会话状态以**同步**方式暴露给路由守卫」——React 路由的守卫必须在首帧
// 就知道有没有登录，否则会先闪一下登录页再跳回来。
// 因此用户信息在 localStorage 里留一份缓存，令牌有效性最终仍由后端裁决
// （任一次请求返回 40100 且续期失败 → 清会话并广播回登录页）。
// ---------------------------------------------------------------------------

/** 会话用户。字段名与页面既有引用保持一致（roleName 等不能改名）。 */
export interface SessionUser {
  id: string;
  username: string;
  name: string;
  role: UserRole;
  roleName: string;
  /** 权限点，含通配形态（`deploy:*`、`*:read`…） */
  permissions: string[];
  loginAt: string;
}

/** 登录页仍需要展示一个可用的入口账号提示（一期为内网演示环境） */
export const DEMO_ACCOUNT = {
  username: 'ops',
  password: 'changeme',
  name: '运维示例',
  role: 'ops' as UserRole,
  roleName: '运维',
};

/** 读取登录态缓存。任何解析异常都按「未登录」处理，避免脏数据把路由卡死。 */
export const getSession = (): SessionUser | null => {
  const raw = tokenStore.getUser() as SessionUser | null;
  return raw && raw.username ? raw : null;
};

export const isLoggedIn = (): boolean => getSession() !== null;

/** 「记住我」勾选状态，用于回填表单 */
export const wasRemembered = (): boolean => tokenStore.getRemember();

/**
 * 登录。成功后写入令牌与用户缓存，返回会话用户。
 * 失败会抛 ApiError（40100 账号或密码错误 / 其他按 message 展示）。
 */
export const login = async (username: string, password: string, remember: boolean): Promise<SessionUser> => {
  const u = await authApi.login(username, password);
  const session: SessionUser = {
    id: u.id,
    username: u.username,
    name: u.name,
    role: u.roleCode,
    roleName: u.roleName,
    permissions: u.permissions ?? [],
    loginAt: new Date().toISOString(),
  };
  tokenStore.setUser(session);
  tokenStore.setRemember(remember);
  return session;
};

/** 登出：吊销服务端会话并清本地。网络失败也会清本地，不把用户卡在登录态。 */
export const logout = async (): Promise<void> => {
  await authApi.logout();
};

/** 仅清本地（会话已失效时用） */
export const clearSession = (): void => tokenStore.clear();

/**
 * 用服务端数据刷新本地缓存。
 * 应用启动时调一次即可——若令牌已失效，这里会抛 40100，由调用方决定是否跳登录。
 */
export const refreshMe = async (): Promise<SessionUser | null> => {
  try {
    const u = await authApi.me();
    const session: SessionUser = {
      id: u.id,
      username: u.username,
      name: u.name,
      role: u.roleCode,
      roleName: u.roleName,
      permissions: u.permissions ?? [],
      loginAt: getSession()?.loginAt ?? new Date().toISOString(),
    };
    tokenStore.setUser(session);
    return session;
  } catch {
    clearSession();
    return null;
  }
};

// --------------------------------------------------------------------------- 权限判断

/**
 * 前端权限匹配——**必须与后端 auth.Match 语义完全一致**。
 *
 * 两边不一致的后果很具体：前端以为能点、后端返回 40300，用户看到的是
 * "按钮点下去报无权限"，而不是"按钮本来就是灰的"。
 * 后端实现见 internal/auth/auth.go 的 Match，改动时两边一起改。
 *
 * 支持形态：`*` / 精确 / `env:*` / `*:read` / `deploy:test,staging`
 */
export function matchPerm(perms: string[], want: string): boolean {
  const w = want.trim();
  if (!w) return true;
  for (const raw of perms) {
    const p = raw.trim();
    if (!p) continue;
    if (p === '*') return true;
    if (p === w) return true;
    // 模块级通配：env:* → 匹配 env:read / env:write
    if (p.endsWith(':*') && w.startsWith(p.slice(0, -1))) return true;
    // 后缀通配：*:read → 匹配 project:read / build:read
    if (p.startsWith('*:') && w.endsWith(p.slice(1))) return true;
    // 枚举：deploy:test,staging
    const i = p.indexOf(':');
    if (i > 0 && p.slice(i + 1).includes(',')) {
      const module = p.slice(0, i);
      if (p.slice(i + 1).split(',').some((item) => `${module}:${item.trim()}` === w)) return true;
    }
  }
  return false;
}

/** 当前用户是否拥有某权限点 */
export const hasPerm = (code: string): boolean => matchPerm(getSession()?.permissions ?? [], code);

/** 是否拥有任意一个权限点（与后端 RequirePerm 的"或"语义一致） */
export const hasAnyPerm = (...codes: string[]): boolean =>
  codes.length === 0 || codes.some((c) => hasPerm(c));

/**
 * 能否在某环境发起部署。
 * 后端：`POST /deployments` 路由只要求登录，真正的环境权限在 handler 里
 * `requireEnvPerm("deploy", envKey)`（internal/api/handler_guard.go）。
 */
export const canDeployOn = (envKey: string): boolean => hasPerm(`deploy:${envKey}`);

/**
 * 能否在某环境发起回滚。后端是**两把锁**，缺一不可：
 *   1. 路由 `RequirePerm("rollback:execute")`（internal/api/router.go）；
 *   2. handler 内 `guardServiceEnv(c, "deploy", svcID)` → `deploy:{env}`。
 *
 * ⚠️ 权限点表里**没有 `rollback:{env}` 这种形态**（只有 rollback:read /
 * rollback:execute / rollback:*）。此前这里按 `rollback:{env}` 判断，等于让
 * 显式持有 `rollback:execute` 的人被前端多禁一道——判据必须与后端逐字对应。
 */
export const canRollbackOn = (envKey: string): boolean =>
  hasPerm('rollback:execute') && canDeployOn(envKey);

// --------------------------------------------------------------------------- 会话失效广播
//
// 令牌彻底失效时（续期也失败），client 会回调这里。用 hash 跳转而不是
// react-router 的 navigate——本模块在 React 树之外，拿不到 navigate。
setUnauthorizedHandler(() => {
  clearSession();
  if (!window.location.hash.startsWith('#/login')) {
    window.location.hash = '#/login';
  }
});
