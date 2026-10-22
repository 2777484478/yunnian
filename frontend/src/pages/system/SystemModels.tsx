import React, { useMemo, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Badge,
  Button,
  Card,
  Col,
  Divider,
  Form,
  Input,
  InputNumber,
  Modal,
  Popconfirm,
  Row,
  Segmented,
  Select,
  Slider,
  Space,
  Switch,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import {
  ApiOutlined,
  CheckCircleOutlined,
  CloudOutlined,
  DeleteOutlined,
  EditOutlined,
  ExperimentOutlined,
  FireOutlined,
  KeyOutlined,
  LinkOutlined,
  PlusOutlined,
  ReloadOutlined,
  RobotOutlined,
  SafetyCertificateOutlined,
  StarFilled,
  StarOutlined,
  SyncOutlined,
  ThunderboltOutlined,
  WarningOutlined,
} from '@ant-design/icons';
import { PageHeader, StatCard } from '@/components/ui';
import { modelApi } from '@/api';
import type { ModelInput } from '@/api';
import { hasPerm } from '@/auth';
import { useApi } from '@/hooks/useApi';
import { fmt } from '@/utils/format';
import type { ModelProvider, ModelVendor } from '@/types';

const { Text } = Typography;

// ---------------------------------------------------------------------------
// 模型设置
//
// 两个真实接口约束决定了这里的写法：
//
// 1. **PUT 是全量语义**。后端对 name/vendor/baseUrl 用了 required，
//    所以"只想改个开关"也必须把完整对象发回去（见 toModelBody）。
//    手写 `{enabled:false}` 会被 40000 拒掉，而且报错信息是"该字段必填"，
//    很容易被误判成前端表单没填完。
//
// 2. **密钥只写不读**。接口只回掩码，所以编辑时留空＝保留原值。
//    更要紧的是：库里可能**根本没有密文**（原型期种子只写了掩码字符串），
//    此时掩码是不可信的——后端现在会如实回 "未配置"。
//    页面据此把"连通正常"这类历史状态标为过期，避免看着绿灯去等一个必然 401 的通道。
// ---------------------------------------------------------------------------

const VENDOR_META: Record<ModelVendor, { label: string; color: string; icon: React.ReactNode }> = {
  dashscope: { label: '阿里云百炼', color: 'blue', icon: <CloudOutlined /> },
  qwen: { label: '通义千问', color: 'purple', icon: <ExperimentOutlined /> },
  deepseek: { label: 'DeepSeek', color: 'geekblue', icon: <ThunderboltOutlined /> },
  openai: { label: 'OpenAI 兼容', color: 'green', icon: <ApiOutlined /> },
  custom: { label: '自定义端点', color: 'default', icon: <ApiOutlined /> },
};

/** 新增通道时的快速预设：默认支持百炼 / 千问 / DeepSeek */
const PRESETS: Array<Pick<ModelProvider, 'vendor' | 'name' | 'baseUrl' | 'models' | 'defaultModel' | 'desc'>> = [
  {
    vendor: 'dashscope',
    name: '阿里云百炼',
    baseUrl: 'https://dashscope.aliyuncs.com/compatible-mode/v1',
    models: ['qwen-max', 'qwen-plus', 'qwen-turbo', 'qwen-long'],
    defaultModel: 'qwen-plus',
    desc: 'OpenAI 兼容模式接入，通义千问全系模型。',
  },
  {
    vendor: 'qwen',
    name: '通义千问',
    baseUrl: 'https://dashscope.aliyuncs.com/api/v1',
    models: ['qwen-max', 'qwen-vl-max', 'qwen-long'],
    defaultModel: 'qwen-max',
    desc: 'DashScope 原生接口通道，支持视觉理解与超长文本。',
  },
  {
    vendor: 'deepseek',
    name: 'DeepSeek',
    baseUrl: 'https://api.deepseek.com/v1',
    models: ['deepseek-chat', 'deepseek-reasoner'],
    defaultModel: 'deepseek-chat',
    desc: '第三方通道，deepseek-reasoner 适合复杂故障链路推理。',
  },
  {
    vendor: 'custom',
    name: '',
    baseUrl: 'http://',
    models: [],
    defaultModel: '',
    desc: '任意 OpenAI 兼容端点，如内网 vLLM / Ollama / One-API 网关。',
  },
];

const STATUS_META: Record<ModelProvider['status'], { text: string; badge: 'success' | 'error' | 'default' | 'processing' }> = {
  connected: { text: '连通正常', badge: 'success' },
  failed: { text: '连接失败', badge: 'error' },
  unchecked: { text: '未检测', badge: 'default' },
  testing: { text: '检测中…', badge: 'processing' },
};

/** 明文密钥 → 掩码（仅用于新增/改密钥后的本地提示，真实掩码由后端返回） */
const maskApiKey = (raw: string): string =>
  `sk-****************${raw.length >= 4 ? raw.slice(-4) : ''.padEnd(raw.length, '*')}`;

/**
 * 构造全量写入 body。
 *
 * 不包含 `apiKey`：留空即"保留原密钥"。只在用户显式输入新密钥时才补上。
 */
const toModelBody = (p: ModelProvider, overrides: Partial<ModelInput> = {}): ModelInput => ({
  name: p.name,
  vendor: p.vendor,
  baseUrl: p.baseUrl,
  models: p.models,
  defaultModel: p.defaultModel,
  enabled: p.enabled,
  timeout: p.timeout,
  maxConcurrency: p.maxConcurrency,
  temperature: p.temperature,
  maxTokens: p.maxTokens,
  desc: p.desc,
  ...overrides,
});

interface FormValues {
  name: string;
  vendor: ModelVendor;
  baseUrl: string;
  apiKey?: string;
  models: string[];
  defaultModel: string;
  timeout: number;
  maxConcurrency: number;
  temperature: number;
  maxTokens: number;
  desc?: string;
}

const SystemModels: React.FC = () => {
  const { message, modal, notification } = AntApp.useApp();
  const canWrite = hasPerm('user:manage');

  const [filter, setFilter] = useState<'all' | 'enabled' | 'disabled'>('all');
  const [open, setOpen] = useState(false);
  const [editingId, setEditingId] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState<string | null>(null);
  const [form] = Form.useForm<FormValues>();
  const watchedModels = Form.useWatch<string[]>('models', form) ?? [];

  const listApi = useApi(() => modelApi.list(), []);
  const list = useMemo(() => listApi.data ?? [], [listApi.data]);

  // 列表很小（个位数通道），筛选与统计都在前端做：
  // 分段控件要显示"全部/已启用/已停用"的条数，走服务端筛选反而要发三次请求。
  const shown = useMemo(
    () => list.filter((m) => (filter === 'all' ? true : filter === 'enabled' ? m.enabled : !m.enabled)),
    [list, filter],
  );

  const stats = useMemo(() => {
    const enabled = list.filter((m) => m.enabled);
    const fallback = list.find((m) => m.isDefault);
    return {
      total: list.length,
      enabled: enabled.length,
      failed: list.filter((m) => m.status === 'failed').length,
      noKey: list.filter((m) => m.apiKeyMasked === '未配置').length,
      fallback: fallback?.name ?? '未设置',
      calls: list.reduce((s, m) => s + m.callCount, 0),
    };
  }, [list]);

  /** 密钥是否真的配置了。掩码为"未配置"时，历史连通状态不可信 */
  const hasKey = (p: ModelProvider) => p.apiKeyMasked !== '未配置';

  // ------------------------------------------------------------------ 写操作
  const toggleEnabled = async (p: ModelProvider, next: boolean) => {
    if (!next && p.isDefault) {
      modal.confirm({
        title: '该通道是平台默认通道',
        content:
          '停用后 AI 助手与失败归因将没有可用通道，相关能力会直接不可用。确定要停用吗？',
        okText: '仍要停用',
        okButtonProps: { danger: true },
        cancelText: '取消',
        onOk: () => doToggle(p, next),
      });
      return;
    }
    await doToggle(p, next);
  };

  const doToggle = async (p: ModelProvider, next: boolean) => {
    try {
      await modelApi.update(p.id, toModelBody(p, { enabled: next }));
      message.success(`已${next ? '启用' : '停用'}「${p.name}」`);
      listApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message || '操作失败');
    }
  };

  const setDefault = async (p: ModelProvider) => {
    try {
      await modelApi.setDefault(p.id);
      notification.success({
        message: `已将「${p.name}」设为平台默认通道`,
        description: hasKey(p)
          ? 'AI 助手未指定模型时会回落到该通道。'
          : '注意：该通道尚未配置 API Key，设为默认后 AI 能力仍会因鉴权失败而不可用。',
      });
      listApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message || '设置失败');
    }
  };

  const remove = async (p: ModelProvider) => {
    try {
      await modelApi.remove(p.id);
      message.success(`已移除通道「${p.name}」`);
      listApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message || '移除失败');
    }
  };

  const testConn = async (p: ModelProvider) => {
    setTesting(p.id);
    try {
      const res = await modelApi.test(p.id);
      notification[res.ok ? 'success' : 'error']({
        message: res.ok ? `「${res.name}」连通正常` : `「${res.name}」连接失败`,
        description: res.ok ? `${res.message}（往返 ${res.latency}ms）` : res.message,
        duration: res.ok ? 4.5 : 10,
      });
      // 测试结果（status/latency/checkedAt）由后端落库，刷新拿权威值
      listApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message || '测试失败');
    } finally {
      setTesting(null);
    }
  };

  const testAll = () => {
    // 串行跑：连通性测试会真的发一次外部请求，并发打过去容易触发供应商限流
    void (async () => {
      for (const p of shown) {
        await testConn(p);
      }
    })();
  };

  // ------------------------------------------------------------------- 表单
  const openCreate = (preset?: (typeof PRESETS)[number]) => {
    setEditingId(null);
    form.setFieldsValue({
      name: preset?.name ?? '',
      vendor: preset?.vendor ?? 'dashscope',
      baseUrl: preset?.baseUrl ?? 'https://',
      models: preset?.models ?? [],
      defaultModel: preset?.defaultModel ?? '',
      timeout: 60,
      maxConcurrency: 3,
      temperature: 0.2,
      maxTokens: 2048,
      desc: preset?.desc ?? '',
      apiKey: '',
    });
    setOpen(true);
  };

  const openEdit = (p: ModelProvider) => {
    setEditingId(p.id);
    form.setFieldsValue({
      name: p.name,
      vendor: p.vendor,
      baseUrl: p.baseUrl,
      models: p.models,
      defaultModel: p.defaultModel,
      timeout: p.timeout,
      maxConcurrency: p.maxConcurrency,
      temperature: p.temperature,
      maxTokens: p.maxTokens,
      desc: p.desc,
      apiKey: '',
    });
    setOpen(true);
  };

  const submit = async () => {
    const v = await form.validateFields();
    setSaving(true);
    try {
      const body: ModelInput = {
        name: v.name,
        vendor: v.vendor,
        baseUrl: v.baseUrl,
        models: v.models,
        defaultModel: v.defaultModel,
        timeout: v.timeout,
        maxConcurrency: v.maxConcurrency,
        temperature: v.temperature,
        maxTokens: v.maxTokens,
        desc: v.desc ?? '',
        // 留空＝保留原密钥（后端语义），所以空串直接不发这个字段
        ...(v.apiKey ? { apiKey: v.apiKey } : {}),
      };

      if (editingId) {
        await modelApi.update(editingId, body);
        notification.success({
          message: '通道配置已更新',
          description: v.apiKey
            ? `密钥已更新为 ${maskApiKey(v.apiKey)}，状态已重置为「未检测」，请重新测试连通性。`
            : '密钥未改动，原密钥继续沿用。',
        });
      } else {
        await modelApi.create({ ...body, enabled: true });
        notification.success({
          message: `已新增通道「${v.name}」`,
          description: v.apiKey
            ? '建议立即测试连通性以确认密钥可用。'
            : '尚未配置 API Key，该通道暂时无法真正调用——请补充密钥后再测试。',
        });
      }
      setOpen(false);
      listApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message || '保存失败');
    } finally {
      setSaving(false);
    }
  };

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: '系统管理' }, { title: '模型设置' }]}
        title="模型设置"
        description="配置 AI 运维助手使用的大模型通道。默认内置阿里云百炼、通义千问、DeepSeek 三个渠道，也可接入任意 OpenAI 兼容端点"
        extra={
          <Space>
            <Button
              icon={<ReloadOutlined />}
              onClick={() => {
                listApi.refresh();
                message.success('已刷新');
              }}
            >
              刷新
            </Button>
            <Button icon={<SyncOutlined />} onClick={testAll} loading={!!testing} disabled={!shown.length}>
              批量检测
            </Button>
            <Tooltip title={canWrite ? '' : '需要 user:manage 权限'}>
              <Button type="primary" icon={<PlusOutlined />} disabled={!canWrite} onClick={() => openCreate()}>
                新增通道
              </Button>
            </Tooltip>
          </Space>
        }
      />

      <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
        <Col xs={12} xl={6}>
          <StatCard
            title="接入通道"
            value={stats.total}
            unit="个"
            icon={<ApiOutlined />}
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                已启用 {stats.enabled} 个
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="平台默认通道"
            value={<span style={{ fontSize: 19 }}>{stats.fallback}</span>}
            icon={<StarFilled />}
            accent="#ff6a00"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                AI 助手未指定模型时回落到此通道
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="累计调用"
            value={stats.calls.toLocaleString()}
            unit="次"
            icon={<FireOutlined />}
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                含构建归因 / 发布风险 / 对话问答
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="待补密钥"
            value={stats.noKey}
            unit="个"
            accent={stats.noKey ? '#ff4d4f' : '#52c41a'}
            icon={stats.noKey ? <WarningOutlined /> : <CheckCircleOutlined />}
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                {stats.noKey ? '未配置密钥的通道无法真正调用' : '全部通道均已配置密钥'}
              </Text>
            }
          />
        </Col>
      </Row>

      {stats.noKey > 0 ? (
        <Alert
          type="error"
          showIcon
          icon={<WarningOutlined />}
          style={{ marginBottom: 16 }}
          message={`有 ${stats.noKey} 个通道尚未配置 API Key`}
          description="这些通道保留了原型期的展示用掩码，但数据库里没有密钥密文，任何调用都会因鉴权失败而报错（实测：模型通道返回 HTTP 401 You didn't provide an API key）。请点开对应通道补填密钥并重新测试——在补填之前，卡片上的「连通正常」是历史状态，不代表当前可用。"
        />
      ) : null}

      <Alert
        type="info"
        showIcon
        icon={<SafetyCertificateOutlined />}
        style={{ marginBottom: 16 }}
        message="密钥只写不读"
        description="API Key 提交后由后端加密存储，接口只回显后 4 位掩码，任何情况下不向前端下发明文；编辑时留空表示保留原密钥不变。"
      />

      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 12 }}>
        <Segmented
          value={filter}
          onChange={(v) => setFilter(v as typeof filter)}
          options={[
            { label: `全部 ${list.length}`, value: 'all' },
            { label: `已启用 ${list.filter((m) => m.enabled).length}`, value: 'enabled' },
            { label: `已停用 ${list.filter((m) => !m.enabled).length}`, value: 'disabled' },
          ]}
        />
        <Text type="secondary" style={{ fontSize: 12 }}>
          baseUrl 统一填写到 <span className="yn-mono">/v1</span> 一级，后端负责拼接具体接口路径
        </Text>
      </div>

      {listApi.error ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message="模型通道加载失败"
          description={listApi.error.message}
          action={
            <Button size="small" onClick={listApi.refresh}>
              重试
            </Button>
          }
        />
      ) : null}

      <Row gutter={[16, 16]}>
        {shown.map((p) => {
          const meta = VENDOR_META[p.vendor] ?? { label: p.vendor, color: 'default', icon: <ApiOutlined /> };
          const keyReady = hasKey(p);
          // 没有密钥时，历史连通状态不可信：统一改显示"未配置密钥"
          const statusView = !keyReady
            ? { text: '未配置密钥', badge: 'error' as const }
            : STATUS_META[p.status] ?? { text: p.status, badge: 'default' as const };
          return (
            <Col key={p.id} xs={24} xl={12}>
              <Card
                className="yn-section-card yn-model-card"
                variant="borderless"
                style={{ height: '100%' }}
                loading={listApi.loading}
                title={
                  <Space size={8}>
                    <span style={{ fontSize: 16, color: '#1677ff' }}>{meta.icon}</span>
                    <span>{p.name}</span>
                    {p.name !== meta.label ? (
                      <Tag color={meta.color} style={{ marginInlineEnd: 0, fontSize: 11, lineHeight: '17px' }}>
                        {meta.label}
                      </Tag>
                    ) : null}
                    {p.isDefault ? (
                      <Tag color="orange" style={{ marginInlineEnd: 0, fontSize: 11, lineHeight: '17px' }}>
                        平台默认
                      </Tag>
                    ) : null}
                    {!p.enabled ? (
                      <Tag style={{ marginInlineEnd: 0, fontSize: 11, lineHeight: '17px' }}>已停用</Tag>
                    ) : null}
                  </Space>
                }
                extra={
                  <Tooltip title={keyReady ? '' : '数据库中没有该通道的密钥密文，调用必然鉴权失败'}>
                    <Badge status={statusView.badge} text={statusView.text} />
                  </Tooltip>
                }
              >
                <div className="yn-model-card__grid">
                  <div className="yn-model-card__cell">
                    <span className="yn-model-card__label">
                      <LinkOutlined /> Base URL
                    </span>
                    <span className="yn-mono yn-model-card__break">{p.baseUrl}</span>
                  </div>
                  <div className="yn-model-card__cell">
                    <span className="yn-model-card__label">
                      <KeyOutlined /> API Key
                    </span>
                    <span className={keyReady ? 'yn-mono' : ''}>
                      {keyReady ? (
                        p.apiKeyMasked
                      ) : (
                        <Text type="danger" style={{ fontSize: 12 }}>
                          未配置
                        </Text>
                      )}
                    </span>
                  </div>
                  <div className="yn-model-card__cell">
                    <span className="yn-model-card__label">
                      <RobotOutlined /> 当前模型
                    </span>
                    <span>
                      <Tooltip title={canWrite ? '切换后立即生效' : '需要 user:manage 权限'}>
                        <Select
                          size="small"
                          value={p.defaultModel || undefined}
                          style={{ minWidth: 168 }}
                          disabled={!canWrite}
                          onChange={async (v) => {
                            try {
                              await modelApi.update(p.id, toModelBody(p, { defaultModel: v }));
                              message.success(`「${p.name}」已切换为 ${v}`);
                              listApi.refresh();
                            } catch (e) {
                              message.error((e as Error)?.message || '切换失败');
                            }
                          }}
                          options={p.models.map((m) => ({ label: m, value: m }))}
                        />
                      </Tooltip>
                    </span>
                  </div>
                  <div className="yn-model-card__cell">
                    <span className="yn-model-card__label">
                      <ThunderboltOutlined /> 参数
                    </span>
                    <span className="yn-model-card__params">
                      <em>超时 {p.timeout}s</em>
                      <em>并发 {p.maxConcurrency}</em>
                      <em>温度 {p.temperature}</em>
                      <em>{p.maxTokens} tokens</em>
                    </span>
                  </div>
                </div>

                <div className="yn-model-card__models">
                  <Text type="secondary" style={{ fontSize: 12, marginRight: 6 }}>
                    可选模型
                  </Text>
                  {p.models.length ? (
                    p.models.map((m) => (
                      <Tag key={m} className="yn-mono" style={{ fontSize: 11.5, marginInlineEnd: 6 }}>
                        {m}
                      </Tag>
                    ))
                  ) : (
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      未配置
                    </Text>
                  )}
                </div>

                {p.desc ? (
                  <div style={{ fontSize: 12.5, color: '#70757f', marginTop: 10, lineHeight: 1.7 }}>{p.desc}</div>
                ) : null}

                <Divider style={{ margin: '14px 0 12px' }} />

                <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', gap: 12, flexWrap: 'wrap' }}>
                  <Space size={14} wrap>
                    <Tooltip title={canWrite ? (p.enabled ? '点击停用该通道' : '点击启用该通道') : '需要 user:manage 权限'}>
                      <Space size={6}>
                        <Switch
                          size="small"
                          checked={p.enabled}
                          disabled={!canWrite}
                          onChange={(v) => toggleEnabled(p, v)}
                        />
                        <Text type="secondary" style={{ fontSize: 12 }}>
                          {p.enabled ? '已启用' : '已停用'}
                        </Text>
                      </Space>
                    </Tooltip>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      累计调用 <b style={{ color: '#2c313a' }}>{p.callCount}</b> 次
                      {p.failCount ? <span style={{ color: '#ff4d4f' }}> · 失败 {p.failCount}</span> : null}
                    </Text>
                    {keyReady && p.latency ? (
                      <Text type="secondary" style={{ fontSize: 12 }}>
                        往返 <b style={{ color: '#52c41a' }}>{p.latency}ms</b>
                      </Text>
                    ) : null}
                    {keyReady && p.checkedAt ? (
                      <Text type="secondary" style={{ fontSize: 12 }} className="yn-mono">
                        {fmt(p.checkedAt)}
                      </Text>
                    ) : null}
                  </Space>

                  <Space size={8}>
                    <Tooltip title={keyReady ? '真的发一次最小请求' : '未配置密钥，测试必然失败'}>
                      <Button
                        size="small"
                        icon={<SyncOutlined />}
                        loading={testing === p.id}
                        disabled={!canWrite}
                        onClick={() => testConn(p)}
                      >
                        测试
                      </Button>
                    </Tooltip>
                    <Tooltip title={p.isDefault ? '已是平台默认通道' : '设为平台默认通道'}>
                      <Button
                        size="small"
                        /**
                         * ⚠️ 星标要显式给颜色。默认它继承按钮文字色（#00000073），
                         * 而"已是默认"时按钮又是 disabled 状态（更淡到 0.25 透明度），
                         * 结果就是「平台默认」这个最重要的状态反而最看不见。
                         * 内联 style 的优先级高于 disabled 态色，所以颜色能保住。
                         */
                        icon={
                          p.isDefault ? (
                            <StarFilled style={{ color: '#1677ff' }} />
                          ) : (
                            <StarOutlined />
                          )
                        }
                        disabled={!canWrite || p.isDefault}
                        onClick={() => setDefault(p)}
                      />
                    </Tooltip>
                    <Tooltip title="编辑">
                      <Button size="small" icon={<EditOutlined />} disabled={!canWrite} onClick={() => openEdit(p)} />
                    </Tooltip>
                    <Popconfirm
                      title={`移除通道「${p.name}」？`}
                      description={
                        p.isDefault
                          ? '该通道是平台默认通道，移除后 AI 助手需重新指定模型。'
                          : '移除后 AI 助手将不再使用该通道。'
                      }
                      okText="移除"
                      cancelText="取消"
                      okButtonProps={{ danger: true }}
                      onConfirm={() => remove(p)}
                      disabled={!canWrite}
                    >
                      <Button size="small" danger icon={<DeleteOutlined />} disabled={!canWrite} />
                    </Popconfirm>
                  </Space>
                </div>
              </Card>
            </Col>
          );
        })}
      </Row>

      {!listApi.loading && !shown.length ? (
        <Card className="yn-section-card" variant="borderless" style={{ textAlign: 'center', padding: '40px 0' }}>
          <ApiOutlined style={{ fontSize: 34, color: '#d9dfe8' }} />
          <div style={{ marginTop: 12, color: '#9aa0aa', fontSize: 13 }}>
            {list.length ? '当前筛选下没有通道' : '还没有配置任何模型通道，点「新增通道」选择预设模板开始'}
          </div>
        </Card>
      ) : null}

      <Card className="yn-section-card" variant="borderless" style={{ marginTop: 16 }} title="接入说明">
        <div style={{ fontSize: 13, color: '#5c6470', lineHeight: 2 }}>
          <div>
            <b>渠道与模型的区别</b>：渠道（通道）= 一个可调用的 API 端点 + 密钥；模型 = 该端点下提供的具体模型名，
            同一个百炼通道可以随时在 <span className="yn-mono">qwen-max / qwen-plus / qwen-turbo</span> 之间切换。
          </div>
          <div>
            <b>平台默认通道</b>：只能有一个。AI 助手的构建失败归因、发布风险分析等自动化任务都走它，
            默认通道停用或未配置密钥，会导致这些能力不可用。
          </div>
          <div>
            <b>保存即全量覆盖</b>：编辑弹窗提交的是整份配置（后端 PUT 为全量语义），
            所以表单里每一项都会写回；只有 API Key 留空时表示"不改动"。
          </div>
          <div>
            <b>落库位置</b>：对应后端 <span className="yn-mono">ai_model_config</span> 表，
            密钥密文 + IV + AuthTag 三列加密存储，运行时解密注入；连通性测试结果（状态 / 耗时 / 检测时间）同样落库。
          </div>
        </div>
      </Card>

      <Modal
        open={open}
        title={editingId ? '编辑模型通道' : '新增模型通道'}
        width={640}
        okText={editingId ? '保存' : '创建'}
        cancelText="取消"
        confirmLoading={saving}
        onOk={submit}
        onCancel={() => setOpen(false)}
        destroyOnClose
        className="yn-batch-modal"
      >
        {!editingId ? (
          <div style={{ marginBottom: 16 }}>
            <Text type="secondary" style={{ fontSize: 12, display: 'block', marginBottom: 8 }}>
              快速预设（点击填充下方表单）
            </Text>
            <Space size={8} wrap>
              {PRESETS.map((p) => (
                <Button key={p.vendor} size="small" onClick={() => openCreate(p)}>
                  {VENDOR_META[p.vendor].icon}
                  <span style={{ marginLeft: 6 }}>{p.name || '自定义端点'}</span>
                </Button>
              ))}
            </Space>
          </div>
        ) : null}

        <Form form={form} layout="vertical" requiredMark="optional">
          <Row gutter={12}>
            <Col xs={24} sm={12}>
              <Form.Item name="name" label="通道名称" rules={[{ required: true, message: '请输入通道名称' }]}>
                <Input placeholder="如：阿里云百炼" />
              </Form.Item>
            </Col>
            <Col xs={24} sm={12}>
              <Form.Item name="vendor" label="渠道类型" rules={[{ required: true }]}>
                <Select
                  options={(Object.keys(VENDOR_META) as ModelVendor[]).map((k) => ({
                    label: VENDOR_META[k].label,
                    value: k,
                  }))}
                />
              </Form.Item>
            </Col>
          </Row>

          <Form.Item
            name="baseUrl"
            label="API Base URL"
            rules={[
              { required: true, message: '请输入 API 地址' },
              { pattern: /^https?:\/\/.+/, message: '需以 http:// 或 https:// 开头' },
              {
                // 与后端 validateModelBaseURL 同规则：带了 /chat/completions 后端会拒
                validator: (_, v: string) =>
                  (v ?? '').endsWith('/chat/completions')
                    ? Promise.reject(new Error('填到 /v1 一级即可，后端会自动补 /chat/completions'))
                    : Promise.resolve(),
              },
            ]}
            extra="填写到 /v1 一级，不要带 /chat/completions；后端会自动拼接"
          >
            <Input className="yn-mono" placeholder="https://dashscope.aliyuncs.com/compatible-mode/v1" />
          </Form.Item>

          <Form.Item
            name="apiKey"
            label="API Key"
            extra={editingId ? '留空表示保留原密钥；密钥只写不读' : '至少填入一个可用密钥，否则该通道无法真正调用'}
          >
            <Input.Password className="yn-mono" placeholder="sk-..." autoComplete="new-password" />
          </Form.Item>

          <Form.Item
            name="models"
            label="可选模型"
            rules={[{ required: true, message: '至少添加一个模型名' }]}
          >
            <Select mode="tags" placeholder="输入模型名后回车，如 qwen-plus" tokenSeparators={[',', ' ']} />
          </Form.Item>

          <Row gutter={12}>
            <Col xs={24} sm={12}>
              <Form.Item
                name="defaultModel"
                label="默认模型"
                rules={[
                  { required: true, message: '请选择默认模型' },
                  {
                    // 后端不校验 defaultModel 是否在 models 里，容易配出一个调不通的默认值
                    validator: (_, v: string) =>
                      v && watchedModels.length && !watchedModels.includes(v)
                        ? Promise.reject(new Error('默认模型必须在「可选模型」列表内'))
                        : Promise.resolve(),
                  },
                ]}
              >
                <Select
                  placeholder="先在上方添加模型"
                  options={watchedModels.map((m) => ({ label: m, value: m }))}
                />
              </Form.Item>
            </Col>
            <Col xs={12} sm={6}>
              <Form.Item name="timeout" label="超时（秒）">
                <InputNumber min={5} max={600} style={{ width: '100%' }} />
              </Form.Item>
            </Col>
            <Col xs={12} sm={6}>
              <Form.Item name="maxConcurrency" label="最大并发">
                <InputNumber min={1} max={50} style={{ width: '100%' }} />
              </Form.Item>
            </Col>
          </Row>

          <Row gutter={12}>
            <Col xs={12} sm={8}>
              <Form.Item name="temperature" label="温度">
                <Slider min={0} max={2} step={0.1} />
              </Form.Item>
            </Col>
            <Col xs={12} sm={8}>
              <Form.Item name="maxTokens" label="最大输出 tokens">
                <InputNumber min={1} max={100000} step={256} style={{ width: '100%' }} />
              </Form.Item>
            </Col>
          </Row>

          <Form.Item name="desc" label="用途说明">
            <Input.TextArea rows={2} placeholder="说明该通道的适用场景，便于多人协作时辨认" />
          </Form.Item>
        </Form>
      </Modal>
    </div>
  );
};

export default SystemModels;
