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
  /** 暂停期间缓冲、尚未并入 lines 的行数（>0 表示"有 N 行待显示"） */
  buffered: number;
}

export interface UseLogStreamOptions {
  /** 关掉订阅（如抽屉未打开时不连） */
  enabled?: boolean;
  /** 最多保留多少行，防止长任务把内存吃爆。默认 5000 */
  maxLines?: number;
  /**
   * 暂停追踪：**连接保持不断**，新到的事件先攒在缓冲里，恢复时一次性并入。
   *
   * 为什么不直接断开连接再重连：重连会重新走一遍 tail（日志从头再来一遍），
   * 重复的行混进视图，还得靠时间戳去重 —— 而时间戳并不保证唯一。
   * 这里选"保持连接 + 缓冲"，代价是暂停期间内存里攒着行（已有 maxLines 上限）。
   */
  paused?: boolean;
}

export function useLogStream(
  url: string | null,
  options: UseLogStreamOptions = {},
): UseLogStreamResult {
  const { enabled = true, maxLines = 5000, paused = false } = options;

  const [lines, setLines] = useState<LogLine[]>([]);
  const [status, setStatus] = useState<LogStatusEvent | null>(null);
  const [ended, setEnded] = useState(false);
  const [endReason, setEndReason] = useState('');
  const [error, setError] = useState('');
  const [buffered, setBuffered] = useState(0);

  // 用 ref 持有上限，避免把它塞进 effect 依赖导致重连
  const maxRef = useRef(maxLines);
  maxRef.current = maxLines;

  // paused 同理必须走 ref：它一变就要重跑 effect 的话，等于暂停/恢复各重连一次，
  // 每次都会重新 tail 一遍，日志重复一大片。
  const pausedRef = useRef(paused);
  pausedRef.current = paused;
  // 暂停期间攒下的行；恢复时一次性并入 lines
  const pendingRef = useRef<LogLine[]>([]);

  // 恢复追踪：把暂停期间缓冲的行一次补上
  useEffect(() => {
    if (paused) return;
    const queued = pendingRef.current;
    if (queued.length === 0) {
      setBuffered(0);
      return;
    }
    pendingRef.current = [];
    setBuffered(0);
    setLines((prev) => {
      const next = [...prev, ...queued];
      return next.length > maxRef.current ? next.slice(next.length - maxRef.current) : next;
    });
  }, [paused]);

  useEffect(() => {
    if (!enabled || !url) return;

    // 换 URL 视为一次全新订阅：清空上一轮的日志与状态，
    // 否则会把 build#1 的日志和 build#2 的日志混在一起。
    setLines([]);
    setStatus(null);
    setEnded(false);
    setEndReason('');
    setError('');
    pendingRef.current = [];
    setBuffered(0);

    const handle = subscribeLogStream(url, {
      onLog: (item) => {
        // 暂停中：只攒不显示。界面停在你正在看的那一屏，恢复时再补齐。
        if (pausedRef.current) {
          pendingRef.current.push(item);
          // 缓冲同样受 maxLines 约束，避免暂停很久把内存吃满
          if (pendingRef.current.length > maxRef.current) {
            pendingRef.current.splice(0, pendingRef.current.length - maxRef.current);
          }
          setBuffered(pendingRef.current.length);
          return;
        }
        setLines((prev) => {
          const next = prev.length >= maxRef.current ? prev.slice(prev.length - maxRef.current + 1) : prev;
          return [...next, item];
        });
      },
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

  return { lines, status, ended, endReason, error, buffered };
}
