import React, { useMemo, useState } from 'react';
import {
  App as AntApp,
  Button,
  Empty,
  Input,
  Modal,
  Popconfirm,
  Select,
  Space,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import { CopyOutlined, DeleteOutlined, EditOutlined, EyeOutlined, PlusOutlined } from '@ant-design/icons';
import { manifestApi } from '@/api/ci';
import { environmentApi } from '@/api/cd';
import type { ProjectManifest } from '@/types';
import { hasPerm } from '@/auth';
import { useApi } from '@/hooks/useApi';

const { Text } = Typography;

/** kind → 展示色。只影响观感，不参与任何判断。 */
const KIND_COLOR: Record<string, string> = {
  Deployment: 'blue',
  StatefulSet: 'geekblue',
  DaemonSet: 'purple',
  Service: 'green',
  Ingress: 'cyan',
  ConfigMap: 'gold',
  Secret: 'volcano',
  HPA: 'magenta',
  Job: 'orange',
  CronJob: 'orange',
};

/** 新建时给的起手模板。
 *
 * 为什么带注释的模板而不是空框：YAML 最难的部分不是语法，而是"该写哪些字段"。
 * 给一份能直接改的骨架，比让用户从 `kubectl explain` 查起省得多。
 * 但模板只是**起点**，保存的是用户最终写的正文，不会被"格式化"覆盖。
 */
const TEMPLATE_DEPLOYMENT = `apiVersion: apps/v1
kind: Deployment
metadata:
  # 资源名建议与项目标识一致 —— 平台靠它定位集群里的这份资源
  name: my-service
  namespace: default
  labels:
    app: my-service
spec:
  replicas: 1
  selector:
    matchLabels:
      app: my-service
  template:
    metadata:
      labels:
        app: my-service
    spec:
      containers:
        - name: my-service
          # 镜像 tag 可以被镜像发布覆盖；这里写一个占位版本即可
          image: es-repo.digiwincloud.com.cn:5000/escloud/my-service:latest
          ports:
            - containerPort: 8080
          resources:
            requests:
              cpu: 100m
              memory: 256Mi
            limits:
              cpu: 1000m
              memory: 1Gi
`;

const TEMPLATE_SERVICE = `apiVersion: v1
kind: Service
metadata:
  name: my-service
  namespace: default
spec:
  selector:
    app: my-service
  ports:
    - name: http
      port: 8080
      targetPort: 8080
  type: ClusterIP
`;

/**
 * 项目详情里的「K8s 资源」页签。
 *
 * 这一期只做「存 + 看」：把项目在各环境的 Deployment / Service … 收进平台。
 * **不提供任何写集群的按钮** —— 点了保存只动平台库，集群里已有的资源不会变。
 * 这一点在页面上要明确说，否则用户看到"保存成功"会以为集群也跟着变了。
 */
const ProjectManifests: React.FC<{ projectKey: string }> = ({ projectKey }) => {
  const { message } = AntApp.useApp();
  const canWrite = hasPerm('project:write') || hasPerm('project:write:own');

  const [envFilter, setEnvFilter] = useState<string>('');
  const envsApi = useApi(() => environmentApi.list({ pageSize: 100 }), []);
  const envs = envsApi.data?.list ?? [];

  const listApi = useApi(
    () => manifestApi.list(projectKey, { envKey: envFilter || undefined }),
    [projectKey, envFilter],
  );
  const rows = listApi.data ?? [];

  // ---- 编辑 ----
  const [editing, setEditing] = useState<ProjectManifest | null>(null);
  const [open, setOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  const [formEnv, setFormEnv] = useState('');
  const [formContent, setFormContent] = useState('');
  const [formNote, setFormNote] = useState('');

  // ---- 查看 ----
  const [viewing, setViewing] = useState<ProjectManifest | null>(null);

  const envOptions = useMemo(
    () => envs.map((e) => ({ value: e.key, label: `${e.name}（${e.key}）` })),
    [envs],
  );

  const openNew = () => {
    setEditing(null);
    // 环境**不自动选**。
    //
    // 原来默认 `envs[0]?.key`，而环境列表的第一项是「阿里云生产环境」——
    // 手一快点了保存，YAML 就落到生产那一档去，不报错、看起来还一切正常。
    // 这一期只存不写集群所以不会真出事，但后期开了 apply，这个默认值就成了真坑。
    // 让用户自己选一次，成本 1 秒，换掉一个静默写错的可能。
    //
    // 例外：顶部已经筛了某个环境时（envFilter），沿用它是**用户明确的意图**，
    // 不算默认值。
    setFormEnv(envFilter || '');
    setFormContent(TEMPLATE_DEPLOYMENT);
    setFormNote('');
    setOpen(true);
  };

  const openEdit = (m: ProjectManifest) => {
    setEditing(m);
    setFormEnv(m.envKey);
    setFormContent(m.content);
    setFormNote(m.note);
    setOpen(true);
  };

  const save = async () => {
    if (!formEnv) {
      message.warning('请先选择环境：同一份资源在不同环境通常不一样');
      return;
    }
    if (!formContent.trim()) {
      message.warning('YAML 内容不能为空');
      return;
    }
    setSaving(true);
    try {
      const body = { envKey: formEnv, content: formContent, note: formNote };
      if (editing) {
        await manifestApi.update(projectKey, editing.id, body);
        message.success('已保存（只改了平台记录，集群里的资源没动）');
      } else {
        await manifestApi.create(projectKey, body);
        message.success('已保存（只存进平台，还没有应用到集群）');
      }
      setOpen(false);
      listApi.refresh();
    } catch (e) {
      // 后端会把 YAML 解析错误、kind 冲突等原因写进 message，原样展示 ——
      // 这才是用户能照着改的东西，笼统的"保存失败"等于让他自己猜。
      message.error((e as Error)?.message ?? '保存失败');
    } finally {
      setSaving(false);
    }
  };

  const remove = async (m: ProjectManifest) => {
    try {
      await manifestApi.remove(projectKey, m.id);
      message.success('已删除记录（集群里的资源不受影响）');
      listApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message ?? '删除失败');
    }
  };

  const copy = async (text: string) => {
    try {
      await navigator.clipboard.writeText(text);
      message.success('已复制');
    } catch {
      message.warning('浏览器不允许复制，请手动选中');
    }
  };

  const columns = [
    {
      title: '资源',
      key: 'resource',
      width: 300,
      render: (_: unknown, m: ProjectManifest) => (
        <Space size={8}>
          <Tag color={KIND_COLOR[m.kind] ?? 'default'} style={{ marginInlineEnd: 0 }}>
            {m.kind}
          </Tag>
          <span className="yn-mono" style={{ fontWeight: 500 }}>
            {m.name}
          </span>
        </Space>
      ),
    },
    {
      title: 'apiVersion',
      dataIndex: 'apiVersion',
      width: 130,
      render: (v: string) => (
        <Text type="secondary" className="yn-mono" style={{ fontSize: 12 }}>
          {v || '—'}
        </Text>
      ),
    },
    {
      title: '环境',
      dataIndex: 'envKey',
      width: 150,
      render: (v: string) => <span className="yn-mono">{v}</span>,
    },
    {
      title: '备注',
      dataIndex: 'note',
      render: (v: string) => (v ? v : <Text type="secondary">—</Text>),
      ellipsis: true,
    },
    {
      title: '更新时间',
      dataIndex: 'updatedAt',
      width: 170,
      render: (v: string) => (
        <Text type="secondary" style={{ fontSize: 12 }}>
          {v ? new Date(v).toLocaleString('zh-CN') : '—'}
        </Text>
      ),
    },
    {
      title: '行数',
      key: 'lines',
      width: 70,
      render: (_: unknown, m: ProjectManifest) => (
        <Text type="secondary" style={{ fontSize: 12 }}>
          {m.content.split('\n').length}
        </Text>
      ),
    },
    {
      title: '操作',
      key: 'action',
      width: 150,
      render: (_: unknown, m: ProjectManifest) => (
        <Space size={2}>
          <Tooltip title="查看">
            <Button type="text" size="small" icon={<EyeOutlined />} onClick={() => setViewing(m)} />
          </Tooltip>
          {canWrite ? (
            <Tooltip title="编辑">
              <Button type="text" size="small" icon={<EditOutlined />} onClick={() => openEdit(m)} />
            </Tooltip>
          ) : null}
          {canWrite ? (
            <Popconfirm
              title="删除这份定义？"
              description={
                <span>
                  只删平台里的记录，
                  <br />
                  集群里已存在的资源<b>不会</b>被删除。
                </span>
              }
              okText="删除"
              okButtonProps={{ danger: true }}
              onConfirm={() => void remove(m)}
            >
              <Button type="text" size="small" danger icon={<DeleteOutlined />} />
            </Popconfirm>
          ) : null}
        </Space>
      ),
    },
  ];

  return (
    <div>
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          gap: 12,
          marginBottom: 12,
          flexWrap: 'wrap',
        }}
      >
        <Select
          allowClear
          placeholder="全部环境"
          style={{ width: 260 }}
          value={envFilter || undefined}
          onChange={(v) => setEnvFilter(v ?? '')}
          options={envOptions}
        />
        <Text type="secondary" style={{ fontSize: 12 }}>
          共 {rows.length} 份定义
        </Text>
        <div style={{ flex: '1 1 auto' }} />
        <Text type="secondary" style={{ fontSize: 12 }}>
          这一期只保存到平台，<b>不会</b>应用到集群
        </Text>
        {canWrite ? (
          <Button type="primary" icon={<PlusOutlined />} onClick={openNew}>
            新增资源
          </Button>
        ) : null}
      </div>

      <Table<ProjectManifest>
        rowKey="id"
        size="middle"
        loading={listApi.loading}
        dataSource={rows}
        columns={columns}
        pagination={false}
        locale={{
          emptyText: (
            <Empty
              image={Empty.PRESENTED_IMAGE_SIMPLE}
              description={
                <span>
                  这个项目还没有登记任何 k8s 资源
                  <br />
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    把 Deployment / Service 的 YAML 存进来，以后发布前能直接看到该环境该有什么
                  </Text>
                </span>
              }
            />
          ),
        }}
      />

      {/* ---------------- 编辑 ---------------- */}
      <Modal
        open={open}
        title={editing ? `编辑：${editing.kind} ${editing.name}` : '新增 k8s 资源'}
        width={860}
        onCancel={() => setOpen(false)}
        onOk={() => void save()}
        okText="保存"
        confirmLoading={saving}
        destroyOnClose
      >
        <div style={{ display: 'flex', flexDirection: 'column', gap: 10 }}>
          <Space size={12} wrap>
            <span style={{ width: 64, textAlign: 'right', color: '#5b6169' }}>环境</span>
            <Select
              style={{ width: 360 }}
              placeholder="请选择环境（同一份资源在不同环境通常不一样）"
              value={formEnv || undefined}
              onChange={setFormEnv}
              options={envOptions}
              disabled={!!editing}
            />
            {editing ? (
              <Text type="secondary" style={{ fontSize: 12 }}>
                环境不可改；要换环境请新建一份
              </Text>
            ) : null}
          </Space>

          {!editing ? (
            <Space size={12} wrap>
              <span style={{ width: 64, textAlign: 'right', color: '#5b6169' }}>模板</span>
              <Button size="small" onClick={() => setFormContent(TEMPLATE_DEPLOYMENT)}>
                Deployment
              </Button>
              <Button size="small" onClick={() => setFormContent(TEMPLATE_SERVICE)}>
                Service
              </Button>
              <Text type="secondary" style={{ fontSize: 12 }}>
                只是起手模板，保存的是你最终写的正文（不做格式化）
              </Text>
            </Space>
          ) : null}

          <div>
            <div style={{ marginBottom: 6, color: '#5b6169' }}>
              YAML 内容
              <Text type="secondary" style={{ fontSize: 12, marginLeft: 10 }}>
                一次只能一个资源；kind / name 由内容自动识别
              </Text>
            </div>
            <Input.TextArea
              value={formContent}
              onChange={(e) => setFormContent(e.target.value)}
              autoSize={{ minRows: 16, maxRows: 26 }}
              spellCheck={false}
              style={{ fontFamily: 'ui-monospace, SFMono-Regular, Menlo, monospace', fontSize: 12.5 }}
            />
          </div>

          <Space size={12} style={{ width: '100%' }}>
            <span style={{ width: 64, textAlign: 'right', color: '#5b6169' }}>备注</span>
            <Input
              style={{ width: 460 }}
              placeholder="为什么这么配（例如：为什么要 2 副本 / 为什么内存给 1Gi）"
              value={formNote}
              onChange={(e) => setFormNote(e.target.value)}
              maxLength={255}
            />
          </Space>
        </div>
      </Modal>

      {/* ---------------- 查看 ---------------- */}
      <Modal
        open={!!viewing}
        title={
          viewing ? (
            <Space size={8}>
              <Tag color={KIND_COLOR[viewing.kind] ?? 'default'} style={{ marginInlineEnd: 0 }}>
                {viewing.kind}
              </Tag>
              <span className="yn-mono">{viewing.name}</span>
              <Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>
                {viewing.envKey}
              </Text>
            </Space>
          ) : (
            '查看'
          )
        }
        width={860}
        onCancel={() => setViewing(null)}
        footer={[
          <Button key="copy" icon={<CopyOutlined />} onClick={() => void copy(viewing?.content ?? '')}>
            复制
          </Button>,
          canWrite ? (
            <Button
              key="edit"
              type="primary"
              onClick={() => {
                const m = viewing;
                setViewing(null);
                if (m) openEdit(m);
              }}
            >
              编辑
            </Button>
          ) : null,
          <Button key="close" onClick={() => setViewing(null)}>
            关闭
          </Button>,
        ]}
      >
        {viewing?.note ? (
          <div style={{ marginBottom: 10, color: '#5b6169', fontSize: 12 }}>备注：{viewing.note}</div>
        ) : null}
        <pre
          className="yn-mono"
          style={{
            margin: 0,
            padding: 12,
            background: 'var(--yn-bg)',
            border: '1px solid var(--yn-border)',
            borderRadius: 8,
            fontSize: 12.5,
            lineHeight: 1.7,
            maxHeight: 520,
            overflow: 'auto',
            whiteSpace: 'pre',
          }}
        >
          {viewing?.content ?? ''}
        </pre>
      </Modal>
    </div>
  );
};

export default ProjectManifests;
