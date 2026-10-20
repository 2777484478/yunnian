import React, { useState } from 'react';
import { App as AntApp, Button, Form, Input, InputNumber, Modal, Space, Switch, Table, Tag, Typography } from 'antd';
import type { ColumnsType } from 'antd/es/table';
import { DeleteOutlined, EditOutlined, PlusOutlined } from '@ant-design/icons';
import { PageHeader, SectionCard } from '@/components/ui';
import { teamApi } from '@/api/team';
import type { Team } from '@/api/team';
import { hasPerm } from '@/auth';
import { useApi } from '@/hooks/useApi';

const { Text } = Typography;

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

  const columns: ColumnsType<Team> = [
    {
      title: '团队',
      dataIndex: 'name',
      width: 200,
      render: (v: string, r) => (
        <Space size={6}>
          <span style={{ fontWeight: 500 }}>{v}</span>
          {!r.enabled ? <Tag>已停用</Tag> : null}
        </Space>
      ),
    },
    {
      title: '说明',
      dataIndex: 'description',
      render: (v?: string) => v || <Text type="secondary">—</Text>,
    },
    {
      title: '项目数',
      dataIndex: 'projectCount',
      width: 100,
      render: (v: number) => (v > 0 ? <Tag color="blue">{v}</Tag> : <Text type="secondary">0</Text>),
    },
    { title: '排序', dataIndex: 'sort', width: 80 },
    {
      title: '操作',
      key: 'act',
      width: 150,
      render: (_: unknown, r) => (
        <Space size={4}>
          <Button size="small" type="link" icon={<EditOutlined />} disabled={!canWrite} onClick={() => openEdit(r)}>
            编辑
          </Button>
          <Button
            size="small"
            type="link"
            danger
            icon={<DeleteOutlined />}
            disabled={!canWrite}
            onClick={() => remove(r)}
          >
            删除
          </Button>
        </Space>
      ),
    },
  ];

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

      <SectionCard bodyStyle={{ padding: 0 }}>
        <Table<Team>
          rowKey={(r) => r.id}
          size="small"
          loading={listApi.loading}
          dataSource={teams}
          columns={columns}
          pagination={false}
        />
      </SectionCard>

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
