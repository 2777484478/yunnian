import React, { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Avatar,
  Button,
  Card,
  Checkbox,
  Col,
  Drawer,
  Form,
  Input,
  Modal,
  Row,
  Select,
  Space,
  Switch,
  Table,
  Tabs,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  PlusOutlined,
  ReloadOutlined,
  SafetyCertificateOutlined,
  SearchOutlined,
  TeamOutlined,
  UserSwitchOutlined,
  WarningOutlined,
} from '@ant-design/icons';
import { PageHeader, SectionCard, StatCard } from '@/components/ui';
import { RoleTag } from '@/components/StatusTag';
import { roleApi, userApi } from '@/api';
import { getSession, hasPerm } from '@/auth';
import { useApi } from '@/hooks/useApi';
import { fmt } from '@/utils/format';
import type { PermissionPoint, SysRole, SysUser, UserRole } from '@/types';

const { Text } = Typography;

// ---------------------------------------------------------------------------
// 用户与权限
//
// 两个约束值得记下来：
//
// 1. **角色权限是"覆盖式"写入**，不是增量。后端 PUT /roles/{code}/permissions
//    用整份列表替换旧值，且拒绝空列表（防误清空）。所以编辑器必须
//    一次性提交勾选结果，并在为空时显式提示。
//
// 2. **不能停用/降权自己**。后端会返回 40000，前端也提前拦一道——
//    把"最后一个管理员把自己锁在门外"变成一句界面提示，而不是一次失败的请求。
// ---------------------------------------------------------------------------

const SystemUsers: React.FC = () => {
  const { message, notification } = AntApp.useApp();
  const me = getSession();
  const canManage = hasPerm('user:manage');
  const canEditRole = hasPerm('role:manage');

  const [tab, setTab] = useState('users');

  // ---------------------------------------------------------------- 用户列表
  const [keywordInput, setKeywordInput] = useState('');
  const [keyword, setKeyword] = useState('');
  const [roleFilter, setRoleFilter] = useState<string>('all');
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(10);

  useEffect(() => {
    const t = setTimeout(() => {
      setKeyword(keywordInput.trim());
      setPage(1);
    }, 350);
    return () => clearTimeout(t);
  }, [keywordInput]);

  const usersApi = useApi(
    () =>
      userApi.list({
        keyword,
        role: roleFilter === 'all' ? undefined : roleFilter,
        page,
        pageSize,
      }),
    [keyword, roleFilter, page, pageSize],
  );
  const users = usersApi.data?.list ?? [];
  const total = usersApi.data?.total ?? 0;

  const rolesApi = useApi(() => roleApi.list(), []);
  const roles = useMemo(() => rolesApi.data ?? [], [rolesApi.data]);

  // 统计：总数与启用数各借一次 pageSize=1 的 total；管理员数直接用角色接口
  // 自带的 users 计数，不再单独查一遍用户表。
  const enabledApi = useApi(() => userApi.list({ enabled: true, pageSize: 1 }), []);
  const stats = useMemo(() => {
    const adminRoles = roles.filter((r) => r.key === 'admin' || r.key === 'ops');
    return {
      total: total,
      enabled: enabledApi.data?.total ?? 0,
      roleCount: roles.length,
      adminCount: adminRoles.reduce((s, r) => s + r.users, 0),
    };
  }, [total, enabledApi.data, roles]);

  const refresh = () => {
    usersApi.refresh();
    rolesApi.refresh();
    enabledApi.refresh();
  };

  // -------------------------------------------------------------- 启停账号
  const [toggling, setToggling] = useState<string | null>(null);

  const toggleEnabled = async (u: SysUser, next: boolean) => {
    if (u.id === me?.id) {
      message.warning('不能停用当前登录账号');
      return;
    }
    setToggling(u.id);
    try {
      await userApi.update(u.id, { enabled: next });
      message.success(`已${next ? '启用' : '停用'}账号 ${u.username}`);
      refresh();
    } catch (e) {
      // 失败时什么都不改：列表本身没做乐观更新，下一次 refresh 就是服务端真相
      message.error((e as Error)?.message || '操作失败');
    } finally {
      setToggling(null);
    }
  };

  // -------------------------------------------------------------- 改角色
  const [roleTarget, setRoleTarget] = useState<SysUser | null>(null);
  const [roleValue, setRoleValue] = useState<string>('dev');
  const [savingRole, setSavingRole] = useState(false);

  const openRoleModal = (u: SysUser) => {
    setRoleTarget(u);
    setRoleValue(u.role);
  };

  const submitRole = async () => {
    if (!roleTarget) return;
    setSavingRole(true);
    try {
      await userApi.update(roleTarget.id, { role: roleValue });
      notification.success({
        message: '角色已调整',
        description: `${roleTarget.name} 的角色已改为「${roles.find((r) => r.key === roleValue)?.name ?? roleValue}」，下次请求即生效。`,
      });
      setRoleTarget(null);
      refresh();
    } catch (e) {
      message.error((e as Error)?.message || '调整失败');
    } finally {
      setSavingRole(false);
    }
  };

  // -------------------------------------------------------------- 重置密码
  const [pwdTarget, setPwdTarget] = useState<SysUser | null>(null);
  const [pwdForm] = Form.useForm<{ password: string }>();
  const [resetting, setResetting] = useState(false);

  const submitPassword = async () => {
    if (!pwdTarget) return;
    const v = await pwdForm.validateFields();
    setResetting(true);
    try {
      await userApi.resetPassword(pwdTarget.id, { password: v.password });
      notification.success({
        message: '密码已重置',
        description: `${pwdTarget.name}（${pwdTarget.username}）的登录密码已更新，请通过安全渠道告知本人。`,
      });
      setPwdTarget(null);
      pwdForm.resetFields();
    } catch (e) {
      message.error((e as Error)?.message || '重置失败');
    } finally {
      setResetting(false);
    }
  };

  // -------------------------------------------------------------- 邀请用户
  const [inviteOpen, setInviteOpen] = useState(false);
  const [inviteForm] = Form.useForm();
  const [inviting, setInviting] = useState(false);

  const submitInvite = async () => {
    const v = await inviteForm.validateFields();
    setInviting(true);
    try {
      const created = await userApi.invite({
        username: v.username,
        name: v.name,
        email: v.email,
        role: v.role,
        ...(v.phone ? { phone: v.phone } : {}),
        ...(v.password ? { password: v.password } : {}),
      });
      notification.success({
        message: '用户已创建',
        description: v.password
          ? `${created.name}（${created.username}）已创建，请把初始密码告知本人。`
          : `${created.name}（${created.username}）已创建，使用平台初始密码登录，建议首次登录后修改。`,
      });
      setInviteOpen(false);
      inviteForm.resetFields();
      refresh();
    } catch (e) {
      message.error((e as Error).message);
    } finally {
      setInviting(false);
    }
  };

  // ---------------------------------------------------------- 角色权限编辑
  const [permRole, setPermRole] = useState<SysRole | null>(null);
  const permPointsApi = useApi(() => roleApi.permissions(), [], { enabled: !!permRole });
  const [checkedPerms, setCheckedPerms] = useState<string[]>([]);
  const [savingPerms, setSavingPerms] = useState(false);

  const openPermEditor = (r: SysRole) => {
    setPermRole(r);
    setCheckedPerms(r.perms);
  };

  const permGroups = useMemo(() => {
    const points = permPointsApi.data ?? [];
    const m = new Map<string, PermissionPoint[]>();
    points.forEach((p) => {
      const arr = m.get(p.module) ?? [];
      arr.push(p);
      m.set(p.module, arr);
    });
    return [...m.entries()];
  }, [permPointsApi.data]);

  const submitPerms = async () => {
    if (!permRole) return;
    if (checkedPerms.length === 0) {
      message.warning('不能把角色权限清空——如需停用某个角色，请改为调整其成员的角色');
      return;
    }
    setSavingPerms(true);
    try {
      await roleApi.setPermissions(permRole.key, checkedPerms);
      notification.success({
        message: '角色权限已更新',
        description: `「${permRole.name}」现有 ${checkedPerms.length} 个权限点，${
          permRole.users
        } 名成员下次请求即生效。`,
      });
      setPermRole(null);
      refresh();
    } catch (e) {
      message.error((e as Error)?.message || '保存失败');
    } finally {
      setSavingPerms(false);
    }
  };

  const columns: ColumnsType<SysUser> = [
    {
      title: '用户',
      dataIndex: 'name',
      width: 240,
      render: (v: string, r) => (
        <Space size={10}>
          <Avatar size={32} style={{ background: r.enabled ? '#1677ff' : '#d9d9d9', fontSize: 13 }}>
            {v.slice(0, 1)}
          </Avatar>
          <div>
            <div style={{ fontWeight: 500 }}>
              {v}
              {r.id === me?.id ? (
                <Tag color="blue" style={{ marginLeft: 6, marginInlineEnd: 0, fontSize: 11 }}>
                  当前账号
                </Tag>
              ) : null}
            </div>
            <div className="yn-mono" style={{ fontSize: 12, color: '#9aa0aa' }}>
              {r.username}
            </div>
          </div>
        </Space>
      ),
    },
    { title: '角色', dataIndex: 'role', width: 130, render: (v: UserRole) => <RoleTag role={v} /> },
    {
      title: '邮箱',
      dataIndex: 'email',
      width: 220,
      render: (v: string) => (
        <span className="yn-mono" style={{ fontSize: 12.5 }}>
          {v}
        </span>
      ),
    },
    {
      title: '手机号',
      dataIndex: 'phone',
      width: 130,
      render: (v: string) => (
        <span className="yn-mono" style={{ fontSize: 12.5 }}>
          {v || '-'}
        </span>
      ),
    },
    {
      title: '所属团队',
      dataIndex: 'teams',
      width: 200,
      render: (v: string[]) =>
        v.length ? (
          <Space size={4} wrap>
            {v.map((t) => (
              <Tag key={t} style={{ marginInlineEnd: 0 }}>
                {t}
              </Tag>
            ))}
          </Space>
        ) : (
          <Text type="secondary">-</Text>
        ),
    },
    {
      title: '状态',
      dataIndex: 'enabled',
      width: 100,
      render: (v: boolean, r) => (
        <Tooltip
          title={
            r.id === me?.id
              ? '不能停用当前登录账号'
              : v
                ? '点击可停用账号'
                : '点击可启用账号'
          }
        >
          <Switch
            size="small"
            checked={v}
            loading={toggling === r.id}
            disabled={!canManage || r.id === me?.id}
            onChange={(next) => toggleEnabled(r, next)}
          />
        </Tooltip>
      ),
    },
    {
      title: '最近登录',
      dataIndex: 'lastLoginAt',
      width: 168,
      render: (v: string) => (
        <span className="yn-mono" style={{ color: '#8c8c8c' }}>
          {v ? fmt(v) : '从未登录'}
        </span>
      ),
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
            style={{ paddingInline: 4 }}
            disabled={!canManage}
            onClick={() => {
              pwdForm.resetFields();
              setPwdTarget(r);
            }}
          >
            重置密码
          </Button>
          <Button
            type="link"
            size="small"
            style={{ paddingInline: 4 }}
            disabled={!canManage}
            onClick={() => openRoleModal(r)}
          >
            改角色
          </Button>
        </Space>
      ),
    },
  ];

  const roleOptions = roles.map((r) => ({ label: r.name, value: r.key }));

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: '系统管理' }, { title: '用户与权限' }]}
        title="用户与权限"
        description="基于 RBAC 的角色权限模型：管理员 / 运维 / 开发 / 测试 / 只读用户，支持按团队与项目授权"
        extra={
          <Space>
            <Button
              icon={<ReloadOutlined />}
              onClick={() => {
                refresh();
                message.success('已刷新');
              }}
            >
              刷新
            </Button>
            <Tooltip title={canManage ? '' : '需要 user:manage 权限'}>
              <Button
                type="primary"
                icon={<PlusOutlined />}
                disabled={!canManage}
                onClick={() => {
                  inviteForm.setFieldsValue({ role: 'dev' });
                  setInviteOpen(true);
                }}
              >
                新增用户
              </Button>
            </Tooltip>
          </Space>
        }
      />

      <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
        <Col xs={12} xl={6}>
          <StatCard title="用户总数" value={stats.total} unit="人" icon={<TeamOutlined />} />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="启用账号"
            value={stats.enabled}
            unit="人"
            accent="#52c41a"
            footer={
              stats.total > stats.enabled ? (
                <Text type="secondary" style={{ fontSize: 12 }}>
                  停用 {stats.total - stats.enabled} 人
                </Text>
              ) : undefined
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="角色数"
            value={stats.roleCount}
            unit="个"
            accent="#722ed1"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                RBAC 最小权限原则
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="管理员"
            value={stats.adminCount}
            unit="人"
            accent="#faad14"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                含运维角色（按角色成员数汇总）
              </Text>
            }
          />
        </Col>
      </Row>

      <Tabs
        activeKey={tab}
        onChange={setTab}
        items={[
          {
            key: 'users',
            label: '用户列表',
            children: (
              <SectionCard bodyStyle={{ padding: 0 }}>
                <div className="yn-filter-bar">
                  <Space size={12} wrap>
                    <Input
                      allowClear
                      prefix={<SearchOutlined style={{ color: '#b5bac4' }} />}
                      placeholder="搜索姓名 / 账号 / 邮箱"
                      style={{ width: 260 }}
                      value={keywordInput}
                      onChange={(e) => setKeywordInput(e.target.value)}
                    />
                    <Select
                      style={{ width: 150 }}
                      value={roleFilter}
                      onChange={(v) => {
                        setRoleFilter(v);
                        setPage(1);
                      }}
                      options={[{ label: '全部角色', value: 'all' }, ...roleOptions]}
                    />
                  </Space>
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    共 {total} 个用户
                  </Text>
                </div>

                {usersApi.error ? (
                  <div style={{ padding: 16 }}>
                    <Alert
                      type="error"
                      showIcon
                      message="用户列表加载失败"
                      description={usersApi.error.message}
                      action={
                        <Button size="small" onClick={usersApi.refresh}>
                          重试
                        </Button>
                      }
                    />
                  </div>
                ) : null}

                <Table<SysUser>
                  rowKey="id"
                  size="middle"
                  loading={usersApi.loading}
                  dataSource={users}
                  columns={columns}
                  scroll={{ x: 1500 }}
                  locale={{ emptyText: usersApi.loading ? ' ' : '没有符合条件的用户' }}
                  pagination={{
                    current: page,
                    pageSize,
                    total,
                    showSizeChanger: true,
                    onChange: (p, s) => {
                      setPage(p);
                      setPageSize(s);
                    },
                    showTotal: (t) => `共 ${t} 个用户`,
                  }}
                />
              </SectionCard>
            ),
          },
          {
            key: 'roles',
            label: '角色与权限',
            children: (
              <>
                <Alert
                  type="info"
                  showIcon
                  style={{ marginBottom: 16 }}
                  message="权限点支持通配"
                  description="如 deploy:* 表示该环境下的全部部署动作；deploy:test,staging 表示仅在测试区与预发区可部署。权限变更保存后立即生效，无需重新登录。"
                />
                <Row gutter={[16, 16]}>
                  {roles.map((r) => (
                    <Col key={r.key} xs={24} md={12} xl={8}>
                      <Card
                        className="yn-section-card"
                        variant="borderless"
                        style={{ height: '100%' }}
                        title={
                          <Space>
                            <SafetyCertificateOutlined style={{ color: '#1677ff' }} />
                            <span>{r.name}</span>
                            <Tag color={r.color} style={{ marginInlineEnd: 0 }}>
                              {r.key}
                            </Tag>
                          </Space>
                        }
                        extra={
                          <Text type="secondary" style={{ fontSize: 12 }}>
                            {r.users} 人
                          </Text>
                        }
                      >
                        <div style={{ fontSize: 13, color: '#5c6470', lineHeight: 1.7 }}>{r.desc}</div>
                        <div style={{ marginTop: 12 }}>
                          <div style={{ fontSize: 12, color: '#9aa0aa', marginBottom: 8 }}>
                            权限范围（{r.perms.length} 项）
                          </div>
                          <Space size={6} wrap>
                            {r.perms.map((p) => (
                              <Tag key={p} className="yn-mono" style={{ marginInlineEnd: 0, fontSize: 11.5 }}>
                                {p}
                              </Tag>
                            ))}
                          </Space>
                        </div>
                        <div style={{ marginTop: 14 }}>
                          <Tooltip title={canEditRole ? '' : '需要 role:manage 权限'}>
                            <Button
                              size="small"
                              icon={<UserSwitchOutlined />}
                              disabled={!canEditRole}
                              onClick={() => openPermEditor(r)}
                            >
                              编辑权限
                            </Button>
                          </Tooltip>
                        </div>
                      </Card>
                    </Col>
                  ))}
                  {!rolesApi.loading && !roles.length ? (
                    <Col span={24}>
                      <Alert type="warning" showIcon message="未取到角色数据" description="请检查后端 /roles 接口是否可用。" />
                    </Col>
                  ) : null}
                </Row>
              </>
            ),
          },
        ]}
      />

      {/* ---------------------------------------------------------- 改角色 */}
      <Modal
        open={!!roleTarget}
        title={`调整角色 · ${roleTarget?.name ?? ''}`}
        okText="保存"
        cancelText="取消"
        confirmLoading={savingRole}
        onOk={submitRole}
        onCancel={() => setRoleTarget(null)}
      >
        <Space direction="vertical" size={14} style={{ width: '100%' }}>
          <Alert
            type="warning"
            showIcon
            icon={<WarningOutlined />}
            message="降权会立即生效"
            description="若把最后一个启用的管理员降权，平台将无人可管理——后端会拒绝这类操作。"
          />
          <div>
            <div style={{ fontSize: 13, marginBottom: 6 }}>目标角色</div>
            <Select
              style={{ width: '100%' }}
              value={roleValue}
              onChange={setRoleValue}
              options={roles.map((r) => ({ label: `${r.name} — ${r.desc}`, value: r.key }))}
            />
          </div>
        </Space>
      </Modal>

      {/* -------------------------------------------------------- 重置密码 */}
      <Modal
        open={!!pwdTarget}
        title={`重置密码 · ${pwdTarget?.name ?? ''}`}
        okText="确认重置"
        cancelText="取消"
        confirmLoading={resetting}
        onOk={submitPassword}
        onCancel={() => setPwdTarget(null)}
        destroyOnClose
      >
        <Form form={pwdForm} layout="vertical" requiredMark="optional">
          <Alert
            type="info"
            showIcon
            style={{ marginBottom: 14 }}
            message="管理员重置后无提示，请自行通过安全渠道告知本人"
          />
          <Form.Item
            name="password"
            label="新密码"
            rules={[
              { required: true, message: '请输入新密码' },
              { min: 8, max: 128, message: '密码长度需 8–128 位' },
            ]}
          >
            <Input.Password placeholder="至少 8 位" autoComplete="new-password" />
          </Form.Item>
        </Form>
      </Modal>

      {/* -------------------------------------------------------- 新增用户 */}
      <Drawer
        open={inviteOpen}
        width={520}
        title="新增用户"
        onClose={() => setInviteOpen(false)}
        extra={
          <Space>
            <Button onClick={() => setInviteOpen(false)}>取消</Button>
            <Button type="primary" loading={inviting} onClick={submitInvite}>
              创建
            </Button>
          </Space>
        }
        destroyOnClose
      >
        <Form form={inviteForm} layout="vertical" requiredMark="optional">
          <Alert
            type="info"
            showIcon
            style={{ marginBottom: 16 }}
            message="账号由平台直接创建（一期未接入 SSO）"
            description="密码留空则使用平台初始密码 changeme，建议提醒用户首次登录后立即修改。"
          />
          <Form.Item
            name="username"
            label="登录账号"
            rules={[
              { required: true, message: '请输入登录账号' },
              { min: 3, max: 64, message: '长度需 3–64 位' },
              { pattern: /^[A-Za-z0-9]+$/, message: '仅支持字母与数字（后端 alphanum 约束）' },
            ]}
          >
            <Input className="yn-mono" placeholder="如 zhangsan" />
          </Form.Item>
          <Form.Item name="name" label="姓名" rules={[{ required: true, message: '请输入姓名' }]}>
            <Input placeholder="如 张三" />
          </Form.Item>
          <Form.Item
            name="email"
            label="邮箱"
            rules={[
              { required: true, message: '请输入邮箱' },
              { type: 'email', message: '邮箱格式不正确' },
            ]}
          >
            <Input className="yn-mono" placeholder="zhangsan@yunnian.cn" />
          </Form.Item>
          <Form.Item name="phone" label="手机号" rules={[{ max: 20, message: '最多 20 位' }]}>
            <Input className="yn-mono" placeholder="选填" />
          </Form.Item>
          <Form.Item name="role" label="授予角色" rules={[{ required: true, message: '请选择角色' }]}>
            <Select options={roles.map((r) => ({ label: `${r.name} — ${r.desc}`, value: r.key }))} />
          </Form.Item>
          <Form.Item
            name="password"
            label="初始密码"
            extra="留空则使用平台初始密码"
            rules={[{ min: 8, max: 128, message: '密码长度需 8–128 位' }]}
          >
            <Input.Password placeholder="选填，至少 8 位" autoComplete="new-password" />
          </Form.Item>
        </Form>
      </Drawer>

      {/* ------------------------------------------------------ 编辑角色权限 */}
      <Modal
        open={!!permRole}
        width={720}
        title={`编辑权限 · ${permRole?.name ?? ''}`}
        okText="保存"
        cancelText="取消"
        confirmLoading={savingPerms}
        onOk={submitPerms}
        onCancel={() => setPermRole(null)}
        destroyOnClose
      >
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 14 }}
          message="覆盖式保存"
          description={`提交后会以当前勾选结果**替换**该角色的全部权限（现 ${permRole?.perms.length ?? 0} 项）。已选 ${checkedPerms.length} 项。`}
        />
        <div style={{ maxHeight: 420, overflowY: 'auto', paddingRight: 4 }}>
          {permPointsApi.loading ? (
            <Text type="secondary">正在加载权限点清单…</Text>
          ) : (
            permGroups.map(([mod, points]) => (
              <div key={mod} style={{ marginBottom: 14 }}>
                <div style={{ fontSize: 12.5, color: '#8c8c8c', marginBottom: 6 }}>
                  {mod}（{points.length}）
                </div>
                <Checkbox.Group
                  value={checkedPerms.filter((p) => points.some((x) => x.code === p))}
                  onChange={(vals) => {
                    const others = checkedPerms.filter((p) => !points.some((x) => x.code === p));
                    setCheckedPerms([...others, ...(vals as string[])]);
                  }}
                >
                  <Space direction="vertical" size={4}>
                    {points.map((p) => (
                      <Checkbox key={p.code} value={p.code}>
                        <span style={{ fontSize: 13 }}>{p.name}</span>
                        <span className="yn-mono" style={{ marginLeft: 8, fontSize: 11.5, color: '#9aa0aa' }}>
                          {p.code}
                        </span>
                      </Checkbox>
                    ))}
                  </Space>
                </Checkbox.Group>
              </div>
            ))
          )}
        </div>
      </Modal>
    </div>
  );
};

export default SystemUsers;
