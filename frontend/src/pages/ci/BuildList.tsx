import React, { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Button,
  Col,
  Drawer,
  Input,
  Progress,
  Row,
  Segmented,
  Select,
  Space,
  Steps,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  BulbOutlined,
  CloseCircleOutlined,
  CopyOutlined,
  FileTextOutlined,
  ReloadOutlined,
  RobotOutlined,
  SearchOutlined,
  StopOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons';
import { Link } from 'react-router-dom';
import { PageHeader, SectionCard, StatCard } from '@/components/ui';
import { LogPanel } from '@/components/LogPanel';
import FailureReason from '@/components/FailureReason';
import { BuildStatusTag, ImageText, ProjectTypeTag } from '@/components/StatusTag';
import { buildApi, projectApi, systemApi } from '@/api';
import { useApi } from '@/hooks/useApi';
import { useJobLogs } from '@/hooks/useJobLogs';
import { humanDuration } from '@/utils/format';
import type { BuildRecord, BuildStatus, ProjectType } from '@/types';

const { Text } = Typography;

/** 处于"未结束"的构建：需要连 SSE 实时日志，而不是读历史日志 */
const isLive = (s: BuildStatus | string) => s === 'running' || s === 'queued';

/**
 * AI 诊断按钮的提示语。
 *
 * 后端 `CIService.BuildAIDiagnosis`（ci.go）对非 failed 一律返回 StateConflict
 * 「仅失败构建支持 AI 归因分析」，所以按钮在成功/取消的行上必须是禁用的。
 * 但**不能因此就不渲染**——原先只在 failed 时出现，结果是手里没有失败构建的人
 * 会认为这个功能根本不存在（"AI 诊断在哪里？我怎么看不到"）。
 * 现在改成常显 + 禁用，并且把"为什么灰着"写在提示里。
 */
const aiDiagTip = (s: BuildStatus | string): string => {
  if (s === 'failed') return 'AI 归因与修复建议';
  if (s === 'success') return '本条构建已成功，无需归因；AI 诊断仅对构建失败时可用';
  if (s === 'canceled') return '本条构建已取消；AI 诊断仅对构建失败时可用';
  // 已中断**必须单列一句**：它不是"构建失败"，没有任何失败原因可归因。
  // 漏掉这一句会掉进最后那句兜底话术「构建结束后，失败时可用 AI 归因」——
  // 对一条已经结束的记录说"构建结束后"，等于告诉用户它还在跑。
  if (s === 'interrupted') return '本条构建因平台重启中断，没有失败原因可归因；可在列表中重新触发';
  return '构建结束后，失败时可用 AI 归因';
};

const BuildList: React.FC = () => {
  const { message, modal } = AntApp.useApp();

  // ---------------------------------------------------------------- 筛选条件
  // 全部走服务端筛选与分页：后端 /builds 支持 keyword / projectKey / projectType /
  // status / branch / page / pageSize / sortBy / order（实测确认）。
  // keyword 在输入框里保持即时反馈，但落成请求要防抖——否则每敲一个字打一次接口。
  const [keywordInput, setKeywordInput] = useState('');
  const [keyword, setKeyword] = useState('');
  const [projectFilter, setProjectFilter] = useState<string | undefined>();
  const [statusFilter, setStatusFilter] = useState<string>('all');
  const [typeFilter, setTypeFilter] = useState<string>('all');
  const [sort, setSort] = useState<{ by: string; order: 'asc' | 'desc' }>({
    by: 'createdAt',
    order: 'desc',
  });
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(12);

  useEffect(() => {
    const t = setTimeout(() => {
      setKeyword(keywordInput.trim());
      setPage(1);
    }, 350);
    return () => clearTimeout(t);
  }, [keywordInput]);

  // 任何一个筛选条件变化都要回到第一页，否则会停在旧结果集的第 3 页看到空列表
  const resetPage = <T,>(setter: (v: T) => void) => (v: T) => {
    setter(v);
    setPage(1);
  };

  // ------------------------------------------------------------------ 取数
  const statsApi = useApi(() => systemApi.stats(), []);
  const stats = statsApi.data;

  const projectsApi = useApi(() => projectApi.list({ pageSize: 200 }), []);
  const projects = useMemo(() => projectsApi.data?.list ?? [], [projectsApi.data]);

  const buildsApi = useApi(
    () =>
      buildApi.list({
        keyword,
        projectKey: projectFilter,
        projectType: typeFilter === 'all' ? undefined : typeFilter,
        status: statusFilter === 'all' ? undefined : statusFilter,
        page,
        pageSize,
        sortBy: sort.by,
        order: sort.order,
      }),
    [keyword, projectFilter, typeFilter, statusFilter, page, pageSize, sort.by, sort.order],
  );

  // "正在构建"面板：running + queued 一次取回（后端 status 支持逗号分隔多值）
  const liveApi = useApi(() => buildApi.list({ status: 'running,queued', pageSize: 20 }), []);
  const live = useMemo(() => liveApi.data?.list ?? [], [liveApi.data]);

  const dataSource = buildsApi.data?.list ?? [];
  const total = buildsApi.data?.total ?? 0;

  const refreshAll = () => {
    buildsApi.refresh();
    liveApi.refresh();
    statsApi.refresh();
  };

  // ------------------------------------------------------------ 日志抽屉
  const [logTarget, setLogTarget] = useState<BuildRecord | null>(null);
  const logs = useJobLogs({
    id: logTarget?.id ?? null,
    live: !!logTarget && isLive(logTarget.status),
    stream: buildApi.stream,
    fetchHistory: buildApi.logs,
  });
  const logLines = logs.lines;

  // 实时流结束（构建跑完）时把列表与统计刷一遍，状态标签才会从"构建中"变成终态
  const streamEnded = logs.ended;
  useEffect(() => {
    if (!streamEnded) return;
    refreshAll();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [streamEnded]);

  // ------------------------------------------------------------ AI 诊断
  const [aiTarget, setAiTarget] = useState<BuildRecord | null>(null);
  const aiApi = useApi(() => buildApi.aiDiagnosis(aiTarget!.id), [aiTarget?.id], {
    enabled: !!aiTarget,
  });

  // ------------------------------------------------------------------ 操作
  const cancelBuild = (r: BuildRecord) =>
    modal.confirm({
      title: `确认取消构建 #${r.buildNo}？`,
      content: '取消后已产生的构建缓存会保留，可稍后重新触发。',
      okText: '确认取消',
      okButtonProps: { danger: true },
      onOk: async () => {
        await buildApi.cancel(r.id);
        message.success(`已取消构建 #${r.buildNo}`);
        refreshAll();
      },
    });

  // 这里刻意没有"重试"：一期纳管的项目全部是 Jenkins 构建，平台不会代为执行
  // docker 构建（后端对这类项目直接返回 40901）。留一个点了必然报错的按钮
  // 就是假承诺——要重跑请到 Jenkins 上触发那个 Job。

  // ------------------------------------------------------------------ 表格
  const columns: ColumnsType<BuildRecord> = [
    {
      title: '构建号',
      dataIndex: 'buildNo',
      width: 96,
      fixed: 'left',
      sorter: true,
      render: (v, r) => (
        <Link to={`/ci/projects/${r.projectKey}?tab=builds`} className="yn-mono">
          #{v}
        </Link>
      ),
    },
    {
      title: '项目',
      dataIndex: 'projectName',
      width: 190,
      render: (v, r) => (
        <div>
          <Space size={6}>
            <Link to={`/ci/projects/${r.projectKey}`} style={{ fontWeight: 500 }}>
              {v}
            </Link>
            <ProjectTypeTag type={r.projectType as ProjectType} />
          </Space>
          <div className="yn-mono" style={{ fontSize: 12, color: '#9aa0aa', marginTop: 2 }}>{r.projectKey}</div>
        </div>
      ),
    },
    {
      title: '分支 / Commit',
      dataIndex: 'branch',
      width: 230,
      render: (v, r) => (
        <div>
          <Tag style={{ marginInlineEnd: 0 }}>{v}</Tag>
          <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 3 }}>
            <span className="yn-mono">{r.commitId}</span> · {(r.commitMsg || '').slice(0, 18)}
            {(r.commitMsg || '').length > 18 ? '…' : ''}
          </div>
        </div>
      ),
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 150,
      render: (v, r) => (
        <div>
          <BuildStatusTag status={v} />
          {v === 'running' ? (
            <Progress percent={r.progress} size="small" style={{ width: 108, marginTop: 4 }} strokeColor="#1677ff" />
          ) : null}
          {v === 'queued' ? (
            <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 3 }}>{r.stage}</div>
          ) : null}
          {/* 失败/中断把真因直接摆在状态列下：这是用户看到这一行时的第一个疑问，
              藏进抽屉等于逼他多点一次。`stage` 在非 live 状态下是过时进度（如"编译构建"），
              拿它当失败原因会误导，所以这里只认真因/错误码。 */}
          {v === 'failed' || v === 'interrupted' ? (
            <FailureReason message={r.errorMessage} code={r.errorCode} tone={v === 'interrupted' ? 'warning' : 'error'} />
          ) : null}
        </div>
      ),
    },
    {
      title: '镜像版本',
      dataIndex: 'image',
      width: 300,
      // 失败的构建没有产物，这里留空比显示 "-" 更能说明问题
      render: (v) => (v ? <ImageText value={v} width={290} /> : <Text type="secondary">—</Text>),
    },
    {
      title: '耗时',
      dataIndex: 'duration',
      width: 92,
      sorter: true,
      /* `|| !v` 与 DeployList / RollbackList 的耗时列取同一口径。
         不加这个兜底，一条**从未开跑**就被平台重启中断的构建会显示「0s」——
         它原本是 `queued`（live，显示「—」），重启后被标成 `interrupted`，
         同一格就变成了「0s」，把「没跑过」说成了「跑了 0 秒」。
         `duration` 为 0 的合法情形只有「不到一秒就跑完」，那种情况显示「—」也不失真。 */
      render: (v, r) => (isLive(r.status) || !v ? <Text type="secondary">—</Text> : humanDuration(v)),
    },
    {
      title: '触发',
      dataIndex: 'trigger',
      width: 130,
      render: (v, r) => (
        <div>
          <div style={{ fontSize: 13 }}>{v}</div>
          <div style={{ fontSize: 12, color: '#9aa0aa' }}>
            {r.triggerType === 'webhook' ? 'Webhook' : r.triggerType === 'schedule' ? '定时' : '手动'}
          </div>
        </div>
      ),
    },
    {
      title: '开始时间',
      dataIndex: 'startedAt',
      width: 168,
      sorter: true,
      render: (v) => <span className="yn-mono" style={{ color: '#8c8c8c' }}>{v}</span>,
    },
    {
      title: '操作',
      key: 'action',
      fixed: 'right',
      // 176：终态行要同时容纳「日志 + AI 诊断」，148 会把 AI 诊断挤到换行
      width: 176,
      render: (_, r) => (
        <Space size={2}>
          <Button type="link" size="small" icon={<FileTextOutlined />} onClick={() => setLogTarget(r)} style={{ paddingInline: 4 }}>
            日志
          </Button>
          {/* AI 诊断：终态行常显（失败可点，其余禁用并说明原因）。
              隐藏入口的代价比"看着灰"大得多——见 aiDiagTip 的注释。 */}
          {!isLive(r.status) ? (
            <Tooltip title={aiDiagTip(r.status)}>
              {/* disabled 的 antd 按钮不派发鼠标事件，Tooltip 必须套一层容器，
                  否则恰好是"灰着的那几行"没有悬停提示——最需要解释的反而是它们。 */}
              <span style={{ display: 'inline-block', verticalAlign: 'middle' }}>
                <Button
                  type="link"
                  size="small"
                  icon={<RobotOutlined />}
                  disabled={r.status !== 'failed'}
                  onClick={() => setAiTarget(r)}
                  style={{ paddingInline: 4 }}
                >
                  AI 诊断
                </Button>
              </span>
            </Tooltip>
          ) : null}
          {/* 只有运行中/排队中才有"取消"，终态不再提供任何操作入口 */}
          {isLive(r.status) ? (
            <Button
              type="link"
              size="small"
              danger
              icon={<StopOutlined />}
              style={{ paddingInline: 4 }}
              onClick={() => cancelBuild(r)}
            >
              取消
            </Button>
          ) : null}
        </Space>
      ),
    },
  ];

  const pageError = buildsApi.error || statsApi.error || liveApi.error;

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: 'CI 镜像构建' }, { title: '构建记录' }]}
        title="构建记录"
        description="全部项目的构建流水，支持查看实时日志与失败归因"
        extra={
          <Space>
            <Button icon={<ReloadOutlined />} onClick={refreshAll} loading={buildsApi.loading} />
            <Button type="primary" icon={<ThunderboltOutlined />} onClick={() => message.info('请前往「项目管理」选择项目触发编译')}>
              新建构建
            </Button>
          </Space>
        }
      />

      {pageError ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message="构建数据加载失败"
          description={pageError.message}
          action={<Button size="small" onClick={refreshAll}>重试</Button>}
        />
      ) : null}

      <Row gutter={[16, 16]}>
        <Col xs={12} xl={6}>
          <StatCard title="今日构建" value={stats?.todayBuilds ?? '—'} unit="次" icon={<ThunderboltOutlined />} />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="构建成功"
            value={stats?.todaySuccess ?? '—'}
            unit="次"
            accent="#52c41a"
            footer={<Text type="secondary" style={{ fontSize: 12 }}>成功率 {stats?.successRate ?? '—'}%</Text>}
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="构建失败"
            value={stats?.todayFailed ?? '—'}
            unit="次"
            accent="#ff4d4f"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                <RobotOutlined /> 点开失败记录可看 AI 归因
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="进行中"
            value={live.length}
            unit="个"
            accent="#1677ff"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                {live[0] ? `${live[0].projectName} · ${live[0].stage || '排队中'}` : '暂无排队任务'}
              </Text>
            }
          />
        </Col>
      </Row>

      {live.length ? (
        <SectionCard title="正在构建" style={{ marginTop: 16 }} bodyStyle={{ padding: '12px 20px' }}>
          {live.map((b) => (
            <div key={b.id} style={{ display: 'flex', alignItems: 'center', gap: 14, padding: '8px 0' }}>
              <span style={{ width: 168, fontWeight: 500 }}>{b.projectName}</span>
              <Tag style={{ marginInlineEnd: 0 }}>{b.branch}</Tag>
              <span className="yn-mono" style={{ fontSize: 12, color: '#9aa0aa' }}>{b.commitId}</span>
              <Progress percent={b.progress} size="small" style={{ flex: 1, maxWidth: 320 }} strokeColor="#1677ff" />
              <Text type="secondary" style={{ fontSize: 12, width: 110 }}>{b.stage || '排队中'}</Text>
              <Button type="link" size="small" onClick={() => setLogTarget(b)} icon={<FileTextOutlined />}>
                实时日志
              </Button>
            </div>
          ))}
        </SectionCard>
      ) : null}

      <SectionCard style={{ marginTop: 16 }} bodyStyle={{ padding: 0 }}>
        <div className="yn-filter-bar">
          <Space size={12} wrap>
            <Input
              allowClear
              prefix={<SearchOutlined style={{ color: '#b5bac4' }} />}
              placeholder="搜索项目 / Commit 号 / Commit 信息"
              style={{ width: 270 }}
              value={keywordInput}
              onChange={(e) => setKeywordInput(e.target.value)}
            />
            <Select
              allowClear
              showSearch
              placeholder="选择项目"
              style={{ width: 190 }}
              value={projectFilter}
              onChange={resetPage(setProjectFilter)}
              optionFilterProp="label"
              options={projects.map((p) => ({ label: p.name, value: p.key }))}
            />
            <Segmented
              value={typeFilter}
              onChange={(v) => {
                setTypeFilter(v as string);
                setPage(1);
              }}
              options={[
                { label: '全部类型', value: 'all' },
                { label: '前端', value: 'frontend' },
                { label: '后端', value: 'backend' },
              ]}
            />
          </Space>
          <Select
            style={{ width: 150 }}
            value={statusFilter}
            onChange={(v) => {
              setStatusFilter(v);
              setPage(1);
            }}
            options={[
              { label: '全部状态', value: 'all' },
              { label: '成功', value: 'success' },
              { label: '失败', value: 'failed' },
              { label: '构建中', value: 'running' },
              { label: '排队中', value: 'queued' },
              { label: '已取消', value: 'canceled' },
              // 已中断单列：平台重启打断的构建既不是失败也不是取消，
              // 想找"重启时丢了哪些构建"的人只能靠这一档筛出来。
              { label: '已中断', value: 'interrupted' },
            ]}
          />
        </div>

        <Table<BuildRecord>
          rowKey="id"
          size="middle"
          loading={buildsApi.loading}
          dataSource={dataSource}
          columns={columns}
          scroll={{ x: 1720 }}
          onChange={(_p, _f, sorter) => {
            // ⚠️ 点击分页**也会**走到这里 —— AntD 把分页变化一并上报给 Table.onChange。
            // 若无条件 setPage(1)，会抹掉 pagination.onChange 刚设好的页码：
            // 两个 setState 合批后后写的赢，page 1→2→1，依赖没变化，
            // 连重新请求都不会发 —— 表现就是「点第 2 页没反应」（实测踩到）。
            // 所以只有页码没变（即本次变化来自表头排序）时才回第 1 页。
            if (_p.current !== page) return;
            const s = Array.isArray(sorter) ? sorter[0] : sorter;
            setSort({
              by: (s?.columnKey as string) || (s?.field as string) || 'createdAt',
              order: s?.order === 'ascend' ? 'asc' : 'desc',
            });
            setPage(1);
          }}
          pagination={{
            current: page,
            pageSize,
            total,
            showSizeChanger: true,
            showTotal: (t) => `共 ${t} 条构建记录`,
            onChange: (p, ps) => {
              setPage(p);
              setPageSize(ps);
            },
          }}
        />
      </SectionCard>

      {/* -------------------------------------------------- 实时日志抽屉 */}
      <Drawer
        open={!!logTarget}
        onClose={() => setLogTarget(null)}
        width={900}
        title={
          <Space size={10}>
            <span>构建日志</span>
            {logTarget ? <span className="yn-mono" style={{ fontSize: 13, color: '#8c8c8c' }}>#{logTarget.buildNo}</span> : null}
            {logTarget ? <BuildStatusTag status={logTarget.status} /> : null}
            {logs.isLive ? <Tag color="processing">实时流式输出中</Tag> : null}
          </Space>
        }
        extra={
          logLines.length ? (
            <Button
              size="small"
              icon={<CopyOutlined />}
              onClick={() => {
                navigator.clipboard?.writeText(
                  logLines.map((l) => `${(l.ts || '').slice(11, 19)}  ${l.line}`).join('\n'),
                );
                message.success('日志已复制');
              }}
            >
              复制日志
            </Button>
          ) : null
        }
        styles={{ body: { padding: 16, background: '#f4f6f9' } }}
      >
        {logTarget ? (
          <>
            {logTarget.status === 'interrupted' ? (
              /* 中断的构建**不渲染 Steps**：那个控件回答的是"走到哪一步了"，
                 而它的取值是一串从 status 猜出来的硬编码下标（下面的三元链）。
                 硬套会让一条中断在拉代码阶段的构建显示成"编译构建已完成"——
                 我们没有任何依据支撑这个进度。宁可整块换成一句实话。 */
              <Alert
                type="warning"
                showIcon
                style={{ marginBottom: 12 }}
                message="该构建因平台重启而中断，未执行完"
                description="平台重启会一并清空正在执行的构建队列，这条记录不会自行恢复；下方日志截止到中断时刻。需要它跑完，请在列表中重新触发。"
              />
            ) : (
              <SectionCard bodyStyle={{ padding: '16px 20px' }} style={{ marginBottom: 12 }}>
                <Steps
                  size="small"
                  current={
                    logTarget.status === 'success'
                      ? 4
                      : logTarget.status === 'failed'
                        ? 3
                        : logTarget.status === 'queued'
                          ? 0
                          : 2
                  }
                  status={logTarget.status === 'failed' ? 'error' : logTarget.status === 'running' ? 'process' : 'finish'}
                  items={[
                    { title: '排队调度' },
                    { title: '拉取代码' },
                    { title: '编译构建' },
                    { title: '推送镜像' },
                    { title: '完成' },
                  ]}
                />
              </SectionCard>
            )}

            {logs.error ? (
              <Alert type="warning" showIcon style={{ marginBottom: 12 }} message={logs.error} />
            ) : null}

            {logTarget.status === 'failed' && logTarget.errorMessage ? (
              /* 真因直接铺在日志上方：用户点「日志」就是想问"为什么失败"，
                 让他自己在几百行里找那条 error，等于把我们刚记下来的东西又藏回去。 */
              <Alert
                type="error"
                showIcon
                style={{ marginBottom: 12 }}
                message={`失败原因${logTarget.errorCode ? `（${logTarget.errorCode}）` : ''}`}
                description={<span style={{ whiteSpace: 'pre-wrap' }}>{logTarget.errorMessage}</span>}
              />
            ) : null}
            <LogPanel
              lines={logLines}
              height={430}
              streaming={false}
              live={logs.isLive}
              emptyText={logs.loading ? '正在加载日志…' : '该构建暂无日志'}
              title={`yunnian-ci / ${logTarget.projectKey} #${logTarget.buildNo}`}
            />
          </>
        ) : null}
      </Drawer>

      {/* -------------------------------------------------- AI 诊断抽屉 */}
      <Drawer
        open={!!aiTarget}
        onClose={() => setAiTarget(null)}
        width={560}
        title={
          <Space>
            <BulbOutlined style={{ color: '#fa8c16' }} />
            AI 构建诊断
          </Space>
        }
      >
        {aiTarget ? (
          <>
            <SectionCard title="构建概要" bodyStyle={{ padding: '14px 20px' }}>
              <div style={{ fontSize: 13, lineHeight: 2 }}>
                <div><Text type="secondary">项目：</Text>{aiTarget.projectName}（<span className="yn-mono">{aiTarget.projectKey}</span>）</div>
                <div><Text type="secondary">构建号：</Text><span className="yn-mono">#{aiTarget.buildNo}</span></div>
                <div><Text type="secondary">分支：</Text>{aiTarget.branch} <span className="yn-mono" style={{ color: '#9aa0aa' }}>{aiTarget.commitId}</span></div>
                <div><Text type="secondary">失败阶段：</Text><Tag color="red">{aiTarget.stage || '构建'}</Tag></div>
                {/* 「阶段」是进度词（"编译构建"），不是原因；这一行才是"为什么失败"。
                    落这条列之前产生的旧记录没有存下真因，这里如实说明，不留白。 */}
                <div>
                  <Text type="secondary">失败原因：</Text>
                  {aiTarget.errorMessage ? (
                    <span style={{ color: '#cf1322' }}>{aiTarget.errorMessage}</span>
                  ) : (
                    <Text type="secondary">未记录（{aiTarget.errorCode || '无错误码'}）</Text>
                  )}
                </div>
                <div><Text type="secondary">耗时：</Text>{humanDuration(aiTarget.duration)}</div>
              </div>
            </SectionCard>

            <SectionCard title="归因分析" style={{ marginTop: 16 }} bodyStyle={{ padding: '16px 20px' }}>
              {aiApi.loading ? (
                <Text type="secondary">正在分析构建日志…</Text>
              ) : aiApi.error ? (
                // 这里只剩"接口本身失败"（如构建并非失败态、后端异常）。
                // 模型通道不可用**不会**走到这里——后端会降级为错误码规则结论，
                // 并把原因放进 notice 一并返回，所以别再提示"去模型设置填 Key 后重试"：
                // 那个动作既不会让本页报错消失，也不影响结论是否正确。
                <Alert type="warning" showIcon message="AI 诊断不可用" description={aiApi.error.message} />
              ) : aiApi.data ? (
                <Space direction="vertical" size={14} style={{ width: '100%' }}>
                  <div className="yn-suggest-card">
                    <div className="yn-suggest-card__head">
                      <CloseCircleOutlined /> 可能原因
                    </div>
                    <div className="yn-suggest-card__row">{aiApi.data.reason}</div>
                  </div>
                  <div className="yn-suggest-card">
                    <div className="yn-suggest-card__head">
                      <RunIcon /> 建议操作
                    </div>
                    <div className="yn-suggest-card__row">{aiApi.data.suggestion}</div>
                  </div>
                  {/* 来源写在明面上：这段结论可能是模型读了日志得出的，也可能是平台错误码
                      查表查出来的。两者长得一样，用户就会把查表结果当成模型分析。 */}
                  <div>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      {aiApi.data.source === 'model'
                        ? `由模型通道「${aiApi.data.model || '默认通道'}」阅读构建日志生成 · 置信度 ${(
                            aiApi.data.confidence * 100
                          ).toFixed(0)}%`
                        : `平台错误码规则匹配 · 置信度 ${(aiApi.data.confidence * 100).toFixed(0)}%`}
                    </Text>
                    {aiApi.data.notice ? (
                      <div style={{ marginTop: 4 }}>
                        <Text type="warning" style={{ fontSize: 12 }}>
                          {aiApi.data.notice}
                        </Text>
                      </div>
                    ) : null}
                  </div>
                </Space>
              ) : null}
            </SectionCard>

            <Space style={{ marginTop: 16 }}>
              <Button
                icon={<FileTextOutlined />}
                onClick={() => {
                  setLogTarget(aiTarget);
                  setAiTarget(null);
                }}
              >
                查看完整日志
              </Button>
            </Space>
          </>
        ) : null}
      </Drawer>
    </div>
  );
};

const RunIcon = () => <ThunderboltOutlined />;

export default BuildList;
