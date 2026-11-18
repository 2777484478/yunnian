import { http } from './client';

export interface Team {
  id: string;
  name: string;
  description: string;
  sort: number;
  enabled: boolean;
  /** 挂在它下面的项目数（后端一次分组统计给出，避免前端 N+1） */
  projectCount: number;
  /**
   * 成员数。决定**谁能看到这个团队的项目**：
   * 0 个成员 + N 个项目 = 那些项目只有超级管理员看得到。
   */
  memberCount: number;
}

/** 团队里的一个人 */
export interface TeamMember {
  id: string;
  username: string;
  name: string;
  role: string;
}

/**
 * 团队编辑弹窗里「项目归属」的一行。
 *
 * 列出**全部**项目（不只是本团队的），因为这块的语义是"把项目迁到本团队"；
 * 只列本团队的就没法迁入了。
 */
export interface TeamProjectOption {
  key: string;
  name: string;
  type: string;
  /** 当前归属团队 */
  team: string;
  /** 是否已经在本团队 */
  inTeam: boolean;
  disabled: boolean;
}

/**
 * 团队主数据。
 *
 * 为什么要有它：`project.team` 一直是个**自由字符串**，候选值写死在前端 6 个 ——
 * 后端没有任何约束，手填一个不存在的团队也能存进去，而用户的数据权限正是按它反查的。
 */
export const teamApi = {
  list: () => http.get<{ list: Team[] }>('/teams').then((r) => r.list ?? []),

  create: (body: { name: string; description?: string; sort?: number }) => http.post<Team>('/teams', body),

  /** 改名会**级联更新所有项目的 team**（后端同一事务里做） */
  update: (id: string, body: { name?: string; description?: string; sort?: number; enabled?: boolean }) =>
    http.patch<Team>(`/teams/${id}`, body),

  /** 有项目或有成员挂着会被拒绝（后端 40901 并说明还有几个） */
  remove: (id: string) => http.del<{ deleted: boolean }>(`/teams/${id}`),

  /** 团队成员 */
  members: (id: string) => http.get<{ list: TeamMember[] }>(`/teams/${id}/members`).then((r) => r.list ?? []),

  /** 覆盖式设置成员（传的就是最终名单，空数组 = 清空） */
  setMembers: (id: string, userIds: string[]) =>
    http.put<{ list: TeamMember[] }>(`/teams/${id}/members`, { userIds }).then((r) => r.list ?? []),

  /** 「项目归属」选项：全部项目 + 各自当前归属 */
  projectOptions: (id: string) =>
    http.get<{ list: TeamProjectOption[] }>(`/teams/${id}/projects`).then((r) => r.list ?? []),

  /**
   * 把选中的项目**迁入**本团队（单向）。
   *
   * 不是"勾选绑定"：一个项目只能属于一个团队，所以这个操作就是
   * 把 `project.team` 改成这个团队的名字，原团队不需要任何清理。
   */
  moveProjects: (id: string, projectKeys: string[]) =>
    http.put<{ moved: number }>(`/teams/${id}/projects`, { projectKeys }),
};
