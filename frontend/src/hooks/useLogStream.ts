import { useEffect, useRef, useState } from 'react';
import { subscribeLogStream } from '@/api';
import type { LogLineRecord } from '@/types';

// ---------------------------------------------------------------------------
// useLogStream —— 订阅后端 SSE 日志流（构建 / 部署 / 回滚 / Pod）
//
// 事件契约与解析都落在 api/sse.ts 的 subscribeLogStream 里，这里只负责
// 把它接到 React 状态上（重置时机、行数上限、卸载清理）。
// 批量执行引擎（BatchRunner）用的是同一个 subscribeLogStream——
// 一份契约只有一个实现，后端加事件时不会漏改某一处。
// ---------------------------------------------------------------------------

/**
 * 流式日志行 = 历史日志行。两者字段完全一致（seq/ts/level/line），
 * 所以不另立类型——否则页面里"历史日志"和"实时日志"会变成两种互不兼容的东西。
 */
export type LogLine = LogLineRecord;

export interface LogStatusEvent {
  status: string;
  progress?: number;
  stage?: string;
  duration?: number;
  image?: string;
}

export interface UseLogStreamResult {
  /** 累计日志行（后端会先补发历史，再推实时） */
  lines: LogLine[];
  /** 最近一次状态事件 */
  status: LogStatusEvent | null;
  /** 流是否已正常结束 */
  ended: boolean;
  /** 结束原因：finished / canceled / failed / timeout */
  endReason: string;
  /** 连接异常信息（自动重连中也会置上，用于提示"正在重连"） */
  error: string;
}

export interface UseLogStreamOptions {
  /** 关掉订阅（如抽屉未打开时不连） */
  enabled?: boolean;
  /** 最多保留多少行，防止长任务把内存吃爆。默认 5000 */
  maxLines?: number;
}

export function useLogStream(
  url: string | null,
  options: UseLogStreamOptions = {},
): UseLogStreamResult {
  const { enabled = true, maxLines = 5000 } = options;

  const [lines, setLines] = useState<LogLine[]>([]);
  const [status, setStatus] = useState<LogStatusEvent | null>(null);
  const [ended, setEnded] = useState(false);
  const [endReason, setEndReason] = useState('');
  const [error, setError] = useState('');

  // 用 ref 持有上限，避免把它塞进 effect 依赖导致重连
  const maxRef = useRef(maxLines);
  maxRef.current = maxLines;

  useEffect(() => {
    if (!enabled || !url) return;

    // 换 URL 视为一次全新订阅：清空上一轮的日志与状态，
    // 否则会把 build#1 的日志和 build#2 的日志混在一起。
    setLines([]);
    setStatus(null);
    setEnded(false);
    setEndReason('');
    setError('');

    const handle = subscribeLogStream(url, {
      onLog: (item) =>
        setLines((prev) => {
          const next = prev.length >= maxRef.current ? prev.slice(prev.length - maxRef.current + 1) : prev;
          return [...next, item];
        }),
      onStatus: setStatus,
      onEnd: (reason) => {
        setEndReason(reason);
        setEnded(true);
      },
      onError: (msg) => {
        // 重连成功时后端发的是空串，表示"恢复正常"，直接清掉提示
        if (msg) setError(msg);
        else setError('');
      },
    });

    return () => handle.close();
  }, [url, enabled]);

  return { lines, status, ended, endReason, error };
}
