import { useCallback, useEffect, useRef, useState } from 'react';
import type { MutableRefObject } from 'react';
import { ApiError } from '@/api';

// ---------------------------------------------------------------------------
// useApi —— 统一的取数 hook
//
// 解决四件页面里最容易写错的事：
//   1. **竞态**：快速切换筛选条件时，先发的请求可能后回来，把新数据覆盖成旧数据。
//      这里用一个自增序号 + AbortController 双保险。
//   2. **卸载后 setState**：组件已卸载还写状态，React 会告警且逻辑错乱。
//   3. **手动 refresh**：变更操作后要重新拉列表，统一提供 refresh。
//   4. **后台静默刷新**：列表页按秒轮询时，不能闪加载遮罩、也不能因一次网络
//      抖动就把整页打成错误态——refreshSilent 走的就是这条路。
//
// 用法：
//   const { data, loading, error, refresh } = useApi(
//     (signal) => projectApi.list({ page }), [page]
//   );
// ---------------------------------------------------------------------------

export interface UseApiResult<T> {
  data: T | null;
  loading: boolean;
  error: ApiError | null;
  /** 重新拉取当前依赖下的数据（变更后刷新用）。会走加载态与错误态 */
  refresh: () => void;
  /**
   * 后台自动刷新用：**不置 loading、失败不写 error、不清空旧数据**。
   *
   * 为什么必须单独一条：轮询每几秒来一次，若每次都把 loading 置上，列表会
   * 一直闪加载遮罩；若失败就把 error 写上，一次网络抖动能让整页变错误页——
   * 而此时用户手上那份数据其实还是对的。
   */
  refreshSilent: () => void;
  /** 静默刷新最近一次的失败原因；为空表示上次静默刷新成功（或还没刷过） */
  silentError: ApiError | null;
  /** 最近一次**成功**取数的时间戳，用来回答"我现在看到的数据有多新" */
  updatedAt: number | null;
  /** 是否有请求在飞行中（含静默）。轮询用它跳过还没回来的那一拍，避免请求叠加 */
  pendingRef: MutableRefObject<boolean>;
  /** 本地直接改数据（乐观更新用），不会触发请求 */
  setData: (v: T | null) => void;
}

export interface UseApiOptions {
  /** 首次是否立即请求，默认 true。条件不满足时可传 false 跳过（如缺少必需的 id） */
  enabled?: boolean;
}

export function useApi<T>(
  fetcher: (signal: AbortSignal) => Promise<T>,
  deps: unknown[] = [],
  options: UseApiOptions = {},
): UseApiResult<T> {
  const { enabled = true } = options;

  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState<boolean>(enabled);
  const [error, setError] = useState<ApiError | null>(null);
  const [silentError, setSilentError] = useState<ApiError | null>(null);
  const [updatedAt, setUpdatedAt] = useState<number | null>(null);

  // fetcher 每次渲染都是新函数，放进依赖会导致无限循环；
  // 用 ref 持有最新引用，effect 只依赖调用方显式声明的 deps。
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;

  const enabledRef = useRef(enabled);
  enabledRef.current = enabled;

  const seqRef = useRef(0);
  const abortRef = useRef<AbortController | null>(null);
  const mountedRef = useRef(true);
  const pendingRef = useRef(false);

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      abortRef.current?.abort();
    };
  }, []);

  const load = useCallback((silent: boolean) => {
    if (!enabledRef.current) return;

    // 新请求发出前取消上一个，避免两条响应交叉写入
    abortRef.current?.abort();
    const ac = new AbortController();
    abortRef.current = ac;

    const seq = ++seqRef.current;
    pendingRef.current = true;
    if (!silent) setLoading(true);

    fetcherRef.current(ac.signal)
      .then((res) => {
        // 只认最后一次请求的结果
        if (!mountedRef.current || seq !== seqRef.current) return;
        setData(res);
        setError(null);
        setSilentError(null);
        setUpdatedAt(Date.now());
      })
      .catch((e) => {
        if (!mountedRef.current || seq !== seqRef.current) return;
        // 主动取消不算错误
        if ((e as Error)?.name === 'AbortError') return;
        const err = e instanceof ApiError ? e : new ApiError(-1, (e as Error)?.message || '加载失败');
        // 静默那拍失败只记原因、不动整页状态：下一次成功会自动清掉。
        if (silent) setSilentError(err);
        else setError(err);
      })
      .finally(() => {
        if (!mountedRef.current || seq !== seqRef.current) return;
        pendingRef.current = false;
        if (!silent) setLoading(false);
      });
  }, []);

  useEffect(() => {
    if (!enabled) {
      setLoading(false);
      return;
    }
    load(false);
    return () => abortRef.current?.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled, load, ...deps]);

  const refresh = useCallback(() => load(false), [load]);
  const refreshSilent = useCallback(() => load(true), [load]);

  return { data, loading, error, refresh, refreshSilent, silentError, updatedAt, pendingRef, setData };
}
