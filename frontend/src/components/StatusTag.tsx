import React from 'react';
import { Badge, Tag, Tooltip } from 'antd';
import {
  CheckCircleFilled,
  ClockCircleFilled,
  CloseCircleFilled,
  LoadingOutlined,
  MinusCircleFilled,
  PauseCircleFilled,
  QuestionCircleFilled,
  SyncOutlined,
  WarningFilled,
} from '@ant-design/icons';
import type {
  BuildStatus,
  DeployStatus,
  EnvStatus,
  ServiceStatus,
  UserRole,
  ProjectType,
  BuildMethod,
  DeployMethod,
} from '@/types';

// --------------------------------------------------------------------------- 构建状态
//
// ⚠️ 两处 map 都是 `Record<Status, ...>` 的**穷举**：往后端加一个状态词，
// 这里漏一个就会在 `MAP[status]` 取到 `undefined`，紧接着 `m.icon` 抛 TypeError
// 把整页表格打白屏。2026-10-09 新增 `interrupted` 时正是靠类型穷举发现的。
//
// `interrupted`（平台重启时中断）单列一档，刻意**不复用** failed / canceled：
//   - 复写 `failed` → 用户会去查"这次构建哪里写错了"，而实际原因是平台重启；
//   - 复写 `canceled` → 把"平台重启"谎报成"有人点了取消"，去找谁取消的必然扑空。
// 它既不是成功也不是错误，只是一件"没跑完、需要重来一次"的事 —— 用中性的橙色暂停态。

const BUILD_MAP: Record<BuildStatus, { text: string; color: string; icon: React.ReactNode }> = {
  queued: { text: '排队中', color: 'default', icon: <ClockCircleFilled style={{ color: '#8c8c8c' }} /> },
  running: { text: '构建中', color: 'processing', icon: <LoadingOutlined style={{ color: '#1677ff' }} /> },
  success: { text: '成功', color: 'success', icon: <CheckCircleFilled style={{ color: '#52c41a' }} /> },
  failed: { text: '失败', color: 'error', icon: <CloseCircleFilled style={{ color: '#ff4d4f' }} /> },
  canceled: { text: '已取消', color: 'default', icon: <MinusCircleFilled style={{ color: '#bfbfbf' }} /> },
  interrupted: { text: '已中断', color: 'orange', icon: <PauseCircleFilled style={{ color: '#fa8c16' }} /> },
};

export const BuildStatusTag: React.FC<{ status?: BuildStatus; showIcon?: boolean }> = ({
  status,
  showIcon = true,
}) => {
  if (!status) return <Tag style={{ marginInlineEnd: 0 }}>无记录</Tag>;
  const m = BUILD_MAP[status];
  return (
    <Tag color={m.color} style={{ marginInlineEnd: 0 }}>
      {showIcon && <span style={{ marginInlineEnd: 5 }}>{m.icon}</span>}
      {m.text}
    </Tag>
  );
};

// --------------------------------------------------------------------------- 部署状态

const DEPLOY_MAP: Record<DeployStatus, { text: string; color: string; icon: React.ReactNode }> = {
  pending_approval: { text: '待审批', color: 'gold', icon: <ClockCircleFilled style={{ color: '#faad14' }} /> },
  queued: { text: '排队中', color: 'default', icon: <ClockCircleFilled style={{ color: '#8c8c8c' }} /> },
  progressing: { text: '部署中', color: 'processing', icon: <LoadingOutlined style={{ color: '#1677ff' }} /> },
  success: { text: '成功', color: 'success', icon: <CheckCircleFilled style={{ color: '#52c41a' }} /> },
  failed: { text: '失败', color: 'error', icon: <CloseCircleFilled style={{ color: '#ff4d4f' }} /> },
  rolled_back: { text: '已回滚', color: 'purple', icon: <SyncOutlined style={{ color: '#722ed1' }} /> },
  canceled: { text: '已取消', color: 'default', icon: <MinusCircleFilled style={{ color: '#bfbfbf' }} /> },
  interrupted: { text: '已中断', color: 'orange', icon: <PauseCircleFilled style={{ color: '#fa8c16' }} /> },
};

export const DeployStatusTag: React.FC<{ status: DeployStatus; showIcon?: boolean }> = ({
  status,
  showIcon = true,
}) => {
  const m = DEPLOY_MAP[status];
  return (
    <Tag color={m.color} style={{ marginInlineEnd: 0 }}>
      {showIcon && <span style={{ marginInlineEnd: 5 }}>{m.icon}</span>}
      {m.text}
    </Tag>
  );
};

// --------------------------------------------------------------------------- 环境资源状态
//
// ⚠️ 这两个词量的是**集群资源水位**，不是服务健康 —— 措辞必须说资源。
//
// 判据只有一条：节点 CPU / 内存用量跨阈值（`cd.go:862-871`：>=90 → error、>=75 → warning、
// metrics 读不到 → unknown）。但 2026-10-09 之前这两档分别叫「存在告警」和「异常」，
// 用户看到一台内存 90% 的机器被打上红色「异常」时，理解只能是"这个环境出故障了" ——
// 而同一时刻集群里两个服务都是 1/1 Ready、kubelet 自报 MemoryPressure=False。
// 换句话说：数据是真的，词是过重的，而词的位置（紧挨环境名）让它更像故障。
// 改成「资源告警 / 资源紧张」后，"是什么在告警"不再需要看代码才能知道。
const ENV_MAP: Record<EnvStatus, { text: string; color: string }> = {
  healthy: { text: '运行正常', color: '#52c41a' },
  warning: { text: '资源告警', color: '#faad14' },
  error: { text: '资源紧张', color: '#ff4d4f' },
  unknown: { text: '未知', color: '#bfbfbf' },
};

export const EnvStatusBadge: React.FC<{ status: EnvStatus; text?: string }> = ({ status, text }) => (
  <Badge color={ENV_MAP[status].color} text={text ?? ENV_MAP[status].text} />
);

// --------------------------------------------------------------------------- 服务运行状态

const SVC_MAP: Record<ServiceStatus, { text: string; color: string; icon: React.ReactNode }> = {
  running: { text: '运行中', color: 'success', icon: <CheckCircleFilled style={{ color: '#52c41a' }} /> },
  deploying: { text: '部署中', color: 'processing', icon: <LoadingOutlined style={{ color: '#1677ff' }} /> },
  error: { text: '异常', color: 'error', icon: <WarningFilled style={{ color: '#ff4d4f' }} /> },
  stopped: { text: '已停止', color: 'default', icon: <MinusCircleFilled style={{ color: '#bfbfbf' }} /> },
  pending: { text: '待调度', color: 'gold', icon: <ClockCircleFilled style={{ color: '#faad14' }} /> },
};

export const ServiceStatusTag: React.FC<{ status: ServiceStatus | 'unknown' }> = ({ status }) => {
  // 状态词认不出时按"未知"渲染：集群返回了工作负载，但状态词不在已知枚举里
  // （后端将来新增状态、或状态为空串）。**不许猜成"运行中"** —— 猜错会让异常服务看起来正常。
  const m =
    status === 'unknown'
      ? { text: '状态未知', color: 'default', icon: <QuestionCircleFilled style={{ color: '#bfbfbf' }} /> }
      : SVC_MAP[status];
  return (
    <Tag color={m.color} style={{ marginInlineEnd: 0 }}>
      <span style={{ marginInlineEnd: 5 }}>{m.icon}</span>
      {m.text}
    </Tag>
  );
};

// --------------------------------------------------------------------------- 业务枚举标签

export const ProjectTypeTag: React.FC<{ type: ProjectType }> = ({ type }) =>
  type === 'frontend' ? (
    <Tag color="cyan" style={{ marginInlineEnd: 0 }}>前端</Tag>
  ) : (
    <Tag color="geekblue" style={{ marginInlineEnd: 0 }}>后端</Tag>
  );

const METHOD_TEXT: Record<BuildMethod | DeployMethod, string> = {
  dockerfile: 'Dockerfile',
  jenkins: 'Jenkins Job',
  script: '自定义脚本',
  k8s: 'K8s Deployment',
  helm: 'Helm',
  compose: 'Docker Compose',
};

export const MethodTag: React.FC<{ method: BuildMethod | DeployMethod }> = ({ method }) => (
  <Tag style={{ marginInlineEnd: 0 }}>{METHOD_TEXT[method] ?? method}</Tag>
);

const ROLE_META: Record<UserRole, { text: string; color: string }> = {
  admin: { text: '超级管理员', color: 'red' },
  ops: { text: '运维', color: 'blue' },
  dev: { text: '开发', color: 'geekblue' },
  qa: { text: '测试', color: 'cyan' },
  readonly: { text: '只读用户', color: 'default' },
};

/**
 * 角色标签。
 *
 * 必须容忍未知/空角色：审计日志里"匿名"操作（登录失败、未认证访问）
 * 后端给的是空字符串，早期直接写 `ROLE_META[role].color`，
 * 遇到空角色会抛 TypeError 把整张表渲染崩掉。
 *
 * 三种情况分开处理：
 *   - 空角色 → 不渲染（审计表里操作人列已经写了"匿名"，再挂一个匿名标签是重复）；
 *   - 已知角色 → 中文名 + 配色；
 *   - 未知角色（后端将来新增）→ 原样显示字符串，别静默吞掉。
 */
export const RoleTag: React.FC<{ role?: UserRole | string }> = ({ role }) => {
  if (!role) return null;
  const meta = ROLE_META[role as UserRole];
  if (!meta) {
    return <Tag style={{ marginInlineEnd: 0 }}>{String(role)}</Tag>;
  }
  return (
    <Tag color={meta.color} style={{ marginInlineEnd: 0 }}>
      {meta.text}
    </Tag>
  );
};

// --------------------------------------------------------------------------- 其他

export const DigestText: React.FC<{ value: string; width?: number }> = ({ value, width = 150 }) => (
  <Tooltip title={value}>
    <span className="yn-mono yn-ellipsis" style={{ maxWidth: width, display: 'inline-block', verticalAlign: 'bottom' }}>
      {value}
    </span>
  </Tooltip>
);

export const ImageText: React.FC<{ value: string; width?: number }> = ({ value, width = 320 }) => {
  const idx = value.lastIndexOf(':');
  const repo = idx > -1 ? value.slice(0, idx) : value;
  const tag = idx > -1 ? value.slice(idx + 1) : '';
  return (
    <Tooltip title={value}>
      <span style={{ display: 'flex', alignItems: 'baseline', maxWidth: width }}>
        <span
          className="yn-mono yn-ellipsis"
          style={{ flex: '0 1 auto', minWidth: 42, color: '#5c6470', fontSize: 12.5 }}
        >
          {repo}
        </span>
        <span className="yn-mono" style={{ flex: '0 0 auto', color: '#1677ff', fontWeight: 500, fontSize: 12.5 }}>
          :{tag}
        </span>
      </span>
    </Tooltip>
  );
};
