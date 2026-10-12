import { http } from './client';
import { unwrapList, unwrapPage, params } from './shape';
import type { AuditLog, PageResult } from '@/types';

// ---------------------------------------------------------------------------
// 审计日志（对应后端 handler_system.go 的 audit 部分）
// ---------------------------------------------------------------------------

export interface AuditQuery {
  module?: string;
  user?: string;
  action?: string;
  result?: string;
  keyword?: string;
  /** 后端同时接受 startTime/startAt 与 endTime/endAt 两种写法 */
  startTime?: string;
  endTime?: string;
  page?: number;
  pageSize?: number;
}

export interface AuditExportResult {
  taskId: string;
  /** 后端同步生成，正常即为 `done`；保留字符串联合以便后端将来改异步 */
  status: 'pending' | 'running' | 'done' | 'success' | 'failed';
  /** 导出完成后的下载地址或内容（视后端实现） */
  url?: string;
  rows?: number;
  /** 达到 limit 被截断（此时 rows == limit，需要收窄条件分批导出） */
  truncated?: boolean;
  /** 建议文件名，如 `yunnian-audit-20260923.csv` */
  filename?: string;
  /**
   * CSV 正文。**注意后端已经在开头写了 UTF-8 BOM**，
   * 前端落盘时不要再补一个，否则双 BOM 会让 Excel 把第一列表头读成乱码。
   */
  content?: string;
  /** 任务创建时间，用于展示"导出中" */
  createdAt?: string;
  message?: string;
}

export const auditApi = {
  list: async (q: AuditQuery = {}): Promise<PageResult<AuditLog>> =>
    unwrapPage<AuditLog>(await http.get('/audit-logs', params(q))),

  get: (id: string) => http.get<AuditLog>(`/audit-logs/${id}`),

  /**
   * 发起导出。后端是**异步任务**：立刻返回 taskId，前端轮询 exportResult 取结果。
   * 允许空过滤条件（导出全量），所以 body 可以为空对象。
   */
  export: (body: {
    module?: string;
    user?: string;
    action?: string;
    result?: string;
    startAt?: string;
    endAt?: string;
    limit?: number;
  } = {}) => http.post<AuditExportResult>('/audit-logs/export', body),

  exportResult: (taskId: string) => http.get<AuditExportResult>(`/audit-logs/export/${taskId}`),
};

export { unwrapList };
