import React from 'react';
import { Popover } from 'antd';

// ---------------------------------------------------------------------------
// 发布拓扑 · 节点圆圈 + 链路连接段
// 圆圈承担三件事：①身份（集群/环境）②版本（版本号 + 构建号）③健康（进度弧）
// ---------------------------------------------------------------------------

export type NodeState = 'healthy' | 'warning' | 'error' | 'deploying' | 'undeployed' | 'unknown';

export interface StateStyle {
  from: string;
  to: string;
  solid: string;
  label: string;
}

export const STATE_STYLE: Record<NodeState, StateStyle> = {
  healthy: { from: '#5cdbd3', to: '#1890ff', solid: '#13c2c2', label: '运行正常' },
  warning: { from: '#ffd666', to: '#fa8c16', solid: '#faad14', label: '副本未就绪' },
  error: { from: '#ff9c9c', to: '#f5222d', solid: '#ff4d4f', label: '运行异常' },
  deploying: { from: '#91caff', to: '#1677ff', solid: '#1677ff', label: '部署中' },
  undeployed: { from: '#eceff3', to: '#d9dde3', solid: '#c2c7d0', label: '未部署' },
  // 「读不到集群」（未接入 / 凭据无权 / 网络超时）必须与「未部署」分开显示：
  // 前者是"平台不知道"，后者是"确认集群里没有"。都写成"未部署"，
  // 运维会以为自己还没发过，而实际是平台连不上集群——排查方向完全相反。
  unknown: { from: '#eceff3', to: '#d9dde3', solid: '#8c93a0', label: '状态未知' },
};

const SIZE = 176;
const R = 78;
const SW = 7;
const CIRC = 2 * Math.PI * R;

export interface TopoNodeProps {
  /** 集群标识，如 k8s-prod-01 */
  clusterLabel: string;
  /**
   * 集群角标 —— **不再是云厂商缩写**。
   *
   * 原先这里显示 ALI / HW（阿里云 ACK / 华为云 CCE），但那三个值是前端写死的：
   * kubeconfig 里只有 server / CA / context 名，**给不出云厂商与区域**。
   * 现在角标改说平台真正知道的事：已接入 / 异常 / 未探测 / 未接入（见 clusterBadgeOf）。
   * 尺寸与位置保持原样，未做任何版式调整。
   */
  clusterBadge: string;
  envName: string;
  stageLabel: string;
  version?: string;
  buildNo?: string;
  ready: number;
  /**
   * **实例总数**（集群 `status.replicas`，含正在崩溃、还没被回收的那些），
   * 不是期望副本数 —— 口径见 `utils/live.ts` 的 `liveReplicaTotal`。
   *
   * ⚠️ 别改回 `spec.replicas`：滚动卡住时集群里会多出一个反复崩溃的实例，
   * 只按期望显示就写成 `1/1`，把"有一个实例在崩"抹掉。
   */
  total: number;
  state: NodeState;
  /** deploying 时的进度 0-100 */
  progress?: number;
  index: number;
  /** 新增节点入场动画 */
  entering?: boolean;
  active: boolean;
  onActiveChange: (open: boolean) => void;
  menu: React.ReactNode;
}

export const TopoNode: React.FC<TopoNodeProps> = ({
  clusterLabel,
  clusterBadge,
  envName,
  stageLabel,
  version,
  buildNo,
  ready,
  total,
  state,
  progress = 0,
  index,
  entering,
  active,
  onActiveChange,
  menu,
}) => {
  const rawId = React.useId();
  const gid = `topo-${rawId.replace(/[^a-zA-Z0-9]/g, '')}`;
  const st = STATE_STYLE[state];

  const deployed = state !== 'undeployed' && state !== 'unknown';
  /** hollow 轨道（虚线空心）：未部署与状态未知都不该画实心进度弧 */
  const hollow = state === 'undeployed' || state === 'unknown';
  /** 进度弧比例：部署中看构建进度，其余看副本就绪度（分母是**实例总数**，见 prop total） */
  const ratio = !deployed ? 0 : state === 'deploying' ? progress / 100 : total ? ready / total : 0;
  const dash = CIRC * Math.min(1, Math.max(0, ratio));

  return (
    <Popover
      open={active}
      onOpenChange={onActiveChange}
      trigger="click"
      placement="right"
      arrow={false}
      rootClassName="yn-topo-pop"
      styles={{ body: { padding: 0, background: 'transparent', boxShadow: 'none' } }}
      content={menu}
    >
      <div
        className={[
          'yn-topo-node',
          `yn-topo-node--${state}`,
          active ? 'is-active' : '',
          entering ? 'yn-topo-node--enter' : '',
        ]
          .filter(Boolean)
          .join(' ')}
        style={{ animationDelay: `${Math.min(index, 8) * 80}ms` }}
      >
        <div className="yn-topo-node__disc">
          {/* 呼吸光环（仅健康/异常时可见，由 CSS 控制） */}
          <span className="yn-topo-node__halo" style={{ borderColor: st.solid }} />

          <svg width={SIZE} height={SIZE} viewBox={`0 0 ${SIZE} ${SIZE}`} className="yn-topo-node__svg">
            <defs>
              <linearGradient id={gid} x1="0%" y1="0%" x2="100%" y2="100%">
                <stop offset="0%" stopColor={st.from} />
                <stop offset="100%" stopColor={st.to} />
              </linearGradient>
              <radialGradient id={`${gid}-fill`} cx="50%" cy="42%" r="62%">
                <stop offset="0%" stopColor="#ffffff" />
                <stop offset="100%" stopColor={hollow ? '#fafbfc' : '#f7fafd'} />
              </radialGradient>
            </defs>

            {/* 内部底色 */}
            <circle cx={SIZE / 2} cy={SIZE / 2} r={R - SW / 2 - 2} fill={`url(#${gid}-fill)`} />

            {/* 轨道 */}
            <circle
              cx={SIZE / 2}
              cy={SIZE / 2}
              r={R}
              fill="none"
              stroke={hollow ? '#e8ebef' : '#edf1f6'}
              strokeWidth={SW}
              strokeDasharray={hollow ? '5 7' : undefined}
            />

            {/* 进度弧 */}
            {deployed ? (
              <circle
                cx={SIZE / 2}
                cy={SIZE / 2}
                r={R}
                fill="none"
                stroke={`url(#${gid})`}
                strokeWidth={SW}
                strokeLinecap="round"
                strokeDasharray={`${dash} ${CIRC - dash}`}
                transform={`rotate(-90 ${SIZE / 2} ${SIZE / 2})`}
                className="yn-topo-node__arc"
                style={{ filter: `drop-shadow(0 0 6px ${st.solid}55)` }}
              />
            ) : null}

            {/* 部署中：外层旋转虚线环 */}
            {state === 'deploying' ? (
              <circle
                cx={SIZE / 2}
                cy={SIZE / 2}
                r={R + 11}
                fill="none"
                stroke={st.solid}
                strokeWidth={2}
                strokeDasharray="8 10"
                opacity={0.85}
                className="yn-topo-node__spin"
              />
            ) : null}

            {/* 异常：外层告警环 */}
            {state === 'error' ? (
              <circle
                cx={SIZE / 2}
                cy={SIZE / 2}
                r={R + 11}
                fill="none"
                stroke={st.solid}
                strokeWidth={2}
                strokeDasharray="3 6"
                opacity={0.9}
                className="yn-topo-node__alarm"
              />
            ) : null}

            {/* 集群角标（原「厂商角标」）：文案与字号未变，只是内容从云厂商改成真实接入状态 */}
            <g transform={`translate(${SIZE / 2} 14)`}>
              <rect x={-22} y={-9} width={44} height={18} rx={9} fill={hollow ? '#f2f4f7' : '#ffffff'} stroke={st.solid} strokeWidth={1} />
              <text x={0} y={3.5} textAnchor="middle" fontSize={9.5} fontWeight={600} fill={st.solid} letterSpacing={0.5}>
                {clusterBadge}
              </text>
            </g>
          </svg>

          <div className="yn-topo-node__core">
            <span className="yn-topo-node__cluster" title={clusterLabel}>
              {clusterLabel}
            </span>
            <span className="yn-topo-node__env">{envName}</span>
            <span className="yn-topo-node__ver" style={{ color: deployed ? '#2c313a' : '#b5bac4' }}>
              {/* 未部署时不显示 "v—"：那一行只在有真实版本时才承载信息，
                  留个空壳字符反而像是"版本号取不到"。
                  状态未知时也不写"未部署"——那会把"读不到"说成"没有"。 */}
              {deployed ? version || '—' : state === 'undeployed' ? '未部署' : '—'}
            </span>
            {/* 构建号只在 tag 里真的能认出构建号时才渲染。
                原先无脑 `buildNo || '—'`：那个 '—' 的意思是"取不到构建号"，
                而现在的 tag（如 latest、2.1）本来就没有构建号这一段 ——
                画个 '—' 等于凭空声明"这里本该有东西却没取到"。 */}
            {deployed && buildNo ? <span className="yn-topo-node__build">{buildNo}</span> : null}
          </div>
        </div>

        <div className="yn-topo-node__foot">
          <span className="yn-topo-node__dot" style={{ background: st.solid }} />
          <span className="yn-topo-node__state">
            {st.label}
            {deployed && total ? ` ${ready}/${total}` : ''}
          </span>
          <span className="yn-topo-node__stage">{stageLabel}</span>
        </div>
      </div>
    </Popover>
  );
};

// --------------------------------------------------------------------------- 链路连接段

export interface TopoLinkProps {
  /** 流转门禁：自动晋级 / 需审批 … */
  gate: string;
  color: string;
  /** 目标环境是否已部署（未部署则链路虚化） */
  active: boolean;
  index: number;
}

export const TopoLink: React.FC<TopoLinkProps> = ({ gate, color, active, index }) => {
  const tone = active ? color : '#d9dde3';
  return (
    <div
      className={`yn-topo-link ${active ? 'is-active' : ''}`}
      style={{ animationDelay: `${Math.min(index, 8) * 80 + 40}ms` }}
    >
      <span
        className="yn-topo-link__gate"
        style={{ color: active ? color : '#a8adb6', borderColor: `${tone}55` }}
      >
        {gate}
      </span>
      <span
        className="yn-topo-link__track"
        style={{ color: tone, background: `linear-gradient(90deg, ${tone}22, ${tone})` }}
      >
        {active ? (
          <span
            className="yn-topo-link__flow"
            style={{ background: color, boxShadow: `0 0 8px ${color}` }}
          />
        ) : null}
      </span>
    </div>
  );
};

export default TopoNode;
