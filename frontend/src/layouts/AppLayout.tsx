import React, { useEffect, useMemo, useState } from 'react';
import {
  App as AntdApp,
  Avatar,
  Badge,
  Button,
  Dropdown,
  Empty,
  Input,
  Layout,
  Menu,
  Space,
  Spin,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import {
  SafetyOutlined,
  ApartmentOutlined,
  ApiOutlined,
  AppstoreOutlined,
  BellOutlined,
  BuildOutlined,
  CloudServerOutlined,
  ClusterOutlined,
  DashboardOutlined,
  DeploymentUnitOutlined,
  FileSearchOutlined,
  GithubOutlined,
  HddOutlined,
  MenuFoldOutlined,
  MenuUnfoldOutlined,
  PartitionOutlined,
  QuestionCircleOutlined,
  RobotOutlined,
  RollbackOutlined,
  SearchOutlined,
  SettingOutlined,
  TeamOutlined,
  ThunderboltOutlined,
  UnorderedListOutlined,
} from '@ant-design/icons';
import { Outlet, useLocation, useNavigate } from 'react-router-dom';
import { LogoCloud } from '@/components/Logo';
import ErrorBoundary from '@/components/ErrorBoundary';
import { getSession, hasAnyPerm, hasPerm, logout } from '@/auth';
import { deployApi, environmentApi, projectApi } from '@/api';
import ProfileModal from '@/components/ProfileModal';
import { useApi } from '@/hooks/useApi';
import { useEnvResources, envResStatusOf } from '@/hooks/useEnvResources';
import type { DeployRecord, Environment } from '@/types';

const { Sider, Header, Content } = Layout;
const { Text } = Typography;

// ---------------------------------------------------------------------------
// 侧边导航
//
// 每一项都挂上它对应接口真正要求的权限点——菜单可见性与后端 RequirePerm 必须一致。
// 不一致的两种表现都很难受：
//   - 菜单显示了但接口 403：用户点进去看到一屏报错；
//   - 菜单藏了但其实能读：用户以为功能不存在。
// 权限点来源见 backend/internal/api/router.go 的 registerXxx。
//
// 注意「第三方集成」是**只登录即可读**（写才需要 user:manage），
// 所以它不能跟着父分组一起被 user:manage 藏掉。
// ---------------------------------------------------------------------------

interface MenuLeaf {
  key: string;
  icon: React.ReactNode;
  label: string;
  /** 需要的权限点（任一满足即可）；为空表示登录可见 */
  perms?: string[];
}

interface MenuGroup {
  key: string;
  icon: React.ReactNode;
  label: string;
  badge?: string;
  children: MenuLeaf[];
}

const MENU: MenuGroup[] = [
  {
    key: 'overview',
    icon: <DashboardOutlined />,
    label: '概览',
    children: [{ key: '/overview', icon: <DashboardOutlined />, label: '概览' }],
  },
  {
    key: 'ci',
    icon: <BuildOutlined />,
    label: 'CI 镜像构建',
    children: [
      { key: '/ci/projects', icon: <AppstoreOutlined />, label: '项目管理', perms: ['project:read'] },
      // Jenkins Job 是独立页，不再是项目管理里的抽屉：
      // 实测那台 Jenkins 有 531 个 job，需要搜索 / 筛选 / 翻页（见 pages/ci/JenkinsJobList.tsx）。
      // 权限点用 build:read（浏览构建作业）——ops/dev/qa/readonly 都持有，与触发所需的
      // build:trigger 分开：能看列表 ≠ 能点构建。
      { key: '/ci/jenkins', icon: <UnorderedListOutlined />, label: 'Jenkins Job', perms: ['build:read'] },
      { key: '/ci/builds', icon: <ThunderboltOutlined />, label: '构建记录', perms: ['build:read'] },
      { key: '/ci/images', icon: <HddOutlined />, label: '镜像版本', perms: ['image:read'] },
    ],
  },
  {
    key: 'cd',
    icon: <CloudServerOutlined />,
    label: 'CD 环境部署',
    children: [
      {
        key: '/cd/topology',
        icon: <PartitionOutlined />,
        label: '发布拓扑',
        perms: ['deploy:read', 'env:read'],
      },
      { key: '/cd/environments', icon: <ApartmentOutlined />, label: '环境管理', perms: ['env:read'] },
      { key: '/cd/deploys', icon: <DeploymentUnitOutlined />, label: '部署记录', perms: ['deploy:read'] },
      { key: '/cd/rollbacks', icon: <RollbackOutlined />, label: '回滚记录', perms: ['rollback:read'] },
    ],
  },
  {
    key: 'ai',
    icon: <RobotOutlined />,
    label: 'AI 运维助手',
    badge: 'Beta',
    children: [{ key: '/ai', icon: <RobotOutlined />, label: 'AI 运维助手', perms: ['ai:query'] }],
  },
  {
    key: 'audit',
    icon: <FileSearchOutlined />,
    label: '操作审计',
    children: [{ key: '/audit', icon: <FileSearchOutlined />, label: '操作审计', perms: ['audit:read'] }],
  },
  {
    key: 'system',
    icon: <SettingOutlined />,
    label: '系统管理',
    children: [
      { key: '/system/users', icon: <TeamOutlined />, label: '用户', perms: ['user:manage'] },
      { key: '/system/roles', icon: <SafetyOutlined />, label: '角色权限', perms: ['user:manage'] },
      { key: '/system/notify', icon: <BellOutlined />, label: '通知渠道', perms: ['notify:manage'] },
      // 只登录即可读：不挂权限点
      { key: '/system/integrations', icon: <GithubOutlined />, label: '第三方集成' },
      // kubeconfig 属管理员级凭据，与模型设置同样按 user:manage 过滤。
      // 代价是 ops 角色看不到这一项——若要让运维也能接入集群，
      // 把 perms 换成 env:write（或新设 cluster:manage）即可，页面本身不依赖该权限点。
      { key: '/system/clusters', icon: <ClusterOutlined />, label: '集群接入', perms: ['user:manage'] },
      { key: '/system/models', icon: <ApiOutlined />, label: '模型设置', perms: ['user:manage'] },
    ],
  },
];

/** 概览与第三方集成的权限点为空 = 登录可见 */
const visible = (perms?: string[]) => !perms?.length || hasAnyPerm(...perms);

// ---------------------------------------------------------------------------

export const AppLayout: React.FC = () => {
  const navigate = useNavigate();
  const location = useLocation();
  const [collapsed, setCollapsed] = useState(false);
  const [profileOpen, setProfileOpen] = useState(false);
  // 改完资料/头像后 +1，让下面那次 getSession() 重算 —— session 不是 state，
  // 不这样推一下，头部还会显示旧姓名/旧头像。
  const [, bumpSession] = useState(0);
  const { message } = AntdApp.useApp();

  const session = getSession();
  const me = {
    name: session?.name ?? '未登录',
    roleName: session?.roleName ?? '',
    username: session?.username ?? '',
    avatar: session?.avatar ?? '',
  };

  // ------------------------------------------------------------------ 菜单

  const menu = useMemo(
    () =>
      MENU.map((g) => ({ ...g, children: g.children.filter((c) => visible(c.perms)) })).filter(
        (g) => g.children.length > 0,
      ),
    // 权限来自会话缓存，登录后不会在同一会话内变化，故只依赖一次
    [],
  );

  /** 单页分组的展示形态与多页分组不同（前者直接作为一个菜单项） */
  const menuItems = useMemo(
    () =>
      menu.map((g) =>
        g.children.length === 1 && g.key === g.children[0].key
          ? {
              key: g.children[0].key,
              icon: g.icon,
              label: g.badge ? (
                <span>
                  {g.children[0].label}
                  <Tag color="orange" style={{ marginLeft: 8, transform: 'scale(0.85)' }}>
                    {g.badge}
                  </Tag>
                </span>
              ) : (
                g.children[0].label
              ),
            }
          : {
              key: g.key,
              icon: g.icon,
              label: g.label,
              children: g.children.map((c) => ({ key: c.key, icon: c.icon, label: c.label })),
            },
      ),
    [menu],
  );

  /** 所有可见路由（用于高亮与"当前路由是否可见"的判断） */
  const visibleRoutes = useMemo(() => menu.flatMap((g) => g.children.map((c) => c.key)), [menu]);

  const selectedKeys = useMemo(() => {
    const p = location.pathname;
    // 详情页高亮到父级列表（用可见路由做前缀匹配，顺序取最长匹配更准）
    const hit = visibleRoutes
      .filter((k) => p === k || p.startsWith(`${k}/`) || (k === '/ci/projects' && p.startsWith('/ci/projects')))
      .sort((a, b) => b.length - a.length)[0];
    return hit ? [hit] : [];
  }, [location.pathname, visibleRoutes]);

  const openKeys = useMemo(() => {
    const p = location.pathname;
    return menu
      .filter((g) => g.children.length > 1 && g.children.some((c) => p.startsWith(c.key.split('/').slice(0, 2).join('/'))))
      .map((g) => g.key);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [location.pathname, menu]);

  // ------------------------------------------------------------------ 顶栏真实状态
  //
  // 顶栏原来挂的是三样写死的东西（集群连通、3 条未读通知、mock 用户）。
  // 现在全部换成真实来源；查不到就明确显示"未知"，不编一个好看的数字。

  const envsApi = useApi(() => environmentApi.list({ pageSize: 100 }), [], {
    enabled: hasPerm('env:read') || hasPerm('deploy:read'),
  });
  const envs: Environment[] = useMemo(() => envsApi.data?.list ?? [], [envsApi.data]);

  /**
   * 顶栏这一枚徽标量的是**集群当下的资源水位**，与「环境管理」「概览」共用同一份口径。
   *
   * 原先读的是 `environment.status`（= `health_status` 快照，只在"同步集群"时写一次）：
   * 那个值可能几小时前就定格了，于是出现过"顶栏挂着「N 个环境资源紧张」、点进列表页
   * 却说全部正常"。同一个判断留在两处必然分叉 —— 这里改成走 `useEnvResources`。
   *
   * `unknown` 单独一档：读不到（集群不通 / 没装 metrics-server / 还没回来）
   * 既不算紧张也不算正常，混进任何一边都是拿没读到的值下结论。
   */
  const envRes = useEnvResources(envs.map((e) => e.key));

  const envHealth = useMemo(() => {
    if (!envs.length) return null;
    const of = (key: string) => envResStatusOf(envRes.map, key);
    const bad = envs.filter((e) => of(e.key) === 'error').length;
    const warn = envs.filter((e) => of(e.key) === 'warning').length;
    const unknown = envs.filter((e) => of(e.key) === 'unknown').length;
    const prod = envs.find((e) => e.key === 'prod');
    return { total: envs.length, bad, warn, unknown, prod };
    // envRes.map 变化（实时值回来了）必须重算，否则徽标停在旧值上
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [envs, envRes.map]);

  // 待审批部署：只有有审批权的账号才请求（后端该接口就要求 deploy:approve）。
  // useApi 原生支持 options.enabled —— 传 false 时既不发首次请求，也不会进入 loading 态，
  // 因此 dev/qa/readonly 这类没有审批权的账号不会再每次进页面都打一条注定 403 的请求。
  const canApprove = hasPerm('deploy:approve');
  const pendingApi = useApi(() => deployApi.pendingApproval(), [], { enabled: canApprove });
  const pending: DeployRecord[] = canApprove ? pendingApi.data ?? [] : [];

  // ------------------------------------------------------------------ 全局搜索
  //
  // 原来这个输入框什么也不做（打了字没反应）。改成真搜索：按关键词查项目，
  // 下拉里直接进项目详情——这是运维最常用的"从名字找项目"路径。
  // 没有 project:read 的账号（如只读以外的小权限角色）直接不渲染这个框。

  const canSearch = hasPerm('project:read');
  const [kw, setKw] = useState('');
  const [debounced, setDebounced] = useState('');
  /** 用户主动点空白关掉面板后置 true，避免打字时又被强制弹开 */
  const [searchClosed, setSearchClosed] = useState(false);

  useEffect(() => {
    const t = window.setTimeout(() => setDebounced(kw.trim()), 280);
    return () => window.clearTimeout(t);
  }, [kw]);

  // 查询词变了就重新允许弹出（否则关一次之后再也打不开）
  useEffect(() => setSearchClosed(false), [debounced]);

  const searchApi = useApi(
    () => projectApi.list({ keyword: debounced, pageSize: 8 }),
    [debounced],
    { enabled: canSearch && debounced.length >= 1 },
  );
  const hits = searchApi.data?.list ?? [];
  const searchOpen = debounced.length > 0 && !searchClosed;

  const version = typeof __APP_VERSION__ === 'string' ? __APP_VERSION__ : '0.0.0';
  const builtAt = typeof __BUILD_TIME__ === 'string' ? __BUILD_TIME__ : '';

  // ------------------------------------------------------------------ 渲染

  return (
    <Layout className="yn-layout">
      <Header className="yn-header">
        <div className="yn-header__left">
          <div className="yn-logo">
            <span className="yn-logo__mark">
              <LogoCloud size={19} />
            </span>
            <span className="yn-logo__text">
              <span className="yn-logo__name">YunNian 云辇</span>
              <span className="yn-logo__sub">AI 运维管理发布平台</span>
            </span>
          </div>
          <div className="yn-header__nav">
            <Menu
              mode="horizontal"
              selectedKeys={[]}
              style={{ border: 'none', minWidth: 300, height: 55, lineHeight: '55px' }}
              items={[
                { key: 'console', label: '控制台' },
                {
                  key: 'apps',
                  label: (
                    <span className="yn-nav-soon">
                      应用中心
                      <em className="yn-nav-soon__badge">二期</em>
                    </span>
                  ),
                  onClick: () => message.info('「应用中心」已排入二期，当前版本暂未实现'),
                },
                {
                  key: 'pipeline',
                  label: (
                    <span className="yn-nav-soon">
                      流水线模板
                      <em className="yn-nav-soon__badge">二期</em>
                    </span>
                  ),
                  onClick: () => message.info('「流水线模板」已排入二期，当前版本暂未实现'),
                },
                {
                  key: 'docs',
                  label: <span className="yn-nav-soon">使用文档</span>,
                  onClick: () => message.info('使用文档将在平台功能定稿后补齐'),
                },
              ]}
            />
          </div>
        </div>

        <div className="yn-header__actions">
          {canSearch ? (
            <Dropdown
              open={searchOpen}
              onOpenChange={(o) => {
                if (!o) setSearchClosed(true);
              }}
              dropdownRender={() => (
                <div className="yn-search-pop">
                  {searchApi.loading ? (
                    <div className="yn-search-pop__empty">
                      <Spin size="small" /> <Text type="secondary">搜索中…</Text>
                    </div>
                  ) : hits.length ? (
                    hits.map((p) => (
                      <div
                        key={p.key}
                        className="yn-search-pop__item"
                        onClick={() => {
                          setSearchClosed(true);
                          setKw('');
                          navigate(`/ci/projects/${p.key}`);
                        }}
                      >
                        <span className="yn-search-pop__name">{p.name}</span>
                        <code className="yn-search-pop__key">{p.key}</code>
                        <span className="yn-search-pop__meta">{p.owner}</span>
                      </div>
                    ))
                  ) : (
                    <div className="yn-search-pop__empty">
                      <Empty
                        image={Empty.PRESENTED_IMAGE_SIMPLE}
                        description={`没有匹配「${debounced}」的项目`}
                        style={{ margin: 8 }}
                      />
                    </div>
                  )}
                  <div className="yn-search-pop__foot">
                    按项目名称 / 标识 / 负责人搜索，回车进入项目列表
                  </div>
                </div>
              )}
            >
              <Input
                allowClear
                prefix={<SearchOutlined style={{ color: '#b5bac4' }} />}
                placeholder="搜索项目 / 负责人"
                style={{ width: 240, marginRight: 12, background: '#f7f8fa' }}
                variant="filled"
                value={kw}
                onChange={(e) => setKw(e.target.value)}
                onPressEnter={() => {
                  if (!kw.trim()) return;
                  setSearchClosed(true);
                  navigate(`/ci/projects?keyword=${encodeURIComponent(kw.trim())}`);
                }}
              />
            </Dropdown>
          ) : null}

          <Tooltip
            title={
              envHealth
                ? `${envHealth.total} 个环境：正常 ${envHealth.total - envHealth.bad - envHealth.warn - envHealth.unknown} · 资源告警 ${envHealth.warn} · 资源紧张 ${envHealth.bad}${envHealth.unknown ? ` · 读取不到 ${envHealth.unknown}` : ''}`
                : '环境状态读取中或当前账号无环境查看权限'
            }
          >
            <span className="yn-header__env-tag">
              {envHealth ? (
                <>
                  <Badge
                    status={
                      envHealth.bad
                        ? 'error'
                        : envHealth.warn
                          ? 'warning'
                          : envHealth.unknown
                            ? 'default'
                            : 'success'
                    }
                  />
                  {/* 这里量的是**集群资源水位**（见 StatusTag.tsx 的 ENV_MAP），
                      所以措辞说资源。原来叫「N 个环境异常」时，顶栏挂一个红点会让人
                      以为有环境出故障了 —— 实际可能只是某台机器内存踩了 90% 的线。 */}
                  {envHealth.bad
                    ? `${envHealth.bad} 个环境资源紧张`
                    : envHealth.warn
                      ? `${envHealth.warn} 个环境资源告警`
                      : envHealth.unknown
                        ? // 读不到**不许**说"全部正常" —— 那是拿没读到的值下结论
                          `${envHealth.unknown} 个环境读取不到`
                        : '全部环境正常'}
                  {envHealth.prod ? ` · ${envHealth.prod.cluster}` : ''}
                </>
              ) : (
                <>
                  <Badge status="default" />
                  环境状态未知
                </>
              )}
            </span>
          </Tooltip>

          {canApprove ? (
            <Dropdown
              trigger={['click']}
              dropdownRender={() => (
                <div className="yn-search-pop" style={{ width: 320 }}>
                  <div className="yn-search-pop__title">待审批部署单</div>
                  {pending.length ? (
                    pending.slice(0, 5).map((d) => (
                      <div
                        key={d.id}
                        className="yn-search-pop__item"
                        onClick={() => navigate('/cd/deploys')}
                      >
                        <span className="yn-search-pop__name">{d.projectKey}</span>
                        <span className="yn-search-pop__meta">
                          {d.envName} · {d.deployNo}
                        </span>
                      </div>
                    ))
                  ) : (
                    <div className="yn-search-pop__empty">没有待审批的部署单</div>
                  )}
                  <div
                    className="yn-search-pop__foot yn-search-pop__foot--link"
                    onClick={() => navigate('/cd/deploys')}
                  >
                    前往部署记录审批 →
                  </div>
                </div>
              )}
            >
              <Tooltip title={pending.length ? `${pending.length} 条待审批部署` : '没有待审批项'}>
                <Badge count={pending.length} size="small" offset={[-2, 4]}>
                  <Button type="text" icon={<BellOutlined style={{ fontSize: 16 }} />} />
                </Badge>
              </Tooltip>
            </Dropdown>
          ) : (
            // 一期没有站内消息中心：构建/部署事件是通过「通知渠道」推到钉钉/企微/邮件的。
            // 与其摆一个永远显示假数字的铃铛，不如把这件事说清楚。
            <Tooltip title="一期没有站内消息中心：构建与部署事件通过「系统管理 › 通知渠道」推送到钉钉 / 企微 / 邮件">
              <Button type="text" icon={<BellOutlined style={{ fontSize: 16, color: '#c2c7d0' }} />} />
            </Tooltip>
          )}

          <Tooltip title="帮助文档（待补齐）">
            <Button
              type="text"
              icon={<QuestionCircleOutlined style={{ fontSize: 16 }} />}
              onClick={() => message.info('使用文档将在平台功能定稿后补齐')}
            />
          </Tooltip>

          <Dropdown
            menu={{
              items: [
                { key: 'profile', label: '个人设置' },
                { key: 'token', label: '访问令牌（二期）' },
                { type: 'divider' },
                {
                  key: 'perms',
                  label: (
                    <span style={{ fontSize: 12, color: '#9aa0aa' }}>
                      当前身份权限 {session?.permissions?.length ?? 0} 项
                    </span>
                  ),
                  disabled: true,
                },
                { key: 'logout', label: '退出登录', danger: true },
              ],
              onClick: ({ key }) => {
                if (key === 'logout') {
                  // 必须调后端登出：JWT 本身无法作废，是服务端会话键让它失效的。
                  // 只清本地会让旧令牌在有效期内继续可用（拿它还能调接口）。
                  logout()
                    .catch(() => {
                      /* 后端不可达也要放行，不能把用户卡在登录态 */
                    })
                    .finally(() => {
                      message.success('已安全退出登录');
                      navigate('/login', { replace: true });
                    });
                  return;
                }
                if (key === 'profile') {
                  setProfileOpen(true);
                  return;
                }
                message.info('该功能排入二期，当前版本暂未实现');
              },
            }}
          >
            <Space size={8} style={{ cursor: 'pointer', marginLeft: 8 }}>
              <Avatar size={30} src={me.avatar || undefined} style={{ background: '#1677ff', fontSize: 13 }}>
                {me.name.slice(0, 1)}
              </Avatar>
              <span style={{ lineHeight: 1.2 }}>
                <div style={{ fontSize: 13, color: '#2c313a' }}>{me.name}</div>
                <div style={{ fontSize: 11, color: '#9aa0aa' }}>
                  {me.roleName || me.username}
                </div>
              </span>
            </Space>
          </Dropdown>
        </div>
      </Header>

      <ProfileModal
        open={profileOpen}
        onClose={() => setProfileOpen(false)}
        onSaved={() => bumpSession((n) => n + 1)}
      />

      <Layout>
        <Sider
          className="yn-sider"
          width={224}
          collapsedWidth={56}
          collapsed={collapsed}
          theme="light"
        >
          <div className="yn-sider__group" style={{ display: collapsed ? 'none' : 'block' }}>
            我的工作台
          </div>
          <Menu
            mode="inline"
            selectedKeys={selectedKeys}
            defaultOpenKeys={openKeys}
            items={menuItems}
            onClick={({ key }) => {
              if (String(key).startsWith('/')) navigate(key);
            }}
          />
          <div style={{ padding: '16px 16px 0', display: collapsed ? 'none' : 'block' }}>
            <Button
              size="small"
              type="text"
              icon={collapsed ? <MenuUnfoldOutlined /> : <MenuFoldOutlined />}
              onClick={() => setCollapsed((c) => !c)}
              style={{ color: '#9aa0aa', paddingLeft: 4 }}
            >
              收起导航
            </Button>
            <div style={{ marginTop: 14, fontSize: 11, color: '#c2c7d0' }}>
              <div className="yn-mono">v{version}{builtAt ? ` · ${builtAt}` : ''}</div>
              <div style={{ marginTop: 2 }}>© 2026 YunNian 云辇 运维平台</div>
            </div>
          </div>
          {collapsed ? (
            <div style={{ textAlign: 'center', marginTop: 4 }}>
              <Button
                size="small"
                type="text"
                icon={<MenuUnfoldOutlined />}
                onClick={() => setCollapsed(false)}
              />
            </div>
          ) : null}
        </Sider>

        <Content className="yn-content">
          {/* 页面级兜底：某个页面渲染崩了，只牺牲内容区，侧边栏与顶栏留着 ——
              用户能点别处走出去，而不是对着整屏白页无从下手。
              resetKey 挂路由：错误状态是粘住的，不带 key 的话切页也还是那一屏报错。 */}
          <ErrorBoundary resetKey={location.pathname} scope="该页面">
            <Outlet />
          </ErrorBoundary>
        </Content>
      </Layout>
    </Layout>
  );
};

export default AppLayout;
export { Text };
