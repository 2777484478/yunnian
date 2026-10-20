import React, { useState } from 'react';
import { App as AntApp, Button, Form, Input, InputNumber, Modal, Space, Switch, Tag } from 'antd';
import { DeleteOutlined, EditOutlined, PlusOutlined } from '@ant-design/icons';
import { PageHeader } from '@/components/ui';
import { teamApi } from '@/api/team';
import type { Team } from '@/api/team';
import { hasPerm } from '@/auth';
import { useApi } from '@/hooks/useApi';


/**
 * 团队主数据管理。
 *
 * 补的洞：`project.team` 原来是自由字符串，候选值写死在前端 6 个 ——
 * 后端没约束、手填也能存，而用户的数据权限正是按它反查的。
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
          ) : (
            <>该团队下没有项目，可以删除。此操作不可撤销。</>
          )}
        </span>
      ),
      okText: '确认删除',
      okButtonProps: { danger: true, disabled: t.projectCount > 0 },
      onOk: async () => {
        await teamApi.remove(t.id);
        message.success('已删除');
        listApi.refresh();
      },
    });
  };

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
        description="项目归属的团队。用户的数据权限按团队反查，所以这里是主数据，不要在项目里手填"
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
                  </span>
                  <span style={{ color: '#b0b6c2' }}>排序 {t.sort}</span>
                </div>
                <div style={{ height: 5, borderRadius: 3, background: '#f1f4f9', overflow: 'hidden' }}>
                  <div style={{ width: `${pct}%`, height: '100%', background: color, borderRadius: 3 }} />
                </div>
              </div>

              <div style={{ display: 'flex', justifyContent: 'flex-end', gap: 4, marginTop: -2 }}>
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
    </div>
  );
};

export default SystemTeams;
