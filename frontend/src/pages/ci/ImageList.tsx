import React, { useEffect, useMemo, useState } from 'react';
import { Alert, App as AntApp, Button, Col, Input, Row, Segmented, Select, Space, Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { CopyOutlined, DeleteOutlined, HddOutlined, ReloadOutlined, SearchOutlined } from '@ant-design/icons';
import { Link } from 'react-router-dom';
import { PageHeader, SectionCard, StatCard } from '@/components/ui';
import { DigestText, ImageText } from '@/components/StatusTag';
import { environmentApi, imageApi, projectApi } from '@/api';
import { useApi } from '@/hooks/useApi';
import { humanSize } from '@/utils/format';
import type { Environment, ImageRecord } from '@/types';
import HarborImages from './HarborImages';

const { Text } = Typography;

/** 环境标签配色：正式区红、预发橙、其余蓝——与拓扑页的语义保持一致 */
const envColor = (key: string) => (key === 'prod' ? 'red' : key === 'staging' ? 'orange' : 'blue');

const ImageList: React.FC = () => {
  const { message, modal } = AntApp.useApp();

  /**
   * 两个视角回答的是不同问题，不能混：
   *   record → 平台记录（我发过什么、当时是哪个提交）
   *   harbor → 仓库实况（现在到底有什么，含 Jenkins / 别人手工推的）
   */
  const [view, setView] = useState<'record' | 'harbor'>('record');
  const [keywordInput, setKeywordInput] = useState('');
  const [keyword, setKeyword] = useState('');
  const [projectFilter, setProjectFilter] = useState<string | undefined>();
  const [envFilter, setEnvFilter] = useState<string | undefined>();
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(12);

  useEffect(() => {
    const t = setTimeout(() => {
      setKeyword(keywordInput.trim());
      setPage(1);
    }, 350);
    return () => clearTimeout(t);
  }, [keywordInput]);

  // 全部筛选走后端：/images 支持 keyword / projectKey / env / page / pageSize
  const imagesApi = useApi(
    () =>
      imageApi.list({
        keyword,
        projectKey: projectFilter,
        env: envFilter,
        page,
        pageSize,
      }),
    [keyword, projectFilter, envFilter, page, pageSize],
  );
  const images = useMemo(() => imagesApi.data?.list ?? [], [imagesApi.data]);
  const total = imagesApi.data?.total ?? 0;

  const projectsApi = useApi(() => projectApi.list({ pageSize: 200 }), []);
  const projects = useMemo(() => projectsApi.data?.list ?? [], [projectsApi.data]);

  const envsApi = useApi(() => environmentApi.list(), []);
  const envs = useMemo<Environment[]>(() => envsApi.data?.list ?? [], [envsApi.data]);

  const refreshAll = () => {
    imagesApi.refresh();
    envsApi.refresh();
  };

  // 统计卡：基于当前页数据聚合。
  // 这三个指标（存储占用/最大镜像/已部署数）后端没有专门的汇总接口，
  // 而镜像是分页拉取的——所以标签要写明是"当前页"，不能让人误以为是全量。
  const pageStorage = images.reduce((n, i) => n + (i.size || 0), 0);
  const largest = useMemo(() => [...images].sort((a, b) => (b.size || 0) - (a.size || 0))[0], [images]);
  const deployedCount = images.filter((i) => (i.deployedEnvs?.length ?? 0) > 0).length;

  const copyImage = (r: ImageRecord) => {
    navigator.clipboard?.writeText(r.image);
    message.success('已复制镜像地址');
  };

  const cleanup = async (r: ImageRecord) => {
    // 先问一次引用情况：正在被环境使用的版本后端会拒绝删除（409）。
    // 提前告知比让用户点完"确认"再吃一个报错好得多。
    const usage = await imageApi.usedBy(r.id).catch(() => null);
    const inUse = usage?.inUse ?? false;
    const usedByText = (usage?.services ?? []).map((s) => `${s.envKey}/${s.service}`).join('、');

    modal.confirm({
      title: inUse ? '该镜像正在被使用，无法清理' : '确认清理该镜像版本？',
      content: (
        <span style={{ fontSize: 13 }}>
          {inUse ? (
            <>
              以下服务实例正在使用 <span className="yn-mono">{r.tag}</span>：{usedByText}。
              请先切换这些服务的版本，再清理该镜像。
            </>
          ) : (
            <>
              将删除镜像标签 <span className="yn-mono">{r.tag}</span>。此操作不可撤销。
            </>
          )}
        </span>
      ),
      okText: '确认清理',
      okButtonProps: { danger: true, disabled: inUse },
      onOk: async () => {
        await imageApi.remove(r.id);
        message.success('镜像已清理');
        refreshAll();
      },
    });
  };

  const columns: ColumnsType<ImageRecord> = [
    {
      title: '镜像',
      dataIndex: 'image',
      width: 360,
      render: (v, r) => (
        <div>
          <ImageText value={v} width={340} />
          <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 3 }}>
            <Link to={`/ci/projects/${r.projectKey}?tab=images`}>{r.projectName}</Link>
            {r.buildNo ? ` · 构建 #${r.buildNo}` : ''} · {r.branch}
          </div>
        </div>
      ),
    },
    { title: 'Tag', dataIndex: 'tag', width: 220, render: (v) => <span className="yn-mono" style={{ fontSize: 12.5 }}>{v}</span> },
    { title: 'Digest', dataIndex: 'digest', width: 190, render: (v) => <DigestText value={v} width={180} /> },
    { title: 'Commit', dataIndex: 'commitId', width: 92, render: (v) => <span className="yn-mono">{v}</span> },
    {
      title: '大小',
      dataIndex: 'size',
      width: 90,
      // 后端给的是**字节**，直接拼 MB 会差 6 个数量级
      render: (v) => humanSize(v),
    },
    {
      title: '已部署环境',
      dataIndex: 'deployedEnvs',
      width: 200,
      render: (v: string[] | undefined) =>
        v?.length ? (
          <Space size={4} wrap>
            {v.map((e) => (
              <Tag key={e} color={envColor(e)} style={{ marginInlineEnd: 0 }}>
                {envs.find((x) => x.key === e)?.name ?? e}
              </Tag>
            ))}
          </Space>
        ) : (
          <Text type="secondary">未部署</Text>
        ),
    },
    // 刻意没有「拉取次数」列：registry v2（当前唯一可用的读路径）不提供拉取统计，
    // 要它得走 Harbor Core 管理接口，后端因此两处硬编码 0。
    // 摆一列恒为 0 的数字比不摆更误导——用户会以为"这镜像没人拉过"。
    {
      title: '推送时间',
      dataIndex: 'pushedAt',
      width: 168,
      render: (v) => <span className="yn-mono" style={{ color: '#8c8c8c' }}>{v}</span>,
    },
    {
      title: '操作',
      key: 'action',
      fixed: 'right',
      width: 150,
      render: (_, r) => (
        <Space size={2}>
          <Button
            type="link"
            size="small"
            icon={<CopyOutlined />}
            style={{ paddingInline: 4 }}
            onClick={() => copyImage(r)}
          >
            复制
          </Button>
          <Button
            type="link"
            size="small"
            danger
            icon={<DeleteOutlined />}
            style={{ paddingInline: 4 }}
            onClick={() => cleanup(r)}
          >
            清理
          </Button>
        </Space>
      ),
    },
  ];

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: 'CI 镜像构建' }, { title: '镜像版本' }]}
        title="镜像版本"
        description="平台构建产出的全部镜像标签，可按项目与环境检索，支持跳转 Harbor 与清理过期版本"
        extra={
          <Space>
            <Segmented
              value={view}
              onChange={(v) => setView(v as 'record' | 'harbor')}
              options={[
                { label: '平台构建记录', value: 'record' },
                { label: 'Harbor 仓库', value: 'harbor' },
              ]}
            />
            {view === 'record' ? (
              <Button icon={<ReloadOutlined />} onClick={refreshAll} loading={imagesApi.loading} />
            ) : null}
          </Space>
        }
      />

      {view === 'harbor' ? (
        <HarborImages />
      ) : (
        <>
      {imagesApi.error ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message="镜像列表加载失败"
          description={imagesApi.error.message}
          action={<Button size="small" onClick={refreshAll}>重试</Button>}
        />
      ) : null}

      <Row gutter={[16, 16]}>
        <Col xs={12} xl={6}>
          <StatCard
            title={`镜像版本总数${total > pageSize ? '（共）' : ''}`}
            value={total}
            unit="个"
            icon={<HddOutlined />}
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="本页存储占用"
            value={humanSize(pageStorage)}
            accent="#722ed1"
            footer={<Text type="secondary" style={{ fontSize: 12 }}>按当前 {images.length} 条记录统计</Text>}
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="本页最大镜像"
            value={largest ? humanSize(largest.size) : '—'}
            accent="#faad14"
            footer={<Text type="secondary" style={{ fontSize: 12 }}>{largest?.projectName ?? '—'}</Text>}
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="本页已部署版本"
            value={deployedCount}
            unit="个"
            accent="#52c41a"
            footer={<Text type="secondary" style={{ fontSize: 12 }}>其余为未部署的历史版本</Text>}
          />
        </Col>
      </Row>

      <SectionCard style={{ marginTop: 16 }} bodyStyle={{ padding: 0 }}>
        <div className="yn-filter-bar">
          <Space size={12} wrap>
            <Input
              allowClear
              prefix={<SearchOutlined style={{ color: '#b5bac4' }} />}
              placeholder="搜索镜像地址 / Tag / Commit"
              style={{ width: 280 }}
              value={keywordInput}
              onChange={(e) => setKeywordInput(e.target.value)}
            />
            <Select
              allowClear
              showSearch
              placeholder="选择项目"
              style={{ width: 200 }}
              value={projectFilter}
              onChange={(v) => {
                setProjectFilter(v);
                setPage(1);
              }}
              optionFilterProp="label"
              options={projects.map((p) => ({ label: p.name, value: p.key }))}
            />
            <Select
              allowClear
              placeholder="按已部署环境筛选"
              style={{ width: 180 }}
              value={envFilter}
              onChange={(v) => {
                setEnvFilter(v);
                setPage(1);
              }}
              options={envs.map((e) => ({ label: e.name, value: e.key }))}
            />
          </Space>
          <Text type="secondary" style={{ fontSize: 12 }}>共 {total} 个镜像版本</Text>
        </div>
        <Table<ImageRecord>
          rowKey="id"
          size="middle"
          loading={imagesApi.loading}
          dataSource={images}
          columns={columns}
          scroll={{ x: 1680 }}
          pagination={{
            current: page,
            pageSize,
            total,
            showSizeChanger: true,
            showTotal: (t) => `共 ${t} 个镜像版本`,
            onChange: (p, ps) => {
              setPage(p);
              setPageSize(ps);
            },
          }}
        />
      </SectionCard>
        </>
      )}
    </div>
  );
};

export default ImageList;
