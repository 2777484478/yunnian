import { http } from './client';

export interface Team {
  id: string;
  name: string;
  description: string;
  sort: number;
  enabled: boolean;
  /** 挂在它下面的项目数（后端一次分组统计给出，避免前端 N+1） */
  projectCount: number;
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

  /** 有项目挂着会被拒绝（后端 40901 并说明还有几个） */
  remove: (id: string) => http.del<{ deleted: boolean }>(`/teams/${id}`),
};
