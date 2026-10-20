import { http, tokenStore } from './client';
import { unwrapList, unwrapPage, params } from './shape';
import type {
  CurrentUser,
  LoginResult,
  PageResult,
  PermissionPoint,
  SysRole,
  SysUser,
} from '@/types';

// ---------------------------------------------------------------------------
// 认证与用户管理接口（对应后端 handler_auth.go）
//
// 本模块承担「认证副作用」——登录后写令牌、登出后清会话，都在这里做掉，
// 页面与路由守卫只面对 src/auth.ts 这个门面，不直接碰 tokenStore。
// ---------------------------------------------------------------------------

export const authApi = {
  /** 账号密码登录。成功后立即写入令牌与用户缓存，返回当前用户。 */
  async login(username: string, password: string): Promise<CurrentUser> {
    const r = await http.post<LoginResult>('/auth/login', { username, password });
    tokenStore.set(r.accessToken, r.refreshToken);
    tokenStore.setUser(r.user);
    return r.user;
  },

  /**
   * 改**自己**的基本信息。
   *
   * 刻意不走 `/users/:id`（那个要 user:manage）—— 每个人都能改自己的资料，
   * 但改不了角色与启用状态。
   */
  updateSelf: (body: { name?: string; email?: string; phone?: string; avatar?: string }) =>
    http.patch<CurrentUser>('/auth/me', body),

  /** 改自己的密码（必须验旧密码） */
  changePassword: (oldPassword: string, newPassword: string) =>
    http.put<{ changed: boolean }>('/auth/me/password', { oldPassword, newPassword }),

  /**
   * 上传头像：**base64 JSON**，不是 multipart。
   *
   * 我们的 http 封装固定发 JSON；走 multipart 就得绕开它，连令牌与错误处理一起丢。
   * 头像 ≤1MB，base64 膨胀 33% 可接受。
   */
  uploadAvatar: (fileName: string, dataUrl: string) =>
    http.post<{ avatar: string }>('/auth/me/avatar', { fileName, data: dataUrl }),

  /** 登出：吊销服务端会话。网络失败也要清本地，否则用户会卡在"登不出去"。 */
  async logout(): Promise<void> {
    try {
      await http.post<null>('/auth/logout');
    } finally {
      tokenStore.clear();
    }
  },

  /** 拉当前用户（含权限点）。这是前端权限判断的唯一权威来源。 */
  async me(): Promise<CurrentUser> {
    const u = await http.get<CurrentUser>('/auth/me');
    tokenStore.setUser(u);
    return u;
  },

  /** SSO 发起地址，交给浏览器整页跳转 */
  oidcAuthorizeUrl: () => '/auth/oidc/authorize',
};

export interface UserQuery {
  keyword?: string;
  role?: string;
  enabled?: boolean;
  page?: number;
  pageSize?: number;
}

/**
 * 「选人」下拉用的精简用户项。
 *
 * 只有三个字段是刻意的：后端 `/users/options` 只回这些。角色、邮箱、启停状态
 * 属用户管理范畴（`GET /users` 的语义，要 `user:manage`），不该为两个下拉而开放。
 */
export interface UserOption {
  id: string;
  username: string;
  name: string;
}

export const userApi = {
  /**
   * 「选人」下拉的名录（环境的负责人 / 审批人）。
   *
   * ⚠️ **不要用 `list()` 代替**：`GET /users` 要 `user:manage`，而维护环境的 ops 只有
   * `env:write` —— 用 `list()` 时它拿到 `40300 缺少权限点：user:manage`，前端不弹错、
   * 候选恒为 `[]`，于是"抽屉能打开、一个人都选不到"（负责人换不了、审批人加不了）。
   */
  options: async (): Promise<UserOption[]> => unwrapList<UserOption>(await http.get('/users/options')),

  list: async (q: UserQuery = {}): Promise<PageResult<SysUser>> =>
    unwrapPage<SysUser>(await http.get('/users', params(q))),

  invite: (body: { username: string; name: string; email: string; role: string }) =>
    http.post<SysUser>('/users/invite', body),

  /**
   * 改角色 / 启停。后端用指针字段接收，只传要改的键即可（其它保持原值）。
   * 注意：不能停用当前登录账号，后端返回 40000。
   */
  update: (id: string, body: { role?: string; enabled?: boolean; name?: string; email?: string }) =>
    http.patch<SysUser>(`/users/${id}`, body),

  /** 删除用户（硬删）。后端拒绝删自己与最后一个超级管理员。 */
  remove: (id: string) => http.del<{ deleted: boolean }>(`/users/${id}`),

  resetPassword: (id: string, body: { password: string }) =>
    http.post<{ password: string } | null>(`/users/${id}/reset-password`, body),
};

export const roleApi = {
  /** 角色列表。perms 里是权限 code 字符串（如 `build:read`、`deploy:*`）。 */
  list: async (): Promise<SysRole[]> => unwrapList<SysRole>(await http.get('/roles')),

  /** 权限点清单（对象数组，带中文名与模块，供权限编辑页勾选）。 */
  permissions: async (): Promise<PermissionPoint[]> =>
    unwrapList<PermissionPoint>(await http.get('/permissions')),

  /**
   * 覆盖式设置角色权限。
   * 传空数组会被后端拒绝（防止误把角色清空），确需清空时 confirmEmpty=true
   * —— 注意它是**查询参数**不是 body 字段（后端 httpx.QueryBool 读取）。
   */
  setPermissions: (code: string, perms: string[], confirmEmpty = false) =>
    http.put<SysRole>(`/roles/${code}/permissions`, { perms }, { confirmEmpty }),
};
