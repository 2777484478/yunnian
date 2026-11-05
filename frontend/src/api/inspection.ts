import { http } from './client';

export interface InspectionFinding {
  level: 'critical' | 'warning' | 'info';
  kind: string;
  title: string;
  detail: string;
  action?: string;
  penalty?: number;
}

export interface InspectionStats {
  svcTotal: number;
  svcReady: number;
  svcNotReady: number;
  svcDegraded: number;
  nodes: number;
  deploys: number;
  deployFail: number;
  certs: number;
  certExpiring: number;
}

export interface InspectionReport {
  reportId: number;
  envKey: string;
  envName: string;
  score: number;
  level: 'healthy' | 'warning' | 'critical';
  summary: string;
  findings: InspectionFinding[];
  stats: InspectionStats;
  size: number;
  createdAt: string;
  notes?: string[];
}

export interface InspectionReportRow {
  id: string;
  envKey: string;
  envName: string;
  score: number;
  level: string;
  summary: string;
  size: number;
  createdAt: string;
}

/**
 * 环境巡检报告。
 *
 * 生成接口**不返回 HTML**（几十 KB 且列表用不上），要看正文走 detail / download。
 */
export const inspectionApi = {
  list: (envKey?: string) =>
    http
      .get<{ list: InspectionReportRow[] }>('/inspection-reports', envKey ? { envKey } : undefined)
      .then((r) => r.list ?? []),

  run: (envKey: string, hours?: number) =>
    http.post<{ report: InspectionReport; htmlLen: number }>('/inspection-reports', { envKey, hours }),

  detail: (id: string) =>
    http.get<{
      id: string;
      envKey: string;
      envName: string;
      score: number;
      level: string;
      summary: string;
      createdAt: string;
      html: string;
    }>(`/inspection-reports/${id}`),

  /** 下载走浏览器原生：后端带 Content-Disposition，这里只要把 token 拼进直链 */
  downloadUrl: (id: string) => `/api/v1/inspection-reports/${id}/download`,
};
