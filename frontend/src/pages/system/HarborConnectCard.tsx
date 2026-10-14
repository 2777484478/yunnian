import React, { useEffect, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Button,
  Card,
  Col,
  Divider,
  Input,
  InputNumber,
  Row,
  Space,
  Switch,
  Tag,
  Typography,
} from 'antd';
import { CheckCircleOutlined, HddOutlined, SaveOutlined, ThunderboltOutlined } from '@ant-design/icons';
import { DisabledTip } from '@/components/DisabledTip';
import { harborApi, type HarborSettings, type HarborTestResult } from '@/api/harbor';
import { useApi } from '@/hooks/useApi';

const { Text, Paragraph } = Typography;

/**
 * Harbor 接入配置卡片（「系统管理 › 第三方集成」）。
 *
 * 两条通道，读的东西不同：
 *
 *   registry 组件地址  走 /v2/（OCI Distribution），读 manifest/digest —— 判"这个 tag 在不在"
 *   Core API 地址      读 tag 列表（v2 还带推送时间）—— 判"有哪些 tag、哪个最新"
 *
 * ⚠️ **两者通常就在同一个入口上**，所以 Core 地址留空即可（平台按 registry 地址探）：
 * Harbor v2 在一个端口同时提供 /v2/ 与 /api/v2.0/；实测那台 Harbor **v1.1.2**
 * 的 /v2/ 与 /api/ 也都在 :5000。这个字段只是给"registry 走独立端口/镜像站"的部署留的覆盖项。
 *
 * 接口路径按 Harbor 版本自动识别（v1 → /api/，v2 → /api/v2.0/），
 * 所以**只填入口地址**，别带 /api 或 /api/v2.0 后缀。
 * v1 拿不到 tag 推送时间（只能给镜像构建时间），测试连接会把这条边界明说出来。
 *
 * 口令：与 Jenkins 同一口径 —— 只回"配没配"，留空保存＝不修改。
 */
const HarborConnectCard: React.FC<{ canWrite: boolean; onChanged?: () => void }> = ({
  canWrite,
  onChanged,
}) => {
  const { message } = AntApp.useApp();
  const api = useApi(() => harborApi.settings(), []);
  const cur = api.data;

  const [form, setForm] = useState<HarborSettings | null>(null);
  const [password, setPassword] = useState('');
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [result, setResult] = useState<HarborTestResult | null>(null);

  // 读回来的值灌进表单。**只在首次拿到数据时灌**，否则每次 refresh 都会把
  // 用户正在编辑的内容顶掉（改地址改到一半、后台刷新一下，前功尽弃）。
  useEffect(() => {
    if (cur && !form) setForm(cur);
  }, [cur, form]);

  if (!form) {
    return (
      <Card className="yn-section-card" variant="borderless" loading={api.loading} title="镜像仓库 Harbor" />
    );
  }

  const patch = (p: Partial<HarborSettings>) => setForm((f) => (f ? { ...f, ...p } : f));

  /** 组装保存/测试的请求体。口令只在"真的填了"时才带上。 */
  const body = () => ({
    enabled: form.enabled,
    baseUrl: form.baseUrl,
    coreApiUrl: form.coreApiUrl,
    registry: form.registry,
    username: form.username,
    insecure: form.insecure,
    timeout: form.timeout,
  });

  const save = async () => {
    setSaving(true);
    try {
      const saved = await harborApi.save({ ...body(), password });
      setForm(saved);
      setPassword('');
      setResult(null);
      message.success('Harbor 接入配置已保存');
      onChanged?.();
    } catch (e) {
      message.error((e as Error).message);
    } finally {
      setSaving(false);
    }
  };

  /**
   * 表单是否与库里那份一致（口令除外）。
   *
   * 一致就发空请求体 ⇒ 后端测**库里已保存的那份**，结论会标"测的是已保存的配置"。
   * 这不是文字游戏：用户想知道的是"我存进去的这套能不能用"，
   * 而"我刚打的字能不能用"是另一件事（后者受未保存的编辑影响，会误导人）。
   */
  const sameAsSaved =
    !!cur &&
    !password &&
    cur.enabled === form.enabled &&
    cur.baseUrl === form.baseUrl &&
    cur.coreApiUrl === form.coreApiUrl &&
    cur.registry === form.registry &&
    cur.username === form.username &&
    cur.insecure === form.insecure &&
    cur.timeout === form.timeout;

  const test = async () => {
    setTesting(true);
    try {
      // 表单改过：带上表单里的值（含刚输入的密码）支持"先测再存"——
      // 否则用户必须把一份没验过的配置写进库才能测。
      const res = sameAsSaved
        ? await harborApi.test()
        : await harborApi.test({ ...body(), ...(password ? { password } : {}) });
      setResult(res);
      if (res.ok) message.success('Harbor 两条通道检查完成');
    } catch (e) {
      message.error((e as Error).message);
    } finally {
      setTesting(false);
    }
  };

  const channelAlert = (ok: boolean, msg: string, label: string, missing: boolean) =>
    msg ? (
      <Alert
        type={ok ? 'success' : missing ? 'warning' : 'error'}
        showIcon
        style={{ marginTop: 8 }}
        message={`${label}：${msg}`}
      />
    ) : null;

  return (
    <Card
      className="yn-section-card"
      variant="borderless"
      title={
        <Space size={8}>
          <HddOutlined />
          镜像仓库 Harbor
          <Tag>{form.source === 'db' ? '平台数据库' : 'configs/config.yaml'}</Tag>
          <Tag color="default">{form.impl}</Tag>
        </Space>
      }
      extra={
        <Space>
          <DisabledTip title={canWrite ? undefined : '需要 user:manage 权限'}>
            <Button icon={<ThunderboltOutlined />} onClick={test} loading={testing} disabled={!canWrite}>
              测试连接
            </Button>
          </DisabledTip>
          <DisabledTip title={canWrite ? undefined : '需要 user:manage 权限'}>
            <Button type="primary" icon={<SaveOutlined />} onClick={save} loading={saving} disabled={!canWrite}>
              保存
            </Button>
          </DisabledTip>
        </Space>
      }
    >
      <Paragraph type="secondary" style={{ fontSize: 12, marginBottom: 12 }}>
        Harbor 有<b>两条通道</b>：registry 读镜像是否存在（digest），Core 读 tag 列表。
        两者<b>通常就在同一个入口上</b> —— <b>Core 地址留空即按上面的 registry 地址探测</b>
        （实测那台 Harbor v1.1.2 的 /v2/ 与 /api/ 都在 :5000）；只有 registry 走独立端口/镜像站时才需要分开填。
      </Paragraph>

      <Row gutter={16}>
        <Col xs={24} md={12}>
          <div style={{ fontSize: 12, color: '#5c6470', marginBottom: 4 }}>registry 组件地址（走 /v2/）</div>
          <Input
            value={form.baseUrl}
            onChange={(e) => patch({ baseUrl: e.target.value })}
            placeholder="https://es-repo.example.com:5000"
            disabled={!canWrite}
          />
        </Col>
        <Col xs={24} md={12}>
          <div style={{ fontSize: 12, color: '#5c6470', marginBottom: 4 }}>
            Core API 地址{' '}
            {form.coreApiUrl ? null : <Tag>留空＝用上面的地址</Tag>}
          </div>
          <Input
            value={form.coreApiUrl}
            onChange={(e) => patch({ coreApiUrl: e.target.value })}
            placeholder="留空即可（多数部署与 registry 同端口）"
            disabled={!canWrite}
          />
          <Text type="secondary" style={{ fontSize: 12 }}>
            只填入口地址，不要带 /api 或 /api/v2.0 —— 路径按 Harbor 版本自动识别
          </Text>
        </Col>
        <Col xs={24} md={12}>
          <div style={{ fontSize: 12, color: '#5c6470', marginBottom: 4 }}>镜像地址里的仓库部分</div>
          <Input
            value={form.registry}
            onChange={(e) => patch({ registry: e.target.value })}
            placeholder="es-repo.example.com:5000"
            disabled={!canWrite}
          />
          <Text type="secondary" style={{ fontSize: 12 }}>
            不要带 http(s):// —— 它会被原样拼进镜像引用
          </Text>
        </Col>
        <Col xs={24} md={12}>
          <div style={{ fontSize: 12, color: '#5c6470', marginBottom: 4 }}>账号（建议用只读 robot 账号）</div>
          <Input
            value={form.username}
            onChange={(e) => patch({ username: e.target.value })}
            placeholder="robot$yunnian"
            disabled={!canWrite}
          />
        </Col>
        <Col xs={24} md={12}>
          <div style={{ fontSize: 12, color: '#5c6470', marginBottom: 4 }}>
            密码{' '}
            {form.hasPassword ? <Tag color="green">已配置</Tag> : <Tag>未配置</Tag>}
          </div>
          <Input.Password
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder={form.hasPassword ? '留空＝不修改' : 'Harbor 账号的口令'}
            disabled={!canWrite}
          />
        </Col>
        <Col xs={12} md={6}>
          <div style={{ fontSize: 12, color: '#5c6470', marginBottom: 4 }}>跳过证书校验</div>
          <Switch
            checked={form.insecure}
            onChange={(v) => patch({ insecure: v })}
            disabled={!canWrite}
            checkedChildren="跳过"
            unCheckedChildren="校验"
          />
        </Col>
        <Col xs={12} md={6}>
          <div style={{ fontSize: 12, color: '#5c6470', marginBottom: 4 }}>启用接入</div>
          <Switch
            checked={form.enabled}
            onChange={(v) => patch({ enabled: v })}
            disabled={!canWrite}
            checkedChildren="启用"
            unCheckedChildren="停用"
          />
        </Col>
        <Col xs={24} md={6}>
          <div style={{ fontSize: 12, color: '#5c6470', marginBottom: 4 }}>超时（秒）</div>
          <InputNumber
            min={1}
            max={120}
            style={{ width: '100%' }}
            value={form.timeout}
            onChange={(v) => patch({ timeout: v ?? 15 })}
            disabled={!canWrite}
          />
        </Col>
      </Row>

      {result ? (
        <>
          <Divider style={{ margin: '16px 0 8px' }} />
          <Alert
            type={result.ok ? 'success' : 'warning'}
            showIcon
            icon={result.ok ? <CheckCircleOutlined /> : undefined}
            message={
              <>
                {result.message}
                <Text type="secondary" style={{ marginLeft: 8, fontSize: 12 }}>
                  {result.usedSavedValues ? '（测的是已保存的配置）' : '（测的是表单里未保存的值）'}
                  {` · 耗时 ${result.latency}ms`}
                </Text>
              </>
            }
          />
          {channelAlert(result.registryOk, result.registryMessage, 'registry 通道', !form.baseUrl)}
          {/* 结论直接透传后端那句话：它已经把版本、可见数量**以及能力边界**
              （v1 没有推送时间）写在一起了，前端再拼一遍只会拼重或拼错。 */}
          {channelAlert(result.coreOk, result.coreMessage, 'Core 通道', !form.baseUrl && !form.coreApiUrl)}
          {result.coreOk && result.coreNoPushTime ? (
            <Alert
              type="warning"
              showIcon
              style={{ marginTop: 8 }}
              message="这台 Harbor 是 v1 接口，没有 tag 推送时间"
              description="下拉里的“最新标签”只能按镜像构建时间或标签名排序 —— 界面上会标明用的是哪一种，不会把它写成“推送时间”。"
            />
          ) : null}
        </>
      ) : null}

      {form.updatedAt ? (
        <div style={{ marginTop: 12, fontSize: 12, color: '#9aa0aa' }}>
          最后修改：{form.updatedAt.slice(0, 19).replace('T', ' ')}
          {form.identity ? ` · 上次识别到的账号：${form.identity}` : ''}
        </div>
      ) : null}
    </Card>
  );
};

export default HarborConnectCard;
