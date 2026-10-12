import React, { useMemo, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Badge,
  Button,
  Descriptions,
  Divider,
  Input,
  Modal,
  Popconfirm,
  Row,
  Col,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
  Upload,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import type { UploadProps } from 'antd';
import {
  ApiOutlined,
  CloudServerOutlined,
  ClusterOutlined,
  InboxOutlined,
  KeyOutlined,
  PlusOutlined,
  ReloadOutlined,
  SafetyCertificateOutlined,
  WarningOutlined,
} from '@ant-design/icons';
import { PageHeader, SectionCard, StatCard } from '@/components/ui';
import { ApiError, clusterApi } from '@/api';
import { DisabledTip } from '@/components/DisabledTip';
import { hasPerm } from '@/auth';
import { useApi } from '@/hooks/useApi';
import { DASH, ago, fmt } from '@/utils/format';
import type {
  ClusterContextOption,
  ClusterParseResult,
  ClusterStatus,
  ClusterTestResult,
  K8sCluster,
} from '@/types';

const { Text } = Typography;

// ---------------------------------------------------------------------------
// 系统管理 › 集群接入
//
// 这个页面存在的理由是补上一个**本来就不存在的东西**：平台原先没有"集群"实体。
// environment.cluster 只是一段自由文本（后端 `binding:"required,max=128"`），
// 新建环境时手输一个拼错的集群名也能存进去，而没有任何代码拿它去建连接；
// 执行侧走 LocalCluster 模拟器（Ping 恒真、Nodes 按集群名哈希编数）。
// 两个直接后果：**「测试连接」永远全绿**；同一个集群在库里能有两个名字
// （种子里 canary 是 k8s-prod-01、prod 是 k8s-prod-01（双可用区））。
//
// 本页把集群变成有凭据、有探测结果、可被环境引用的实体，三处刻意的设计：
//
//   1. **「接入成功」的判据是真的拨通了一次 API**（读 /version + 列 Node）。
//      探测不过就不算接入 —— 这是唯一能让"永远全绿"不再发生的办法，
//      因为结论必须由集群回答，不能由平台自己声称。
//
//   2. **kubeconfig 只进不出**。它含 client-key / token，是管理员级凭据。
//      页面只显示"已配置"，永远不回显内容；编辑时留空 = 不修改。
//
//   3. **unprobed ≠ unreachable**。「还没探测过」与「连不上」是两件事，
//      混在一起会让运维去查一个并不存在的网络故障，所以状态里分开。
//
// 后端 `/clusters` 七条接口已落地（`internal/api/handler_cluster.go`）。
// 本页**不做演示回退**（jenkins.ts 的文件头记录过这次教训：把 404 当成"后端还没做"
// 而悄悄换成假数据，会让"其实什么都没接上"看起来像"一切正常"）。
// 接口不可用时页面照实显示错误与接口契约，**没有任何示例数据兜底**——
// 曾经有过一个「预览界面数据」开关，后端接通后已整块删除：
// 那时它是"后端没做完、让评审先看版式"的权宜，现在留着只会让人分不清
// 屏幕上的集群到底是真的接进来了、还是点了个开关看到的示意。
// ---------------------------------------------------------------------------

// --------------------------------------------------------------------------- 状态映射

type BadgeStatus = 'success' | 'error' | 'warning' | 'default' | 'processing';

const STATUS_META: Record<ClusterStatus, { text: string; badge: BadgeStatus; hint: string }> = {
  connected: {
    text: '连接正常',
    badge: 'success',
    hint: '最近一次探测从集群读到了版本与节点列表',
  },
  unreachable: {
    text: '无法访问',
    badge: 'error',
    hint: '探测时连不上 API Server：网络不通、server 地址写错，或证书不受信',
  },
  unauthorized: {
    text: '凭据被拒',
    badge: 'error',
    hint: '网络能通，但 kubeconfig 里的证书/令牌被集群拒绝（换一份凭据，或检查 RBAC 绑定）',
  },
  unprobed: {
    text: '尚未探测',
    badge: 'default',
    hint: 'kubeconfig 已保存，但还没成功探测过。这不等于"连不上"——只是还没问过它',
  },
};

/** 由 kubeconfig 里的集群名派生一个合法标识（与环境 key 同一套规则） */
const slugOf = (s: string) =>
  s
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 60) || 'cluster';

/**
 * 从 apiServer 派生一个**有区分度**的候选标识。
 *
 * 背景：公司里很多集群是统一方式（kubeadm）装的，kubeconfig 里 cluster 名**一律**是
 * `cluster.local`，context 名也一律是 `kubernetes-admin@cluster.local` —— 但它们确实是
 * 不同的机器。此时唯一能区分它们的就只有地址，所以取地址里最有辨识度的一段：
 *   - IP（`172.16.1.100`）→ 最后一段 → `152`
 *   - 域名（`k8s.prod.example.com`）→ 第一段 → `k8s`
 * 前缀沿用原标识的第一段（`cluster-local` → `cluster`），避免 `cluster-local-152`
 * 这种越撞越长的累叠。
 */
const hostKeyOf = (apiServer: string, base: string): string => {
  const host = (apiServer || '')
    .replace(/^[a-z]+:\/\//i, '')
    .split('/')[0]
    .split(':')[0]
    .trim();
  if (!host) return '';
  const isIP = /^\d{1,3}(\.\d{1,3}){3}$/.test(host);
  const tag = isIP ? host.split('.').pop() ?? '' : host.split('.')[0];
  if (!/^[a-z0-9]/i.test(tag)) return '';
  const prefix = (base.split('-')[0] || 'cluster').replace(/[^a-z0-9]/g, '');
  if (!prefix) return '';
  return `${prefix}-${tag}`
    .toLowerCase()
    .replace(/[^a-z0-9._-]/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 64);
};

/**
 * 从一组候选里挑一个**未被占用**的标识；候选都不行才加序号。
 *
 * 为什么必须有这一步：标识是平台从 kubeconfig **派生**的，不是用户主动选的 ——
 * 让他为了接一台机器去纠结命名是错的。而"统一安装的集群 cluster 名都一样"
 * 使得接完第一台之后，后面每一台都会被 `40900 集群标识已被占用` 挡在门外。
 *
 * 有意**不静默**：自动改了用户看得见的值，就必须在界面上说出来
 * （标识列的 `renamed` 提示），否则用户不知道自己填的东西被换掉了。
 */
const uniqueKeyOf = (candidates: string[], used: Set<string>): string => {
  for (const c of candidates) {
    if (c && !used.has(c)) return c;
  }
  const base = candidates.find(Boolean) || 'cluster';
  // 留出后缀位，保证总长不超过后端 64 字符上限（KEY_RE 的 `{0,63}`）
  const stem = base.slice(0, 60);
  for (let i = 2; i < 1000; i += 1) {
    const candidate = `${stem}-${i}`;
    if (!used.has(candidate)) return candidate;
  }
  return base;
};

/**
 * 集群标识规则。**必须与后端 service/cluster.go 的 clusterKeyRe 逐字一致**
 * （`^[a-z0-9][a-z0-9._-]{0,63}$`）。
 *
 * 两边不一致的两种后果都不好：前端更松 → 用户填得动、提交被拒
 * （正是"勾得动、发得出、最后失败"那类假承诺）；前端更严 → 后端明明接受的
 * 标识（如带点号的 `k8s.prod-01`）在界面上根本输不进去。
 */
const KEY_RE = /^[a-z0-9][a-z0-9._-]{0,63}$/;

/** 凭据形态的中文说法。来自后端解析 kubeconfig 的判定，不是页面猜的。 */
const CRED_KIND_LABEL: Record<string, string> = {
  token: '令牌',
  'token-file': '令牌文件',
  'client-cert': '客户端证书',
  basic: '用户名密码',
  exec: 'exec 插件',
  'auth-provider': 'auth-provider',
  none: '无凭据',
};

// =========================================================================== 页面

const ClusterAccess: React.FC = () => {
  const { message } = AntApp.useApp();
  const canWrite = hasPerm('user:manage');

  const listApi = useApi(() => clusterApi.list(), []);
  const clusters = listApi.data ?? [];

  const [testingKey, setTestingKey] = useState<string | null>(null);
  const [testResult, setTestResult] = useState<ClusterTestResult | null>(null);
  const [importOpen, setImportOpen] = useState(false);
  const [editTarget, setEditTarget] = useState<K8sCluster | null>(null);

  const apiDown = !!listApi.error;

  const summaries = useMemo(() => {
    const ok = clusters.filter((c) => c.status === 'connected').length;
    const bad = clusters.filter((c) => c.status === 'unreachable' || c.status === 'unauthorized').length;
    const unprobed = clusters.filter((c) => c.status === 'unprobed').length;
    const used = clusters.filter((c) => c.usedByEnvs.length > 0).length;
    return { total: clusters.length, ok, bad, unprobed, used };
  }, [clusters]);

  const doTest = async (c: K8sCluster) => {
    setTestingKey(c.key);
    try {
      // 探测永远返回 200：通不通看 res.ok，不能靠 try/catch 判断
      const res = await clusterApi.test(c.key);
      setTestResult(res);
      if (res.ok) {
        message.success(`探测通过（${res.latencyMs}ms）`);
        listApi.refresh();
      } else {
        message.warning('探测未通过，原因见列表下方的结果');
      }
    } catch (e) {
      message.error(e instanceof ApiError ? e.message : '探测请求失败');
    } finally {
      setTestingKey(null);
    }
  };

  const doRemove = async (c: K8sCluster) => {
    try {
      await clusterApi.remove(c.key);
      message.success(`已删除集群「${c.name}」`);
      listApi.refresh();
    } catch (e) {
      message.error(e instanceof ApiError ? e.message : '删除失败');
    }
  };

  const columns: ColumnsType<K8sCluster> = [
    {
      title: '集群',
      dataIndex: 'name',
      width: 190,
      render: (_, r) => (
        <div>
          <Space size={6}>
            <CloudServerOutlined style={{ color: '#1677ff' }} />
            <span style={{ fontWeight: 500 }}>{r.name}</span>
            {r.remark ? (
              <Text type="secondary" style={{ fontSize: 12 }}>
                {r.remark}
              </Text>
            ) : null}
          </Space>
          <div>
            <Text type="secondary" className="yn-mono" style={{ fontSize: 11.5 }}>
              {r.key}
            </Text>
          </div>
        </div>
      ),
    },
    {
      title: 'API Server',
      dataIndex: 'apiServer',
      width: 250,
      render: (v: string, r) => (
        <div>
          <Text className="yn-mono" style={{ fontSize: 12.5 }}>
            {v || DASH}
          </Text>
          <div>
            <Text type="secondary" style={{ fontSize: 11.5 }}>
              context {r.context || DASH}
              {r.kubeconfigCluster && r.kubeconfigCluster !== r.context
                ? ` · cluster ${r.kubeconfigCluster}`
                : ''}
            </Text>
          </div>
        </div>
      ),
    },
    {
      title: 'K8s 版本',
      dataIndex: 'k8sVersion',
      width: 108,
      render: (v: string) =>
        v ? (
          <Tag className="yn-mono" style={{ marginInlineEnd: 0 }}>
            {v}
          </Tag>
        ) : (
          <Text type="secondary">{DASH}</Text>
        ),
    },
    {
      title: '节点',
      width: 96,
      render: (_, r) =>
        r.nodes === null ? (
          <Text type="secondary">{DASH}</Text>
        ) : (
          <span>
            {r.nodesReady ?? 0} / {r.nodes}
            <Text type="secondary" style={{ fontSize: 11.5, marginLeft: 4 }}>
              就绪
            </Text>
          </span>
        ),
    },
    {
      title: '命名空间',
      dataIndex: 'namespaceCount',
      width: 92,
      render: (v: number | null) => (v === null ? <Text type="secondary">{DASH}</Text> : v),
    },
    {
      title: '连接状态',
      width: 120,
      render: (_, r) => {
        // 后端将来加了新状态时不能让整页崩：未知一律按"尚未探测"展示
        const m = STATUS_META[r.status] ?? STATUS_META.unprobed;
        return (
          <Tooltip title={m.hint}>
            <Badge status={m.badge} text={m.text} />
          </Tooltip>
        );
      },
    },
    {
      title: '上次探测',
      width: 158,
      render: (_, r) =>
        r.lastProbedAt ? (
          <Tooltip
            title={
              <div>
                {fmt(r.lastProbedAt)}
                <br />
                {r.lastProbeMessage || '—'}
              </div>
            }
          >
            <span>
              {ago(r.lastProbedAt)}
              <Text type="secondary" style={{ fontSize: 11.5, marginLeft: 6 }}>
                {r.lastProbeMs > 0 ? `${r.lastProbeMs}ms` : ''}
              </Text>
            </span>
          </Tooltip>
        ) : (
          <Text type="secondary">从未探测</Text>
        ),
    },
    {
      title: '被环境引用',
      width: 150,
      render: (_, r) =>
        r.usedByEnvs.length ? (
          <Space size={4} wrap>
            {r.usedByEnvs.map((k) => (
              <Tag key={k} className="yn-mono" style={{ marginInlineEnd: 0 }}>
                {k}
              </Tag>
            ))}
          </Space>
        ) : (
          <Text type="secondary">未被引用</Text>
        ),
    },
    {
      title: '操作',
      fixed: 'right',
      width: 180,
      render: (_, r) => (
        <Space size={4}>
          <Button
            type="link"
            size="small"
            icon={<ApiOutlined />}
            loading={testingKey === r.key}
            onClick={() => doTest(r)}
          >
            探测
          </Button>
          <DisabledTip title={!canWrite ? '需要 user:manage 权限' : undefined}>
            <Button
              type="link"
              size="small"
              disabled={!canWrite}
              onClick={() => setEditTarget(r)}
            >
              编辑
            </Button>
          </DisabledTip>
          <DisabledTip
            title={
              !canWrite
                ? '需要 user:manage 权限'
                : r.usedByEnvs.length
                    ? `被 ${r.usedByEnvs.length} 个环境引用（${r.usedByEnvs.join('、')}），请先在这些环境里改掉所属集群`
                    : undefined
            }
          >
            <Popconfirm
              title={`删除集群「${r.name}」？`}
              description="该集群的 kubeconfig 会一并从平台删除。集群本身的资源不受影响。"
              okText="删除"
              okButtonProps={{ danger: true }}
              cancelText="取消"
              onConfirm={() => doRemove(r)}
            >
              <Button
                type="link"
                size="small"
                danger
                disabled={!canWrite || r.usedByEnvs.length > 0}
              >
                删除
              </Button>
            </Popconfirm>
          </DisabledTip>
        </Space>
      ),
    },
  ];

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: '系统管理' }, { title: '集群接入' }]}
        title="集群接入"
        subTitle="K8s Cluster"
        description="导入 kubeconfig 把 K8s 集群接入平台：探测连通性、读取版本与节点，并作为 CD 环境的集群来源"
        extra={
          <Space>
            <Button
              icon={<ReloadOutlined />}
              onClick={() => {
                listApi.refresh();
                setTestResult(null);
                message.success('已刷新集群列表');
              }}
            >
              刷新
            </Button>
            <DisabledTip
              title={
                !canWrite
                  ? '接入集群需要 user:manage 权限（kubeconfig 属管理员级凭据）'
                  : undefined
              }
            >
              <Button
                type="primary"
                icon={<PlusOutlined />}
                disabled={!canWrite}
                onClick={() => setImportOpen(true)}
              >
                导入 kubeconfig
              </Button>
            </DisabledTip>
          </Space>
        }
      />

      {/* ---------------- 接口不可用：如实显示真实错误，并把"为什么"讲清楚 ---------------- */}
      {apiDown ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message="集群接口不可用"
          description={
            <div style={{ fontSize: 12.5, lineHeight: 1.9 }}>
              <div>
                读取集群列表失败：
                <Text code>{listApi.error?.message || '未知错误'}</Text>
              </div>
              <div style={{ marginTop: 4 }}>
                集群接口共七条：
                <Text code>GET /clusters</Text>、<Text code>POST /clusters/parse</Text>、
                <Text code>POST /clusters</Text>、<Text code>PUT /clusters/{'{key}'}</Text>、
                <Text code>DELETE /clusters/{'{key}'}</Text>、
                <Text code>POST /clusters/{'{key}'}/test</Text>、
                <Text code>GET /clusters/{'{key}'}/namespaces</Text>。
              </div>
            </div>
          }
          action={
            <Button size="small" onClick={listApi.refresh}>
              重试
            </Button>
          }
        />
      ) : null}

      <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
        <Col xs={12} xl={6}>
          <StatCard title="已接入集群" value={summaries.total} unit="个" icon={<ClusterOutlined />} />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="连接正常"
            value={summaries.ok}
            unit="个"
            accent="#52c41a"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                最近一次探测真实读到过集群信息
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="异常 / 未探测"
            value={summaries.bad + summaries.unprobed}
            unit="个"
            accent={summaries.bad ? '#ff4d4f' : '#faad14'}
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                无法访问 {summaries.bad} · 尚未探测 {summaries.unprobed}
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="已被环境引用"
            value={summaries.used}
            unit="个"
            accent="#1677ff"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                被引用的集群不允许删除
              </Text>
            }
          />
        </Col>
      </Row>

      {/* ---------------- 三处说明：凭据怎么存、为什么必须真探测、一期只读到什么程度 ---------------- */}
      <Alert
        type="info"
        showIcon
        icon={<KeyOutlined />}
        style={{ marginBottom: 16 }}
        message="kubeconfig 加密存储，页面永不回显内容"
        description={
          <div style={{ fontSize: 12.5, lineHeight: 1.9 }}>
            <div>
              kubeconfig 里含 client-key / 证书 / 令牌，属于集群管理员级凭据，因此
              与 GitLab 令牌、Jenkins API Token 同样处理：AES-256-GCM 加密落库，
              界面只显示「已配置」，没有任何读回明文的出口。编辑时留空即表示不修改。
            </div>
            <div style={{ marginTop: 4 }}>
              <Text strong>「接入成功」的唯一判据是真的拨通了一次集群 API</Text>
              （读 <Text code>/version</Text> 与节点列表）。探测不过就不会被标记为已接入——
              平台自己声称"连通"没有意义，结论必须由集群回答。
            </div>
          </div>
        }
      />

      <Alert
        type="warning"
        showIcon
        icon={<SafetyCertificateOutlined />}
        style={{ marginBottom: 16 }}
        message="集群操作：读全量，写只走变更单"
        description="可读：节点、命名空间、Pod 列表与日志。可写：发布镜像、扩缩容、滚动重启——但这三项都不直接改 Deployment，而是下发一张 AppRelease 变更单，由集群侧的 operator 落地执行。工作负载一经 operator 接管，直改 Deployment 会被秒级拉回（两边日志都正常、现场却变回去了）；对还没接管的工作负载，平台首次发布时会先建一张按当前值抄好的接管单，因此接管本身不改变任何东西。平台不会删除工作负载：下线一个服务只把副本数缩到 0 并解除纳管，K8s 里的 Deployment / Service 对象保留。探测本身是只读的。"
      />

      {testResult ? (
        <Alert
          style={{ marginBottom: 16 }}
          type={testResult.ok ? 'success' : 'error'}
          showIcon
          closable
          onClose={() => setTestResult(null)}
          message={
            <Space size={8} wrap>
              <span>
                {testResult.key} · {testResult.ok ? '探测通过' : '探测未通过'}
              </span>
              <Tag style={{ marginInlineEnd: 0 }}>{testResult.latencyMs}ms</Tag>
              {testResult.ok ? (
                <>
                  {testResult.k8sVersion ? (
                    <Tag className="yn-mono" style={{ marginInlineEnd: 0 }}>
                      {testResult.k8sVersion}
                    </Tag>
                  ) : null}
                  {testResult.nodes !== null ? (
                    <Tag style={{ marginInlineEnd: 0 }}>
                      节点 {testResult.nodesReady ?? 0}/{testResult.nodes}
                    </Tag>
                  ) : null}
                  {testResult.namespaceCount !== null ? (
                    <Tag style={{ marginInlineEnd: 0 }}>命名空间 {testResult.namespaceCount}</Tag>
                  ) : null}
                </>
              ) : null}
            </Space>
          }
          description={
            <div style={{ fontSize: 12.5, lineHeight: 1.9 }}>
              <div>{testResult.message}</div>
              {testResult.warnings?.length ? (
                <ul style={{ margin: '6px 0 0', paddingLeft: 18 }}>
                  {testResult.warnings.map((w) => (
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

      <SectionCard
        title="集群列表"
        extra={
          <Text type="secondary" style={{ fontSize: 12 }}>
            共 {clusters.length} 个集群
          </Text>
        }
      >
        <Table<K8sCluster>
          rowKey="key"
          size="middle"
          loading={listApi.loading}
          dataSource={clusters}
          columns={columns}
          scroll={{ x: 1400 }}
          locale={{
            // 接口挂了却说"还没有接入任何集群"会把故障说成正常状态，必须分开
            emptyText: listApi.loading
              ? ' '
              : apiDown
                ? '接口不可用，未能取到集群列表'
                : '还没有接入任何集群',
          }}
          pagination={false}
        />
      </SectionCard>

      {!listApi.loading && !clusters.length && !apiDown ? (
        <Alert
          type="info"
          showIcon
          style={{ marginTop: 16 }}
          message="还没有接入集群"
          description="平台的 CD 环境需要引用一个已接入的集群。点右上角「导入 kubeconfig」，粘贴或上传集群的 kubeconfig（一份可以含多个 context，能一次接入多个集群）。"
          action={
            <DisabledTip
              title={!canWrite ? '接入集群需要 user:manage 权限' : undefined}
            >
              <Button
                size="small"
                type="primary"
                disabled={!canWrite}
                onClick={() => setImportOpen(true)}
              >
                导入 kubeconfig
              </Button>
            </DisabledTip>
          }
        />
      ) : null}

      <ImportModal
        open={importOpen}
        onClose={() => setImportOpen(false)}
        onDone={() => {
          listApi.refresh();
        }}
        existing={clusters}
      />

      <EditModal
        key={editTarget?.key ?? 'none'}
        target={editTarget}
        onClose={() => setEditTarget(null)}
        onDone={() => {
          listApi.refresh();
          setEditTarget(null);
        }}
      />
    </div>
  );
};

// =========================================================================== 导入弹窗

interface ImportProps {
  open: boolean;
  onClose: () => void;
  onDone: () => void;
  /**
   * 已接入的集群。用来把**派生出来的**标识与名称避开它们（见 `uniqueKeyOf`）。
   *
   * 后端按标识判唯一，而"已接入"是按 (apiServer, context) 判的 —— 两个判据维度不同，
   * 所以前端**必须**拿真实标识来预检：否则用户会勾得动、发得出，最后在探测完之后才收到
   * 「标识已被占用」（而这一步的探测是**真连了一次集群**，白花的）。
   */
  existing: K8sCluster[];
}

const ImportModal: React.FC<ImportProps> = ({ open, onClose, onDone, existing }) => {
  const { message, notification } = AntApp.useApp();

  const [kubeconfig, setKubeconfig] = useState('');
  const [parsing, setParsing] = useState(false);
  const [parsed, setParsed] = useState<ClusterParseResult | null>(null);
  const [selected, setSelected] = useState<string[]>([]);
  const [names, setNames] = useState<Record<string, string>>({});
  const [keys, setKeys] = useState<Record<string, string>>({});
  /** 记录哪些 context 的标识**被自动改过**（原值 → 用于在界面上如实说明改了什么）。 */
  const [renamed, setRenamed] = useState<Record<string, string>>({});
  const [submitting, setSubmitting] = useState(false);

  const reset = () => {
    setKubeconfig('');
    setParsed(null);
    setSelected([]);
    setNames({});
    setKeys({});
    setRenamed({});
  };

  const uploadProps: UploadProps = {
    accept: '.yaml,.yml,.config,.conf,.txt',
    maxCount: 1,
    showUploadList: false,
    beforeUpload: async (file) => {
      const text = await file.text();
      setKubeconfig(text);
      setParsed(null);
      setSelected([]);
      message.success(`已读取 ${file.name}（${text.length} 字符），点「解析」查看其中的 context`);
      // 返回 false 阻止上传：kubeconfig 只在浏览器里解析，不需要经由服务端落盘
      return false;
    },
  };

  const doParse = async () => {
    if (!kubeconfig.trim()) {
      message.warning('请先粘贴 kubeconfig 内容，或上传文件');
      return;
    }
    setParsing(true);
    setParsed(null);
    try {
      const res = await clusterApi.parse(kubeconfig);
      setParsed(res);
      if (!res.ok) {
        // 失败原因由弹窗里的内联面板列出（可留存、可逐条读，见下方 `{!parsed.ok}` 分支）。
        // 这里刻意**不再弹 toast** —— 同一句话弹一次还会被下一次盖掉，
        // 而这个面板本来就是为了"让用户能对着原因改文件"。
        setSelected([]);
        return;
      }
      // 不能接入的项要一并排除，而不是让用户勾了再在探测阶段收到报错：
      // problem 非空说明后端已判定这个 context 用不了（引用的集群不存在、
      // 只有证书没有私钥…），勾得动就是假承诺。
      const importable = res.contexts.filter((c) => !c.alreadyImported && !c.problem);
      // 默认勾选 current-context（最符合"我要接的就是现在 kubectl 在用的那个"）；
      // 它已被接入过、或不存在时，退而勾选全部未接入项。
      const def = importable.some((c) => c.context === res.currentContext)
        ? [res.currentContext]
        : importable.map((c) => c.context);
      setSelected(def);
      const n: Record<string, string> = {};
      const k: Record<string, string> = {};
      const re: Record<string, string> = {};
      // 已接入的标识/名称 + 本批已分配的，一起进占用集合：
      // 既避开已有集群，也避开"本次导入里两个 context 派生出同一个标识"——
      // 后者原先要等用户点提交、被 duplicateKey 拦下才知道。
      // 所有 context 都占名额（含没勾选的），这样用户后续补勾也不会撞。
      const used = new Set(existing.map((c) => c.key));
      const usedNames = new Set(existing.map((c) => c.name).filter(Boolean));
      res.contexts.forEach((c) => {
        // 名称默认值也要去重：统一安装的集群 cluster 名都叫 `cluster.local`，
        // 接进来后列表里会出现两个一模一样的名字，而**用户看的是名称**，
        // 标识唯一并不能帮他在列表里认出谁是谁。
        const baseName = c.cluster || c.context;
        let nm = baseName;
        for (let i = 2; usedNames.has(nm); i += 1) nm = `${baseName} (${i})`;
        usedNames.add(nm);
        n[c.context] = nm;

        const base = slugOf(baseName);
        // 首选 cluster 名派生；撞了就改用机器地址派生（统一安装的集群 cluster 名全一样，
        // 只有地址能把它们分开）。两者都撞才落到序号。
        const assigned = uniqueKeyOf([base, hostKeyOf(c.apiServer, base)], used);
        used.add(assigned);
        k[c.context] = assigned;
        if (assigned !== base) re[c.context] = base;
      });
      setNames(n);
      setKeys(k);
      setRenamed(re);
    } catch (e) {
      message.error(e instanceof ApiError ? e.message : '解析请求失败');
    } finally {
      setParsing(false);
    }
  };

  const keyInvalid = () =>
    selected.some((ctx) => !KEY_RE.test((keys[ctx] ?? '').trim()));

  const duplicateKey = () => {
    const used = selected.map((ctx) => (keys[ctx] ?? '').trim());
    return new Set(used).size !== used.length;
  };

  /** 与**已接入集群**撞标识的项（用户手改回去就会撞）。 */
  const keyTaken = () => {
    const taken = new Set(existing.map((c) => c.key));
    return selected.map((ctx) => (keys[ctx] ?? '').trim()).filter((k) => taken.has(k));
  };

  const submit = async () => {
    const picked = (parsed?.contexts ?? []).filter((c) => selected.includes(c.context));
    if (!picked.length) {
      message.warning('请至少勾选一个 context');
      return;
    }
    if (keyInvalid()) {
      message.warning('集群标识不合法：小写字母开头，仅含小写字母、数字与中划线');
      return;
    }
    if (duplicateKey()) {
      message.warning('多个 context 的集群标识重复了，请改掉其中一个');
      return;
    }
    const clash = keyTaken();
    if (clash.length) {
      message.warning(`集群标识 ${clash.join('、')} 已被占用，请换一个，或先删除原集群再接入`);
      return;
    }
    setSubmitting(true);
    try {
      const res = await clusterApi.create(
        picked.map((c) => ({
          key: (keys[c.context] ?? '').trim(),
          name: (names[c.context] ?? '').trim() || c.cluster || c.context,
          context: c.context,
          kubeconfig,
        })),
      );
      const okCount = res.clusters?.length ?? 0;
      const fails = res.errors ?? [];
      if (fails.length) {
        // 部分成功必须逐条说清：否则用户看到"少了一个"却不知道为什么
        notification.warning({
          message: `已接入 ${okCount} 个集群，${fails.length} 个探测未通过`,
          description: (
            <ul style={{ margin: 0, paddingLeft: 18 }}>
              {fails.map((f) => (
                <li key={f.context}>
                  <Text code>{f.context}</Text>：{f.message}
                </li>
              ))}
            </ul>
          ),
        });
      } else {
        notification.success({
          message: `已接入 ${okCount} 个集群`,
          description: '接入时已逐个探测通过，版本与节点数已回填。',
        });
      }
      reset();
      onDone();
      onClose();
    } catch (e) {
      message.error(e instanceof ApiError ? e.message : '接入失败');
    } finally {
      setSubmitting(false);
    }
  };

  const ctxColumns: ColumnsType<ClusterContextOption> = [
    {
      title: 'context',
      dataIndex: 'context',
      width: 150,
      render: (v: string, r) => (
        <Space size={6} wrap>
          <Text className="yn-mono" style={{ fontSize: 12.5 }}>
            {v}
          </Text>
          {r.context === parsed?.currentContext ? <Tag color="blue">当前</Tag> : null}
          {r.alreadyImported ? <Tag>已接入</Tag> : null}
        </Space>
      ),
    },
    {
      title: 'API Server',
      dataIndex: 'apiServer',
      width: 210,
      ellipsis: true,
      render: (v: string) => (
        <Text className="yn-mono" style={{ fontSize: 12 }}>
          {v}
        </Text>
      ),
    },
    {
      title: '凭据',
      dataIndex: 'credentialKind',
      width: 130,
      render: (v: string, r) =>
        r.problem ? (
          // 后端已经判定这个 context 用不了，原因要原样给出——
          // 否则用户只会看到"被禁用了"，不知道要改 kubeconfig 的哪一段。
          <Tooltip title={r.problem}>
            <Tag color="error">不可接入</Tag>
          </Tooltip>
        ) : (
          <Tag>{CRED_KIND_LABEL[v] ?? v}</Tag>
        ),
    },
    {
      title: '默认命名空间',
      dataIndex: 'namespace',
      width: 110,
      render: (v: string) => v || <Text type="secondary">{DASH}</Text>,
    },
    {
      title: '集群名称',
      width: 180,
      render: (_, r) => (
        <Input
          size="small"
          value={names[r.context] ?? ''}
          placeholder="如：测试集群"
          onChange={(e) => setNames((m) => ({ ...m, [r.context]: e.target.value }))}
        />
      ),
    },
    {
      title: '集群标识',
      width: 190,
      render: (_, r) => (
        <div>
          <Input
            size="small"
            className="yn-mono"
            value={keys[r.context] ?? ''}
            placeholder="k8s-test-01"
            status={KEY_RE.test((keys[r.context] ?? '').trim()) ? undefined : 'error'}
            onChange={(e) => setKeys((m) => ({ ...m, [r.context]: e.target.value }))}
          />
          {renamed[r.context] ? (
            // 自动改了用户看到的值，就必须**说出来**——静默改比报错更让人困惑
            <Text style={{ fontSize: 11.5, color: '#d46b08' }}>
              {renamed[r.context]} 已被占用 → 自动改为 {keys[r.context]}
            </Text>
          ) : (
            <Text type="secondary" style={{ fontSize: 11.5 }}>
              环境将引用这个标识
            </Text>
          )}
        </div>
      ),
    },
  ];

  return (
    <Modal
      open={open}
      title={
        <Space size={8}>
          <KeyOutlined style={{ color: '#1677ff' }} />
          导入 kubeconfig
        </Space>
      }
      width={880}
      okText="接入并探测"
      cancelText="取消"
      confirmLoading={submitting}
      okButtonProps={{ disabled: !parsed?.ok || !selected.length }}
      onCancel={() => {
        reset();
        onClose();
      }}
      onOk={submit}
    >
      <Alert
        type="info"
        showIcon
        style={{ marginBottom: 14 }}
        message="一份 kubeconfig 可以含多个集群"
        description="先解析，再勾选要接入的 context。接入时平台会逐个真探测（读版本 + 列节点），探测不过的不会被标记为已接入。"
      />

      <Row gutter={16}>
        <Col span={14}>
          <div style={{ marginBottom: 6 }}>
            <Text strong style={{ fontSize: 13 }}>
              粘贴 kubeconfig 内容
            </Text>
            <Text type="secondary" style={{ fontSize: 12, marginLeft: 8 }}>
              内容只在浏览器与平台之间传递，界面不会回显
            </Text>
          </div>
          <Input.TextArea
            rows={10}
            className="yn-mono"
            style={{ fontSize: 12 }}
            placeholder={'apiVersion: v1\nkind: Config\nclusters:\n- cluster:\n    server: https://10.0.0.1:6443\n    certificate-authority-data: ****\n  name: prod\ncontexts:\n- context:\n    cluster: prod\n    user: admin\n  name: yunnian-prod\ncurrent-context: yunnian-prod'}
            value={kubeconfig}
            onChange={(e) => {
              setKubeconfig(e.target.value);
              setParsed(null);
              setSelected([]);
            }}
          />
        </Col>
        <Col span={10}>
          <div style={{ marginBottom: 6 }}>
            <Text strong style={{ fontSize: 13 }}>
              或上传文件
            </Text>
          </div>
          <Upload.Dragger {...uploadProps} style={{ padding: '4px 0' }}>
            <p className="ant-upload-drag-icon" style={{ marginBottom: 6 }}>
              <InboxOutlined />
            </p>
            <p style={{ fontSize: 13, marginBottom: 2 }}>点击或拖拽 kubeconfig 文件</p>
            <p className="ant-upload-hint" style={{ fontSize: 12 }}>
              支持 .yaml / .yml / .config / .conf
            </p>
          </Upload.Dragger>
          <div style={{ marginTop: 12 }}>
            <Space size={8}>
              <Button type="primary" size="small" loading={parsing} onClick={doParse}>
                解析
              </Button>
              <Text type="secondary" style={{ fontSize: 12 }}>
                {kubeconfig.trim() ? `已载入 ${kubeconfig.length} 字符` : '尚未载入内容'}
              </Text>
            </Space>
          </div>
        </Col>
      </Row>

      {parsed ? (
        <>
          <Divider orientation="left" orientationMargin={0} style={{ margin: '16px 0 10px' }}>
            <span style={{ fontSize: 12.5 }}>
              解析结果
              {parsed.ok ? (
                <Text type="secondary" style={{ fontSize: 11.5, marginLeft: 8 }}>
                  共 {parsed.contexts.length} 个 context，已选 {selected.length} 个
                </Text>
              ) : null}
            </span>
          </Divider>
          {!parsed.ok ? (
            <Alert
              type="error"
              showIcon
              message="无法解析这份 kubeconfig"
              description={
                <ul style={{ margin: 0, paddingLeft: 18 }}>
                  {(parsed.errors?.length ? parsed.errors : ['内容不是合法的 kubeconfig']).map((e) => (
                    <li key={e}>{e}</li>
                  ))}
                </ul>
              }
            />
          ) : (
            <Table<ClusterContextOption>
              rowKey="context"
              size="small"
              dataSource={parsed.contexts}
              columns={ctxColumns}
              pagination={false}
              scroll={{ x: 840 }}
              rowSelection={{
                selectedRowKeys: selected,
                onChange: (keysSel) => setSelected(keysSel as string[]),
                // 已接入的 context 不允许重复勾选：再接入一次只会多出一条同名集群，
                // 而环境引用的是 key，重复项只会让"到底该引哪个"变含糊。
                // 与上面的 importable 过滤同一口径：problem 非空 = 后端已判定接入不了，
                // 页面默认不勾选它，也不允许手动勾上（否则会提交一个必然失败的 context）。
                getCheckboxProps: (r) => ({ disabled: r.alreadyImported || !!r.problem }),
              }}
            />
          )}
        </>
      ) : null}
    </Modal>
  );
};

// =========================================================================== 编辑弹窗

interface EditProps {
  target: K8sCluster | null;
  onClose: () => void;
  onDone: () => void;
}

const EditModal: React.FC<EditProps> = ({ target, onClose, onDone }) => {
  const { message } = AntApp.useApp();
  // 父组件用 key={target.key} 让本弹窗在换目标时重挂载，所以这里直接用 props 初始化。
  // 不要写"在 render 里对齐 target"的写法——那会在渲染期间 setState。
  const [name, setName] = useState(target?.name ?? '');
  const [remark, setRemark] = useState(target?.remark ?? '');
  const [kubeconfig, setKubeconfig] = useState('');
  const [saving, setSaving] = useState(false);

  const save = async () => {
    if (!target) return;
    setSaving(true);
    try {
      await clusterApi.update(target.key, {
        name: name.trim() || target.name,
        context: target.context,
        remark: remark.trim(),
        // 留空 = 不修改凭据。凭据没有读出口，所以"不改"只能靠不传表达
        ...(kubeconfig.trim() ? { kubeconfig } : {}),
      });
      message.success('集群已更新');
      setKubeconfig('');
      onDone();
    } catch (e) {
      message.error(e instanceof ApiError ? e.message : '保存失败');
    } finally {
      setSaving(false);
    }
  };

  return (
    <Modal
      open={!!target}
      title={`编辑集群 · ${target?.name ?? ''}`}
      width={620}
      okText="保存"
      cancelText="取消"
      confirmLoading={saving}
      onCancel={() => {
        onClose();
      }}
      onOk={save}
    >
      <Descriptions column={1} size="small" colon={false} style={{ marginBottom: 12 }}>
        <Descriptions.Item label="集群标识">
          <Text className="yn-mono">{target?.key}</Text>
          <Text type="secondary" style={{ fontSize: 11.5, marginLeft: 8 }}>
            环境引用的是这个标识，不随名称改动
          </Text>
        </Descriptions.Item>
        <Descriptions.Item label="API Server">
          <Text className="yn-mono">{target?.apiServer || DASH}</Text>
        </Descriptions.Item>
        <Descriptions.Item label="context">
          <Text className="yn-mono">{target?.context || DASH}</Text>
        </Descriptions.Item>
      </Descriptions>

      <div style={{ marginBottom: 6 }}>
        <Text strong style={{ fontSize: 13 }}>
          集群名称
        </Text>
      </div>
      <Input
        size="small"
        value={name}
        placeholder="如：测试集群"
        onChange={(e) => setName(e.target.value)}
      />

      <div style={{ margin: '12px 0 6px' }}>
        <Text strong style={{ fontSize: 13 }}>
          备注
        </Text>
      </div>
      <Input
        size="small"
        value={remark}
        placeholder="如：双可用区 / 用途"
        onChange={(e) => setRemark(e.target.value)}
      />

      <Divider orientation="left" orientationMargin={0} style={{ margin: '16px 0 10px' }}>
        <span style={{ fontSize: 12.5 }}>更换 kubeconfig（可选）</span>
      </Divider>
      <Input.TextArea
        rows={5}
        className="yn-mono"
        style={{ fontSize: 12 }}
        placeholder="留空表示保持现有凭据不变。凭据不会回显，所以这里永远是空的。"
        value={kubeconfig}
        onChange={(e) => setKubeconfig(e.target.value)}
      />
      {kubeconfig.trim() ? (
        <Alert
          type="warning"
          showIcon
          style={{ marginTop: 10 }}
          message="保存后将用这份新凭据重新探测"
          description="探测不过会保留原凭据与状态，不会把一个连不上的集群写成「已接入」。"
        />
      ) : null}
    </Modal>
  );
};

export default ClusterAccess;
