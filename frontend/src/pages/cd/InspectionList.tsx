import React, { useMemo, useState } from 'react';
import {
  App as AntApp,
  Button,
  Descriptions,
  Empty,
  InputNumber,
  Modal,
  Select,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  DownloadOutlined,
  EyeOutlined,
  FileSearchOutlined,
  SyncOutlined,
} from '@ant-design/icons';
import { PageHeader, SectionCard } from '@/components/ui';
import { ApiError, environmentApi, inspectionApi } from '@/api';
import { tokenStore } from '@/api/client';
import type { InspectionReportRow } from '@/api/inspection';
import { useApi } from '@/hooks/useApi';
import { fmt } from '@/utils/format';

const { Text } = Typography;

// ---------------------------------------------------------------------------
// 巡检报告
//
// 这个页面承担两件事：
//   1. **存档浏览** —— 每次生成都落一份报告，能翻历史、能对比；
//   2. **下载** —— 报告是自包含 HTML，下载下来双击就能看、能转给别人。
//
// 为什么不把报告正文渲染在页面里而是用 iframe：
// 报告带的是一套独立的配色与字体（要能离线看），塞进当前页面会与平台样式
// 互相污染（`body`/`h2`/`table` 全是全局选择器）。iframe + srcDoc 天然隔离。
// ---------------------------------------------------------------------------

const levelMeta: Record<string, { color: string; text: string; tag: string }> = {
  healthy: { color: '#237804', text: '健康', tag: 'success' },
  warning: { color: '#d46b08', text: '需关注', tag: 'warning' },
  critical: { color: '#cf1322', text: '需处理', tag: 'error' },
};

const metaOf = (level: string) => levelMeta[level] ?? levelMeta.warning;

/** 健康分徽标：数字 + 颜色。分数本身不可解释，所以旁边永远跟着 level 文案。 */
const ScoreBadge: React.FC<{ score: number; level: string }> = ({ score, level }) => {
  const m = metaOf(level);
  return (
    <span style={{ display: 'inline-flex', alignItems: 'baseline', gap: 6 }}>
      <b style={{ fontSize: 17, color: m.color, fontVariantNumeric: 'tabular-nums' }}>{score}</b>
      <Tag color={m.tag} style={{ marginInlineEnd: 0 }}>
        {m.text}
      </Tag>
    </span>
  );
};

const InspectionList: React.FC = () => {
  const { message, notification } = AntApp.useApp();
  const [envFilter, setEnvFilter] = useState<string | undefined>();
  const [running, setRunning] = useState(false);
  const [runOpen, setRunOpen] = useState(false);
  const [runEnv, setRunEnv] = useState<string | undefined>();
  const [hours, setHours] = useState<number>(168);
  const [viewing, setViewing] = useState<{ id: string; html: string; title: string } | null>(null);
  const [viewLoading, setViewLoading] = useState(false);

  const envsApi = useApi(() => environmentApi.list({ pageSize: 100 }), []);
  const envOptions = useMemo(
    () => (envsApi.data?.list ?? []).map((e) => ({ label: `${e.name}（${e.key}）`, value: e.key })),
    [envsApi.data],
  );

  const reportsApi = useApi(() => inspectionApi.list(envFilter), [envFilter]);
  const rows = reportsApi.data ?? [];

  const run = async () => {
    if (!runEnv) {
      message.warning('请先选择要巡检的环境');
      return;
    }
    setRunning(true);
    try {
      const res = await inspectionApi.run(runEnv, hours);
      const r = res.report;
      setRunOpen(false);
      notification.success({
        message: `巡检完成：${r.score} 分（${metaOf(r.level).text}）`,
        description: (
          <div style={{ fontSize: 12.5, lineHeight: 1.8 }}>
            <div>{r.summary}</div>
            <div style={{ color: '#8a909a' }}>
              报告已存档为 #{r.reportId}，可点「查看」看彩色版，或「下载」保存 HTML。
            </div>
          </div>
        ),
      });
      await reportsApi.refresh();
    } catch (e) {
      message.error(e instanceof ApiError ? e.message : '巡检失败');
    } finally {
      setRunning(false);
    }
  };

  const view = async (row: InspectionReportRow) => {
    setViewLoading(true);
    try {
      const d = await inspectionApi.detail(row.id);
      setViewing({ id: d.id, html: d.html, title: `${d.envName} · ${d.createdAt}` });
    } catch (e) {
      message.error(e instanceof ApiError ? e.message : '读取报告失败');
    } finally {
      setViewLoading(false);
    }
  };

  const download = async (row: InspectionReportRow) => {
    try {
      const token = tokenStore.get();
      const res = await fetch(inspectionApi.downloadUrl(row.id), {
        headers: token ? { Authorization: `Bearer ${token}` } : {},
      });
      if (!res.ok) {
        message.error(`下载失败（HTTP ${res.status}）`);
        return;
      }
      const blob = await res.blob();
      // 文件名以服务端 Content-Disposition 为准（它同时给了中文名与 ASCII 兜底）
      let name = `inspection-${row.envKey}-${row.id}.html`;
      const cd = res.headers.get('Content-Disposition') ?? '';
      const m = /filename\*=UTF-8''([^;]+)/i.exec(cd);
      if (m) name = decodeURIComponent(m[1]);
      const a = document.createElement('a');
      a.href = URL.createObjectURL(blob);
      a.download = name;
      document.body.appendChild(a);
      a.click();
      a.remove();
      URL.revokeObjectURL(a.href);
    } catch {
      message.error('下载失败，请检查网络');
    }
  };

  const columns: ColumnsType<InspectionReportRow> = [
    {
      title: '环境',
      key: 'env',
      width: 220,
      render: (_, r) => (
        <div style={{ display: 'flex', flexDirection: 'column', minWidth: 0 }}>
          <Text strong style={{ fontSize: 13 }}>
            {r.envName}
          </Text>
          <Text type="secondary" className="yn-mono" style={{ fontSize: 11.5 }}>
            {r.envKey}
          </Text>
        </div>
      ),
    },
    {
      title: '健康分',
      key: 'score',
      width: 130,
      render: (_, r) => <ScoreBadge score={r.score} level={r.level} />,
    },
    {
      title: '结论',
      key: 'summary',
      render: (_, r) => (
        <span className="yn-ellipsis" title={r.summary} style={{ display: 'block' }}>
          {r.summary}
        </span>
      ),
    },
    {
      title: '生成时间',
      key: 'createdAt',
      width: 160,
      render: (_, r) => <Text type="secondary" style={{ fontSize: 12 }}>{fmt(r.createdAt)}</Text>,
    },
    {
      title: '操作',
      key: 'op',
      width: 150,
      render: (_, r) => (
        <Space size={4}>
          <Button size="small" type="link" icon={<EyeOutlined />} onClick={() => void view(r)}>
            查看
          </Button>
          <Tooltip title="下载 HTML（离线可看，可直接转给别人）">
            <Button size="small" type="link" icon={<DownloadOutlined />} onClick={() => void download(r)}>
              下载
            </Button>
          </Tooltip>
        </Space>
      ),
    },
  ];

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: 'CD 环境部署' }, { title: '巡检报告' }]}
        title="巡检报告"
        description="对某个环境做一次健康体检：集群资源、服务就绪、集群证书到期、近期部署，给出健康分与逐项建议。报告会存档，可下载 HTML 离线查看"
        extra={
          <Space size={8}>
            <Select
              allowClear
              size="small"
              style={{ width: 220 }}
              placeholder="按环境筛选"
              options={envOptions}
              value={envFilter}
              onChange={(v) => setEnvFilter(v)}
            />
            <Tooltip title="重新拉取列表">
              <Button size="small" icon={<SyncOutlined />} onClick={() => void reportsApi.refresh()} />
            </Tooltip>
            <Button
              type="primary"
              size="small"
              icon={<FileSearchOutlined />}
              onClick={() => {
                setRunEnv(envFilter ?? envOptions[0]?.value);
                setRunOpen(true);
              }}
            >
              生成巡检报告
            </Button>
          </Space>
        }
      />

      <SectionCard bodyStyle={{ padding: 0 }}>
        <Table<InspectionReportRow>
          rowKey={(r) => r.id}
          size="small"
          loading={reportsApi.loading}
          dataSource={rows}
          columns={columns}
          pagination={rows.length > 20 ? { pageSize: 20, size: 'small' } : false}
          locale={{
            emptyText: (
              <Empty
                image={Empty.PRESENTED_IMAGE_SIMPLE}
                description="还没有巡检报告。点右上角「生成巡检报告」跑一次。"
              />
            ),
          }}
        />
      </SectionCard>

      {/* 生成 */}
      <Modal
        open={runOpen}
        title="生成巡检报告"
        okText="开始巡检"
        cancelText="取消"
        confirmLoading={running}
        onOk={() => void run()}
        onCancel={() => setRunOpen(false)}
      >
        <Descriptions column={1} size="small" colon={false} style={{ marginTop: 12 }}>
          <Descriptions.Item label="环境">
            <Select style={{ width: 260 }} placeholder="选择环境" options={envOptions} value={runEnv} onChange={setRunEnv} />
          </Descriptions.Item>
          <Descriptions.Item label="统计窗口">
            <Space size={6}>
              <InputNumber min={1} max={720} value={hours} onChange={(v) => setHours(v ?? 168)} />
              <Text type="secondary" style={{ fontSize: 12 }}>
                小时（默认 168 = 7 天，用于统计部署失败率）
              </Text>
            </Space>
          </Descriptions.Item>
        </Descriptions>
        <Text type="secondary" style={{ fontSize: 12 }}>
          巡检只读，不会改动环境。耗时通常 1~3 秒。
        </Text>
      </Modal>

      {/* 查看 */}
      <Modal
        open={!!viewing}
        title={`巡检报告 · ${viewing?.title ?? ''}`}
        width={920}
        footer={
          <Space>
            <Button onClick={() => setViewing(null)}>关闭</Button>
            <Button
              type="primary"
              icon={<DownloadOutlined />}
              onClick={() => {
                const row = rows.find((r) => r.id === viewing?.id);
                if (row) void download(row);
              }}
            >
              下载 HTML
            </Button>
          </Space>
        }
        onCancel={() => setViewing(null)}
        styles={{ body: { padding: 0 } }}
      >
        {viewLoading || !viewing ? (
          <div style={{ padding: 40, textAlign: 'center' }}>
            <Text type="secondary">读取报告…</Text>
          </div>
        ) : (
          // srcDoc + iframe：报告的 CSS 是全局选择器（body/h2/table），
          // 直接塞进页面会污染平台样式；iframe 天然隔离。
          <iframe
            title="inspection-report"
            srcDoc={viewing.html}
            style={{ width: '100%', height: '68vh', border: 'none', borderRadius: 8, background: '#f4f6f9' }}
            sandbox="allow-same-origin"
          />
        )}
      </Modal>
    </div>
  );
};

export default InspectionList;
