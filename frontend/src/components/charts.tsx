import React from 'react';

// ---------------------------------------------------------------------------
// 轻量手写 SVG 图表（避免引入 echarts 这类重依赖，原型阶段够用且体积可控）
// 配色遵循浅色主题：白底、深色文字、蓝色主色
// ---------------------------------------------------------------------------

export interface SeriesPoint {
  label: string;
  value: number;
}

/** 面积折线图（可叠加第二条柱/线） */
export const AreaChart: React.FC<{
  data: SeriesPoint[];
  height?: number;
  color?: string;
  secondary?: SeriesPoint[];
  secondaryColor?: string;
  unit?: string;
  showGrid?: boolean;
}> = ({
  data,
  height = 200,
  color = '#1677ff',
  secondary,
  secondaryColor = '#ff4d4f',
  unit = '',
  showGrid = true,
}) => {
  const width = 760;
  const padL = 40;
  const padR = 16;
  const padT = 14;
  const padB = 26;
  const innerW = width - padL - padR;
  const innerH = height - padT - padB;

  const max = Math.max(...data.map((d) => d.value), ...(secondary?.map((d) => d.value) ?? [0])) * 1.15 || 1;
  const x = (i: number) => padL + (innerW * i) / Math.max(1, data.length - 1);
  const y = (v: number) => padT + innerH - (innerH * v) / max;

  const line = data.map((d, i) => `${i === 0 ? 'M' : 'L'}${x(i).toFixed(1)},${y(d.value).toFixed(1)}`).join(' ');
  const area = `${line} L${x(data.length - 1).toFixed(1)},${padT + innerH} L${padL},${padT + innerH} Z`;

  const ticks = 4;
  const gridLines = Array.from({ length: ticks + 1 }, (_, i) => {
    const v = (max / ticks) * i;
    return { v, y: y(v) };
  });

  return (
    <svg viewBox={`0 0 ${width} ${height}`} width="100%" height={height} role="img" aria-label="趋势图">
      <defs>
        <linearGradient id="yn-area" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor={color} stopOpacity="0.22" />
          <stop offset="100%" stopColor={color} stopOpacity="0.02" />
        </linearGradient>
      </defs>

      {showGrid &&
        gridLines.map((g, i) => (
          <g key={i}>
            <line x1={padL} y1={g.y} x2={width - padR} y2={g.y} stroke="#eef0f3" strokeWidth="1" />
            <text x={padL - 8} y={g.y + 4} textAnchor="end" fontSize="11" fill="#9aa0aa">
              {Math.round(g.v)}
            </text>
          </g>
        ))}

      {secondary?.length ? (
        <path
          d={secondary
            .map((d, i) => `${i === 0 ? 'M' : 'L'}${x(i).toFixed(1)},${y(d.value).toFixed(1)}`)
            .join(' ')}
          fill="none"
          stroke={secondaryColor}
          strokeWidth="1.6"
          strokeDasharray="4 3"
        />
      ) : null}

      <path d={area} fill="url(#yn-area)" />
      <path d={line} fill="none" stroke={color} strokeWidth="2.2" strokeLinejoin="round" strokeLinecap="round" />

      {data.map((d, i) => (
        <g key={i}>
          <circle cx={x(i)} cy={y(d.value)} r="2.8" fill="#fff" stroke={color} strokeWidth="1.8" />
          <title>{`${d.label}：${d.value}${unit}`}</title>
        </g>
      ))}

      {data.map((d, i) =>
        i % Math.ceil(data.length / 7) === 0 || i === data.length - 1 ? (
          <text key={`l${i}`} x={x(i)} y={height - 6} textAnchor="middle" fontSize="11" fill="#9aa0aa">
            {d.label}
          </text>
        ) : null,
      )}
    </svg>
  );
};

/** 迷你趋势线（表格内联用） */
export const Sparkline: React.FC<{ data: number[]; width?: number; height?: number; color?: string }> = ({
  data,
  width = 96,
  height = 28,
  color = '#1677ff',
}) => {
  const max = Math.max(...data, 1);
  const min = Math.min(...data, 0);
  const span = max - min || 1;
  const x = (i: number) => (width * i) / Math.max(1, data.length - 1);
  const y = (v: number) => height - 3 - ((height - 6) * (v - min)) / span;
  const d = data.map((v, i) => `${i === 0 ? 'M' : 'L'}${x(i).toFixed(1)},${y(v).toFixed(1)}`).join(' ');
  return (
    <svg viewBox={`0 0 ${width} ${height}`} width={width} height={height}>
      <path d={`${d} L${width},${height} L0,${height} Z`} fill={color} fillOpacity="0.08" />
      <path d={d} fill="none" stroke={color} strokeWidth="1.6" />
    </svg>
  );
};

/** 环形占比图 */
export const DonutChart: React.FC<{
  data: { label: string; value: number; color: string }[];
  size?: number;
  centerLabel?: string;
  centerValue?: string;
}> = ({ data, size = 160, centerLabel, centerValue }) => {
  const total = data.reduce((n, d) => n + d.value, 0) || 1;
  const r = size / 2 - 12;
  const c = 2 * Math.PI * r;
  let offset = 0;

  return (
    <div className="yn-donut">
      <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`}>
        <g transform={`rotate(-90 ${size / 2} ${size / 2})`}>
          {data.map((d, i) => {
            const len = (d.value / total) * c;
            const el = (
              <circle
                key={i}
                cx={size / 2}
                cy={size / 2}
                r={r}
                fill="none"
                stroke={d.color}
                strokeWidth="14"
                strokeDasharray={`${len} ${c - len}`}
                strokeDashoffset={-offset}
              >
                <title>{`${d.label}：${d.value}（${((d.value / total) * 100).toFixed(1)}%）`}</title>
              </circle>
            );
            offset += len;
            return el;
          })}
        </g>
        <text x={size / 2} y={size / 2 - 2} textAnchor="middle" fontSize="22" fontWeight="600" fill="#2c313a">
          {centerValue}
        </text>
        <text x={size / 2} y={size / 2 + 16} textAnchor="middle" fontSize="12" fill="#9aa0aa">
          {centerLabel}
        </text>
      </svg>
      <ul className="yn-donut__legend">
        {data.map((d) => (
          <li key={d.label}>
            <span className="yn-dot" style={{ background: d.color }} />
            <span className="yn-donut__label">{d.label}</span>
            <span className="yn-donut__value">
              {d.value}
              <em>{((d.value / total) * 100).toFixed(0)}%</em>
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
};

/** 水平条形图（用于"构建耗时 TOP"这类排行） */
export const BarList: React.FC<{
  data: { label: string; value: number; suffix?: string; color?: string }[];
  max?: number;
  /** 全局单位后缀，可被 data[].suffix 覆盖 */
  suffix?: string;
}> = ({ data, max, suffix = '' }) => {
  const top = max ?? Math.max(...data.map((d) => d.value), 1);
  return (
    <div className="yn-barlist">
      {data.map((d, i) => (
        <div className="yn-barlist__row" key={i}>
          <span className="yn-barlist__label">{d.label}</span>
          <span className="yn-barlist__track">
            <span
              className="yn-barlist__fill"
              style={{ width: `${Math.max(3, (d.value / top) * 100)}%`, background: d.color ?? '#1677ff' }}
            />
          </span>
          <span className="yn-barlist__value">
            {d.value}
            {d.suffix ?? suffix}
          </span>
        </div>
      ))}
    </div>
  );
};

/** 资源用量进度条 */
export const UsageBar: React.FC<{ label: string; percent: number; detail?: string }> = ({
  label,
  percent,
  detail,
}) => {
  const color = percent >= 85 ? '#ff4d4f' : percent >= 65 ? '#faad14' : '#52c41a';
  return (
    <div className="yn-usage">
      <div className="yn-usage__head">
        <span>{label}</span>
        <span className="yn-usage__detail">{detail ?? `${percent}%`}</span>
      </div>
      <div className="yn-usage__track">
        <div className="yn-usage__fill" style={{ width: `${percent}%`, background: color }} />
      </div>
    </div>
  );
};
