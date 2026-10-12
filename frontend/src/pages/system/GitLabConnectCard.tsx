import React, { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Button,
  Col,
  Descriptions,
  Input,
  InputNumber,
  Popconfirm,
  Row,
  Space,
  Switch,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import {
  ApiOutlined,
  CheckCircleOutlined,
  GithubOutlined,
  KeyOutlined,
  ExclamationCircleOutlined,
} from '@ant-design/icons';
import { SectionCard } from '@/components/ui';
import { gitlabApi, ApiError } from '@/api';
import type { GitLabSettings, GitLabTestResult } from '@/api';
import { fmt } from '@/utils/format';

const { Text, Link } = Typography;

// ---------------------------------------------------------------------------
// GitLab 接入配置卡片
//
// 为什么单独做成一张卡片、而不是塞进下面的"平台运行参数"：
// 那几个参数是平台自身的运行数值（超时、保留天数），改错了顶多行为不对；
// 而这里管的是**凭据**——需要脱敏回显、需要连通性测试、
// 需要"清空"与"不修改"两种意图分开表达。混在一起会让三类操作都变得含糊。
//
// 页面上刻意呈现三件容易被做假的事：
//   1. **配置来源**（平台库 / config.yaml）——不显示的话，运维会对着
//      config.yaml 改半天，而实际生效的是库里那份。
//   2. **实际实现**（gitlab / local）——开关打开 ≠ 真的在用；
//      连不上时仍是 gitlab 实现，故障由探活结论体现。
//   3. **令牌元数据**（作用域 / 到期日）——这两项不体现在"连接成功"里，
//      却是"过一阵子突然全挂"的常见原因，所以测试结果里必须带出来。
// ---------------------------------------------------------------------------

interface Props {
  /** 有 user:manage 才可读写；无权限时只说明原因，不发起请求（接口会 403）。 */
  canWrite: boolean;
  /** 保存成功后通知父组件刷新集成状态（启用状态会影响 impl）。 */
  onChanged?: () => void;
}

/** 判定测试是否针对"表单里改过但还没保存的值" */
function isDirty(form: FormState, saved: GitLabSettings): boolean {
  return (
    form.baseUrl !== saved.baseUrl ||
    form.timeout !== saved.timeout ||
    form.token.trim() !== ''
  );
}

interface FormState {
  enabled: boolean;
  baseUrl: string;
  timeout: number;
  token: string;
}

const emptyForm: FormState = { enabled: false, baseUrl: '', timeout: 10, token: '' };

const GitLabConnectCard: React.FC<Props> = ({ canWrite, onChanged }) => {
  const { message, notification } = AntApp.useApp();

  const [saved, setSaved] = useState<GitLabSettings | null>(null);
  const [loading, setLoading] = useState(canWrite);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [form, setForm] = useState<FormState>(emptyForm);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [result, setResult] = useState<GitLabTestResult | null>(null);
  /** 本次测试用的是不是表单里未保存的值，用于给结果加一句说明 */
  const [testedDraft, setTestedDraft] = useState(false);

  const load = async () => {
    if (!canWrite) return;
    setLoading(true);
    try {
      const s = await gitlabApi.settings();
      setSaved(s);
      setForm({ enabled: s.enabled, baseUrl: s.baseUrl, timeout: s.timeout, token: '' });
      setLoadError(null);
    } catch (e) {
      setLoadError(e instanceof ApiError ? e.message : '读取 GitLab 配置失败');
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
    // 仅在权限变化时重取；load 内部只用 canWrite。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [canWrite]);

  // 保存成功后后端会回一份权威配置，逐字段对齐，避免表单与库内不一致。
  const syncFromServer = (s: GitLabSettings) => {
    setSaved(s);
    setForm({ enabled: s.enabled, baseUrl: s.baseUrl, timeout: s.timeout, token: '' });
  };

  const dirty = useMemo(
    () => (saved ? isDirty(form, saved) || form.enabled !== saved.enabled : false),
    [form, saved],
  );

  const save = async () => {
    setSaving(true);
    try {
      const next = await gitlabApi.save({
        enabled: form.enabled,
        baseUrl: form.baseUrl.trim(),
        timeout: form.timeout,
        // 留空即"不修改"：不传该字段，避免被当成清空
        ...(form.token.trim() ? { token: form.token.trim() } : {}),
      });
      syncFromServer(next);
      setResult(null);
      notification.success({
        message: 'GitLab 配置已保存',
        description:
          '配置立即生效，无需重启平台。建议接着点一次「测试连接」确认令牌可用。',
      });
      onChanged?.();
    } catch (e) {
      message.error(e instanceof ApiError ? e.message : '保存失败');
    } finally {
      setSaving(false);
    }
  };

  const clearToken = async () => {
    setSaving(true);
    try {
      // 必须显式传 clearToken：留空 token 的语义是"不修改"，
      // 想删掉已存凭据只能这样表达。
      const next = await gitlabApi.save({
        enabled: form.enabled,
        baseUrl: form.baseUrl.trim(),
        timeout: form.timeout,
        clearToken: true,
      });
      syncFromServer(next);
      setResult(null);
      message.success('已清除平台保存的访问令牌');
      onChanged?.();
    } catch (e) {
      message.error(e instanceof ApiError ? e.message : '清除失败');
    } finally {
      setSaving(false);
    }
  };

  const test = async () => {
    setTesting(true);
    const draft = saved ? isDirty(form, saved) : false;
    try {
      // 表单里有未保存的改动就测改动后的值（"先测再存"），否则测库里那份。
      const res = await gitlabApi.test(
        draft
          ? {
              baseUrl: form.baseUrl.trim(),
              timeout: form.timeout,
              ...(form.token.trim() ? { token: form.token.trim() } : {}),
            }
          : {},
      );
      setResult(res);
      setTestedDraft(draft);
      if (res.ok) {
        message.success(`连通正常（${res.latency}ms）`);
        onChanged?.();
      } else {
        // ok=false 是正常响应：请求发出去了，但没通过（401/地址错/网络不通）。
        message.warning('连接未通过，原因见下方');
      }
    } catch (e) {
      message.error(e instanceof ApiError ? e.message : '测试请求失败');
    } finally {
      setTesting(false);
    }
  };

  const statusTag = () => {
    if (!saved) return null;
    if (!saved.enabled) {
      return <Tag color="default">未启用 · 使用本地模拟</Tag>;
    }
    if (saved.impl === 'gitlab') {
      return <Tag color="blue">已启用 · 实际实现 gitlab</Tag>;
    }
    return <Tag color="warning">已启用 · 但实现仍为 {saved.impl}</Tag>;
  };

  if (!canWrite) {
    return (
      <SectionCard title="GitLab 接入" style={{ marginBottom: 16 }} bodyStyle={{ padding: '16px 20px' }}>
        <Alert
          type="info"
          showIcon
          icon={<KeyOutlined />}
          message="只读视图"
          description="GitLab 的接入地址与访问令牌涉及凭据，读取与修改都需要 user:manage 权限。"
        />
      </SectionCard>
    );
  }

  return (
    <SectionCard
      title="GitLab 接入"
      style={{ marginBottom: 16 }}
      bodyStyle={{ padding: '16px 20px' }}
      extra={statusTag()}
    >
      {/* 认证方式说明放在最前面：这是用户最可能问的第一个问题，
          与其让他在输入框上试错，不如一次说清为什么没有账号密码这一项。 */}
      <Alert
        type="info"
        showIcon
        icon={<GithubOutlined />}
        style={{ marginBottom: 16 }}
        message="GitLab 只支持访问令牌（Personal Access Token），不支持账号密码"
        description={
          <div style={{ fontSize: 12.5, lineHeight: 1.9 }}>
            <div>
              GitLab 的 REST API 只认令牌，没有"用账号密码登录 API"这种方式
              （<Text code>PRIVATE-TOKEN</Text> 头）。所以这里需要的是一个令牌，而不是密码。
            </div>
            <div style={{ marginTop: 4 }}>
              用 GitLab 账号密码换令牌的那条路（OAuth 密码模式）也不建议用：开启了两步验证的账号
              直接不可用，且换来的令牌只有 2 小时有效期，需要不断续期——一旦续期失败，
              构建和部署会一起断掉。
            </div>
            <div style={{ marginTop: 4 }}>
              创建方式：GitLab 右上角头像 → <Text strong>Edit profile</Text> →{' '}
              <Text strong>Access tokens</Text>，作用域勾 <Text code>api</Text>
              （只读场景 <Text code>read_api</Text> 也可），保存后令牌只显示一次。
              {saved?.tokenPageUrl ? (
                <>
                  {' '}
                  <Link href={saved.tokenPageUrl} target="_blank" rel="noreferrer">
                    打开创建页面
                  </Link>
                </>
              ) : null}
            </div>
          </div>
        }
      />

      {loadError ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message="GitLab 配置加载失败"
          description={loadError}
          action={
            <Button size="small" onClick={load}>
              重试
            </Button>
          }
        />
      ) : null}

      <Descriptions column={{ xs: 1, sm: 2, xl: 3 }} size="small" colon={false}>
        <Descriptions.Item label="启用接入">
          <Space size={8}>
            <Switch
              size="small"
              checked={form.enabled}
              loading={loading}
              onChange={(v) => setForm((f) => ({ ...f, enabled: v }))}
            />
            <Text type="secondary" style={{ fontSize: 12 }}>
              关闭时平台使用内置本地实现，不会访问 GitLab
            </Text>
          </Space>
        </Descriptions.Item>

        <Descriptions.Item label="服务地址">
          <Input
            size="small"
            style={{ width: 240 }}
            className="yn-mono"
            placeholder="https://gitlab.example.com"
            value={form.baseUrl}
            onChange={(e) => setForm((f) => ({ ...f, baseUrl: e.target.value }))}
          />
          <div>
            <Text type="secondary" style={{ fontSize: 11.5 }}>
              填 GitLab 站点根地址，不是仓库地址
            </Text>
          </div>
        </Descriptions.Item>

        <Descriptions.Item label="请求超时">
          <Space size={6}>
            <InputNumber
              size="small"
              min={1}
              max={300}
              value={form.timeout}
              onChange={(v) => setForm((f) => ({ ...f, timeout: v ?? 10 }))}
            />
            <Text type="secondary" style={{ fontSize: 12 }}>
              秒
            </Text>
          </Space>
        </Descriptions.Item>

        <Descriptions.Item label="访问令牌" span={2}>
          <Input.Password
            size="small"
            style={{ width: 280 }}
            className="yn-mono"
            autoComplete="new-password"
            placeholder={saved?.hasToken ? '留空表示不修改现有令牌' : '粘贴令牌，如 glpat-xxxxxxxxxxxx'}
            value={form.token}
            onChange={(e) => setForm((f) => ({ ...f, token: e.target.value }))}
          />
          <Space size={8} style={{ marginLeft: 8 }}>
            {saved?.hasToken ? (
              <>
                <Tooltip title="平台只保存密文，这里显示的是掩码">
                  <Tag className="yn-mono" style={{ marginInlineEnd: 0 }}>
                    已配置 {saved.tokenMasked}
                  </Tag>
                </Tooltip>
                <Popconfirm
                  title="清除已保存的访问令牌？"
                  description="清除后平台将无法访问 GitLab 仓库，需要重新填写。"
                  okText="清除"
                  cancelText="取消"
                  onConfirm={clearToken}
                >
                  <Button size="small" danger type="link" loading={saving}>
                    清除
                  </Button>
                </Popconfirm>
              </>
            ) : (
              <Tag style={{ marginInlineEnd: 0 }}>未配置</Tag>
            )}
          </Space>
          <div>
            <Text type="secondary" style={{ fontSize: 11.5 }}>
              令牌加密存储（AES-256-GCM），保存后无法读回明文，只能重新填一次
            </Text>
          </div>
        </Descriptions.Item>
      </Descriptions>

      <div style={{ marginTop: 16 }}>
        <Space size={8} wrap>
          <Button type="primary" size="small" loading={saving} disabled={!dirty} onClick={save}>
            保存
          </Button>
          <Button size="small" icon={<ApiOutlined />} loading={testing} onClick={test}>
            测试连接
          </Button>
          <Button
            size="small"
            disabled={!dirty || saving}
            onClick={() => {
              if (!saved) return;
              setForm({ enabled: saved.enabled, baseUrl: saved.baseUrl, timeout: saved.timeout, token: '' });
              setResult(null);
            }}
          >
            放弃修改
          </Button>
          {saved ? (
            <Text type="secondary" style={{ fontSize: 12 }}>
              配置来源：
              {saved.source === 'db' ? (
                <Tag color="blue" style={{ marginInlineEnd: 0 }}>
                  平台数据库
                </Tag>
              ) : (
                <Tooltip title="尚未在界面保存过，当前用的是 configs/config.yaml 里的值；保存后以平台库为准">
                  <Tag style={{ marginInlineEnd: 0 }}>config.yaml</Tag>
                </Tooltip>
              )}
              {saved.updatedAt ? ` · 最后修改 ${fmt(saved.updatedAt)}` : null}
            </Text>
          ) : null}
        </Space>
      </div>

      {/* ---------------- 令牌身份与到期 ---------------- */}
      {saved?.hasToken && (saved.identity || saved.tokenExpiresAt) ? (
        <Row gutter={[16, 8]} style={{ marginTop: 12 }}>
          <Col>
            <Text type="secondary" style={{ fontSize: 12 }}>
              令牌身份：
            </Text>
            <Text style={{ fontSize: 12.5 }}>{saved.identity || '未知'}</Text>
          </Col>
          {saved.tokenExpiresAt ? (
            <Col>
              <Text type="secondary" style={{ fontSize: 12 }}>
                令牌到期：
              </Text>
              <Text style={{ fontSize: 12.5 }}>{saved.tokenExpiresAt}</Text>
            </Col>
          ) : null}
        </Row>
      ) : null}

      {/* ---------------- 测试结果 ---------------- */}
      {result ? (
        <Alert
          style={{ marginTop: 16 }}
          type={result.ok ? 'success' : 'error'}
          showIcon
          icon={result.ok ? <CheckCircleOutlined /> : <ExclamationCircleOutlined />}
          message={
            <Space size={8} wrap>
              <span>{result.ok ? '连接正常' : '连接未通过'}</span>
              <Tag style={{ marginInlineEnd: 0 }}>{result.latency}ms</Tag>
              <Tag color={testedDraft ? 'orange' : 'default'} style={{ marginInlineEnd: 0 }}>
                {testedDraft ? '测的是表单里未保存的值' : '测的是已保存的配置'}
              </Tag>
            </Space>
          }
          description={
            <div style={{ fontSize: 12.5, lineHeight: 1.9 }}>
              <div>{result.message}</div>
              {result.ok ? (
                <div style={{ marginTop: 4 }}>
                  {[
                    result.version ? `版本 ${result.version}` : '',
                    result.username ? `身份 ${result.username}${result.name && result.name !== result.username ? `（${result.name}）` : ''}${result.isAdmin ? ' · 管理员' : ''}` : '',
                    result.scopes?.length ? `作用域 ${result.scopes.join('、')}` : '',
                    result.expiresAt ? `到期 ${result.expiresAt}` : '',
                  ]
                    .filter(Boolean)
                    .join(' · ')}
                </div>
              ) : null}
              {/* 警告项与"是否连上"无关：能连上但迟早出事，必须单独列出来 */}
              {result.warnings?.length ? (
                <ul style={{ margin: '6px 0 0', paddingLeft: 18 }}>
                  {result.warnings.map((w) => (
                    <li key={w} style={{ color: '#d46b08' }}>
                      {w}
                    </li>
                  ))}
                </ul>
              ) : null}
            </div>
          }
        />
      ) : null}
    </SectionCard>
  );
};

export default GitLabConnectCard;
