import React, { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Button,
  Col,
  Drawer,
  Input,
  Modal,
  Progress,
  Row,
  Segmented,
  Select,
  Space,
  Steps,
  Table,
  Tag,
  Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  CheckOutlined,
  CloseOutlined,
  CopyOutlined,
  FileTextOutlined,
  ReloadOutlined,
  RobotOutlined,
  SafetyCertificateOutlined,
  SearchOutlined,
  StopOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons';
import { Link, useNavigate } from 'react-router-dom';
import { PageHeader, SectionCard, StatCard } from '@/components/ui';
import { LogPanel } from '@/components/LogPanel';
import { DisabledTip } from '@/components/DisabledTip';
import FailureReason from '@/components/FailureReason';
import { DeployStatusTag, ImageText } from '@/components/StatusTag';
import { deployApi, environmentApi, rollbackApi, systemApi } from '@/api';
import { canDeployOn, hasPerm } from '@/auth';
import { useApi } from '@/hooks/useApi';
import { useJobLogs } from '@/hooks/useJobLogs';
import { humanDuration } from '@/utils/format';
import type { DeployRecord, DeployStatus } from '@/types';

const { Text } = Typography;

/** 仍在进行中的部署：需要连 SSE 实时日志而不是读历史 */
const isLive = (s: DeployStatus | string) => s === 'progressing' || s === 'queued';

/** 各环境在状态列表里的配色（沿用原型期的语义化配色） */
const envTagColor = (envKey: string) =>
  envKey === 'prod' ? 'red' : envKey === 'staging' ? 'orange' : envKey === 'canary' ? 'purple' : 'blue';

/** 触发方式筛选值：后端 deploy 列表不带这个字段，只能按"有无回滚单"等外部线索…… 
 *  实际后端 /deployments 查询没有 triggerType，所以这里不提供该筛选项，避免假控件。 */

const DeployList: React.FC = () => {
  const navigate = useNavigate();
  const { message, modal, notification } = AntApp.useApp();
  const canApprove = hasPerm('deploy:approve');
  // AI 诊断接口 GET /deployments/:id/ai-diagnosis 在路由层要求 ai:query
  // （internal/api/router.go）。演示账号 ops 没有该权限点，不门控的话点下去必 40300。
  const canAiQuery = hasPerm('ai:query');

  // ---------------------------------------------------------------- 筛选条件
  const [keywordInput, setKeywordInput] = useState('');
  const [keyword, setKeyword] = useState('');
  const [envFilter, setEnvFilter] = useState<string | undefined>();
  const [statusFilter, setStatusFilter] = useState<string>('all');
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(12);

  useEffect(() => {
    const t = setTimeout(() => {
      setKeyword(keywordInput.trim());
      setPage(1);
    }, 350);
    return () => clearTimeout(t);
  }, [keywordInput]);

  // ------------------------------------------------------------------ 取数
  const envsApi = useApi(() => environmentApi.list({ pageSize: 100 }), []);
  const envs = useMemo(() => envsApi.data?.list ?? [], [envsApi.data]);

  const deploysApi = useApi(
    () =>
      deployApi.list({
        keyword,
        envKey: envFilter,
        status: statusFilter === 'all' ? undefined : statusFilter,
        page,
        pageSize,
      }),
    [keyword, envFilter, statusFilter, page, pageSize],
  );
  const dataSource = deploysApi.data?.list ?? [];
  const total = deploysApi.data?.total ?? 0;

  const statsApi = useApi(() => systemApi.stats(), []);
  const stats = statsApi.data;

  // 成功 / 失败次数：后端没有单独的聚合接口，用 pageSize=1 借 total 拿计数。
  // 比把全量记录拉到前端再数要省得多，也不需要为此加接口。
  const successApi = useApi(() => deployApi.list({ status: 'success', pageSize: 1 }), []);
  const failedApi = useApi(() => deployApi.list({ status: 'failed', pageSize: 1 }), []);

  const refreshAll = () => {
    deploysApi.refresh();
    statsApi.refresh();
    successApi.refresh();
    failedApi.refresh();
  };

  // ------------------------------------------------------------ 日志抽屉
  const [logTarget, setLogTarget] = useState<DeployRecord | null>(null);
  const logs = useJobLogs({
    id: logTarget?.id ?? null,
    live: !!logTarget && isLive(logTarget.status),
    stream: deployApi.stream,
    fetchHistory: deployApi.logs,
  });

  // 实时流正常结束 → 部署已进终态，把列表与统计刷一遍
  useEffect(() => {
    if (!logs.ended) return;
    refreshAll();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [logs.ended]);

  // 日志抽屉里那条 kubectl 命令的命名空间：必须是环境真实字段（形如 yunnian-perf-sh），
  // 不能拿 envKey 拼 `yunnian-{envKey}`——那跟集群里实际的 namespace 对不上。
  // 环境列表里查不到就不显示这条命令（宁可不给，也不给一条错的）。
  const logNamespace = logTarget ? envs.find((e) => e.key === logTarget.envKey)?.namespace : undefined;

  // ------------------------------------------------------------ AI 诊断
  const [aiTarget, setAiTarget] = useState<DeployRecord | null>(null);
  const aiApi = useApi(() => deployApi.aiDiagnosis(aiTarget!.id), [aiTarget?.id], { enabled: !!aiTarget });

  // ------------------------------------------------------------ 审批/操作
  const [approveTarget, setApproveTarget] = useState<DeployRecord | null>(null);
  const [approveRemark, setApproveRemark] = useState('');
  const [rejecting, setRejecting] = useState(false);

  const submitApprove = async () => {
    if (!approveTarget) return;
    await deployApi.approve(approveTarget.id, approveRemark || undefined);
    notification.success({
      message: '已通过审批',
      description: `${approveTarget.deployNo} 已进入部署队列。`,
    });
    setApproveTarget(null);
    setApproveRemark('');
    refreshAll();
  };

  const submitReject = async () => {
    if (!approveTarget) return;
    if (!approveRemark.trim()) {
      message.warning('驳回必须填写原因');
      return;
    }
    setRejecting(true);
    try {
      await deployApi.reject(approveTarget.id, approveRemark.trim());
      message.success(`已驳回 ${approveTarget.deployNo}`);
      setApproveTarget(null);
      setApproveRemark('');
      refreshAll();
    } finally {
      setRejecting(false);
    }
  };

  const cancelDeploy = (r: DeployRecord) =>
    modal.confirm({
      title: `确认取消部署 ${r.deployNo}？`,
      content: '已完成的步骤不会回退，已更新的工作负载需手动回滚。',
      okText: '确认取消',
      okButtonProps: { danger: true },
      onOk: async () => {
        await deployApi.cancel(r.id);
        message.success(`已取消 ${r.deployNo}`);
        refreshAll();
      },
    });

  const retryDeploy = async (r: DeployRecord) => {
    await deployApi.retry(r.id);
    message.success(`已重新提交 ${r.deployNo}`);
    refreshAll();
  };

  /** 从诊断结果一键回滚：诊断接口已经把目标服务与目标镜像都算好了 */
  const rollbackByDiagnosis = (r: DeployRecord) => {
    // 成功单现在也能进这个抽屉（去看回滚目标），所以措辞不能写死"AI 建议"——
    // 那种情况下后端给的建议是"无需回滚"，照搬会自相矛盾。
    const byDiagnosis = r.status === 'failed';
    return modal.confirm({
      title: byDiagnosis ? `按 AI 建议回滚 ${r.projectKey}？` : `回滚 ${r.projectKey} 到上一版？`,
      content: (
        <div style={{ fontSize: 13, lineHeight: 1.9 }}>
          <div>目标版本：<span className="yn-mono">{aiApi.data?.rollbackImage || '—'}</span></div>
          <div style={{ color: '#8c8c8c' }}>
            {byDiagnosis ? aiApi.data?.suggestion : '回滚到该服务在此环境的上一个稳定版本'}
          </div>
        </div>
      ),
      okText: '确认回滚',
      okButtonProps: { danger: true },
      onOk: async () => {
        const d = aiApi.data;
        if (!d?.rollbackAvailable || !d.serviceId) {
          message.warning('当前没有可回滚的目标版本');
          return;
        }
        await rollbackApi.create({
          serviceId: d.serviceId,
          toImage: d.rollbackImage,
          reason: byDiagnosis ? `AI 建议回滚：${d.reason}` : '按诊断结果回滚到上一稳定版本',
          triggerType: 'ai_suggested',
          confidence: d.confidence,
        });
        notification.success({ message: '回滚任务已提交', description: `正在将 ${r.projectKey} 回滚至上一稳定版本。` });
        refreshAll();
      },
    });
  };

  // ------------------------------------------------------------------ 表格
  const columns: ColumnsType<DeployRecord> = [
    {
      title: '部署单号',
      dataIndex: 'deployNo',
      width: 156,
      fixed: 'left',
      render: (v) => <span className="yn-mono">{v}</span>,
    },
    {
      title: '环境',
      dataIndex: 'envName',
      width: 130,
      render: (v, r) => (
        <Link to={`/cd/environments/${r.envKey}?tab=deploys`}>
          <Tag color={envTagColor(r.envKey)} style={{ marginInlineEnd: 0 }}>
            {v}
          </Tag>
        </Link>
      ),
    },
    {
      title: '服务',
      dataIndex: 'projectKey',
      width: 170,
      render: (v, r) => (
        <div>
          <Link to={`/ci/projects/${v}`} style={{ fontWeight: 500 }}>
            {v}
          </Link>
          <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 2 }}>{r.projectName}</div>
        </div>
      ),
    },
    { title: '镜像版本', dataIndex: 'image', width: 300, render: (v) => <ImageText value={v} width={290} /> },
    {
      title: '状态',
      dataIndex: 'status',
      width: 170,
      render: (v, r) => (
        <div>
          <DeployStatusTag status={v} />
          {v === 'progressing' ? (
            <Progress percent={r.progress} size="small" style={{ width: 118, marginTop: 4 }} strokeColor="#1677ff" />
          ) : null}
          {/* 失败/中断直接把真因摆在状态列下。与 BuildList 同一口径：
              「部署失败不回滚」之后现场只剩"集群停在新镜像"，这条真因是唯一的解释，
              藏进抽屉等于逼用户多点一次才能知道发生了什么。 */}
          {v === 'failed' || v === 'interrupted' ? (
            <FailureReason message={r.errorMessage} code={r.errorCode} tone={v === 'interrupted' ? 'warning' : 'error'} />
          ) : null}
        </div>
      ),
    },
    { title: '副本', dataIndex: 'replicas', width: 78 },
    { title: '操作人', dataIndex: 'operator', width: 96 },
    {
      title: '审批人',
      dataIndex: 'approver',
      width: 100,
      render: (v) => v ?? <Text type="secondary">—</Text>,
    },
    {
      title: '耗时',
      dataIndex: 'duration',
      width: 92,
      render: (v, r) => (isLive(r.status) || !v ? <Text type="secondary">—</Text> : humanDuration(v)),
    },
    {
      title: '开始时间',
      dataIndex: 'startedAt',
      width: 168,
      render: (v) => <span className="yn-mono" style={{ color: '#8c8c8c' }}>{v}</span>,
    },
    {
      title: '操作',
      key: 'action',
      fixed: 'right',
      width: 210,
      render: (_, r) => (
        <Space size={2}>
          <Button
            type="link"
            size="small"
            icon={<FileTextOutlined />}
            style={{ paddingInline: 4 }}
            onClick={() => setLogTarget(r)}
          >
            日志
          </Button>
          {/* AI 诊断：终态行常显且可点。
              后端 DeploymentAIDiagnosis（cd.go）对非失败单**也受理**——返回的是
              "该部署单当前没有异常 / 无需回滚"，并在有上一版时给出可回滚目标。
              所以这里不是放开门控，而是把本来就存在的能力露出来；而且部署成功
              但想退回上一版时，正是从这儿看回滚目标最直接。
              （CI 构建那边不同：BuildAIDiagnosis 只受理 failed，那边必须禁用。）
              但接口路由要求 ai:query，缺这个权限点的账号（如演示账号 ops）点了必 403，
              故按权限禁用并说明——状态口径（终态、非待审批）保持不变。 */}
          {!isLive(r.status) && r.status !== 'pending_approval' ? (
            <DisabledTip title={canAiQuery ? undefined : '需要 ai:query 权限'}>
              <Button
                type="link"
                size="small"
                icon={<RobotOutlined />}
                style={{ paddingInline: 4 }}
                disabled={!canAiQuery}
                onClick={() => setAiTarget(r)}
              >
                AI 诊断
              </Button>
            </DisabledTip>
          ) : null}
          {r.status === 'pending_approval' ? (
            <DisabledTip title={canApprove ? undefined : '当前账号没有 deploy:approve 权限'}>
              <Button
                type="link"
                size="small"
                icon={<SafetyCertificateOutlined />}
                style={{ paddingInline: 4 }}
                disabled={!canApprove}
                onClick={() => {
                  setApproveTarget(r);
                  setApproveRemark('');
                }}
              >
                审批
              </Button>
            </DisabledTip>
          ) : r.status === 'failed' || r.status === 'interrupted' ? (
            /* `interrupted`（平台重启时中断）与 failed 共用「重试」：两者在用户眼里
               都是"这单没成，原样再来一次"，后端 RetryDeployment 也已明确放行这个状态。
               没有重试入口的话，这单在界面上就是个死结 —— 既不能取消（不是 live），
               也不能审批（不在待审批），只能眼看着它永远停在「已中断」。 */
            <DisabledTip title={canDeployOn(r.envKey) ? undefined : `当前账号没有 deploy:${r.envKey} 权限`}>
              <Button
                type="link"
                size="small"
                icon={<ReloadOutlined />}
                style={{ paddingInline: 4 }}
                disabled={!canDeployOn(r.envKey)}
                onClick={() => retryDeploy(r)}
              >
                重试
              </Button>
            </DisabledTip>
          ) : isLive(r.status) ? (
            /* 取消：后端 handler 用 guardDeployEnv 校验 deploy:{env}（40300），
               此前只按状态放开、任何有 deploy:read 的人都能点，补上门控。 */
            <DisabledTip title={canDeployOn(r.envKey) ? undefined : `当前账号没有 deploy:${r.envKey} 权限`}>
              <Button
                type="link"
                size="small"
                danger
                icon={<StopOutlined />}
                style={{ paddingInline: 4 }}
                disabled={!canDeployOn(r.envKey)}
                onClick={() => cancelDeploy(r)}
              >
                取消
              </Button>
            </DisabledTip>
          ) : null}
        </Space>
      ),
    },
  ];

  const pageError = deploysApi.error || envsApi.error;

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: 'CD 环境部署' }, { title: '部署记录' }]}
        title="部署记录"
        description="全部环境的部署流水，正式区部署需审批；异常部署附带 AI 诊断与建议"
        extra={
          <Space>
            <Button icon={<ReloadOutlined />} loading={deploysApi.loading} onClick={refreshAll} />
            {/* 「新建部署」原来只弹一句提示、什么都不做，是个死入口。
                部署必须在某个环境里选服务发起，这里直接跳到环境管理列表。 */}
            <Button type="primary" icon={<ThunderboltOutlined />} onClick={() => navigate('/cd/environments')}>
              新建部署
            </Button>
          </Space>
        }
      />

      {pageError ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message="部署数据加载失败"
          description={pageError.message}
          action={
            <Button size="small" onClick={refreshAll}>
              重试
            </Button>
          }
        />
      ) : null}

      <Row gutter={[16, 16]}>
        <Col xs={12} xl={6}>
          <StatCard title="部署总数" value={stats?.totalDeploys ?? '—'} unit="次" />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="部署成功"
            value={successApi.data?.total ?? '—'}
            unit="次"
            accent="#52c41a"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                {successApi.data && successApi.data.total + (failedApi.data?.total ?? 0) > 0
                  ? `成功率 ${Math.round((successApi.data.total / (successApi.data.total + (failedApi.data?.total ?? 0))) * 100)}%`
                  : '暂无已结束的部署'}
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard title="部署失败" value={failedApi.data?.total ?? '—'} unit="次" accent="#ff4d4f" />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="待审批"
            value={stats?.pendingApproval ?? '—'}
            unit="单"
            accent="#faad14"
            footer={<Text type="secondary" style={{ fontSize: 12 }}>正式区部署需审批人确认</Text>}
          />
        </Col>
      </Row>

      <SectionCard style={{ marginTop: 16 }} bodyStyle={{ padding: 0 }}>
        <div className="yn-filter-bar">
          <Space size={12} wrap>
            <Input
              allowClear
              prefix={<SearchOutlined style={{ color: '#b5bac4' }} />}
              placeholder="搜索部署单号 / 服务 / 镜像 / 操作人"
              style={{ width: 280 }}
              value={keywordInput}
              onChange={(e) => setKeywordInput(e.target.value)}
            />
            <Select
              allowClear
              style={{ width: 150 }}
              placeholder="全部环境"
              value={envFilter}
              onChange={(v) => {
                setEnvFilter(v);
                setPage(1);
              }}
              options={envs.map((e) => ({ label: e.name, value: e.key }))}
            />
          </Space>
          <Segmented
            value={statusFilter}
            onChange={(v) => {
              setStatusFilter(v as string);
              setPage(1);
            }}
            options={[
              { label: '全部', value: 'all' },
              { label: '成功', value: 'success' },
              { label: '失败', value: 'failed' },
              { label: '部署中', value: 'progressing' },
              { label: '待审批', value: 'pending_approval' },
              { label: '已回滚', value: 'rolled_back' },
              // 已中断单列：平台重启打断的单子既不是失败也不是取消，混进「失败」会让人
              // 去查发布哪里错了，而这里没有人做错任何事。
              { label: '已中断', value: 'interrupted' },
            ]}
          />
        </div>
        <Table<DeployRecord>
          rowKey="id"
          size="middle"
          loading={deploysApi.loading}
          dataSource={dataSource}
          columns={columns}
          scroll={{ x: 1720 }}
          pagination={{
            current: page,
            pageSize,
            total,
            showSizeChanger: true,
            showTotal: (t) => `共 ${t} 条部署记录`,
            onChange: (p, ps) => {
              setPage(p);
              setPageSize(ps);
            },
          }}
        />
      </SectionCard>

      {/* -------------------------------------------------- 部署详情抽屉 */}
      <Drawer
        open={!!logTarget}
        onClose={() => setLogTarget(null)}
        width={900}
        title={
          <Space size={10}>
            <span>部署详情</span>
            {logTarget ? <span className="yn-mono" style={{ fontSize: 13, color: '#8c8c8c' }}>{logTarget.deployNo}</span> : null}
            {logTarget ? <DeployStatusTag status={logTarget.status} /> : null}
            {logs.isLive ? <Tag color="processing">实时流式输出中</Tag> : null}
          </Space>
        }
        extra={
          logs.lines.length ? (
            <Button
              size="small"
              icon={<CopyOutlined />}
              onClick={() => {
                navigator.clipboard?.writeText(logs.lines.map((l) => `${(l.ts || '').slice(11, 19)}  ${l.line}`).join('\n'));
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
            <SectionCard bodyStyle={{ padding: '16px 20px' }} style={{ marginBottom: 12 }}>
              <Row gutter={[16, 12]}>
                <Col span={8}>
                  <div style={{ fontSize: 12, color: '#9aa0aa' }}>目标环境</div>
                  <div style={{ fontSize: 14, fontWeight: 500, marginTop: 3 }}>
                    {logTarget.envName}（<span className="yn-mono">{logTarget.envKey}</span>）
                  </div>
                </Col>
                <Col span={8}>
                  <div style={{ fontSize: 12, color: '#9aa0aa' }}>服务</div>
                  <div style={{ fontSize: 14, fontWeight: 500, marginTop: 3 }}>{logTarget.projectKey}</div>
                </Col>
                <Col span={8}>
                  <div style={{ fontSize: 12, color: '#9aa0aa' }}>副本数</div>
                  <div style={{ fontSize: 14, fontWeight: 500, marginTop: 3 }}>{logTarget.replicas}</div>
                </Col>
                <Col span={16}>
                  <div style={{ fontSize: 12, color: '#9aa0aa', marginBottom: 4 }}>镜像版本</div>
                  <ImageText value={logTarget.image} width={520} />
                </Col>
                <Col span={4}>
                  <div style={{ fontSize: 12, color: '#9aa0aa' }}>耗时</div>
                  <div style={{ fontSize: 14, fontWeight: 500, marginTop: 3 }}>
                    {logTarget.duration && !isLive(logTarget.status) ? humanDuration(logTarget.duration) : '进行中'}
                  </div>
                </Col>
                <Col span={8}>
                  <div style={{ fontSize: 12, color: '#9aa0aa' }}>操作人</div>
                  <div style={{ marginTop: 3 }}>{logTarget.operator || '—'}</div>
                </Col>
                <Col span={8}>
                  <div style={{ fontSize: 12, color: '#9aa0aa' }}>审批人</div>
                  <div style={{ marginTop: 3 }}>{logTarget.approver ?? '—'}</div>
                </Col>
                <Col span={8}>
                  <div style={{ fontSize: 12, color: '#9aa0aa' }}>开始时间</div>
                  <div className="yn-mono" style={{ marginTop: 3 }}>{logTarget.startedAt}</div>
                </Col>
              </Row>
            </SectionCard>

            {/* 真因排在最前：这一屏的部署进度 Steps 只回答"走到哪一步"，
                而用户点进来想问的是"为什么没成"。落这列之前的旧单如实说明未记录。 */}
            {logTarget.status === 'failed' || logTarget.status === 'interrupted' ? (
              <Alert
                type={logTarget.status === 'interrupted' ? 'warning' : 'error'}
                showIcon
                style={{ marginBottom: 12 }}
                message={
                  logTarget.errorMessage
                    ? `失败原因${logTarget.errorCode ? `（${logTarget.errorCode}）` : ''}`
                    : '这条部署单没有记录失败原因'
                }
                description={
                  logTarget.errorMessage || (
                    <>
                      记录产生于平台把失败原因落库之前（{logTarget.errorCode || '无错误码'}）。
                      当时的原因只进了易失的日志流，重启后已无法找回 —— 可参考下方「AI 诊断」，
                      但那是<b>事后按错误码推断的结论</b>，不是当时的事实。
                    </>
                  )
                }
              />
            ) : null}

            {logTarget.steps?.length ? (
              <SectionCard title="部署进度" bodyStyle={{ padding: '18px 20px' }} style={{ marginBottom: 12 }}>
                <Steps
                  size="small"
                  current={logTarget.steps.filter((s) => s.status === 'success').length}
                  status={logTarget.status === 'failed' ? 'error' : isLive(logTarget.status) ? 'process' : 'finish'}
                  items={logTarget.steps.map((s) => ({
                    title: s.name,
                    description: (
                      <span style={{ fontSize: 12, color: '#9aa0aa' }}>
                        {s.status === 'failed' ? '失败' : ((s.at || '').slice(11, 19) || '')}
                      </span>
                    ),
                  }))}
                />
              </SectionCard>
            ) : null}

            {logs.error ? (
              <Alert type="warning" showIcon style={{ marginBottom: 12 }} message={logs.error} />
            ) : null}

            {/* 命令里的命名空间用环境真实 namespace（取不到则不显示这条命令） */}
            <LogPanel
              lines={logs.lines}
              height={400}
              streaming={false}
              live={logs.isLive}
              emptyText={logs.loading ? '正在加载日志…' : '该部署暂无日志'}
              title={
                logNamespace
                  ? `kubectl -n ${logNamespace} rollout status deployment/${logTarget.projectKey}`
                  : undefined
              }
            />
          </>
        ) : null}
      </Drawer>

      {/* -------------------------------------------------- AI 诊断抽屉 */}
      <Drawer
        open={!!aiTarget}
        onClose={() => setAiTarget(null)}
        width={600}
        title={
          <Space>
            <RobotOutlined style={{ color: '#fa8c16' }} />
            {/* 这个抽屉现在成功单也能进（看回滚目标），标题再叫"异常诊断"就跟内容打架了 */}
            {aiTarget?.status === 'failed' ? '部署异常诊断' : '部署诊断'}
          </Space>
        }
        styles={{ body: { padding: 16, background: '#f4f6f9' } }}
      >
        {aiTarget ? (
          <>
            <SectionCard bodyStyle={{ padding: '16px 20px' }} style={{ marginBottom: 12 }}>
              <div style={{ fontSize: 13, lineHeight: 2 }}>
                <div>
                  <Text type="secondary">部署单：</Text>
                  <span className="yn-mono">{aiTarget.deployNo}</span>
                </div>
                <div>
                  <Text type="secondary">环境：</Text>
                  {aiTarget.envName}（<span className="yn-mono">{aiTarget.envKey}</span>）
                </div>
                <div>
                  <Text type="secondary">服务：</Text>
                  {aiTarget.projectName}（<span className="yn-mono">{aiTarget.projectKey}</span>）
                </div>
                <div style={{ marginTop: 4 }}>
                  <Text type="secondary">当前镜像：</Text>
                  <ImageText value={aiTarget.image} width={400} />
                </div>
                {/* 真因单独一行：后面的 AI 归因是**结论**（可能降级成错误码套话），
                    这一行是失败当时记下的原始事实。排查顺序应当是"先看事实，再信结论"。 */}
                {aiTarget.status === 'failed' || aiTarget.status === 'interrupted' ? (
                  <div style={{ marginTop: 4 }}>
                    <Text type="secondary">失败原因：</Text>
                    {aiTarget.errorMessage ? (
                      <span style={{ color: '#cf1322', whiteSpace: 'pre-wrap' }}>{aiTarget.errorMessage}</span>
                    ) : (
                      <Text type="secondary">未记录（{aiTarget.errorCode || '无错误码'}）</Text>
                    )}
                  </div>
                ) : null}
              </div>
            </SectionCard>

            {aiApi.error ? (
              // 这里只剩"接口本身失败"（权限不足、部署单不存在、后端异常）。
              // 模型通道不可用**不会**走到这里——后端会降级为错误码规则结论，
              // 并把原因放进 notice 一并返回。所以曾经那句
              // 「若为模型通道未配置密钥，请在模型设置填 Key 后重试」已经删掉：
              // 那个动作既不会让本页的报错消失，也不影响结论是否正确，属于
              // 「因后端改成降级而不可能再触发」的死提示。
              <Alert
                type="warning"
                showIcon
                message="诊断不可用"
                description={aiApi.error.message}
              />
            ) : aiApi.data ? (
              <Space direction="vertical" size={12} style={{ width: '100%' }}>
                {aiTarget.status === 'failed' ? (
                  <>
                    <div className="yn-suggest-card">
                      <div className="yn-suggest-card__head">
                        <CloseOutlined /> 可能原因
                      </div>
                      <div className="yn-suggest-card__row">{aiApi.data.reason}</div>
                    </div>
                    <div className="yn-suggest-card">
                      <div className="yn-suggest-card__head">
                        <CheckOutlined /> 建议操作
                      </div>
                      <div className="yn-suggest-card__row">{aiApi.data.suggestion}</div>
                    </div>
                    {/* 来源写在明面上：这段结论可能是模型读了平台日志 + 集群侧证据
                        （容器状态 / 退出码 / 崩溃前那一轮日志 / Pod 事件）得出的，也可能是
                        平台错误码查表查出来的。两者文案长得一样，不标出来用户就会把查表
                        结果当成日志已经被看过一遍。notice 则解释"这次为什么没用上模型"。 */}
                    <div>
                      <Text type="secondary" style={{ fontSize: 12 }}>
                        {aiApi.data.source === 'model'
                          ? `由模型通道「${aiApi.data.model || '默认通道'}」阅读平台日志与集群侧证据生成 · 置信度 ${(
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
                  </>
                ) : (
                  /* 非失败单：后端给的是"该部署单当前没有异常 / 无需回滚"。
                     硬套橙色「可能原因」卡片会变成红叉图标配"没有异常"，所以换一套说法。
                     回滚目标仍在下面照常显示——那才是这条路径对成功部署的价值。 */
                  <Alert
                    type="success"
                    showIcon
                    message="本次部署未发现异常"
                    description={aiApi.data.reason}
                  />
                )}

                {aiApi.data.rollbackAvailable ? (
                  <SectionCard title="可回滚目标版本" bodyStyle={{ padding: '14px 20px' }}>
                    <div style={{ fontSize: 13, marginBottom: 8 }}>
                      <ImageText value={aiApi.data.rollbackImage} width={460} />
                    </div>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      来自上一次成功部署 {aiApi.data.rollbackDeployNo || '—'}
                    </Text>
                  </SectionCard>
                ) : (
                  /* rollbackAvailable=false 的语义是"找不到与本次镜像不同的历史版本"，
                     不等于"该服务只有这一次部署记录"（同镜像重复部署过就没有可回滚目标）。 */
                  <Alert
                    type="info"
                    showIcon
                    message="没有可回滚的历史版本"
                    description="没有找到与本次镜像不同的历史版本，暂无可回滚目标。"
                  />
                )}
              </Space>
            ) : (
              /* 后端 CD 诊断目前只做本地规则查表，并没有"读日志"这一步；
                 原「正在分析部署日志…」是夸大。用不预设手段的中性措辞。 */
              <Text type="secondary">正在获取诊断结论…</Text>
            )}

            <Space style={{ marginTop: 16 }}>
              <Button
                type="primary"
                danger
                icon={<ReloadOutlined />}
                disabled={!aiApi.data?.rollbackAvailable}
                onClick={() => {
                  rollbackByDiagnosis(aiTarget);
                }}
              >
                {aiTarget.status === 'failed' ? '按建议回滚' : '回滚到上一版'}
              </Button>
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

      {/* -------------------------------------------------- 审批弹窗 */}
      <Modal
        open={!!approveTarget}
        width={600}
        title={
          <Space>
            <SafetyCertificateOutlined style={{ color: '#fa8c16' }} />
            审批部署单 {approveTarget?.deployNo}
          </Space>
        }
        onCancel={() => setApproveTarget(null)}
        footer={[
          <Button key="reject" danger loading={rejecting} onClick={submitReject}>
            驳回
          </Button>,
          <Button key="ok" type="primary" onClick={submitApprove}>
            审批通过并部署
          </Button>,
        ]}
        destroyOnClose
      >
        {approveTarget ? (
          <div style={{ fontSize: 13, lineHeight: 2 }}>
            <div>
              环境：<b>{approveTarget.envName}</b>
            </div>
            <div>
              服务：<b>{approveTarget.projectKey}</b>（{approveTarget.projectName}）
            </div>
            <div>
              镜像：<ImageText value={approveTarget.image} width={460} />
            </div>
            <div>副本：{approveTarget.replicas} 个</div>
            <div style={{ marginTop: 8, color: '#ff4d4f' }}>
              审批通过后平台将立即开始部署，请确认镜像版本与变更内容已回归验证。
            </div>
            <div style={{ marginTop: 12 }}>
              <Text type="secondary" style={{ fontSize: 13 }}>审批意见（驳回时必填）</Text>
              <Input.TextArea
                rows={2}
                style={{ marginTop: 6 }}
                placeholder="如：已确认回归通过 / 缺少变更说明，请补充后重新提交"
                value={approveRemark}
                onChange={(e) => setApproveRemark(e.target.value)}
              />
            </div>
          </div>
        ) : null}
      </Modal>
    </div>
  );
};

export default DeployList;
