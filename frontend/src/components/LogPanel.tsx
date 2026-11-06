import React, { useEffect, useMemo, useRef, useState } from 'react';
import { Button, Input, Space, Switch, Tooltip, Typography, message } from 'antd';
import {
  CloseOutlined,
  CopyOutlined,
  DownloadOutlined,
  SearchOutlined,
  VerticalAlignBottomOutlined,
} from '@ant-design/icons';
import type { LogLineRecord } from '@/types';

const { Text } = Typography;

/**
 * 日志行入参。两种形态都支持：
 * - 纯文本行（`string[]`）：批量任务脚本、静态示例日志；
 * - 结构化行（`LogLineRecord[]`）：后端构建/部署/回滚/Pod 日志接口与 SSE `log` 事件的形状，
 *   自带 `level` 与 `ts`，无需再靠文本猜级别。
 */
export type LogLineInput = string | LogLineRecord;

type LineTone = 'error' | 'success' | 'cmd' | 'warn' | 'plain';

/** 按内容着色：命令行 / 错误 / 成功 / 时间戳 */
function classify(line: string): LineTone {
  if (/ERROR|error:|failed|Failed|ERR!|退出码/.test(line)) return 'error';
  if (/✅|成功|SUCCESS|successfully/.test(line)) return 'success';
  if (/\$\s|^\[build\]|>\s/.test(line)) return 'cmd';
  if (/WARN|warn/.test(line)) return 'warn';
  return 'plain';
}

/**
 * 着色优先级：后端给了明确 level 就采信（它的词汇表 info/cmd/error/warn/success
 * 与本组件的配色分类一致），只在 level 为空或未知时才回退到文本判定。
 */
function toneOf(level: string | undefined, text: string): LineTone {
  switch ((level || '').toLowerCase()) {
    case 'error':
    case 'fatal':
      return 'error';
    case 'warn':
    case 'warning':
      return 'warn';
    case 'cmd':
      return 'cmd';
    case 'success':
      return 'success';
    default:
      return classify(text);
  }
}

/** ISO 时间戳（如 2026-09-23T15:11:38+08:00）裁到 HH:mm:ss；没有时间信息则留空 */
function clockOf(ts?: string): string {
  if (!ts) return '';
  const m = ts.match(/\d{2}:\d{2}:\d{2}/);
  return m ? m[0] : ts;
}

interface NormalizedLine {
  text: string;
  tone: LineTone;
  clock: string;
}

function normalize(lines: LogLineInput[]): NormalizedLine[] {
  return lines.map((l) =>
    typeof l === 'string'
      ? { text: l, tone: classify(l), clock: '' }
      : { text: l.line, tone: toneOf(l.level, l.line), clock: clockOf(l.ts) },
  );
}

export interface LogPanelProps {
  lines: LogLineInput[];
  height?: number;
  /** 是否处于"实时输出"状态，会启动逐行追加动画（日志一次性给定） */
  streaming?: boolean;
  /** 是否接的是实时流（日志由外部持续追加，如批量任务的 SSE 通道） */
  live?: boolean;
  streamingSpeed?: number;
  title?: React.ReactNode;
  extra?: React.ReactNode;
  emptyText?: string;
  /**
   * 暂停追踪（仅实时流场景）。传了 `onPausedChange` 才会显示这个开关 ——
   * 静态日志没有"追踪"可言，多一个开关只会让人以为点了会有效果。
   *
   * 与旧的 "跟随（自动滚动）" 的区别：
   *   跟随 = 有新行时把视口滚到底部（纯显示层，日志照样进）；
   *   追踪 = 要不要继续接收新日志（数据层，暂停时新行不进来）。
   */
  paused?: boolean;
  onPausedChange?: (paused: boolean) => void;
  /** 暂停期间已缓冲、尚未显示的行数 */
  buffered?: number;
}

/**
 * 日志查看器：模拟"实时推送"效果（对应后端 SSE / WebSocket 通道）
 * 真实接入时把 streaming 由接口事件驱动即可，组件无需改动。
 */
export const LogPanel: React.FC<LogPanelProps> = ({
  lines,
  height = 380,
  streaming = false,
  live = false,
  streamingSpeed = 90,
  title,
  extra,
  emptyText = '暂无日志',
  paused = false,
  onPausedChange,
  buffered = 0,
}) => {
  const [keyword, setKeyword] = useState('');
  const [autoScroll, setAutoScroll] = useState(true);
  const [visibleCount, setVisibleCount] = useState(streaming ? 1 : lines.length);
  const boxRef = useRef<HTMLDivElement>(null);
  // 记住"这次暂停是滚动触发的"还是用户点的开关：
  // 只有滚动触发的暂停，才会在滚回底部时自动恢复；
  // 用户手动暂停的，滚到底也不该自作主张恢复。
  const autoPausedRef = useRef(false);

  // 统一成 { 文本, 配色, 时刻 } —— 下游逻辑只认这一种形状
  const rows = useMemo(() => normalize(lines), [lines]);

  useEffect(() => {
    setVisibleCount(streaming ? 1 : rows.length);
  }, [rows, streaming]);

  useEffect(() => {
    if (!streaming) return;
    if (visibleCount >= rows.length) return;
    const t = setTimeout(() => setVisibleCount((n) => n + 1), streamingSpeed);
    return () => clearTimeout(t);
  }, [streaming, visibleCount, rows.length, streamingSpeed]);

  const shown = useMemo(() => rows.slice(0, visibleCount), [rows, visibleCount]);

  const display = useMemo(() => {
    if (!keyword.trim()) return shown.map((r, i) => ({ r, i }));
    const k = keyword.toLowerCase();
    return shown.map((r, i) => ({ r, i })).filter((x) => x.r.text.toLowerCase().includes(k));
  }, [shown, keyword]);

  useEffect(() => {
    if (autoScroll && boxRef.current) {
      boxRef.current.scrollTop = boxRef.current.scrollHeight;
    }
  }, [display.length, autoScroll]);

  /**
   * 滚动时自动暂停/恢复追踪。
   *
   * 为什么需要：追踪开着时新日志一来就把视口拽到底部，你正看第 300 行的报错根本读不完。
   * 所以往上翻就自动停住（常见的终端行为），滚回底部再自动续上。
   * 只对"滚动触发的暂停"负责 —— 用户手动点的暂停，滚回底部也不恢复。
   */
  const handleScroll = () => {
    const el = boxRef.current;
    if (!el || !onPausedChange) return;
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
    if (!atBottom && !paused) {
      autoPausedRef.current = true;
      onPausedChange(true);
    } else if (atBottom && paused && autoPausedRef.current) {
      autoPausedRef.current = false;
      onPausedChange(false);
    }
  };

  const done = visibleCount >= rows.length;

  /** 导出/复制时把时刻拼回行首，保留时序信息 */
  const plainText = useMemo(
    () => rows.map((r) => (r.clock ? `${r.clock} ${r.text}` : r.text)).join('\n'),
    [rows],
  );

  return (
    <div className="yn-logpanel">
      <div className="yn-logpanel__bar">
        <div className="yn-logpanel__bar-left">
          <span className="yn-logdot" style={{ background: '#ff5f57' }} />
          <span className="yn-logdot" style={{ background: '#febc2e' }} />
          <span className="yn-logdot" style={{ background: '#28c840' }} />
          {title ? <span className="yn-logpanel__title">{title}</span> : null}
          {paused ? (
            <span className="yn-logpanel__done">
              已暂停{buffered > 0 ? ` · 待显示 ${buffered} 行` : ''}
            </span>
          ) : (streaming && !done) || live ? (
            <span className="yn-logpanel__live">
              <span className="yn-logpanel__pulse" />
              LIVE
            </span>
          ) : (
            <span className="yn-logpanel__done">{rows.length} 行 · 已结束</span>
          )}
        </div>
        <div className="yn-logpanel__bar-right">
          <Input
            size="small"
            allowClear
            prefix={<SearchOutlined style={{ color: '#8c8c8c' }} />}
            placeholder="搜索关键字"
            value={keyword}
            onChange={(e) => setKeyword(e.target.value)}
            style={{ width: 168 }}
          />
          {onPausedChange ? (
            // 实时流场景：用「自动追踪 / 暂停追踪」一个开关说两件事 ——
            // 要不要继续收新日志，以及（收的时候）视口是否跟着走。
            // 与之并列的旧「跟随」开关在这里会重复，所以只在没有 onPausedChange 时显示。
            <Tooltip title={paused ? '继续接收新日志（会补齐暂停期间的行）' : '暂停接收，方便查看已有日志'}>
              <Space size={4}>
                <Switch size="small" checked={!paused} onChange={(v) => {
                  autoPausedRef.current = false;
                  onPausedChange(!v);
                }} />
                <Text style={{ fontSize: 12, color: paused ? '#d48806' : '#b5bac4' }}>
                  {paused ? '暂停追踪' : '自动追踪'}
                </Text>
              </Space>
            </Tooltip>
          ) : (
            <Tooltip title="自动滚动到底部">
              <Space size={4}>
                <Switch size="small" checked={autoScroll} onChange={setAutoScroll} />
                <Text style={{ fontSize: 12, color: '#b5bac4' }}>跟随</Text>
              </Space>
            </Tooltip>
          )}
          <Tooltip title="复制全部">
            <Button
              type="text"
              size="small"
              style={{ color: '#b5bac4' }}
              icon={<CopyOutlined />}
              onClick={() => {
                navigator.clipboard?.writeText(plainText);
                message.success('日志已复制到剪贴板');
              }}
            />
          </Tooltip>
          <Tooltip title="下载日志">
            <Button
              type="text"
              size="small"
              style={{ color: '#b5bac4' }}
              icon={<DownloadOutlined />}
              onClick={() => {
                const blob = new Blob([plainText], { type: 'text/plain;charset=utf-8' });
                const a = document.createElement('a');
                a.href = URL.createObjectURL(blob);
                a.download = `yunnian-log-${Date.now()}.log`;
                a.click();
                URL.revokeObjectURL(a.href);
                message.success('日志已开始下载');
              }}
            />
          </Tooltip>
          {extra}
        </div>
      </div>

      <div className="yn-logpanel__body" ref={boxRef} style={{ height }} onScroll={handleScroll}>
        {display.length === 0 ? (
          <div className="yn-logpanel__empty">{keyword ? '未匹配到日志行' : emptyText}</div>
        ) : (
          display.map(({ r, i }) => (
            <div className={`yn-logline yn-logline--${r.tone}`} key={i}>
              <span className="yn-logline__no">{i + 1}</span>
              {r.clock ? <span className="yn-logline__ts">{r.clock}</span> : null}
              <span className="yn-logline__text">{r.text || '\u00a0'}</span>
            </div>
          ))
        )}
        {streaming && !done ? <div className="yn-logpanel__cursor">▊</div> : null}
      </div>

      <div className="yn-logpanel__footer">
        <Button
          type="link"
          size="small"
          icon={<VerticalAlignBottomOutlined />}
          onClick={() => {
            setVisibleCount(rows.length);
            // 「跳到最新」在暂停状态下也顺手恢复追踪 —— 用户的意图就是"我要看最新的"，
            // 只滚到底但日志不再进来，等于点了个寂寞。
            if (paused && onPausedChange) {
              autoPausedRef.current = false;
              onPausedChange(false);
            }
          }}
          disabled={!live && done && !paused}
        >
          跳到最新
        </Button>
        <Text style={{ fontSize: 12, color: '#8c8c8c' }}>
          {paused
            ? `已暂停 · 待显示 ${buffered} 行（点「自动追踪」补齐）`
            : live
              ? `已输出 ${rows.length} 行 · 持续接收中`
              : `已加载 ${shown.length} / ${rows.length} 行`}
        </Text>
      </div>
    </div>
  );
};

export default LogPanel;
export { CloseOutlined };
