import React, { useMemo, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Button,
  Checkbox,
  Col,
  Drawer,
  Form,
  Input,
  Popconfirm,
  Row,
  Segmented,
  Select,
  Space,
  Switch,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  ApiOutlined,
  BellOutlined,
  CheckCircleOutlined,
  DingdingOutlined,
  MailOutlined,
  PlusOutlined,
  ReloadOutlined,
  WarningOutlined,
} from '@ant-design/icons';
import { PageHeader, SectionCard, StatCard } from '@/components/ui';
import { notifyApi } from '@/api';
import { useApi } from '@/hooks/useApi';
import { fmt } from '@/utils/format';
import type { NotifyChannel } from '@/types';

const { Text } = Typography;

// ---------------------------------------------------------------------------
// 通知渠道
//
// 这一页的每一处选项都必须与后端的真实能力对齐，因为用户默认「能配 == 能收到」：
//
//  · **渠道类型只有邮件与钉钉**。平台只实现了这两个发送器（service/notify.go），
//    后端入参枚举也收成了这两个。企业微信/飞书/自定义 Webhook 曾经能选，
//    但选了也永远不会发 —— 那种"安静的假承诺"没有任何地方会亮红灯。
//    库里可能还有历史记录，界面对认不出的类型原样显示类型名，不伪装。
//
//  · **事件只列后端真会触发的三个**。曾经这里有 8 个码（build_success、
//    rollback_executed、env_unhealthy…），其中 5 个没有任何代码会发出。
//    勾上它们只是往库里写一个字符串，用户却以为配好了。
//
//  · **测试按钮现在是真投递**。返回 ok=false 不再是"后端还没接入"，
//    而是"这次真的没发出去"——所以照实报错，并把对端的原话（签名不匹配、
//    关键词不在内容里、SMTP 认证失败…）原样展示。
//
//  · 表格里的「最近投递」是排查入口：配了渠道却没收到消息时，
//    这一列直接告诉你平台到底发没发成功、失败了几次、为什么失败。
// ---------------------------------------------------------------------------

const TYPE_META: Record<string, { label: string; icon: React.ReactNode; color: string }> = {
  email: { label: '邮件', icon: <MailOutlined />, color: 'blue' },
  dingtalk: { label: '钉钉', icon: <DingdingOutlined />, color: 'cyan' },
};

/**
 * 事件码 → 中文。**只收录平台真会发出的事件**（与后端 SupportedNotifyEvents 同步）。
 *
 * 库里可能还存着旧配置里的事件码；那些码会走下面的"原样透传 + 平台不推送"分支，
 * 而不是被静默丢掉 —— 用户需要看到自己配的东西现在处于什么状态。
 */
const EVENT_LABEL: Record<string, string> = {
  build_failed: '构建失败',
  build_interrupted: '构建被中断',
  deploy_failed: '部署失败',
  deploy_interrupted: '部署被中断',
  deploy_pending_approval: '部署待审批',
};

const EVENT_OPTIONS = Object.keys(EVENT_LABEL).map((code) => ({
  label: `${EVENT_LABEL[code]}（${code}）`,
  value: code,
}));

const eventLabel = (code: string) => EVENT_LABEL[code] ?? code;

/**
 * 关键事件：至少要有 1 个启用的渠道订阅，否则出事时无人收到。
 *
 * 「被中断」也在这里。它的成因不是构建/部署本身出错（所以和 `*_failed` 分开订阅），
 * 但它**不会自行恢复** —— 记录停在终态，唯一该做的事是"重新触发"。
 * 没有订阅就等于这件事只存在于列表页里，等谁碰巧翻到。
 */
const CRITICAL_EVENTS = ['build_failed', 'build_interrupted', 'deploy_failed', 'deploy_interrupted'];

const SystemNotify: React.FC = () => {
  const { message, notification } = AntApp.useApp();
  const [keyword, setKeyword] = useState('');
  const [typeFilter, setTypeFilter] = useState<string>('all');
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [editing, setEditing] = useState<NotifyChannel | null>(null);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState<string | null>(null);
  const [form] = Form.useForm();

  const channelsApi = useApi(() => notifyApi.list(), []);
  const channels = useMemo(() => channelsApi.data ?? [], [channelsApi.data]);

  const dataSource = channels.filter((c) => {
    const kw = keyword.trim().toLowerCase();
    const hitKw = !kw || c.name.toLowerCase().includes(kw) || c.target.toLowerCase().includes(kw);
    const hitType = typeFilter === 'all' || c.type === typeFilter;
    return hitKw && hitType;
  });

  // 订阅矩阵：行取**所有渠道事件的并集**（而不是写死几行），列的渠道按当前筛选走。
  // 并集里会混进历史配置留下的、平台已经不推送的事件码 —— 那些也要显示，
  // 并打上「平台不推送」：用户需要知道自己配的这一项现在是什么状态，
  // 而不是看着它在矩阵里凭空消失。
  const matrixRows = useMemo(() => {
    const codes = new Set<string>();
    channels.forEach((c) => c.events.forEach((e) => codes.add(e)));
    const known = Object.keys(EVENT_LABEL).filter((e) => codes.has(e));
    const unknown = [...codes].filter((e) => !(e in EVENT_LABEL)).sort();
    return [...known, ...unknown];
  }, [channels]);

  const stats = useMemo(() => {
    const enabled = channels.filter((c) => c.enabled);
    const covered = new Set(enabled.flatMap((c) => c.events));
    const uncovered = CRITICAL_EVENTS.filter((e) => !covered.has(e));
    return {
      total: channels.length,
      enabled: enabled.length,
      eventKinds: new Set(channels.flatMap((c) => c.events)).size,
      uncovered,
    };
  }, [channels]);

  const openCreate = () => {
    setEditing(null);
    form.resetFields();
    form.setFieldsValue({
      type: 'dingtalk',
      enabled: true,
      // 默认勾上**全部关键事件**（而不是写死两项）：新建时不勾，用户多半不会回头补，
      // 而这四项正是"出事了必须有人知道"的那一批。直接从 CRITICAL_EVENTS 取，两处不会漂移。
      events: [...CRITICAL_EVENTS],
    });
    setDrawerOpen(true);
  };

  const openEdit = (c: NotifyChannel) => {
    setEditing(c);
    form.setFieldsValue({ ...c, enabled: c.enabled });
    setDrawerOpen(true);
  };

  const submit = async () => {
    const v = await form.validateFields();
    setSaving(true);
    try {
      if (editing) {
        await notifyApi.update(editing.id, v);
        message.success(`渠道「${v.name}」已保存`);
      } else {
        await notifyApi.create(v);
        message.success(`渠道「${v.name}」已创建`);
      }
      setDrawerOpen(false);
      channelsApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message || '保存失败');
    } finally {
      setSaving(false);
    }
  };

  const toggleEnabled = async (c: NotifyChannel, next: boolean) => {
    try {
      await notifyApi.setEnabled(c.id, next);
      message.success(`已${next ? '启用' : '停用'}「${c.name}」`);
      channelsApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message || '操作失败');
    }
  };

  const remove = async (c: NotifyChannel) => {
    try {
      await notifyApi.remove(c.id);
      message.success(`已删除渠道「${c.name}」`);
      channelsApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message || '删除失败');
    }
  };

  const test = async (c: NotifyChannel) => {
    setTesting(c.id);
    try {
      const res = await notifyApi.test(c.id);
      // 这里是**真投递**的结果，不再是格式预检：ok=false 意味着这次真的没发出去。
      // 所以用 error 而不是 warning —— 黄色提示会被读成"有点小问题，不影响"，
      // 而对一个"以为配好了"的人来说，这是最需要立刻纠正的印象。
      // description 是对端的原话（签名不匹配 / 关键词不在内容里 / SMTP 认证失败…），原样展示。
      notification[res.ok ? 'success' : 'error']({
        message: res.ok ? `「${c.name}」测试消息已发出` : `「${c.name}」投递失败`,
        description: res.message,
        duration: res.ok ? 6 : 12,
      });
      // 投递成功/失败都会写回 lastSentAt / failCount / lastError，刷新让这一列立刻反映出来。
      channelsApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message || '测试失败');
    } finally {
      setTesting(null);
    }
  };

  const columns: ColumnsType<NotifyChannel> = [
    {
      title: '渠道名称',
      dataIndex: 'name',
      width: 240,
      render: (v: string, r) => {
        const meta = TYPE_META[r.type] ?? { label: r.type, icon: <ApiOutlined />, color: 'default' };
        return (
          <Space size={10}>
            <span style={{ fontSize: 16, color: '#1677ff' }}>{meta.icon}</span>
            <div>
              <div style={{ fontWeight: 500 }}>{v}</div>
              <Tag color={meta.color} style={{ marginTop: 3, marginInlineEnd: 0 }}>
                {meta.label}
              </Tag>
            </div>
          </Space>
        );
      },
    },
    {
      title: '推送目标',
      dataIndex: 'target',
      render: (v: string) => (
        <span className="yn-mono" style={{ fontSize: 12.5, color: '#5c6470', wordBreak: 'break-all' }}>
          {v}
        </span>
      ),
    },
    {
      title: '订阅事件',
      dataIndex: 'events',
      width: 330,
      render: (v: string[]) => (
        <Space size={4} wrap>
          {v.map((e) => (
            <Tooltip
              key={e}
              title={e in EVENT_LABEL ? e : '平台当前不推送这个事件：它不会产生任何通知，建议把它从订阅里去掉'}
            >
              <Tag
                color={e in EVENT_LABEL ? undefined : 'warning'}
                style={{ marginInlineEnd: 0, fontSize: 11.5 }}
              >
                {eventLabel(e)}
              </Tag>
            </Tooltip>
          ))}
        </Space>
      ),
    },
    {
      title: '状态',
      dataIndex: 'enabled',
      width: 110,
      render: (v: boolean, r) => (
        <Switch size="small" checked={v} onChange={(next) => toggleEnabled(r, next)} />
      ),
    },
    {
      /**
       * 最近投递 —— "我配了渠道，可消息没收到"唯一的自查入口。
       *
       * 只显示两件事：现在正常吗（失败了几次 / 上次成功是什么时候），
       * 不正常的话为什么（悬浮看原因）。特意不显示"累计成功 N 条"之类的统计：
       * 这一列要回答的是**当前状态**，不是历史战绩。
       */
      title: '最近投递',
      key: 'delivery',
      width: 210,
      render: (_, r) => {
        if (r.failCount > 0) {
          return (
            <Tooltip
              // 失败原因是对端的原话，钉钉的 errmsg 会带上一整段"解决方案+参考链接"，
              // 不限制宽度会撑出一屏长的浮层。
              overlayStyle={{ maxWidth: 460 }}
              title={r.lastError || '未记录失败原因'}
            >
              <Tag color="error" style={{ marginInlineEnd: 0, cursor: 'help' }}>
                连续失败 {r.failCount} 次
              </Tag>
            </Tooltip>
          );
        }
        if (r.lastSentAt) {
          return (
            <span className="yn-mono" style={{ fontSize: 12.5, color: '#5c6470' }}>
              {fmt(r.lastSentAt)}
            </span>
          );
        }
        // 注意措辞：lastSentAt 为空说的是"还没有一次成功投递"，
        // 而不是"平台没发过"—— 发过但全失败的情况由上面那个分支显示。
        return (
          <Text type="secondary" style={{ fontSize: 12 }}>
            尚无成功投递
          </Text>
        );
      },
    },
    {
      title: '更新时间',
      dataIndex: 'updatedAt',
      width: 168,
      render: (v: string) => (
        <span className="yn-mono" style={{ color: '#8c8c8c' }}>
          {v ? fmt(v) : '-'}
        </span>
      ),
    },
    {
      title: '操作',
      key: 'action',
      fixed: 'right',
      width: 176,
      render: (_, r) => (
        <Space size={2}>
          <Tooltip title="立即向该渠道发送一条测试消息（真实投递，失败会给出对端原因）">
            <Button
              type="link"
              size="small"
              style={{ paddingInline: 4 }}
              loading={testing === r.id}
              onClick={() => test(r)}
            >
              测试
            </Button>
          </Tooltip>
          <Button type="link" size="small" style={{ paddingInline: 4 }} onClick={() => openEdit(r)}>
            编辑
          </Button>
          <Popconfirm
            title={`删除渠道「${r.name}」？`}
            description="删除后该渠道不再接收任何事件推送。"
            okText="删除"
            cancelText="取消"
            okButtonProps={{ danger: true }}
            onConfirm={() => remove(r)}
          >
            <Button type="link" size="small" danger style={{ paddingInline: 4 }}>
              删除
            </Button>
          </Popconfirm>
        </Space>
      ),
    },
  ];

  const watchedType = Form.useWatch('type', form) as string | undefined;
  const isDingtalk = watchedType === 'dingtalk';

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: '系统管理' }, { title: '通知渠道' }]}
        title="通知渠道"
        // 这句原先写的是「构建成功/失败、部署成功/失败、待审批与回滚事件可推送至邮件、
        // 钉钉、企业微信、飞书或自定义 Webhook」—— **其中两个短语都是承诺不出来的东西**：
        // 企微/飞书/Webhook 没有发送器，构建成功/部署成功/回滚也没有任何代码会触发。
        // 页面描述是用户对这个功能的第一印象，写成"可选"就等于承诺。
        description="构建 / 部署失败与被中断、部署待审批，会推送到已启用且订阅了该事件的渠道"
        extra={
          <Space>
            <Button
              icon={<ReloadOutlined />}
              onClick={() => {
                channelsApi.refresh();
                message.success('已刷新');
              }}
            >
              刷新
            </Button>
            <Button type="primary" icon={<PlusOutlined />} onClick={openCreate}>
              新建渠道
            </Button>
          </Space>
        }
      />

      <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
        <Col xs={12} xl={6}>
          <StatCard title="通知渠道" value={stats.total} unit="个" icon={<BellOutlined />} />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="已启用"
            value={stats.enabled}
            unit="个"
            accent="#52c41a"
            footer={
              stats.total > stats.enabled ? (
                <Text type="secondary" style={{ fontSize: 12 }}>
                  停用 {stats.total - stats.enabled} 个
                </Text>
              ) : undefined
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="订阅事件类型"
            value={stats.eventKinds}
            unit="类"
            accent="#1677ff"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                全部渠道的去重并集
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="关键事件覆盖"
            value={stats.uncovered.length === 0 ? '已覆盖' : '有缺口'}
            accent={stats.uncovered.length === 0 ? '#52c41a' : '#ff4d4f'}
            icon={stats.uncovered.length === 0 ? <CheckCircleOutlined /> : <WarningOutlined />}
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                {/* 这里原本逐个列出事件名（"构建失败 / 部署失败均有渠道订阅"），
                    每加一个关键事件都要回来改一次，忘了就成假话。改成不点名的说法，
                    具体缺哪些由下面那句用 `eventLabel` 现算 —— 加事件码不用再动这里。 */}
                {stats.uncovered.length === 0
                  ? `全部 ${CRITICAL_EVENTS.length} 个关键事件均有渠道订阅`
                  : `以下事件无启用渠道订阅：${stats.uncovered.map(eventLabel).join('、')}`}
              </Text>
            }
          />
        </Col>
      </Row>

      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 16 }}
        message="构建 / 部署失败与被中断、部署待审批，会按订阅矩阵推送到已启用的渠道"
        description={
          <>
            目前接入两种通道：<b>邮件</b>（SMTP，发信服务器在平台配置 <span className="yn-mono">notify.email</span> 里维护）
            与 <b>钉钉</b>（自定义机器人）。企业微信、飞书、自定义 Webhook 尚未接入发送器，因此不再出现在可选类型里。
            渠道配好之后建议先用「测试」发一条 —— 它会<b>真的投递</b>，并把失败原因原样告诉你。
          </>
        }
      />

      <SectionCard title="事件订阅矩阵" bodyStyle={{ padding: '16px 20px' }} style={{ marginBottom: 16 }}>
        <div style={{ overflowX: 'auto' }}>
          <table className="yn-matrix">
            <thead>
              <tr>
                <th style={{ textAlign: 'left' }}>事件类型</th>
                {channels.map((c) => (
                  <th key={c.id}>
                    <div style={{ fontSize: 12 }}>{c.name}</div>
                    <div style={{ fontSize: 11, color: '#9aa0aa' }}>
                      {(TYPE_META[c.type] ?? { label: c.type }).label}
                      {c.enabled ? '' : ' · 已停用'}
                    </div>
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {matrixRows.map((ev) => (
                <tr key={ev}>
                  <td style={{ fontWeight: 500 }}>
                    {eventLabel(ev)}
                    {ev in EVENT_LABEL ? null : (
                      <Tag color="warning" style={{ marginLeft: 6, marginInlineEnd: 0, fontSize: 10.5 }}>
                        平台不推送
                      </Tag>
                    )}
                  </td>
                  {channels.map((c) => (
                    <td key={c.id} style={{ textAlign: 'center' }}>
                      <Checkbox
                        checked={c.events.includes(ev)}
                        disabled
                        style={{ pointerEvents: 'none', opacity: c.enabled ? 1 : 0.45 }}
                      />
                    </td>
                  ))}
                </tr>
              ))}
              {!matrixRows.length ? (
                <tr>
                  <td colSpan={channels.length + 1} style={{ color: '#9aa0aa', fontSize: 13 }}>
                    尚无渠道订阅任何事件
                  </td>
                </tr>
              ) : null}
            </tbody>
          </table>
        </div>
        <Text type="secondary" style={{ fontSize: 12 }}>
          矩阵按当前渠道列表实时生成；勾选状态由各渠道的配置决定，编辑渠道即可调整。
        </Text>
      </SectionCard>

      <SectionCard bodyStyle={{ padding: 0 }}>
        <div className="yn-filter-bar">
          <Space size={12} wrap>
            <Input.Search
              allowClear
              placeholder="搜索渠道名称 / 推送目标"
              style={{ width: 280 }}
              value={keyword}
              onChange={(e) => setKeyword(e.target.value)}
            />
            <Segmented
              value={typeFilter}
              onChange={(v) => setTypeFilter(v as string)}
              options={[
                { label: '全部', value: 'all' },
                ...Object.entries(TYPE_META).map(([k, v]) => ({ label: v.label, value: k })),
              ]}
            />
          </Space>
          <Text type="secondary" style={{ fontSize: 12 }}>
            共 {dataSource.length} 个渠道
          </Text>
        </div>

        {channelsApi.error ? (
          <div style={{ padding: 16 }}>
            <Alert
              type="error"
              showIcon
              message="通知渠道加载失败"
              description={channelsApi.error.message}
              action={
                <Button size="small" onClick={channelsApi.refresh}>
                  重试
                </Button>
              }
            />
          </div>
        ) : null}

        <Table<NotifyChannel>
          rowKey="id"
          size="middle"
          loading={channelsApi.loading}
          dataSource={dataSource}
          columns={columns}
          scroll={{ x: 1400 }}
          pagination={false}
          locale={{ emptyText: channelsApi.loading ? ' ' : '还没有配置任何通知渠道' }}
        />
      </SectionCard>

      <Drawer
        open={drawerOpen}
        width={560}
        title={editing ? `编辑渠道 · ${editing.name}` : '新建通知渠道'}
        onClose={() => setDrawerOpen(false)}
        extra={
          <Space>
            <Button onClick={() => setDrawerOpen(false)}>取消</Button>
            <Button type="primary" loading={saving} onClick={submit}>
              保存
            </Button>
          </Space>
        }
        destroyOnClose
      >
        <Form form={form} layout="vertical" requiredMark="optional">
          <Form.Item name="type" label="渠道类型" rules={[{ required: true }]}>
            <Select
              options={Object.entries(TYPE_META).map(([k, v]) => ({ label: v.label, value: k }))}
              onChange={(v) => {
                // 切到邮件时清掉 webhook 的占位地址，避免把 https:// 当成邮箱提交
                const cur = form.getFieldValue('target');
                if (v === 'email' && typeof cur === 'string' && cur.startsWith('http')) {
                  form.setFieldValue('target', '');
                }
              }}
            />
          </Form.Item>
          <Form.Item name="name" label="渠道名称" rules={[{ required: true, message: '请输入渠道名称' }]}>
            <Input placeholder="如 CI/CD 告警群（钉钉）" />
          </Form.Item>
          <Form.Item
            name="target"
            label="推送目标"
            rules={[
              { required: true, message: '请输入推送目标' },
              {
                // 与后端 validateNotifyTarget 同规则，提前拦掉而不是等一次 40000
                validator: (_, value: string) => {
                  const t = (value ?? '').trim();
                  if (!t) return Promise.resolve();
                  if (watchedType === 'email') {
                    return t.includes('@')
                      ? Promise.resolve()
                      : Promise.reject(new Error('邮件目标需为邮箱地址（多个用英文逗号分隔）'));
                  }
                  return t.startsWith('http://') || t.startsWith('https://')
                    ? Promise.resolve()
                    : Promise.reject(new Error('钉钉机器人的目标需以 http:// 或 https:// 开头'));
                },
              },
            ]}
            tooltip={
              isDingtalk
                ? '钉钉机器人的完整 Webhook 地址。若机器人的安全设置选了「加签」，光有 access_token 会被拒成 310000，需要把加签密钥以 &secret=SECxxx 的形式追在地址后面'
                : '邮箱地址，多个用英文逗号分隔（也认分号、空格、换行）'
            }
          >
            <Input.TextArea
              rows={2}
              className={isDingtalk ? 'yn-mono' : undefined}
              placeholder={
                isDingtalk
                  ? 'https://oapi.dingtalk.com/robot/send?access_token=***&secret=SEC***'
                  : 'ops-oncall@yunnian.cn'
              }
            />
          </Form.Item>
          <Form.Item
            name="events"
            label="订阅事件"
            rules={[{ required: true, message: '请至少选择一个事件' }]}
            extra="这五个是平台当前会实际推送的全部事件（含「被中断」——平台重启打断、不会自行恢复，需要重新触发）；构建成功、回滚执行等暂不推送，所以没有提供勾选。"
          >
            <Checkbox.Group options={EVENT_OPTIONS} />
          </Form.Item>
          <Form.Item name="enabled" label="启用" valuePropName="checked">
            <Switch checkedChildren="启用" unCheckedChildren="停用" />
          </Form.Item>
        </Form>
      </Drawer>
    </div>
  );
};

export default SystemNotify;
