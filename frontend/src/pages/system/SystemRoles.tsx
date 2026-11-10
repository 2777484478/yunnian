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
 * 「环境操作功能」里的七项（用户 2026-11-10 拍板：环境详情服务列表里的操作逐个可勾）。
 *
 * 这些是环境详情「服务列表」的操作，按功能拆成独立权限点；它们**从常规分组里摘出来**，
 * 单独在这一个分区里勾选（对应后端 router.go 里 /services 的变更类路由）。
 * 顺序对齐服务列表：重启 / 日志 / 重新部署 / 回滚 / 设置参数 / 扩缩容 / 取消纳管。
 */
const SERVICE_FUNCS: { code: string; name: string }[] = [
  { code: 'deploy:restart', name: '重启' },
  { code: 'log:read', name: '日志' },
  { code: 'deploy:redeploy', name: '重新部署' },
  { code: 'rollback:execute', name: '回滚' },
  { code: 'deploy:params', name: '设置参数' },
  { code: 'deploy:scale', name: '扩缩容' },
  { code: 'deploy:offline', name: '取消纳管' },
];
const SERVICE_FUNC_CODES = new Set(SERVICE_FUNCS.map((f) => f.code));

// deploy: 前缀下「非环境」的后缀：通配 * / 功能点 read、approve、以及 7 项操作功能里的
// deploy 系点（restart/redeploy/params/scale/offline）。除此以外（test、test-152、
// pre-aliyun…）都算「环境类 deploy」，由「可部署环境」多选表达。
const DEPLOY_NON_ENV_SUFFIXES = new Set(['*', 'read', 'approve', 'restart', 'redeploy', 'params', 'scale', 'offline']);

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

  /**
   * 可部署环境：与「可见范围」同款多选，存 `deploy:<envKey>`（发布拓扑里展示哪些圈）。
   * 兼容老写法 deploy:test,staging → 拆成 [test, staging] 勾上。
   */
  const [deployDraft, setDeployDraft] = useState<string[]>([]);

  useEffect(() => {
    const r = roles.find((x) => x.key === active);
    if (!r || r.perms.includes('*') || r.perms.includes('deploy:*')) {
      setDeployDraft([]);
      return;
    }
    const keys: string[] = [];
    r.perms.forEach((p) => {
      if (!p.startsWith('deploy:')) return;
      p.slice('deploy:'.length).split(',').forEach((k) => {
        const t = k.trim();
        if (t && !DEPLOY_NON_ENV_SUFFIXES.has(t)) keys.push(t);
      });
    });
    setDeployDraft([...new Set(keys)]);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [active]);

  /** 合并成最终要提交的权限点数组 */
  const mergedPerms = useMemo(() => {
    // 环境类的 deploy:<env> 由 deployDraft 统一生成，先从 draft 里摘掉，避免两份写法。
    const isEnvDeploy = (p: string) =>
      p.startsWith('deploy:') && !DEPLOY_NON_ENV_SUFFIXES.has(p.slice('deploy:'.length));
    return [
      ...draft.filter((p) => !p.startsWith('env:view:') && !isEnvDeploy(p)),
      ...visibleDraft.map((k) => `env:view:${k}`),
      ...deployDraft.map((k) => `deploy:${k}`),
    ];
  }, [draft, visibleDraft, deployDraft]);

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
    // 归一化环境类 deploy：老写法 deploy:test,staging 在「可部署环境」里被拆成
    // deploy:test + deploy:staging，合并回去字符串不同但语义相同，比较前两边都拆平。
    const norm = (ps: string[]) => {
      const out = new Set<string>();
      ps.forEach((p) => {
        if (p.startsWith('deploy:') && !DEPLOY_NON_ENV_SUFFIXES.has(p.slice('deploy:'.length))) {
          p.slice('deploy:'.length).split(',').forEach((k) => {
            const t = k.trim();
            if (t) out.add('deploy:' + t);
          });
        } else {
          out.add(p);
        }
      });
      return [...out].sort().join(',');
    };
    return norm(mergedPerms) !== norm(role.perms);
  }, [mergedPerms, role, isSuper]);

  const grouped = useMemo(() => {
    const q = kw.trim().toLowerCase();
    const map = new Map<string, PermissionPoint[]>();
    points
      // 「环境操作功能」的七项由上面的功能勾选表达，不在常规分组里再出现一次。
      .filter((p) => !SERVICE_FUNC_CODES.has(p.code))
      // 环境类的 deploy:<envKey>（deploy:test / deploy:test,staging 等老写法）由
      // 「可部署环境」多选表达，也不再逐项列出。
      .filter(
        (p) =>
          !(p.code.startsWith('deploy:') && p.code !== 'deploy:read' && p.code !== 'deploy:approve' && !p.code.startsWith('deploy:*')),
      )
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
                        没配过 = 全部不可见；勾了哪些，环境列表 / 详情里就只看得到这些
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

                    <div style={{ display: 'flex', alignItems: 'center', gap: 8, margin: '14px 0 8px' }}>
                      <span style={{ width: 3, height: 12, borderRadius: 2, background: '#d46b08', opacity: 0.7 }} />
                      <span style={{ fontSize: 12.5, fontWeight: 500, color: '#1f2a44' }}>可部署环境</span>
                      <Text type="secondary" style={{ fontSize: 11.5 }}>
                        勾了才能在发布拓扑里展示哪几个圈；没勾的环境不展示、也找不到发布入口
                      </Text>
                    </div>
                    <Select
                      mode="multiple"
                      allowClear
                      showSearch
                      optionFilterProp="label"
                      style={{ width: '100%' }}
                      placeholder={isSuper || draft.includes('deploy:*') ? '当前角色可部署全部环境' : '选允许该角色部署（拓扑展示）的环境'}
                      disabled={!canWrite || isSuper || draft.includes('deploy:*')}
                      value={deployDraft}
                      onChange={(v) => setDeployDraft(v as string[])}
                      options={allEnvs.map((e) => ({ label: `${e.name}（${e.key}）`, value: e.key }))}
                      loading={envApi.loading}
                    />

                    <div style={{ display: 'flex', alignItems: 'center', gap: 8, margin: '14px 0 8px' }}>
                      <span style={{ width: 3, height: 12, borderRadius: 2, background: '#389e0d', opacity: 0.7 }} />
                      <span style={{ fontSize: 12.5, fontWeight: 500, color: '#1f2a44' }}>环境操作功能</span>
                      <Text type="secondary" style={{ fontSize: 11.5 }}>
                        勾选后，该角色可对可见环境执行对应操作
                      </Text>
                    </div>
                    <Checkbox.Group
                      value={draft.filter((c) => SERVICE_FUNC_CODES.has(c))}
                      onChange={(v) => {
                        setDraft([...draft.filter((c) => !SERVICE_FUNC_CODES.has(c)), ...(v as string[])]);
                      }}
                      disabled={!canWrite}
                      style={{ display: 'flex', flexWrap: 'wrap', gap: '8px 24px' }}
                    >
                      {SERVICE_FUNCS.map((f) => (
                        <Checkbox key={f.code} value={f.code}>
                          <span style={{ fontSize: 12.5 }}>{f.name}</span>
                          <span className="yn-mono" style={{ fontSize: 11, color: '#b0b6c2', marginLeft: 6 }}>
                            {f.code}
                          </span>
                        </Checkbox>
                      ))}
                    </Checkbox.Group>
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
                          /**
                           * ⚠️ **每个模块一个 Checkbox.Group，但共用同一个 draft**。
                           *
                           * antd 的 `onChange` 只返回**本组内**被选中的值 ——
                           * 直接 `setDraft(v)` 会把其它模块的勾选全部冲掉
                           *（实测：在「镜像」里勾一下「清理镜像」，构建/环境/日志的勾选全消失）。
                           *
                           * 所以 onChange 必须**合并**：先摘掉本组的 code，再放入本组的新选中。
                           */
                          value={draft}
                          onChange={(v) => {
                            const groupCodes = new Set(list.map((p) => p.code));
                            setDraft([...draft.filter((c) => !groupCodes.has(c)), ...(v as string[])]);
                          }}
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
