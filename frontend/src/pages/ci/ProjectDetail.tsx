import React, { useMemo, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Button,
  Col,
  Descriptions,
  Drawer,
  Empty,
  Row,
  Space,
  Spin,
  Statistic,
  Table,
  Tabs,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  CopyOutlined,
  EditOutlined,
  FileTextOutlined,
  HistoryOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons';
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom';
import { PageHeader, SectionCard } from '@/components/ui';
import { LogPanel } from '@/components/LogPanel';
import ProjectManifests from '@/components/ProjectManifests';
import { BuildStatusTag, DigestText, ImageText, MethodTag, ProjectTypeTag, ServiceStatusTag } from '@/components/StatusTag';
import { buildApi, environmentApi, projectApi } from '@/api';
import { useApi } from '@/hooks/useApi';
import { useJobLogs } from '@/hooks/useJobLogs';
import { humanDuration, humanSize } from '@/utils/format';
import { liveFallbackText, liveReplicaTotal, liveState, liveStatusWord } from '@/utils/live';
import type { BuildRecord, EnvService, ImageRecord } from '@/types';

const { Text } = Typography;

const isLiveBuild = (s: string) => s === 'running' || s === 'queued';

/**
 * 部署实例的"当前镜像 / 副本 / 状态"一律以**集群实时状态**为准。
 *
 * 平台库里的 `service_instance.current_image / replicas / status` 是纳管时按默认值写入的
 * 期望，之后只有部署成功才更新，与集群实况会长期不一致（实测 demobasic：库值 1.2.0.2761、
 * 集群 1.2.0.2577）。CI 项目详情页与 CD 各页面展示的是同一批值，口径必须一致。
 * 读不到集群时**不回落**到库值，而是如实说读不到。
 */
const liveOf = (s: EnvService) => liveState(s);

/** 状态标签：读得到就给实时状态，读不到就把原因（未接入 / 无权 / 集群中不存在）如实写出来。 */
const DeployStateTag: React.FC<{ s: EnvService }> = ({ s }) => {
  const lv = liveOf(s);
  if (lv) return <ServiceStatusTag status={liveStatusWord(lv.status)} />;
  return (
    <Tag style={{ marginInlineEnd: 0, color: '#8c8c8c' }} color="default">
      {liveFallbackText(s)}
    </Tag>
  );
};

const ProjectDetail: React.FC = () => {
  const { key = '' } = useParams();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const { message } = AntApp.useApp();

  const [logTarget, setLogTarget] = useState<BuildRecord | null>(null);
  const [buildPage, setBuildPage] = useState(1);
  const [imagePage, setImagePage] = useState(1);
  const PAGE_SIZE = 10;

  const projectApi_ = useApi(() => projectApi.get(key), [key], { enabled: !!key });
  const project = projectApi_.data;

  // 三个 Tab 各自独立分页：放在一个请求里会让"翻构建页"把镜像页也重置
  const buildsApi = useApi(
    () => projectApi.builds(key, { page: buildPage, pageSize: PAGE_SIZE }),
    [key, buildPage],
    { enabled: !!key },
  );
  const imagesApi = useApi(
    () => projectApi.images(key, { page: imagePage, pageSize: PAGE_SIZE }),
    [key, imagePage],
    { enabled: !!key },
  );
  const builds = buildsApi.data?.list ?? [];
  const images = imagesApi.data?.list ?? [];

  /**
   * 该项目在各环境的服务实例。
   *
   * 后端没有"按项目查服务实例"的接口，只有 `/environments/{key}/services`，
   * 所以这里先取环境列表再并发拉各环境的实例，最后按 projectKey 过滤。
   * 环境只有个位数，代价可接受；将来若环境变多，应该让后端补一个查询接口。
   */
  const servicesApi = useApi(
    async () => {
      const envPage = await environmentApi.list({ pageSize: 100 });
      const envs = envPage.list ?? [];
      const groups = await Promise.all(
        envs.map(async (e) => {
          const list = await environmentApi.services(e.key).catch(() => [] as EnvService[]);
          return list.map((s) => ({ ...s, envName: e.name }));
        }),
      );
      return groups.flat().filter((s) => s.projectKey === key);
    },
    [key],
    { enabled: !!key },
  );
  const deployments = useMemo(
    () => (servicesApi.data ?? []) as (EnvService & { envName?: string })[],
    [servicesApi.data],
  );

  // ------------------------------------------------------------------ 日志
  const logs = useJobLogs({
    id: logTarget?.id ?? null,
    live: !!logTarget && isLiveBuild(logTarget.status),
    stream: buildApi.stream,
    fetchHistory: buildApi.logs,
  });

  const refreshAll = () => {
    servicesApi.refresh();
    projectApi_.refresh();
  };

  if (projectApi_.error) {
    return (
      <div style={{ marginTop: 40 }}>
        <Alert
          type="error"
          showIcon
          message={`项目 ${key} 加载失败`}
          description={projectApi_.error.message}
          action={<Button size="small" onClick={() => navigate('/ci/projects')}>返回列表</Button>}
        />
      </div>
    );
  }

  if (!project) {
    return (
      <div style={{ textAlign: 'center', marginTop: 120 }}>
        <Spin />
      </div>
    );
  }

  const last = project.lastBuild;

  const buildColumns: ColumnsType<BuildRecord> = [
    { title: '构建号', dataIndex: 'buildNo', width: 96, render: (v) => <span className="yn-mono">#{v}</span> },
    {
      title: '分支 / Commit',
      dataIndex: 'branch',
      width: 210,
      render: (v, r) => (
        <div>
          <Tag style={{ marginInlineEnd: 0 }}>{v}</Tag>
          <div className="yn-mono" style={{ fontSize: 12, color: '#9aa0aa', marginTop: 3 }}>
            {r.commitId} · {(r.commitMsg || '').slice(0, 22)}
            {(r.commitMsg || '').length > 22 ? '…' : ''}
          </div>
        </div>
      ),
    },
    { title: '状态', dataIndex: 'status', width: 108, render: (v) => <BuildStatusTag status={v} /> },
    {
      title: '镜像版本',
      dataIndex: 'image',
      render: (v) => (v ? <ImageText value={v} width={300} /> : <Text type="secondary">—</Text>),
    },
    {
      title: '耗时',
      dataIndex: 'duration',
      width: 92,
      /* 口径同上（BuildList 耗时列）：`!v` 兜底，避免"从未开跑"的构建显示成「0s」。 */
      render: (v, r) => (isLiveBuild(r.status) ? <Text type="secondary">进行中</Text> : v ? humanDuration(v) : <Text type="secondary">—</Text>),
    },
    { title: '触发人', dataIndex: 'trigger', width: 100 },
    { title: '开始时间', dataIndex: 'startedAt', width: 168, render: (v) => <span className="yn-mono" style={{ color: '#8c8c8c' }}>{v}</span> },
    {
      title: '操作',
      key: 'action',
      width: 100,
      fixed: 'right',
      // 不再提供"重试"：平台不替 Jenkins 项目重新构建（后端返回 40901），
      // 一期纳管的项目又全是 Jenkins，留个点了必然报错的按钮就是假承诺。
      render: (_, r) => (
        <Space size={2}>
          <Button type="link" size="small" icon={<FileTextOutlined />} onClick={() => setLogTarget(r)} style={{ paddingInline: 4 }}>
            日志
          </Button>
        </Space>
      ),
    },
  ];

  const imageColumns: ColumnsType<ImageRecord> = [
    {
      title: '镜像 Tag',
      dataIndex: 'tag',
      width: 300,
      render: (v, r) => (
        <div>
          <ImageText value={r.image} width={290} />
          <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 3 }}>
            构建 #{r.buildNo} · {r.branch}
          </div>
        </div>
      ),
    },
    { title: 'Digest', dataIndex: 'digest', width: 190, render: (v) => <DigestText value={v} width={180} /> },
    { title: '大小', dataIndex: 'size', width: 90, render: (v) => humanSize(v) },
    {
      title: '已部署环境',
      dataIndex: 'deployedEnvs',
      width: 180,
      render: (v: string[] | undefined) =>
        v?.length ? (
          <Space size={4} wrap>
            {v.map((e) => (
              <Tag key={e} color="blue" style={{ marginInlineEnd: 0 }}>
                {e}
              </Tag>
            ))}
          </Space>
        ) : (
          <Text type="secondary">未部署</Text>
        ),
    },
    // 刻意没有「拉取次数」列：这个值要走 Harbor Core 管理接口才拿得到，
    // 平台当前走的是 registry v2（只有 pull 权限，读不到拉取统计），
    // 后端两处都硬编码 0。摆一列永远为 0 的数字，比不摆更误导。
    { title: '推送时间', dataIndex: 'pushedAt', width: 168, render: (v) => <span className="yn-mono" style={{ color: '#8c8c8c' }}>{v}</span> },
    {
      title: '操作',
      key: 'action',
      fixed: 'right',
      width: 160,
      render: (_, r) => (
        <Space size={2}>
          <Button
            type="link"
            size="small"
            icon={<CopyOutlined />}
            style={{ paddingInline: 4 }}
            onClick={() => {
              navigator.clipboard?.writeText(r.image);
              message.success('镜像地址已复制');
            }}
          >
            复制地址
          </Button>
          <Button type="link" size="small" onClick={() => navigate('/cd/environments')} style={{ paddingInline: 4 }}>
            去部署
          </Button>
        </Space>
      ),
    },
  ];

  const activeTab = searchParams.get('tab') ?? 'overview';
  const err = buildsApi.error || imagesApi.error || servicesApi.error;

  return (
    <div>
      <PageHeader
        breadcrumb={[
          { title: 'CI 镜像构建' },
          { title: '项目管理', path: '/ci/projects' },
          { title: project.name },
        ]}
        title={project.name}
        subTitle={project.key}
        tags={
          <>
            <ProjectTypeTag type={project.type} />
            <MethodTag method={project.buildMethod} />
            <Tag color={project.enabled ? 'success' : 'default'} style={{ marginInlineEnd: 0 }}>
              {project.enabled ? '已启用' : '已停用'}
            </Tag>
          </>
        }
        description={project.description}
        extra={
          <Space>
            <Button icon={<EditOutlined />} onClick={() => navigate('/ci/projects')}>
              编辑
            </Button>
            <Button icon={<HistoryOutlined />} onClick={() => setSearchParams({ tab: 'builds' })}>
              构建历史
            </Button>
            <Button type="primary" icon={<ThunderboltOutlined />} onClick={() => navigate('/ci/projects')}>
              编译
            </Button>
          </Space>
        }
      />

      {err ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message="部分数据加载失败"
          description={err.message}
          action={<Button size="small" onClick={refreshAll}>重试</Button>}
        />
      ) : null}

      {/* 最近一次构建的"成品"信息 */}
      <SectionCard
        title="最近镜像版本"
        extra={
          last ? (
            <Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>
              构建 #{last.buildNo} · {last.startedAt}
            </Text>
          ) : null
        }
        bodyStyle={{ padding: '16px 20px' }}
      >
        {last ? (
          <Row gutter={[24, 16]} align="middle">
            <Col xs={24} xl={11}>
              <div style={{ fontSize: 12, color: '#9aa0aa', marginBottom: 6 }}>镜像地址</div>
              {last.image ? (
                <>
                  <Space size={8} style={{ maxWidth: '100%' }}>
                    <ImageText value={last.image} width={380} />
                    <Tooltip title="复制镜像地址">
                      <Button
                        size="small"
                        type="text"
                        icon={<CopyOutlined />}
                        onClick={() => {
                          navigator.clipboard?.writeText(last.image);
                          message.success('镜像地址已复制');
                        }}
                      />
                    </Tooltip>
                  </Space>
                  <div style={{ marginTop: 10, fontSize: 12, color: '#9aa0aa' }}>
                    Digest：<DigestText value={last.imageDigest} width={320} />
                  </div>
                  <Space size={8} style={{ marginTop: 12 }}>
                    <Button
                      size="small"
                      icon={<CopyOutlined />}
                      onClick={() => {
                        navigator.clipboard?.writeText(`docker pull ${last.image}`);
                        message.success('已复制 docker pull 命令');
                      }}
                    >
                      docker pull
                    </Button>
                    <Button size="small" icon={<FileTextOutlined />} onClick={() => setLogTarget(last)}>
                      查看构建日志
                    </Button>
                  </Space>
                </>
              ) : (
                <Text type="secondary">
                  最近一次构建（#{last.buildNo}）{last.status === 'failed' ? '失败' : '未成功'}，没有产出镜像
                </Text>
              )}
            </Col>
            <Col xs={12} sm={8} xl={3}>
              <Statistic title="构建耗时" value={humanDuration(last.duration)} valueStyle={{ fontSize: 20 }} />
            </Col>
            <Col xs={12} sm={8} xl={3}>
              <Statistic title="镜像大小" value={last.imageSize ? humanSize(last.imageSize) : '—'} valueStyle={{ fontSize: 20 }} />
            </Col>
            <Col xs={12} sm={8} xl={3}>
              <Statistic title="Commit" value={last.commitId} valueStyle={{ fontSize: 20, fontFamily: 'monospace' }} />
            </Col>
            <Col xs={12} sm={8} xl={4}>
              <div style={{ fontSize: 12, color: '#9aa0aa', marginBottom: 6 }}>最近构建状态</div>
              <BuildStatusTag status={last.status} />
            </Col>
          </Row>
        ) : (
          <Empty description="暂无构建记录，点击右上角「编译」触发首次构建" />
        )}
      </SectionCard>

      <Tabs
        activeKey={activeTab}
        onChange={(k) => setSearchParams({ tab: k })}
        style={{ marginTop: 16 }}
        items={[
          {
            key: 'overview',
            label: '项目概览',
            children: (
              <Row gutter={[16, 16]}>
                <Col xs={24} xl={14}>
                  <SectionCard title="基本信息" bodyStyle={{ padding: '4px 0' }}>
                    <Descriptions
                      column={2}
                      size="middle"
                      style={{ padding: '12px 20px' }}
                      items={[
                        { key: 'name', label: '项目名称', children: project.name },
                        { key: 'key', label: '项目标识', children: <span className="yn-mono">{project.key}</span> },
                        { key: 'type', label: '项目类型', children: <ProjectTypeTag type={project.type} /> },
                        { key: 'team', label: '所属团队', children: project.team || '—' },
                        { key: 'owner', label: '负责人', children: project.owner || '—' },
                        { key: 'enabled', label: '启用状态', children: project.enabled ? '已启用' : '已停用' },
                        {
                          key: 'git',
                          label: 'Git 仓库',
                          span: 2,
                          // Jenkins 项目由 Job 自己管仓库，平台侧可能没有仓库地址（显示 '—'）。
                          // 没有地址时还挂一个"复制仓库地址"按钮，复制出来的是空串——那种按钮
                          // 点一下只会让人以为复制成功了。
                          children: project.gitUrl ? (
                            <Space size={6}>
                              <span className="yn-mono" style={{ fontSize: 12.5 }}>{project.gitUrl}</span>
                              <Tooltip title="复制仓库地址">
                                <Button
                                  type="text"
                                  size="small"
                                  icon={<CopyOutlined />}
                                  onClick={() => {
                                    navigator.clipboard?.writeText(project.gitUrl);
                                    message.success('仓库地址已复制');
                                  }}
                                />
                              </Tooltip>
                            </Space>
                          ) : (
                            <span style={{ color: '#9aa0aa' }}>—（仓库由 Jenkins Job 自己管理）</span>
                          ),
                        },
                        { key: 'branch', label: '默认分支', children: <Tag style={{ marginInlineEnd: 0 }}>{project.defaultBranch}</Tag> },
                        { key: 'method', label: '构建方式', children: <MethodTag method={project.buildMethod} /> },
                        { key: 'dockerfile', label: 'Dockerfile 路径', children: <span className="yn-mono">{project.dockerfilePath}</span> },
                        { key: 'context', label: '构建上下文', children: <span className="yn-mono">{project.buildContext}</span> },
                        { key: 'registry', label: '镜像仓库', span: 2, children: <span className="yn-mono">{project.registry}</span> },
                        { key: 'imageName', label: '镜像名称', span: 2, children: <span className="yn-mono">{project.registry}/{project.imageName}</span> },
                        { key: 'tagPattern', label: 'Tag 规则', span: 2, children: <span className="yn-mono">{project.tagPattern}</span> },
                        { key: 'createdAt', label: '创建时间', children: project.createdAt },
                        { key: 'updatedAt', label: '更新时间', children: project.updatedAt },
                      ]}
                    />
                  </SectionCard>
                </Col>

                <Col xs={24} xl={10}>
                  <Space direction="vertical" size={16} style={{ width: '100%' }}>
                    <SectionCard title="构建统计" bodyStyle={{ padding: '16px 20px' }}>
                      <Row gutter={16}>
                        <Col span={8}>
                          <Statistic title="累计构建" value={project.totalBuilds} valueStyle={{ fontSize: 20 }} />
                        </Col>
                        <Col span={8}>
                          <Statistic
                            title="成功率"
                            value={project.successRate}
                            suffix="%"
                            valueStyle={{ fontSize: 20, color: project.successRate >= 95 ? '#52c41a' : '#faad14' }}
                          />
                        </Col>
                        <Col span={8}>
                          <Statistic title="平均耗时" value={humanDuration(project.avgDuration)} valueStyle={{ fontSize: 20 }} />
                        </Col>
                      </Row>
                    </SectionCard>

                    <SectionCard title="构建参数" bodyStyle={{ padding: '8px 20px 16px' }}>
                      {project.buildParams?.length ? (
                        project.buildParams.map((p) => (
                          <div
                            key={p.key}
                            style={{ display: 'flex', justifyContent: 'space-between', gap: 12, padding: '8px 0', borderBottom: '1px dashed #f0f2f5', fontSize: 13 }}
                          >
                            <div>
                              <div className="yn-mono" style={{ fontSize: 12.5 }}>{p.key}</div>
                              {p.desc ? <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 2 }}>{p.desc}</div> : null}
                            </div>
                            <div className="yn-mono" style={{ color: '#5c6470', textAlign: 'right', wordBreak: 'break-all' }}>
                              {p.secret ? '••••••••' : p.value}
                            </div>
                          </div>
                        ))
                      ) : (
                        <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="未配置构建参数" />
                      )}
                    </SectionCard>

                    <SectionCard
                      title="已部署环境"
                      extra={<Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>{deployments.length} 个实例</Text>}
                      bodyStyle={{ padding: '8px 20px 16px' }}
                    >
                      {deployments.length ? (
                        deployments.map((d) => (
                          <div key={d.id} style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '8px 0', borderBottom: '1px dashed #f0f2f5' }}>
                            <div>
                              <Link to={`/cd/environments/${d.envKey}`} style={{ fontSize: 13, fontWeight: 500 }}>
                                {d.envName ?? d.envKey}
                              </Link>
                              <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 2 }}>
                                {liveOf(d)
                                  ? `${liveOf(d)!.readyReplicas}/${liveReplicaTotal(liveOf(d)!)} 副本（集群实时） · 平台记录更新于 ${d.updatedAt}`
                                  : `${liveFallbackText(d)} · 平台记录更新于 ${d.updatedAt}`}
                              </div>
                            </div>
                            <DeployStateTag s={d} />
                          </div>
                        ))
                      ) : (
                        <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="尚未部署到任何环境" />
                      )}
                    </SectionCard>
                  </Space>
                </Col>
              </Row>
            ),
          },
          {
            key: 'builds',
            label: `构建历史 ${buildsApi.data?.total ?? 0}`,
            children: (
              <SectionCard bodyStyle={{ padding: 0 }}>
                <Table<BuildRecord>
                  rowKey="id"
                  size="middle"
                  loading={buildsApi.loading}
                  dataSource={builds}
                  columns={buildColumns}
                  scroll={{ x: 1240 }}
                  pagination={{
                    current: buildPage,
                    pageSize: PAGE_SIZE,
                    total: buildsApi.data?.total ?? 0,
                    onChange: setBuildPage,
                    showTotal: (t) => `共 ${t} 条构建记录`,
                  }}
                />
              </SectionCard>
            ),
          },
          {
            key: 'images',
            label: `镜像版本 ${imagesApi.data?.total ?? 0}`,
            children: (
              <SectionCard bodyStyle={{ padding: 0 }}>
                <Table<ImageRecord>
                  rowKey="id"
                  size="middle"
                  loading={imagesApi.loading}
                  dataSource={images}
                  columns={imageColumns}
                  scroll={{ x: 1200 }}
                  pagination={{
                    current: imagePage,
                    pageSize: PAGE_SIZE,
                    total: imagesApi.data?.total ?? 0,
                    onChange: setImagePage,
                    showTotal: (t) => `共 ${t} 个镜像版本`,
                  }}
                />
              </SectionCard>
            ),
          },
          {
            key: 'deploys',
            label: `部署情况 ${deployments.length}`,
            children: (
              <SectionCard bodyStyle={{ padding: 0 }}>
                <Table
                  rowKey="id"
                  size="middle"
                  loading={servicesApi.loading}
                  dataSource={deployments}
                  columns={[
                    {
                      title: '环境',
                      dataIndex: 'envKey',
                      width: 140,
                      render: (v, r) => <Link to={`/cd/environments/${v}`}>{r.envName ?? v}</Link>,
                    },
                    { title: '工作负载', dataIndex: 'kind', width: 120 },
                    {
                      title: '当前镜像',
                      dataIndex: 'image',
                      render: (v, r) =>
                        liveOf(r) ? (
                          <ImageText value={liveOf(r)!.image} width={300} />
                        ) : (
                          <span style={{ fontSize: 12, color: '#9aa0aa' }}>{liveFallbackText(r)}</span>
                        ),
                    },
                    {
                      title: '副本',
                      dataIndex: 'replicas',
                      width: 100,
                      // 就绪/期望都取集群实况：拿平台库的 2/2 去描述一个正在掉副本的服务，
                      // 比不显示更糟。
                      render: (v, r) => (liveOf(r) ? `${liveOf(r)!.readyReplicas}/${liveReplicaTotal(liveOf(r)!)}` : '—'),
                    },
                    { title: '状态', dataIndex: 'status', width: 110, render: (v, r) => <DeployStateTag s={r} /> },
                    { title: '最近更新', dataIndex: 'updatedAt', width: 168, render: (v) => <span className="yn-mono" style={{ color: '#8c8c8c' }}>{v}</span> },
                    { title: '更新人', dataIndex: 'updatedBy', width: 100 },
                  ]}
                  pagination={false}
                />
              </SectionCard>
            ),
          },
          {
            // 放在最后：前四个是"这个项目跑成了什么样"，这一项是"它该长什么样"的前置配置，
            // 日常看得少、配置时才会进来。
            key: 'k8s',
            label: 'K8s 资源',
            children: <ProjectManifests projectKey={project.key} />,
          },
        ]}
      />

      {/* 构建日志抽屉 */}
      <Drawer
        open={!!logTarget}
        onClose={() => setLogTarget(null)}
        width={860}
        title={
          <Space size={10}>
            <span>构建日志</span>
            {logTarget ? <span className="yn-mono" style={{ fontSize: 13, color: '#8c8c8c' }}>#{logTarget.buildNo}</span> : null}
            {logTarget ? <BuildStatusTag status={logTarget.status} /> : null}
            {logs.isLive ? <Tag color="processing">实时流式输出中</Tag> : null}
          </Space>
        }
        styles={{ body: { padding: 16, background: '#f4f6f9' } }}
      >
        {logTarget ? (
          <>
            <div style={{ background: '#fff', border: '1px solid #eceef2', borderRadius: 6, padding: '10px 14px', marginBottom: 12, fontSize: 13 }}>
              <Space split={<span style={{ color: '#e3e6ea' }}>|</span>} wrap size={10}>
                <span><Text type="secondary">分支 </Text>{logTarget.branch}</span>
                <span><Text type="secondary">Commit </Text><span className="yn-mono">{logTarget.commitId}</span></span>
                <span><Text type="secondary">触发人 </Text>{logTarget.trigger}</span>
                <span><Text type="secondary">阶段 </Text>{logTarget.stage || '—'}</span>
                <span><Text type="secondary">开始 </Text><span className="yn-mono">{logTarget.startedAt}</span></span>
              </Space>
            </div>
            {logs.error ? (
              <Alert type="warning" showIcon style={{ marginBottom: 12 }} message={logs.error} />
            ) : null}
            <LogPanel
              lines={logs.lines}
              height={460}
              streaming={false}
              live={logs.isLive}
              emptyText={logs.loading ? '正在加载日志…' : '该构建暂无日志'}
              title={`yunnian-ci / ${logTarget.projectKey} #${logTarget.buildNo}`}
            />
          </>
        ) : null}
      </Drawer>
    </div>
  );
};

export default ProjectDetail;
