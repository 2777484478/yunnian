import React, { useEffect, useMemo, useRef, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Button,
  Form,
  Input,
  Modal,
  Popconfirm,
  Segmented,
  Select,
  Space,
  Switch,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import {
  ClockCircleOutlined,
  CloudServerOutlined,
  DeleteOutlined,
  EditOutlined,
  PlayCircleOutlined,
  PlusOutlined,
  ReloadOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons';
import { Link } from 'react-router-dom';
import { PageHeader, SectionCard } from '@/components/ui';
import { environmentApi } from '@/api/cd';
import { imageApi, projectApi } from '@/api/ci';
import { scheduleApi } from '@/api/schedule';
import type { ScheduleInput, ScheduleTask, ScheduleTaskType } from '@/api/schedule';
import { useApi } from '@/hooks/useApi';
import { hasPerm } from '@/auth';
import { fmt } from '@/utils/format';

const { Text } = Typography;

/**
 * cron → 人话。
 *
 * 为什么要有它：`0 2 * * *` 对写惯 cron 的人很清楚，但对"我只想每天两点跑一次"
 * 的人来说是一串符号。列表里 8 个任务全是表达式，等于每次都要在脑子里翻译一遍。
 *
 * 只覆盖最常见的那几种；认不出来就原样显示表达式 —— **绝不猜**，
 * 猜错会让人以为任务的时间和实际不符。
 */
function describeCron(expr: string): string {
  const p = expr.trim().split(/\s+/);
  if (p.length !== 5) return '';
  const [mi, ho, dom, mon, dow] = p;
  const isNum = (s: string) => /^\d+$/.test(s);

  // 每 N 分钟
  if (p[0].startsWith('*/') && p[1] === '*' && dom === '*' && mon === '*' && dow === '*') {
    return `每 ${p[0].slice(2)} 分钟`;
  }
  // 每小时第 M 分
  if (isNum(mi) && ho === '*' && dom === '*' && mon === '*' && dow === '*') {
    return `每小时第 ${mi} 分`;
  }
  // 每 N 小时
  if (isNum(mi) && ho.startsWith('*/') && dom === '*' && mon === '*' && dow === '*') {
    return `每 ${ho.slice(2)} 小时（第 ${mi} 分）`;
  }
  if (!isNum(mi) || !isNum(ho)) return '';
  const hhmm = `${ho.padStart(2, '0')}:${mi.padStart(2, '0')}`;

  // 每天
  if (dom === '*' && mon === '*' && dow === '*') return `每天 ${hhmm}`;
  // 工作日
  if (dom === '*' && mon === '*' && dow === '1-5') return `工作日 ${hhmm}`;
  // 每周几
  if (dom === '*' && mon === '*' && isNum(dow)) {
    const names = ['周日', '周一', '周二', '周三', '周四', '周五', '周六'];
    return `每${names[Number(dow)] ?? `周${dow}`} ${hhmm}`;
  }
  // 每周几（多选）
  if (dom === '*' && mon === '*' && /^[\d,]+$/.test(dow)) {
    const names = ['日', '一', '二', '三', '四', '五', '六'];
    const ds = dow.split(',').map((d) => names[Number(d)] ?? d);
    return `每周${ds.join('、')} ${hhmm}`;
  }
  // 每月某天
  if (isNum(dom) && mon === '*' && dow === '*') return `每月 ${dom} 号 ${hhmm}`;
  return '';
}

/** 常用预设，用户不用会写 cron。 */
const CRON_PRESETS: { label: string; cron: string }[] = [
  { label: '每天 02:00', cron: '0 2 * * *' },
  { label: '每天 06:00', cron: '0 6 * * *' },
  { label: '每工作日 09:00', cron: '0 9 * * 1-5' },
  { label: '每小时', cron: '0 * * * *' },
  { label: '每 30 分钟', cron: '*/30 * * * *' },
];

/** 上次执行结果 → 标签。 */
function LastStatusTag({ task }: { task: ScheduleTask }) {
  const st = task.lastStatus;
  if (!st) return <Text type="secondary">还没执行过</Text>;
  const map: Record<string, { color: string; label: string }> = {
    success: { color: 'green', label: '成功' },
    failed: { color: 'red', label: '失败' },
    skipped: { color: 'orange', label: '跳过' },
    missed: { color: 'gold', label: '错过' },
    running: { color: 'blue', label: '执行中' },
  };
  const m = map[st] ?? { color: 'default', label: st };
  return (
    <Space size={6} direction="vertical" style={{ lineHeight: 1.4 }}>
      <Space size={6}>
        <Tag color={m.color} style={{ marginInlineEnd: 0 }}>
          {m.label}
        </Tag>
        {task.lastRunAt ? (
          <Text type="secondary" style={{ fontSize: 12 }}>
            {fmt(task.lastRunAt)}
          </Text>
        ) : null}
      </Space>
      {task.lastError ? (
        <Tooltip title={task.lastError}>
          <Text type="secondary" style={{ fontSize: 12 }}>
            {task.lastError.length > 34 ? `${task.lastError.slice(0, 34)}…` : task.lastError}
          </Text>
        </Tooltip>
      ) : null}
      {task.missedCount > 0 ? (
        <Tooltip title="服务停机期间到点但没执行。按配置不会补跑">
          <Text type="warning" style={{ fontSize: 12 }}>
            累计错过 {task.missedCount} 次
          </Text>
        </Tooltip>
      ) : null}
    </Space>
  );
}

const ScheduleList: React.FC = () => {
  const { message } = AntApp.useApp();
  const canWrite = hasPerm('build:trigger') || hasPerm('deploy:read');

  const [taskType, setTaskType] = useState<string>('');
  const listApi = useApi((signal) => scheduleApi.list(), []);
  const rows = listApi.data ?? [];

  const projApi = useApi(() => projectApi.list({ pageSize: 200 }), []);
  const envApi = useApi(() => environmentApi.list({ pageSize: 100 }), []);
  const projects = projApi.data?.list ?? [];
  const envs = envApi.data?.list ?? [];

  const shown = useMemo(
    () => (taskType ? rows.filter((r) => r.taskType === taskType) : rows),
    [rows, taskType],
  );

  // ---- 弹窗 ----
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState<ScheduleTask | null>(null);
  const [saving, setSaving] = useState(false);
  const [running, setRunning] = useState<string | null>(null);

  const [name, setName] = useState('');
  const [kind, setKind] = useState<ScheduleTaskType>('build');
  const [projectKey, setProjectKey] = useState('');
  const [envKey, setEnvKey] = useState('');
  const [cron, setCron] = useState('0 2 * * *');
  const [branch, setBranch] = useState('');
  const [imageSource, setImageSource] = useState<'fixed' | 'latest'>('latest');
  const [imageChoice, setImageChoice] = useState('');
  const [remark, setRemark] = useState('');
  const [enabled, setEnabled] = useState(true);

  // cron 预览（防抖）
  const [preview, setPreview] = useState<{ valid: boolean; error: string; next: string[] } | null>(null);
  const previewTimer = useRef<number | null>(null);

  // 选定项目后拉它的镜像记录（作为"指定版本"的候选）
  const [imageOptions, setImageOptions] = useState<{ value: string; label: string }[]>([]);
  const project = projects.find((p) => p.key === projectKey);
  const isJenkins = (project?.buildMethod ?? '') === 'jenkins';

  useEffect(() => {
    if (!open || kind !== 'deploy' || !projectKey) {
      setImageOptions([]);
      return;
    }
    let cancelled = false;
    void (async () => {
      try {
        const res = await imageApi.list({ projectKey, pageSize: 50 });
        if (cancelled) return;
        setImageOptions(
          (res.list ?? []).map((im) => ({
            value: `p:${im.id}`,
            label: `${im.tag}（平台构建）`,
          })),
        );
      } catch {
        if (!cancelled) setImageOptions([]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [open, kind, projectKey]);

  useEffect(() => {
    if (!open) return;
    if (previewTimer.current) window.clearTimeout(previewTimer.current);
    previewTimer.current = window.setTimeout(() => {
      void scheduleApi
        .preview(cron)
        .then((r) => setPreview(r))
        .catch(() => setPreview(null));
    }, 300);
    return () => {
      if (previewTimer.current) window.clearTimeout(previewTimer.current);
    };
  }, [cron, open]);

  const openNew = () => {
    setEditing(null);
    setName('');
    setKind('build');
    setProjectKey('');
    setEnvKey('');
    setCron('0 2 * * *');
    setBranch('');
    setImageSource('latest');
    setImageChoice('');
    setRemark('');
    setEnabled(true);
    setPreview(null);
    setOpen(true);
  };

  const openEdit = (t: ScheduleTask) => {
    setEditing(t);
    setName(t.name);
    setKind(t.taskType);
    setProjectKey(t.projectKey);
    setEnvKey(t.envKey);
    setCron(t.cron);
    setBranch(t.branch ?? '');
    setImageSource((t.imageSource as 'fixed' | 'latest') || 'latest');
    // fixed 时回填成 "p:<id>" 形态，与下拉选项对齐
    setImageChoice(t.imageRecordId ? `p:${t.imageRecordId}` : t.imageTag ? `r:${t.imageTag}` : '');
    setRemark(t.remark ?? '');
    setEnabled(t.enabled);
    setPreview(null);
    setOpen(true);
  };

  const save = async () => {
    if (!name.trim()) {
      message.warning('给任务起个名字：它会显示在构建/部署记录的"操作人"上');
      return;
    }
    if (!projectKey) {
      message.warning('请选择项目');
      return;
    }
    if (kind === 'deploy' && !envKey) {
      message.warning('定时部署必须选择环境');
      return;
    }
    if (!cron.trim()) {
      message.warning('请填 cron 表达式');
      return;
    }

    const body: ScheduleInput = {
      name: name.trim(),
      taskType: kind,
      projectKey,
      cron: cron.trim(),
      enabled,
      remark: remark.trim(),
    };
    if (kind === 'build') {
      body.branch = branch.trim();
    } else {
      body.envKey = envKey;
      body.imageSource = imageSource;
      if (imageSource === 'fixed') {
        if (!imageChoice) {
          message.warning('请选择要发的版本，或改成「每次取最新」');
          return;
        }
        if (imageChoice.startsWith('p:')) {
          body.imageRecordId = imageChoice.slice(2);
        } else {
          body.imageTag = imageChoice.slice(2);
        }
      }
    }

    setSaving(true);
    try {
      if (editing) {
        await scheduleApi.update(editing.id, body);
        message.success('已保存');
      } else {
        await scheduleApi.create(body);
        message.success('已创建');
      }
      setOpen(false);
      listApi.refresh();
    } catch (e) {
      // 后端的拒绝理由都是能照着改的（cron 哪里非法、哪个环境没权限），原样展示
      message.error((e as Error)?.message ?? '保存失败');
    } finally {
      setSaving(false);
    }
  };

  const toggle = async (t: ScheduleTask) => {
    try {
      await scheduleApi.setEnabled(t.id, !t.enabled);
      message.success(t.enabled ? '已停用（不会再自动执行）' : '已启用');
      listApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message ?? '操作失败');
    }
  };

  const runNow = async (t: ScheduleTask) => {
    setRunning(t.id);
    try {
      await scheduleApi.runNow(t.id);
      message.success('已触发一次（不会改动下次计划时间）');
      listApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message ?? '触发失败');
    } finally {
      setRunning(null);
    }
  };

  const remove = async (t: ScheduleTask) => {
    try {
      await scheduleApi.remove(t.id);
      message.success('已删除');
      listApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message ?? '删除失败');
    }
  };

  const columns = [
    {
      title: '任务',
      dataIndex: 'name',
      width: 220,
      render: (_: unknown, r: ScheduleTask) => (
        <Space size={6} direction="vertical" style={{ lineHeight: 1.4 }}>
          <Space size={6}>
            <b>{r.name}</b>
            {!r.enabled ? <Tag>已停用</Tag> : null}
          </Space>
          {r.remark ? (
            <Text type="secondary" style={{ fontSize: 12 }}>
              {r.remark}
            </Text>
          ) : null}
        </Space>
      ),
    },
    {
      title: '类型',
      dataIndex: 'taskType',
      width: 92,
      render: (v: string) =>
        v === 'build' ? (
          <Tag color="blue" icon={<ThunderboltOutlined />}>
            构建
          </Tag>
        ) : (
          <Tag color="geekblue" icon={<CloudServerOutlined />}>
            部署
          </Tag>
        ),
    },
    {
      title: '目标',
      width: 230,
      render: (_: unknown, r: ScheduleTask) => (
        <Space size={4} direction="vertical" style={{ lineHeight: 1.4 }}>
          <span className="yn-mono">{r.projectKey}</span>
          {r.taskType === 'deploy' ? (
            <Text type="secondary" style={{ fontSize: 12 }}>
              → {r.envKey} ·{' '}
              {r.imageSource === 'latest' ? '每次取最新构建' : r.imageTag || `#${r.imageRecordId}`}
            </Text>
          ) : (
            <Text type="secondary" style={{ fontSize: 12 }}>
              {r.branch ? `分支 ${r.branch}` : '用项目默认分支'}
            </Text>
          )}
        </Space>
      ),
    },
    {
      title: '计划',
      dataIndex: 'cron',
      width: 190,
      render: (v: string) => {
        const human = describeCron(v);
        return (
          <Space size={4} direction="vertical" style={{ lineHeight: 1.4 }}>
            <span>{human || v}</span>
            {human ? (
              <Text type="secondary" className="yn-mono" style={{ fontSize: 12 }}>
                {v}
              </Text>
            ) : null}
          </Space>
        );
      },
    },
    {
      title: '下次执行',
      dataIndex: 'nextRunAt',
      width: 160,
      render: (v: string | null, r: ScheduleTask) =>
        !r.enabled ? (
          <Text type="secondary">已停用</Text>
        ) : v ? (
          // 同样去秒：表格里 19 个字符刚好把这一列撑满，秒对"下次什么时候跑"毫无信息量
          fmt(v).slice(0, 16)
        ) : (
          <Text type="secondary">—</Text>
        ),
    },
    {
      title: '上次结果',
      width: 250,
      render: (_: unknown, r: ScheduleTask) => (
        <Space size={8} align="start">
          <LastStatusTag task={r} />
          {r.lastBuildId ? (
            <Link to={`/ci/builds?keyword=${r.projectKey}`} style={{ fontSize: 12 }}>
              看构建
            </Link>
          ) : null}
          {r.lastDeployId ? (
            <Link to="/cd/deploys" style={{ fontSize: 12 }}>
              看部署
            </Link>
          ) : null}
        </Space>
      ),
    },
    {
      title: '操作',
      width: 160,
      fixed: 'right' as const,
      render: (_: unknown, r: ScheduleTask) =>
        canWrite ? (
          <Space size={2}>
            <Tooltip title="立即执行一次（不改动下次计划时间）">
              <Button
                type="text"
                size="small"
                icon={<PlayCircleOutlined />}
                loading={running === r.id}
                onClick={() => void runNow(r)}
              />
            </Tooltip>
            <Tooltip title={r.enabled ? '停用（不再自动执行）' : '启用'}>
              <Switch size="small" checked={r.enabled} onChange={() => void toggle(r)} />
            </Tooltip>
            <Tooltip title="编辑">
              <Button type="text" size="small" icon={<EditOutlined />} onClick={() => openEdit(r)} />
            </Tooltip>
            <Popconfirm
              title="删除这个定时任务？"
              description={<span>只删任务本身，已经发出去的构建/部署记录不受影响。</span>}
              okText="删除"
              okButtonProps={{ danger: true }}
              onConfirm={() => void remove(r)}
            >
              <Button type="text" size="small" danger icon={<DeleteOutlined />} />
            </Popconfirm>
          </Space>
        ) : (
          <Text type="secondary">—</Text>
        ),
    },
  ];

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: 'CI 镜像构建' }, { title: '定时任务' }]}
        title="定时任务"
        description="到点自动触发构建或部署。它只负责「建单」——排队、并发、失败通知都走现有链路，与手动触发完全一致。"
      />

      <SectionCard bodyStyle={{ padding: 0 }}>
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 12,
            padding: '12px 16px',
            borderBottom: '1px solid var(--yn-border)',
            flexWrap: 'wrap',
          }}
        >
          <Segmented
            value={taskType}
            onChange={(v) => setTaskType(String(v))}
            options={[
              { label: '全部', value: '' },
              { label: '定时构建', value: 'build' },
              { label: '定时部署', value: 'deploy' },
            ]}
          />
          <Text type="secondary" style={{ fontSize: 12 }}>
            共 {shown.length} 个任务
          </Text>
          <div style={{ flex: '1 1 auto' }} />
          <Tooltip title="刷新">
            <Button icon={<ReloadOutlined />} onClick={listApi.refresh} />
          </Tooltip>
          {canWrite ? (
            <Button type="primary" icon={<PlusOutlined />} onClick={openNew}>
              新增任务
            </Button>
          ) : null}
        </div>

        {listApi.error ? (
          <div style={{ padding: 16 }}>
            <Alert type="error" showIcon message={listApi.error.message} />
          </div>
        ) : null}

        <Table<ScheduleTask>
          rowKey="id"
          size="middle"
          loading={listApi.loading}
          dataSource={shown}
          columns={columns}
          scroll={{ x: 1300 }}
          pagination={shown.length > 20 ? { pageSize: 20, showSizeChanger: false } : false}
          locale={{ emptyText: '还没有定时任务。点右上角「新增任务」创建第一个。' }}
        />
      </SectionCard>

      {/* ---------------- 新建 / 编辑 ---------------- */}
      <Modal
        open={open}
        title={editing ? '编辑定时任务' : '新增定时任务'}
        width={720}
        onCancel={() => setOpen(false)}
        onOk={() => void save()}
        okText="保存"
        confirmLoading={saving}
        destroyOnClose
        styles={{ body: { maxHeight: 'calc(100vh - 260px)', overflowY: 'auto' } }}
      >
        <Form layout="vertical" size="middle">
          <Form.Item label="任务名" required style={{ marginBottom: 14 }}>
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="例如：夜间打包 / 每天发最新到预生产"
              maxLength={64}
            />
            <Text type="secondary" style={{ fontSize: 12 }}>
              这个名字会显示在构建/部署记录的「操作人」上，方便事后区分「是不是定时发的」
            </Text>
          </Form.Item>

          <Form.Item label="做什么" style={{ marginBottom: 14 }}>
            <Segmented
              value={kind}
              onChange={(v) => setKind(v as ScheduleTaskType)}
              options={[
                { label: '定时构建', value: 'build' },
                { label: '定时部署', value: 'deploy' },
              ]}
            />
          </Form.Item>

          <Form.Item label="项目" required style={{ marginBottom: 14 }}>
            <Select
              showSearch
              optionFilterProp="label"
              value={projectKey || undefined}
              onChange={(v) => {
                setProjectKey(v);
                setImageChoice('');
              }}
              placeholder="选择项目"
              options={projects.map((p) => ({
                value: p.key,
                label: `${p.name}（${p.key}）`,
              }))}
              style={{ width: '100%' }}
            />
            {kind === 'build' && isJenkins ? (
              <Text type="secondary" style={{ fontSize: 12 }}>
                该项目走 Jenkins：平台只负责到点调 Jenkins，产物由 Jenkins 自己打包推送
              </Text>
            ) : null}
          </Form.Item>

          {kind === 'deploy' ? (
            <>
              <Form.Item label="环境" required style={{ marginBottom: 14 }}>
                <Select
                  value={envKey || undefined}
                  onChange={setEnvKey}
                  placeholder="发到哪个环境"
                  options={envs.map((e) => ({ value: e.key, label: `${e.name}（${e.key}）` }))}
                  style={{ width: '100%' }}
                />
              </Form.Item>
              <Form.Item label="发哪个版本" required style={{ marginBottom: 14 }}>
                <Segmented
                  value={imageSource}
                  onChange={(v) => setImageSource(v as 'fixed' | 'latest')}
                  options={[
                    { label: '每次取最新构建', value: 'latest' },
                    { label: '指定固定版本', value: 'fixed' },
                  ]}
                />
                {imageSource === 'fixed' ? (
                  <Select
                    showSearch
                    optionFilterProp="label"
                    style={{ width: '100%', marginTop: 8 }}
                    value={imageChoice || undefined}
                    onChange={setImageChoice}
                    placeholder={projectKey ? '选一个平台构建过的版本' : '先选项目'}
                    options={imageOptions}
                    notFoundContent={
                      projectKey ? '这个项目在平台上还没有构建记录' : '先选项目'
                    }
                  />
                ) : (
                  <Text type="secondary" style={{ fontSize: 12, display: 'block', marginTop: 6 }}>
                    每次执行时取该项目<b>此刻最新</b>的成功构建。选这个就不用在每次构建后改任务了。
                  </Text>
                )}
              </Form.Item>
            </>
          ) : (
            <Form.Item label="分支" style={{ marginBottom: 14 }}>
              <Input
                value={branch}
                onChange={(e) => setBranch(e.target.value)}
                placeholder={isJenkins ? 'Jenkins 项目用 Job 自己的参数，这里不用填' : '留空 = 用项目默认分支'}
                disabled={isJenkins}
              />
            </Form.Item>
          )}

          <Form.Item label="什么时候跑" required style={{ marginBottom: 8 }}>
            <Space size={8} wrap style={{ marginBottom: 8 }}>
              {CRON_PRESETS.map((p) => (
                <Button key={p.cron} size="small" onClick={() => setCron(p.cron)}>
                  {p.label}
                </Button>
              ))}
            </Space>
            <Input
              value={cron}
              onChange={(e) => setCron(e.target.value)}
              className="yn-mono"
              placeholder="分 时 日 月 周，例如 0 2 * * *"
            />
          </Form.Item>
          <div style={{ marginBottom: 14, minHeight: 42 }}>
            {preview?.valid ? (
              <Space size={6} wrap>
                <Text type="secondary" style={{ fontSize: 12 }}>
                  接下来：
                </Text>
                {preview.next.slice(0, 4).map((t) => (
                  // 去掉秒：`2026-10-30 02:00:00` 里那个 :00 只是噪音，
                  // 而 4 个时间并排时它占的宽度恰好把一行挤满。
                  <Tag key={t} style={{ fontSize: 12 }}>
                    {fmt(t).slice(0, 16)}
                  </Tag>
                ))}
              </Space>
            ) : preview && !preview.valid ? (
              <Text type="danger" style={{ fontSize: 12 }}>
                {preview.error}
              </Text>
            ) : (
              <Text type="secondary" style={{ fontSize: 12 }}>
                <ClockCircleOutlined /> 正在校验表达式…
              </Text>
            )}
          </div>

          <Form.Item label="备注" style={{ marginBottom: 14 }}>
            <Input
              value={remark}
              onChange={(e) => setRemark(e.target.value)}
              placeholder="为什么配这个定时（例如：每天凌晨出一次包给测试用）"
              maxLength={255}
            />
          </Form.Item>

          <Form.Item label="启用" style={{ marginBottom: 0 }}>
            <Switch checked={enabled} onChange={setEnabled} />
            <Text type="secondary" style={{ fontSize: 12, marginLeft: 10 }}>
              停用后不会自动执行，但任务保留（可以随时再打开）
            </Text>
          </Form.Item>
        </Form>
      </Modal>
    </div>
  );
};

export default ScheduleList;
