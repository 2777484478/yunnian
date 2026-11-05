import { http } from './client';
import { tokenStore } from './client';

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
  /** 平台记录与集群不一致的服务数（不影响健康分） */
  svcDegraded: number;
  /** 期望副本为 0、当前未运行的服务数（不影响健康分） */
  svcIdle: number;
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

/** 带 token 拉报告 HTML 并触发浏览器下载（下载接口需要 Authorization） */
export async function downloadInspectionFile(id: string, fallbackName: string): Promise<void> {
  const token = tokenStore.get();
  const res = await fetch(`/api/v1/inspection-reports/${id}/download`, {
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  });
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  const blob = await res.blob();
  let name = fallbackName;
  const cd = res.headers.get('Content-Disposition') ?? '';
  const m = /filename\*=UTF-8''([^;]+)/i.exec(cd);
  if (m) name = decodeURIComponent(m[1]);
  const a = document.createElement('a');
  a.href = URL.createObjectURL(blob);
  a.download = name;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(a.href);
}
