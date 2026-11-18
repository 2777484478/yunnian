import React, { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Button,
  Drawer,
  Empty,
  Form,
  Input,
  InputNumber,
  Modal,
  Select,
  Space,
  Switch,
  Table,
  Tag,
  Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { DeleteOutlined, EditOutlined, PlusOutlined, TeamOutlined, AppstoreOutlined } from '@ant-design/icons';
import { PageHeader } from '@/components/ui';
import { teamApi } from '@/api/team';
import type { Team, TeamMember, TeamProjectOption } from '@/api/team';
import { userApi } from '@/api/auth';
import type { UserOption } from '@/api/auth';
import { hasPerm } from '@/auth';
import { useApi } from '@/hooks/useApi';

const { Text } = Typography;


/**
 * 团队管理。
 *
 * 两件事在同一页里，但**性质完全不同**，界面上要分开说：
 *
 *  1. 团队本身 = 项目的业务分组（一个项目只属于一个团队）；
 *  2. 团队成员 = **数据可见范围**：属于哪个团队，就能在「项目管理」里
 *     看到那个团队的项目。它**不影响**角色权限（能不能发布、能不能审批）。
 *
 * 所以卡片上必须同时显示"几个项目 / 几个人"：0 个成员 + 有项目 =
 * 那些项目只有超级管理员看得到，这是个需要立刻被看见的状态。
 */
const SystemTeams: React.FC = () => {
  const { message, modal } = AntApp.useApp();
  const canWrite = hasPerm('team:manage');

  const listApi = useApi(() => teamApi.list(), []);
  const teams = listApi.data ?? [];

  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState<Team | null>(null);
  const [saving, setSaving] = useState(false);
  const [form] = Form.useForm();

  const openNew = () => {
    setEditing(null);
    form.setFieldsValue({ name: '', description: '', sort: ((teams.length ? teams[teams.length - 1].sort : 0) ?? 0) + 10, enabled: true });
    setOpen(true);
  };
  const openEdit = (t: Team) => {
    setEditing(t);
    form.setFieldsValue({ name: t.name, description: t.description, sort: t.sort, enabled: t.enabled });
    setOpen(true);
  };

  const save = async () => {
    const v = await form.validateFields();
    setSaving(true);
    try {
      if (editing) {
        await teamApi.update(editing.id, v);
        message.success('已保存（若改了名称，所有项目的团队已同步）');
      } else {
        await teamApi.create(v);
        message.success('已新建团队');
      }
      setOpen(false);
      listApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message ?? '保存失败');
    } finally {
      setSaving(false);
    }
  };

  const remove = (t: Team) => {
    modal.confirm({
      title: `删除团队「${t.name}」？`,
      content: (
        <span style={{ fontSize: 13 }}>
          {t.projectCount > 0 ? (
            <>
              该团队下还有 <b>{t.projectCount}</b> 个项目，不能删除。
              请先把这些项目改到别的团队。
            </>
          ) : t.memberCount > 0 ? (
            <>
              该团队下还有 <b>{t.memberCount}</b> 名成员，不能删除。
              请先把他们移出该团队（否则他们会失去这个团队的可见范围）。
            </>
          ) : (
            <>该团队下没有项目和成员，可以删除。此操作不可撤销。</>
          )}
        </span>
      ),
      okText: '确认删除',
      okButtonProps: { danger: true, disabled: t.projectCount > 0 || t.memberCount > 0 },
      onOk: async () => {
        await teamApi.remove(t.id);
        message.success('已删除');
        listApi.refresh();
      },
    });
  };

  // ---------------------------------------------------------------- 成员（数据可见范围）
  const [memberTeam, setMemberTeam] = useState<Team | null>(null);
  const [memberIds, setMemberIds] = useState<string[]>([]);
  const [memberLoading, setMemberLoading] = useState(false);
  const [memberSaving, setMemberSaving] = useState(false);
  // 「选人」名录走 /users/options：它不要求 user:manage（只有 team:manage 的
  // 角色也能用），否则团队管理员连候选人都拉不到。
  const userOptionsApi = useApi(() => userApi.options(), []);
  const userOptions = useMemo(
    () => (userOptionsApi.data ?? []).map((u: UserOption) => ({ label: `${u.name}（${u.username}）`, value: u.id })),
    [userOptionsApi.data],
  );

  const openMembers = async (t: Team) => {
    setMemberTeam(t);
    setMemberIds([]);
    setMemberLoading(true);
    try {
      const list = await teamApi.members(t.id);
      setMemberIds(list.map((m) => m.id));
    } catch (e) {
      message.error((e as Error)?.message ?? '读取成员失败');
    } finally {
      setMemberLoading(false);
    }
  };

  const saveMembers = async () => {
    if (!memberTeam) return;
    setMemberSaving(true);
    try {
      await teamApi.setMembers(memberTeam.id, memberIds);
      message.success(`已保存：「${memberTeam.name}」共 ${memberIds.length} 名成员`);
      setMemberTeam(null);
      listApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message ?? '保存失败');
    } finally {
      setMemberSaving(false);
    }
  };

  // ---------------------------------------------------------------- 项目归属（迁入）
  const [projTeam, setProjTeam] = useState<Team | null>(null);
  const [projRows, setProjRows] = useState<TeamProjectOption[]>([]);
  const [projChecked, setProjChecked] = useState<React.Key[]>([]);
  const [projLoading, setProjLoading] = useState(false);
  const [projSaving, setProjSaving] = useState(false);

  const openProjects = async (t: Team) => {
    setProjTeam(t);
    setProjRows([]);
    setProjChecked([]);
    setProjLoading(true);
    try {
      const list = await teamApi.projectOptions(t.id);
      setProjRows(list);
      setProjChecked(list.filter((p) => p.inTeam).map((p) => p.key));
    } catch (e) {
      message.error((e as Error)?.message ?? '读取项目失败');
    } finally {
      setProjLoading(false);
    }
  };

  /**
   * 保存项目归属。
   *
   * 语义是**迁入**而不是"勾选绑定"：`project.team` 是单值，一个项目只能属于
   * 一个团队。所以这个操作只做一件事——把勾中的项目改成这个团队。
   * 取消勾选**不会**把项目移出去（它没地方可去），界面上要写清楚。
   */
  const saveProjects = async () => {
    if (!projTeam) return;
    setProjSaving(true);
    try {
      const keys = projChecked.map(String);
      const res = await teamApi.moveProjects(projTeam.id, keys);
      message.success(`已把 ${res.moved ?? keys.length} 个项目归到「${projTeam.name}」`);
      setProjTeam(null);
      listApi.refresh();
    } catch (e) {
      message.error((e as Error)?.message ?? '保存失败');
    } finally {
      setProjSaving(false);
    }
  };

  const projColumns: ColumnsType<TeamProjectOption> = [
    { title: '项目', dataIndex: 'key', width: 220, render: (v: string, r) => (
      <span>
        <span className="yn-mono" style={{ fontWeight: 500 }}>{v}</span>
        <div style={{ fontSize: 12, color: '#9aa0aa' }}>{r.name}</div>
      </span>
    ) },
    { title: '当前团队', dataIndex: 'team', width: 160, render: (v: string, r) =>
      r.inTeam ? <Tag color="blue" style={{ marginInlineEnd: 0 }}>本团队</Tag>
               : <span style={{ color: '#5a6478' }}>{v || '—'}</span> },
    { title: '类型', dataIndex: 'type', width: 80, render: (v: string) => (v === 'frontend' ? '前端' : '后端') },
  ];

  const totalProjects = teams.reduce((n, t) => n + t.projectCount, 0);
  const maxProjects = Math.max(1, ...teams.map((t) => t.projectCount));

  /** 首字头像的配色：按团队名稳定取色（同一个团队永远同一个颜色） */
  const AVATAR_COLORS = ['#1677ff', '#13a8a8', '#722ed1', '#d46b08', '#1590ff', '#c41d7f'];
  const colorOf = (name: string) => AVATAR_COLORS[[...name].reduce((a, ch) => a + ch.charCodeAt(0), 0) % AVATAR_COLORS.length];

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: '系统管理' }, { title: '团队' }]}
        title="团队"
        subTitle="TEAMS"
        description="团队决定两件事：项目归谁（分类）、以及成员在「项目管理」里能看到哪些项目（可见范围）。它不影响角色权限"
        extra={
          <Button type="primary" icon={<PlusOutlined />} disabled={!canWrite} onClick={openNew}>
            新建团队
          </Button>
        }
      />

      {/* 概览：团队数 / 关联项目 / 项目最多的团队 */}
      <div
        style={{
          display: 'grid',
          gridTemplateColumns: 'repeat(auto-fit, minmax(200px, 1fr))',
          gap: 12,
          marginBottom: 16,
        }}
      >
        {[
          { label: '团队总数', value: teams.length, unit: '个', accent: '#1677ff' },
          { label: '关联项目', value: totalProjects, unit: '个', accent: '#13a8a8' },
          {
            label: '项目最多的团队',
            value: teams.reduce<Team | null>((a, b) => (!a || b.projectCount > a.projectCount ? b : a), null)?.name ?? '—',
            unit: '',
            accent: '#722ed1',
          },
        ].map((c) => (
          <div
            key={c.label}
            style={{
              padding: '14px 16px',
              background: '#fff',
              border: '1px solid #eef1f6',
              borderRadius: 10,
              borderLeft: `3px solid ${c.accent}`,
            }}
          >
            <div style={{ fontSize: 12, color: '#8c8c95', marginBottom: 6 }}>{c.label}</div>
            <div style={{ display: 'flex', alignItems: 'baseline', gap: 4 }}>
              <span style={{ fontSize: 22, fontWeight: 600, color: '#1f2a44', lineHeight: 1.1 }}>{c.value}</span>
              {c.unit ? <span style={{ fontSize: 12, color: '#8c8c95' }}>{c.unit}</span> : null}
            </div>
          </div>
        ))}
      </div>

      <div
        style={{
          display: 'grid',
          gridTemplateColumns: 'repeat(auto-fill, minmax(280px, 1fr))',
          gap: 14,
        }}
      >
        {teams.map((t) => {
          const pct = Math.round((t.projectCount / maxProjects) * 100);
          const color = colorOf(t.name);
          return (
            <div
              key={t.id}
              style={{
                background: '#fff',
                border: '1px solid #eef1f6',
                borderRadius: 10,
                padding: '14px 16px',
                display: 'flex',
                flexDirection: 'column',
                gap: 10,
              }}
            >
              <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
                <div
                  style={{
                    width: 34,
                    height: 34,
                    borderRadius: 9,
                    background: `${color}14`,
                    color,
                    display: 'flex',
                    alignItems: 'center',
                    justifyContent: 'center',
                    fontSize: 15,
                    fontWeight: 600,
                    flex: 'none',
                  }}
                >
                  {t.name.slice(0, 1)}
                </div>
                <div style={{ minWidth: 0, flex: 1 }}>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                    <span style={{ fontSize: 14, fontWeight: 500, color: '#1f2a44' }}>{t.name}</span>
                    {!t.enabled ? <Tag style={{ marginInlineEnd: 0 }}>已停用</Tag> : null}
                  </div>
                  <div style={{ fontSize: 12, color: '#8c8c95', marginTop: 2 }}>
                    {t.description || '未填写说明'}
                  </div>
                </div>
              </div>

              {/* 项目占比条：一眼看出哪个团队盘子最大 */}
              <div>
                <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: 12, color: '#5a6478', marginBottom: 4 }}>
                  <span>
                    <b style={{ color: '#1f2a44' }}>{t.projectCount}</b> 个项目
                    <span style={{ margin: '0 6px', color: '#dfe3ea' }}>·</span>
                    <b style={{ color: t.memberCount ? '#1f2a44' : '#ff4d4f' }}>{t.memberCount}</b> 名成员
                  </span>
                  <span style={{ color: '#b0b6c2' }}>排序 {t.sort}</span>
                </div>
                <div style={{ height: 5, borderRadius: 3, background: '#f1f4f9', overflow: 'hidden' }}>
                  <div style={{ width: `${pct}%`, height: '100%', background: color, borderRadius: 3 }} />
                </div>
              </div>

              {t.projectCount > 0 && t.memberCount === 0 ? (
                <div style={{ fontSize: 12, color: '#d4380d', background: '#fff7f0', border: '1px solid #ffe2c8', borderRadius: 6, padding: '6px 8px' }}>
                  没有成员：这 {t.projectCount} 个项目目前只有超级管理员看得到
                </div>
              ) : null}

              <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 4, marginTop: -2, flexWrap: 'wrap' }}>
                <Button size="small" type="text" icon={<TeamOutlined />} onClick={() => void openMembers(t)}>
                  成员
                </Button>
                <Button size="small" type="text" icon={<AppstoreOutlined />} onClick={() => void openProjects(t)}>
                  项目
                </Button>
                <Button size="small" type="text" icon={<EditOutlined />} disabled={!canWrite} onClick={() => openEdit(t)}>
                  编辑
                </Button>
                <Button size="small" type="text" danger icon={<DeleteOutlined />} disabled={!canWrite} onClick={() => remove(t)}>
                  删除
                </Button>
              </div>
            </div>
          );
        })}
      </div>

      <Modal
        open={open}
        title={editing ? `编辑团队 · ${editing.name}` : '新建团队'}
        onCancel={() => setOpen(false)}
        onOk={save}
        confirmLoading={saving}
        okText="保存"
        destroyOnHidden
      >
        <Form form={form} layout="vertical" requiredMark={false} style={{ marginTop: 12 }}>
          <Form.Item
            label="团队名称"
            name="name"
            rules={[{ required: true, message: '请填写团队名称' }]}
            extra={editing ? '改名会同步更新所有项目的团队字段（同一事务）' : undefined}
          >
            <Input maxLength={64} placeholder="如 基础架构组" />
          </Form.Item>
          <Form.Item label="说明" name="description">
            <Input maxLength={255} placeholder="选填" />
          </Form.Item>
          <Form.Item label="排序" name="sort" extra="数字小的排在前面">
            <InputNumber style={{ width: 160 }} />
          </Form.Item>
          {editing ? (
            <Form.Item label="启用" name="enabled" valuePropName="checked">
              <Switch />
            </Form.Item>
          ) : null}
        </Form>
      </Modal>

      {/* ------------------------------------------------------------ 成员 */}
      <Drawer
        open={!!memberTeam}
        width={480}
        title={`成员 · ${memberTeam?.name ?? ''}`}
        onClose={() => setMemberTeam(null)}
        destroyOnHidden
        extra={
          <Space>
            <Button onClick={() => setMemberTeam(null)}>取消</Button>
            <Button type="primary" loading={memberSaving} disabled={!canWrite} onClick={() => void saveMembers()}>
              保存
            </Button>
          </Space>
        }
      >
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          message="成员决定「项目管理」里能看到哪些项目"
          description={
            <span>
              属于这个团队的人，在项目列表里能看到本团队的项目（多个团队取并集）。
              这里<b>不影响</b>角色权限——能不能发布、能不能审批，仍然由角色决定。
              超级管理员不受此限制。
            </span>
          }
        />
        <div style={{ fontSize: 13, marginBottom: 6 }}>选择成员</div>
        <Select
          mode="multiple"
          allowClear
          loading={memberLoading}
          style={{ width: '100%' }}
          placeholder="搜索姓名或账号"
          optionFilterProp="label"
          value={memberIds}
          onChange={setMemberIds}
          options={userOptions}
        />
        <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 8 }}>
          清空后保存 = 这个团队没有任何成员，它下面的项目只有超级管理员看得到。
        </div>
      </Drawer>

      {/* ------------------------------------------------------------ 项目归属 */}
      <Drawer
        open={!!projTeam}
        width={760}
        title={`项目归属 · ${projTeam?.name ?? ''}`}
        onClose={() => setProjTeam(null)}
        destroyOnHidden
        extra={
          <Space>
            <Button onClick={() => setProjTeam(null)}>取消</Button>
            <Button type="primary" loading={projSaving} disabled={!canWrite} onClick={() => void saveProjects()}>
              保存
            </Button>
          </Space>
        }
      >
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 14 }}
          message="勾选 = 把这些项目「迁入」本团队"
          description={
            <span>
              一个项目只能属于一个团队，所以这里不是"多选绑定"：勾上并保存后，
              这些项目会从原团队<b>移过来</b>。取消勾选不会把项目移出去（它必须有个归属），
              要改归属请到目标团队里勾它。
            </span>
          }
        />
        <Table<TeamProjectOption>
          rowKey="key"
          size="small"
          loading={projLoading}
          dataSource={projRows}
          columns={projColumns}
          pagination={{ pageSize: 20, showSizeChanger: false }}
          rowSelection={{
            selectedRowKeys: projChecked,
            onChange: setProjChecked,
            getCheckboxProps: (r) => ({ disabled: r.disabled }),
          }}
          locale={{ emptyText: <Empty description="没有项目" /> }}
        />
        <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 10 }}>
          已停用的项目置灰不可选（迁一个停用项目没有意义）。
        </div>
      </Drawer>
    </div>
  );
};

export default SystemTeams;
