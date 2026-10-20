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

  // 镜像仓库域名（拼完整地址用）—— 配在后端，前端拿不到
  const metaApi = useApi(() => imageApi.harborMeta(), []);

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
  /**
   * 完整镜像地址 = registry（配置里的镜像仓库域名）/ project / repo。
   *
   * ⚠️ 必须带 registry。只写 `escloud/xxljobexecutor` 是**仓库内路径**，不是能
   * `docker pull` 的地址 —— 用户要的是后者（2026-10-20 指出）。
   * registry 拿不到时退回项目表里的同名字段，再没有就只显示路径（不编域名）。
   */
  const registry = metaApi.data?.registry || service?.registry || '';
  const address = repo ? (registry ? `${registry}/${project}/${repo}` : `${project}/${repo}`) : '';

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

  /**
   * ⚠️ Digest / 大小 两列**只在真的有值时才出现**。
   *
   * Harbor v1 的 core 接口这两项都不给，于是整列只剩「—」—— 它占了近一半宽度、
   * 一个字符的有效信息都没有，一屏看下去就是"半张表是空的"（2026-10-20 用户的原话：
   * "页面展示效果太low了"）。再花哨的样式也补不回这件事，所以直接不渲染。
   *
   * v2 上这两项有值，列会自动出现 —— 同一个组件，两种 Harbor 各自呈现自己的真实能力。
   */
  const showDigest = rows.some((r) => !!r.digest);
  const showSize = rows.some((r) => (r.size ?? 0) > 0);

  /** 第一行按 push_time 倒序拿的，就是最新那个 —— 给它打个标 */
  const latestTag = rows[0]?.tag;

  const columns: ColumnsType<HarborTagRow> = [
    {
      title: 'Tag',
      dataIndex: 'tag',
      ...(showSize ? { width: 240 } : {}),
      render: (v: string) => (
        <Space size={6}>
          <span className="yn-harbor-tag yn-mono">{v}</span>
          {v === latestTag ? <span className="yn-harbor-latest">最新</span> : null}
        </Space>
      ),
    },
    ...(showDigest
      ? [
          {
            title: 'Digest',
            dataIndex: 'digest',
            render: (v?: string) => (
              <Tooltip title={v}>
                <span className="yn-mono" style={{ fontSize: 12, color: '#7a8290' }}>
                  {v && v.length > 26 ? `${v.slice(0, 26)}…` : v}
                </span>
              </Tooltip>
            ),
          } as ColumnsType<HarborTagRow>[number],
        ]
      : []),
    ...(showSize
      ? [
          {
            title: '大小',
            dataIndex: 'size',
            width: 110,
            render: (v: number) => (v ? humanSize(v) : '—'),
          } as ColumnsType<HarborTagRow>[number],
        ]
      : []),
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

      {repo && rows.length ? (
        <div className="yn-harbor-summary">
          <span>
            共 <b>{total}</b> 个 tag
          </span>
          <span className="yn-harbor-summary__sep" />
          <span>
            最新 <span className="yn-mono" style={{ color: '#1677ff', fontWeight: 500 }}>{latestTag}</span>
          </span>
          {rows[0]?.pushedAt ? (
            <>
              <span className="yn-harbor-summary__sep" />
              <span>{ago(rows[0].pushedAt)}更新</span>
            </>
          ) : null}
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
          className="yn-harbor-table"
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
