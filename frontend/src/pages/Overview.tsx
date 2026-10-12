import React, { useEffect, useMemo, useState } from 'react';
import { Badge, Button, Col, Progress, Row, Space, Table, Tag, Tooltip, Typography } from 'antd';
import {
  AppstoreOutlined,
  ArrowRightOutlined,
  CheckCircleOutlined,
  CloudServerOutlined,
  ExclamationCircleOutlined,
  ReloadOutlined,
  RocketOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons';
import { Link, useNavigate } from 'react-router-dom';
import { PageHeader, SectionCard, StatCard } from '@/components/ui';
import { AreaChart, BarList, DonutChart } from '@/components/charts';
import { BuildStatusTag, EnvStatusBadge, ProjectTypeTag } from '@/components/StatusTag';
import { buildApi, environmentApi, projectApi, systemApi } from '@/api';
import { useApi } from '@/hooks/useApi';
import { envResStatusOf, useEnvResources } from '@/hooks/useEnvResources';
import { isServiceAbnormal, liveWordOf } from '@/utils/live';
import { ago, humanDuration } from '@/utils/format';
import { getSession } from '@/auth';
import type { BuildRecord, Environment } from '@/types';

const { Text } = Typography;

const pad = (n: number) => String(n).padStart(2, '0');

const Overview: React.FC = () => {
  const navigate = useNavigate();

  // 实时时钟：给首页一个「正在运行」的时间锚点
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const timer = window.setInterval(() => setNow(new Date()), 1000);
    return () => window.clearInterval(timer);
  }, []);

  const clock = `${pad(now.getHours())}:${pad(now.getMinutes())}:${pad(now.getSeconds())}`;
  const dateText = `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())} 星期${
    '日一二三四五六'[now.getDay()]
  }`;
  const hour = now.getHours();
  const greeting = hour < 6 ? '凌晨好' : hour < 12 ? '上午好' : hour < 14 ? '中午好' : hour < 18 ? '下午好' : '晚上好';
  const me = getSession()?.name ?? '运维示例';

  // 四路取数并行：平台口径的汇总数字走 /system/stats（后端已收敛口径），
  // 分布类图表从列表接口聚合——后端没有为每张图单独开接口，这是有意的：
  // 概览页的图表都是"同一批数据的另一种切法"，多开接口只会让口径出现分歧。
  const statsApi = useApi(() => systemApi.stats(), []);
  const buildsApi = useApi(() => buildApi.list({ pageSize: 100 }), []);
  const projectsApi = useApi(() => projectApi.list({ pageSize: 200 }), []);
  const envsApi = useApi(() => environmentApi.list({ pageSize: 100 }), []);

  const stats = statsApi.data;
  const builds = useMemo(() => buildsApi.data?.list ?? [], [buildsApi.data]);
  const projects = useMemo(() => projectsApi.data?.list ?? [], [projectsApi.data]);
  const envs = useMemo(() => envsApi.data?.list ?? [], [envsApi.data]);

  /**
   * 环境 → 其下服务实例。后端的环境对象里没有"服务数/异常数"这类汇总，
   * 只能逐个环境取一次 /environments/{key}/services 再在前端合并。
   * 环境是一期个位数（5 个），这点请求量可以接受；
   * 若将来环境数量上去了，应该由后端在环境列表里直接带上汇总字段。
   */
  const envServicesApi = useApi(async () => {
        const pairs = await Promise.all(
          envs.map(async (e) => [e.key, await environmentApi.services(e.key)] as const),
        );
        return new Map(pairs);
      }, [envs], { enabled: envs.length > 0 });
  const envServices = useMemo(
    () => envServicesApi.data ?? new Map<string, Awaited<ReturnType<typeof environmentApi.services>>>(),
    [envServicesApi.data],
  );

  /**
   * 环境资源状态同样读**实时值**（`/environments/{key}/resources`），
   * 与「环境管理」「环境详情」共用 `useEnvResources` 一份口径。
   *
   * 原先用的是环境快照 `environment.health_status` —— 它只在「同步集群」时写一次，
   * 于是首页可能挂着一个几小时前的"资源紧张"。（与本页的"环境资源状态"卡片同名，
   * 两边必须同源，否则用户从首页点进环境管理会看到两个不一样的结论。）
   */
  const envRes = useEnvResources(envs.map((e) => e.key));
  const envStatusOf = (envKey: string) => envResStatusOf(envRes.map, envKey);

  /**
   * 「需要关注的服务」按**集群实时状态**数，不用 `/system/stats` 的 `abnormalServices`。
   *
   * 后端那一项是 `service_instance.status IN ('error','pending')` 的**库内计数**，
   * 而库里的 status 是纳管时写入的期望值、之后只有部署成功才更新 ——
   * 实测生产库里有服务记着 `pending`、`image` 还是空串，集群里其实 running 1/1，
   * 首页因此常年报一个并不存在的"1 个服务状态需要关注"。
   */
  const svcStats = useMemo(() => {
    let abnormal = 0;
    let unknown = 0;
    envServices.forEach((list) =>
      list.forEach((s) => {
        if (isServiceAbnormal(s)) abnormal += 1;
        else if (liveWordOf(s) === 'unknown') unknown += 1;
      }),
    );
    return { abnormal, unknown };
  }, [envServices]);

  const loading = statsApi.loading || buildsApi.loading || projectsApi.loading || envsApi.loading;

  const refreshAll = () => {
    statsApi.refresh();
    buildsApi.refresh();
    projectsApi.refresh();
    envsApi.refresh();
    // 环境资源状态是现读集群的，刷新整页时也得跟着重读，否则"点刷新"只更新了一半。
    envRes.refresh();
  };

  const activeBuilds = stats?.runningBuilds ?? 0;

  const typeDistribution = useMemo(
    () => [
      { label: '前端项目', value: projects.filter((p) => p.type === 'frontend').length, color: '#1677ff' },
      { label: '后端项目', value: projects.filter((p) => p.type === 'backend').length, color: '#722ed1' },
    ],
    [projects],
  );

  const buildDistribution = useMemo(
    () => [
      { label: '成功', value: builds.filter((b) => b.status === 'success').length, color: '#52c41a' },
      { label: '失败', value: builds.filter((b) => b.status === 'failed').length, color: '#ff4d4f' },
      { label: '进行中', value: builds.filter((b) => b.status === 'running' || b.status === 'queued').length, color: '#1677ff' },
      { label: '已取消', value: builds.filter((b) => b.status === 'canceled').length, color: '#d9d9d9' },
      // 已中断**单列一瓣**，不许并进"已取消"：两者在饼图上是同一个灰色扇区，
      // 但一个是"有人主动叫停"、一个是"平台重启打断"，合并后这张图就开始说假话。
      { label: '已中断', value: builds.filter((b) => b.status === 'interrupted').length, color: '#fa8c16' },
    ],
    [builds],
  );

  const recentBuilds = useMemo(() => builds.slice(0, 8), [builds]);

  const pendingApprovals = stats?.pendingApproval ?? 0;

  const durationTop = useMemo(
    () =>
      projects
        .map((p) => ({ label: p.name, value: p.avgDuration, suffix: 's' }))
        .sort((a, b) => b.value - a.value)
        .slice(0, 7),
    [projects],
  );

  /** 环境资源水位分布。stats 只给了环境总数，各档按**实时**资源状态聚合（见 envRes）。 */
  const envDistribution = useMemo(
    () => ({
      total: stats?.environments ?? envs.length,
      services: stats?.activeServices ?? 0,
      healthy: envs.filter((e) => envStatusOf(e.key) === 'healthy').length,
      warning: envs.filter((e) => envStatusOf(e.key) === 'warning').length,
      error: envs.filter((e) => envStatusOf(e.key) === 'error').length,
      // 读不到集群用量的环境单列一档：既不算正常也不算告警，否则三档相加会少于总数
      // 而没人说明少在哪。
      unknown: envs.filter((e) => envStatusOf(e.key) === 'unknown').length,
    }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [envs, stats, envRes.map],
  );

  /**
   * 近 14 天构建趋势。
   * 后端未提供趋势接口，这里按 startedAt 的日期在前端聚合。
   * 只统计已取回的构建（默认 100 条），因此 14 天前的历史可能不全——
   * 对"近两周走势"这个用途足够；若将来要精确，应由后端出聚合接口。
   */
  const buildTrend = useMemo(() => {
    const dayKey = (t: string) => (t || '').slice(0, 10);
    const buckets = new Map<string, { total: number; failed: number }>();
    const days: { date: string; total: number; failed: number }[] = [];
    for (let i = 13; i >= 0; i -= 1) {
      const d = new Date();
      d.setDate(d.getDate() - i);
      const key = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
      buckets.set(key, { total: 0, failed: 0 });
    }
    for (const b of builds) {
      const k = dayKey(b.startedAt);
      const bucket = buckets.get(k);
      if (!bucket) continue;
      bucket.total += 1;
      if (b.status === 'failed') bucket.failed += 1;
    }
    for (const [date, v] of buckets) days.push({ date: date.slice(5), total: v.total, failed: v.failed });
    return days;
  }, [builds]);

  /**
   * 平均构建耗时：只算**真正跑完**的构建（成功 / 失败）。
   *
   * 两个排除项各有各的理由，都不能省：
   *  - 进行中的耗时是 0，算进来会凭空拉低均值；
   *  - `interrupted`（平台重启打断）**不是一次完整的构建**，它的耗时只到被切断那一刻，
   *    跟跑完的构建不是一回事。拉进来会让"平均构建耗时"随平台重启频率上下浮动 ——
   *    一个衡量流水线速度的指标，不该被运维动作污染。
   */
  const avgBuildDuration = useMemo(() => {
    const done = builds.filter((b) => b.status === 'success' || b.status === 'failed');
    if (!done.length) return 0;
    return Math.round(done.reduce((sum, b) => sum + (b.duration || 0), 0) / done.length);
  }, [builds]);

  /** 最快 / 最慢耗时，用于成功率卡片下方的区间展示。取数与均值同一口径（见上）。 */
  const durationRange = useMemo(() => {
    const vals = builds
      .filter((b) => b.status === 'success' || b.status === 'failed')
      .map((b) => b.duration)
      .filter((n) => n > 0);
    if (!vals.length) return { min: 0, max: 0 };
    return { min: Math.min(...vals), max: Math.max(...vals) };
  }, [builds]);

  /**
   * 待办项全部由真实数据推导。
   *
   * 注意这里**删掉了原型阶段的「Harbor 存储使用率 78%」那条**：
   * 后端没有暴露仓库存储容量（本地实现也没有这个概念），
   * 继续展示一个编出来的百分比，等于让运维照着假数字去清镜像。
   * 第四项改成有真实来源的「镜像总数」。
   */
  const todoItems = useMemo(() => {
    const failed = builds.filter((b) => b.status === 'failed');
    // 被平台重启打断的构建。**单列一条**，既不并进「失败」也不省略：
    //  - 并进失败 → 用户按"这批构建为什么挂"去排查，可实际原因是平台重启，
    //    代码和流水线都没问题，白查一场；
    //  - 省略 → 重启一次悄悄丢掉几个构建，界面上一条痕迹都没有。
    // 它的动作很明确（重新触发），但这个动作不会自己发生，所以必须被说出来。
    const interrupted = builds.filter((b) => b.status === 'interrupted');
    // 用 svcStats（集群实时）而不是 stats.abnormalServices（库内计数）—— 见 svcStats 的注释。
    const abnormal = svcStats.abnormal;
    const pending = envs.filter((e) => e.needApproval).map((e) => e.name);
    return [
      {
        key: 'approval',
        level: 'warning' as const,
        title: pendingApprovals ? `${pendingApprovals} 个部署单待审批` : '暂无待审批部署单',
        desc: pending.length ? `需审批环境：${pending.join(' / ')}` : '当前没有开启审批的环境',
        to: '/cd/deploys',
      },
      {
        key: 'failed',
        level: 'error' as const,
        title: failed.length ? `${failed.length} 次构建失败` : '暂无失败构建',
        desc: failed.length
          ? `最近失败：${failed
              .slice(0, 3)
              .map((b) => b.projectKey)
              .join(' / ')}`
          : '近期待构建任务的镜像均已产出',
        to: '/ci/builds',
      },
      // 只有真的发生过才出现：常态下它恒为 0，常驻一行"暂无中断构建"纯属噪音。
      // 另外三条是永久槽位（各有零态文案），这一条是事件 —— 两者性质不同。
      ...(interrupted.length
        ? [
            {
              key: 'interrupted',
              level: 'warning' as const,
              title: `${interrupted.length} 次构建被平台重启中断`,
              desc: `涉及：${interrupted
                .slice(0, 3)
                .map((b) => b.projectKey)
                .join(' / ')}；需重新触发才会产出镜像`,
              to: '/ci/builds',
            },
          ]
        : []),
      {
        key: 'svc',
        level: 'error' as const,
        title: abnormal ? `${abnormal} 个服务状态异常` : '服务实例全部正常',
        // 「涉及环境」也要按同一口径找：拿资源水位（`e.status`）当筛选条件会挑出
        // 一批与"哪个服务坏了"无关的环境。这里直接把确实有异常服务的环境列出来。
        desc: abnormal
          ? `涉及环境：${
              envs
                .filter((e) => (envServices.get(e.key) ?? []).some(isServiceAbnormal))
                .map((e) => e.name)
                .join(' / ') || '待排查'
            }`
          : `共 ${stats?.activeServices ?? 0} 个服务实例在运行${
              svcStats.unknown ? `（${svcStats.unknown} 个集群状态读取不到，未纳入判断）` : ''
            }`,
        to: '/cd/environments',
      },
      {
        key: 'images',
        level: 'warning' as const,
        title: `镜像仓库共 ${stats?.totalImages ?? 0} 个镜像`,
        desc: '可按项目与时间清理不再使用的版本，释放仓库空间',
        to: '/ci/images',
      },
    ];
  }, [pendingApprovals, builds, envs, stats, envServices, svcStats]);

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: '首页' }, { title: '概览' }]}
        title="运维发布总览"
        description="平台运行概况，数据来自后端实时接口"
        extra={
          <Space>
            <Text type="secondary" style={{ fontSize: 12 }} className="yn-mono">
              实时 · {clock}
            </Text>
            <Button icon={<ReloadOutlined />} loading={loading} onClick={refreshAll}>
              刷新
            </Button>
            <Button type="primary" icon={<ThunderboltOutlined />} onClick={() => navigate('/ci/projects')}>
              去编译
            </Button>
          </Space>
        }
      />

      {/* ---------------------------------------------------------------- 动态横幅 */}
      <div className="yn-hero yn-rise">
        <div className="yn-hero__inner">
          <div>
            <div className="yn-hero__greet">
              <span className="yn-live-dot" />
              {greeting}，{me}
            </div>
            <div className="yn-hero__sub">
              平台运行平稳：{projects.length} 个项目纳管，今日构建 {stats?.todayBuilds ?? 0} 次、成功率{' '}
              {stats?.successRate ?? 0}%，{svcStats.abnormal} 个服务状态需要关注。
            </div>
            <div className="yn-hero__chips">
              <span className="yn-hero__chip">
                <span className="yn-live-dot" />
                纳管环境 <b>{envDistribution.total}</b> 个
              </span>
              <span className="yn-hero__chip">
                进行中构建 <b>{activeBuilds}</b> 个
              </span>
              <span className="yn-hero__chip">
                待审批部署 <b>{pendingApprovals}</b> 单
              </span>
              <span className="yn-hero__chip">
                <span className="yn-live-dot yn-live-dot--warning" />
                告警服务 <b>{svcStats.abnormal}</b> 个
              </span>
            </div>
          </div>
          <div style={{ textAlign: 'right' }}>
            <div className="yn-hero__clock">{clock}</div>
            <div className="yn-hero__clock-label">{dateText} · 数据实时刷新</div>
          </div>
        </div>
      </div>

      {/* 说明：原型阶段这两张卡片带「较昨日 / 较上周」的涨跌角标，
          但后端没有历史快照可比（/system/stats 只给当期绝对值），
          编一个 delta 会让页面凭空多出一条假趋势，因此这里**去掉角标**，
          改为展示能从真实数据算出来的区间信息（最快/最慢、进行中）。 */}
      <Row gutter={[16, 16]} className="yn-stagger">
        <Col xs={24} sm={12} xl={6}>
          <StatCard
            title="纳管项目"
            value={projects.length}
            icon={<AppstoreOutlined />}
            accent="#1677ff"
            footer={
              <Space size={6}>
                <Tag color="cyan" style={{ marginInlineEnd: 0 }}>前端 {typeDistribution[0].value}</Tag>
                <Tag color="geekblue" style={{ marginInlineEnd: 0 }}>后端 {typeDistribution[1].value}</Tag>
                <Text type="secondary" style={{ fontSize: 12 }}>启用 {projects.filter((p) => p.enabled).length}</Text>
              </Space>
            }
          />
        </Col>
        <Col xs={24} sm={12} xl={6}>
          <StatCard
            title="今日构建"
            value={stats?.todayBuilds ?? 0}
            unit="次"
            icon={<ThunderboltOutlined />}
            accent="#1677ff"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                进行中 {stats?.runningBuilds ?? 0} 次 · 待审批 {pendingApprovals} 单
              </Text>
            }
          />
        </Col>
        <Col xs={24} sm={12} xl={6}>
          <StatCard
            title="构建成功率"
            value={stats?.successRate ?? 0}
            unit="%"
            icon={<CheckCircleOutlined />}
            accent="#52c41a"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                样本 {builds.length} 次 · 平均耗时 {humanDuration(avgBuildDuration)}
              </Text>
            }
          />
        </Col>
        <Col xs={24} sm={12} xl={6}>
          <StatCard
            title="环境 / 服务"
            value={`${envDistribution.total} / ${envDistribution.services}`}
            icon={<CloudServerOutlined />}
            accent="#722ed1"
            footer={
              <Space size={6}>
                <Badge status="success" text={<span style={{ fontSize: 12 }}>正常 {envDistribution.healthy}</span>} />
                <Badge status="warning" text={<span style={{ fontSize: 12 }}>资源告警 {envDistribution.warning}</span>} />
                <Badge status="error" text={<span style={{ fontSize: 12 }}>资源紧张 {envDistribution.error}</span>} />
                {envDistribution.unknown ? (
                  <Badge
                    status="default"
                    text={<span style={{ fontSize: 12 }}>读取不到 {envDistribution.unknown}</span>}
                  />
                ) : null}
              </Space>
            }
          />
        </Col>
      </Row>

      <Row gutter={[16, 16]} style={{ marginTop: 16 }}>
        <Col xs={24} xl={16}>
          <SectionCard
            title="构建趋势（近 14 天）"
            extra={
              <Space size={16}>
                <span className="yn-legend"><i style={{ background: '#1677ff' }} />构建总量</span>
                <span className="yn-legend"><i style={{ background: '#ff4d4f' }} />失败数</span>
              </Space>
            }
            bodyStyle={{ padding: '12px 16px 4px' }}
          >
            <AreaChart
              data={buildTrend.map((d) => ({ label: d.date, value: d.total }))}
              secondary={buildTrend.map((d) => ({ label: d.date, value: d.failed }))}
              height={214}
              unit=" 次"
            />
          </SectionCard>
        </Col>
        <Col xs={24} xl={8}>
          <SectionCard title="构建结果分布" bodyStyle={{ padding: '18px 20px' }} style={{ height: '100%' }}>
            <DonutChart
              data={buildDistribution}
              centerValue={`${stats?.successRate ?? 0}%`}
              centerLabel="成功率"
            />
            <div style={{ marginTop: 18, borderTop: '1px solid #f0f2f5', paddingTop: 14 }}>
              <div style={{ display: 'flex', justifyContent: 'space-between', marginBottom: 8 }}>
                <Text type="secondary" style={{ fontSize: 12.5 }}>平均构建耗时</Text>
                <Text strong>{humanDuration(avgBuildDuration)}</Text>
              </div>
              <Progress
                percent={stats?.successRate ?? 0}
                showInfo={false}
                strokeColor={{ from: '#69b1ff', to: '#1677ff' }}
                size={['100%', 6]}
              />
              <div style={{ display: 'flex', justifyContent: 'space-between', marginTop: 6 }}>
                <Text type="secondary" style={{ fontSize: 11.5 }}>最快 {humanDuration(durationRange.min)}</Text>
                <Text type="secondary" style={{ fontSize: 11.5 }}>最慢 {humanDuration(durationRange.max)}</Text>
              </div>
            </div>
          </SectionCard>
        </Col>
      </Row>

      <Row gutter={[16, 16]} style={{ marginTop: 16 }}>
        <Col xs={24} xl={16}>
          <SectionCard
            title="最近构建"
            extra={
              <Link to="/ci/builds" style={{ fontSize: 13, fontWeight: 400 }}>
                查看全部 <ArrowRightOutlined style={{ fontSize: 11 }} />
              </Link>
            }
            bodyStyle={{ padding: 0 }}
          >
            <Table<BuildRecord>
              rowKey="id"
              size="small"
              dataSource={recentBuilds}
              pagination={false}
              columns={[
                {
                  title: '项目',
                  dataIndex: 'projectName',
                  render: (v, r) => (
                    <Space size={6}>
                      <Link to={`/ci/projects/${r.projectKey}`}>{v}</Link>
                      <ProjectTypeTag type={r.projectType} />
                    </Space>
                  ),
                },
                {
                  title: '构建号',
                  dataIndex: 'buildNo',
                  width: 92,
                  render: (v) => <span className="yn-mono">#{v}</span>,
                },
                {
                  title: '分支',
                  dataIndex: 'branch',
                  width: 118,
                  render: (v) => <Tag style={{ marginInlineEnd: 0 }}>{v}</Tag>,
                },
                {
                  title: '状态',
                  dataIndex: 'status',
                  width: 108,
                  render: (v) => <BuildStatusTag status={v} />,
                },
                {
                  title: '耗时',
                  dataIndex: 'duration',
                  width: 112,
                  // 进行中的构建用流动进度条代替耗时，页面就有「在跑」的观感
                  render: (v, r) =>
                    r.status === 'running' ? (
                      <Progress
                        percent={r.progress}
                        size="small"
                        showInfo={false}
                        strokeColor="#1677ff"
                        className="yn-progress-live"
                      />
                    ) : r.status === 'queued' ? (
                      '—'
                    ) : (
                      humanDuration(v)
                    ),
                },
                {
                  title: '触发人',
                  dataIndex: 'trigger',
                  width: 100,
                  render: (v, r) => (
                    <Tooltip title={r.triggerType === 'webhook' ? '由 Webhook 自动触发' : r.triggerType === 'schedule' ? '定时任务触发' : '手动触发'}>
                      <span>{v}</span>
                    </Tooltip>
                  ),
                },
                {
                  title: '时间',
                  dataIndex: 'startedAt',
                  width: 168,
                  render: (v) => <span className="yn-mono" style={{ color: '#8c8c8c' }}>{v}</span>,
                },
              ]}
            />
          </SectionCard>
        </Col>

        <Col xs={24} xl={8}>
          <SectionCard
            title="环境资源状态"
            extra={
              <Link to="/cd/environments" style={{ fontSize: 13, fontWeight: 400 }}>
                管理 <ArrowRightOutlined style={{ fontSize: 11 }} />
              </Link>
            }
            bodyStyle={{ padding: '8px 8px 12px' }}
            style={{ height: '100%' }}
          >
            {envs.map((env: Environment) => {
              const svcs = envServices.get(env.key) ?? [];
              // 「待关注」按**集群实时状态**数（`isServiceAbnormal`），不用库里的 status。
              const bad = svcs.filter(isServiceAbnormal).length;
              return (
                <div
                  key={env.key}
                  onClick={() => navigate(`/cd/environments/${env.key}`)}
                  style={{
                    display: 'flex',
                    alignItems: 'center',
                    justifyContent: 'space-between',
                    padding: '10px 12px',
                    borderRadius: 6,
                    cursor: 'pointer',
                  }}
                  className="yn-env-row"
                >
                  <div>
                    <Space size={8}>
                      <span style={{ fontWeight: 500 }}>{env.name}</span>
                      <span className="yn-mono" style={{ fontSize: 11.5, color: '#9aa0aa' }}>
                        {env.namespace}
                      </span>
                    </Space>
                    <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 3 }}>
                      {svcs.length} 个服务{bad ? ` · ${bad} 个待关注` : ''}
                    </div>
                  </div>
                  <EnvStatusBadge status={envStatusOf(env.key)} />
                </div>
              );
            })}
          </SectionCard>
        </Col>
      </Row>

      <Row gutter={[16, 16]} style={{ marginTop: 16 }}>
        <Col xs={24} xl={12}>
          <SectionCard title="待处理事项" bodyStyle={{ padding: '6px 8px 10px' }}>
            {todoItems.map((t) => (
              <div
                key={t.key}
                onClick={() => navigate(t.to)}
                style={{
                  display: 'flex',
                  gap: 10,
                  padding: '10px 12px',
                  borderRadius: 6,
                  cursor: 'pointer',
                }}
                className="yn-env-row"
              >
                <ExclamationCircleOutlined
                  style={{ color: t.level === 'error' ? '#ff4d4f' : '#faad14', marginTop: 3 }}
                />
                <div style={{ flex: 1 }}>
                  <div style={{ fontSize: 13.5 }}>{t.title}</div>
                  <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 3 }}>{t.desc}</div>
                </div>
                <ArrowRightOutlined style={{ color: '#c2c7d0', fontSize: 12, marginTop: 4 }} />
              </div>
            ))}
          </SectionCard>
        </Col>
        <Col xs={24} xl={12}>
          <SectionCard
            title="构建耗时排行（平均）"
            bodyStyle={{ padding: '12px 20px 16px' }}
            extra={<Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>单位：秒</Text>}
          >
            <BarList data={durationTop} suffix="s" />
            <div style={{ marginTop: 12 }}>
              <Button
                type="link"
                size="small"
                style={{ paddingLeft: 0 }}
                onClick={() => navigate('/ci/builds')}
                icon={<RocketOutlined />}
              >
                去优化流水线
              </Button>
            </div>
          </SectionCard>
        </Col>
      </Row>
    </div>
  );
};

export default Overview;
