import React, { useEffect, useMemo, useState } from 'react';
import { Alert, App as AntApp, Button, Checkbox, Select, Empty, Input, Space, Spin, Tag, Tooltip, Typography } from 'antd';
import { LockOutlined, SaveOutlined } from '@ant-design/icons';
import { PageHeader, SectionCard } from '@/components/ui';
import { environmentApi, roleApi } from '@/api';
import { hasPerm } from '@/auth';
import { useApi } from '@/hooks/useApi';
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

  /** 环境列表 —— 只用于"可见范围"多选 */
  const envApi = useApi(() => environmentApi.list(), []);
  const allEnvs = useMemo(() => envApi.data?.list ?? [], [envApi.data]);

  /**
   * 环境可见范围用**独立 state**，不再"从 draft 里派生 + 写回 draft"。
   *
   * ⚠️ 之前那样做的结果就是**多选只能选一个**（2026-10-20 用户实测）：
   * `visibleKeys` 是 `draft` 的派生值，而 `setVisibleKeys` 又依赖**渲染时快照的 draft**
   * 去做过滤 —— 选第二个时，`onChange` 给的完整数组被写进 draft，但下一次渲染的
   * 派生值可能与 Select 内部状态错位，表现为"选一个就把上一个顶掉"。
   *
   * 派生 + 回写同一个状态，本质是在拿 React 的渲染时序赌正确性。独立 state 不赌。
   * 提交时才合并：`draft` 里其它权限 + `env:view:<key>`。
   */
  const [visibleDraft, setVisibleDraft] = useState<string[]>([]);

  /** 切角色时把该角色已配的可见环境读进 visibleDraft（只在**换角色**时同步） */
  useEffect(() => {
    const r = roles.find((x) => x.key === active);
    if (!r || r.perms.includes('*')) {
      setVisibleDraft([]);
      return;
    }
    setVisibleDraft(r.perms.filter((p) => p.startsWith('env:view:')).map((p) => p.slice('env:view:'.length)));
    // 依赖只放 active：保存后 roles 会刷新，但那时不该把用户正在改的勾选冲掉
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active]);

  /** 合并成最终要提交的权限点数组 */
  const mergedPerms = useMemo(
    () => [...draft.filter((p) => !p.startsWith('env:view:')), ...visibleDraft.map((k) => `env:view:${k}`)],
    [draft, visibleDraft],
  );

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
    const a = [...mergedPerms].sort().join(',');
    const b = [...role.perms].sort().join(',');
    return a !== b;
  }, [mergedPerms, role, isSuper]);

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
      const updated = await roleApi.setPermissions(role.key, mergedPerms, false);
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
        description="配置每个角色拥有哪些权限点。改动对下一次请求生效（权限在每次请求时现查，不缓存在令牌里）"
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
                      display: 'flex',
                      alignItems: 'center',
                      gap: 9,
                      padding: '8px 10px',
                      borderRadius: 8,
                      cursor: 'pointer',
                      background: on ? '#eaf3ff' : 'transparent',
                      border: `1px solid ${on ? '#c4ddff' : 'transparent'}`,
                    }}
                  >
                    <div
                      style={{
                        width: 28,
                        height: 28,
                        borderRadius: 8,
                        flex: 'none',
                        display: 'flex',
                        alignItems: 'center',
                        justifyContent: 'center',
                        fontSize: 13,
                        fontWeight: 600,
                        color: on ? '#0958d9' : '#5a6478',
                        background: on ? '#d3e7ff' : '#f1f4f9',
                      }}
                    >
                      {r.name.slice(0, 1)}
                    </div>
                    <div style={{ minWidth: 0, flex: 1 }}>
                      <div style={{ display: 'flex', alignItems: 'center', gap: 5 }}>
                        <span style={{ fontSize: 13, fontWeight: on ? 500 : 400, color: on ? '#0958d9' : '#1f2a44' }}>
                          {r.name}
                        </span>
                        {r.perms.includes('*') ? (
                          <Tooltip title="拥有全部权限（通配 *），不逐项配置">
                            <LockOutlined style={{ fontSize: 11, color: '#9aa0aa' }} />
                          </Tooltip>
                        ) : null}
                      </div>
                      <div style={{ fontSize: 11.5, color: '#8c8c95', marginTop: 1 }}>
                        {r.perms.includes('*') ? '全部权限' : `${r.perms.length} 项权限`} · {r.users} 人
                      </div>
                    </div>
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
                  <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
                    <b style={{ fontSize: 14, color: '#1f2a44' }}>{role.name}</b>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      {role.desc}
                    </Text>
                  </span>
                  <Tag color={isSuper ? 'red' : 'blue'} style={{ marginInlineEnd: 0 }}>
                    {isSuper ? '全部权限' : `${draft.length} / ${points.length} 个权限点`}
                  </Tag>
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
                  {/* 环境可见范围：不在"权限点"清单里逐个勾，而是直接选环境 ——
                      用户脑子里想的是"他能看哪几台"，不是"他有 env:view:test-152 这个码"。
                      存库仍是 env:view:<key>，与后端 VisibleEnvKeys 一一对应。 */}
                  <div
                    style={{
                      marginBottom: 18,
                      padding: '12px 14px',
                      background: '#f7f9fc',
                      border: '1px solid #eef1f6',
                      borderRadius: 8,
                    }}
                  >
                    <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 8 }}>
                      <span style={{ width: 3, height: 12, borderRadius: 2, background: '#1677ff', opacity: 0.7 }} />
                      <span style={{ fontSize: 12.5, fontWeight: 500, color: '#1f2a44' }}>环境可见范围</span>
                      <Text type="secondary" style={{ fontSize: 11.5 }}>
                        没配过 = 全部可见；配了之后看不到未选中的环境（列表里就没有）
                      </Text>
                    </div>
                    <Select
                      mode="multiple"
                      allowClear
                      showSearch
                      optionFilterProp="label"
                      style={{ width: '100%' }}
                      placeholder={isSuper || draft.includes('env:*') ? '当前角色可见全部环境' : '选要开放给该角色的环境'}
                      disabled={!canWrite || isSuper || draft.includes('env:*')}
                      value={visibleDraft}
                      onChange={(v) => setVisibleDraft(v as string[])}
                      options={allEnvs.map((e) => ({ label: `${e.name}（${e.key}）`, value: e.key }))}
                      loading={envApi.loading}
                    />
                  </div>

                  {grouped.length === 0 ? (
                    <Empty description="没有匹配的权限点" />
                  ) : (
                    grouped.map(([mod, list]) => (
                      <div key={mod} style={{ marginBottom: 18 }}>
                        <div
                          style={{
                            display: 'flex',
                            alignItems: 'center',
                            gap: 6,
                            fontSize: 12.5,
                            fontWeight: 500,
                            color: '#1f2a44',
                            marginBottom: 8,
                          }}
                        >
                          <span style={{ width: 3, height: 12, borderRadius: 2, background: '#1677ff', opacity: 0.7 }} />
                          {MODULE_LABEL[mod] ?? mod}
                          <span style={{ fontSize: 11.5, color: '#b0b6c2', fontWeight: 400 }}>{list.length}</span>
                        </div>
                        <Checkbox.Group
                          value={draft}
                          onChange={(v) => setDraft(v as string[])}
                          style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(210px, 1fr))', gap: 8 }}
                        >
                          {list.map((p) => (
                            <Checkbox key={p.code} value={p.code} disabled={!canWrite}>
                              <span style={{ fontSize: 12.5 }}>{p.name || p.code}</span>
                              {/* 通配权限（`build:*` 这类）单独标一下：它一项顶一片，
                                  和 `build:read` 并排摆着却不区分，很容易以为勾了它还要再勾一遍 */}
                              {p.code.endsWith(':*') || p.code === '*' ? (
                                <Tooltip title="通配权限：包含该模块下所有具体权限，不需要再逐个勾">
                                  <span
                                    style={{
                                      marginLeft: 6,
                                      fontSize: 10.5,
                                      color: '#d46b08',
                                      background: '#fff7e6',
                                      border: '1px solid #ffe7ba',
                                      borderRadius: 3,
                                      padding: '0 4px',
                                    }}
                                  >
                                    通配
                                  </span>
                                </Tooltip>
                              ) : null}
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
