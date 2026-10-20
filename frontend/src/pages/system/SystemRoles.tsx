import React, { useEffect, useMemo, useState } from 'react';
import { Alert, App as AntApp, Button, Checkbox, Empty, Input, Space, Spin, Tag, Tooltip, Typography } from 'antd';
import { LockOutlined, SaveOutlined } from '@ant-design/icons';
import { PageHeader, SectionCard } from '@/components/ui';
import { roleApi } from '@/api';
import { hasPerm } from '@/auth';
import type { PermissionPoint, SysRole } from '@/types';

const { Text } = Typography;

/**
 * 模块中文名。
 *
 * 后端给的是 `ai` / `build` / `deploy` 这类机器码，直接铺在界面上是"给开发看的"。
 * 这里只做**展示**映射，勾选与提交仍用原码 —— 不要把它当成"权限点清单"的另一份来源，
 * 缺哪个模块会自动落到"其它"，不会把权限点弄丢。
 */
const MODULE_LABEL: Record<string, string> = {
  ai: 'AI 助手',
  audit: '审计',
  build: '构建',
  cd: '环境部署',
  deploy: '部署',
  env: '环境',
  environment: '环境',
  image: '镜像',
  log: '日志',
  notify: '通知',
  project: '项目',
  role: '角色',
  rollback: '回滚',
  service: '服务实例',
  system: '系统',
  user: '用户',
  cluster: '集群',
  model: '模型',
  runner: '构建执行',
};

/**
 * 角色权限管理。
 *
 * 补的是这么个洞：`SystemUsers` 只能给人**分配角色**，但**没有任何界面能配
 * "每个角色到底有哪些权限点"** —— 后端 `PUT /roles/{code}/permissions` 早就有了，
 * 前端一直没入口，等于权限模型只能靠种子数据。
 */
const SystemRoles: React.FC = () => {
  const { message } = AntApp.useApp();
  const canWrite = hasPerm('role:manage') || hasPerm('user:manage');

  const [roles, setRoles] = useState<SysRole[]>([]);
  const [points, setPoints] = useState<PermissionPoint[]>([]);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const [active, setActive] = useState<string | undefined>();
  /** 当前编辑中的勾选（与角色上的 perms 分开：改完没保存时要能看出差异） */
  const [draft, setDraft] = useState<string[]>([]);
  const [kw, setKw] = useState('');

  const load = async () => {
    setLoading(true);
    setError(null);
    try {
      const [rs, ps] = await Promise.all([roleApi.list(), roleApi.permissions()]);
      setRoles(rs);
      setPoints(ps);
      const first = rs.find((r) => !r.perms.includes('*')) ?? rs[0];
      setActive(first?.key);
      setDraft(first?.perms ?? []);
    } catch (e) {
      setError((e as Error)?.message ?? '读取角色/权限点失败');
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
  }, []);

  const role = roles.find((r) => r.key === active);
  /** `*` = 全部权限。它是**结构性**的（后端按通配匹配），不该被逐项勾选表改掉 */
  const isSuper = !!role?.perms.includes('*');
  const dirty = useMemo(() => {
    if (!role || isSuper) return false;
    const a = [...draft].sort().join(',');
    const b = [...role.perms].sort().join(',');
    return a !== b;
  }, [draft, role, isSuper]);

  const grouped = useMemo(() => {
    const q = kw.trim().toLowerCase();
    const map = new Map<string, PermissionPoint[]>();
    points
      .filter((p) => !q || p.code.toLowerCase().includes(q) || (p.name || '').toLowerCase().includes(q))
      .forEach((p) => {
        const arr = map.get(p.module) ?? [];
        arr.push(p);
        map.set(p.module, arr);
      });
    return [...map.entries()].sort((a, b) => a[0].localeCompare(b[0]));
  }, [points, kw]);

  const save = async () => {
    if (!role) return;
    setSaving(true);
    try {
      const updated = await roleApi.setPermissions(role.key, draft, false);
      setRoles((prev) => prev.map((r) => (r.key === role.key ? { ...r, ...updated } : r)));
      setDraft(updated.perms ?? draft);
      message.success(`已保存「${role.name}」的权限`);
    } catch (e) {
      message.error((e as Error)?.message ?? '保存失败');
    } finally {
      setSaving(false);
    }
  };

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: '系统管理' }, { title: '角色权限' }]}
        title="角色权限"
        subTitle="ROLES & PERMISSIONS"
        description="配置每个角色拥有哪些权限点。改动对**下一次请求**生效（权限在每次请求时现查，不缓存在令牌里）"
      />

      {error ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message="加载失败"
          description={error}
          action={<Button size="small" onClick={load}>重试</Button>}
        />
      ) : null}

      <div style={{ display: 'grid', gridTemplateColumns: '230px minmax(0, 1fr)', gap: 16, alignItems: 'start' }}>
        <SectionCard bodyStyle={{ padding: 10 }}>
          {loading ? (
            <div style={{ padding: 24, textAlign: 'center' }}>
              <Spin size="small" />
            </div>
          ) : (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
              {roles.map((r) => {
                const on = r.key === active;
                return (
                  <div
                    key={r.key}
                    onClick={() => {
                      setActive(r.key);
                      setDraft(r.perms);
                    }}
                    style={{
                      padding: '8px 10px',
                      borderRadius: 6,
                      cursor: 'pointer',
                      background: on ? '#eaf3ff' : 'transparent',
                      border: `1px solid ${on ? '#c4ddff' : 'transparent'}`,
                    }}
                  >
                    <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                      <span style={{ fontSize: 13, fontWeight: on ? 500 : 400, color: on ? '#0958d9' : undefined }}>
                        {r.name}
                      </span>
                      {r.perms.includes('*') ? (
                        <Tooltip title="拥有全部权限（通配 *），不逐项配置">
                          <LockOutlined style={{ fontSize: 11, color: '#9aa0aa' }} />
                        </Tooltip>
                      ) : null}
                    </div>
                    <Text type="secondary" style={{ fontSize: 11.5 }}>
                      {r.key} · {r.users} 人
                    </Text>
                  </div>
                );
              })}
            </div>
          )}
        </SectionCard>

        <SectionCard bodyStyle={{ padding: 0 }}>
          {!role ? (
            <div style={{ padding: 48 }}>
              <Empty description="选择一个角色来配置权限" />
            </div>
          ) : (
            <>
              <div className="yn-filter-bar">
                <Space size={12} wrap>
                  <span style={{ fontSize: 13 }}>
                    <b>{role.name}</b>
                    <Text type="secondary" style={{ fontSize: 12, marginLeft: 8 }}>
                      {role.desc}
                    </Text>
                  </span>
                  <Tag style={{ marginInlineEnd: 0 }}>{draft.length} 个权限点</Tag>
                  {dirty ? <Tag color="orange" style={{ marginInlineEnd: 0 }}>未保存</Tag> : null}
                  <Input
                    allowClear
                    placeholder="搜索权限点"
                    style={{ width: 200 }}
                    value={kw}
                    onChange={(e) => setKw(e.target.value)}
                  />
                  <Button
                    type="primary"
                    icon={<SaveOutlined />}
                    loading={saving}
                    disabled={!dirty || isSuper || !canWrite}
                    onClick={save}
                  >
                    保存
                  </Button>
                </Space>
              </div>

              {isSuper ? (
                <div style={{ padding: 16 }}>
                  <Alert
                    type="info"
                    showIcon
                    message="该角色拥有全部权限（通配 *）"
                    description="它不按权限点逐项匹配，因此不在这里配置。要收窄它的能力，请改用其它角色。"
                  />
                </div>
              ) : (
                <div style={{ padding: '12px 16px 20px' }}>
                  {grouped.length === 0 ? (
                    <Empty description="没有匹配的权限点" />
                  ) : (
                    grouped.map(([mod, list]) => (
                      <div key={mod} style={{ marginBottom: 18 }}>
                        <div style={{ fontSize: 12, color: '#5a6478', marginBottom: 8 }}>
                          {MODULE_LABEL[mod] ?? mod}
                        </div>
                        <Checkbox.Group
                          value={draft}
                          onChange={(v) => setDraft(v as string[])}
                          style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(210px, 1fr))', gap: 8 }}
                        >
                          {list.map((p) => (
                            <Checkbox key={p.code} value={p.code} disabled={!canWrite}>
                              <span style={{ fontSize: 12.5 }}>{p.name || p.code}</span>
                              <Tooltip title={p.description || p.code}>
                                <span className="yn-mono" style={{ fontSize: 11, color: '#b0b6c2', marginLeft: 6 }}>
                                  {p.code}
                                </span>
                              </Tooltip>
                            </Checkbox>
                          ))}
                        </Checkbox.Group>
                      </div>
                    ))
                  )}
                </div>
              )}
            </>
          )}
        </SectionCard>
      </div>
    </div>
  );
};

export default SystemRoles;
