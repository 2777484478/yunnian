import React, { useMemo, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Badge,
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
  Tooltip,
  Typography,
} from 'antd';
import {
  ApiOutlined,
  CheckCircleOutlined,
  CloudServerOutlined,
  ClusterOutlined,
  ContainerOutlined,
  DatabaseOutlined,
  HddOutlined,
  GithubOutlined,
  InfoCircleOutlined,
  LinkOutlined,
  ReloadOutlined,
  SafetyCertificateOutlined,
  ThunderboltOutlined,
  WarningOutlined,
} from '@ant-design/icons';
import { PageHeader, SectionCard, StatCard } from '@/components/ui';
import { systemApi } from '@/api';
import GitLabConnectCard from '@/pages/system/GitLabConnectCard';
import JenkinsConnectCard from '@/pages/system/JenkinsConnectCard';
import HarborConnectCard from '@/pages/system/HarborConnectCard';
import { hasPerm } from '@/auth';
import { useApi } from '@/hooks/useApi';
import { fmt } from '@/utils/format';
import type { IntegrationStatus, SystemConfigItem } from '@/types';

const { Text } = Typography;

// ---------------------------------------------------------------------------
// 第三方集成
//
// 这一页的核心职责是**别让运维把模拟当真实**。后端 /system/integrations
// 返回的 `impl` 就是"当前是谁在干活"：`local` 表示内置模拟实现，
// 而不是真的连上了 Harbor / GitLab / K8s。因此：
//   - 状态徽标优先看 `impl`，local 一律标"本地模拟"（灰），
//     绝不用绿色"已连接"去覆盖一个其实没连的东西；
//   - 只有"显式启用且探活通过"（enabled && ready）才给绿色。
// ---------------------------------------------------------------------------

const ICONS: Record<string, React.ReactNode> = {
  harbor: <HddOutlined />,
  gitlab: <GithubOutlined />,
  jenkins: <ThunderboltOutlined />,
  kubernetes: <ClusterOutlined />,
  registry: <ContainerOutlined />,
  database: <DatabaseOutlined />,
  redis: <CloudServerOutlined />,
  sso: <SafetyCertificateOutlined />,
};

type BadgeStatus = 'success' | 'error' | 'warning' | 'default' | 'processing';

/** 依据 impl / enabled / ready 三字段判定展示态，顺序不能颠倒 */
function statusOf(it: IntegrationStatus): { text: string; badge: BadgeStatus; hint: string } {
  const isLocal = it.impl === 'local';
  if (isLocal) {
    return {
      text: '本地模拟',
      badge: 'default',
      hint: '未接入真实下游，平台使用内置本地实现，数据由平台自己产生',
    };
  }
  if (!it.enabled) {
    return { text: '未启用', badge: 'default', hint: `已配置 ${it.impl} 实现但未启用` };
  }
  if (!it.ready) {
    return { text: '连接异常', badge: 'error', hint: `已启用 ${it.impl} 实现，但最近一次探活未通过` };
  }
  return { text: '连接正常', badge: 'success', hint: `已启用 ${it.impl} 实现，探活通过` };
}

const CONFIG_GROUP_LABEL: Record<string, string> = {
  ai: 'AI 能力',
  audit: '审计日志',
  build: '构建',
  deploy: '部署',
  image: '镜像',
  notify: '通知',
  // GitLab 接入配置的**非密字段**落在同一张 system_config 表、group 为 integration，
  // 所以下面的"平台运行参数"里也会列出它们。这里改也生效（运行期每次都从库读），
  // 但建议用上面的专用卡片：那里有格式校验与连通性测试，
  // 而令牌不在这些键里——它加密存在 secret_credential，不会出现在本表中。
  integration: '下游集成',
  // 并发上限（构建 / 部署）。单独成组，因为它是"用着用着才发现要调"的那类旋钮 ——
  // 用户遇到"批量发布怎么只排队发一个"时，得能一眼找到它（见下方该组的说明）。
  runner: '运行参数（并发上限）',
};

/** 按 valueType 渲染合适的编辑器；值统一以字符串存取（后端契约） */
const ConfigEditor: React.FC<{
  item: SystemConfigItem;
  value: string;
  disabled: boolean;
  onChange: (v: string) => void;
}> = ({ item, value, disabled, onChange }) => {
  if (item.valueType === 'bool') {
    return (
      <Switch
        size="small"
        disabled={disabled}
        checked={value === 'true'}
        onChange={(v) => onChange(v ? 'true' : 'false')}
      />
    );
  }
  if (item.valueType === 'number') {
    return (
      <InputNumber
        size="small"
        disabled={disabled}
        style={{ width: 160 }}
        value={value === '' ? undefined : Number(value)}
        onChange={(v) => onChange(v === null || v === undefined ? '' : String(v))}
      />
    );
  }
  return (
    <Input
      size="small"
      disabled={disabled}
      style={{ width: 220 }}
      className="yn-mono"
      value={value}
      onChange={(e) => onChange(e.target.value)}
    />
  );
};

/** 数值型配置的可读化（字节/秒/毫秒 → 人话），只在描述里补一句，不改变提交值 */
function readableValue(item: SystemConfigItem): string | null {
  if (item.valueType !== 'number') return null;
  const n = Number(item.configValue);
  if (!Number.isFinite(n)) return null;
  if (item.configKey.endsWith('.max.size')) return `${(n / 1024 / 1024).toFixed(0)} MB`;
  if (item.configKey.endsWith('.ttl')) return `${(n / 60000).toFixed(0)} 分钟`;
  if (item.configKey.endsWith('.timeout') || item.configKey.endsWith('.timeout.default')) {
    return `${(n / 60).toFixed(0)} 分钟`;
  }
  return null;
}

const SystemIntegrations: React.FC = () => {
  const { message, notification } = AntApp.useApp();
  const canWrite = hasPerm('user:manage');

  const integrationsApi = useApi(() => systemApi.integrations(), []);
  const configApi = useApi(() => systemApi.config(), []);
  const statsApi = useApi(() => systemApi.stats(), []);

  const integrations = useMemo(() => integrationsApi.data ?? [], [integrationsApi.data]);
  const config = useMemo(() => configApi.data ?? [], [configApi.data]);

  // 编辑中的配置值：仅记录被改过的键，未改的沿用后端值，
  // 这样"保存"永远只提交真实的差异，不会把没动过的项也写一遍（那会产生无意义的审计记录）。
  const [draft, setDraft] = useState<Record<string, string>>({});
  const [saving, setSaving] = useState(false);

  const valueOf = (item: SystemConfigItem) => draft[item.configKey] ?? item.configValue;
  const dirtyKeys = Object.keys(draft).filter(
    (k) => draft[k] !== config.find((c) => c.configKey === k)?.configValue,
  );

  const configGroups = useMemo(() => {
    const m = new Map<string, SystemConfigItem[]>();
    config.forEach((c) => {
      const arr = m.get(c.groupName) ?? [];
      arr.push(c);
      m.set(c.groupName, arr);
    });
    return [...m.entries()];
  }, [config]);

  const summaries = useMemo(
    () => ({
      total: integrations.length,
      ready: integrations.filter((i) => i.impl !== 'local' && i.enabled && i.ready).length,
      local: integrations.filter((i) => i.impl === 'local').length,
      abnormal: integrations.filter((i) => i.impl !== 'local' && i.enabled && !i.ready).length,
    }),
    [integrations],
  );

  const saveConfig = async () => {
    if (!dirtyKeys.length) return;
    setSaving(true);
    try {
      // valueType 必须回传：后端用它挑校验分支，漏传等于把服务端校验关掉
      await systemApi.updateConfig(
        dirtyKeys.map((k) => {
          const item = config.find((c) => c.configKey === k);
          return { configKey: k, configValue: draft[k], valueType: item?.valueType ?? 'string' };
        }),
      );
      notification.success({
        message: '平台参数已更新',
        description: `本次提交 ${dirtyKeys.length} 项：${dirtyKeys.join('、')}。并发上限类参数（runner.*）已立即生效；其余个别参数需重启相关组件后才生效。`,
      });
      setDraft({});
      configApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message || '保存失败');
    } finally {
      setSaving(false);
    }
  };

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: '系统管理' }, { title: '第三方集成' }]}
        title="第三方集成"
        description="平台依赖的外部系统连接状态：Git 仓库、容器镜像仓库、构建引擎、K8s 集群与统一认证"
        extra={
          <Space>
            <Button
              icon={<ReloadOutlined />}
              onClick={() => {
                integrationsApi.refresh();
                configApi.refresh();
                statsApi.refresh();
                message.success('已刷新集成状态');
              }}
            >
              刷新状态
            </Button>
          </Space>
        }
      />

      <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
        <Col xs={12} xl={6}>
          <StatCard title="集成系统" value={summaries.total} unit="个" icon={<ApiOutlined />} />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="真实下游已就绪"
            value={summaries.ready}
            unit="个"
            accent="#52c41a"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                仅统计非本地实现的启用项
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="本地模拟实现"
            value={summaries.local}
            unit="个"
            accent="#8c8c8c"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                数据由平台自己产生，非真实下游
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="纳管仓库"
            value={statsApi.data?.projects ?? '-'}
            unit="个"
            accent="#1677ff"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                来自平台概览统计
              </Text>
            }
          />
        </Col>
      </Row>

      {summaries.abnormal > 0 ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message={`有 ${summaries.abnormal} 个已启用的下游连接异常`}
          description="这些集成已配置为真实实现但探活未通过，构建与部署可能因此失败，请优先排查。"
        />
      ) : null}

      <Alert
        type="info"
        showIcon
        icon={<LinkOutlined />}
        style={{ marginBottom: 16 }}
        message="凭证统一由平台托管"
        description="GitLab 访问令牌、Jenkins API Token、Harbor 账号口令均加密存储于平台数据库（AES-256-GCM），页面只显示掩码、不回显明文，修改后立即生效无需重启。K8s 集群凭据在「系统管理 › 集群接入」里单独维护（导入 kubeconfig 时逐集群探测）。"
      />

      {/* GitLab 接入配置。放在集成状态卡片之后、平台运行参数之前：
          先看"连没连上"，再改"怎么连"。 */}
      <GitLabConnectCard
        canWrite={canWrite}
        onChanged={() => {
          // 启用/停用会切换实际实现（gitlab ⇄ local），
          // 保存后必须重取集成状态，否则卡片上还写着旧的实现名。
          integrationsApi.refresh();
        }}
      />

      {/* Jenkins 接入配置。与 GitLab 卡片并排同层：两者都是「平台主动去调的下游」，
          认证方式不同（GitLab 一个令牌 / Jenkins 用户名+令牌），但配置与生效逻辑同构。 */}
      <JenkinsConnectCard
        canWrite={canWrite}
        onChanged={() => {
          // 同上：启用/停用会在 jenkins ⇄ local 之间切换实际实现。
          integrationsApi.refresh();
        }}
      />

      {/* Harbor 接入配置。与上面两张卡并排同层。
          它有**两条通道**（registry 组件 /v2/ 读 digest、Core /api/v2.0/ 读 tag 推送时间），
          所以卡片上是两个地址、测试连接分别出结论 —— 合成一个地址会出现
          "接上了但某一类信息永远读不到"。 */}
      <HarborConnectCard
        canWrite={canWrite}
        onChanged={() => {
          // 启用/停用会在 harbor-registry-v2 ⇄ local 之间切换实际实现。
          integrationsApi.refresh();
        }}
      />

      <Row gutter={[16, 16]}>
        {integrations.map((it) => {
          const st = statusOf(it);
          return (
            <Col key={it.key} xs={24} md={12} xl={8}>
              <Card
                className="yn-section-card"
                variant="borderless"
                style={{ height: '100%' }}
                loading={integrationsApi.loading}
                title={
                  <Space>
                    <span style={{ fontSize: 17, color: '#1677ff' }}>{ICONS[it.key] ?? <ApiOutlined />}</span>
                    <span>{it.name}</span>
                  </Space>
                }
                extra={
                  <Tooltip title={st.hint}>
                    <Badge status={st.badge} text={st.text} />
                  </Tooltip>
                }
              >
                <div style={{ fontSize: 13, color: '#5c6470', lineHeight: 1.9 }}>
                  <div>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      实现方式：
                    </Text>
                    <Tag
                      color={it.impl === 'local' ? 'default' : 'blue'}
                      className="yn-mono"
                      style={{ marginInlineEnd: 0, fontSize: 11.5 }}
                    >
                      {it.impl}
                    </Tag>
                  </div>
                  <div style={{ marginTop: 6 }}>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      启用状态：
                    </Text>
                    {it.enabled ? (
                      <Tag color="green" style={{ marginInlineEnd: 0 }}>
                        已启用
                      </Tag>
                    ) : (
                      <Tag style={{ marginInlineEnd: 0 }}>未启用</Tag>
                    )}
                    <Tag
                      color={it.ready ? 'success' : 'default'}
                      style={{ marginInlineEnd: 0, marginLeft: 6 }}
                    >
                      {it.ready ? '探活通过' : '未探活/未通过'}
                    </Tag>
                  </div>
                  <div style={{ marginTop: 8 }}>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      运行信息：
                    </Text>
                    {it.detail}
                  </div>
                </div>
              </Card>
            </Col>
          );
        })}
        {!integrationsApi.loading && !integrations.length ? (
          <Col span={24}>
            <Alert
              type="warning"
              showIcon
              message="未取到集成状态"
              description="请检查后端 /system/integrations 接口是否可用。"
              action={
                <Button size="small" onClick={integrationsApi.refresh}>
                  重试
                </Button>
              }
            />
          </Col>
        ) : null}
      </Row>

      <SectionCard
        title="平台运行参数"
        style={{ marginTop: 16 }}
        bodyStyle={{ padding: '16px 20px' }}
        extra={
          <Space>
            {dirtyKeys.length ? (
              <Text type="warning" style={{ fontSize: 12 }}>
                {dirtyKeys.length} 项待保存
              </Text>
            ) : null}
            <Button
              size="small"
              onClick={() => setDraft({})}
              disabled={!dirtyKeys.length || saving}
            >
              放弃修改
            </Button>
            <Button
              size="small"
              type="primary"
              loading={saving}
              disabled={!dirtyKeys.length || !canWrite}
              onClick={saveConfig}
            >
              保存
            </Button>
          </Space>
        }
      >
        {!canWrite ? (
          <Alert
            type="info"
            showIcon
            style={{ marginBottom: 14 }}
            message="只读视图"
            description="修改平台参数需要 user:manage 权限。"
          />
        ) : null}

        {configApi.error ? (
          <Alert
            type="error"
            showIcon
            message="平台参数加载失败"
            description={configApi.error.message}
            action={
              <Button size="small" onClick={configApi.refresh}>
                重试
              </Button>
            }
          />
        ) : null}

        {configGroups.map(([group, items], gi) => (
          <div key={group} style={{ marginTop: gi === 0 ? 0 : 18 }}>
            <Divider orientation="left" orientationMargin={0} style={{ margin: '8px 0 12px' }}>
              <span style={{ fontSize: 12.5 }}>
                {CONFIG_GROUP_LABEL[group] ?? group}
                <span className="yn-mono" style={{ marginLeft: 8, color: '#9aa0aa', fontSize: 11.5 }}>
                  {group}
                </span>
              </span>
            </Divider>
            {/* 这一组必须额外说明两件事，否则用户改完还会怀疑"到底生效没有"：
                ① 保存后立即生效、不需要重启（与页面底部那句"部分参数需重启"不同）；
                ② 并发不是越大越快 —— 构建吃机器、部署抢集群资源，还会打乱依赖顺序。 */}
            {group === 'runner' ? (
              <Alert
                type="info"
                showIcon
                style={{ marginBottom: 12 }}
                message="并发上限保存后立即生效，不需要重启服务"
                description={
                  <span style={{ fontSize: 12 }}>
                    超出并发数的任务会显示「排队中」，前面的任务结束后自动补位。
                    <br />
                    <b>构建并发对 Jenkins 项目同样生效</b>：轮到平台构建槽位时才把任务提交给
                    Jenkins，所以 Jenkins 侧不会堆积（执行器只跑该跑的那几个）。
                    提交后即使关掉页面也照常往下走。
                    <br />
                    <b>部署并发 1 = 逐服务串行发布</b>（默认）：同一环境一次只更新一个服务，
                    避免滚动更新互相抢占资源、也避免上游服务还没就绪下游就跟着发。
                    调大即同时发布多个不同服务（同一个服务始终不允许并发）。
                  </span>
                }
              />
            ) : null}
            {/*
              自定义网格而不是 `Descriptions`。
              
              ⚠️ 原来这里用 `label={item.description}`，而 description 是一整句
              （"同时执行的部署任务数上限（超出排队的等待）。1 = 逐服务串行发布，避免…"）。
              Descriptions 是**左右布局**：超长 label 把 content 一路往右挤，
              于是输入框被推到容器外，右侧露出一片空白 —— 用户看到的就是"好多框都出来了"。
              而且 3 列布局下每列更窄，长 label 还要换行，整页显得乱。
              
              改成**上下布局**（标签一行、控件一行），标签再长也只是自己换行、
              不会去挤控件；同时把 description 按第一个句号拆成"短标签 + 说明小字"，
              正文降到 11.5px 次要色。
            */}
            <div className="yn-cfg-grid">
              {items.map((item) => {
                // 拆成短标签 + 说明：description 的第一句当标签，其余当补充说明。
                // 拆不开就用整句当标签（不会因此丢信息）。
                const parts = (item.description || item.configKey).split('。');
                const shortLabel = parts[0];
                const longDesc = parts.slice(1).join('。').trim();
                return (
                  <div className="yn-cfg-item" key={item.configKey}>
                    <div className="yn-cfg-item__head">
                      <Tooltip title={item.configKey}>
                        <span className="yn-cfg-item__label">{shortLabel}</span>
                      </Tooltip>
                      {draft[item.configKey] !== undefined &&
                      draft[item.configKey] !== item.configValue ? (
                        <span className="yn-cfg-item__dirty">未保存</span>
                      ) : null}
                    </div>
                    <div className="yn-cfg-item__ctrl">
                      <ConfigEditor
                        item={item}
                        value={valueOf(item)}
                        disabled={!canWrite}
                        onChange={(v) => setDraft((d) => ({ ...d, [item.configKey]: v }))}
                      />
                      {readableValue(item) ? (
                        <Text type="secondary" style={{ fontSize: 11.5 }}>
                          ≈ {readableValue(item)}
                        </Text>
                      ) : null}
                    </div>
                    {longDesc ? <div className="yn-cfg-item__desc">{longDesc}</div> : null}
                  </div>
                );
              })}
            </div>
          </div>
        ))}

        {config.length ? (
          <div style={{ marginTop: 14 }}>
            <Space size={8} wrap>
              <InfoCircleOutlined style={{ color: '#1678ff' }} />
              <Text type="secondary" style={{ fontSize: 12 }}>
                共 {config.length} 项参数，最近更新：
                {config
                  .map((c) => c.updatedAt)
                  .filter(Boolean)
                  .sort()
                  .slice(-1)
                  .map((t) => fmt(t))[0] ?? '-'}
                。参数由后端 system_config 表驱动，保存后立即落库。
              </Text>
            </Space>
          </div>
        ) : null}
      </SectionCard>

      {summaries.local > 0 ? (
        <Alert
          type="warning"
          showIcon
          icon={<WarningOutlined />}
          style={{ marginTop: 16 }}
          message="当前多数组件运行在本地模拟实现下"
          description="平台自身的数据库与 Redis 是真实连接；Harbor / GitLab / Kubernetes 尚未接入真实下游，构建产物、仓库分支、集群资源等数据均由平台内置实现产生。用于流程演示没有问题，但请不要据此判断真实环境状态。"
        />
      ) : (
        <Alert
          type="success"
          showIcon
          icon={<CheckCircleOutlined />}
          style={{ marginTop: 16 }}
          message="全部下游均已接入真实实现"
        />
      )}
    </div>
  );
};

export default SystemIntegrations;
