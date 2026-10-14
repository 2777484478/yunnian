import React, { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Button,
  Col,
  Descriptions,
  Divider,
  Drawer,
  Dropdown,
  Empty,
  Form,
  Input,
  InputNumber,
  Modal,
  Progress,
  Radio,
  Row,
  Select,
  Space,
  Steps,
  Table,
  Tabs,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  AppstoreAddOutlined,
  CloudDownloadOutlined,
  ContainerOutlined,
  DeleteOutlined,
  DownOutlined,
  ExpandOutlined,
  FileTextOutlined,
  HistoryOutlined,
  MoreOutlined,
  ReloadOutlined,
  RollbackOutlined,
  SafetyCertificateOutlined,
  SettingOutlined,
  SwapOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons';
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom';
import { PageHeader, SectionCard, KVList, StatCard } from '@/components/ui';
import { UsageBar } from '@/components/charts';
import { LogPanel } from '@/components/LogPanel';
import { KVFields } from '@/components/KVFields';
// antd 的 Tooltip 对 disabled 子元素不生效（disabled 的 button 不派发鼠标事件），
// 所有"灰着的按钮"的说明一律经 DisabledTip 下发。
import { DisabledTip } from '@/components/DisabledTip';
// 环境级动作（编辑 / 删除）与环境表单：与列表页**共用同一份实现**。
// 本页原先两样都没有 —— 从卡片"进入"之后反而什么都改不了。
import { EnvironmentActions } from '@/components/EnvironmentActions';
import { EnvironmentEditDrawer } from '@/components/EnvironmentEditDrawer';
import {
  DeployStatusTag,
  EnvStatusBadge,
  ImageText,
  MethodTag,
  ServiceStatusTag,
} from '@/components/StatusTag';
import { ApiError, deployApi, environmentApi, imageApi, rollbackApi, serviceApi } from '@/api';
import type { DeployCreated } from '@/api';
import { envResStatus, envUsage } from '@/hooks/useEnvResources';
import type { EnvProjectCandidate } from '@/api/cd';
import { canDeployOn, canRollbackOn, hasPerm } from '@/auth';
import AutoRefreshButton from '@/components/AutoRefreshButton';
import { useApi } from '@/hooks/useApi';
import { useAutoRefresh, useAutoRefreshPref } from '@/hooks/useAutoRefresh';
import { useLogStream } from '@/hooks/useLogStream';
import { humanDuration, pad2 } from '@/utils/format';
import type {
  DeployRecord,
  DeployVersion,
  EnvService,
  EnvVar,
  ImageRecord,
  RollbackRecord,
  ServicePod,
} from '@/types';
import {
  OWN_CLUSTER_BADGE,
  OWN_CLUSTER_HINT,
  isOwnCluster,
} from '@/config/topology';
import {
  isServiceAbnormal,
  liveFallbackText,
  liveHealthy,
  liveReplicaTotal,
  liveState,
  liveStatusWord,
  liveWordOf,
} from '@/utils/live';

const { Text } = Typography;

/** 集群状态不可用时的占位渲染（统一字号与颜色，避免每列各写一套）。 */
const LiveFallback: React.FC<{ s: EnvService }> = ({ s }) => (
  <span style={{ fontSize: 12, color: '#9aa0aa' }}>{liveFallbackText(s)}</span>
);

const envTagColor = (envKey: string) =>
  envKey === 'prod' ? 'red' : envKey === 'staging' ? 'orange' : envKey === 'canary' ? 'purple' : 'blue';

/** Pod 状态 → 标签色。只有 `Running` 算正常；`Pending` 是"还在等"（未必是坏），其余按异常处理。 */
const podStatusColor = (s: string) =>
  s === 'Running' ? 'success' : s === 'Pending' ? 'gold' : 'error';

/**
 * 服务下的 Pod 明细（展开行按需拉取）。
 *
 * **为什么必须逐个列出来**：服务表格上那个 `1/1 Ready` 是 **Deployment 的聚合值** ——
 * 老版本的 Pod 还在健康地跑，就把就绪数撑满了，于是"新 Pod 起不来"这件事在列表上
 * 完全看不见。实测 demobasic 正是这一种：老 Pod `Running` 让 Deployment 报
 * `readyReplicas=1`（表格显示 1/1），而新 Pod 已经 CrashLoopBackOff 崩了 8 次。
 * 界面上"一切正常"，实际正在发布失败 —— 排查问题要的是每个 Pod 的状态，
 * 不是聚合后的一个数字。
 *
 * **按需拉取**：后端刻意不在环境列表接口里带 Pod（`cd.go`：那会把一次列表请求
 * 变成 3N 次下游调用），所以这里跟着展开行挂载/卸载 —— 展开才请求。
 */
const ServicePodsInline: React.FC<{ serviceId: string; onOpenLogs: () => void }> = ({
  serviceId,
  onOpenLogs,
}) => {
  const api = useApi(() => serviceApi.pods(serviceId), [serviceId]);
  const pods: ServicePod[] = useMemo(() => api.data ?? [], [api.data]);

  return (
    <>
      <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
        <span className="yn-sub-title" style={{ margin: 0 }}>
          Pod 实例{pods.length ? `（${pods.length}）` : ''}
        </span>
        <Tooltip title="重新读取集群里这个服务的 Pod">
          <Button type="text" size="small" icon={<ReloadOutlined />} loading={api.loading} onClick={api.refresh} />
        </Tooltip>
      </div>
      {api.error ? (
        <Text type="secondary" style={{ fontSize: 12 }}>
          Pod 列表读取失败：{api.error.message}
        </Text>
      ) : pods.length ? (
        <Space size={[6, 6]} wrap style={{ marginTop: 4 }}>
          {pods.map((p) => (
            <Tag key={p.name} color={podStatusColor(p.status)} style={{ marginInlineEnd: 0 }}>
              <span className="yn-mono">{p.name}</span> · {p.status} · 重启 {p.restarts} · {p.age}
            </Tag>
          ))}
        </Space>
      ) : (
        <Text type="secondary" style={{ fontSize: 12 }}>
          {api.loading ? '读取中…' : '该服务当前没有 Pod'}
        </Text>
      )}
      <div style={{ marginTop: 8 }}>
        <Button type="link" size="small" style={{ paddingInline: 0 }} onClick={onOpenLogs}>
          查看 Pod 日志
        </Button>
      </div>
    </>
  );
};

const EnvironmentDetail: React.FC = () => {
  const { key = '' } = useParams();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const { message, modal, notification } = AntApp.useApp();

  /**
   * 把后端的拒绝理由如实展示给用户。
   *
   * client.ts 抛的是 ApiError（带后端 message），但**它自己不弹任何提示** ——
   * 不 catch 就是一个未处理的 Promise rejection：按钮转完圈，弹窗要么留着要么关掉，
   * 用户一个字都看不到。而这些变更接口的拒绝理由都极其具体
   * （工作负载由 HPA 伸缩 / 该服务正在部署中 / 权限不足 / 集群里没有这个工作负载），
   * 丢掉它们等于让用户反复重试同一个必然失败的操作。
   */
  const fail = (e: unknown, fallback: string) =>
    message.error(e instanceof ApiError ? e.message : fallback);

  const canWrite = hasPerm('env:write');
  // 判据与后端 guard 逐字对应：
  // services 下的变更接口（redeploy / scale / restart / params / delete）路由层只要求登录，
  // 真正的权限在 handler 里 guardServiceEnv(c,"deploy",id) → 需要 deploy:{env}，否则 40300。
  const canDeploy = canDeployOn(key);
  // 回滚是两把锁：路由要求 rollback:execute，handler 再要求 deploy:{env}（见 canRollbackOn）。
  const canRollback = canRollbackOn(key);
  // 「问 AI 助手」进的是 /ai 页面，路由守卫要求 ai:query；演示账号 ops 没有这个权限点，
  // 不门控就会落到 403 空壳页。
  const canAskAI = hasPerm('ai:query');

  // 环境级编辑抽屉。表单本体在 `EnvironmentEditDrawer`（与列表页共用），
  // 本页只负责开/关 —— 页面里所有能改环境的地方都走它，不再各写一份字段。
  const [envEditOpen, setEnvEditOpen] = useState(false);

  // ------------------------------------------------------------------ 取数
  const envApi = useApi(() => environmentApi.get(key), [key], { enabled: !!key });
  const env = envApi.data;

  // 编辑抽屉的"存量集群标识"候选：本页只有这一个环境，把它的当前值带上，
  // 否则编辑时那个值不在候选里（集群实体化之前手输的标识不在集群表内）。
  const clusterValuesInUse = useMemo(() => (env?.cluster ? [env.cluster] : []), [env?.cluster]);

  // /environments/{key}/services 本身就带着该环境的全部服务，不需要再逐个拉 Pod。
  // Pod 明细在抽屉里按需取（/services/{id}/pods），避免列表页 N×M 次请求。
  const servicesApi = useApi(() => environmentApi.services(key), [key], { enabled: !!key });
  const services = useMemo(() => servicesApi.data ?? [], [servicesApi.data]);

  const deploysApi = useApi(() => deployApi.list({ envKey: key, pageSize: 50 }), [key], { enabled: !!key });
  const envDeploys = useMemo(() => deploysApi.data?.list ?? [], [deploysApi.data]);

  const rollbacksApi = useApi(() => rollbackApi.list({ envKey: key, pageSize: 50 }), [key], { enabled: !!key });
  const envRollbacks = useMemo(() => rollbacksApi.data?.list ?? [], [rollbacksApi.data]);

  // 环境对象里的 cpuUsage/memUsage 是**上次同步时的快照**；resources 接口读的是实时值，
  // 且额外给出集群 K8s 版本。本页一切与"资源水位"有关的东西都只认 `usage`。
  const resApi = useApi(() => environmentApi.resources(key), [key], { enabled: !!key });
  const res = resApi.data;
  /** 规范化后的实时用量：`metricsAvailable === false`（没装 metrics-server / 无权读）时为 null。 */
  const usage = envUsage(res);

  const refreshAll = () => {
    envApi.refresh();
    servicesApi.refresh();
    deploysApi.refresh();
    rollbacksApi.refresh();
    resApi.refresh();
  };

  /**
   * 自动刷新（默认每 5 秒）。
   *
   * 这一页要看的"状态"几乎全是**别处**在改的：镜像版本由部署执行器推、副本数与
   * 就绪数由 K8s 自己收敛、集群资源实时值每刻都在动。页面自己不会变 —— 不自动刷新，
   * 用户就只能盯着一份进页面时的快照，得手动按 F5 才知道那个部署是不是已经起来了。
   *
   * 走 refreshSilent 而不是 refresh：每 5 秒闪一次加载遮罩比不刷新更难受；而且后台
   * 那一拍失败就把整页打成错误态，等于"网络抖一下，整页没了"—— 而手上那份数据其实
   * 还是对的。静默那拍失败只在刷新控件里留一句原因，下一次成功自动清掉。
   *
   * 页面切到后台会暂停、切回来立刻补一次（见 useAutoRefresh）。
   *
   * 默认 5 秒而不是项目列表那个 3 秒：这里的 services / resources 都是**真去读一次
   * 集群**的接口，比纯库查询贵；5 秒对"看部署有没有起来"已经足够快。用户可在控件里调。
   */
  // scope='cd'：与发布拓扑共用一个键（同属 CD 视图，节奏偏好一致），
  // 但**不共用** CI 项目列表那个全局键——那边关掉不该连带把这里关掉。
  const [autoRefreshMs, setAutoRefreshMs] = useAutoRefreshPref(5000, 'cd');
  useAutoRefresh({
    enabled: !!key && autoRefreshMs !== null,
    intervalMs: autoRefreshMs ?? 5000,
    onTick: () => {
      envApi.refreshSilent();
      servicesApi.refreshSilent();
      deploysApi.refreshSilent();
      rollbacksApi.refreshSilent();
      resApi.refreshSilent();
    },
    // 任一请求还在飞就跳过这一拍，避免慢后端下滚出一串在飞请求
    isBusy: () =>
      envApi.pendingRef.current ||
      servicesApi.pendingRef.current ||
      deploysApi.pendingRef.current ||
      rollbacksApi.pendingRef.current ||
      resApi.pendingRef.current,
  });

  // ---------------------------------------------------------------- 纳管项目
  // 环境↔项目的关联（environment_project）与服务实例（service_instance）是**部署的前置**：
  // POST /deployments 第一步就查实例，查不到直接 404「该环境尚未纳管此项目」。
  // 但原先唯一的入口埋在列表页「编辑」抽屉最深处，而列表页默认又是卡片视图（卡片上没有编辑）
  // —— 用户建完环境后根本走不到这一步，反映"哪里有编辑？我没看到啊"。
  // 所以这里给一个显眼的直达入口；拓扑上未纳管的圆圈也跳到这里（带 ?link=1 直接弹开）。
  const [linkOpen, setLinkOpen] = useState(false);
  const [linkKeys, setLinkKeys] = useState<string[]>([]);
  const [linkSaving, setLinkSaving] = useState(false);

  const candApi = useApi(() => environmentApi.projectCandidates(key), [key], {
    enabled: !!key && linkOpen,
  });
  const candidates: EnvProjectCandidate[] = useMemo(() => candApi.data ?? [], [candApi.data]);

  const openLink = (keys: string[]) => {
    setLinkKeys(keys);
    setLinkOpen(true);
  };

  // 从发布拓扑的「去纳管」跳过来时直接弹开。用完把参数清掉，
  // 否则用户每次刷新这个页面都会被这个弹窗拦一次。
  useEffect(() => {
    if (!env || searchParams.get('link') !== '1') return;
    openLink(env.projectKeys);
    const next = new URLSearchParams(searchParams);
    next.delete('link');
    setSearchParams(next, { replace: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [env]);

  // 取消勾选会连带删掉服务实例（后端 replaceEnvProjects 就是这么做的）。
  // 已经跑着负载的项目先问一句，别让一次"顺手取消"把平台跟踪关系抹掉。
  const unlinkedRunning = useMemo(
    () =>
      (env?.projectKeys ?? []).filter(
        (k) => !linkKeys.includes(k) && services.some((s) => s.projectKey === k),
      ),
    [env, linkKeys, services],
  );

  const submitLink = () => {
    const doSave = async () => {
      setLinkSaving(true);
      try {
        await environmentApi.setProjects(key, linkKeys);
        message.success(`已更新「${env?.name ?? key}」的纳管项目`);
        setLinkOpen(false);
        refreshAll();
      } finally {
        setLinkSaving(false);
      }
    };
    if (!unlinkedRunning.length) {
      void doSave();
      return;
    }
    modal.confirm({
      title: '确认解除这些项目的纳管？',
      content: (
        <div style={{ fontSize: 13, lineHeight: 1.9 }}>
          以下项目在本环境已有服务实例，取消纳管会<b>删除平台里的服务实例记录</b>：
          <div className="yn-mono" style={{ marginTop: 6 }}>{unlinkedRunning.join('、')}</div>
          <div style={{ color: '#ff4d4f', marginTop: 6 }}>
            集群里已运行的负载不会自动下线，但平台不再跟踪它们（部署历史仍保留）。
            之后要重新纳管才能再发布。
          </div>
        </div>
      ),
      okText: '确认解除',
      okButtonProps: { danger: true },
      onOk: doSave,
    });
  };

  // ---------------------------------------------------------------- 部署向导
  const [deployOpen, setDeployOpen] = useState(false);
  const [deployStep, setDeployStep] = useState(0);
  const [deployTarget, setDeployTarget] = useState<EnvService | null>(null);
  const [deployForm] = Form.useForm();
  const [submitting, setSubmitting] = useState(false);
  // 注意类型是 DeployCreated（后端创建接口只回单号/镜像/状态/日志流），
  // 不是完整 DeployRecord —— 这里只需要 id / deployNo / status 三样。
  const [createdDeploy, setCreatedDeploy] = useState<DeployCreated | null>(null);

  // 被选中的项目名（用于按需拉它的镜像列表）
  const [deployProjectKey, setDeployProjectKey] = useState<string | undefined>();
  const imagesApi = useApi(
    () => imageApi.list({ projectKey: deployProjectKey!, pageSize: 50 }),
    [deployProjectKey],
    { enabled: !!deployProjectKey },
  );
  const images = useMemo(() => imagesApi.data?.list ?? [], [imagesApi.data]);

  // 执行中的实时日志：只有拿到部署单 id 后才连
  const deployStream = useLogStream(createdDeploy ? deployApi.stream(createdDeploy.id) : null, {
    enabled: !!createdDeploy,
  });

  // 流结束说明部署已进终态 —— 刷新列表与资源，让表格里的状态标签跟上
  useEffect(() => {
    if (!deployStream.ended) return;
    refreshAll();
    if (createdDeploy) {
      if (deployStream.endReason === 'finished') {
        notification.success({ message: '部署完成', description: `${createdDeploy.deployNo} 已执行结束。` });
      } else {
        // canceled / timeout 都不是成功，不要统一报成功
        notification.warning({
          message: `部署未成功（${deployStream.endReason}）`,
          description: `${createdDeploy.deployNo} 已结束，请在部署历史中查看详情。`,
        });
      }
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [deployStream.ended]);

  const openDeploy = (svc?: EnvService) => {
    setDeployTarget(svc ?? services[0] ?? null);
    setDeployStep(0);
    setCreatedDeploy(null);
    setDeployProjectKey(svc?.projectKey);
    deployForm.resetFields();
    deployForm.setFieldsValue({
      projectKey: svc?.projectKey,
      replicas: svc?.replicas ?? 1,
      strategy: 'RollingUpdate',
      remark: '',
    });
    setDeployOpen(true);
  };

  const submitDeploy = async () => {
    const v = await deployForm.validateFields();
    if (!v.imageRecordId) {
      message.warning('请选择要部署的镜像版本');
      return;
    }
    setSubmitting(true);
    try {
      const created = await deployApi.create({
        envKey: key,
        projectKey: v.projectKey,
        imageRecordId: v.imageRecordId,
        replicas: v.replicas,
        strategy: v.strategy,
        remark: v.remark,
        // 幂等键：同一次确认只能落一个部署单，重复点击不会重复发布
        idempotencyKey: `ui-${key}-${v.projectKey}-${v.imageRecordId}-${Date.now()}`,
      });
      setCreatedDeploy(created);
      setDeployStep(3);
      notification.info({ message: '部署单已提交', description: `${created.deployNo} 正在执行，日志实时输出中。` });
      refreshAll();
    } finally {
      setSubmitting(false);
    }
  };

  // ---------------------------------------------------------------- 日志抽屉
  const [logOpen, setLogOpen] = useState(false);
  const [logService, setLogService] = useState<EnvService | null>(null);
  const [logPod, setLogPod] = useState<string>('');
  const [logTail, setLogTail] = useState(500);

  const podsApi = useApi(() => serviceApi.pods(logService!.id), [logService?.id], { enabled: !!logService });
  const pods: ServicePod[] = useMemo(() => podsApi.data ?? [], [podsApi.data]);

  // Pod 列表回来后默认选第一个（"全部 Pod"是原型期的假选项——后端是按 pod 名拉日志的，
  // 没有"聚合多个 Pod"的能力，留着它只会让用户以为选"全部"能看全）。
  useEffect(() => {
    if (logOpen && !logPod && pods.length) setLogPod(pods[0].name);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pods, logOpen]);

  const podLogUrl = logService && logPod ? serviceApi.podLogStream(logService.id, logPod, logTail) : null;
  const podLogs = useLogStream(podLogUrl, { enabled: !!podLogUrl });

  const openLogs = (svc: EnvService) => {
    setLogService(svc);
    setLogPod('');
    setLogTail(500);
    setLogOpen(true);
  };

  // ---------------------------------------------------------------- 回滚
  const [rollbackOpen, setRollbackOpen] = useState(false);
  const [rollbackTarget, setRollbackTarget] = useState<EnvService | null>(null);
  const [rollbackTo, setRollbackTo] = useState<string>('');
  const [rollbackReason, setRollbackReason] = useState('');
  const [rollbackRunning, setRollbackRunning] = useState(false);

  const versionsApi = useApi(
    () => serviceApi.versions(rollbackTarget!.id),
    [rollbackTarget?.id],
    { enabled: !!rollbackTarget },
  );
  const versions: DeployVersion[] = useMemo(() => versionsApi.data ?? [], [versionsApi.data]);

  const openRollback = (svc: EnvService) => {
    setRollbackTarget(svc);
    setRollbackTo('');
    setRollbackReason('');
    setRollbackOpen(true);
  };

  // 版本列表回来后备选第一个「与当前不同」的版本作为回滚目标——
  // 回滚到当前版本是空操作，默认选中它会让用户白点一次。
  useEffect(() => {
    if (!rollbackOpen || rollbackTo || !versions.length || !rollbackTarget) return;
    const first = versions.find((v) => v.image !== rollbackTarget.image) ?? versions[0];
    setRollbackTo(first.image);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [versions, rollbackOpen, rollbackTarget]);

  const submitRollback = async () => {
    if (!rollbackTarget) return;
    if (!rollbackTo.trim()) {
      message.warning('请选择回滚目标版本');
      return;
    }
    if (!rollbackReason.trim()) {
      message.warning('回滚原因必填，它会写入审计日志');
      return;
    }
    setRollbackRunning(true);
    try {
      await rollbackApi.create({
        serviceId: rollbackTarget.id,
        toImage: rollbackTo,
        reason: rollbackReason.trim(),
        triggerType: 'manual',
      });
      notification.success({
        message: '回滚任务已提交',
        description: `正在将 ${rollbackTarget.projectKey} 回滚至 ${rollbackTo.split(':').pop()}，可在部署历史中查看进度。`,
      });
      setRollbackOpen(false);
      refreshAll();
    } finally {
      setRollbackRunning(false);
    }
  };

  // ---------------------------------------------------------------- 参数 / 扩缩容
  const [paramOpen, setParamOpen] = useState(false);
  const [paramTarget, setParamTarget] = useState<EnvService | null>(null);
  const [paramForm] = Form.useForm();
  const [paramSaving, setParamSaving] = useState(false);

  const openParams = (svc: EnvService) => {
    setParamTarget(svc);
    paramForm.setFieldsValue({
      replicas: svc.replicas,
      cpuRequest: svc.cpuRequest,
      cpuLimit: svc.cpuLimit,
      memRequest: svc.memRequest,
      memLimit: svc.memLimit,
      envVars: svc.envVars,
      configMaps: svc.configMaps,
      secrets: svc.secrets,
      note: '',
    });
    setParamOpen(true);
  };

  const submitParams = async () => {
    const v = await paramForm.validateFields();
    setParamSaving(true);
    try {
      await serviceApi.updateParams(paramTarget!.id, {
        replicas: v.replicas,
        cpuRequest: v.cpuRequest,
        cpuLimit: v.cpuLimit,
        memRequest: v.memRequest,
        memLimit: v.memLimit,
        envVars: v.envVars,
        configMaps: v.configMaps,
        secrets: v.secrets,
        note: v.note,
      });
      message.success('参数已保存，将在下次部署 / 重启时生效');
      setParamOpen(false);
      refreshAll();
    } finally {
      setParamSaving(false);
    }
  };

  const [scaleOpen, setScaleOpen] = useState(false);
  const [scaleTarget, setScaleTarget] = useState<EnvService | null>(null);
  /**
   * 目标副本数。**读不到集群时留空**，不预填。
   *
   * 原先一律 `setScaleValue(svc.replicas)`，而那个值是平台库里的"期望"，与集群实况
   * 长期不一致（实测 demobasic：库值 2/2、集群 1/1）。预填错的比不预填更危险——
   * 用户点「确认调整」就把生产从 1 改成 2，而他自己并没有要改。
   */
  const [scaleValue, setScaleValue] = useState<number | null>(null);
  const [scaleReason, setScaleReason] = useState('');
  const [scaleRunning, setScaleRunning] = useState(false);

  const openScale = (svc: EnvService) => {
    const lv = liveState(svc);
    setScaleTarget(svc);
    setScaleValue(lv ? lv.replicas : null);
    setScaleReason('');
    setScaleOpen(true);
  };

  /** 扩容弹窗里展示的集群实况（读不到为 null）。 */
  const scaleLive = scaleTarget ? liveState(scaleTarget) : null;

  const submitScale = async () => {
    if (scaleValue === null) {
      message.warning('请先填写目标副本数');
      return;
    }
    setScaleRunning(true);
    try {
      await serviceApi.scale(scaleTarget!.id, scaleValue, scaleReason || undefined);
      message.success(`已将 ${scaleTarget!.projectKey} 副本数调整为 ${scaleValue}`);
      setScaleOpen(false);
      refreshAll();
    } catch (e) {
      // 不关弹窗：用户填的数值还在，改一下就能重试。后端此时可能拒绝得很具体
      // （例如"该工作负载由 HPA 伸缩，平台不能直接声明副本数"）。
      fail(e, `调整 ${scaleTarget!.projectKey} 的副本数失败`);
    } finally {
      setScaleRunning(false);
    }
  };

  const doRestart = (svc: EnvService) => {
    // 个数取集群实况，读不到才退回平台记录 —— 弹窗里的数字必须是集群里真正在跑的个数。
    const lv = liveState(svc);
    return modal.confirm({
      title: `确认重启 ${svc.projectKey}？`,
      content: `将按当前更新策略依次重建 ${lv ? lv.replicas : svc.replicas} 个 Pod，期间服务可能出现短暂抖动。`,
      okText: '确认重启',
      onOk: async () => {
        try {
          await serviceApi.restart(svc.id);
          // 后端是**同步确认**过才返回的（等到 Deployment 的 generation 递增且被控制器观察到），
          // 不是"已提交、排队中"。所以这里说"已触发"，不说"已提交任务"。
          message.success(`已触发 ${svc.projectKey} 的滚动重启`);
          refreshAll();
        } catch (e) {
          fail(e, `触发 ${svc.projectKey} 的重启失败`);
          throw e; // 让弹窗留着，用户可重试或先去处理原因
        }
      },
    });
  };

  const doRedeploy = (svc: EnvService) =>
    modal.confirm({
      title: `确认重新部署 ${svc.projectKey}？`,
      content: '将使用当前镜像版本触发一次滚动更新。',
      okText: '开始滚动更新',
      onOk: async () => {
        try {
          await serviceApi.redeploy(svc.id);
          message.success(`已触发 ${svc.projectKey} 的滚动更新`);
          refreshAll();
        } catch (e) {
          fail(e, `重新部署 ${svc.projectKey} 失败`);
          throw e;
        }
      },
    });

  const doOffline = (svc: EnvService) =>
    modal.confirm({
      title: `确认从${env?.name ?? key}取消纳管 ${svc.projectKey}？`,
      content: (
        <div style={{ fontSize: 13 }}>
          {/* 口径与后端 DeleteService 的**实际动作**一致（2026-10-14 改）：
              只删掉平台下发的变更单（AppRelease）+ 平台里的服务实例记录，
              **不缩容、不删 Deployment、不改镜像** —— 集群里那个服务照旧在跑。
              原先这里写"会把副本数缩容到 0…业务将立即不可用"，而那也是当时的真实行为；
              用户指出一个发布平台不该有这种副作用，前后端一起改成了"只取消纳管"。
              文案必须跟着行为走：写重了没人敢用，写轻了就是骗人。 */}
          <b>平台只是发布通道：这一步只解除纳管，不会改动集群里正在运行的服务。</b>
          <br />
          具体动作是删除平台下发的变更单（AppRelease）并移除平台里的服务实例记录；
          Deployment 的镜像与副本数保持不变，**业务不中断**。
          <br />
          <Text type="secondary">
            取消纳管后平台不再跟踪该服务；想再次接管，重新纳管并部署一次即可（集群里那个负载仍在，会被重新接管）。
          </Text>
        </div>
      ),
      okText: '确认取消纳管',
      okButtonProps: { danger: true },
      onOk: async () => {
        try {
          await serviceApi.remove(svc.id);
          message.success(`已取消纳管 ${svc.projectKey}（集群里的服务未受影响）`);
          refreshAll();
        } catch (e) {
          fail(e, `取消纳管 ${svc.projectKey} 失败`);
          throw e;
        }
      },
    });

  const syncCluster = async () => {
    await environmentApi.sync(key);
    message.success('已同步集群状态');
    refreshAll();
  };

  // ---------------------------------------------------------------- 环境变量编辑
  const [varsOpen, setVarsOpen] = useState(false);
  const [varsForm] = Form.useForm();
  const [varsSaving, setVarsSaving] = useState(false);

  const submitVars = async () => {
    const v = await varsForm.validateFields();
    setVarsSaving(true);
    try {
      await environmentApi.saveVars(
        key,
        (v.vars as EnvVar[]).filter((x) => x?.key),
      );
      message.success('环境变量已保存');
      setVarsOpen(false);
      refreshAll();
    } finally {
      setVarsSaving(false);
    }
  };

  // ---------------------------------------------------------------- 服务表列
  // ⋮ 菜单里的每一项都是"发起变更"，禁用条件必须与后端 guard 逐条对应，
  // 否则只读角色点得动、发得出、最后 403。对应关系：
  //   redeploy / scale / restart / params / delete → handler 内 guardServiceEnv(c,"deploy",id)
  //   rollback                                     → 路由 rollback:execute + handler deploy:{env}
  const deployGuardTip = canDeploy ? undefined : `需要 deploy:${key} 权限`;
  const rollbackGuardTip = canRollback ? undefined : `需要 rollback:execute 与 deploy:${key} 权限`;
  const serviceColumns: ColumnsType<EnvService> = [
    {
      title: '服务 / 项目',
      dataIndex: 'projectName',
      width: 200,
      fixed: 'left',
      render: (v, r) => (
        <div>
          <div className="yn-service-name">
            <Link to={`/ci/projects/${r.projectKey}`}>{r.projectKey}</Link>
          </div>
          <div className="yn-service-sub">
            {v} · {r.kind}
          </div>
        </div>
      ),
    },
    {
      title: '当前镜像版本',
      dataIndex: 'image',
      width: 320,
      render: (v, r) => {
        const lv = liveState(r);
        // 展示**集群里正在跑的**镜像，而不是平台库里记的那个：
        // 两者在"构建过但还没部署"时会不同（实测 demobasic：库里 1.2.0.2761、
        // 集群里 1.2.0.2577），而用户关心的显然是线上跑的是哪个。
        return (
          <div>
            {lv ? <ImageText value={lv.image} width={320} /> : <LiveFallback s={r} />}
            <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 3 }}>
              {lv ? `集群实时 · 平台记录更新于 ${r.updatedAt}` : `平台记录 ${v || '—'}`}
            </div>
          </div>
        );
      },
    },
    {
      title: '副本',
      key: 'replicas',
      width: 106,
      render: (_, r) => {
        const lv = liveState(r);
        if (!lv) return <LiveFallback s={r} />;
        // 取色口径见 liveHealthy：**就绪数相等 ≠ 健康** ——
        // 新 Pod 起不来时老 Pod 会把就绪数撑满，只看比值就会把"发布失败中"渲染成绿色。
        const healthy = liveHealthy(lv);
        return (
          <div>
            <span style={{ fontWeight: 500, color: healthy ? '#52c41a' : '#faad14' }}>
              {lv.readyReplicas}
            </span>
            {/* 分母用**实例总数**而不是期望数：滚动卡住时集群里会多出一个崩溃的实例，
                只按期望显示就会写成 `1/1`，把"有一个实例在崩"整个抹掉。
                口径见 utils/live.ts 的 liveReplicaTotal。 */}
            <span style={{ color: '#9aa0aa' }}> / {liveReplicaTotal(lv)}</span>
            <div style={{ fontSize: 12, color: '#9aa0aa' }}>Ready / 实例</div>
          </div>
        );
      },
    },
    {
      title: '状态',
      key: 'status',
      width: 106,
      render: (_, r) => {
        const lv = liveState(r);
        if (!lv) return <LiveFallback s={r} />;
        return <ServiceStatusTag status={liveStatusWord(lv.status)} />;
      },
    },
    {
      title: '资源',
      key: 'res',
      width: 170,
      render: (_, r) => {
        const lv = liveState(r);
        if (!lv) return <LiveFallback s={r} />;
        // 这是容器**声明的规格**（request / limit），不是使用量；使用量在 Pod 明细里。
        // 空串 = 容器没写这个规格，显示 "—" 而不是 0。
        return (
          <div style={{ fontSize: 12, color: '#5c6470', lineHeight: 1.8 }}>
            <div>
              CPU {lv.cpuRequest || '—'} / {lv.cpuLimit || '—'}
            </div>
            <div>
              MEM {lv.memRequest || '—'} / {lv.memLimit || '—'}
            </div>
          </div>
        );
      },
    },
    {
      title: 'Pod',
      key: 'pods',
      width: 92,
      render: (_, r) => (
        <Button type="link" size="small" style={{ paddingInline: 0 }} onClick={() => openLogs(r)}>
          查看日志
        </Button>
      ),
    },
    {
      title: '操作',
      key: 'action',
      fixed: 'right',
      width: 150,
      render: (_, r) => (
        <Space size={2}>
          {/* 原写法是 <Tooltip><Button disabled/></Tooltip>：disabled 的 button 不派发
              鼠标事件，提示永远弹不出来。改用 DisabledTip 承接 hover。 */}
          <DisabledTip title={canDeploy ? undefined : `当前账号没有 deploy:${key} 权限`}>
            <Button
              type="link"
              size="small"
              style={{ paddingInline: 4 }}
              icon={<ThunderboltOutlined />}
              disabled={!canDeploy}
              onClick={() => openDeploy(r)}
            >
              部署
            </Button>
          </DisabledTip>
          <Button type="link" size="small" style={{ paddingInline: 4 }} icon={<FileTextOutlined />} onClick={() => openLogs(r)}>
            日志
          </Button>
          <Dropdown
            menu={{
              items: [
                {
                  key: 'redeploy',
                  icon: <ReloadOutlined />,
                  // POST /services/:id/redeploy → guardServiceEnv(c,"deploy",id)
                  disabled: !canDeploy,
                  label: (
                    <DisabledTip title={deployGuardTip}>
                      <span>重新部署（当前版本）</span>
                    </DisabledTip>
                  ),
                },
                {
                  key: 'rollback',
                  icon: <RollbackOutlined />,
                  // POST /rollbacks → 路由 rollback:execute + handler guardServiceEnv(c,"deploy",id)
                  disabled: !canRollback,
                  label: (
                    <DisabledTip title={rollbackGuardTip}>
                      <span>回滚到历史版本</span>
                    </DisabledTip>
                  ),
                },
                {
                  key: 'params',
                  icon: <SettingOutlined />,
                  // PUT /services/:id/params → guardServiceEnv(c,"deploy",id)
                  disabled: !canDeploy,
                  label: (
                    <DisabledTip title={deployGuardTip}>
                      <span>设置参数</span>
                    </DisabledTip>
                  ),
                },
                {
                  key: 'scale',
                  icon: <ExpandOutlined />,
                  // POST /services/:id/scale → guardServiceEnv(c,"deploy",id)
                  disabled: !canDeploy,
                  label: (
                    <DisabledTip title={deployGuardTip}>
                      <span>扩缩容</span>
                    </DisabledTip>
                  ),
                },
                {
                  key: 'restart',
                  icon: <SwapOutlined />,
                  // POST /services/:id/restart → guardServiceEnv(c,"deploy",id)
                  disabled: !canDeploy,
                  label: (
                    <DisabledTip title={deployGuardTip}>
                      <span>重启 Deployment</span>
                    </DisabledTip>
                  ),
                },
                { type: 'divider' },
                {
                  key: 'offline',
                  icon: <DeleteOutlined />,
                  danger: true,
                  // DELETE /services/:id → guardServiceEnv(c,"deploy",id)
                  disabled: !canDeploy,
                  label: (
                    <DisabledTip title={deployGuardTip}>
                      {/* 不叫"下线"：它**不会**让服务停下来（那要用户显式改副本数）。
                          名字与动作对不上，用户就不敢点、或者点完以为业务停了。 */}
                      <span>取消纳管</span>
                    </DisabledTip>
                  ),
                },
              ],
              onClick: ({ key: k }) => {
                if (k === 'redeploy') doRedeploy(r);
                else if (k === 'rollback') openRollback(r);
                else if (k === 'params') openParams(r);
                else if (k === 'scale') openScale(r);
                else if (k === 'restart') doRestart(r);
                else if (k === 'offline') doOffline(r);
              },
            }}
          >
            <Button type="text" size="small" icon={<MoreOutlined />} />
          </Dropdown>
        </Space>
      ),
    },
  ];

  // ---------------------------------------------------------------- 派生指标
  /**
   * "运行中 / 待处理"的判据只认**集群实时状态**。
   *
   * 平台库里的 `service_instance.status` 是纳管时按默认值写入的期望，之后只有部署成功
   * 才更新，拿它统计会得出"纳管 15 个、运行正常 15 个"这种与集群实况无关的漂亮数字。
   *
   * 读不到集群的服务单列一档（unknown）：既不算"运行正常"，也不算"待处理"——
   * 混进任何一边都是在拿没读到的值下结论。
   *
   * 实现搬到了 `utils/live.ts`（`liveWordOf` / `isServiceAbnormal`）：概览页与环境列表
   * 要数同一个东西，三处各写一遍迟早会出现"这边说 3 个正常、那边说 1 个待关注"。
   */
  const runningCount = services.filter((s) => liveWordOf(s) === 'running').length;
  const liveUnknownCount = services.filter((s) => liveWordOf(s) === 'unknown').length;
  const abnormal = services.filter(isServiceAbnormal);
  const pendingApproval = envDeploys.filter((d) => d.status === 'pending_approval').length;
  // 不能用 toISOString().slice(0,10)：那是 UTC 日期，而后端下发的 startedAt 是
  // 带本地时区的 RFC3339（如 2026-09-29T00:30:00+08:00），东八区凌晨 0–8 点会算错一天。
  // 这里按本地日历取日期，与被比较串的前 10 位口径一致。
  const now = new Date();
  const todayKey = `${now.getFullYear()}-${pad2(now.getMonth() + 1)}-${pad2(now.getDate())}`;
  const todayDeploys = envDeploys.filter((d) => (d.startedAt || '').startsWith(todayKey)).length;

  const activeTab = searchParams.get('tab') ?? 'services';
  const setTab = (t: string) => setSearchParams({ tab: t });

  // 环境本身加载失败 / 不存在：给出可操作的空态，而不是白屏
  if (!envApi.loading && !env && envApi.error) {
    return (
      <div>
        <PageHeader
          breadcrumb={[{ title: 'CD 环境部署' }, { title: '环境管理', path: '/cd/environments' }, { title: key }]}
          title={key}
          onBack={() => navigate('/cd/environments')}
        />
        <Alert
          type="error"
          showIcon
          message="环境加载失败"
          description={envApi.error.message}
          action={
            <Space>
              <Button size="small" onClick={() => envApi.refresh()}>
                重试
              </Button>
              <Button size="small" type="primary" onClick={() => navigate('/cd/environments')}>
                返回列表
              </Button>
            </Space>
          }
        />
      </div>
    );
  }
  if (!envApi.loading && !env) {
    return (
      <div>
        <PageHeader
          breadcrumb={[{ title: 'CD 环境部署' }, { title: '环境管理', path: '/cd/environments' }, { title: key }]}
          title={key}
          onBack={() => navigate('/cd/environments')}
        />
        <Empty description={`未找到环境 ${key}`} style={{ marginTop: 80 }} />
      </div>
    );
  }
  if (!env) {
    // 首次加载中
    return (
      <SectionCard bodyStyle={{ padding: 48 }}>
        <div style={{ textAlign: 'center', color: '#9aa0aa', fontSize: 13 }}>正在加载环境信息…</div>
      </SectionCard>
    );
  }

  return (
    <div>
      <PageHeader
        breadcrumb={[
          { title: 'CD 环境部署' },
          { title: '环境管理', path: '/cd/environments' },
          { title: env.name },
        ]}
        title={env.name}
        subTitle={env.key}
        tags={
          <>
            {/* 徽标取**同一页那份实时结果**（`res`），不取 `env.status`：
                否则会出现"下面「集群资源」写着 93%、上面的徽标却按几小时前的 96%
                判成资源紧张"—— 一张页面自相矛盾。 */}
            <EnvStatusBadge status={envResStatus(res)} />
            <MethodTag method={env.deployMethod} />
            {env.needApproval ? (
              <Tooltip title={`审批人：${env.approvers.join('、') || '未指定'}`}>
                <Tag color="orange" icon={<SafetyCertificateOutlined />} style={{ marginInlineEnd: 0 }}>
                  部署需审批
                </Tag>
              </Tooltip>
            ) : null}
            {!env.enabled ? <Tag style={{ marginInlineEnd: 0 }}>已停用</Tag> : null}
          </>
        }
        description={env.description}
        onBack={() => navigate('/cd/environments')}
        extra={
          <Space>
            {/* 状态自动刷新：本页数据（副本/就绪、部署单状态、集群资源）都是别处在改的 */}
            <AutoRefreshButton
              value={autoRefreshMs}
              onChange={setAutoRefreshMs}
              updatedAt={servicesApi.updatedAt}
              errorText={servicesApi.silentError?.message}
            />
            <DisabledTip title={canWrite ? undefined : '当前账号没有 env:write 权限'}>
              <Button
                icon={<AppstoreAddOutlined />}
                disabled={!canWrite}
                onClick={() => openLink(env.projectKeys)}
              >
                纳管项目
              </Button>
            </DisabledTip>
            <Button icon={<ReloadOutlined />} onClick={syncCluster} disabled={!canWrite}>
              同步集群
            </Button>
            <Button icon={<HistoryOutlined />} onClick={() => setTab('deploys')}>
              部署历史
            </Button>
            {/* 禁用有两个理由，都要说清：缺 deploy:{env} 权限，或该环境还没有纳管服务
                （部署向导的项目下拉就是 env.projectKeys，空列表时无可选项；
                后端对未纳管项目也是 40400「该环境尚未纳管此项目」）。 */}
            <DisabledTip
              title={
                canDeploy
                  ? services.length
                    ? undefined
                    : '该环境还没有纳管服务，暂无可部署的项目'
                  : `当前账号没有 deploy:${key} 权限`
              }
            >
              <Button
                type="primary"
                icon={<ThunderboltOutlined />}
                onClick={() => openDeploy()}
                disabled={!canDeploy || !services.length}
              >
                部署新服务
              </Button>
            </DisabledTip>
            {/* 环境级动作：与列表页共用同一个组件（权限门控、实例数预检、二次确认
                都只有一份）。用常规按钮而不是文字链接 —— 这一排已经有
                「纳管项目 / 同步集群 / 部署历史」，两个裸的"编辑 / 删除"会分不清在编辑什么。 */}
            <EnvironmentActions
              env={env}
              svcCount={servicesApi.error || servicesApi.loading ? null : services.length}
              variant="button"
              onEdit={() => setEnvEditOpen(true)}
              onRemoved={() => navigate('/cd/environments')}
            />
          </Space>
        }
      />

      {pendingApproval ? (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message={`有 ${pendingApproval} 个部署单等待审批`}
          description={`${env.name}部署需要 ${env.approvers.join('、') || '指定审批人'} 审批后方可执行。`}
          action={
            <Button size="small" onClick={() => setTab('deploys')}>
              去处理
            </Button>
          }
        />
      ) : null}

      {abnormal.length ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message={`${abnormal.length} 个服务状态异常`}
          description={
            <span>
              {abnormal
                .map((s) => {
                  // 状态词也要取自**集群实时状态**：`abnormal` 已经是按 live 选出来的，
                  // 这里再用平台库的 `s.status` 取词就会写出"待调度"而集群里其实已 failed。
                  const w = liveWordOf(s);
                  const word =
                    w === 'error'
                      ? '异常'
                      : w === 'deploying'
                        ? '部署中'
                        : w === 'running'
                          ? '运行中'
                          : w === 'stopped'
                            ? '已停止'
                            : '状态未知';
                  return `${s.projectKey}（${word}）`;
                })
                .join('、')}
              ，点开服务日志排查。
            </span>
          }
          action={
            <Space>
              <Button size="small" onClick={() => openLogs(abnormal[0])}>
                查看日志
              </Button>
              {/* /ai 路由守卫要求 ai:query（seed 里只有 admin 有；ops 没有），
                  缺权限点放行只会落到 403 空壳页，故按权限禁用并说明。 */}
              <DisabledTip title={canAskAI ? undefined : '需要 ai:query 权限'}>
                <Button size="small" type="primary" ghost disabled={!canAskAI} onClick={() => navigate('/ai')}>
                  问 AI 助手
                </Button>
              </DisabledTip>
            </Space>
          }
        />
      ) : null}

      <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
        <Col xs={12} xl={6}>
          <StatCard
            title="纳管服务"
            value={services.length}
            unit="个"
            accent="#1677ff"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                运行正常 {runningCount} 个
                {liveUnknownCount ? `，${liveUnknownCount} 个集群状态读取不到` : ''}
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="项目 / 节点"
            value={`${env.projectKeys.length} / ${res?.nodes ?? env.nodes}`}
            accent="#722ed1"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                已纳管项目 / 集群节点{res ? '（实时）' : '（上次同步）'}
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            // 计数只在 pageSize:50 拉回的前 50 条部署里统计，不能写成全量口吻
            title="今日部署（近 50 条）"
            value={todayDeploys}
            unit="次"
            accent="#52c41a"
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                最近同步 {(env.lastSyncAt || '').slice(11, 19) || '—'}
              </Text>
            }
          />
        </Col>
        <Col xs={12} xl={6}>
          <StatCard
            title="集群资源"
            // 读不到就写 '—'，**不退回 `env.cpuUsage / env.memUsage`**：那是上次同步
            // 留下的旧值，填进来会与下面「集群资源」卡片里那句"读不到集群用量"打架
            // —— 一张页面同时说"没读到"和"93%"。
            value={usage ? `${usage.cpuUsage}/${usage.memUsage}` : '—'}
            unit={usage ? '%' : ''}
            accent={usage && (usage.cpuUsage > 85 || usage.memUsage > 85) ? '#ff4d4f' : '#faad14'}
            footer={
              <Text type="secondary" style={{ fontSize: 12 }}>
                CPU / 内存 使用率
              </Text>
            }
          />
        </Col>
      </Row>

      <Row gutter={[16, 16]}>
        <Col xs={24} xl={17}>
          <Tabs
            activeKey={activeTab}
            onChange={setTab}
            items={[
              {
                key: 'services',
                label: `服务列表 ${services.length}`,
                children: (
                  <SectionCard bodyStyle={{ padding: 0 }}>
                    <Table<EnvService>
                      rowKey="id"
                      size="middle"
                      loading={servicesApi.loading}
                      dataSource={services}
                      columns={serviceColumns}
                      scroll={{ x: 1180 }}
                      expandable={{
                        expandedRowRender: (r) => {
                          const lv = liveState(r);
                          return (
                          <div style={{ padding: '4px 8px 8px' }}>
                            <Row gutter={[24, 12]}>
                              <Col xs={24} md={10}>
                                <div className="yn-sub-title">健康检查</div>
                                <Space size={6} wrap>
                                  {/* ⚠️ 这里曾经读 `r.health.liveness/readiness` —— 那是**平台库里的
                                      静态派生值**（`seed_ci.go` 按平台 status 反推、新建服务时写死 true），
                                      与集群里的探针状态**无关**。实测 demobasic：库里两项都是 true、
                                      界面显示"通过"，而集群里新 Pod 正在 CrashLoopBackOff、
                                      Deployment 已报 ProgressDeadlineExceeded ——
                                      两个"通过"恰好把异常盖住，正是"只展示成功的一面"。
                                      集群读得到就听集群的；读不到就说读不到，不回退库里的旧值。 */}
                                  <Tag
                                    color={lv ? (liveHealthy(lv) ? 'success' : 'error') : 'default'}
                                    style={{ marginInlineEnd: 0 }}
                                  >
                                    {lv ? (liveHealthy(lv) ? '健康' : '异常') : '健康状态未知'}
                                  </Tag>
                                  {/* 口径同「副本」列（见 liveHealthy）：就绪数相等不足以判健康 */}
                                  <Tag
                                    color={lv ? (liveHealthy(lv) ? 'success' : 'warning') : 'default'}
                                    style={{ marginInlineEnd: 0 }}
                                  >
                                    {lv
                                      ? `${lv.readyReplicas}/${liveReplicaTotal(lv)} Ready`
                                      : liveFallbackText(r)}
                                  </Tag>
                                </Space>
                                {lv?.message ? (
                                  <div style={{ fontSize: 12, color: '#d46b08', marginTop: 6, lineHeight: 1.7 }}>
                                    集群说明：{lv.message}
                                  </div>
                                ) : null}
                                <div className="yn-sub-title" style={{ marginTop: 12 }}>
                                  工作负载
                                </div>
                                <div style={{ fontSize: 12.5, color: '#5c6470', lineHeight: 1.9 }}>
                                  <div>类型：{r.kind}</div>
                                  <div>
                                    访问入口：<span className="yn-mono">{r.entrypoint || '—'}</span>
                                  </div>
                                  <div>
                                    最近更新：{r.updatedAt} · {r.updatedBy || '—'}
                                  </div>
                                </div>
                              </Col>
                              <Col xs={24} md={8}>
                                <div className="yn-sub-title">环境变量</div>
                                <div style={{ maxHeight: 132, overflowY: 'auto' }}>
                                  {r.envVars.length ? (
                                    r.envVars.map((v) => (
                                      <div key={v.key} style={{ fontSize: 12.5, lineHeight: 1.9 }}>
                                        <span className="yn-mono" style={{ color: '#1677ff' }}>
                                          {v.key}
                                        </span>
                                        <span style={{ color: '#9aa0aa' }}> = </span>
                                        <span className="yn-mono" style={{ color: v.secret ? '#9aa0aa' : '#5c6470' }}>
                                          {v.secret ? '••••••' : v.value}
                                        </span>
                                      </div>
                                    ))
                                  ) : (
                                    <div style={{ fontSize: 12.5, color: '#9aa0aa' }}>未配置服务级变量</div>
                                  )}
                                </div>
                              </Col>
                              <Col xs={24} md={6}>
                                <div className="yn-sub-title">配置与密钥</div>
                                <div style={{ fontSize: 12.5, lineHeight: 1.9, color: '#5c6470' }}>
                                  <div>ConfigMap：{r.configMaps.join('、') || '—'}</div>
                                  <div>Secret：{r.secrets.join('、') || '—'}</div>
                                </div>
                              </Col>
                            </Row>
                            {/* Pod 明细横跨整行（不塞进上面某一列）：Pod 名本身就长，
                                再挤进 1/4 宽的列里就只能换行堆叠，看不出"哪个正常、哪个在崩"。
                                见 ServicePodsInline 的注释 —— 聚合数字会把失败盖住。 */}
                            <div style={{ marginTop: 14 }}>
                              <ServicePodsInline serviceId={r.id} onOpenLogs={() => openLogs(r)} />
                            </div>
                          </div>
                          );
                        },
                        rowExpandable: () => true,
                      }}
                      pagination={{ pageSize: 10, showTotal: (t) => `共 ${t} 个服务` }}
                    />
                  </SectionCard>
                ),
              },
              {
                key: 'deploys',
                // 数据源是 deployApi.list({pageSize:50})，只有前 50 条，不能写成全量口吻
                label: `部署历史 ${envDeploys.length}（近 50 条）`,
                children: (
                  <SectionCard bodyStyle={{ padding: 0 }}>
                    <Table<DeployRecord>
                      rowKey="id"
                      size="middle"
                      loading={deploysApi.loading}
                      dataSource={envDeploys}
                      columns={[
                        { title: '部署单号', dataIndex: 'deployNo', width: 150, render: (v) => <span className="yn-mono">{v}</span> },
                        { title: '服务', dataIndex: 'projectKey', width: 150, render: (v) => <Link to={`/ci/projects/${v}`}>{v}</Link> },
                        { title: '镜像版本', dataIndex: 'image', width: 310, render: (v) => <ImageText value={v} width={300} /> },
                        { title: '副本', dataIndex: 'replicas', width: 70 },
                        { title: '状态', dataIndex: 'status', width: 112, render: (v) => <DeployStatusTag status={v} /> },
                        { title: '操作人', dataIndex: 'operator', width: 96, render: (v) => v || '—' },
                        { title: '审批人', dataIndex: 'approver', width: 96, render: (v) => v ?? <Text type="secondary">—</Text> },
                        { title: '耗时', dataIndex: 'duration', width: 92, render: (v) => (v ? humanDuration(v) : '—') },
                        { title: '开始时间', dataIndex: 'startedAt', width: 168, render: (v) => <span className="yn-mono" style={{ color: '#8c8c8c' }}>{v}</span> },
                      ]}
                      scroll={{ x: 1260 }}
                      pagination={{ pageSize: 10, showTotal: (t) => `共 ${t} 条部署记录` }}
                    />
                  </SectionCard>
                ),
              },
              {
                key: 'rollbacks',
                // 同「部署历史」：数据源是 rollbackApi.list({pageSize:50})，只有前 50 条
                label: `回滚记录 ${envRollbacks.length}（近 50 条）`,
                children: (
                  <SectionCard bodyStyle={{ padding: 0 }}>
                    <Table<RollbackRecord>
                      rowKey="id"
                      size="middle"
                      loading={rollbacksApi.loading}
                      dataSource={envRollbacks}
                      columns={[
                        { title: '服务', dataIndex: 'projectKey', width: 150, render: (v) => <Link to={`/ci/projects/${v}`}>{v}</Link> },
                        { title: '从（异常版本）', dataIndex: 'fromImage', width: 300, render: (v) => <ImageText value={v} width={290} /> },
                        { title: '回滚到', dataIndex: 'toImage', width: 300, render: (v) => <ImageText value={v} width={290} /> },
                        {
                          title: '触发方式',
                          dataIndex: 'triggerType',
                          width: 120,
                          render: (v) =>
                            v === 'ai_suggested' ? (
                              <Tag color="purple">AI 建议</Tag>
                            ) : v === 'auto' ? (
                              <Tag color="orange">自动</Tag>
                            ) : (
                              <Tag>人工</Tag>
                            ),
                        },
                        { title: '操作人', dataIndex: 'operator', width: 96, render: (v) => v || '—' },
                        {
                          title: '状态',
                          dataIndex: 'status',
                          width: 108,
                          render: (v: string) => (
                            <Tag
                              color={
                                v === 'success' ? 'success' : v === 'failed' ? 'error' : v === 'interrupted' ? 'orange' : 'processing'
                              }
                            >
                              {/* 已中断单列：回滚复用部署状态机，平台重启会让它永远停在 progressing，
                                  原来的兜底分支会把它显示成「执行中」。 */}
                              {v === 'success' ? '成功' : v === 'failed' ? '失败' : v === 'interrupted' ? '已中断' : '执行中'}
                            </Tag>
                          ),
                        },
                        { title: '耗时', dataIndex: 'duration', width: 90, render: (v) => (v ? humanDuration(v) : '—') },
                        { title: '时间', dataIndex: 'startedAt', width: 168, render: (v) => <span className="yn-mono" style={{ color: '#8c8c8c' }}>{v}</span> },
                      ]}
                      scroll={{ x: 1400 }}
                      pagination={{ pageSize: 10, showTotal: (t) => `共 ${t} 条回滚记录` }}
                    />
                  </SectionCard>
                ),
              },
              {
                key: 'vars',
                label: '环境变量',
                children: (
                  <SectionCard
                    title="环境级变量（对所有服务生效）"
                    extra={
                      <DisabledTip title={canWrite ? undefined : '当前账号没有 env:write 权限'}>
                        <Button
                          size="small"
                          type="primary"
                          ghost
                          icon={<SettingOutlined />}
                          disabled={!canWrite}
                          onClick={() => {
                            varsForm.setFieldsValue({ vars: env.vars });
                            setVarsOpen(true);
                          }}
                        >
                          编辑
                        </Button>
                      </DisabledTip>
                    }
                    bodyStyle={{ padding: 0 }}
                  >
                    <Table<EnvVar>
                      rowKey="key"
                      size="middle"
                      pagination={false}
                      dataSource={env.vars}
                      columns={[
                        { title: '变量名', dataIndex: 'key', width: 260, render: (v) => <span className="yn-mono">{v}</span> },
                        {
                          title: '变量值',
                          dataIndex: 'value',
                          render: (v, r) => (
                            <span className="yn-mono" style={{ color: r.secret ? '#9aa0aa' : '#2c313a' }}>
                              {r.secret ? '••••••••••' : v}
                            </span>
                          ),
                        },
                        {
                          title: '类型',
                          dataIndex: 'secret',
                          width: 100,
                          render: (v) => (v ? <Tag color="orange">密文</Tag> : <Tag>明文</Tag>),
                        },
                      ]}
                      locale={{ emptyText: '该环境还没有配置环境变量' }}
                    />
                  </SectionCard>
                ),
              },
            ]}
          />
        </Col>

        <Col xs={24} xl={7}>
          <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <SectionCard title="环境信息" bodyStyle={{ padding: 0 }}>
              <KVList
                columns={1}
                data={[
                  { label: '环境标识 / Namespace', value: <span className="yn-mono">{env.key} / {env.namespace}</span> },
                  {
                    label: 'K8s 集群',
                    value: (
                      <span>
                        {env.cluster}
                        {/* 自带凭据时集群标识只是标签，注册表里同名的记录是另一台机器 */}
                        {isOwnCluster(env) ? (
                          <Tooltip title={OWN_CLUSTER_HINT}>
                            <Tag style={{ marginInlineStart: 6, marginInlineEnd: 0 }}>
                              {OWN_CLUSTER_BADGE}
                            </Tag>
                          </Tooltip>
                        ) : null}{' '}
                        <Text type="secondary" style={{ fontSize: 12 }}>
                          {res?.version || env.clusterVersion}
                        </Text>
                      </span>
                    ),
                  },
                  { label: '部署方式', value: <MethodTag method={env.deployMethod} /> },
                  { label: '镜像仓库', value: <span className="yn-mono">{env.registry}</span> },
                  { label: '入口域名', value: <span className="yn-mono">{env.ingressDomain || '—'}</span> },
                  { label: '负责人', value: env.owner || '—' },
                  { label: '最近同步', value: <span className="yn-mono">{env.lastSyncAt}</span> },
                ]}
              />
            </SectionCard>

            <SectionCard title="集群资源" bodyStyle={{ padding: '16px 20px' }}>
              {/* 用量只在**真的读到**时才画进度条。
                  读不到时不回退到 env.cpuUsage / env.memUsage：那是上次同步留下的旧值，
                  拿它画一条 60% 的条会让人以为这就是集群当下的水位。 */}
              {resApi.loading && !res ? (
                <Text type="secondary" style={{ fontSize: 12.5 }}>
                  正在读取集群资源…
                </Text>
              ) : usage ? (
                <>
                  <UsageBar label="CPU 使用率" percent={usage.cpuUsage} detail={`${usage.cpuUsage}%`} />
                  <UsageBar label="内存使用率" percent={usage.memUsage} detail={`${usage.memUsage}%`} />
                </>
              ) : (
                <Alert
                  type={resApi.error ? 'error' : 'warning'}
                  showIcon
                  style={{ marginBottom: 12 }}
                  message="读不到集群用量"
                  description={
                    resApi.error
                      ? resApi.error.message
                      : '集群未提供 metrics 指标（需安装 metrics-server），或当前凭据无权读取该接口。'
                  }
                />
              )}
              <Divider style={{ margin: '4px 0 12px' }} />
              <Row gutter={12}>
                <Col span={12}>
                  <Statistic
                    label={res ? '节点数（集群实时）' : '节点数（上次同步）'}
                    value={res?.nodes ?? env.nodes}
                  />
                </Col>
                <Col span={12}>
                  <Statistic label="服务数" value={services.length} />
                </Col>
              </Row>
            </SectionCard>

            <SectionCard title="最近部署动态" bodyStyle={{ padding: '8px 8px 12px' }}>
              {envDeploys.slice(0, 6).map((d) => (
                <div key={d.id} style={{ padding: '8px 12px', borderRadius: 6 }}>
                  <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center' }}>
                    <Space size={8}>
                      <span style={{ fontSize: 13, fontWeight: 500 }}>{d.projectKey}</span>
                      <DeployStatusTag status={d.status} showIcon={false} />
                    </Space>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      {(d.startedAt || '').slice(5, 16)}
                    </Text>
                  </div>
                  <div
                    className="yn-mono"
                    style={{ fontSize: 11.5, color: '#9aa0aa', marginTop: 3, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}
                  >
                    {d.image.split(':').pop()}
                  </div>
                </div>
              ))}
              {!envDeploys.length ? (
                <div style={{ padding: '16px 12px', color: '#9aa0aa', fontSize: 13 }}>该环境还没有部署记录</div>
              ) : null}
            </SectionCard>

            <SectionCard title="待处理" bodyStyle={{ padding: '8px 8px 12px' }}>
              {abnormal.length ? (
                abnormal.map((s) => (
                  <div key={s.id} style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '8px 12px' }}>
                    <div>
                      <div style={{ fontSize: 13 }}>{s.projectKey}</div>
                      <div style={{ fontSize: 12, color: '#9aa0aa' }}>
                        {/* 就绪数同样只认集群实况：拿平台库的 2/2 去报一个正在掉副本的服务，
                            比不显示更糟。分母用实例总数（见 liveReplicaTotal）——
                            滚动卡住时多出来的那个崩溃实例必须在数字里看得见。 */}
                        {liveState(s)
                          ? `${liveState(s)!.readyReplicas}/${liveReplicaTotal(liveState(s)!)} 副本就绪`
                          : liveFallbackText(s)}
                      </div>
                    </div>
                    <Space size={2}>
                      <Button size="small" type="link" onClick={() => openLogs(s)} style={{ paddingInline: 4 }}>
                        日志
                      </Button>
                      {/* 后端两把锁：路由 rollback:execute + handler deploy:{env}（见 canRollbackOn）。
                          权限点表里没有 rollback:{env} 这种形态，不能照 canRollback 反向编一个。 */}
                      <DisabledTip title={canRollback ? undefined : `需要 rollback:execute 与 deploy:${key} 权限`}>
                        <Button size="small" type="link" danger disabled={!canRollback} onClick={() => openRollback(s)} style={{ paddingInline: 4 }}>
                          回滚
                        </Button>
                      </DisabledTip>
                    </Space>
                  </div>
                ))
              ) : (
                <div style={{ padding: '16px 12px', color: '#9aa0aa', fontSize: 13 }}>
                  暂无待处理事项
                  {liveUnknownCount ? `（${liveUnknownCount} 个服务的集群状态读取不到，未纳入判断）` : ''}
                </div>
              )}
            </SectionCard>
          </Space>
        </Col>
      </Row>

      {/* ================================================== 部署向导 */}
      <Modal
        open={deployOpen}
        width={800}
        title={
          <Space>
            <ThunderboltOutlined style={{ color: '#1677ff' }} />
            部署到 {env.name}
            <Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>
              {env.namespace}
            </Text>
          </Space>
        }
        onCancel={() => {
          setDeployOpen(false);
          setDeployStep(0);
          setCreatedDeploy(null);
        }}
        footer={
          deployStep >= 3 ? (
            <Button
              type="primary"
              onClick={() => {
                setDeployOpen(false);
                setDeployStep(0);
                setCreatedDeploy(null);
              }}
            >
              完成
            </Button>
          ) : (
            <Space>
              <Button onClick={() => setDeployOpen(false)}>取消</Button>
              {deployStep > 0 ? <Button onClick={() => setDeployStep((s) => s - 1)}>上一步</Button> : null}
              {deployStep < 2 ? (
                <Button type="primary" onClick={() => setDeployStep((s) => s + 1)}>
                  下一步
                </Button>
              ) : (
                <Button type="primary" icon={<ThunderboltOutlined />} loading={submitting} onClick={submitDeploy}>
                  确认部署
                </Button>
              )}
            </Space>
          )
        }
        destroyOnClose
      >
        <Steps
          size="small"
          current={deployStep}
          style={{ marginBottom: 20 }}
          items={[{ title: '选择项目与版本' }, { title: '填写说明' }, { title: '确认部署' }, { title: '执行中' }]}
        />

        {env.needApproval && deployStep === 2 ? (
          <Alert
            type="warning"
            showIcon
            icon={<SafetyCertificateOutlined />}
            style={{ marginBottom: 16 }}
            message="该环境为受保护环境，提交后需审批"
            description={`审批人：${env.approvers.join('、') || '未指定'}。审批通过后平台才会开始部署。`}
          />
        ) : null}

        <Form form={deployForm} layout="vertical" requiredMark="optional">
          {deployStep === 0 ? (
            <Row gutter={16}>
              <Col span={12}>
                <Form.Item name="projectKey" label="选择项目" rules={[{ required: true, message: '请选择项目' }]}>
                  <Select
                    showSearch
                    optionFilterProp="label"
                    placeholder="选择已纳管到本环境的项目"
                    options={env.projectKeys.map((k) => ({ label: k, value: k }))}
                    onChange={(v) => {
                      setDeployProjectKey(v);
                      // 换项目必须清掉上一个项目的镜像选择，否则会把 A 项目的镜像 id 配到 B 项目
                      deployForm.setFieldsValue({ imageRecordId: undefined });
                    }}
                  />
                </Form.Item>
              </Col>
              <Col span={12}>
                <Form.Item
                  name="imageRecordId"
                  label="选择镜像版本"
                  rules={[{ required: true, message: '请选择镜像版本' }]}
                  tooltip="列表来自该项目的镜像仓库记录；无产物说明该项目还没构建过"
                >
                  <Select
                    placeholder={deployProjectKey ? '选择镜像版本' : '请先选择项目'}
                    disabled={!deployProjectKey}
                    loading={imagesApi.loading}
                    options={images.map((i: ImageRecord, idx) => ({
                      label: `${i.tag}${idx === 0 ? '（最新）' : ''}`,
                      value: i.id,
                    }))}
                    notFoundContent={imagesApi.loading ? '加载中…' : '该项目暂无镜像产物，请先构建'}
                  />
                </Form.Item>
              </Col>
              <Col span={8}>
                <Form.Item name="replicas" label="副本数" rules={[{ required: true }]}>
                  <InputNumber min={1} max={key === 'prod' ? 20 : 10} style={{ width: '100%' }} />
                </Form.Item>
              </Col>
              <Col span={8}>
                <Form.Item name="strategy" label="更新策略">
                  <Select
                    options={[
                      { label: '滚动更新（推荐）', value: 'RollingUpdate' },
                      { label: '重建（先停后起）', value: 'Recreate' },
                    ]}
                  />
                </Form.Item>
              </Col>
            </Row>
          ) : null}

          {deployStep === 1 ? (
            <Form.Item
              name="remark"
              label="部署说明"
              rules={env.needApproval ? [{ required: true, message: '需审批的环境必须填写变更说明' }] : []}
            >
              <Input.TextArea rows={3} placeholder="如：修复订单超时问题，关联需求 REQ-2026-0912" maxLength={200} showCount />
            </Form.Item>
          ) : null}

          {deployStep === 2 ? (
            <Form.Item noStyle shouldUpdate>
              {({ getFieldValue }) => {
                const img = images.find((i) => i.id === getFieldValue('imageRecordId'));
                return (
                  <Descriptions column={2} bordered size="small" labelStyle={{ width: 110, background: '#fafbfc' }}>
                    <Descriptions.Item label="目标环境" span={2}>
                      {env.name}（<span className="yn-mono">{env.namespace}</span>）
                    </Descriptions.Item>
                    <Descriptions.Item label="项目" span={2}>
                      {getFieldValue('projectKey') ?? '—'}
                    </Descriptions.Item>
                    <Descriptions.Item label="镜像版本" span={2}>
                      <span className="yn-mono">{img?.image ?? '—'}</span>
                    </Descriptions.Item>
                    <Descriptions.Item label="副本数">{getFieldValue('replicas') ?? '—'}</Descriptions.Item>
                    <Descriptions.Item label="更新策略">{getFieldValue('strategy') ?? '—'}</Descriptions.Item>
                    <Descriptions.Item label="部署说明" span={2}>
                      {getFieldValue('remark') || '—'}
                    </Descriptions.Item>
                  </Descriptions>
                );
              }}
            </Form.Item>
          ) : null}
        </Form>

        {deployStep === 3 ? (
          <div>
            <div style={{ marginBottom: 14 }}>
              <Space size={12}>
                <Text type="secondary" style={{ fontSize: 12 }}>
                  部署单
                </Text>
                <span className="yn-mono">{createdDeploy?.deployNo ?? '—'}</span>
                {createdDeploy ? <DeployStatusTag status={createdDeploy.status} /> : null}
                {deployStream.status?.stage ? <Tag>{deployStream.status.stage}</Tag> : null}
              </Space>
            </div>
            <Progress
              percent={deployStream.status?.progress ?? 0}
              status={deployStream.ended ? 'success' : 'active'}
              strokeColor={{ from: '#69b1ff', to: '#1677ff' }}
              style={{ marginBottom: 14 }}
            />
            {deployStream.error ? (
              <Alert type="warning" showIcon style={{ marginBottom: 12 }} message={deployStream.error} />
            ) : null}
            <LogPanel
              lines={deployStream.lines}
              height={260}
              streaming={false}
              live={!!createdDeploy && !deployStream.ended}
              emptyText="等待部署日志输出…"
              title={`kubectl -n ${env.namespace} rollout`}
            />
          </div>
        ) : null}
      </Modal>

      {/* ================================================== 容器日志 */}
      <Drawer
        open={logOpen}
        onClose={() => setLogOpen(false)}
        width={920}
        title={
          <Space>
            <ContainerOutlined />
            容器日志 · {logService?.projectKey}
            <Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>
              {env.namespace}
            </Text>
          </Space>
        }
        styles={{ body: { padding: 16, background: '#f4f6f9' } }}
      >
        {logService ? (
          <>
            <SectionCard bodyStyle={{ padding: '14px 16px' }} style={{ marginBottom: 12 }}>
              <Space size={12} wrap>
                <span>
                  <Text type="secondary" style={{ fontSize: 12, marginRight: 6 }}>
                    Pod
                  </Text>
                  <Select
                    size="small"
                    style={{ width: 320 }}
                    value={logPod || undefined}
                    onChange={setLogPod}
                    loading={podsApi.loading}
                    placeholder={podsApi.loading ? '加载中…' : '选择 Pod'}
                    options={pods.map((p) => ({ label: `${p.name}（${p.status}）`, value: p.name }))}
                    notFoundContent="该服务当前没有运行中的 Pod"
                  />
                </span>
                <span>
                  <Text type="secondary" style={{ fontSize: 12, marginRight: 6 }}>
                    回溯行数
                  </Text>
                  <Select
                    size="small"
                    style={{ width: 110 }}
                    value={logTail}
                    onChange={setLogTail}
                    options={[200, 500, 1000, 2000].map((n) => ({ label: String(n), value: n }))}
                  />
                </span>
                <Button
                  size="small"
                  icon={<CloudDownloadOutlined />}
                  disabled={!podLogs.lines.length}
                  onClick={() => {
                    navigator.clipboard?.writeText(podLogs.lines.map((l) => `${(l.ts || '').slice(11, 19)}  ${l.line}`).join('\n'));
                    message.success('日志已复制到剪贴板');
                  }}
                >
                  复制
                </Button>
              </Space>
            </SectionCard>

            {pods.length ? (
              <SectionCard bodyStyle={{ padding: '12px 16px' }} style={{ marginBottom: 12 }}>
                <Space size={[8, 8]} wrap>
                  {pods.map((p) => (
                    <Tag
                      key={p.name}
                      color={podStatusColor(p.status)}
                      style={{ marginInlineEnd: 0 }}
                    >
                      {p.name} · {p.status} · 重启 {p.restarts}
                    </Tag>
                  ))}
                </Space>
              </SectionCard>
            ) : null}

            {podLogs.error ? <Alert type="warning" showIcon style={{ marginBottom: 12 }} message={podLogs.error} /> : null}

            <LogPanel
              lines={podLogs.lines}
              height={420}
              streaming={false}
              live={!!podLogUrl && !podLogs.ended}
              emptyText={logPod ? (podLogs.error ? '日志流不可用' : '暂无日志输出…') : '请先选择一个 Pod'}
              title={`kubectl logs ${logPod || '<pod>'} --tail=${logTail}`}
            />
          </>
        ) : null}
      </Drawer>

      {/* ================================================== 回滚 */}
      <Modal
        open={rollbackOpen}
        width={720}
        title={
          <Space>
            <RollbackOutlined style={{ color: '#ff4d4f' }} />
            回滚 {rollbackTarget?.projectKey}
          </Space>
        }
        onCancel={() => setRollbackOpen(false)}
        onOk={submitRollback}
        okText="确认回滚"
        confirmLoading={rollbackRunning}
        okButtonProps={{ danger: true }}
        destroyOnClose
      >
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message="回滚会以历史版本重新创建 Pod"
          description="建议确认该历史版本在测试区已验证通过；回滚操作会写入审计日志。"
        />

        <div style={{ marginBottom: 12, fontSize: 13 }}>
          {/* 这里刻意用**平台记录**的当前镜像，而不是上面表格里那份集群实况：
              回滚接口的"当前版本"是后端按 service_instance.current_image 判定的，
              下面那个"当前版本不可选"的禁用逻辑必须与它一致，否则会禁错一项。
              两者口径不同，所以标签里写明来源，避免与表格里的数字对不上时被当成 bug。 */}
          <Text type="secondary">当前版本（平台记录）：</Text>
          <ImageText value={rollbackTarget?.image ?? ''} width={460} />
        </div>

        <div style={{ fontSize: 13, marginBottom: 8 }}>选择回滚目标版本：</div>
        {versionsApi.loading ? (
          <Text type="secondary">正在加载历史版本…</Text>
        ) : versions.length ? (
          <Radio.Group value={rollbackTo} onChange={(e) => setRollbackTo(e.target.value)} style={{ width: '100%' }}>
            {/* 后端 /services/:id/versions 最多返回 30 条（cd.go serviceVersions 里 Limit(30)），
                原先只渲染前 6 条会让第 7–30 个版本根本选不到；改为全量渲染 + 限高滚动，
                避免弹窗被撑高。 */}
            <Space direction="vertical" size={8} style={{ width: '100%', maxHeight: 340, overflowY: 'auto', paddingRight: 4 }}>
              {versions.map((h, i) => (
                <Radio
                  key={`${h.image}-${i}`}
                  value={h.image}
                  disabled={h.image === rollbackTarget?.image}
                  style={{
                    width: '100%',
                    border: rollbackTo === h.image ? '1px solid #ffccc7' : '1px solid #eceef2',
                    background: rollbackTo === h.image ? '#fff2f0' : '#fff',
                    borderRadius: 6,
                    padding: '10px 12px',
                    marginInlineEnd: 0,
                  }}
                >
                  <div style={{ paddingLeft: 4 }}>
                    <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
                      <span className="yn-mono" style={{ fontSize: 12.5 }}>
                        {h.tag}
                      </span>
                      {h.image === rollbackTarget?.image ? <Tag>当前版本</Tag> : i === 0 ? <Tag color="green">最近一次部署</Tag> : null}
                      <Tag color={h.status === 'success' ? 'default' : 'error'}>{h.status === 'success' ? '部署成功' : '部署失败'}</Tag>
                    </div>
                    <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 3 }}>
                      部署于 {h.deployedAt} · 操作人 {h.operator || '—'}
                      {h.note ? ` · ${h.note}` : ''}
                    </div>
                  </div>
                </Radio>
              ))}
            </Space>
          </Radio.Group>
        ) : (
          <Alert type="info" showIcon message="该服务没有可回滚的历史版本" description="只有一次部署记录，没有第二个版本可退。" />
        )}

        <div style={{ marginTop: 16 }}>
          <Text type="secondary" style={{ fontSize: 13 }}>
            回滚原因（必填，将记录到审计日志）
          </Text>
          <Input.TextArea
            rows={2}
            style={{ marginTop: 6 }}
            placeholder="如：新版本下单接口 P99 耗时上升，人工介入回滚"
            value={rollbackReason}
            onChange={(e) => setRollbackReason(e.target.value)}
          />
        </div>
      </Modal>

      {/* ================================================== 设置参数 */}
      <Drawer
        open={paramOpen}
        onClose={() => setParamOpen(false)}
        width={680}
        title={
          <Space>
            <SettingOutlined />
            设置参数 · {paramTarget?.projectKey}
          </Space>
        }
        extra={
          <Space>
            <Button onClick={() => setParamOpen(false)}>取消</Button>
            <Button type="primary" loading={paramSaving} onClick={submitParams}>
              保存并生效
            </Button>
          </Space>
        }
        destroyOnClose
      >
        <Form form={paramForm} layout="vertical" requiredMark="optional">
          <Divider orientation="left" plain style={{ marginTop: 0 }}>
            资源配置
          </Divider>
          <Row gutter={16}>
            <Col span={8}>
              <Form.Item name="replicas" label="副本数">
                <InputNumber min={0} max={200} style={{ width: '100%' }} />
              </Form.Item>
            </Col>
            <Col span={8}>
              <Form.Item name="cpuRequest" label="CPU Request">
                <Input placeholder="如 100m" />
              </Form.Item>
            </Col>
            <Col span={8}>
              <Form.Item name="cpuLimit" label="CPU Limit">
                <Input placeholder="如 500m" />
              </Form.Item>
            </Col>
            <Col span={8}>
              <Form.Item name="memRequest" label="内存 Request">
                <Input placeholder="如 128Mi" />
              </Form.Item>
            </Col>
            <Col span={8}>
              <Form.Item name="memLimit" label="内存 Limit">
                <Input placeholder="如 512Mi" />
              </Form.Item>
            </Col>
          </Row>

          <Divider orientation="left" plain>
            环境变量
          </Divider>
          {/* 注意：外层 Form.Item **不能带 name** —— KVFields 内部已经是 Form.List name="envVars"，
              再套一层同名的 Form.Item 会让两个 field 争抢同一个数据路径，取值会互相覆盖。 */}
          <Form.Item>
            <KVFields name="envVars" addText="添加环境变量" />
          </Form.Item>

          <Divider orientation="left" plain>
            配置项与密钥
          </Divider>
          <Row gutter={16}>
            <Col span={12}>
              <Form.Item name="configMaps" label="ConfigMap">
                <Select mode="tags" placeholder="输入后回车添加" />
              </Form.Item>
            </Col>
            <Col span={12}>
              <Form.Item name="secrets" label="Secret">
                <Select mode="tags" placeholder="输入后回车添加" />
              </Form.Item>
            </Col>
          </Row>

          <Divider orientation="left" plain>
            变更说明
          </Divider>
          <Form.Item name="note" label="本次变更原因" tooltip="会写入 deploy_param 的参数快照，便于日后追溯">
            <Input.TextArea rows={2} placeholder="如：双十一前扩容至 8 副本" maxLength={255} showCount />
          </Form.Item>

          {/* 口径与后端一致：PUT /services/:id/params 只把参数落库（并写一份 deploy_param 快照），
              容器里的环境变量与资源限制要等下次滚动更新才生效——接口返回 applyRequiresRestart=true
              （handler_cd.go updateServiceParams）就是为了说清这一点。参数历史只有查询接口
              （GET /services/:id/params/history），**没有回退接口**，所以不能承诺"可回退"。 */}
          <Alert
            type="info"
            showIcon
            message="参数变更会保留历史版本"
            description="每次保存都会记录操作人与变更内容，可在参数历史中查询与对比；参数只写入平台记录，需下次部署 / 重启 Deployment 后才会应用（保存接口返回 applyRequiresRestart = true）。"
          />
        </Form>
      </Drawer>

      {/* ================================================== 扩缩容 */}
      <Modal
        open={scaleOpen}
        title={`扩缩容 · ${scaleTarget?.projectKey}`}
        onCancel={() => setScaleOpen(false)}
        onOk={submitScale}
        confirmLoading={scaleRunning}
        okText="确认调整"
        okButtonProps={{ disabled: scaleValue === null }}
        destroyOnClose
      >
        <div style={{ fontSize: 13, color: '#5c6470', marginBottom: 14 }}>
          {scaleLive ? (
            <>
              当前副本数 <b>{scaleLive.replicas}</b>，就绪 <b>{scaleLive.readyReplicas}</b>。
            </>
          ) : (
            <>
              读取不到集群当前副本数：{scaleTarget ? liveFallbackText(scaleTarget) : '—'}
              。请确认目标副本数后再提交。
            </>
          )}
        </div>
        <Space size={16} align="center">
          <Text type="secondary" style={{ fontSize: 13 }}>
            目标副本数
          </Text>
          <InputNumber min={0} max={200} value={scaleValue} onChange={(v) => setScaleValue(v ?? null)} />
          <Space size={4}>
            {[1, 2, 3, 5, 8].map((n) => (
              <Button key={n} size="small" onClick={() => setScaleValue(n)}>
                {n}
              </Button>
            ))}
          </Space>
        </Space>
        <div style={{ marginTop: 16 }}>
          {/* 原来的 Progress 用 percent=(副本数/20)*100 展示，分母 20 与输入上限 200 不一致、
              与后端也没有对应关系（POST /services/:id/scale 只是把副本数下发给集群），
              纯装饰性进度条已删除；「预计 30 秒内完成」同样是无源 ETA，一并去掉。 */}
          <Text type="secondary" style={{ fontSize: 12 }}>
            {scaleValue === null
              ? '填写目标副本数后会提示这是扩容还是缩容'
              : scaleValue > (scaleLive?.replicas ?? 0)
                ? '扩容将新增 Pod'
                : '缩容会先摘除流量再销毁 Pod'}
          </Text>
        </div>
        <div style={{ marginTop: 16 }}>
          <Text type="secondary" style={{ fontSize: 13 }}>
            变更原因（可选）
          </Text>
          <Input
            style={{ marginTop: 6 }}
            placeholder="如：大促前扩容"
            value={scaleReason}
            onChange={(e) => setScaleReason(e.target.value)}
            maxLength={255}
          />
        </div>
      </Modal>

      {/* ================================================== 环境变量编辑 */}
      <Drawer
        open={varsOpen}
        onClose={() => setVarsOpen(false)}
        width={640}
        title={`编辑环境变量 · ${env.name}`}
        extra={
          <Space>
            <Button onClick={() => setVarsOpen(false)}>取消</Button>
            <Button type="primary" loading={varsSaving} onClick={submitVars}>
              保存
            </Button>
          </Space>
        }
        destroyOnClose
      >
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 16 }}
          message="覆盖式保存"
          description="提交后以当前列表为准整体覆盖该环境的变量。密文变量只回显掩码，未改动时保持原值即可。"
        />
        <Form form={varsForm} layout="vertical">
          {/* 外层不加 name，原因同上：KVFields 自带 Form.List name="vars" */}
          <Form.Item>
            <KVFields name="vars" addText="添加环境变量" keyPlaceholder="如 DB_HOST" />
          </Form.Item>
        </Form>
      </Drawer>

      {/* ------------------------------------------------------ 纳管项目 */}
      {/* 与列表页「编辑 › 纳管项目」写的是同一份数据（后端同一个 replaceEnvProjects），
          这里只是把入口提到台面上。 */}
      <Modal
        open={linkOpen}
        title={`纳管项目 · ${env.name}`}
        width={560}
        onCancel={() => setLinkOpen(false)}
        okText="保存"
        confirmLoading={linkSaving}
        onOk={submitLink}
        destroyOnClose
      >
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message="取消勾选会同时删除该环境下的服务实例"
          description="平台不再跟踪这些项目的服务，之后要重新纳管才能在本环境发布；集群里已运行的负载不会自动下线，部署历史也保留。"
        />
        <div style={{ fontSize: 13, color: '#5c6470', marginBottom: 8 }}>
          只有纳管进来的项目，才能在本环境部署 —— 拓扑上的圆圈、部署向导的项目下拉都取自这里。
        </div>
        <Select
          mode="multiple"
          style={{ width: '100%' }}
          placeholder="选择要接入本环境的项目"
          loading={candApi.loading}
          optionFilterProp="label"
          value={linkKeys}
          onChange={setLinkKeys}
          options={candidates.map((c) => ({
            label: `${c.name}（${c.key}）${c.hasBuild ? '' : ' · 无镜像产物'}`,
            value: c.key,
          }))}
        />
        {candApi.error ? (
          <div style={{ color: '#ff4d4f', fontSize: 12, marginTop: 8 }}>
            项目候选加载失败：{candApi.error.message}
          </div>
        ) : null}
      </Modal>

      {/* 环境级编辑：与列表页同一份表单与同一套提交口径 */}
      <EnvironmentEditDrawer
        open={envEditOpen}
        target={env}
        clusterValuesInUse={clusterValuesInUse}
        onClose={() => setEnvEditOpen(false)}
        onSaved={refreshAll}
      />
    </div>
  );
};

const Statistic: React.FC<{ label: string; value: React.ReactNode }> = ({ label, value }) => (
  <div>
    <div style={{ fontSize: 12, color: '#9aa0aa' }}>{label}</div>
    <div style={{ fontSize: 18, fontWeight: 600, color: '#2c313a', marginTop: 2 }}>{value}</div>
  </div>
);

export default EnvironmentDetail;
