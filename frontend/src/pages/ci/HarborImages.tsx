import React, { useEffect, useMemo, useState } from 'react';
import { Alert, Button, Empty, Select, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { CopyOutlined, ReloadOutlined } from '@ant-design/icons';
import { SectionCard } from '@/components/ui';
import { imageApi, projectApi } from '@/api';
import type { HarborTagRow } from '@/api/ci';
import { useApi } from '@/hooks/useApi';
import { fmt, humanSize } from '@/utils/format';

const { Text } = Typography;

/**
 * Harbor 上的仓库项目名，默认 `escloud`。
 *
 * ⚠️ 平台的 `imageName` 分前后端（`backend/userv2`、`frontend/aio-doc`），
 * 但 Harbor 上这些镜像**统一放在 escloud 下**（`escloud/userv2`）——
 * 直接拿 imageName 拼地址会显示成 `backend/userv2`，而那仓库根本不存在
 * （2026-10-20 用户报的就是这个）。所以地址 = `registry + escloud + 镜像名最后一段`。
 *
 * 后端有一份同名配置（`harbor.project`），默认值也是 escloud ——
 * 两边必须一致；以后要改成可配，两处一起改。
 */
const HARBOR_PROJECT = 'escloud';

/** `backend/userv2` → `userv2`；`escloud/aio-mcp-server` → `aio-mcp-server` */
const repoOf = (imageName?: string) => (imageName || '').split('/').filter(Boolean).pop() || '';

/**
 * 「Harbor 仓库」视角：**实时**列仓库里某个项目镜像的真实 tag。
 *
 * 为什么它必须与「平台构建记录」分开（2026-10-20 用户提"镜像版本好像没接入 harbor"）：
 *
 * 那个视角列的是 `image_record` —— 只有**平台自己构建并登记过**的镜像。
 * Jenkins 推上去的、别人手工 push 的、更早的历史 tag，在那里一个都看不到，
 * 而页面标题就叫"镜像版本"，很容易让人以为那是仓库里的全部。
 *
 * 两个视角回答的是不同问题：
 *   · 平台记录 → "我发过什么、当时用的是哪个提交"（能点进日志）
 *   · Harbor 仓库 → "仓库里现在到底有什么"（含不是平台推的）
 * 所以**不合并**：混在一屏会让人分不清某一行是"平台发的"还是"别处推的"。
 */
const HarborImages: React.FC = () => {
  const [projectKey, setProjectKey] = useState<string | undefined>();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);
  const [rows, setRows] = useState<HarborTagRow[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [reloadAt, setReloadAt] = useState(0);

  const projectsApi = useApi(() => projectApi.list({ pageSize: 200 }), []);
  const projects = useMemo(() => projectsApi.data?.list ?? [], [projectsApi.data]);

  useEffect(() => {
    if (!projectKey) {
      setRows([]);
      setTotal(0);
      setError(null);
      return;
    }
    let alive = true;
    setLoading(true);
    setError(null);
    imageApi
      .harborTags(projectKey, page, pageSize)
      .then((r) => {
        if (!alive) return;
        setRows(r.list ?? []);
        setTotal(r.total ?? 0);
      })
      .catch((e) => {
        if (!alive) return;
        setRows([]);
        setTotal(0);
        setError(e?.message ?? '读取 Harbor 失败');
      })
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [projectKey, page, pageSize, reloadAt]);

  const project = projects.find((p) => p.key === projectKey);

  const columns: ColumnsType<HarborTagRow> = [
    {
      title: 'Tag',
      dataIndex: 'tag',
      width: 240,
      render: (v: string) => <span className="yn-mono" style={{ fontWeight: 600 }}>{v}</span>,
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
          // v1 的 core 接口给不出 digest —— 如实写"未提供"，不拿别的东西顶上
          <Text type="secondary" style={{ fontSize: 12 }}>该 Harbor 未提供</Text>
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
      // 200：`2026-10-15 14:37:31（构建）` 一行放得下，不会折成两行
      width: 200,
      render: (v?: string, r?: HarborTagRow) =>
        v ? (
          <span>
            {fmt(v)}
            {/* 时间口径要说清楚：Harbor v1 只有"构建时间"，不是"推送到仓库的时间" */}
            {r?.timeSource && r.timeSource !== 'push' ? (
              <Text type="secondary" style={{ fontSize: 11.5 }}>
                {' '}
                （{r.timeSource === 'build' ? '构建' : r.timeSource}）
              </Text>
            ) : null}
          </span>
        ) : (
          <Text type="secondary" style={{ fontSize: 12 }}>—</Text>
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
          onClick={() => {
            const full = `${project?.registry ?? ''}/${HARBOR_PROJECT}/${repoOf(project?.imageName)}:${r.tag}`.replace(/^\/+/, '');
            navigator.clipboard?.writeText(full);
          }}
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
          <Select
            showSearch
            optionFilterProp="label"
            placeholder="选择项目（必选）"
            style={{ width: 280 }}
            value={projectKey}
            onChange={(v) => {
              setProjectKey(v);
              setPage(1);
            }}
            options={projects.map((p) => ({ label: `${p.name}（${p.key}）`, value: p.key }))}
          />
          <Button
            icon={<ReloadOutlined />}
            loading={loading}
            disabled={!projectKey}
            onClick={() => setReloadAt((n) => n + 1)}
          >
            重新读取
          </Button>
          {project ? (
            <Tag style={{ marginInlineEnd: 0 }} className="yn-mono">
              {project.registry}/{HARBOR_PROJECT}/{repoOf(project.imageName)}
            </Tag>
          ) : null}
        </Space>
      </div>

      {error ? (
        <div style={{ padding: 16 }}>
          <Alert
            type="error"
            showIcon
            message="读取 Harbor 失败"
            description={error}
            action={
              <Button size="small" onClick={() => setReloadAt((n) => n + 1)}>
                重试
              </Button>
            }
          />
        </div>
      ) : null}

      {!projectKey ? (
        <div style={{ padding: '48px 0' }}>
          <Empty
            description={
              <span style={{ fontSize: 13, color: '#8c8c95' }}>
                Harbor 按仓库查 tag —— 先选一个项目，我去仓库把它真实存在的 tag 列出来
              </span>
            }
          />
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
            pageSizeOptions: [10, 20, 50],
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
