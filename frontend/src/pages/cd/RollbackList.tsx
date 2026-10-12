import React, { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Button,
  Col,
  Descriptions,
  Drawer,
  Input,
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
import { CopyOutlined, FileTextOutlined, ReloadOutlined, RobotOutlined, RollbackOutlined, SearchOutlined } from '@ant-design/icons';
import { Link } from 'react-router-dom';
import { PageHeader, SectionCard, StatCard } from '@/components/ui';
import { LogPanel } from '@/components/LogPanel';
import { ImageText } from '@/components/StatusTag';
import { environmentApi, rollbackApi, systemApi } from '@/api';
import { useApi } from '@/hooks/useApi';
import { useJobLogs } from '@/hooks/useJobLogs';
import { humanDuration } from '@/utils/format';
import type { RollbackRecord } from '@/types';

const { Text } = Typography;

/** 仍在执行中的回滚：走 SSE 实时日志。
 *  后端状态枚举只有 success / failed / progressing，**没有 pending**（原先的分支永不成立故删）。 */
const isLive = (s: string) => s === 'progressing';

const envTagColor = (envKey: string) =>
  envKey === 'prod' ? 'red' : envKey === 'staging' ? 'orange' : envKey === 'canary' ? 'purple' : 'blue';

const RollbackList: React.FC = () => {
  const { message } = AntApp.useApp();

  // ---------------------------------------------------------------- 筛选条件
  const [keywordInput, setKeywordInput] = useState('');
  const [keyword, setKeyword] = useState('');
  const [envFilter, setEnvFilter] = useState<string | undefined>();
  const [triggerFilter, setTriggerFilter] = useState<string>('all');
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

  const listApi = useApi(
    () =>
      rollbackApi.list({
        keyword,
        envKey: envFilter,
        triggerType: triggerFilter === 'all' ? undefined : (triggerFilter as 'manual' | 'ai_suggested' | 'auto'),
        page,
        pageSize,
      }),
    [keyword, envFilter, triggerFilter, page, pageSize],
  );
  const dataSource = listApi.data?.list ?? [];
  const total = listApi.data?.total ?? 0;

  const statsApi = useApi(() => systemApi.stats(), []);

  // 成功 / 失败 / AI 建议 / 执行中 四个计数。
  // 后端没有回滚的聚合接口，但计数可以借分页的 total：pageSize=1 拿 total 就行，
  // 比把全量记录拉到前端再数省得多。一次 effect 里并发发出去，不串行等待。
  const facetsApi = useApi(async () => {
    const [succ, fail, ai, running] = await Promise.all([
      rollbackApi.list({ status: 'success', pageSize: 1 }),
      rollbackApi.list({ status: 'failed', pageSize: 1 }),
      rollbackApi.list({ triggerType: 'ai_suggested', pageSize: 1 }),
      rollbackApi.list({ status: 'progressing', pageSize: 1 }),
    ]);
    return { succ: succ.total, fail: fail.total, ai: ai.total, running: running.total };
  }, []);
  const facets = facetsApi.data;

  const refreshAll = () => {
    listApi.refresh();
    statsApi.refresh();
    facetsApi.refresh();
  };

  // ------------------------------------------------------------ 详情 / 日志
  const [detail, setDetail] = useState<RollbackRecord | null>(null);

  // 回滚的日志流挂在它关联的部署单上（后端 rollbackLogStream 用 rb.DeployID），
  // 历史日志则取那次部署的日志 —— 注意 rollback 自身只有 stream、没有 logs 接口。
  //
  // 这里两个 id 不是同一个键，必须分开传：`stream` 要**回滚单 id**，
  // 而历史日志要**部署单 id**（用回滚单 id 去打 /deployments/{id}/logs 会查空）。
  const rollbackLogs = useJobLogs({
    id: detail?.id ?? null,
    live: !!detail && isLive(detail.status),
    stream: rollbackApi.stream,
    fetchHistory: () => rollbackApi.deployLogs(detail?.deployId ?? ''),
  });

  useEffect(() => {
    if (!rollbackLogs.ended) return;
    refreshAll();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [rollbackLogs.ended]);

  // ------------------------------------------------------------------ 表格
  const columns: ColumnsType<RollbackRecord> = [
    {
      title: '服务',
      dataIndex: 'service',
      width: 180,
      fixed: 'left',
      render: (v, r) => (
        <div>
          <Link to={`/ci/projects/${r.projectKey}`} style={{ fontWeight: 500 }}>
            {r.projectKey}
          </Link>
          <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 2 }}>{r.deployNo}</div>
        </div>
      ),
    },
    {
      title: '环境',
      dataIndex: 'envName',
      width: 130,
      render: (v, r) => (
        <Link to={`/cd/environments/${r.envKey}?tab=rollbacks`}>
          <Tag color={envTagColor(r.envKey)} style={{ marginInlineEnd: 0 }}>
            {v}
          </Tag>
        </Link>
      ),
    },
    { title: '从（异常版本）', dataIndex: 'fromImage', width: 300, render: (v) => <ImageText value={v} width={290} /> },
    {
      title: '',
      key: 'arrow',
      width: 44,
      align: 'center',
      render: () => <RollbackOutlined style={{ color: '#722ed1' }} />,
    },
    { title: '回滚到', dataIndex: 'toImage', width: 300, render: (v) => <ImageText value={v} width={290} /> },
    {
      title: '触发方式',
      dataIndex: 'triggerType',
      width: 122,
      render: (v) =>
        v === 'ai_suggested' ? (
          <Tag color="purple" icon={<RobotOutlined />} style={{ marginInlineEnd: 0 }}>
            AI 建议
          </Tag>
        ) : v === 'auto' ? (
          // ⚠️ 「自动」现在只可能是**历史记录**：自动回滚（发布失败后由平台退回上一版）
          // 已于 2026-10-08 下线，现状是**失败不回滚**（见 `cd.go` 失败分支的注释与那句
          // "平台不会自动回滚，工作负载当前停在本次变更后的镜像上"），接口 binding 也只收
          // `manual | ai_suggested`。但下线前产生的记录仍带 `auto`，所以这一支要留着 ——
          // 且**不能并进「人工」**：把自动回滚说成人工回滚，等于告诉用户"有人处理过了"。
          <Tag color="orange" icon={<RollbackOutlined />} style={{ marginInlineEnd: 0 }}>
            自动
          </Tag>
        ) : (
          <Tag style={{ marginInlineEnd: 0 }}>人工</Tag>
        ),
    },
    {
      title: '原因',
      dataIndex: 'reason',
      ellipsis: true,
      render: (v) => (
        <Text style={{ fontSize: 13 }} type="secondary">
          {v}
        </Text>
      ),
    },
    { title: '操作人', dataIndex: 'operator', width: 96 },
    {
      title: '状态',
      dataIndex: 'status',
      width: 108,
      render: (v: string) => (
        <Tag
          color={
            v === 'success'
              ? 'success'
              : v === 'failed'
                ? 'error'
                : v === 'interrupted'
                  ? 'orange'
                  : 'processing'
          }
          style={{ marginInlineEnd: 0 }}
        >
          {/* 已中断必须单列：原先只有"成功/失败/其它=执行中"三档，
              一条被平台重启打断的回滚会显示成永远转不完的「执行中」。
              回滚记录的收尾写入点只有一个（finishRollbackOf，由部署执行器调用），
              平台一重启那次调用就没了，状态会永远停在 progressing。 */}
          {v === 'success' ? '成功' : v === 'failed' ? '失败' : v === 'interrupted' ? '已中断' : '执行中'}
        </Tag>
      ),
    },
    {
      title: '耗时',
      dataIndex: 'duration',
      width: 90,
      render: (v, r) => (isLive(r.status) || !v ? <Text type="secondary">—</Text> : humanDuration(v)),
    },
    {
      title: '时间',
      dataIndex: 'startedAt',
      width: 168,
      render: (v) => <span className="yn-mono" style={{ color: '#8c8c8c' }}>{v}</span>,
    },
    {
      title: '操作',
      key: 'action',
      fixed: 'right',
      width: 130,
      render: (_, r) => (
        <Space size={2}>
          <Button type="link" size="small" style={{ paddingInline: 4 }} onClick={() => setDetail(r)}>
            详情
          </Button>
          <Button type="link" size="small" icon={<FileTextOutlined />} style={{ paddingInline: 4 }} onClick={() => setDetail(r)}>
            日志
          </Button>
        </Space>
      ),
    },
  ];

  const successRate = facets && facets.succ + facets.fail > 0 ? Math.round((facets.succ / (facets.succ + facets.fail)) * 100) : null;
  const pageError = listApi.error || envsApi.error;

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: 'CD 环境部署' }, { title: '回滚记录' }]}
        title="回滚记录"
        description="所有环境的版本回滚流水，记录回滚原因、操作人与耗时，AI 异常诊断可直接生成回滚建议"
        extra={
          <Space>
            <Button icon={<ReloadOutlined />} loading={listApi.loading} onClick={refreshAll} />
          </Space>
        }
      />

      {pageError ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message="回滚数据加载失败"
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
          <StatCard
            title="回滚次数"
            value={statsApi.data?.totalRollbacks ?? '—'}
            unit="次"
            accent="#722ed1"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                执行中 {facets?.running ?? 0} 次
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="AI 建议回滚"
            value={facets?.ai ?? '—'}
            unit="次"
            accent="#1677ff"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                {statsApi.data?.totalRollbacks && facets
                  ? `占比 ${Math.round((facets.ai / statsApi.data.totalRollbacks) * 100)}%`
                  : '来自 AI 异常诊断的一键回滚'}
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="回滚成功率"
            value={successRate ?? '—'}
            unit="%"
            accent="#52c41a"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                成功 {facets?.succ ?? 0} · 失败 {facets?.fail ?? 0}
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="待关注"
            value={facets?.fail ?? '—'}
            unit="次"
            accent="#ff4d4f"
            footer={<Text type="secondary" style={{ fontSize: 12 }}>失败的回滚需人工确认现场</Text>}
          />
        </Col>
      </Row>

      <SectionCard style={{ marginTop: 16 }} bodyStyle={{ padding: 0 }}>
        <div className="yn-filter-bar">
          <Space size={12} wrap>
            <Input
              allowClear
              prefix={<SearchOutlined style={{ color: '#b5bac4' }} />}
              placeholder="搜索服务 / 回滚原因 / 部署单号"
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
            value={triggerFilter}
            onChange={(v) => {
              setTriggerFilter(v as string);
              setPage(1);
            }}
            options={[
              { label: '全部', value: 'all' },
              { label: '人工回滚', value: 'manual' },
              { label: '自动回滚', value: 'auto' },
              /* 这一项保留是为了**查历史**：自动回滚已于 2026-10-08 下线，现状是失败不回滚，
                 但下线前的记录仍在库里。删掉筛选项会让那些记录查不出来，而不是"不存在"。 */
              { label: 'AI 建议', value: 'ai_suggested' },
            ]}
          />
        </div>
        <Table<RollbackRecord>
          rowKey="id"
          size="middle"
          loading={listApi.loading}
          dataSource={dataSource}
          columns={columns}
          scroll={{ x: 1900 }}
          pagination={{
            current: page,
            pageSize,
            total,
            showSizeChanger: true,
            showTotal: (t) => `共 ${t} 条回滚记录`,
            onChange: (p, ps) => {
              setPage(p);
              setPageSize(ps);
            },
          }}
        />
      </SectionCard>

      <Drawer
        open={!!detail}
        onClose={() => setDetail(null)}
        width={820}
        title={
          <Space size={10}>
            <RollbackOutlined style={{ color: '#722ed1' }} />
            <span>回滚详情 · {detail?.projectKey}</span>
            {/* 后端在待审批/排队期间也会把回滚单标成 progressing，故不能承诺"正在实时输出"，只陈述"执行中" */}
            {rollbackLogs.isLive ? <Tag color="processing">执行中</Tag> : null}
          </Space>
        }
        extra={
          rollbackLogs.lines.length ? (
            <Button
              size="small"
              icon={<CopyOutlined />}
              onClick={() => {
                navigator.clipboard?.writeText(rollbackLogs.lines.map((l) => `${(l.ts || '').slice(11, 19)}  ${l.line}`).join('\n'));
                message.success('日志已复制');
              }}
            >
              复制日志
            </Button>
          ) : null
        }
        styles={{ body: { padding: 16, background: '#f4f6f9' } }}
      >
        {detail ? (
          <>
            <SectionCard bodyStyle={{ padding: '16px 20px' }} style={{ marginBottom: 12 }}>
              <Descriptions
                column={2}
                size="small"
                items={[
                  { key: 'no', label: '关联部署单', children: <span className="yn-mono">{detail.deployNo}</span> },
                  {
                    key: 'env',
                    label: '环境',
                    children: <Link to={`/cd/environments/${detail.envKey}`}>{detail.envName}</Link>,
                  },
                  { key: 'op', label: '操作人', children: detail.operator },
                  {
                    key: 'trigger',
                    label: '触发方式',
                    children:
                      detail.triggerType === 'ai_suggested' ? (
                        <Tag color="purple">AI 建议</Tag>
                      ) : detail.triggerType === 'auto' ? (
                        /* 陈述**历史**，不用现在时：自动回滚能力已于 2026-10-08 下线
                           （现状是失败不回滚）。写成「发布失败后平台回滚」会让用户以为
                           平台现在还会替他兜底 —— 那是本页最容易给错的一个预期。 */
                        <Tag color="orange">自动（历史记录，该能力已下线）</Tag>
                      ) : (
                        <Tag>人工</Tag>
                      ),
                  },
                  { key: 'time', label: '回滚时间', children: <span className="yn-mono">{detail.startedAt}</span> },
                  {
                    key: 'dur',
                    label: '回滚耗时',
                    // 与列表列同一口径：**终态但没有耗时 → 显示"—"，不要写"进行中"**。
                    // 一条 status=success 的记录配一句"进行中"是自相矛盾的。
                    children: detail.duration
                      ? humanDuration(detail.duration)
                      : isLive(detail.status)
                        ? '进行中'
                        : '—',
                  },
                  {
                    key: 'reason',
                    label: '回滚原因',
                    span: 2,
                    children: <Text style={{ color: '#8c1616' }}>{detail.reason}</Text>,
                  },
                ]}
              />
            </SectionCard>

            <SectionCard
              title="版本变化"
              bodyStyle={{ padding: '16px 20px' }}
              style={{ marginBottom: 12 }}
            >
              <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
                <div>
                  <div style={{ fontSize: 12, color: '#9aa0aa', marginBottom: 4 }}>回滚前（异常版本）</div>
                  <ImageText value={detail.fromImage} width={720} />
                </div>
                <RollbackOutlined style={{ color: '#722ed1', fontSize: 16 }} />
                <div>
                  <div style={{ fontSize: 12, color: '#9aa0aa', marginBottom: 4 }}>回滚后（目标版本）</div>
                  <ImageText value={detail.toImage} width={720} />
                </div>
              </div>
            </SectionCard>

            <SectionCard title="执行过程" bodyStyle={{ padding: '18px 20px' }} style={{ marginBottom: 12 }}>
              {/*
                ⚠️ 这一段描述的是"平台会做什么"，所以**自动回滚与人工回滚不能共用一套文案**：
                  · 人工回滚的目标版本来自平台镜像记录（resolveRollbackTarget 会校验归属）；
                  · 自动回滚的目标版本来自**集群当下真实运行的值**（刻意不读平台记录，两者会漂移），
                    而且平台**没有**为它写操作审计 —— 那是人发起的动作才有的。
                共用的后果就是界面替系统说了两件它没做的事。
                另：`auto` 分支**只服务历史记录**（自动回滚能力 2026-10-08 已下线），
                所以下面 `isAuto` 的文案一律用过去时，不要写成"平台会…"。
              */}
              {(() => {
                const isAuto = detail.triggerType === 'auto';
                // 中断的回滚**不渲染步骤条**：Steps 的 status / current 是从状态硬编码
                // 推出来的（success→finish@5、failed→error@2、其余→process@3），
                // 中断会掉进第三种，于是界面用蓝色转圈显示「执行滚动更新」—— 而它早就停了。
                // 而且回滚一中断，"集群里到底滚到了哪一步"平台**并不知道**，编不出来就别编。
                if (detail.status === 'interrupted') {
                  return (
                    <Alert
                      type="warning"
                      showIcon
                      message="该回滚因平台重启而中断，未执行完"
                      description="平台重启会一并清空正在执行的部署队列，这条回滚不会再继续。服务当前实际运行的版本可能既不是回滚前那版、也不是回滚目标版，建议先在环境详情里核对再决定是否重新执行。"
                    />
                  );
                }
                return (
                  <Steps
                    direction="vertical"
                    size="small"
                    status={detail.status === 'success' ? 'finish' : detail.status === 'failed' ? 'error' : 'process'}
                    current={detail.status === 'success' ? 5 : detail.status === 'failed' ? 2 : 3}
                    items={[
                      isAuto
                        ? {
                            title: '取回退目标',
                            description: '取工作负载当前实际运行的版本作为回退目标（不读平台镜像记录，避免两者漂移）',
                          }
                        : { title: '校验目标版本', description: '在平台镜像记录中解析目标版本，不属于该项目的镜像会被拒绝' },
                      {
                        title: isAuto ? '登记回滚记录' : '登记回滚单',
                        description: '回滚前后镜像写入回滚记录，供审计与二次回滚查询',
                      },
                      { title: '执行滚动更新', description: `更新 Deployment 镜像 → ${detail.toImage.split(':').pop()}` },
                      // 步骤 4/5 的描述原先无条件写死"已通过/已记录"，未成功时也是假结论。
                      // 改为只有 success 才写结论，其余状态只陈述中性意图（不冒充已发生）。
                      {
                        title: '健康检查',
                        description:
                          detail.status === 'success'
                            ? 'Readiness probe 通过，Pod 全部就绪'
                            : '更新后按部署流程检查 Pod 就绪状态',
                      },
                      isAuto
                        ? {
                            title: '记录失败与回退结果',
                            description:
                              detail.status === 'success'
                                ? '失败原因与回退结果已写入部署单与本页回滚记录'
                                : '失败原因与回退结果写入部署单与回滚记录',
                          }
                        : {
                            title: '写入审计日志',
                            description:
                              detail.status === 'success'
                                ? `已记录操作人 ${detail.operator} 的回滚行为`
                                : '回滚结束后写入审计日志',
                          },
                    ]}
                  />
                );
              })()}
            </SectionCard>

            {rollbackLogs.error ? (
              <Alert type="warning" showIcon style={{ marginBottom: 12 }} message={rollbackLogs.error} />
            ) : null}

            <LogPanel
              lines={rollbackLogs.lines}
              height={320}
              streaming={false}
              live={rollbackLogs.isLive}
              emptyText={
                rollbackLogs.loading
                  ? '正在加载日志…'
                  : isLive(detail.status)
                    ? '正在连接实时日志…'
                    : '该回滚暂无日志（部署日志只保留在实时流中，平台重启后不可回溯）'
              }
              title={`rollback / ${detail.projectKey} @ ${detail.envKey}`}
            />
          </>
        ) : null}
      </Drawer>
    </div>
  );
};

export default RollbackList;
