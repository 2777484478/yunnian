import React from 'react';
import { Tooltip } from 'antd';

/**
 * 失败真因（后端 `errorMessage`）。
 *
 * 为什么必须有它：`errorCode` 只是机器码（`TIMEOUT` / `ROLLOUT_STUCK` / `QUEUE_REJECTED`），
 * 解释不了现场。而真因以前**只活在日志流里** —— 日志流是进程内 + TTL 的，平台一重启
 * 那条失败记录就只剩一个机器码，没人说得清当时为什么失败（构建侧与部署侧都是这个毛病）。
 * 2026-10-10 起后端把真因落库（`build_record` / `deploy_record` 的 `error_message`）并下发。
 *
 * 兜底顺序刻意是「真因 → 错误码 → 一句实话」：
 *   · 落这列之前产生的旧记录没有 `errorMessage`，此时显示错误码比留白有用；
 *   · 两者都没有时**明说「未记录」**，不要留白 —— 留白会被读成"这条没有失败原因"，
 *     而事实是"我们当时没把它记下来"，这是相反的结论。
 */
const FailureReason: React.FC<{
  message?: string;
  code?: string;
  /** 与状态标签同色：失败=红(`#ff4d4f`)、中断=橙(`#fa8c16`)。 */
  tone?: 'error' | 'warning';
  maxWidth?: number;
}> = ({ message, code, tone = 'error', maxWidth = 136 }) => {
  const text = (message || '').trim();
  const fallback = (code || '').trim();
  const shown = text || fallback;

  if (!shown) {
    return (
      <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 3 }} title="落这列之前的旧记录没有存下失败原因">
        未记录失败原因
      </div>
    );
  }

  return (
    <Tooltip
      title={
        // 真因多为集群/下游的原文，可能带换行与很长的对象名；`pre-wrap` 让它在提示里可读。
        text ? <span style={{ whiteSpace: 'pre-wrap' }}>{text}</span> : `错误码：${fallback}`
      }
    >
      <div
        style={{
          fontSize: 12,
          color: tone === 'warning' ? '#fa8c16' : '#ff4d4f',
          marginTop: 3,
          maxWidth,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
        }}
      >
        {shown}
      </div>
    </Tooltip>
  );
};

export default FailureReason;
