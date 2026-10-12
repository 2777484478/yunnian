import { useEffect, useRef, useState } from 'react';

// ---------------------------------------------------------------------------
// useAutoRefresh —— 列表页按固定间隔后台刷新
//
// 两件不做就会挨骂的事：
//   1. **页面切到后台还照刷**：浏览器不会因为标签页不可见就停掉定时器，
//      于是"用户开着页面去吃午饭"也会一直打后端。定时器只在可见时真正触发，
//      切回来的瞬间补一次，观感上是"一回来数据就是新的"。
//   2. **上一拍没回来就发下一拍**：后端慢的时候会滚成一串在飞请求。
//      用调用方给的 busy 判断跳过这一拍。
//
// 只负责"什么时候刷"，不负责"刷什么"——真正的取数交给 useApi 的
// refreshSilent（它不会闪加载遮罩、失败也不会把页面打成错误态）。
// ---------------------------------------------------------------------------

export interface UseAutoRefreshOptions {
  /** 是否开启；关闭时不装定时器 */
  enabled: boolean;
  /** 间隔（毫秒） */
  intervalMs: number;
  /** 到点执行。内部用 ref 持有，换函数不会重装定时器 */
  onTick: () => void;
  /** 返回 true 表示上一拍还没结束，跳过这一拍。默认不跳过 */
  isBusy?: () => boolean;
}

export function useAutoRefresh({ enabled, intervalMs, onTick, isBusy }: UseAutoRefreshOptions): void {
  const tickRef = useRef(onTick);
  tickRef.current = onTick;
  const busyRef = useRef(isBusy);
  busyRef.current = isBusy;

  useEffect(() => {
    if (!enabled) return;
    let timer = 0;
    let lastFireAt = 0;

    const fire = () => {
      if (document.hidden) return;
      if (busyRef.current?.()) return;
      lastFireAt = Date.now();
      tickRef.current();
    };

    const start = () => {
      window.clearInterval(timer);
      timer = window.setInterval(fire, intervalMs);
    };
    start();

    // 切回页面立刻补一次：否则最长要干等一个间隔，看起来像"刷新没生效"。
    // 顺便重置计时，让下一拍从"重新看见"开始算，而不是刚刷完又刷。
    //
    // 但要挡一下短时间内的重复派发：浏览器在窗口聚焦/失去焦点、跨屏拖动、
    // 自动化驱动等情况下会连着抛好几个 visibilitychange，逐个补刷就会连发
    // 几个一模一样的请求（实测切回一次能连发 4 条）。刚刷过就不再补。
    const onVisible = () => {
      if (document.hidden) return;
      start();
      if (Date.now() - lastFireAt < 1500) return;
      fire();
    };
    document.addEventListener('visibilitychange', onVisible);

    return () => {
      window.clearInterval(timer);
      document.removeEventListener('visibilitychange', onVisible);
    };
  }, [enabled, intervalMs]);
}

// ---------------------------------------------------------------------------
// 间隔偏好：记住用户选的值
//
// 不记的话，路由切走再回来组件重挂，用户调好的 10 秒又变回默认——一个每次
// 进页面都要重设的开关，等于没有开关。
//
// **必须按作用域分开存**（2026-10-09 加）。原先全局一个 key，于是"在 CI 项目
// 列表点了关闭自动刷新"会连带把 CD 的环境详情、发布拓扑一起关掉——用户在某一页
// 表达的"这一屏我想安静看"被当成了"整个平台都别刷"。更糟的是它会被**误当成
// 功能没实现**：明明做了自动刷新，进页面却是关的，看着就像没做。
//
// 不给 scope 就沿用老的全局 key —— CI 项目列表的行为因此**一字未变**。
// ---------------------------------------------------------------------------

const PREF_KEY = 'yn.autorefresh.interval';

const prefKey = (scope?: string): string => (scope ? `${PREF_KEY}:${scope}` : PREF_KEY);

/** 读偏好：没存过用 defaultMs；存的是 'off' 则返回 null（关闭） */
export function readAutoRefreshPref(defaultMs = 3000, scope?: string): number | null {
  try {
    const raw = localStorage.getItem(prefKey(scope));
    if (raw === null) return defaultMs;
    if (raw === 'off') return null;
    const n = Number(raw);
    return Number.isFinite(n) && n >= 1000 ? n : defaultMs;
  } catch {
    // 隐私模式 / 存储被禁：退回默认值，不要让整个页面挂掉
    return defaultMs;
  }
}

export function writeAutoRefreshPref(ms: number | null, scope?: string): void {
  try {
    localStorage.setItem(prefKey(scope), ms === null ? 'off' : String(ms));
  } catch {
    /* 同上，静默忽略 */
  }
}

/**
 * 组合成 state：值与写回偏好一起管理。
 *
 * @param scope 作用域，同一屏 / 同一组页面共用一个键；不传 = 老的全局键
 */
export function useAutoRefreshPref(
  defaultMs = 3000,
  scope?: string,
): [number | null, (ms: number | null) => void] {
  const [ms, setMs] = useState<number | null>(() => readAutoRefreshPref(defaultMs, scope));
  const update = (v: number | null) => {
    setMs(v);
    writeAutoRefreshPref(v, scope);
  };
  return [ms, update];
}
