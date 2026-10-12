// ---------------------------------------------------------------------------
// SSE 读取（fetch 版）
//
// 为什么不用原生 EventSource：
//   1. EventSource 只能 GET，而 `/ai/chat` 是 **POST + SSE**（消息体里带问题）；
//   2. EventSource 不能自定义请求头，无法带 Authorization。
//
// 那 GET 类日志流为什么仍用 EventSource？因为原生实现自带**断线重连**与
// `Last-Event-ID` 续传（后端已按 seq 实现续传），自己重写这轮子不划算。
// 结论：GET 流用 EventSource（见 hooks/useLogStream），POST 流用本文件。

import { API_BASE, refreshAccessToken, tokenStore } from './client';
import type { LogLineRecord } from '@/types';

export interface SSEFrame {
  /** 事件名，对应后端 `event:` 字段 */
  event: string;
  /** 数据体（多行 data: 已按规范用 \n 拼接） */
  data: string;
  /** 事件 id，对应后端 `id:` 字段（日志流里是 seq） */
  id?: string;
}

export interface OpenSSEOptions {
  method?: 'GET' | 'POST';
  body?: unknown;
  /** `?token=` 这类查询串由调用方拼好 */
  signal?: AbortSignal;
  /** 每收到一帧回调一次 */
  onFrame: (f: SSEFrame) => void;
  /** 连接关闭（正常或异常）时回调一次；err 非空表示异常中断 */
  onClose?: (err?: Error) => void;
}
/** 解析一帧原始报文（已按空行切分）。返回 null 表示该帧无有效内容（纯注释/心跳）。 */
function parseFrame(raw: string): SSEFrame | null {
  let event = 'message';
  let id: string | undefined;
  const dataLines: string[] = [];

  for (const line of raw.split('\n')) {
    if (!line || line.startsWith(':')) continue; // `: ping` 心跳，忽略
    const colon = line.indexOf(':');
    const field = colon === -1 ? line : line.slice(0, colon);
    // 规范：冒号后若有一个空格要去掉
    let value = colon === -1 ? '' : line.slice(colon + 1);
    if (value.startsWith(' ')) value = value.slice(1);

    if (field === 'event') event = value;
    else if (field === 'data') dataLines.push(value);
    else if (field === 'id') id = value;
    // retry 字段会由 EventSource 语义处理；fetch 版由调用方自己决定重连策略，这里忽略
  }

  if (!dataLines.length) return null;
  return { event, data: dataLines.join('\n'), id };
}

/**
 * 以 fetch 建立 SSE 连接并逐帧回调。
 *
 * 注意：本函数返回的 Promise 在**流结束时**才 resolve，不是收到首帧就 resolve。
 * 页面里若要并发做别的事，别 await 它。
 */
export async function openSSE(path: string, opts: OpenSSEOptions): Promise<void> {
  const { method = 'GET', body, signal, onFrame, onClose } = opts;

  try {
    // fetch 版可以自定义请求头，所以这里走 Authorization（不像 EventSource 只能把令牌塞进 URL）
    const headers: Record<string, string> = {};
    const token = tokenStore.get();
    if (token) headers.Authorization = `Bearer ${token}`;
    if (body !== undefined) headers['Content-Type'] = 'application/json';

    const res = await fetch(`${API_BASE}${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
    });

    if (!res.ok || !res.body) {
      throw new Error(`SSE 连接失败（HTTP ${res.status}）`);
    }

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = '';

    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      // 统一换行，避免 \r\n\r\n 切帧失败
      buf += decoder.decode(value, { stream: true }).replace(/\r\n/g, '\n');

      for (;;) {
        const idx = buf.indexOf('\n\n');
        if (idx < 0) break;
        const raw = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        const frame = parseFrame(raw);
        if (frame) onFrame(frame);
      }
    }

    // 流结束前残留的最后一帧（后端正常收尾时一般不会出现，防御性处理）
    const tail = parseFrame(buf);
    if (tail) onFrame(tail);
    onClose?.();
  } catch (e) {
    const err = e as Error;
    // 主动 abort 不算异常——页面切走/组件卸载都会走到这里
    if (err?.name === 'AbortError') onClose?.();
    else onClose?.(err);
  }
}

// ---------------------------------------------------------------------------
// 日志流订阅（EventSource 版）
//
// 后端四条日志流（构建 / 部署 / 回滚 / Pod）共用同一套事件契约：
//   log    {seq,ts,level,line}                        一行日志
//   status {status,progress,stage,duration,image?}    状态变化（含终态）
//   end    {reason}                                   流结束（finished/canceled/failed/timeout）
//
// 这个契约值得只有一个实现：日志面板、批量执行引擎都靠它，
// 分别写一遍的结果就是"某天后端多了一个事件，只有一处跟着改"。
// EventSource 相对 fetch 版多拿两样东西：断线自动重连、`Last-Event-ID` 续传
// （后端按 seq 补发缺失日志，刷新页面也能接上历史）。
// ---------------------------------------------------------------------------

/** 订阅句柄。close() 是幂等的，重复调用不会抛错。 */
export interface LogStreamHandle {
  close: () => void;
}

export interface LogStreamHandlers {
  /** 一行日志（后端会先补发历史 backlog，再推实时） */
  onLog?: (line: LogLineRecord) => void;
  onStatus?: (st: { status: string; progress?: number; stage?: string; duration?: number; image?: string }) => void;
  /** 流正常结束。reason: finished / canceled / failed / timeout */
  onEnd?: (reason: string) => void;
  /**
   * 连接异常。EventSource 会自己重连，所以收到它不等于失败——
   * 页面可以据此显示"正在重连"，但**不要**在这里判定任务失败。
   */
  onError?: (message: string) => void;
}

/**
 * 订阅一条后端日志流。返回的句柄在组件卸载 / 任务结束时必须 close()，
 * 否则 EventSource 会一直挂在连接上（后端也一直有一个订阅者）。
 */
export function subscribeLogStream(url: string, h: LogStreamHandlers): LogStreamHandle {
  const es = new EventSource(url);
  let closed = false;

  const shutdown = () => {
    if (closed) return;
    closed = true;
    es.close();
  };

  es.addEventListener('log', (ev) => {
    try {
      h.onLog?.(JSON.parse((ev as MessageEvent).data) as LogLineRecord);
    } catch {
      // 忽略脏帧，不能让一行坏数据终止整个日志面板
    }
  });

  es.addEventListener('status', (ev) => {
    try {
      h.onStatus?.(JSON.parse((ev as MessageEvent).data));
    } catch {
      /* 同上 */
    }
  });

  es.addEventListener('end', (ev) => {
    let reason = 'finished';
    try {
      reason = (JSON.parse((ev as MessageEvent).data) as { reason?: string })?.reason ?? 'finished';
    } catch {
      /* 保持默认 */
    }
    // 后端已明确结束，主动断开，避免 EventSource 反复重连到一个已完结的流
    shutdown();
    h.onEnd?.(reason);
  });

  es.onerror = () => {
    // readyState=CLOSED 说明浏览器放弃了重连（如 401、404）
    h.onError?.(es.readyState === EventSource.CLOSED ? '日志流已断开' : '连接中断，正在重连…');
  };
  es.onopen = () => h.onError?.('');

  return { close: shutdown };
}
