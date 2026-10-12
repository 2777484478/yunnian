import React, { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Button,
  Col,
  DatePicker,
  Descriptions,
  Drawer,
  Input,
  Row,
  Segmented,
  Select,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  ExportOutlined,
  FileSearchOutlined,
  ReloadOutlined,
  SearchOutlined,
  WarningOutlined,
} from '@ant-design/icons';
import type { Dayjs } from 'dayjs';
import { PageHeader, SectionCard, StatCard } from '@/components/ui';
import { RoleTag } from '@/components/StatusTag';
import { auditApi } from '@/api';
import { useApi } from '@/hooks/useApi';
import { fmt } from '@/utils/format';
import type { AuditLog } from '@/types';

const { Text } = Typography;
const { RangePicker } = DatePicker;

// ---------------------------------------------------------------------------
// 操作审计
//
// 三处刻意的设计取舍，都是被真实接口"教"出来的：
//
// 1. **筛选项从数据里派生，不硬编码**。后端 action 是中英混排的：
//    有 `login` / `build:trigger` / `audit:export` 这种代码式，也有
//    「批量发布」「审批部署」这种中文短语。硬编码一份中文清单，
//    等于让一半的筛选项永远筛不出任何数据——而且看起来"控件是好的"。
//
// 2. **统计卡全部取后端的 total，不在前端数**。列表是分页的，
//    拿当前页去统计会把"共 191 条"显示成"共 20 条"。
//    这里的做法是带条件 + pageSize=1 借 total，零额外接口开销。
//
// 3. **导出的 CSV 直接落盘，不等轮询**。后端同步生成并同时返回 content
//    与 taskId（结果另缓存 10 分钟），所以没必要让它先"排队再回查"。
// ---------------------------------------------------------------------------

/** 后端 action 里带 `:` 的是权限点风格（`build:trigger`），展示时做一次可读化 */
const ACTION_LABEL: Record<string, string> = {
  login: '登录',
  logout: '登出',
  'build:trigger': '触发构建',
  'deploy:create': '创建部署',
  'audit:export': '导出审计',
  'env:test-connection': '环境连通性测试',
  'notify:test': '测试通知渠道',
};

const actionLabel = (v: string) => ACTION_LABEL[v] ?? v;

const MODULE_COLOR: Record<string, string> = {
  CI: 'blue',
  CD: 'geekblue',
  系统: 'purple',
  认证: 'default',
};

/** 后端 module 的固定取值（DB 里就是这四类），作为 Segmented 的回退选项 */
const MODULE_FALLBACK = ['CI', 'CD', '系统', '认证'];

/** 参与"变更类"判定的动作：命中即视为会改变平台状态的操作 */
const CHANGE_ACTIONS = new Set([
  '批量发布',
  '审批部署',
  '驳回部署',
  '取消构建',
  '触发构建',
  'build:trigger',
  'deploy:create',
  'rollback:create',
  'env:sync',
  '修改模型通道',
]);

/** 派生的筛选项来源条数上限（后端 maxPageSize = 200） */
const FACET_SAMPLE = 200;

const AuditLogPage: React.FC = () => {
  const { message, notification } = AntApp.useApp();

  // ---------------------------------------------------------------- 筛选条件
  const [keywordInput, setKeywordInput] = useState('');
  const [keyword, setKeyword] = useState('');
  const [moduleFilter, setModuleFilter] = useState<string>('all');
  const [actionFilter, setActionFilter] = useState<string | undefined>();
  const [userFilter, setUserFilter] = useState<string | undefined>();
  const [resultFilter, setResultFilter] = useState<string>('all');
  const [range, setRange] = useState<[Dayjs, Dayjs] | null>(null);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);

  useEffect(() => {
    const t = setTimeout(() => {
      setKeyword(keywordInput.trim());
      setPage(1);
    }, 350);
    return () => clearTimeout(t);
  }, [keywordInput]);

  // 后端只认 `YYYY-MM-DD HH:mm:ss` / RFC3339 / `YYYY-MM-DD`，dayjs 默认的 ISO
  // 带毫秒和 Z，虽然也能被 RFC3339 接住，但显式格式化一遍最稳。
  const startTime = range?.[0] ? range[0].format('YYYY-MM-DD HH:mm:ss') : undefined;
  const endTime = range?.[1] ? range[1].format('YYYY-MM-DD HH:mm:ss') : undefined;

  const resetPage = () => setPage(1);

  // ------------------------------------------------------------------ 取数
  const listApi = useApi(
    () =>
      auditApi.list({
        keyword,
        module: moduleFilter === 'all' ? undefined : moduleFilter,
        action: actionFilter,
        user: userFilter,
        result: resultFilter === 'all' ? undefined : resultFilter,
        startTime,
        endTime,
        page,
        pageSize,
      }),
    [keyword, moduleFilter, actionFilter, userFilter, resultFilter, startTime, endTime, page, pageSize],
  );
  const dataSource = listApi.data?.list ?? [];
  const total = listApi.data?.total ?? 0;

  // 筛选项来源：拉最近 200 条做频次统计。
  // 这是"够用且诚实"的做法——真要穷举全量 distinct 得加后端聚合接口，
  // 而筛选项只需要覆盖最近发生的动作类型。
  const facetApi = useApi(() => auditApi.list({ pageSize: FACET_SAMPLE }), []);
  const facets = useMemo(() => {
    const rows = facetApi.data?.list ?? [];
    const tally = (pick: (r: AuditLog) => string) => {
      const m = new Map<string, number>();
      rows.forEach((r) => {
        const v = pick(r);
        if (v) m.set(v, (m.get(v) ?? 0) + 1);
      });
      return [...m.entries()].sort((a, b) => b[1] - a[1]);
    };
    return {
      modules: tally((r) => r.module),
      actions: tally((r) => r.action),
      users: tally((r) => r.userName),
      sampled: rows.length,
    };
  }, [facetApi.data]);

  const moduleOptions = facets.modules.length ? facets.modules.map(([v]) => v) : MODULE_FALLBACK;

  // 统计卡：全部带 pageSize=1，只用 total，不拉数据
  const totalApi = useApi(() => auditApi.list({ pageSize: 1 }), []);
  const since24h = useMemo(() => {
    const d = new Date(Date.now() - 24 * 3600 * 1000);
    const p = (n: number) => String(n).padStart(2, '0');
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`;
  }, []);
  const last24hApi = useApi(() => auditApi.list({ startTime: since24h, pageSize: 1 }), [since24h]);
  const failedApi = useApi(() => auditApi.list({ result: 'failed', pageSize: 1 }), []);

  // 变更类计数：逐个动作码累加 total。动作种类有限（十几个），
  // 并发发出去比"拉全量再前端数"省得多，也比编一个假数字诚实。
  const changeActions = useMemo(() => {
    const fromData = facets.actions.map(([v]) => v).filter((a) => CHANGE_ACTIONS.has(a));
    const fromConst = [...CHANGE_ACTIONS];
    return [...new Set([...fromData, ...fromConst])];
  }, [facets.actions]);

  const [changeCount, setChangeCount] = useState<number | null>(null);
  useEffect(() => {
    let alive = true;
    Promise.all(
      changeActions.map((a) => auditApi.list({ action: a, pageSize: 1 }).then((r) => r.total).catch(() => 0)),
    )
      .then((nums) => {
        if (alive) setChangeCount(nums.reduce((s, n) => s + n, 0));
      })
      .catch(() => {
        if (alive) setChangeCount(null);
      });
    return () => {
      alive = false;
    };
  }, [changeActions]);

  const refreshAll = () => {
    listApi.refresh();
    facetApi.refresh();
    totalApi.refresh();
    last24hApi.refresh();
    failedApi.refresh();
  };

  // ------------------------------------------------------------------ 导出
  const [exporting, setExporting] = useState(false);

  const doExport = async () => {
    setExporting(true);
    try {
      const res = await auditApi.export({
        module: moduleFilter === 'all' ? undefined : moduleFilter,
        action: actionFilter,
        user: userFilter,
        result: resultFilter === 'all' ? undefined : resultFilter,
        startAt: startTime,
        endAt: endTime,
        limit: 50000,
      });

      if (!res.content) {
        message.warning('当前条件下没有可导出的记录');
        return;
      }

      // 后端生成的 CSV 已经带 UTF-8 BOM 了，这里只在缺失时补，
      // 否则双 BOM 会让 Excel 把第一列表头读成 `ï»¿时间`。
      const csv = res.content.startsWith('\ufeff') ? res.content : '\ufeff' + res.content;
      const blob = new Blob([csv], { type: 'text/csv;charset=utf-8' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = res.filename || `audit-logs-${Date.now()}.csv`;
      document.body.appendChild(a);
      a.click();
      document.body.removeChild(a);
      URL.revokeObjectURL(url);

      notification.success({
        message: '审计日志已导出',
        description: res.truncated
          ? `本次导出 ${res.rows} 行，已达到上限被截断——请缩小时间范围后分批导出。`
          : `本次导出 ${res.rows} 行（${res.filename}）。`,
      });
      // 导出本身也会写一条审计，所以列表得刷一遍
      refreshAll();
    } catch (e) {
      message.error((e as Error)?.message || '导出失败');
    } finally {
      setExporting(false);
    }
  };

  // ------------------------------------------------------------ 详情抽屉
  const [detail, setDetail] = useState<AuditLog | null>(null);

  const columns: ColumnsType<AuditLog> = [
    {
      title: '操作时间',
      dataIndex: 'at',
      width: 176,
      render: (v: string) => (
        <span className="yn-mono" style={{ color: '#5c6470' }}>
          {fmt(v)}
        </span>
      ),
    },
    {
      title: '操作人',
      dataIndex: 'userName',
      width: 170,
      render: (v: string, r) => (
        <div>
          <span style={{ fontWeight: 500 }}>{v}</span>
          <div style={{ marginTop: 3 }}>
            <RoleTag role={r.role} />
          </div>
        </div>
      ),
    },
    {
      title: '模块',
      dataIndex: 'module',
      width: 88,
      render: (v: string) => (
        <Tag color={MODULE_COLOR[v]} style={{ marginInlineEnd: 0 }}>
          {v}
        </Tag>
      ),
    },
    {
      title: '操作',
      dataIndex: 'action',
      width: 158,
      render: (v: string) => (
        <Tooltip title={v}>
          <span style={{ fontWeight: 500 }}>{actionLabel(v)}</span>
        </Tooltip>
      ),
    },
    {
      title: '操作对象',
      dataIndex: 'target',
      width: 210,
      render: (v: string, r) => (
        <div>
          <span className="yn-mono" style={{ fontSize: 12.5 }}>
            {v || '-'}
          </span>
          <div style={{ fontSize: 12, color: '#9aa0aa' }}>{r.targetType}</div>
        </div>
      ),
    },
    {
      title: '操作详情',
      dataIndex: 'detail',
      render: (v: string) => (
        <Text type="secondary" style={{ fontSize: 13 }}>
          {v}
        </Text>
      ),
    },
    {
      title: '来源 IP',
      dataIndex: 'ip',
      width: 136,
      render: (v: string) => (
        <span className="yn-mono" style={{ fontSize: 12.5 }}>
          {v || '-'}
        </span>
      ),
    },
    {
      title: '结果',
      dataIndex: 'result',
      width: 90,
      render: (v: string) =>
        v === 'success' ? (
          <Tag color="success" style={{ marginInlineEnd: 0 }}>
            成功
          </Tag>
        ) : (
          <Tag color="error" style={{ marginInlineEnd: 0 }}>
            失败
          </Tag>
        ),
    },
  ];

  const filterActive = !!(
    keyword ||
    actionFilter ||
    userFilter ||
    range ||
    moduleFilter !== 'all' ||
    resultFilter !== 'all'
  );

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: '操作审计' }]}
        title="操作审计"
        description="记录谁在什么时间、对哪个项目或环境、执行了什么操作，满足变更可追溯要求"
        extra={
          <Space>
            <Button icon={<ExportOutlined />} loading={exporting} onClick={doExport} disabled={total === 0}>
              导出
            </Button>
            <Button
              icon={<ReloadOutlined />}
              onClick={() => {
                refreshAll();
                message.success('已刷新');
              }}
            >
              刷新
            </Button>
          </Space>
        }
      />

      <Row gutter={[16, 16]}>
        <Col xs={12} xl={6}>
          <StatCard
            title="审计记录总数"
            value={totalApi.data?.total ?? '-'}
            unit="条"
            icon={<FileSearchOutlined />}
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="近 24 小时"
            value={last24hApi.data?.total ?? '-'}
            unit="条"
            accent="#1677ff"
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="变更类操作"
            value={changeCount ?? '-'}
            unit="条"
            accent="#722ed1"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                按 {changeActions.length} 类动作累加
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="失败/异常"
            value={failedApi.data?.total ?? '-'}
            unit="条"
            accent="#ff4d4f"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                含登录失败与权限不足
              </Text>
            }
          />
        </Col>
      </Row>

      <SectionCard style={{ marginTop: 16 }} bodyStyle={{ padding: 0 }}>
        <div className="yn-filter-bar" style={{ flexWrap: 'wrap', rowGap: 10 }}>
          <Space size={12} wrap>
            <Input
              allowClear
              prefix={<SearchOutlined style={{ color: '#b5bac4' }} />}
              placeholder="搜索详情 / 对象 / IP"
              style={{ width: 240 }}
              value={keywordInput}
              onChange={(e) => setKeywordInput(e.target.value)}
            />
            <Select
              allowClear
              showSearch
              optionFilterProp="label"
              placeholder="选择操作人"
              style={{ width: 190 }}
              value={userFilter}
              onChange={(v) => {
                setUserFilter(v);
                resetPage();
              }}
              options={facets.users.map(([v, n]) => ({ label: `${v}（${n}）`, value: v }))}
              notFoundContent={<Text type="secondary" style={{ fontSize: 12 }}>先加载数据</Text>}
            />
            <Select
              allowClear
              showSearch
              optionFilterProp="label"
              placeholder="选择操作类型"
              style={{ width: 200 }}
              value={actionFilter}
              onChange={(v) => {
                setActionFilter(v);
                resetPage();
              }}
              options={facets.actions.map(([v, n]) => ({ label: `${actionLabel(v)}（${n}）`, value: v }))}
              notFoundContent={<Text type="secondary" style={{ fontSize: 12 }}>先加载数据</Text>}
            />
            <RangePicker
              style={{ width: 340 }}
              showTime
              value={range}
              onChange={(v) => {
                setRange(v as [Dayjs, Dayjs] | null);
                resetPage();
              }}
            />
          </Space>
          <Space size={12} wrap>
            <Segmented
              value={moduleFilter}
              onChange={(v) => {
                setModuleFilter(v as string);
                resetPage();
              }}
              options={[
                { label: '全部模块', value: 'all' },
                ...moduleOptions.map((m) => ({ label: m, value: m })),
              ]}
            />
            <Select
              style={{ width: 118 }}
              value={resultFilter}
              onChange={(v) => {
                setResultFilter(v);
                resetPage();
              }}
              options={[
                { label: '全部结果', value: 'all' },
                { label: '成功', value: 'success' },
                { label: '失败', value: 'failed' },
              ]}
            />
            {filterActive ? (
              <Button
                type="link"
                size="small"
                onClick={() => {
                  setKeywordInput('');
                  setKeyword('');
                  setModuleFilter('all');
                  setActionFilter(undefined);
                  setUserFilter(undefined);
                  setResultFilter('all');
                  setRange(null);
                  resetPage();
                }}
              >
                清空筛选
              </Button>
            ) : null}
          </Space>
        </div>

        {listApi.error ? (
          <div style={{ padding: 16 }}>
            <Alert
              type="error"
              showIcon
              message="审计日志加载失败"
              description={listApi.error.message}
              action={
                <Button size="small" onClick={listApi.refresh}>
                  重试
                </Button>
              }
            />
          </div>
        ) : null}

        <Table<AuditLog>
          rowKey="id"
          size="middle"
          loading={listApi.loading}
          dataSource={dataSource}
          columns={columns}
          scroll={{ x: 1500 }}
          onRow={(r) => ({ onClick: () => setDetail(r), style: { cursor: 'pointer' } })}
          locale={{ emptyText: listApi.loading ? ' ' : '没有符合条件的审计记录' }}
          pagination={{
            current: page,
            pageSize,
            total,
            showSizeChanger: true,
            onChange: (p, s) => {
              setPage(p);
              setPageSize(s);
            },
            showTotal: (t) => `共 ${t} 条审计记录`,
          }}
        />

        {facets.sampled >= FACET_SAMPLE ? (
          <div style={{ padding: '10px 16px', borderTop: '1px solid #f0f2f5' }}>
            <Space size={6}>
              <WarningOutlined style={{ color: '#faad14' }} />
              <Text type="secondary" style={{ fontSize: 12 }}>
                操作人与操作类型的下拉选项取自最近 {FACET_SAMPLE} 条记录，更早出现的类型需用关键词搜索定位。
              </Text>
            </Space>
          </div>
        ) : null}
      </SectionCard>

      <Drawer
        open={!!detail}
        width={560}
        title="审计详情"
        onClose={() => setDetail(null)}
        extra={
          detail ? (
            <Tag color={detail.result === 'success' ? 'success' : 'error'} style={{ marginInlineEnd: 0 }}>
              {detail.result === 'success' ? '成功' : '失败'}
            </Tag>
          ) : null
        }
      >
        {detail ? (
          <>
            <Descriptions
              column={1}
              size="small"
              items={[
                { key: 'at', label: '操作时间', children: <span className="yn-mono">{fmt(detail.at)}</span> },
                {
                  key: 'user',
                  label: '操作人',
                  children: (
                    <Space size={8}>
                      <span>{detail.userName}</span>
                      <RoleTag role={detail.role} />
                      {detail.user ? (
                        <span className="yn-mono" style={{ fontSize: 12, color: '#9aa0aa' }}>
                          {detail.user}
                        </span>
                      ) : null}
                    </Space>
                  ),
                },
                { key: 'module', label: '模块', children: detail.module },
                {
                  key: 'action',
                  label: '操作',
                  children: (
                    <Space size={8}>
                      <span>{actionLabel(detail.action)}</span>
                      {actionLabel(detail.action) !== detail.action ? (
                        <span className="yn-mono" style={{ fontSize: 12, color: '#9aa0aa' }}>
                          {detail.action}
                        </span>
                      ) : null}
                    </Space>
                  ),
                },
                { key: 'targetType', label: '对象类型', children: detail.targetType || '-' },
                { key: 'target', label: '操作对象', children: <span className="yn-mono">{detail.target || '-'}</span> },
                { key: 'ip', label: '来源 IP', children: <span className="yn-mono">{detail.ip || '-'}</span> },
                { key: 'id', label: '记录 ID', children: <span className="yn-mono">{detail.id}</span> },
              ]}
            />
            <div style={{ marginTop: 16 }}>
              <div style={{ fontSize: 13, color: '#8c8c8c', marginBottom: 6 }}>操作详情</div>
              <div
                style={{
                  background: '#f7f8fa',
                  border: '1px solid #eceef2',
                  borderRadius: 6,
                  padding: '10px 12px',
                  fontSize: 13,
                  lineHeight: 1.8,
                  wordBreak: 'break-word',
                }}
              >
                {detail.detail || '（后端未记录详情）'}
              </div>
            </div>
          </>
        ) : null}
      </Drawer>
    </div>
  );
};

export default AuditLogPage;
