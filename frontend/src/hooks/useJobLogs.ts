import { useApi } from '@/hooks/useApi';
import { useLogStream } from '@/hooks/useLogStream';
import type { LogLineRecord } from '@/types';

// ---------------------------------------------------------------------------
// useJobLogs —— 任务日志（构建 / 部署 / 回滚）的统一取数
//
// 三种日志来源必须合起来看，页面不该各自实现一遍：
//   1. 任务进行中 → 连 SSE 实时流（拿得到 status/end 事件，日志边跑边出）；
//   2. 任务已结束 → 读历史日志接口（一次性全量，比开流更省）；
//   3. 流补发 → 后端已把已落库的历史先推一遍，所以 (1) 的场景也能看到完整日志。
//
// 这里的"进行中"由调用方按任务状态判定后传进来（live），因为
// 构建是 running/queued、部署是 progressing/queued、回滚又有自己的状态集。
// ---------------------------------------------------------------------------

export interface UseJobLogsOptions {
  /** 任务 id；为空表示没有选中任何任务 */
  id: string | null;
  /** 任务是否仍在进行中 */
  live: boolean;
  /** 实时流地址构造（一般是 xxxApi.stream） */
  stream: (id: string) => string;
  /** 历史日志取数（一般是 xxxApi.logs） */
  fetchHistory: (id: string) => Promise<LogLineRecord[]>;
}

export interface UseJobLogsResult {
  lines: LogLineRecord[];
  /** 历史日志加载中（实时流不走这个标志） */
  loading: boolean;
  /** 人类可读的错误信息，空串表示无错 */
  error: string;
  /** 实时流是否已正常结束（可用于触发列表刷新） */
  ended: boolean;
  /** 当前是否接的是实时流 */
  isLive: boolean;
}

export function useJobLogs({ id, live, stream, fetchHistory }: UseJobLogsOptions): UseJobLogsResult {
  const streamUrl = id && live ? stream(id) : null;
  const s = useLogStream(streamUrl, { enabled: !!streamUrl });

  const history = useApi(() => fetchHistory(id!), [id], { enabled: !!id && !live });

  return {
    lines: streamUrl ? s.lines : (history.data ?? []),
    loading: !streamUrl && !!id && history.loading,
    error: streamUrl ? s.error : (history.error?.message ?? ''),
    ended: s.ended,
    isLive: !!streamUrl,
  };
}
