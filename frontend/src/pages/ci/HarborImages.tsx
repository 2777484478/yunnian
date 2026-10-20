import React, { useEffect, useMemo, useState } from 'react';
import { Alert, Button, Empty, Input, Space, Table, Tag, Tooltip, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { ArrowLeftOutlined, CopyOutlined, ReloadOutlined } from '@ant-design/icons';
import { SectionCard } from '@/components/ui';
import { imageApi } from '@/api';
import type { HarborRepoRow, HarborTagRow } from '@/api/ci';
import { fmt, humanSize } from '@/utils/format';

const { Text } = Typography;

/**
 * Harbor 上的仓库项目名，默认 `escloud`。
 *
 * ⚠️ 平台的 `imageName` 分前后端（`backend/userv2`、`frontend/aio-doc`），
 * 但 Harbor 上这些镜像**统一放在 escloud 下**（`escloud/userv2`）。
 * 拿 imageName 拼地址会得到 `backend/userv2` —— 那仓库不存在
 * （2026-10-20 用户报的正是这个）。
 *
 * 做成可改的输入框：以后 Harbor 上若分了别的 project，不用改代码。
 * 后端对应配置是 `harbor.project`，默认值同样是 escloud。
 */
const DEFAULT_PROJECT = 'escloud';

/**
 * 「Harbor 仓库」视角。
 *
 * 两级视图（2026-10-20 用户要求"加一个读取 harbor 项目的所有接口，点击显示所有项目"）：
 *   ① 仓库列表 —— 直接列 Harbor 某个 project 下的**全部仓库**，不依赖平台项目记录
 *   ② 某个仓库的 tag —— 点进去才查
 *
 * 为什么不能只做"先选平台项目"：那样想看仓库里有什么，得先在脑子里把项目名映射成
 * 仓库名，还得祈祷 `image_name` 拼对了。而这个视角的意义恰恰是**绕过平台记录看仓库真相**
 *（含 Jenkins 推的、别人手工 push 的、平台根本不认识的镜像）。
 */
const HarborImages: React.FC = () => {
  const [project, setProject] = useState(DEFAULT_PROJECT);
  /** 当前打开 tag 列表的仓库（null = 停在仓库列表这一级） */
  const [repo, setRepo] = useState<string | null>(null);

  const [repos, setRepos] = useState<HarborRepoRow[]>([]);
  const [repoTotal, setRepoTotal] = useState(0);
  const [repoPage, setRepoPage] = useState(1);

  const [tags, setTags] = useState<HarborTagRow[]>([]);
  const [tagTotal, setTagTotal] = useState(0);
  const [tagPage, setTagPage] = useState(1);
  const [pageSize, setPageSize] = useState(20);

  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [reloadAt, setReloadAt] = useState(0);

  // ① 仓库列表（只在第一级拉）
  useEffect(() => {
    if (repo) return;
    let alive = true;
    setLoading(true);
    setError(null);
    imageApi
      .harborRepos(project, repoPage, 50)
      .then((r) => {
        if (!alive) return;
        setRepos(r.list ?? []);
        setRepoTotal(r.total ?? 0);
      })
      .catch((e) => {
        if (!alive) return;
        setRepos([]);
        setRepoTotal(0);
        setError(e?.message ?? '读取 Harbor 仓库列表失败');
      })
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [project, repoPage, repo, reloadAt]);

  // ② 某个仓库的 tag
  useEffect(() => {
    if (!repo) return;
    let alive = true;
    setLoading(true);
    setError(null);
    imageApi
      .harborTags(project, repo, tagPage, pageSize)
      .then((r) => {
        if (!alive) return;
        setTags(r.list ?? []);
        setTagTotal(r.total ?? 0);
      })
      .catch((e) => {
        if (!alive) return;
        setTags([]);
        setTagTotal(0);
        setError(e?.message ?? '读取 Harbor tag 失败');
      })
      .finally(() => {
        if (alive) setLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [project, repo, tagPage, pageSize, reloadAt]);

  const address = useMemo(() => `${project}/${repo ?? ''}`, [project, repo]);

  const repoColumns: ColumnsType<HarborRepoRow> = [
    {
      title: '仓库',
      dataIndex: 'name',
      render: (v: string) => (
        <span className="yn-mono" style={{ fontWeight: 600 }}>
          {v}
        </span>
      ),
    },
    {
      title: 'Tag 数',
      dataIndex: 'tagCount',
      width: 110,
      render: (v: number) =>
        // 0 可能是"真的没有 tag"，也可能是 v1 不回收 artifact_count —— 显示 — 而不是编一个数
        typeof v === 'number' && v > 0 ? <Tag color="blue">{v}</Tag> : <Text type="secondary">—</Text>,
    },
    {
      title: '操作',
      key: 'act',
      width: 110,
      render: (_: unknown, r: HarborRepoRow) => (
        <Button
          size="small"
          type="link"
          onClick={() => {
            setRepo(r.name);
            setTagPage(1);
          }}
        >
          查看 tag
        </Button>
      ),
    },
  ];

  const tagColumns: ColumnsType<HarborTagRow> = [
    {
      title: 'Tag',
      dataIndex: 'tag',
      width: 240,
      render: (v: string) => (
        <span className="yn-mono" style={{ fontWeight: 600 }}>
          {v}
        </span>
      ),
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
          // v1 的 core 接口给不出 digest —— 如实写，不拿别的东西顶上
          <Text type="secondary" style={{ fontSize: 12 }}>
            该 Harbor 未提供
          </Text>
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
      width: 200,
      render: (v?: string, r?: HarborTagRow) =>
        v ? (
          <span>
            {fmt(v)}
            {/* 时间口径说清楚：v1 只有构建时间，不是推送到仓库的时间 */}
            {r?.timeSource && r.timeSource !== 'push' ? (
              <Text type="secondary" style={{ fontSize: 11.5 }}>
                {' '}
                （{r.timeSource === 'build' ? '构建' : r.timeSource}）
              </Text>
            ) : null}
          </span>
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
          {/* 左边是"选什么"，右边是"看到了什么" —— 顺序就是用户 2026-10-20 指出的那个 */}
          <span style={{ fontSize: 13, color: '#5a6478' }}>
            Harbor 项目
            <Input
              style={{ width: 150, marginLeft: 8 }}
              value={project}
              onChange={(e) => {
                const v = e.target.value.trim();
                setProject(v || DEFAULT_PROJECT);
                setRepo(null);
                setRepoPage(1);
              }}
              placeholder={DEFAULT_PROJECT}
            />
          </span>
          <Button icon={<ReloadOutlined />} loading={loading} onClick={() => setReloadAt((n) => n + 1)}>
            重新读取
          </Button>
          {repo ? (
            <>
              <Button icon={<ArrowLeftOutlined />} type="link" onClick={() => setRepo(null)}>
                返回仓库列表
              </Button>
              <Tag className="yn-mono" style={{ marginInlineEnd: 0 }}>
                {address}
              </Tag>
            </>
          ) : (
            <Tag style={{ marginInlineEnd: 0 }}>该 Harbor 项目下共 {repoTotal} 个仓库</Tag>
          )}
        </Space>
      </div>

      {error ? (
        <div style={{ padding: 16 }}>
          <Alert
            type="error"
            showIcon
            message={repo ? '读取 Harbor tag 失败' : '读取 Harbor 仓库列表失败'}
            description={error}
            action={
              <Button size="small" onClick={() => setReloadAt((n) => n + 1)}>
                重试
              </Button>
            }
          />
        </div>
      ) : null}

      {repo ? (
        <Table<HarborTagRow>
          rowKey={(r) => `${r.tag}-${r.digest ?? ''}`}
          size="small"
          loading={loading}
          dataSource={tags}
          columns={tagColumns}
          pagination={{
            current: tagPage,
            pageSize,
            total: tagTotal,
            showSizeChanger: true,
            pageSizeOptions: [10, 20, 50, 100],
            showTotal: (n) => `共 ${n} 个 tag`,
            onChange: (p, ps) => {
              setTagPage(p);
              setPageSize(ps);
            },
          }}
        />
      ) : (
        <Table<HarborRepoRow>
          rowKey={(r) => r.fullName}
          size="small"
          loading={loading}
          dataSource={repos}
          columns={repoColumns}
          locale={{
            emptyText: (
              <Empty
                description={
                  <span style={{ fontSize: 13, color: '#8c8c95' }}>
                    项目 <b>{project}</b> 下没有读到仓库 —— 确认 Harbor 上项目名是否正确
                  </span>
                }
              />
            ),
          }}
          pagination={
            repoTotal > 50
              ? {
                  current: repoPage,
                  pageSize: 50,
                  total: repoTotal,
                  showTotal: (n) => `共 ${n} 个仓库`,
                  onChange: (p) => setRepoPage(p),
                }
              : false
          }
        />
      )}
    </SectionCard>
  );
};

export default HarborImages;
