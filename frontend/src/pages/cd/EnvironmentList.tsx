import React, { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Badge,
  Button,
  Col,
  Divider,
  Input,
  Row,
  Segmented,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  AppstoreOutlined,
  CloudServerOutlined,
  ExportOutlined,
  PlusOutlined,
  ReloadOutlined,
  SafetyCertificateOutlined,
  SyncOutlined,
  UnorderedListOutlined,
} from '@ant-design/icons';
import { Link, useNavigate } from 'react-router-dom';
import { PageHeader, SectionCard, StatCard } from '@/components/ui';
import { UsageBar } from '@/components/charts';
import { EnvStatusBadge } from '@/components/StatusTag';
import { DisabledTip } from '@/components/DisabledTip';
import { EnvironmentActions } from '@/components/EnvironmentActions';
import { EnvironmentEditDrawer } from '@/components/EnvironmentEditDrawer';
import { environmentApi } from '@/api';
import type { EnvResources } from '@/api/cd';
import { OWN_CLUSTER_BADGE, OWN_CLUSTER_HINT, isOwnCluster } from '@/config/topology';
import { hasPerm } from '@/auth';
import { useApi } from '@/hooks/useApi';
import { envResStatusOf, envUsageOf, useEnvResources } from '@/hooks/useEnvResources';
import { isServiceAbnormal, liveWordOf } from '@/utils/live';
import type { Environment, EnvService } from '@/types';

const { Text } = Typography;

const EnvironmentList: React.FC = () => {
  const navigate = useNavigate();
  const { message } = AntApp.useApp();
  const canWrite = hasPerm('env:write');

  const [view, setView] = useState<'card' | 'table'>('card');
  const [keywordInput, setKeywordInput] = useState('');
  const [keyword, setKeyword] = useState('');
  const [syncing, setSyncing] = useState(false);

  // keyword 落成请求要防抖：这个页面一次拿全量环境（数量少），
  // 但每敲一个字仍然会打一次接口，没有防抖会白刷十几次。
  useEffect(() => {
    const t = setTimeout(() => setKeyword(keywordInput.trim()), 300);
    return () => clearTimeout(t);
  }, [keywordInput]);

  // ------------------------------------------------------------------ 取数
  const envApi = useApi(() => environmentApi.list({ keyword, pageSize: 100 }), [keyword]);
  const envs = useMemo(() => envApi.data?.list ?? [], [envApi.data]);

  // 集群候选、用户候选、变量回填都在 `EnvironmentEditDrawer` 里 —— 不再各自算一份。
  // 本页只负责把"现有环境用过的集群标识"递过去，供抽屉补存量候选
  // （历史手输的值不在集群表里，不列出来编辑时连当前所属集群都看不到）。
  const clusterValuesInUse = useMemo(() => envs.map((e) => e.cluster).filter(Boolean), [envs]);

  // 卡片上要显示"该项目下有几个服务 / 几个待关注"，环境接口本身不带这个聚合值。
  // 环境数量是个位数，每个环境打一次 /services 比让后端为列表页加一个聚合字段更划算，
  // 也避免为原型改接口契约。keys 用 join 当依赖，避免数组引用变化导致重复请求。
  const envKeys = useMemo(() => envs.map((e) => e.key).sort().join(','), [envs]);
  const servicesApi = useApi(
    async () => {
      const keys = envKeys ? envKeys.split(',') : [];
      const groups = await Promise.all(keys.map((k) => environmentApi.services(k)));
      return groups.flat();
    },
    [envKeys],
    { enabled: !!envKeys },
  );
  const allServices = useMemo(() => servicesApi.data ?? [], [servicesApi.data]);
  const servicesOf = (envKey: string): EnvService[] => allServices.filter((s) => s.envKey === envKey);

  /**
   * 删除确认弹窗用的实例数：**读不到就给 `null`**（不预判），不拿 0 充数。
   *
   * 后端 `DeleteEnvironment` 在环境内仍有服务实例时直接 40901 拒绝，所以这个数只用来
   * "把能不能删提前讲清楚"（锁住确认按钮）；判错了代价是用户点下去才吃一个 409，
   * 或者明明能删却被拦住。服务列表没加载完/失败时只能退回确定性文案。
   */
  const svcCountOf = (envKey: string): number | null =>
    servicesApi.error || servicesApi.loading ? null : servicesOf(envKey).length;

  /**
   * 卡片 / 表格上的 CPU、内存与「资源状态」**一律读集群实时值**。
   *
   * 原先用的是环境快照（`environment.cpu_usage / mem_usage`）：它由「同步集群」写入，
   * 而列表页没有任何定时刷新 —— 于是卡片把几小时前的旧数当"现在"显示。
   * 实测同一个环境：卡片内存 96%，点进详情页实时是 93%（CPU 6% / 8%）。
   * 两个数**都没算错** —— 后端两次调用走的是同一段 `Nodes()`，差的只有时间点。
   * 真正的问题是把快照当现状展示，且一个字的时间标识都没有。
   *
   * 取数与阈值推导都在 `useEnvResources` 里（概览页、详情页共用同一份口径）。
   */
  const envRes = useEnvResources(envs.map((e) => e.key));
  const liveResOf = (envKey: string): EnvResources | null => envRes.map?.get(envKey) ?? null;
  const usageOf = (envKey: string): EnvResources | null => envUsageOf(envRes.map, envKey);
  const resStatusOf = (envKey: string): Environment['status'] => envResStatusOf(envRes.map, envKey);

  const refreshAll = () => {
    envApi.refresh();
    servicesApi.refresh();
    // 必须跟着刷：卡片上的 CPU/内存/资源状态现在取的是实时值，
    // 不刷它就会出现"点了同步集群、徽标却还是旧的"。
    envRes.refresh();
  };

  // ------------------------------------------------------------------ 统计
  const stats = useMemo(() => {
    const healthy = envs.filter((e) => resStatusOf(e.key) === 'healthy').length;
    const warning = envs.filter((e) => resStatusOf(e.key) === 'warning').length;
    const error = envs.filter((e) => resStatusOf(e.key) === 'error').length;
    const unknown = envs.filter((e) => resStatusOf(e.key) === 'unknown').length;
    // 「待关注服务」只认**集群实时状态**，与「环境详情」同口径。
    //
    // 原先数的是平台库的 `service_instance.status` —— 那是纳管时按默认值写入的期望，
    // 之后只有部署成功才更新。实测同一个环境：平台记 `status=pending`、`image` 还是空串，
    // 集群里其实 running 1/1 —— 于是卡片对着一个健康服务报了"1 个待关注"，
    // 而用户点进详情页看到的是"运行正常 3 个"。一句假警报，比不显示更糟。
    const abnormal = allServices.filter(isServiceAbnormal).length;
    const svcUnknown = allServices.filter((s) => liveWordOf(s) === 'unknown').length;
    return {
      total: envs.length,
      healthy,
      warning,
      error,
      unknown,
      services: allServices.length,
      abnormal,
      svcUnknown,
    };
    // resStatusOf / svcWordOf 每次渲染都是新函数，依赖用它们真正读到的两份数据。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [envs, allServices, envRes.map]);

  // ------------------------------------------------------------------ 同步集群
  // 后端是「按环境」同步（POST /environments/{key}/sync），列表页的"同步集群"
  // 就是把它跑一遍。串行而不是 Promise.all：sync 会去改库、可能触发集群调用，
  // 一串并发打过去对真实集群不友好，而且失败时看不出是哪个环境挂了。
  const syncAll = async () => {
    if (!envs.length) return;
    setSyncing(true);
    const failed: string[] = [];
    for (const e of envs) {
      try {
        await environmentApi.sync(e.key);
      } catch {
        failed.push(e.name);
      }
    }
    setSyncing(false);
    refreshAll();
    if (failed.length) message.warning(`${failed.join('、')} 同步失败，其余环境已更新`);
    else message.success(`已同步 ${envs.length} 个环境的集群状态`);
  };

  // ------------------------------------------------------------------ 导出
  // 原型期的"导出"按钮之前是纯摆设。这里改成真实的 CSV 下载：数据已经在手上，
  // 不值得为它加一个后端接口。
  const exportCsv = () => {
    // 「部署方式」列去掉了：平台改集群只有一条路（写 AppRelease，由 operator 下发），
    // 这一列恒为同一个值 —— 一个恒常列在导出的表里也一样没有信息量。
    const head = ['环境标识', '环境名称', 'K8s集群', 'Namespace', '项目数', '服务数', 'CPU%', '内存%', '负责人', '状态', '启用'];
    const rows = envs.map((e) => [
      e.key,
      e.name,
      e.cluster,
      e.namespace,
      String(e.projectKeys.length),
      String(servicesOf(e.key).length),
      // 导出与页面上是同一份数：实时读到的写数字，读不到留空。
      // 填快照值（或填 0）都会让拿到表格的人以为那就是当时的集群水位。
      usageOf(e.key) ? String(usageOf(e.key)!.cpuUsage) : '',
      usageOf(e.key) ? String(usageOf(e.key)!.memUsage) : '',
      e.owner,
      resStatusOf(e.key),
      e.enabled ? '启用' : '停用',
    ]);
    const csv = [head, ...rows].map((r) => r.map((c) => `"${String(c).replace(/"/g, '""')}"`).join(',')).join('\n');
    const url = URL.createObjectURL(new Blob([`\uFEFF${csv}`], { type: 'text/csv;charset=utf-8' }));
    const a = document.createElement('a');
    a.href = url;
    a.download = `yunnian-environments-${new Date().toISOString().slice(0, 10)}.csv`;
    a.click();
    URL.revokeObjectURL(url);
    message.success(`已导出 ${envs.length} 条环境记录`);
  };

  // ------------------------------------------------------------------ 抽屉
  // 表单、集群/用户候选、变量回填、提交口径全在 `EnvironmentEditDrawer` 里；
  // 本页只管"开给谁" —— 这里只 set，不做任何 setFieldsValue（漏填的后果不是空的，
  // 是"看着正常、一保存就把某字段清掉"）。
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [editTarget, setEditTarget] = useState<Environment | null>(null);

  const openCreate = () => {
    setEditTarget(null);
    setDrawerOpen(true);
  };

  const openEdit = (env: Environment) => {
    setEditTarget(env);
    setDrawerOpen(true);
  };

  // 变量取回后补进表单（不能放在 openEdit 里——那边拿不到异步结果）
  // ------------------------------------------------------------------ 表格列
  const columns: ColumnsType<Environment> = [
    {
      title: '环境名称',
      dataIndex: 'name',
      width: 200,
      fixed: 'left',
      render: (v, r) => (
        <div>
          <Link to={`/cd/environments/${r.key}`} style={{ fontWeight: 500 }}>
            {v}
          </Link>
          <div className="yn-mono" style={{ fontSize: 12, color: '#9aa0aa', marginTop: 2 }}>
            {r.key} · {r.namespace}
          </div>
        </div>
      ),
    },
    // 标题**不能叫「健康状态」**：这一列的取值只由该环境所在集群的**节点 CPU / 内存水位**
    // 决定（后端 `cd.go` 的 SyncEnvironment：≥90 → error、≥75 → warning、读不到 → unknown），
    // 与服务健康无关。叫「健康状态」会让人以为"这个环境出故障了"——
    // 与上一轮改掉的 ENV_MAP 文案（异常 → 资源紧张）是同一处口径。
    // 取 `resStatusOf`（实时）而不是 `r.status`（上次同步的快照）：同一个环境，
    // 快照可能在几小时前就写下了"资源紧张"，而集群现在早已回落。
    { title: '资源状态', key: 'resStatus', width: 108, render: (_, r) => <EnvStatusBadge status={resStatusOf(r.key)} /> },
    {
      title: 'K8s 集群 / Namespace',
      dataIndex: 'cluster',
      width: 260,
      render: (v, r) => (
        <div>
          <div>
            {v}
            {/* 自带凭据时 `cluster` 只是个标签：后端会绕过平台集群表、直接用环境那份
                凭据，注册表里同名的记录**是另一台机器**。不标出来，两个不同的集群
                会以同一个名字并列在列表里（实测：test-152 与 test 都叫 cluster-local，
                实际一个是 v1.23.10、一个是 v1.22.12）。 */}
            {isOwnCluster(r) ? (
              <Tooltip title={OWN_CLUSTER_HINT}>
                <Tag style={{ marginInlineStart: 6, marginInlineEnd: 0 }}>{OWN_CLUSTER_BADGE}</Tag>
              </Tooltip>
            ) : null}
          </div>
          <div className="yn-mono" style={{ fontSize: 12, color: '#9aa0aa', marginTop: 2 }}>
            {r.namespace} · {r.clusterVersion}
          </div>
        </div>
      ),
    },
    {
      title: '服务 / 项目',
      key: 'svc',
      width: 130,
      render: (_, r) => (
        <span>
          <b>{r.projectKeys.length}</b> 个项目 · {servicesOf(r.key).length} 个服务
        </span>
      ),
    },
    {
      title: '资源用量',
      key: 'usage',
      width: 240,
      render: (_, r) => {
        // 与卡片同一份**实时**数据（原先这里是环境快照，会和点进去看到的对不上）。
        const u = usageOf(r.key);
        if (!u) {
          // 读不到就不画条、也不填 0：一条 0% 的灰条会被读成"这份集群很闲"。
          return (
            <span style={{ fontSize: 12, color: '#9aa0aa' }}>
              {envRes.error
                ? '读不到集群用量'
                : envRes.loading
                  ? '正在读取…'
                  : '集群未提供用量指标'}
            </span>
          );
        }
        return (
          <Space direction="vertical" size={4} style={{ width: '100%' }}>
            <div style={{ fontSize: 12, color: '#5c6470' }}>
              CPU {u.cpuUsage}% · 内存 {u.memUsage}%
            </div>
            <div style={{ display: 'flex', gap: 4 }}>
              <div style={{ flex: 1, height: 5, borderRadius: 3, background: '#f0f2f5', overflow: 'hidden' }}>
                <div style={{ width: `${u.cpuUsage}%`, height: '100%', background: u.cpuUsage > 85 ? '#ff4d4f' : '#1677ff' }} />
              </div>
              <div style={{ flex: 1, height: 5, borderRadius: 3, background: '#f0f2f5', overflow: 'hidden' }}>
                <div style={{ width: `${u.memUsage}%`, height: '100%', background: u.memUsage > 85 ? '#ff4d4f' : '#722ed1' }} />
              </div>
            </div>
          </Space>
        );
      },
    },
    {
      title: '审批',
      dataIndex: 'needApproval',
      width: 130,
      render: (v: boolean, r) =>
        v ? (
          <Tooltip title={`审批人：${r.approvers.join('、') || '未指定'}`}>
            <Tag color="orange" icon={<SafetyCertificateOutlined />} style={{ marginInlineEnd: 0 }}>
              需审批
            </Tag>
          </Tooltip>
        ) : (
          <Tag style={{ marginInlineEnd: 0 }}>免审批</Tag>
        ),
    },
    { title: '负责人', dataIndex: 'owner', width: 96 },
    { title: '状态', dataIndex: 'enabled', width: 92, render: (v: boolean) => <Badge status={v ? 'success' : 'default'} text={v ? '启用' : '停用'} /> },
    {
      title: '操作',
      key: 'action',
      fixed: 'right',
      width: 190,
      render: (_, r) => (
        <EnvironmentActions
          env={r}
          svcCount={svcCountOf(r.key)}
          withEnter
          onEdit={openEdit}
          onRemoved={refreshAll}
        />
      ),
    },
  ];

  const pageError = envApi.error || servicesApi.error;

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: 'CD 环境部署' }, { title: '环境管理' }]}
        title="环境管理"
        description="维护测试 / 预发布 / 灰度 / 正式等部署环境，配置集群与 Namespace"
        extra={
          <Space>
            <Segmented
              value={view}
              onChange={(v) => setView(v as 'card' | 'table')}
              options={[
                { value: 'card', icon: <AppstoreOutlined /> },
                { value: 'table', icon: <UnorderedListOutlined /> },
              ]}
            />
            <Button icon={<ExportOutlined />} onClick={exportCsv} disabled={!envs.length}>
              导出
            </Button>
            <DisabledTip title={!canWrite ? '当前账号没有 env:write 权限' : envs.length ? undefined : '暂无可同步的环境'}>
              <Button icon={<SyncOutlined />} onClick={syncAll} loading={syncing} disabled={!canWrite || !envs.length}>
                同步集群
              </Button>
            </DisabledTip>
            <DisabledTip title={canWrite ? undefined : '当前账号没有 env:write 权限'}>
              <Button type="primary" icon={<PlusOutlined />} onClick={openCreate} disabled={!canWrite}>
                新建环境
              </Button>
            </DisabledTip>
          </Space>
        }
      />

      {pageError ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message="环境数据加载失败"
          description={pageError.message}
          action={
            <Button size="small" onClick={refreshAll}>
              重试
            </Button>
          }
        />
      ) : null}

      <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
        <Col xs={12} xl={6}>
          <StatCard title="环境总数" value={stats.total} unit="个" icon={<CloudServerOutlined />} />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="资源正常环境"
            value={`${stats.healthy}/${stats.total}`}
            accent="#52c41a"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                资源告警 {stats.warning} · 资源紧张 {stats.error}
                {stats.unknown ? ` · ${stats.unknown} 个读取不到` : ''}
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="纳管服务"
            value={stats.services}
            unit="个"
            accent="#722ed1"
            footer={<Text type="secondary" style={{ fontSize: 12 }}>跨 {stats.total} 个环境的部署实例</Text>}
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="待关注服务"
            value={stats.abnormal}
            unit="个"
            accent="#ff4d4f"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                异常 / 部署中 / 已停止
                {stats.svcUnknown ? ` · ${stats.svcUnknown} 个状态读取不到` : ''}
              </Text>
            }
          />
        </Col>
      </Row>

      <div style={{ marginBottom: 12 }}>
        <Input.Search
          allowClear
          placeholder="搜索环境名称 / 标识 / Namespace / 集群"
          style={{ width: 340 }}
          value={keywordInput}
          onChange={(e) => setKeywordInput(e.target.value)}
        />
      </div>

      {!envs.length && !envApi.loading && !pageError ? (
        <SectionCard>
          <div style={{ padding: '32px 0', textAlign: 'center', color: '#9aa0aa' }}>
            {keyword ? `没有匹配「${keyword}」的环境` : '还没有环境，点右上角「新建环境」开始接入集群'}
          </div>
        </SectionCard>
      ) : view === 'card' ? (
        <Row gutter={[16, 16]}>
          {envs.map((env) => {
            const svcs = servicesOf(env.key);
            // 判据全部来自**集群实时状态**（`live`），不是平台库里的 `service_instance.status`。
            const bad = svcs.filter(isServiceAbnormal);
            const svcUnknown = svcs.filter((s) => liveWordOf(s) === 'unknown').length;
            const r = liveResOf(env.key);
            const usage = usageOf(env.key);
            return (
              <Col key={env.key} xs={24} md={12} xl={8}>
                <div className="yn-env-card" onClick={() => navigate(`/cd/environments/${env.key}`)}>
                  <div className="yn-env-card__head">
                    <div>
                      <div className="yn-env-card__name">
                        {env.name}
                        {env.needApproval ? (
                          <Tooltip title={`审批人：${env.approvers.join('、') || '未指定'}`}>
                            <Tag color="orange" icon={<SafetyCertificateOutlined />} style={{ marginInlineEnd: 0 }}>
                              需审批
                            </Tag>
                          </Tooltip>
                        ) : null}
                        {!env.enabled ? <Tag style={{ marginInlineEnd: 0 }}>已停用</Tag> : null}
                      </div>
                      <div className="yn-env-card__key">
                        <span className="yn-mono">{env.key}</span>
                        <Divider type="vertical" style={{ margin: '0 6px' }} />
                        <span className="yn-mono">{env.namespace}</span>
                      </div>
                    </div>
                    {/* 徽标与下面的 CPU/内存取的是**同一份实时数据**：两处若各取一处
                        （徽标读快照、数字读实时），就会出现"数字写着 93%、徽标却按
                        几小时前的 96% 判成资源紧张"这种自相矛盾的卡片。 */}
                    <EnvStatusBadge status={resStatusOf(env.key)} />
                  </div>

                  <div className="yn-env-card__body">
                    <div style={{ fontSize: 13, color: '#5c6470' }}>
                      {env.cluster}
                      {isOwnCluster(env) ? (
                        <Tooltip title={OWN_CLUSTER_HINT}>
                          <Tag style={{ marginInlineStart: 6, marginInlineEnd: 0 }}>
                            {OWN_CLUSTER_BADGE}
                          </Tag>
                        </Tooltip>
                      ) : null}
                      <Text type="secondary" style={{ fontSize: 12, marginLeft: 6 }}>
                        {env.clusterVersion}
                      </Text>
                    </div>
                    <div style={{ marginTop: 10 }}>
                      <Space size={6} wrap>
                        {/* 原「部署方式」标签已去掉：平台只有 AppRelease/operator 一条路，
                            每个环境都顶着同一个标签，等于没信息。 */}
                        {/* 节点数读得到就用**集群实时的**（这个值跟 CPU/内存同一次请求回来，
                            不受 metrics-server 缺失影响）；整条请求失败才退到环境快照
                            —— 那时下面的用量行会同时显示"读不到集群用量"，不会让人误以为
                            这是一份新鲜数据。 */}
                        <Tag style={{ marginInlineEnd: 0 }}>{r?.nodes ?? env.nodes} 节点</Tag>
                        <Tag style={{ marginInlineEnd: 0 }}>{svcs.length} 个服务</Tag>
                        {/* 该环境没有纳管服务时不给任何结论：既不是"全部正常"
                            （没有东西可以正常），也不该报"待关注"。 */}
                        {!svcs.length ? null : bad.length ? (
                          <Tag color="error" style={{ marginInlineEnd: 0 }}>
                            {bad.length} 个待关注
                          </Tag>
                        ) : svcUnknown ? (
                          <Tooltip title="这些服务没能读到集群状态，未纳入判断">
                            <Tag color="warning" style={{ marginInlineEnd: 0 }}>
                              {svcUnknown} 个状态读取不到
                            </Tag>
                          </Tooltip>
                        ) : (
                          <Tag color="success" style={{ marginInlineEnd: 0 }}>
                            全部正常
                          </Tag>
                        )}
                      </Space>
                    </div>

                    {/* 读不到集群用量时显示"—"而不是 0%：一个 0 会被读成"这份集群很闲"，
                        而实际是"没读到"。与「环境详情」同一条纪律，那边连进度条都不画。 */}
                    <div className="yn-env-card__metrics">
                      <div className="yn-env-card__metric">
                        <div className="yn-env-card__metric-label">CPU</div>
                        <div className="yn-env-card__metric-value" style={{ color: usage && usage.cpuUsage > 85 ? '#ff4d4f' : '#2c313a' }}>
                          {usage ? `${usage.cpuUsage}%` : '—'}
                        </div>
                      </div>
                      <div className="yn-env-card__metric">
                        <div className="yn-env-card__metric-label">内存</div>
                        <div className="yn-env-card__metric-value" style={{ color: usage && usage.memUsage > 85 ? '#ff4d4f' : '#2c313a' }}>
                          {usage ? `${usage.memUsage}%` : '—'}
                        </div>
                      </div>
                      <div className="yn-env-card__metric">
                        <div className="yn-env-card__metric-label">项目</div>
                        <div className="yn-env-card__metric-value">{env.projectKeys.length}</div>
                      </div>
                    </div>

                    {usage ? (
                      <div style={{ marginTop: 14 }}>
                        <UsageBar label="CPU 用量" percent={usage.cpuUsage} />
                        <div style={{ marginBottom: 0 }}>
                          <UsageBar label="内存用量" percent={usage.memUsage} />
                        </div>
                      </div>
                    ) : (
                      <div style={{ marginTop: 14, fontSize: 12, color: '#9aa0aa' }}>
                        {envRes.error
                          ? '读不到集群用量'
                          : envRes.loading
                            ? '正在读取集群用量…'
                            : '集群未提供用量指标（需安装 metrics-server）'}
                      </div>
                    )}
                  </div>

                  {/* 环境级动作**必须两个视图一致**。
                      这块原先只补了「编辑」：它原本只长在表格视图的操作列里，而本页默认落在
                      卡片视图上，于是"去编辑里把项目纳管进来"在默认视图里无处可点
                      （用户 2026-09-29 反馈"哪里有编辑？我没看到啊"）。
                      补编辑时漏了删除，于是同一个毛病换到删除上继续犯。
                      现在两个视图共用 `EnvironmentActions`，加动作只需改那一处。
                      卡片整卡就是"进入"，所以这里 `withEnter` 不传。 */}
                  <div className="yn-env-card__foot">
                    <span style={{ display: 'flex', gap: 12, minWidth: 0, overflow: 'hidden' }}>
                      <span style={{ whiteSpace: 'nowrap' }}>
                        入口域名 <span className="yn-mono">{env.ingressDomain || '—'}</span>
                      </span>
                      <span style={{ whiteSpace: 'nowrap' }}>负责人 {env.owner || '—'}</span>
                    </span>
                    <EnvironmentActions
                      env={env}
                      svcCount={svcCountOf(env.key)}
                      compact
                      onEdit={openEdit}
                      onRemoved={refreshAll}
                    />
                  </div>
                </div>
              </Col>
            );
          })}
        </Row>
      ) : (
        <SectionCard bodyStyle={{ padding: 0 }}>
          <Table<Environment>
            rowKey="id"
            size="middle"
            loading={envApi.loading}
            dataSource={envs}
            columns={columns}
            // 1446 = 各列固定宽度之和（200+108+260+130+240+130+96+92+190）。
            // 原值 1720 是含「部署方式」列时的写法；删列后必须重算一遍 ——
            // 这个"表格最小宽度"一旦大于真实列宽，表格右侧就会凭空多出一段空白。
            // 值仍大于容器宽（约 1200），所以横向滚动照旧：9 列本来就放不下。
            scroll={{ x: 1446 }}
            pagination={{ pageSize: 12, showTotal: (t) => `共 ${t} 个环境` }}
          />
        </SectionCard>
      )}

      {/* -------------------------------------------------- 新建 / 编辑环境
          表单本体在 `EnvironmentEditDrawer`（列表页与详情页共用一份）：
          字段口径只留一处，避免"两个入口、字段漂移、静默丢字段"。 */}
      <EnvironmentEditDrawer
        open={drawerOpen}
        target={editTarget}
        clusterValuesInUse={clusterValuesInUse}
        onClose={() => setDrawerOpen(false)}
        onSaved={refreshAll}
      />
    </div>
  );
};

export default EnvironmentList;
