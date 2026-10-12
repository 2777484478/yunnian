import React, { Suspense, lazy } from 'react';
import { HashRouter, Navigate, Route, Routes, useLocation } from 'react-router-dom';
import { Button, Result, Spin } from 'antd';
import AppLayout from '@/layouts/AppLayout';
import ErrorBoundary from '@/components/ErrorBoundary';
import { hasAnyPerm, isLoggedIn } from '@/auth';

// 路由懒加载（原型阶段直接打包成一个文件，懒加载仅做结构示范）
const Login = lazy(() => import('@/pages/Login'));
const Overview = lazy(() => import('@/pages/Overview'));
const ProjectList = lazy(() => import('@/pages/ci/ProjectList'));
const ProjectDetail = lazy(() => import('@/pages/ci/ProjectDetail'));
const BuildList = lazy(() => import('@/pages/ci/BuildList'));
const JenkinsJobList = lazy(() => import('@/pages/ci/JenkinsJobList'));
const ImageList = lazy(() => import('@/pages/ci/ImageList'));
const EnvironmentList = lazy(() => import('@/pages/cd/EnvironmentList'));
const Topology = lazy(() => import('@/pages/cd/Topology'));
const EnvironmentDetail = lazy(() => import('@/pages/cd/EnvironmentDetail'));
const DeployList = lazy(() => import('@/pages/cd/DeployList'));
const RollbackList = lazy(() => import('@/pages/cd/RollbackList'));
const AiAssistant = lazy(() => import('@/pages/AiAssistant'));
const AuditLogPage = lazy(() => import('@/pages/AuditLogPage'));
const SystemUsers = lazy(() => import('@/pages/system/SystemUsers'));
const SystemNotify = lazy(() => import('@/pages/system/SystemNotify'));
const SystemIntegrations = lazy(() => import('@/pages/system/SystemIntegrations'));
const SystemModels = lazy(() => import('@/pages/system/SystemModels'));
const ClusterAccess = lazy(() => import('@/pages/system/ClusterAccess'));

const Loading = () => (
  <div style={{ display: 'grid', placeItems: 'center', minHeight: 320 }}>
    <Spin tip="加载中…" size="large" />
  </div>
);

/**
 * 路由守卫：未登录一律回登录页。
 * 带上原目标地址，登录成功后跳回用户本来想去的页面，避免每次都落回概览。
 */
const RequireAuth: React.FC<{ children: React.ReactElement }> = ({ children }) => {
  const location = useLocation();
  if (!isLoggedIn()) {
    return <Navigate to="/login" replace state={{ from: `${location.pathname}${location.search}` }} />;
  }
  return children;
};

/**
 * 页面级权限守卫。
 *
 * 侧边栏已经把没权限的入口藏掉了，但地址栏、收藏夹、别人发来的链接都还能直达。
 * 没有这层守卫的话，用户会落进一个「页面渲染出来了、每个接口都 403」的空壳——
 * 界面上一堆红色报错，却看不出真正的原因是"你本来就不该看到这一页"。
 * 所以这里直接拦住，并说清楚缺哪个权限点。
 */
const RequirePerm: React.FC<{ codes: string[]; children: React.ReactElement }> = ({
  codes,
  children,
}) =>
  hasAnyPerm(...codes) ? (
    children
  ) : (
    <Result
      status="403"
      title="没有访问权限"
      subTitle={`当前账号缺少该页面所需的权限点：${codes.join(' 或 ')}。如需开通请联系系统管理员在「系统管理 › 用户与权限」中授予。`}
      extra={
        <Button type="primary" onClick={() => window.location.assign('#/overview')}>
          返回概览
        </Button>
      }
    />
  );

/**
 * 全局兜底边界。
 *
 * 页面级的那一层在 AppLayout 里（保住侧边栏与顶栏）；这一层是最后一道，
 * 兜住「连布局都没渲染出来」的情况（比如 Login 页、或 AppLayout 自身出错）。
 * resetKey 挂当前路由：错误状态是粘住的，不带 key 的话用户切页也还是那一屏报错。
 */
const RoutedApp: React.FC = () => {
  const location = useLocation();
  return (
    <ErrorBoundary resetKey={location.pathname} fullscreen>
      <Suspense fallback={<Loading />}>
        <Routes>
          <Route path="/login" element={<Login />} />

          <Route
            path="/"
            element={
              <RequireAuth>
                <AppLayout />
              </RequireAuth>
            }
          >
            <Route index element={<Navigate to="/overview" replace />} />
            <Route path="overview" element={<Overview />} />

            <Route path="ci/projects" element={<RequirePerm codes={['project:read']}><ProjectList /></RequirePerm>} />
            <Route path="ci/projects/:key" element={<RequirePerm codes={['project:read']}><ProjectDetail /></RequirePerm>} />
            <Route path="ci/jenkins" element={<RequirePerm codes={['build:read']}><JenkinsJobList /></RequirePerm>} />
            <Route path="ci/builds" element={<RequirePerm codes={['build:read']}><BuildList /></RequirePerm>} />
            <Route path="ci/images" element={<RequirePerm codes={['image:read']}><ImageList /></RequirePerm>} />

            <Route path="cd/topology" element={<RequirePerm codes={['deploy:read', 'env:read']}><Topology /></RequirePerm>} />
            <Route path="cd/environments" element={<RequirePerm codes={['env:read']}><EnvironmentList /></RequirePerm>} />
            <Route path="cd/environments/:key" element={<RequirePerm codes={['env:read']}><EnvironmentDetail /></RequirePerm>} />
            <Route path="cd/deploys" element={<RequirePerm codes={['deploy:read']}><DeployList /></RequirePerm>} />
            <Route path="cd/rollbacks" element={<RequirePerm codes={['rollback:read']}><RollbackList /></RequirePerm>} />

            <Route path="ai" element={<RequirePerm codes={['ai:query']}><AiAssistant /></RequirePerm>} />
            <Route path="audit" element={<RequirePerm codes={['audit:read']}><AuditLogPage /></RequirePerm>} />

            <Route path="system/users" element={<RequirePerm codes={['user:manage']}><SystemUsers /></RequirePerm>} />
            <Route path="system/notify" element={<RequirePerm codes={['notify:manage']}><SystemNotify /></RequirePerm>} />
            {/* 第三方集成只登录即可读（写操作才需要 user:manage），不加权限守卫 */}
            <Route path="system/integrations" element={<SystemIntegrations />} />
            {/* 集群接入：kubeconfig 是管理员级凭据，与模型设置同口径按 user:manage 守卫 */}
            <Route path="system/clusters" element={<RequirePerm codes={['user:manage']}><ClusterAccess /></RequirePerm>} />
            <Route path="system/models" element={<RequirePerm codes={['user:manage']}><SystemModels /></RequirePerm>} />

            <Route
              path="*"
              element={<Result status="404" title="页面不存在" subTitle="请检查地址是否正确，或从左侧导航进入。" />}
            />
          </Route>
        </Routes>
      </Suspense>
    </ErrorBoundary>
  );
};

const App: React.FC = () => (
  <HashRouter>
    <RoutedApp />
  </HashRouter>
);

export default App;
