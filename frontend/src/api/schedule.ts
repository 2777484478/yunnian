import { http } from './client';
import { unwrapList } from './shape';

// ---------------------------------------------------------------------------
// 定时任务
//
// 它**不是第二套 CI/CD**：到点只是替用户"点一下按钮" —— 建一张普通的构建单
// 或部署单，后面的排队、失败通知、超时判定全部走现成链路。
//
// 所以这里所有接口都很轻：没有日志、没有进度，只有一个"建单结果"。
// 想看执行过程请点任务里的「上次构建 / 上次部署」跳到对应记录页。
// ---------------------------------------------------------------------------

/** 任务类型：build = 定时构建，deploy = 定时部署。 */
export type ScheduleTaskType = 'build' | 'deploy';

/** 定时部署发哪个版本。 */
export type ScheduleImageSource = 'fixed' | 'latest';

export interface ScheduleTask {
  id: string;
  name: string;
  taskType: ScheduleTaskType;
  projectKey: string;
  /** 只有 deploy 任务有值 */
  envKey: string;
  /** 标准 5 字段 cron（分 时 日 月 周） */
  cron: string;
  enabled: boolean;

  /** build：平台自建构建用的分支 */
  branch: string;
  /** build：Jenkins 参数 / 平台构建参数 */
  buildParams?: Record<string, string>;

  /** deploy：fixed = 每次都发同一个版本；latest = 每次取最新构建 */
  imageSource: ScheduleImageSource | '';
  /** deploy：具体版本（平台 tag 或仓库标签） */
  imageTag: string;
  /** deploy：平台构建记录 id（fixed 且来自平台构建时有值） */
  imageRecordId: string;
  /** deploy：空 = 保持服务当前副本数 */
  replicas?: number | null;

  remark: string;

  /** 下次执行时间（RFC3339）。停用 / 表达式非法时为 null */
  nextRunAt?: string | null;
  lastRunAt?: string | null;
  /** success / failed / skipped / missed / running */
  lastStatus: string;
  lastError: string;
  /** 累计错过次数（服务停机期间到点但没跑）。跑成功一次会归零 */
  missedCount: number;
  lastBuildId: string;
  lastDeployId: string;

  createdAt: string;
  updatedAt: string;
}

/** 新建 / 编辑入参。 */
export interface ScheduleInput {
  name: string;
  taskType: ScheduleTaskType;
  projectKey: string;
  envKey?: string;
  cron: string;
  enabled?: boolean;
  branch?: string;
  buildParams?: Record<string, string>;
  imageSource?: ScheduleImageSource;
  imageTag?: string;
  imageRecordId?: string;
  replicas?: number | null;
  remark?: string;
}

/** cron 预览结果：校验表达式 + 接下来 5 次执行时间。 */
export interface SchedulePreview {
  valid: boolean;
  error: string;
  next: string[];
}

export const scheduleApi = {
  list: async (q?: { taskType?: string; projectKey?: string }): Promise<ScheduleTask[]> =>
    unwrapList<ScheduleTask>(
      await http.get<ScheduleTask[]>('/schedules', {
        taskType: q?.taskType || undefined,
        projectKey: q?.projectKey || undefined,
      }),
    ),
  get: (id: string) => http.get<ScheduleTask>(`/schedules/${id}`),
  create: (body: ScheduleInput) => http.post<ScheduleTask>('/schedules', body),
  update: (id: string, body: ScheduleInput) => http.put<ScheduleTask>(`/schedules/${id}`, body),
  remove: (id: string) => http.del(`/schedules/${id}`),
  setEnabled: (id: string, enabled: boolean) =>
    http.post<ScheduleTask>(`/schedules/${id}/enabled`, { enabled }),
  /** 立即执行一次（调试用）。不会推进下次计划时间。 */
  runNow: (id: string) => http.post<ScheduleTask>(`/schedules/${id}/run`, {}),
  /** 校验 cron 并预览。纯计算，不落库。 */
  preview: (cron: string) => http.get<SchedulePreview>('/schedules/preview', { cron }),
};
