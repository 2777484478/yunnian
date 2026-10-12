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
  ExclamationCircleOutlined,
  KeyOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons';
import { SectionCard } from '@/components/ui';
import { jenkinsApi, ApiError } from '@/api';
import type { JenkinsSettings, JenkinsTestResult } from '@/api';
import { fmt } from '@/utils/format';

const { Text, Link } = Typography;

// ---------------------------------------------------------------------------
// Jenkins 接入配置卡片
//
// 与隔壁 GitLab 卡片同构（同类事用同一套交互，不该让用户学两遍），
// 差异只在认证方式上，而这处差异必须写在最显眼的位置：
//
//   GitLab 是「令牌一个值」；Jenkins 是 HTTP Basic，需要**用户名 + 令牌两个值**。
//   而且 Jenkins **确实**接受账号密码——所以用户一定会问"为什么让我填令牌"。
//   与其让他在输入框上试错，不如一次说清：不是不能用密码，是密码一旦被改、
//   或者账号启用了双因素，平台就会在某个毫无征兆的早上静默失效；
//   令牌可以单独吊销、可以只给读权限，且不受改密码影响。
//
// 另外两件容易被做假的事，与 GitLab 卡片保持一致口径：
//   1. 配置来源（平台库 / config.yaml）——不显示就会对着配置文件改半天；
//   2. 实际实现（jenkins / local）——开关打开 ≠ 真的在用。
// ---------------------------------------------------------------------------

interface Props {
  /** 有 user:manage 才可读写；无权限时只说明原因，不发起请求（接口会 403）。 */
  canWrite: boolean;
  /** 保存成功后通知父组件刷新集成状态（启用状态会影响 impl）。 */
  onChanged?: () => void;
}

interface FormState {
  enabled: boolean;
  baseUrl: string;
  username: string;
  timeout: number;
  token: string;
}

const emptyForm: FormState = { enabled: false, baseUrl: '', username: '', timeout: 10, token: '' };

/** 判定测试是否针对"表单里改过但还没保存的值" */
function isDirty(form: FormState, saved: JenkinsSettings): boolean {
  return (
    form.baseUrl !== saved.baseUrl ||
    form.username !== saved.username ||
    form.timeout !== saved.timeout ||
    form.token.trim() !== ''
  );
}

const toForm = (s: JenkinsSettings): FormState => ({
  enabled: s.enabled,
  baseUrl: s.baseUrl,
  username: s.username,
  timeout: s.timeout,
  token: '',
});

const JenkinsConnectCard: React.FC<Props> = ({ canWrite, onChanged }) => {
  const { message, notification } = AntApp.useApp();

  const [saved, setSaved] = useState<JenkinsSettings | null>(null);
  const [loading, setLoading] = useState(canWrite);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [form, setForm] = useState<FormState>(emptyForm);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [result, setResult] = useState<JenkinsTestResult | null>(null);
  const [testedDraft, setTestedDraft] = useState(false);

  const load = async () => {
    if (!canWrite) return;
    setLoading(true);
    try {
      const s = await jenkinsApi.settings();
      setSaved(s);
      setForm(toForm(s));
      setLoadError(null);
    } catch (e) {
      setLoadError(e instanceof ApiError ? e.message : '读取 Jenkins 配置失败');
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
    // 仅在权限变化时重取；load 内部只用 canWrite。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [canWrite]);

  const syncFromServer = (s: JenkinsSettings) => {
    setSaved(s);
    setForm(toForm(s));
  };

  const dirty = useMemo(
    () => (saved ? isDirty(form, saved) || form.enabled !== saved.enabled : false),
    [form, saved],
  );

  const save = async () => {
    setSaving(true);
    try {
      const next = await jenkinsApi.save({
        enabled: form.enabled,
        baseUrl: form.baseUrl.trim(),
        username: form.username.trim(),
        timeout: form.timeout,
        // 留空即"不修改"：不传该字段，避免被当成清空
        ...(form.token.trim() ? { token: form.token.trim() } : {}),
      });
      syncFromServer(next);
      setResult(null);
      notification.success({
        message: 'Jenkins 配置已保存',
        description: '配置立即生效，无需重启平台。建议接着点一次「测试连接」确认账号可用。',
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
      const next = await jenkinsApi.save({
        enabled: form.enabled,
        baseUrl: form.baseUrl.trim(),
        username: form.username.trim(),
        timeout: form.timeout,
        clearToken: true,
      });
      syncFromServer(next);
      setResult(null);
      message.success('已清除平台保存的密钥');
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
      const res = await jenkinsApi.test(
        draft
          ? {
              baseUrl: form.baseUrl.trim(),
              username: form.username.trim(),
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
    if (saved.impl === 'jenkins') {
      return <Tag color="blue">已启用 · 实际实现 jenkins</Tag>;
    }
    return <Tag color="warning">已启用 · 但实现仍为 {saved.impl}</Tag>;
  };

  if (!canWrite) {
    return (
      <SectionCard title="Jenkins 接入" style={{ marginBottom: 16 }} bodyStyle={{ padding: '16px 20px' }}>
        <Alert
          type="info"
          showIcon
          icon={<KeyOutlined />}
          message="只读视图"
          description="Jenkins 的接入地址、用户名与 API Token 涉及凭据，读取与修改都需要 user:manage 权限。"
        />
      </SectionCard>
    );
  }

  return (
    <SectionCard
      title="Jenkins 接入"
      style={{ marginBottom: 16 }}
      bodyStyle={{ padding: '16px 20px' }}
      extra={statusTag()}
    >
      {/* 认证方式说明放在最前面：Jenkins 与 GitLab 不同，它确实接受账号密码，
          所以"为什么让我填令牌"一定会被问，不如一次说清。 */}
      <Alert
        type="info"
        showIcon
        icon={<ThunderboltOutlined />}
        style={{ marginBottom: 16 }}
        message="Jenkins 用「用户名 + 密钥」做 Basic 认证，密钥可填 API Token 或账号密码"
        description={
          <div style={{ fontSize: 12.5, lineHeight: 1.9 }}>
            <div>
              Jenkins 的 REST API 走 HTTP Basic 认证，需要<Text strong>两个值</Text>：
              用户名 + 密钥（拼成 <Text code>用户名:密钥</Text> 提交）。所以这里比 GitLab 多一个用户名字段。
            </div>
            <div style={{ marginTop: 4 }}>
              <Text strong>密钥两种都收：</Text>API Token 或账号密码。Basic 认证本身不区分，
              平台两种都支持（实测目标 Jenkins 用账号密码即可连通）。
            </div>
            <div style={{ marginTop: 4 }}>
              <Text strong>更推荐 API Token：</Text>账号密码虽然能用，但有两个会"过一阵子突然全挂"的毛病——
              账号改了密码（或被强制改密）平台就静默失效；账号一旦启用双因素认证，Basic 认证会直接被拒。
              令牌可以单独吊销而不影响账号、可以只授予
              <Text code>Overall/Read</Text> + <Text code>Job/Build</Text> 最小权限，且不受改密码影响。
            </div>
            <div style={{ marginTop: 4 }}>
              令牌生成方式：Jenkins 右上角用户名 → <Text strong>Configure</Text> →{' '}
              <Text strong>API Token</Text> → <Text strong>Add new Token</Text>，
              生成后只显示一次（该页也可重新生成）。
              {saved?.tokenPageUrl ? (
                <>
                  {' '}
                  <Link href={saved.tokenPageUrl} target="_blank" rel="noreferrer">
                    打开配置页面
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
          message="Jenkins 配置加载失败"
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
              关闭时平台使用内置本地实现，不会访问 Jenkins
            </Text>
          </Space>
        </Descriptions.Item>

        <Descriptions.Item label="服务地址">
          <Input
            size="small"
            style={{ width: 240 }}
            className="yn-mono"
            placeholder="http://jenkins.internal:8080"
            value={form.baseUrl}
            onChange={(e) => setForm((f) => ({ ...f, baseUrl: e.target.value }))}
          />
          <div>
            <Text type="secondary" style={{ fontSize: 11.5 }}>
              填 Jenkins 站点根地址，末尾不要带 /job/…
            </Text>
          </div>
        </Descriptions.Item>

        <Descriptions.Item label="用户名">
          <Input
            size="small"
            style={{ width: 200 }}
            className="yn-mono"
            autoComplete="off"
            placeholder="ops"
            value={form.username}
            onChange={(e) => setForm((f) => ({ ...f, username: e.target.value }))}
          />
          <div>
            <Text type="secondary" style={{ fontSize: 11.5 }}>
              令牌所属的 Jenkins 账号名
            </Text>
          </div>
        </Descriptions.Item>

        <Descriptions.Item label="密钥（Token / 密码）" span={2}>
          <Input.Password
            size="small"
            style={{ width: 280 }}
            className="yn-mono"
            autoComplete="new-password"
            placeholder={saved?.hasToken ? '留空表示不修改现有密钥' : '粘贴 API Token，或直接填账号密码'}
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
                  title="清除已保存的密钥？"
                  description="清除后平台将无法触发 Jenkins 构建，需要重新填写。"
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
              setForm(toForm(saved));
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

      {/* ---------------- 账号身份与 CSRF ---------------- */}
      {saved?.hasToken && saved.identity ? (
        <Row gutter={[16, 8]} style={{ marginTop: 12 }}>
          <Col>
            <Text type="secondary" style={{ fontSize: 12 }}>
              令牌账号：
            </Text>
            <Text style={{ fontSize: 12.5 }}>{saved.identity}</Text>
          </Col>
          {saved.crumbEnabled ? (
            <Col>
              <Text type="secondary" style={{ fontSize: 12 }}>
                CSRF 保护：
              </Text>
              <Tooltip title="Jenkins 开启了 CSRF 保护，触发构建前需要先取 crumb。平台会自动处理这一步，无需人工干预。">
                <Tag color="blue" style={{ marginInlineEnd: 0 }}>
                  已开启（平台自动取 crumb）
                </Tag>
              </Tooltip>
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
                    result.username ? `账号 ${result.username}` : '',
                    result.jobCount ? `可见 Job ${result.jobCount} 个` : '',
                    result.crumbEnabled ? 'CSRF 已开启（平台自动取 crumb）' : '',
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

export default JenkinsConnectCard;
