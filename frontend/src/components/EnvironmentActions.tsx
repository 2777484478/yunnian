import React from 'react';
import { App as AntApp, Button, Space } from 'antd';
import { DeleteOutlined, EditOutlined } from '@ant-design/icons';
import { useNavigate } from 'react-router-dom';
import { DisabledTip } from '@/components/DisabledTip';
import { environmentApi } from '@/api';
import { hasPerm } from '@/auth';
import type { Environment } from '@/types';

export interface EnvironmentActionsProps {
  env: Environment;
  /**
   * 该环境下的服务实例数。
   *
   * **`null` 表示"没读到"**（列表还在加载 / 接口失败），此时**不预判**、退回确定性文案；
   * 也绝不能拿 `0` 充数 —— 那会把"能不能删"判反，用户点下去才吃一个 409。
   * 依据是后端 `DeleteEnvironment` 的硬约束：环境内仍有服务实例时直接 40901 拒绝。
   */
  svcCount: number | null;
  /** `false` 用于"整卡点击即进入"的卡片视图，避免同一个动作出现两个入口。 */
  withEnter?: boolean;
  onEdit?: (env: Environment) => void;
  /** 删除成功后回调：列表页刷新列表，详情页跳回列表。 */
  onRemoved?: () => void;
  /** 卡片脚部的紧凑样式（默认：列表操作列的常规 link 按钮）。 */
  compact?: boolean;
  /**
   * `link`（默认）= 列表操作列 / 卡片脚部的文字按钮；
   * `button` = 详情页 header 里的常规按钮（文案带"环境"二字，避免与旁边的
   * 「纳管项目 / 同步集群」混为一谈）。
   */
  variant?: 'link' | 'button';
}

/**
 * 环境级动作（进入 / 编辑 / 删除）的**唯一渲染入口**。
 *
 * 为什么要有这个组件：这些动作原先各自长在 `EnvironmentList` 的**表格视图**操作列里，
 * 而那一页默认落在**卡片视图**上 —— 于是「编辑」曾经在默认视图里够不着
 * （用户 2026-09-29 反馈"哪里有编辑？我没看到啊"），补完编辑又漏了删除，
 * 环境详情页更是两样都没有。根因不是漏写，是**动作定义在"视图"这一层，而视图有两个**：
 * 每加一个动作就得记得补两遍。
 *
 * 所以收口成一个组件：卡片、表格、详情页三处都渲染它，权限判断、实例数预检、
 * 二次确认、删除调用都只有一份。**加新动作时请改这里，不要再往各视图里抄按钮。**
 */
export const EnvironmentActions: React.FC<EnvironmentActionsProps> = ({
  env,
  svcCount,
  withEnter,
  onEdit,
  onRemoved,
  compact,
  variant = 'link',
}) => {
  const navigate = useNavigate();
  const { message, modal } = AntApp.useApp();
  const canWrite = hasPerm('env:write');

  const blocked = svcCount !== null && svcCount > 0;
  const linkStyle = compact
    ? { paddingInline: 4, fontSize: 12, height: 20 }
    : { paddingInline: 4 };

  const remove = () =>
    modal.confirm({
      title: `确认删除环境「${env.name}」？`,
      content: (
        <div style={{ fontSize: 13, lineHeight: 1.9 }}>
          删除后该环境下的部署与回滚记录会保留，但环境配置不可恢复。
          <div style={{ color: '#ff4d4f', marginTop: 6 }}>
            {blocked
              ? `该环境下还有 ${svcCount} 个服务实例，须先下线全部服务再删除环境。`
              : '该环境下若仍有服务实例将无法删除，须先下线全部服务。'}
          </div>
        </div>
      ),
      okText: '确认删除',
      okButtonProps: { danger: true, disabled: blocked },
      onOk: async () => {
        await environmentApi.remove(env.key);
        message.success(`环境「${env.name}」已删除`);
        onRemoved?.();
      },
    });

  const editTip = canWrite ? undefined : '当前账号没有 env:write 权限';

  if (variant === 'button') {
    return (
      <Space>
        <DisabledTip title={editTip}>
          <Button icon={<EditOutlined />} disabled={!canWrite} onClick={() => onEdit?.(env)}>
            编辑环境
          </Button>
        </DisabledTip>
        <DisabledTip title={editTip}>
          <Button danger icon={<DeleteOutlined />} disabled={!canWrite} onClick={remove}>
            删除环境
          </Button>
        </DisabledTip>
      </Space>
    );
  }

  // 整块动作区**吞掉自己的点击**：卡片整卡就是"进入"，
  // 而灰按钮、以及 `DisabledTip` 那层承接 hover 的 span 都会把点击冒泡上去，
  // 变成"点了个点不动的按钮，却跳进了详情页"。
  return (
    <Space size={2} onClick={(e) => e.stopPropagation()}>
      {withEnter ? (
        <Button
          type="link"
          size="small"
          style={linkStyle}
          onClick={() => navigate(`/cd/environments/${env.key}`)}
        >
          进入
        </Button>
      ) : null}
      <DisabledTip title={editTip}>
        <Button
          type="link"
          size="small"
          icon={<EditOutlined />}
          style={linkStyle}
          disabled={!canWrite}
          onClick={() => onEdit?.(env)}
        >
          编辑
        </Button>
      </DisabledTip>
      <DisabledTip title={editTip}>
        <Button
          type="link"
          size="small"
          danger
          style={linkStyle}
          disabled={!canWrite}
          onClick={remove}
        >
          删除
        </Button>
      </DisabledTip>
    </Space>
  );
};

export default EnvironmentActions;
