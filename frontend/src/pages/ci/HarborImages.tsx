import React, { useEffect, useMemo, useState } from 'react';
import { Alert, Button, Select, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { CopyOutlined, ReloadOutlined } from '@ant-design/icons';
import { SectionCard } from '@/components/ui';
import { imageApi, projectApi } from '@/api';
import type { HarborTagRow } from '@/api/ci';
import { useApi } from '@/hooks/useApi';
import { ago, fmt, humanSize } from '@/utils/format';

const { Text } = Typography;

/** Harbor 项目默认值（拉不到列表时兜底，与后端 `harbor.project` 默认一致） */
const DEFAULT_PROJECT = 'escloud';

/**
 * 「Harbor 仓库」视角。
 *
 * 结构（2026-10-20 用户确认的效果图）：
 *   [Harbor 项目 ▼]  [微服务 ▼]  [重新读取]  地址
 *   ── Tag / Digest / 大小 / 时间 / 操作 ──
 *
 * 两个下拉各管一件事，不要合并：
 *   · **Harbor 项目** —— Harbor 上的 project（候选实时从 Harbor 拉，默认 escloud）
 *   · **微服务** —— 平台**已纳管**的项目，一个不多一个不少
 * 查的是「project + 微服务的镜像名末段」拼出来的那个仓库。
 *
 * ⚠️ 为什么不直接用平台的 `imageName` 整段：
 * 它分前后端（`backend/userv2`、`frontend/aio-doc`），而 Harbor 上这些镜像
 * **统一在 escloud 下**（`escloud/userv2`）。拿第一段当 project 会查一个不存在的仓库
 * （2026-10-20 用户报的正是这个）。
 */
const repoOf = (imageName?: string) => (imageName || '').split('/').filter(Boolean).pop() || '';

const HarborImages: React.FC = () => {
  const [project, setProject] = useState(DEFAULT_PROJECT);
  const [serviceKey, setServiceKey] = useState<string | undefined>();

  const [rows, setRows] = useState<HarborTagRow[]>([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [reloadAt, setReloadAt] = useState(0);

  // Harbor 项目候选：实时从 Harbor 拉；拉不到就退回默认值（不让下拉空着）
  const projectsApi = useApi(() => imageApi.harborProjects(), []);
  const projectOptions = useMemo(() => {
    const list = projectsApi.data ?? [];
    return (list.length ? list : [DEFAULT_PROJECT]).map((n) => ({ label: n, value: n }));
  }, [projectsApi.data]);

  // 微服务候选：平台已纳管的项目
  const servicesApi = useApi(() => projectApi.list({ pageSize: 200 }), []);
  const services = useMemo(() => servicesApi.data?.list ?? [], [servicesApi.data]);

  useEffect(() => {
    if (projectOptions.length && !projectOptions.some((o) => o.value === project)) {
      setProject(projectOptions[0].value);
    }
  }, [projectOptions, project]);

  const service = services.find((s) => s.key === serviceKey);
  const repo = repoOf(service?.imageName);
  const address = repo ? `${project}/${repo}` : '';

  useEffect(() => {
    if (!repo) {
      setRows([]);
      setTotal(0);
      return;
    }
    let alive = true;
    setLoading(true);
    setError(null);
    imageApi
      .harborTags(project, repo, page, pageSize)
      .then((r) => {
        if (!alive) return;
        setRows(r.list ?? []);
        setTotal(r.total ?? 0);
      })
      .catch((e) => {
        if (!alive) return;
        setRows([]);
        setTotal(0);
        setError(e?.message ?? '读取 Harbor tag 失败');
      })
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [project, repo, page, pageSize, reloadAt]);

  const columns: ColumnsType<HarborTagRow> = [
    {
      title: 'Tag',
      dataIndex: 'tag',
      width: 220,
      render: (v: string) => <span className="yn-harbor-tag yn-mono">{v}</span>,
    },
    {
      title: 'Digest',
      dataIndex: 'digest',
      render: (v?: string) =>
        v ? (
          <Tooltip title={v}>
            <span className="yn-mono" style={{ fontSize: 12, color: '#7a8290' }}>
              {v.length > 26 ? `${v.slice(0, 26)}…` : v}
            </span>
          </Tooltip>
        ) : (
          // 只画弱化的 —，把原因挂 tooltip：v1 上每行都写同一句话会把这列糊掉
          <Tooltip title="该 Harbor 版本的 core 接口不返回 digest（v1），需要 digest 请用 v2 的 artifacts 接口">
            <Text type="secondary" style={{ fontSize: 12, cursor: 'help' }}>
              —
            </Text>
          </Tooltip>
        ),
    },
    {
      title: '大小',
      dataIndex: 'size',
      width: 110,
      render: (v: number) => (v ? humanSize(v) : '—'),
    },
    {
      title: '时间',
      dataIndex: 'pushedAt',
      width: 190,
      render: (v?: string, r?: HarborTagRow) =>
        v ? (
          <Tooltip title={`${fmt(v)}${r?.timeSource === 'build' ? '（构建时间，不是推送时间）' : ''}`}>
            <span>
              {ago(v)}
              {r?.timeSource && r.timeSource !== 'push' ? (
                <Text type="secondary" style={{ fontSize: 11.5 }}>
                  {' '}
                  （{r.timeSource === 'build' ? '构建' : r.timeSource}）
                </Text>
              ) : null}
            </span>
          </Tooltip>
        ) : (
          <Text type="secondary" style={{ fontSize: 12 }}>
            —
          </Text>
        ),
    },
    {
      title: '操作',
      key: 'act',
      width: 90,
      render: (_: unknown, r: HarborTagRow) => (
        <Button
          size="small"
          type="text"
          icon={<CopyOutlined />}
          onClick={() => navigator.clipboard?.writeText(`${address}:${r.tag}`)}
        >
          复制
        </Button>
      ),
    },
  ];

  return (
    <SectionCard bodyStyle={{ padding: 0 }}>
      <div className="yn-filter-bar">
        <Space size={12} wrap>
          <span style={{ fontSize: 13, color: '#5a6478' }}>
            Harbor 项目
            <Select
              style={{ width: 150, marginLeft: 8 }}
              value={project}
              onChange={(v) => {
                setProject(v);
                setPage(1);
              }}
              options={projectOptions}
              loading={projectsApi.loading}
              // 拉不到列表时提示一下，别让人以为"Harbor 上只有一个 project"
              notFoundContent={projectsApi.error ? '读取 Harbor 项目列表失败' : '没有读到项目'}
            />
          </span>
          <span style={{ fontSize: 13, color: '#5a6478' }}>
            微服务
            <Select
              showSearch
              optionFilterProp="label"
              placeholder="选择平台已纳管的项目"
              style={{ width: 260, marginLeft: 8 }}
              value={serviceKey}
              onChange={(v) => {
                setServiceKey(v);
                setPage(1);
              }}
              options={services.map((s) => ({ label: `${s.name}（${s.key}）`, value: s.key }))}
              loading={servicesApi.loading}
            />
          </span>
          <Button icon={<ReloadOutlined />} loading={loading} disabled={!repo} onClick={() => setReloadAt((n) => n + 1)}>
            重新读取
          </Button>
          {address ? (
            <Tag className="yn-mono" style={{ marginInlineEnd: 0 }}>
              {address}
            </Tag>
          ) : null}
        </Space>
      </div>

      {error ? (
        <div style={{ padding: 16 }}>
          <Alert
            type="error"
            showIcon
            message="读取 Harbor tag 失败"
            description={error}
            action={
              <Button size="small" onClick={() => setReloadAt((n) => n + 1)}>
                重试
              </Button>
            }
          />
        </div>
      ) : null}

      {!repo ? (
        <div style={{ padding: '44px 0', textAlign: 'center' }}>
          <span style={{ fontSize: 13, color: '#8c8c95' }}>
            选一个微服务，我去 <b>{project}</b> 下查它对应的仓库有哪些 tag
          </span>
        </div>
      ) : (
        <Table<HarborTagRow>
          rowKey={(r) => `${r.tag}-${r.digest ?? ''}`}
          size="small"
          loading={loading}
          dataSource={rows}
          columns={columns}
          pagination={{
            current: page,
            pageSize,
            total,
            showSizeChanger: true,
            pageSizeOptions: [10, 20, 50, 100],
            showTotal: (n) => `共 ${n} 个 tag`,
            onChange: (p, ps) => {
              setPage(p);
              setPageSize(ps);
            },
          }}
        />
      )}
    </SectionCard>
  );
};

export default HarborImages;
