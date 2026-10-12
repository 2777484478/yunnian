import React from 'react';
import { Button, Dropdown } from 'antd';
import { SyncOutlined } from '@ant-design/icons';
import { fmt } from '@/utils/format';

// ---------------------------------------------------------------------------
// AutoRefreshButton —— 列表页头部的「自动刷新」控件
//
// 做成「按钮 + 下拉」而不是一个纯开关：间隔该多长取决于用户当下在干什么
// （盯着构建看 vs 开着页面干别的），写死一个值总有人要跟它较劲。
//
// 三个可选间隔 + 关闭。**默认 3 秒**——构建/部署这类状态的最小变化粒度就是
// 秒级，再慢就看不出"活"；真要长时间开着，往下调到 10s/30s 更省。
//
// 状态说明放在下拉面板顶部（上次刷新时间、后台刷新是否失败）：这几个字是回答
// "我看到的这份数据有多新"的唯一地方，藏进 Tooltip 反而没人看得到。
// ---------------------------------------------------------------------------

/** 预设间隔（毫秒） */
const PRESETS: { ms: number; label: string }[] = [
  { ms: 3000, label: '每 3 秒' },
  { ms: 5000, label: '每 5 秒' },
  { ms: 10000, label: '每 10 秒' },
  { ms: 30000, label: '每 30 秒' },
];

const OFF_KEY = 'off';
const STATUS_KEY = 'status';

export interface AutoRefreshButtonProps {
  /** 当前间隔（毫秒）；null = 已关闭 */
  value: number | null;
  onChange: (ms: number | null) => void;
  /** 最近一次**成功**取数的时间戳 */
  updatedAt?: number | null;
  /** 后台刷新最近一次失败的原因；有值就说明"现在看到的是旧数据" */
  errorText?: string;
}

const AutoRefreshButton: React.FC<AutoRefreshButtonProps> = ({ value, onChange, updatedAt, errorText }) => {
  const on = value !== null;
  const failed = on && !!errorText;

  const statusNode = (
    <div style={{ maxWidth: 260, fontSize: 12, lineHeight: '18px', whiteSpace: 'normal' }}>
      <div style={{ color: 'rgba(0,0,0,0.65)' }}>
        {on ? `每 ${value! / 1000} 秒自动刷新；页面切到后台时暂停` : '已关闭，列表需要手动刷新'}
      </div>
      <div style={{ color: 'rgba(0,0,0,0.45)' }}>
        {updatedAt ? `上次刷新 ${fmt(updatedAt)}` : '进入页面后还没有成功刷新过'}
      </div>
      {failed ? (
        <div style={{ color: '#cf1322' }}>上次自动刷新失败：{errorText}（正在重试）</div>
      ) : null}
    </div>
  );

  return (
    <Dropdown
      trigger={['click']}
      menu={{
        selectable: true,
        selectedKeys: [on ? String(value) : OFF_KEY],
        items: [
          { key: STATUS_KEY, label: statusNode, disabled: true },
          { type: 'divider' },
          ...PRESETS.map((p) => ({ key: String(p.ms), label: p.label })),
          { type: 'divider' },
          { key: OFF_KEY, label: '关闭自动刷新' },
        ],
        onClick: ({ key }) => {
          if (key === STATUS_KEY) return;
          onChange(key === OFF_KEY ? null : Number(key));
        },
      }}
    >
      <Button
        icon={<SyncOutlined style={failed ? { color: '#ff4d4f' } : on ? { color: '#1677ff' } : undefined} />}
      >
        {on ? `自动刷新 ${value! / 1000}s` : '自动刷新'}
      </Button>
    </Dropdown>
  );
};

export default AutoRefreshButton;
