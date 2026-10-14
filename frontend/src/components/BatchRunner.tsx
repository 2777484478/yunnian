import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { App as AntApp, Button, Progress, Segmented, Space, Switch, Tag, Tooltip, Typography } from 'antd';
import {
  CheckCircleFilled,
  ClockCircleFilled,
  CloseCircleFilled,
  LoadingOutlined,
  MergeCellsOutlined,
  MinusCircleFilled,
  PauseCircleFilled,
  ProfileOutlined,
  StopOutlined,
  SyncOutlined,
  ThunderboltFilled,
} from '@ant-design/icons';
import { LogPanel } from './LogPanel';
import { ApiError, subscribeLogStream } from '@/api';
import type { LogLineRecord } from '@/types';

const { Text } = Typography;

// ---------------------------------------------------------------------------
// BatchRunner —— 批量任务执行面板（左侧任务队列，右侧实时日志）
//
// 这个组件**不是**一个"播放器"。早期版本按前端预生成的日志帧播放，
// 看上去很热闹但它只是一段动画：任务并没有真的提交，日志也不是真的。
// 现在它是一台调度器，每个任务都对应后端一张真实单据：
//
//   1. 按并发上限从队列里领任务 → 调 startTask 提交（POST /builds 或 /deployments）；
//   2. 用后端下发的 logStreamUrl 订阅 SSE，日志**逐行来自服务端**；
//   3. 流结束（event: end）后调 fetchOutcome 取终态 —— 单靠 end 判不了成败，
//      因为 canceled / failed / finished 都发 end，必须回查记录。
//
// 由此产生的两个"不能糊弄"的点：
//   - **没有速度/暂停了**。真实日志的快慢由 Runner 决定，前端调速只会让
//     日志与真实进度对不上。要"暂停看日志"请用日志面板的搜索与跟随开关。
//   - **待审批不是执行中**。正式区发出去的单据状态是 pending_approval，
//     它不会产生任何执行日志。这里给它独立状态，而不是让它挂在"执行中"
//     让用户以为已经发了版。
// ---------------------------------------------------------------------------

/** 任务定义：只有"是什么"，没有"怎么跑"（怎么跑由 startTask 决定） */
export interface BatchTaskDef {
  id: string;
  title: string;
  subtitle: string;
  /** 日志合并视图里的短前缀，如 user-web */
  short: string;
  projectKey: string;
  /** 只读展示：本次将要使用的镜像 / 版本（由项目规则派生，用户改不了） */
  imageHint?: string;
}

/**
 * 任务状态。
 * - `submitting` 指的是"已经发出创建请求，还没拿到单据"，它与 queued 不同：
 *   queued 是还没发，submitting 是发了但还不知道后端建成没建成。
 */
export type BatchTaskStatus =
  | 'queued'
  | 'submitting'
  | 'running'
  | 'pending'
  | 'success'
  | 'failed'
  | 'canceled'
  /**
   * 平台重启时中断（后端 `interrupted`）。**独立一档，不是 failed 的马甲**：
   * 平台一重启，进程内的执行队列连同跑着的任务一起消失。
   * 归到 failed 会让人去问"这批为什么失败"，可代码、镜像、环境都没问题。
   */
  | 'interrupted';

/** 提交成功后后端返回的执行句柄 */
export interface BatchTaskHandle {
  /** 后端主键：构建单 / 部署单 id */
  runId: string;
  /** 后端下发的日志流地址（已带令牌） */
  stream: string;
  /** 展示用单号，如 DP202609230013 */
  runNo?: string;
  /**
   * 后端受理这一刻的**真实状态**。默认按 running 处理。
   *
   * 为什么要它：批量提交时后端构建池可能已满，它会把这单排进等待队列，
   * 记录状态就是 queued。面板若一律显示"执行中"，用户会以为已经在跑了，
   * 而后端其实还没开始——这正是"界面说的与后端做的不一致"。
   */
  initialStatus?: 'queued' | 'running';
  /** 与 initialStatus 配套的阶段说明（后端原文，含"前面还有 N 个任务"这类真实位置） */
  initialStage?: string;
  /** 无需执行即为终态（正式区待审批） */
  awaitingApproval?: boolean;
  /** 待审批时的审批人（后端 approvers） */
  approvers?: string[];
}

export interface BatchTaskOutcome {
  /** 是否成功。pending/canceled 都不算 ok */
  ok: boolean;
  /** 后端终态：success / failed / canceled / pending_approval … */
  status: string;
  /** 失败原因或阶段说明 */
  detail?: string;
}

export interface BatchResult {
  total: number;
  success: number;
  failed: number;
  canceled: number;
  /** 已提交但等待审批，既不算成功也不算失败 */
  pending: number;
  /**
   * 被平台重启中断（非失败）。
   *
   * **必须单列**：漏掉它，`!failed && !canceled` 会把一批"跑了一半就没了"的任务
   * 判成"全部成功"，弹出一句绿色的「批量发布完成」 —— 那是这批任务里最贵的一个谎，
   * 因为用户看到绿色就关页面走了。
   */
  interrupted: number;
  /** 总耗时（毫秒） */
  ms: number;
}

export interface BatchRunnerProps {
  kind: 'ci' | 'cd';
  batchNo: string;
  tasks: BatchTaskDef[];
  /** 并发上限（CD 由「运行参数」里的部署并发决定，见 BatchDeployWizard） */
  concurrency?: number;
  /** 目标环境名（CD 展示用） */
  envLabel?: string;
  /** 真正把任务提交到后端，返回执行句柄 */
  startTask: (task: BatchTaskDef) => Promise<BatchTaskHandle>;
  /** 流结束后回查终态（不能只靠 end 事件判成败） */
  fetchOutcome: (runId: string) => Promise<BatchTaskOutcome>;
  /** 取消一个已提交的任务（后端构建/部署取消接口） */
  cancelRun?: (runId: string) => Promise<void>;
  /** 单任务结束时回调（用于提示 / 追加本地视图） */
  onTaskDone?: (
    task: BatchTaskDef,
    outcome: BatchTaskOutcome,
    lines: LogLineRecord[],
    handle: BatchTaskHandle,
  ) => void;
  /** 全部结束时回调 */
  onFinish?: (r: BatchResult) => void;
  onRetryFailed?: () => void;
  /** 打开记录列表（构建记录 / 部署记录） */
  onOpenRecords?: () => void;
  height?: number;
}

interface Rt {
  status: BatchTaskStatus;
  lines: LogLineRecord[];
  progress: number;
  stage: string;
  runId?: string;
  runNo?: string;
  stream?: string;
  approvers?: string[];
  error?: string;
  /** 用户在提交途中点了取消：拿到 runId 后要补一次真实取消 */
  cancelRequested?: boolean;
  startedAt: number;
  finishedAt: number;
}

const initRt = (): Rt => ({
  status: 'queued',
  lines: [],
  progress: 0,
  stage: '',
  startedAt: 0,
  finishedAt: 0,
});

/** 已结束、不再变化的状态 */
const TERMINAL: BatchTaskStatus[] = ['success', 'failed', 'canceled', 'pending', 'interrupted'];
/**
 * 是否占用并发槽。
 *
 * 判定依据不是"状态等不等于 running"，而是**后端有没有受理**：
 *   - submitting：请求已发出，还没拿到单据 → 占槽（不占槽会连发两次）；
 *   - 已拿到 runId：后端已受理 → 占槽，**即使它显示"排队中"**。
 *     后端排队（构建池满）不代表这次构建可以被再提交一遍。
 */
const occupiesSlot = (s: Rt): boolean =>
  !TERMINAL.includes(s.status) && (s.status === 'submitting' || !!s.runId);

/** 单任务日志上限，防止长任务把内存吃爆（与 useLogStream 的默认值一致） */
const MAX_LINES = 5000;
/** 调度节拍：既是取任务频率，也是日志刷进 React 的频率 */
const TICK = 400;

const STATUS_STYLE: Record<
  BatchTaskStatus,
  { label: string; color: string; icon: React.ReactNode; bar: string }
> = {
  queued: { label: '排队中', color: '#8c8c95', icon: <ClockCircleFilled />, bar: '#d9dde3' },
  submitting: { label: '提交中', color: '#1677ff', icon: <LoadingOutlined />, bar: '#1677ff' },
  running: { label: '执行中', color: '#1677ff', icon: <SyncOutlined spin />, bar: '#1677ff' },
  pending: { label: '待审批', color: '#fa8c16', icon: <ClockCircleFilled />, bar: '#ffd591' },
  success: { label: '成功', color: '#52c41a', icon: <CheckCircleFilled />, bar: '#52c41a' },
  failed: { label: '失败', color: '#ff4d4f', icon: <CloseCircleFilled />, bar: '#ff4d4f' },
  canceled: { label: '已取消', color: '#b5bac4', icon: <MinusCircleFilled />, bar: '#d9dde3' },
  interrupted: { label: '已中断', color: '#fa8c16', icon: <PauseCircleFilled />, bar: '#ffd591' },
};

const errText = (e: unknown, fallback: string) =>
  e instanceof ApiError ? e.message : (e as Error)?.message || fallback;

export const BatchRunner: React.FC<BatchRunnerProps> = ({
  kind,
  batchNo,
  tasks,
  concurrency = 3,
  envLabel,
  startTask,
  fetchOutcome,
  cancelRun,
  onTaskDone,
  onFinish,
  onRetryFailed,
  onOpenRecords,
  height = 330,
}) => {
  const { message } = AntApp.useApp();

  const [rt, setRt] = useState<Record<string, Rt>>(() =>
    Object.fromEntries(tasks.map((t) => [t.id, initRt()])),
  );
  const [activeId, setActiveId] = useState<string | undefined>(tasks[0]?.id);
  const [view, setView] = useState<'single' | 'merged'>('single');
  const [follow, setFollow] = useState(true);

  // ------------------------------------------------------------------ 引擎内核
  //
  // 调度状态放在 ref 里（storeRef）而不是 React state：
  // 定时器、SSE 回调、异步提交三条执行流都要读写它，走 state 会拿到过期闭包。
  // storeRef 是唯一真相，flush() 把它镜像进 React 供渲染。
  const storeRef = useRef<Record<string, Rt>>(
    Object.fromEntries(tasks.map((t) => [t.id, initRt()])),
  );
  const streamsRef = useRef<Map<string, { close: () => void }>>(new Map());
  const settledRef = useRef<Set<string>>(new Set());
  const finishedRef = useRef(false);
  const disposedRef = useRef(false);
  const startedAtRef = useRef(Date.now());

  const cbRef = useRef({ startTask, fetchOutcome, cancelRun, onTaskDone, onFinish });
  cbRef.current = { startTask, fetchOutcome, cancelRun, onTaskDone, onFinish };
  const taskRef = useRef(tasks);
  taskRef.current = tasks;

  const flush = useCallback(() => setRt({ ...storeRef.current }), []);

  /** 改一项运行时状态。**必须**走这里，直接改 state 会被下一次 flush 覆盖。 */
  const upd = useCallback((id: string, p: Partial<Rt>) => {
    const cur = storeRef.current[id];
    if (!cur) return;
    storeRef.current[id] = { ...cur, ...p };
  }, []);

  /** 关掉某任务的日志流（终态 / 取消 / 卸载都要调，否则连接一直挂着） */
  const closeStream = useCallback((id: string) => {
    streamsRef.current.get(id)?.close();
    streamsRef.current.delete(id);
  }, []);

  const appendLines = useCallback(
    (id: string, add: LogLineRecord[]) => {
      if (!add.length) return;
      const cur = storeRef.current[id];
      if (!cur) return;
      const merged = [...cur.lines, ...add];
      // 超上限时丢最旧的，保留"最新的现场"
      const kept = merged.length > MAX_LINES ? merged.slice(merged.length - MAX_LINES) : merged;
      upd(id, { lines: kept });
    },
    [upd],
  );

  /**
   * 落终态。所有结束路径（流 end / 提交失败 / 用户取消）都必须汇到这里，
   * 否则会出现"没有计入任何一类"的任务，进度条就永远走不到 100%。
   */
  const settle = useCallback(
    (id: string, status: BatchTaskStatus, extra: Partial<Rt> = {}) => {
      if (settledRef.current.has(id)) return;
      settledRef.current.add(id);
      closeStream(id);
      const cur = storeRef.current[id];
      if (!cur) return;
      const next: Partial<Rt> = {
        status,
        finishedAt: Date.now(),
        progress: status === 'success' ? 100 : cur.progress,
        ...extra,
      };
      upd(id, next);
      flush();

      if (cbRef.current.onTaskDone) {
        const task = taskRef.current.find((t) => t.id === id);
        const after = storeRef.current[id];
        if (task && after) {
          cbRef.current.onTaskDone(
            task,
            {
              ok: status === 'success',
              status: status === 'pending' ? 'pending_approval' : status,
              detail: after.error || after.stage,
            },
            after.lines,
            {
              runId: after.runId ?? '',
              stream: after.stream ?? '',
              runNo: after.runNo,
            },
          );
        }
      }
    },
    [closeStream, upd, flush],
  );

  /** 流结束后回查终态：end 事件只说明"流完了"，成败要看记录 */
  const finalize = useCallback(
    async (id: string) => {
      const cur = storeRef.current[id];
      if (!cur || TERMINAL.includes(cur.status) || !cur.runId) return;
      try {
        const outcome = await cbRef.current.fetchOutcome(cur.runId);
        // 终态到状态的映射放在这里而不是各调用方：
        // 「取消」与「待审批」都不是失败，标成失败会让批次统计说谎。
        const s = (outcome.status || '').toLowerCase();
        const status: BatchTaskStatus = outcome.ok
          ? 'success'
          : s === 'canceled' || s === 'cancelled' || s === 'rolled_back'
            ? 'canceled'
            : s === 'pending_approval'
              ? 'pending'
              : s === 'interrupted'
                ? 'interrupted'
                : 'failed';
        settle(id, status, {
          // 中断也要把那句说明显示出来（"部署被平台重启中断，需重新发起"），
          // 否则队列行只会孤零零写一个「已中断」，用户不知道下一步该干什么。
          error: status === 'failed' || status === 'interrupted' ? outcome.detail : undefined,
          stage: outcome.detail || cur.stage,
        });
      } catch (e) {
        // 查不到终态不能算成功——宁可在界面上留一个失败，也不要假绿
        settle(id, 'failed', { error: errText(e, '未能读取任务终态') });
      }
    },
    [settle],
  );

  const openStream = useCallback(
    (id: string, url: string) => {
      closeStream(id);
      const handle = subscribeLogStream(url, {
        onLog: (line) => appendLines(id, [line]),
        onStatus: (st) => {
          const cur = storeRef.current[id];
          if (!cur) return;
          const next: Partial<Rt> = {
            progress: typeof st.progress === 'number' ? st.progress : cur.progress,
            stage: st.stage || cur.stage,
          };
          // 状态也跟随后端：它说还在排队，界面就显示排队。
          // 只在两个非终态之间切换——终态由 settle / finalize 负责，
          // 不能让一条迟到的 status 事件把已经落定的结果翻回去。
          if (cur.status === 'queued' || cur.status === 'running') {
            if (st.status === 'queued') next.status = 'queued';
            else if (st.status === 'running') next.status = 'running';
          }
          upd(id, next);
        },
        onEnd: () => void finalize(id),
        onError: (msg) => {
          if (msg) upd(id, { stage: msg });
        },
      });
      streamsRef.current.set(id, handle);
    },
    [appendLines, closeStream, finalize, upd],
  );

  /** 提交一个任务：拿句柄 → 挂流 / 落待审批 */
  const submit = useCallback(
    async (task: BatchTaskDef) => {
      try {
        const h = await cbRef.current.startTask(task);
        if (disposedRef.current) return;

        const cur = storeRef.current[task.id];
        if (!cur) return;

        // 提交途中用户点了取消：句柄已经拿到，要补一次真实取消，
        // 否则界面上是"已取消"而后端还在真跑——这是最不能接受的一种谎。
        if (cur.cancelRequested) {
          upd(task.id, { runId: h.runId, runNo: h.runNo, stream: h.stream });
          if (cbRef.current.cancelRun) {
            try {
              await cbRef.current.cancelRun(h.runId);
            } catch (e) {
              upd(task.id, { error: errText(e, '取消失败') });
            }
          }
          settle(task.id, 'canceled');
          return;
        }

        upd(task.id, {
          runId: h.runId,
          runNo: h.runNo,
          stream: h.stream,
          approvers: h.approvers,
        });

        if (h.awaitingApproval) {
          // 待审批不产生执行日志，挂流只会看到一个永远不动的连接
          settle(task.id, 'pending', {
            stage: h.approvers?.length ? `等待 ${h.approvers.join('、')} 审批` : '等待审批',
          });
          return;
        }

        upd(task.id, { status: h.initialStatus === 'queued' ? 'queued' : 'running', stage: h.initialStage || '已提交，等待调度' });
        flush();
        if (h.stream) openStream(task.id, h.stream);
        else void finalize(task.id);
      } catch (e) {
        settle(task.id, 'failed', { error: errText(e, '提交失败') });
      }
    },
    [finalize, flush, openStream, settle, upd],
  );

  // ------------------------------------------------------------------ 调度循环
  useEffect(() => {
    disposedRef.current = false;
    let stopped = false;

    const tick = () => {
      if (stopped || finishedRef.current) return;
      const store = storeRef.current;
      let active = taskRef.current.filter((t) => store[t.id] && occupiesSlot(store[t.id])).length;
      let dirty = false;

      for (const t of taskRef.current) {
        if (active >= concurrency) break;
        const cur = store[t.id];
        // runId 已存在 = 后端已受理这次构建，绝不能再提交一遍。
        // 这里必须显式判 runId：后端把它排进等待队列时状态仍显示"排队中"（queued），
        // 只看 status 会把它当成"还没发出去"的任务反复提交，建出一堆重复构建单。
        if (!cur || cur.status !== 'queued' || cur.runId) continue;
        // 先占槽再发请求：startTask 是异步的，不占槽的话同一个任务会被连发两次
        upd(t.id, { status: 'submitting', startedAt: Date.now(), stage: '正在创建…' });
        active += 1;
        dirty = true;
        void submit(t);
      }

      if (dirty) flush();

      // 全部落终态 → 结算一次
      const allDone = taskRef.current.every((t) => TERMINAL.includes(store[t.id]?.status));
      if (allDone && !finishedRef.current) {
        finishedRef.current = true;
        const list = taskRef.current.map((t) => store[t.id].status);
        cbRef.current.onFinish?.({
          total: taskRef.current.length,
          success: list.filter((s) => s === 'success').length,
          failed: list.filter((s) => s === 'failed').length,
          canceled: list.filter((s) => s === 'canceled').length,
          pending: list.filter((s) => s === 'pending').length,
          interrupted: list.filter((s) => s === 'interrupted').length,
          ms: Date.now() - startedAtRef.current,
        });
        flush();
      }
    };

    const timer = window.setInterval(tick, TICK);
    tick();
    return () => {
      stopped = true;
      window.clearInterval(timer);
    };
  }, [concurrency, flush, submit, upd]);

  // 卸载：掐断所有流，否则离开弹窗后 SSE 还挂着
  useEffect(
    () => () => {
      disposedRef.current = true;
      streamsRef.current.forEach((h) => h.close());
      streamsRef.current.clear();
    },
    [],
  );

  // 跟随运行中任务
  useEffect(() => {
    if (!follow) return;
    const running = tasks.find((t) => rt[t.id]?.status === 'running' || rt[t.id]?.status === 'submitting');
    if (running && running.id !== activeId) setActiveId(running.id);
  }, [rt, follow, tasks, activeId]);

  // ------------------------------------------------------------------ 派生数据
  const counts = useMemo(() => {
    const list = tasks.map((t) => rt[t.id]?.status ?? 'queued');
    return {
      success: list.filter((s) => s === 'success').length,
      failed: list.filter((s) => s === 'failed').length,
      running: list.filter((s) => s === 'submitting' || s === 'running').length,
      queued: list.filter((s) => s === 'queued').length,
      canceled: list.filter((s) => s === 'canceled').length,
      pending: list.filter((s) => s === 'pending').length,
      interrupted: list.filter((s) => s === 'interrupted').length,
    };
  }, [rt, tasks]);

  const donePct = useMemo(() => {
    const done = tasks.reduce((n, t) => {
      const r = rt[t.id];
      if (!r) return n;
      if (r.status === 'success' || r.status === 'pending') return n + 1;
      if (r.status === 'failed' || r.status === 'canceled' || r.status === 'interrupted') return n + 1;
      return n + Math.min(1, Math.max(0, (r.progress || 0) / 100));
    }, 0);
    return Math.round((done / Math.max(1, tasks.length)) * 100);
  }, [rt, tasks]);

  const finished = counts.running === 0 && counts.queued === 0;

  // 全部结束后自动定位到第一个失败任务（用户最关心的是为什么失败）
  const autoFocusedRef = useRef(false);
  useEffect(() => {
    if (autoFocusedRef.current || !finished) return;
    autoFocusedRef.current = true;
    const bad = tasks.find((t) => rt[t.id]?.status === 'failed');
    if (bad) {
      setActiveId(bad.id);
      setFollow(false);
    }
  }, [finished, rt, tasks]);

  const activeTask = tasks.find((t) => t.id === activeId);
  const activeRt = activeId ? rt[activeId] : undefined;

  const mergedLines = useMemo(() => {
    const rows: { ts: string; seq: number; line: number; rec: LogLineRecord; short: string }[] = [];
    tasks.forEach((t, seq) => {
      (rt[t.id]?.lines ?? []).forEach((rec, line) => {
        rows.push({ ts: rec.ts || '', seq, line, rec, short: t.short });
      });
    });
    rows.sort((a, b) => a.ts.localeCompare(b.ts) || a.seq - b.seq || a.line - b.line);
    return rows.map((r) => ({ ...r.rec, line: `${r.short.padEnd(18)} ${r.rec.line}` }));
  }, [rt, tasks]);

  // ------------------------------------------------------------------ 操作

  const [cancelBusy, setCancelBusy] = useState(false);

  /** 取消单个任务。已提交的走后端取消接口，提交中的打标记等句柄回来再取消。 */
  const cancelOne = useCallback(
    async (t: BatchTaskDef) => {
      const cur = storeRef.current[t.id];
      if (!cur || TERMINAL.includes(cur.status)) return;

      if (!cur.runId) {
        upd(t.id, { cancelRequested: true });
        return;
      }
      if (cbRef.current.cancelRun) {
        try {
          await cbRef.current.cancelRun(cur.runId);
        } catch (e) {
          message.error(errText(e, '取消失败'));
          return; // 后端没取消成功，就不能在界面上说"已取消"
        }
      }
      settle(t.id, 'canceled');
    },
    [message, settle, upd],
  );

  const cancelAll = async () => {
    setCancelBusy(true);
    try {
      const pending = tasks.filter((t) => {
        const s = storeRef.current[t.id]?.status;
        return s && !TERMINAL.includes(s);
      });
      await Promise.all(pending.map((t) => cancelOne(t)));
      message.info(`已取消 ${pending.length} 个未完成任务`);
    } finally {
      setCancelBusy(false);
    }
  };

  const listForPanel = view === 'merged' ? mergedLines : activeRt?.lines ?? [];

  // ------------------------------------------------------------------ 渲染

  return (
    <div className="yn-batch">
      {/* 头部：批次信息 + 控制 */}
      <div className="yn-batch__head">
        <div className="yn-batch__head-row">
          <Space size={8} wrap>
            <span className="yn-batch__no">{batchNo}</span>
            <Tag color={kind === 'ci' ? 'blue' : 'geekblue'} style={{ marginInlineEnd: 0 }}>
              {kind === 'ci' ? '批量镜像打包' : '排队发布'}
            </Tag>
            {envLabel ? (
              <Tag color="purple" style={{ marginInlineEnd: 0 }}>
                目标环境 · {envLabel}
              </Tag>
            ) : null}
            <Text type="secondary" style={{ fontSize: 12 }}>
              {kind === 'ci'
                ? `并发 ${concurrency} 个任务 · 由平台后端实际执行`
                : concurrency > 1
                  ? `并发 ${concurrency} 个服务 · 逐服务滚动更新`
                  : `串行发布 · 逐服务滚动更新`}
            </Text>
          </Space>

          <Space size={8}>
            {onOpenRecords ? (
              <Button size="small" onClick={onOpenRecords}>
                查看{kind === 'ci' ? '构建' : '部署'}记录
              </Button>
            ) : null}
            <Button
              size="small"
              danger
              icon={<StopOutlined />}
              loading={cancelBusy}
              onClick={cancelAll}
              disabled={finished}
            >
              全部取消
            </Button>
          </Space>
        </div>

        <div className="yn-batch__meters">
          <div className="yn-batch__meter-main">
            <div className="yn-batch__meter-label">
              <span>批次总进度</span>
              <b>{donePct}%</b>
            </div>
            <Progress
              percent={donePct}
              showInfo={false}
              strokeColor={{ from: '#1677ff', to: '#13c2c2' }}
              trailColor="#eef1f5"
              size="small"
            />
          </div>

          <div className="yn-batch__chips">
            <span className="yn-batch__chip yn-batch__chip--run">
              <SyncOutlined spin={counts.running > 0} /> 执行中 <b>{counts.running}</b>
            </span>
            <span className="yn-batch__chip yn-batch__chip--ok">
              <CheckCircleFilled /> 成功 <b>{counts.success}</b>
            </span>
            <span className="yn-batch__chip yn-batch__chip--fail">
              <CloseCircleFilled /> 失败 <b>{counts.failed}</b>
            </span>
            <span className="yn-batch__chip yn-batch__chip--wait">
              <ClockCircleFilled /> 排队 <b>{counts.queued}</b>
            </span>
            {counts.pending ? (
              <span className="yn-batch__chip" style={{ color: '#fa8c16' }}>
                <ClockCircleFilled /> 待审批 <b>{counts.pending}</b>
              </span>
            ) : null}
            {counts.canceled ? (
              <span className="yn-batch__chip">
                <StopOutlined /> 取消 <b>{counts.canceled}</b>
              </span>
            ) : null}
            {/* 已中断只在真发生过时出现：混进「失败」会让这批看起来是代码/配置的问题 */}
            {counts.interrupted ? (
              <span className="yn-batch__chip" style={{ color: '#fa8c16' }}>
                <PauseCircleFilled /> 已中断 <b>{counts.interrupted}</b>
              </span>
            ) : null}
          </div>

          <div className="yn-batch__eta">
            <span>
              已用时 <b>{Math.max(0, Math.round((Date.now() - startedAtRef.current) / 1000))}s</b>
            </span>
            <span className="yn-batch__eta-sep" />
            <span>
              状态 <b>{finished ? '已结束' : '执行中'}</b>
            </span>
          </div>
        </div>
      </div>

      {/* 主体：队列 + 日志 */}
      <div className="yn-batch__body">
        <div className="yn-batch__queue">
          <div className="yn-batch__queue-head">
            <span>任务队列（{tasks.length}）</span>
            <Text type="secondary" style={{ fontSize: 11 }}>
              点击查看日志
            </Text>
          </div>
          <div className="yn-batch__queue-list">
            {tasks.map((t) => {
              const r = rt[t.id] ?? initRt();
              const st = STATUS_STYLE[r.status];
              const pct =
                r.status === 'success' || r.status === 'pending'
                  ? 100
                  : r.status === 'canceled'
                    ? 0
                    : Math.min(100, Math.max(0, r.progress || 0));
              const sec = r.startedAt
                ? Math.max(0, Math.round(((r.finishedAt || Date.now()) - r.startedAt) / 1000))
                : 0;
              /**
               * 队列行里显示哪一句话。
               *
               * `stage` 是"进度播报"，最后写它的地方是提交前那句「正在创建…」。
               * 一旦提交本身失败（后端拒绝：已有进行中部署、没有可部署镜像…），
               * 任务就停在 submitting→failed，stage 永远停在「正在创建…」，
               * 于是队列里那一行写着"失败 · 正在创建…"——把失败原因藏起来了，
               * 只有点进去才在右侧看到红条。失败/取消时用 error 顶掉 stage。
               */
              const note =
                (r.status === 'failed' || r.status === 'canceled' || r.status === 'interrupted') && r.error
                  ? r.error
                  : r.stage;
              const closable = !TERMINAL.includes(r.status);
              return (
                <div
                  key={t.id}
                  className={`yn-batch__item ${activeId === t.id ? 'is-active' : ''} yn-batch__item--${r.status}`}
                  onClick={() => {
                    setActiveId(t.id);
                    setFollow(false);
                    setView('single');
                  }}
                >
                  <span className="yn-batch__item-icon" style={{ color: st.color }}>
                    {st.icon}
                  </span>
                  <div className="yn-batch__item-main">
                    <div className="yn-batch__item-title">
                      <span className="yn-ellipsis">{t.title}</span>
                      <em>{t.subtitle}</em>
                    </div>
                    <div className="yn-batch__item-track">
                      <i style={{ width: `${pct}%`, background: r.status === 'running' ? '#1677ff' : st.bar }} />
                    </div>
                    <div className="yn-batch__item-meta">
                      <span style={{ color: st.color }}>{st.label}</span>
                      {note ? (
                        <span
                          className="yn-ellipsis"
                          title={note}
                          style={
                            (r.status === 'failed' || r.status === 'canceled' || r.status === 'interrupted') && r.error
                              ? // 中断不是错误，别用报错的红色 —— 它只是"没跑完"
                                { color: r.status === 'interrupted' ? '#fa8c16' : '#ff4d4f' }
                              : undefined
                          }
                        >
                          {note}
                        </span>
                      ) : null}
                      {sec ? <span>{sec}s</span> : null}
                      {r.runNo ? <code className="yn-batch__item-tag">{r.runNo}</code> : null}
                    </div>
                  </div>
                  {closable ? (
                    <Tooltip title="取消该任务">
                      <Button
                        type="text"
                        size="small"
                        className="yn-batch__item-x"
                        icon={<StopOutlined />}
                        onClick={(e) => {
                          e.stopPropagation();
                          void cancelOne(t);
                        }}
                      />
                    </Tooltip>
                  ) : null}
                </div>
              );
            })}
          </div>
        </div>

        <div className="yn-batch__view">
          <div className="yn-batch__view-head">
            <Segmented
              size="small"
              value={view}
              onChange={(v) => setView(v as 'single' | 'merged')}
              options={[
                { label: '当前任务', value: 'single', icon: <ProfileOutlined /> },
                { label: '合并视图', value: 'merged', icon: <MergeCellsOutlined /> },
              ]}
            />
            {view === 'single' && activeTask ? (
              <Space size={6}>
                <b style={{ fontSize: 12 }}>{activeTask.short}</b>
                {activeRt ? (
                  <Tag
                    color={
                      activeRt.status === 'success'
                        ? 'success'
                        : activeRt.status === 'failed'
                          ? 'error'
                          : activeRt.status === 'pending'
                            ? 'warning'
                            : activeRt.status === 'running' || activeRt.status === 'submitting'
                              ? 'processing'
                              : 'default'
                    }
                    style={{ marginInlineEnd: 0 }}
                  >
                    {STATUS_STYLE[activeRt.status].label}
                  </Tag>
                ) : null}
                {activeRt?.runNo ? <code className="yn-batch__item-tag">{activeRt.runNo}</code> : null}
              </Space>
            ) : (
              <Text type="secondary" style={{ fontSize: 12 }}>
                全部任务日志按后端时间戳合并（{mergedLines.length} 行）
              </Text>
            )}
            <span className="yn-batch__view-spacer" />
            <Space size={4}>
              <Text type="secondary" style={{ fontSize: 12 }}>
                跟随执行中任务
              </Text>
              <Switch size="small" checked={follow} onChange={setFollow} />
            </Space>
          </div>

          {activeRt?.error && view === 'single' ? (
            <div className="yn-batch__error">
              <CloseCircleFilled style={{ color: '#ff4d4f' }} />
              <span className="yn-ellipsis">{activeRt.error}</span>
            </div>
          ) : null}

          <LogPanel
            lines={listForPanel}
            height={height}
            live={activeRt?.status === 'running'}
            emptyText={
              activeRt?.status === 'queued'
                ? '该任务尚未开始执行'
                : activeRt?.status === 'pending'
                  ? '待审批任务不产生执行日志，审批通过后才会开始发布'
                  : view === 'merged'
                    ? '暂无日志'
                    : '暂无日志'
            }
          />

          {finished && (counts.failed > 0 || counts.interrupted > 0) && onRetryFailed ? (
            <div className="yn-batch__retry">
              <ThunderboltFilled style={{ color: '#faad14' }} />
              {/* 中断项也要能一键带回：它们不在 lastOk 里，重试链路本来就覆盖得到，
                  只是入口原先只对 failed 开放，于是中断的任务只能手工一个个重挑。 */}
              <span>
                {counts.failed > 0 ? `${counts.failed} 个任务失败` : ''}
                {counts.failed > 0 && counts.interrupted > 0 ? '、' : ''}
                {counts.interrupted > 0 ? `${counts.interrupted} 个任务被平台重启中断` : ''}
                ，可重试未成功项，或到记录页查看 AI 归因建议
              </span>
              <Button size="small" type="primary" ghost onClick={onRetryFailed}>
                重试未成功项
              </Button>
            </div>
          ) : null}
        </div>
      </div>
    </div>
  );
};

export default BatchRunner;
