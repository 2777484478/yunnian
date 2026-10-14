import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  Alert,
  App as AntApp,
  AutoComplete,
  Button,
  Descriptions,
  Drawer,
  Empty,
  Form,
  Input,
  InputNumber,
  Modal,
  Progress,
  Segmented,
  Select,
  Space,
  Switch,
  Tag,
  Timeline,
  Tooltip,
  Typography,
} from 'antd';
import type { FormInstance } from 'antd';
import {
  AppstoreOutlined,
  ClusterOutlined,
  CloudServerOutlined,
  ExclamationCircleFilled,
  FileTextOutlined,
  GithubOutlined,
  HistoryOutlined,
  NodeIndexOutlined,
  ReloadOutlined,
  RocketOutlined,
  RollbackOutlined,
  SettingOutlined,
  SlidersOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons';
import { useNavigate, useSearchParams } from 'react-router-dom';
import { PageHeader, SectionCard } from '@/components/ui';
import { LogPanel } from '@/components/LogPanel';
import BatchDeployWizard from '@/components/BatchDeployWizard';
import { STATE_STYLE, TopoLink, TopoNode } from '@/components/TopoNode';
import type { NodeState } from '@/components/TopoNode';
import {
  DEPLOY_METHOD_LABEL,
  clusterBadgeOf,
  clusterKeyOf,
  gateOf,
  isOwnCluster,
  sortByStage,
  stageOf,
  versionViewOf,
} from '@/config/topology';
import { ApiError, clusterApi, deployApi, environmentApi, imageApi, projectApi, rollbackApi, serviceApi } from '@/api';
import type { DeployCreated } from '@/api';
import { canDeployOn, canRollbackOn, hasPerm } from '@/auth';
import AutoRefreshButton from '@/components/AutoRefreshButton';
import { useApi } from '@/hooks/useApi';
import { useAutoRefresh, useAutoRefreshPref } from '@/hooks/useAutoRefresh';
import { useLogStream } from '@/hooks/useLogStream';
import { liveFallbackText, liveReplicaTotal, liveState, liveStatusWord } from '@/utils/live';
import type {
  DeployRecord,
  DeployVersion,
  Environment,
  EnvService,
  ImageRecord,
  K8sCluster,
  Project,
  ServiceStatus,
} from '@/types';

const { Text } = Typography;

// --------------------------------------------------------------------------- 类型

/** 某个项目在某个环境的运行态（未部署时是 EMPTY_RT） */
type Runtime = {
  tag: string;
  image: string;
  /** 期望副本数（`spec.replicas`）。表单预填"当前副本数"用的是它 —— 那是"想要几个"。 */
  replicas: number;
  /** 就绪数。 */
  ready: number;
  /**
   * **实例总数**（`status.replicas`，含正在崩溃、还没被回收的那些）。
   *
   * 圆圈上显示 `就绪/实例` 用的是它而不是 `replicas`：滚动卡住时集群里会多出一个
   * 反复崩溃的实例，只按期望显示就会写成 `1/1`，把"有一个实例在崩"抹掉 ——
   * 用户在自己的运维面板里看到的是 `1/2`。口径见 `utils/live.ts` 的 `liveReplicaTotal`。
   */
  total: number;
  status: ServiceStatus | 'undeployed' | 'unknown';
  progress: number;
};

type ActionType =
  | 'deploy'
  | 'redeploy'
  | 'rollback'
  | 'logs'
  | 'podlog'
  | 'scale'
  | 'params'
  | 'offline';

const EMPTY_RT: Runtime = {
  tag: '',
  image: '',
  replicas: 0,
  ready: 0,
  total: 0,
  status: 'undeployed',
  progress: 0,
};

/**
 * 圆圈要显示的值：**优先取集群实时状态**。
 *
 * 平台库里的 replicas / status / tag 是"平台期望"——纳管时按默认值写入，
 * 之后只有部署成功才更新，直接拿来显示就会出现"平台说 2/2、集群里只有 1/1"。
 *
 * 读不到集群时**不回退到库里的值**：那正是要消掉的假数据。
 * 改为 unknown，让圆圈如实说"状态未知"。
 */
const runtimeOf = (svc?: EnvService): Runtime => {
  if (!svc) return EMPTY_RT;
  const live = svc.live;
  // ① 读到了：字段即事实
  if (live && live.available && live.exists) {
    return {
      tag: live.tag,
      image: live.image,
      replicas: live.replicas,
      ready: live.readyReplicas,
      // 圆圈上的 `就绪/实例` 用这个（见 Runtime.total 的说明）。
      total: liveReplicaTotal(live),
      // 状态词转换与「环境详情」表格共用同一个函数（@/utils/live），
      // 两处各写一套迟早会出现"圆圈说运行中、表格说异常"。
      status: liveStatusWord(live.status),
      progress: live.status === 'progressing' ? 60 : 0,
    };
  }
  // ② 确认集群里没有这个工作负载 —— 说"未部署"是准确的
  if (live && live.available && !live.exists) return EMPTY_RT;
  // ③ 没读到（未接入 / 权限 / 网络）—— 不能假装成"未部署"
  return { ...EMPTY_RT, status: 'unknown' };
};

/** 该节点是否算"已经部署过"（用于顶部统计与分组计数）。未知不算。 */
const isDeployed = (n: Runtime) => n.status !== 'undeployed' && n.status !== 'unknown';

/**
 * 部署单在「拓扑 · 部署记录」时间线里的三件套：圆点色 / 标签色 / 标签文案。
 *
 * 三个值必须**同时**认得出同一个状态词，所以集中在一处 —— 分开写迟早出现
 * "圆点说中断、标签说进行中"这种自相矛盾的界面。
 *
 * 原先这里只认 failed / success 两档，其余全走蓝色「进行中」的兜底分支，
 * 于是两类单子被说成了假话：
 *   - `interrupted`（平台重启打断，这条记录永远不会再动）→ 显示「进行中」；
 *   - `pending_approval`（根本还没开始跑，审批人可能明天才点）→ 显示「进行中」。
 * 后者不是偶发错误行：待审批是常态，也就是说这个时间线一直在把"没开始"说成"在跑"。
 */
const RECORD_STATUS: Record<string, { dot: string; tag: string; text: string }> = {
  pending_approval: { dot: 'gray', tag: 'gold', text: '待审批' },
  queued: { dot: 'blue', tag: 'default', text: '排队中' },
  progressing: { dot: 'blue', tag: 'processing', text: '部署中' },
  success: { dot: 'green', tag: 'success', text: '成功' },
  failed: { dot: 'red', tag: 'error', text: '失败' },
  rolled_back: { dot: 'purple', tag: 'purple', text: '已回滚' },
  canceled: { dot: 'gray', tag: 'default', text: '已取消' },
  interrupted: { dot: '#fa8c16', tag: 'orange', text: '已中断' },
};

/** 认不出的状态词**原样显示**，不兜底成"进行中"——兜底等于替后端猜一个状态。 */
const recordStatus = (s: string) => RECORD_STATUS[s] ?? { dot: 'blue', tag: 'default', text: s || '—' };

/**
 * 时间线上的耗时文案。
 *
 * ⚠️ 原来的写法是 `d.duration ? … : '进行中'`：只要 duration 为 0 就说"进行中"。
 * 中断单的耗时可能确实是 0（重启太快，还没来得及记录），于是一条**已经终结**的单子
 * 又被说成在跑。只有真正处于 live 状态才配得上"进行中"，其余一律如实写 `—`。
 */
const recordDurationText = (d: DeployRecord): string => {
  if (d.duration) return `${d.duration}s`;
  if (d.status === 'progressing' || d.status === 'queued') return '进行中';
  return '—';
};

/**
 * 表单里"当前副本数"的预填值。
 *
 * **读不到集群时不给数字**。早期一律 `n.replicas || 2`，而未知态的 replicas 恒为 0
 * → 表单预填 2。用户只是想换个版本，一点提交副本数就被从 5 悄悄改成 2
 * （后端按提交值执行，不会"保持原样"）。宁可留空要求用户确认，也不给编出来的默认值。
 */
const prefilledReplicas = (n: Runtime): number | undefined =>
  n.status === 'unknown' || n.status === 'undeployed' || !n.replicas ? undefined : n.replicas;

const runtimeState = (n: Runtime): NodeState => {
  // unknown 的判断必须在最前：它是"平台不知道"，与"未部署"（确认没有）不是一回事。
  if (n.status === 'unknown') return 'unknown';
  if (n.status === 'undeployed') return 'undeployed';
  if (n.status === 'deploying') return 'deploying';
  if (n.status === 'error' || n.status === 'stopped') return 'error';
  if (n.total && n.ready < n.total) return 'warning';
  return 'healthy';
};

const LEGEND: { state: NodeState; label: string }[] = [
  { state: 'healthy', label: '运行正常' },
  { state: 'deploying', label: '部署中' },
  { state: 'warning', label: '副本未就绪' },
  { state: 'error', label: '运行异常' },
  { state: 'undeployed', label: '未部署' },
  { state: 'unknown', label: '状态未知' },
];

/** 正在执行的一次编排（部署 / 重新部署 / 回滚），用于驱动圆圈动画与日志面板 */
interface Flow {
  envKey: string;
  /** SSE 地址 */
  url: string;
  /** 面板标题 */
  title: string;
  /** 目标版本（用于圆圈上立刻显示"要变成什么版本"） */
  tag: string;
  image: string;
  replicas: number;
  /** 成功后提示语 */
  doneText: string;
}

// --------------------------------------------------------------------------- 环境实际连的集群

// isOwnCluster 已提到 `@/config/topology`（环境列表 / 环境详情 / 发布拓扑三处共用）。

/** 把一组值收敛成"去重后的唯一值"，用于按组汇总时如实说明"有几个不同的值"。 */
const uniqOf = <T,>(list: readonly T[]): T[] => [...new Set(list)];

/** 环境实际连的**那一个**集群的展示信息，见 `envClusterView`。 */
interface EnvClusterView {
  /** 是否自带 kubeconfig */
  own: boolean;
  /** 平台注册表记录。**只有 own=false 时才可信** —— 见 `envClusterView` 的注释 */
  reg?: K8sCluster;
  /** K8s 版本（取不到为空串） */
  version: string;
  /** 节点数（取不到为 null） */
  nodes: number | null;
  /** 集群角标文案 */
  badge: string;
}

// --------------------------------------------------------------------------- 回滚目标版本

/**
 * 回滚目标版本选择器。
 *
 * **为什么必须单独成一个组件、并由调用方用 `key={serviceId}` 挂载**：
 * `useApi` 在依赖变化时刻意**不清空旧 `data`**（那是为"轮询静默刷新"设计的，
 * 见 `hooks/useApi.ts` 的注释）。常驻的 hook 实例于是会出现
 * 「`options` 是当前服务的、`value` 却是上一个服务的」，而下拉里还残留上一个服务的
 * option 节点 —— 最糟的不是难看，是那个 `value` 会被 `submitRollback` 原样发给后端。
 * 用 `key` 重挂载后，换服务＝全新实例：`data` 从 `null` 起步，预选只可能落在
 * **本服务的数据**上；`Select` 连同它的下拉 portal 一起重建，不残留旧节点。
 */
const RollbackTargetField: React.FC<{
  serviceId?: string;
  /** 平台库里的当前版本（后端判"回滚到当前版本是无操作"用的就是它，必须同口径） */
  currentImage: string;
  form: FormInstance;
  /**
   * 把"有没有可选目标"报给调用方，用来禁用「确认回滚」。
   *
   * 这个状态只有本组件知道（它才持有版本列表），而禁用的是 Modal 的 footer 按钮，
   * 所以必须往上提一格。**不能只看"表单里有没有值"**：无可选目标时预选会落到
   * "当前版本"上，表单照样有值、必填规则照样过，按钮也就照样能点 ——
   * 而后端 `CreateRollback` 在 `target.Image == svc.CurrentImage` 时必定拒
   * （`目标版本与当前版本相同，无需回滚`）。留着可点就是"能勾选但后端必然拒"。
   */
  onNoTargetChange?: (noTarget: boolean) => void;
}> = ({ serviceId, currentImage, form, onNoTargetChange }) => {
  const versionsApi = useApi(() => serviceApi.versions(serviceId!), [serviceId], {
    enabled: !!serviceId,
  });

  // 默认选第一个「与当前版本不同」的版本（回滚到当前版本是空操作）。
  // 本组件的 data 一定属于本服务，所以这里不需要再防串台，只需要防覆盖用户的选择。
  useEffect(() => {
    const list = versionsApi.data;
    if (!list) return;
    if (form.getFieldValue('image')) return;
    const first = list.find((v) => v.image !== currentImage) ?? list[0];
    if (first) form.setFieldsValue({ image: first.image });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [versionsApi.data]);

  const list: DeployVersion[] = versionsApi.data ?? [];
  /**
   * 该服务的部署记录**全是同一个 tag**（= 只部署过一个版本）。
   *
   * 这时的"选不中"是对的：后端默认回滚分支查的是 `image <> currentImage`，
   * 同样查不到、会返 `StateConflict("找不到可回滚的历史版本")`。
   * 但只给一列灰条，用户得到的信息是"坏了"，而不是"这个服务没有第二个版本"——
   * 所以要把话说出来（原先 9 条全灰却不解释，是这一处最大的问题）。
   */
  const noTarget = list.length > 0 && list.every((v) => v.image === currentImage);

  // 报给调用方（禁用「确认回滚」）。`onNoTargetChange` 在父级用 `useCallback` 固定，
  // 所以这个 effect 只在 noTarget 真的变化时跑，不会来回抖。
  useEffect(() => {
    onNoTargetChange?.(noTarget);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [noTarget]);

  return (
    <Form.Item
      name="image"
      label="目标版本"
      rules={[{ required: true, message: '请选择要回滚到的版本' }]}
      extra={noTarget ? '该服务只部署过这一个版本，没有可回退的历史目标。' : undefined}
    >
      <Select
        placeholder="从历史部署版本中选择"
        loading={versionsApi.loading}
        notFoundContent={
          versionsApi.loading ? '加载中…' : noTarget ? '没有可回退的历史版本' : '该服务没有历史版本可回滚'
        }
        options={list.map((h, i) => ({
          value: h.image,
          disabled: h.image === currentImage,
          label: `${h.tag}  ·  ${(h.deployedAt || '').slice(5, 16)}  ·  ${h.operator || '—'}${
            h.image === currentImage ? '（当前版本）' : h.status === 'failed' ? '（上次失败）' : i === 0 ? '（最近一次）' : ''
          }`,
        }))}
      />
    </Form.Item>
  );
};

// --------------------------------------------------------------------------- 页面

/** 上次在发布拓扑里选过的项目（key）。命名与 `yn.autorefresh.*` 保持一致。 */
const TOPO_PROJECT_KEY = 'yn.topo.project';

/** 读上次选过的项目。存储不可用（隐私模式）时返回空串，由调用方兜底。 */
function readLastProject(): string {
  try {
    return localStorage.getItem(TOPO_PROJECT_KEY) ?? '';
  } catch {
    return '';
  }
}

const Topology: React.FC = () => {
  const { message } = AntApp.useApp();
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();

  /**
   * 把后端的拒绝理由如实展示给用户。
   *
   * 与 EnvironmentDetail 同因：client.ts 抛 ApiError 但自己不弹提示，不 catch
   * 就是一个未处理的 rejection —— 圆圈菜单点了没反应、也不说为什么。
   * 而这些接口的拒绝理由很具体（HPA 接管 / 正在部署中 / 集群里没有这个工作负载 /
   * 该项目在本环境尚未纳管），丢掉就等于让用户反复试同一个必然失败的操作。
   */
  const fail = (e: unknown, fallback: string) =>
    message.error(e instanceof ApiError ? e.message : fallback);

  const [projectKey, setProjectKey] = useState<string>('');
  const [layout, setLayout] = useState<'pipeline' | 'cluster'>('pipeline');
  const [activeEnv, setActiveEnv] = useState<string | null>(null);
  const [action, setAction] = useState<{ type: ActionType; envKey: string } | null>(null);
  /**
   * 回滚弹窗里"没有任何可回退的历史目标"（该服务只部署过一个 tag）。
   *
   * 由 `RollbackTargetField` 上报（只有它持有版本列表），用来把「确认回滚」置灰 ——
   * 否则表单里有"当前版本"这个值、必填也过，按钮能点，而后端必定拒。
   */
  const [rollbackNoTarget, setRollbackNoTarget] = useState(false);
  const onRollbackNoTargetChange = useCallback((v: boolean) => setRollbackNoTarget(v), []);
  const [batchOpen, setBatchOpen] = useState(false);
  const [flowOpen, setFlowOpen] = useState(false);
  const [flow, setFlow] = useState<Flow | null>(null);
  const canvasRef = useRef<HTMLDivElement>(null);
  const [scrollHint, setScrollHint] = useState({ left: false, right: false });

  const [deployForm] = Form.useForm();
  const [rollbackForm] = Form.useForm();
  const [scaleForm] = Form.useForm();
  const [syncing, setSyncing] = useState(false);

  // ------------------------------------------------------------------ 取数
  const projectsApi = useApi(() => projectApi.list({ pageSize: 200 }), []);
  const projects: Project[] = useMemo(() => projectsApi.data?.list ?? [], [projectsApi.data]);

  // 默认选中第一个项目（而不是硬编码某个 key —— 真实数据里那个 key 不一定存在）
  /**
   * 选中的项目要**记得住**。
   *
   * 原先每次进这一页（或刷新）都弹回 `projects[0]` —— 发布完回来想看刚才那个服务，
   * 又被甩到默认项目上，得再选一次。项目是这一页的**主语**，丢了主语等于丢了上下文。
   *
   * 三个来源，优先级从高到低：
   *   1. URL 的 `?project=`：可分享、可刷新、从别处跳进来能带上下文；
   *   2. 上次选过的（localStorage）：日常"发完回来看一眼"走这条；
   *   3. 第一个项目：兜底，且**必须校验 key 仍然存在**（项目可能刚被删）。
   */
  /** 把 URL 里的 `?project=` 对齐到 `key`（replace，不堆历史 —— 翻项目不该让"返回"按好几次）。 */
  const syncUrlProject = (key: string) => {
    if ((searchParams.get('project') ?? '') === key) return;
    const next = new URLSearchParams(searchParams);
    if (key) next.set('project', key);
    else next.delete('project');
    setSearchParams(next, { replace: true });
  };

  const pickProject = (key: string) => {
    setProjectKey(key);
    try {
      localStorage.setItem(TOPO_PROJECT_KEY, key);
    } catch {
      // 隐私模式 / 存储被禁：记不住就不记，不影响这一页能用
    }
    syncUrlProject(key);
  };

  /**
   * 决定"现在该看哪个项目"，并把 URL 对齐过去。
   *
   * ⚠️ 必须监听 `urlProject`：HashRouter 下**只改 hash 不会重新挂载组件**，
   * 如果只在"没选过"时才读 URL，那么"已经在拓扑页、又点了一个带 ?project= 的链接"
   * 就会被静默忽略（实测：地址栏换了参数，选中的项目原地不动）。
   */
  const urlProject = searchParams.get('project') ?? '';
  useEffect(() => {
    if (!projects.length) return;
    const known = (k: string) => !!k && projects.some((p) => p.key === k);
    const next = known(urlProject)
      ? urlProject
      : known(projectKey)
        ? projectKey // 已经选好了，URL 只是没跟上
        : known(readLastProject())
          ? readLastProject()
          : projects[0].key;
    if (next !== projectKey) {
      pickProject(next);
      return;
    }
    // 走到这里说明选中的就是它，但 URL 里可能是个失效/不存在的 key —— 顺手纠正，
    // 否则地址栏会一直显示一个平台根本不认识的项目（分享出去就是坏链接）。
    if (urlProject !== next) syncUrlProject(next);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projects, urlProject, projectKey]);

  const envsApi = useApi(() => environmentApi.list({ pageSize: 100 }), []);
  const envList = useMemo(() => sortByStage(envsApi.data?.list ?? []), [envsApi.data]);

  // 已接入的集群。**集群信息不再由前端编造** —— 原先拓扑上的云厂商/区域/可用区
  // 是 config/topology.ts 里写死的三条，而 kubeconfig 根本给不出这些字段。
  // 现在只显示平台真正读得到的：标识 / K8s 版本 / 节点数 / 连通状态；
  // 环境里填的集群标识对不上任何已接入集群时，如实标"未接入"，而不是编一个厂商名。
  const clustersApi = useApi(() => clusterApi.list(), []);
  const clusterByKey = useMemo(() => {
    const m = new Map<string, K8sCluster>();
    (clustersApi.data ?? []).forEach((c) => m.set(clusterKeyOf(c.key), c));
    return m;
  }, [clustersApi.data]);

  /**
   * 环境 → **它实际连的那个集群**的信息。
   *
   * 原先这里只有 `clusterOfEnv(env.cluster)`：拿环境里填的**字符串标识**去平台集群注册表
   * 匹配一条记录，然后把那条记录的 K8s 版本 / 节点数 / 连通状态当成这个环境的集群。
   * 对环境**自带 kubeconfig** 的形态，这个前提是错的 —— 后端 `clusterRefOf` 在这种情况下
   * **绕过平台集群表**、直接用环境自己那份凭据（那句注释的原话：
   * 「自带凭据的环境会被拿平台集群的凭据去读——拿到的是另一个集群的数据，而且看不出错」）。
   * 也就是说 `env.cluster` 此时只是个**名字标签**，注册表里同名的记录是**另一个集群**。
   *
   * 实测（2026-10-09）：环境 `test-152` 自带凭据、实际集群 v1.23.10，拓扑却显示注册表里
   * `cluster-local` 的 v1.22.12 —— 环境管理页显示的 v1.23.10 才是对的（那是后端
   * `SyncEnvironment` 从**它自己那份凭据**指向的集群读回来写进 `environment.cluster_version`
   * 的）。版本号只是最显眼的一处：同一份错数据还被用来显示节点数、命名空间数与
   * 「已接入 / 未接入」角标 —— **对象整个是错的**。
   *
   * 所以：自带凭据时**只认环境自己的同步结果**，一个字段都不向注册表借；
   * 引用平台集群时照旧用注册表（那才是有凭据、真探过的那一条）。
   */
  const envClusterView = (env: Environment): EnvClusterView => {
    if (isOwnCluster(env)) {
      // `lastSyncAt` 为空 = 从来没同步过这个环境，集群值不是"没有"而是"还没问过"。
      // 与 `SyncEnvironment` 一起写入的 `cluster_version` / `node_count` 是同一批数据，
      // 所以拿它当"读过这个集群没有"的判据。读到之前一律显示"—"，不拿 0 充数。
      const synced = !!env.lastSyncAt;
      return {
        own: true,
        version: synced ? env.clusterVersion || '' : '',
        nodes: synced && typeof env.nodes === 'number' ? env.nodes : null,
        // 角标**不能**沿用「已接入 / 未接入」：那问的是"这个集群在平台注册表里吗"，
        // 而自带凭据的环境压根不走平台集群接入 —— 回答这个问题本身就是答错了对象。
        badge: '自带凭据',
      };
    }
    const reg = clusterByKey.get(clusterKeyOf(env.cluster));
    return {
      own: false,
      reg,
      version: reg?.k8sVersion ?? '',
      nodes: typeof reg?.nodes === 'number' ? reg.nodes : null,
      badge: clusterRegistryKnown ? clusterBadgeOf(reg?.status) : '未知',
    };
  };

  /**
   * 集群注册表到底读到没有 —— 只区分「读到了（可能是空表）」与「没读到」。
   *
   * 为什么必须区分：`未接入` 是一句**断言**（"这些集群确实没在平台里注册"），
   * 但把 `GET /clusters` 请求失败与"注册表为空"混为一谈时，这句断言是没有依据的
   * —— 我们只是没读到，不是读到了空。
   *
   * 这一个区别现在尤其要紧：/clusters 尚未实现，**整页环境全部处于"没读到"的状态**，
   * 若不区分，用户看到的 5 个"未接入"会被理解成"这 5 个集群真的没接入"，
   * 与事实（集群接入功能还没有后端）相反。
   */
  const clusterRegistryKnown = !clustersApi.error;

  /** 集群角标：读不到注册表时不冒充结论，显示中性值 */
  const badgeOf = (cluster?: K8sCluster): string =>
    clusterRegistryKnown ? clusterBadgeOf(cluster?.status) : '未知';

  // 本页**不再有"新建环境"的入口**（用户 2026-09-29 定）：环境的唯一写入口是
  // 「CD 环境部署 › 环境管理」，拓扑只负责"看"。原先这里各自维护一套建环境表单，
  // 字段很快就与那边漂移了（这里多出发布阶段/链路序号/独立 kubeconfig，那边则多出
  // 纳管项目/环境变量），同一个实体两套口径必然对不上。
  //

  // 每个环境下的服务实例。拓扑要同时看"所有环境 × 当前项目"，
  // 所以需要跨环境的服务清单；环境数量是个位数，逐个取比让后端加聚合接口更直接。
  const envKeys = useMemo(() => envList.map((e) => e.key).sort().join(','), [envList]);
  const servicesApi = useApi(
    async () => {
      const keys = envKeys ? envKeys.split(',') : [];
      const groups = await Promise.all(keys.map((k) => environmentApi.services(k)));
      return groups.flat();
    },
    [envKeys],
    { enabled: !!envKeys },
  );
  const allServices: EnvService[] = useMemo(() => servicesApi.data ?? [], [servicesApi.data]);

  /**
   * 服务实例清单到底读到没有 —— 与上面 `clusterRegistryKnown` 同一个道理。
   *
   * 「未纳管」是一句**断言**（"这个环境确实没关联这个项目"），只有真读到清单才配说。
   * 请求失败时 svcMap 是空的，若不加区分就会把全部环境的「部署新版」一起灰掉
   * —— 那是"没读到"，不是"没纳管"。（同款判据见 EnvironmentList 的 removeEnv）
   */
  const servicesKnown = !servicesApi.error && !servicesApi.loading;

  /** envKey → projectKey → 服务实例 */
  const svcMap = useMemo(() => {
    const m: Record<string, Record<string, EnvService>> = {};
    for (const s of allServices) {
      if (!m[s.envKey]) m[s.envKey] = {};
      m[s.envKey][s.projectKey] = s;
    }
    return m;
  }, [allServices]);

  const imagesApi = useApi(() => imageApi.list({ projectKey, pageSize: 50 }), [projectKey], {
    enabled: !!projectKey,
  });
  const images: ImageRecord[] = useMemo(() => imagesApi.data?.list ?? [], [imagesApi.data]);
  const latest = images[0];

  const project = useMemo(() => projects.find((p) => p.key === projectKey), [projects, projectKey]);

  const refreshAll = () => {
    projectsApi.refresh();
    envsApi.refresh();
    servicesApi.refresh();
    imagesApi.refresh();
  };

  /**
   * 自动刷新（默认每 5 秒）。
   *
   * 圆圈上的每个数字都来自**别处**：副本数与就绪数由 K8s 收敛、镜像 tag 由部署
   * 执行器推、集群角标由连通性探测更新。页面自己不会变 —— 不自动刷新，用户看到
   * 的就是一张"进来那一刻"的照片，得手动刷新才知道某个环境的实例是被拉起还是崩了。
   *
   * 走 refreshSilent 而不是 refresh：每 5 秒闪一次加载遮罩比不刷新更难受；后台那一拍
   * 失败也不会把整页打成错误态（否则一次网络抖动 = 拓扑消失），只在刷新控件里留一句原因。
   *
   * projectsApi **不参与轮询**：项目清单是纳管时写入的，不是运行态，几小时不变一次；
   * 每 5 秒拉 200 条去重渲染项目下拉，纯属白烧。用户改完项目从这里跳走再回来即可。
   *
   * 页面切到后台会暂停、切回来立刻补一次（见 useAutoRefresh）。
   */
  // scope='cd'：与「环境详情」共用一个键，但不共用 CI 项目列表那个全局键
  const [autoRefreshMs, setAutoRefreshMs] = useAutoRefreshPref(5000, 'cd');
  useAutoRefresh({
    enabled: autoRefreshMs !== null,
    intervalMs: autoRefreshMs ?? 5000,
    onTick: () => {
      envsApi.refreshSilent();
      // 拓扑是全环境的服务清单（N 个环境 = N 个请求），所以这里也更不该按 3 秒打
      servicesApi.refreshSilent();
      clustersApi.refreshSilent();
      imagesApi.refreshSilent();
    },
    // 任一请求还在飞就跳过这一拍，避免慢后端下滚出一串在飞请求
    isBusy: () =>
      envsApi.pendingRef.current ||
      servicesApi.pendingRef.current ||
      clustersApi.pendingRef.current ||
      imagesApi.pendingRef.current,
  });

  // ------------------------------------------------------------------ 编排流
  // 一次只允许一个编排在跑（同项目多环境并发发布不做：圆圈动画与日志面板都是单实例）。
  const stream = useLogStream(flow?.url ?? null, { enabled: !!flow });

  useEffect(() => {
    if (!flow || !stream.ended) return;
    const f = flow;
    setFlow(null);
    refreshAll();
    if (stream.endReason === 'finished') {
      message.success(f.doneText);
    } else {
      message.warning(`执行未成功（${stream.endReason}），请在部署记录中查看详情`);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [stream.ended]);

  /** 节点运行态：集群实时状态（读不到则为未知） + 正在进行的编排覆盖 */
  const nodeOf = (envKey: string): Runtime => {
    const svc = svcMap[envKey]?.[projectKey];
    const base = runtimeOf(svc);

    if (flow && flow.envKey === envKey && !stream.ended) {
      const p = stream.status?.progress ?? 0;
      return {
        tag: flow.tag || base.tag,
        image: flow.image || base.image,
        replicas: flow.replicas || base.replicas,
        ready: Math.floor(((flow.replicas || base.replicas) * p) / 100),
        // 部署中：按用户提交的期望数展示（此时集群里还没有那批新实例）。
        total: flow.replicas || base.total,
        status: 'deploying',
        progress: p,
      };
    }
    return base;
  };

  /** 该项目在某环境下的集群实时状态（未纳管、或接口未回时为空）。 */
  const liveOfEnv = (envKey: string) => svcMap[envKey]?.[projectKey]?.live ?? null;

  // ------------------------------------------------------------------ 画布滚动提示
  const syncScrollHint = () => {
    const el = canvasRef.current;
    if (!el) return;
    const left = el.scrollLeft > 4;
    const right = el.scrollLeft + el.clientWidth < el.scrollWidth - 4;
    setScrollHint((prev) => (prev.left === left && prev.right === right ? prev : { left, right }));
  };

  useEffect(() => {
    syncScrollHint();
    const onResize = () => syncScrollHint();
    window.addEventListener('resize', onResize);
    return () => window.removeEventListener('resize', onResize);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [layout, envList.length, projectKey, allServices.length]);

  const stats = useMemo(() => {
    const list = envList.map((e) => nodeOf(e.key));
    const deployed = list.filter(isDeployed).length;
    const bad = list.filter((n) => ['error', 'stopped'].includes(String(n.status))).length;
    const busy = list.filter((n) => n.status === 'deploying').length;
    return { deployed, bad, busy, total: envList.length };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [envList, svcMap, projectKey, flow, stream.status, stream.ended]);

  // ------------------------------------------------------------------ 操作入口
  const actionEnv = action ? envList.find((e) => e.key === action.envKey) : undefined;
  const actionSvc = action ? svcMap[action.envKey]?.[projectKey] : undefined;
  const actionRt = action ? nodeOf(action.envKey) : EMPTY_RT;
  // 抽屉里展示资源规格时同样只认集群实时值：同一次预览里"镜像"取自集群、
  // "资源"取自平台库，两处口径不一致必然对不上（改了规格没重新部署时最明显）。
  const actionLive = actionSvc ? liveState(actionSvc) : null;
  const actionResFallback = actionSvc ? liveFallbackText(actionSvc) : '—';

  const openAction = (type: ActionType, envKey: string) => {
    setActiveEnv(null);
    const n = nodeOf(envKey);
    if (type === 'deploy') {
      deployForm.setFieldsValue({
        imageRecordId: undefined,
        replicas: prefilledReplicas(n),
        note: '',
      });
    }
    if (type === 'rollback') {
      rollbackForm.setFieldsValue({ image: undefined, reason: '' });
    }
    if (type === 'scale') {
      scaleForm.setFieldsValue({ replicas: prefilledReplicas(n), reason: '' });
    }
    setAction({ type, envKey });
  };

  const closeAction = () => setAction(null);

  // 当前动作对应的服务实例 id（多数变更接口要它）
  const actionSvcId = actionSvc?.id;

  // ---------------------------------------------------------------- 变更类接口
  const startFlow = (f: Flow) => {
    setFlow(f);
    setFlowOpen(true);
  };

  const submitDeploy = async () => {
    const v = await deployForm.validateFields();
    if (!actionEnv) return;
    // 后端 `CreateDeployment` 第一步就是 `serviceByEnvProject(env.ID, proj.ID)`，
    // 查不到直接 `NotFoundf("该环境尚未纳管此项目：请先在环境详情里关联该项目")`，
    // **全链路没有任何"部署时自动建实例"的分支**（旧注释这么写过，是假的，已改）。
    // 所以"没纳管也能发"不成立：菜单那一侧必须同样置灰，见 renderNode 的 gate()。
    const img = images.find((i) => i.id === v.imageRecordId);
    if (!img) {
      message.warning('请选择要部署的镜像版本');
      return;
    }
    let created: DeployCreated;
    try {
      created = await deployApi.create({
        envKey: actionEnv.key,
        projectKey,
        imageRecordId: img.id,
        replicas: Number(v.replicas),
        strategy: 'RollingUpdate',
        remark: v.note,
        idempotencyKey: `topo-${actionEnv.key}-${projectKey}-${img.id}-${Date.now()}`,
      });
    } catch (e) {
      // 建单这一步最可能被拒：该环境尚未纳管此项目 / 已有进行中部署单 / 仓库里没有这个镜像。
      // 后端是唯一裁决者，它的原话必须原样送到用户眼前，且不能关掉弹窗把参数丢掉。
      fail(e, '发起部署失败');
      return;
    }
    closeAction();
    startFlow({
      envKey: actionEnv.key,
      url: deployApi.stream(created.id),
      title: `部署 ${projectKey} → ${actionEnv.name}`,
      tag: img.tag,
      image: img.image,
      replicas: Number(v.replicas),
      doneText: `${project?.name ?? projectKey} 已发布到${actionEnv.name}（${img.tag}）`,
    });
  };

  const submitRedeploy = async () => {
    if (!actionEnv || !actionSvcId) return;
    let created: DeployCreated;
    try {
      created = await serviceApi.redeploy(actionSvcId);
    } catch (e) {
      fail(e, '发起重新部署失败');
      return;
    }
    closeAction();
    startFlow({
      envKey: actionEnv.key,
      url: deployApi.stream(created.id),
      title: `重新部署 ${projectKey} → ${actionEnv.name}`,
      tag: actionRt.tag,
      image: actionRt.image,
      replicas: actionRt.replicas,
      doneText: `${actionEnv.name} 已按当前版本完成滚动重启`,
    });
  };

  const submitRollback = async () => {
    const v = await rollbackForm.validateFields();
    if (!actionEnv || !actionSvcId) return;
    let rb: Awaited<ReturnType<typeof rollbackApi.create>>;
    try {
      rb = await rollbackApi.create({
        serviceId: actionSvcId,
        toImage: v.image,
        reason: v.reason,
        triggerType: 'manual',
      });
    } catch (e) {
      fail(e, '发起回滚失败');
      return;
    }
    closeAction();
    startFlow({
      envKey: actionEnv.key,
      url: rollbackApi.stream(rb.id),
      title: `回滚 ${projectKey} → ${actionEnv.name}`,
      tag: String(v.image).split(':').pop() ?? '',
      image: v.image,
      replicas: actionRt.replicas || 1,
      doneText: `${actionEnv.name} 已回滚至 ${String(v.image).split(':').pop()}`,
    });
  };

  const submitScale = async () => {
    const v = await scaleForm.validateFields();
    if (!actionEnv || !actionSvcId) return;
    try {
      await serviceApi.scale(actionSvcId, Number(v.replicas), v.reason || undefined);
    } catch (e) {
      fail(e, '调整副本数失败');
      return; // 菜单与弹窗都留着，改一下就能重试
    }
    closeAction();
    message.success(`${actionEnv.name} 副本数已调整为 ${v.replicas}`);
    refreshAll();
  };

  const submitOffline = async () => {
    if (!actionEnv || !actionSvcId) return;
    try {
      await serviceApi.remove(actionSvcId);
    } catch (e) {
      fail(e, '取消纳管失败');
      return;
    }
    closeAction();
    message.success(
      `${project?.name ?? projectKey} 已从${actionEnv.name}取消纳管（集群里的服务未受影响）`,
    );
    refreshAll();
  };

  const syncAllClusters = async () => {
    if (!envList.length) return;
    setSyncing(true);
    const failed: string[] = [];
    for (const e of envList) {
      try {
        await environmentApi.sync(e.key);
      } catch {
        failed.push(e.name);
      }
    }
    setSyncing(false);
    refreshAll();
    if (failed.length) message.warning(`${failed.join('、')} 同步失败`);
    else message.success(`已同步 ${envList.length} 个环境的实时状态`);
  };

  // ---------------------------------------------------------------- 抽屉数据（按需）
  // 这三份数据只在对应抽屉打开时才请求，避免拓扑首屏打一堆无关接口。
  const timelineApi = useApi(
    () => deployApi.list({ envKey: action!.envKey, projectKey, pageSize: 8 }),
    [action?.envKey, projectKey],
    { enabled: action?.type === 'logs' && !!projectKey },
  );

  // 回滚目标版本的取数**不在这一层**：它必须随 `serviceId` 重新挂载，
  // 否则 `useApi` 不清旧 data 的特性会让上一个项目的版本串进来。
  // 见 `RollbackTargetField`（挂在回滚 Modal 里，带 `key`）。

  const paramHistApi = useApi(() => serviceApi.paramHistory(actionSvc!.id), [actionSvc?.id], {
    enabled: action?.type === 'params' && !!actionSvc,
  });

  const podsApi = useApi(() => serviceApi.pods(actionSvc!.id), [actionSvc?.id], {
    enabled: action?.type === 'podlog' && !!actionSvc,
  });
  const [podName, setPodName] = useState('');
  useEffect(() => {
    const list = podsApi.data ?? [];
    if (action?.type === 'podlog' && !podName && list.length) setPodName(list[0].name);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [podsApi.data, action]);

  const podLogUrl =
    action?.type === 'podlog' && actionSvc && podName ? serviceApi.podLogStream(actionSvc.id, podName, 500) : null;
  const podLogs = useLogStream(podLogUrl, { enabled: !!podLogUrl });

  // 回滚目标版本的**预选**也随组件走（见 RollbackTargetField）：
  // 留在这里就会用"上一个项目的旧 data"先在 `action?.type` 变化时跑一次，
  // 把错的值写进表单，而真数据回来时又被 `getFieldValue('image')` 的守卫挡掉。

  // ------------------------------------------------------------------ 节点渲染
  const renderNode = (env: Environment, index: number) => {
    const n = nodeOf(env.key);
    const st = runtimeState(n);
    const vv = versionViewOf(n.tag);
    // 这个环境**实际连的**集群（自带凭据时不是注册表里那条同名记录），见 envClusterView。
    const cv = envClusterView(env);
    const stage = stageOf(env);
    const undeployed = st === 'undeployed';
    const disabledItem = 'yn-topo-menu__item yn-topo-menu__item--disabled';
    const item = 'yn-topo-menu__item';
    const canDeploy = canDeployOn(env.key);
    const canRollback = canRollbackOn(env.key);
    /**
     * 该项目在**本环境**是否已纳管（= service_instances 里有行）。
     *
     * 必须单独判一次：`gate()` 原先只看权限，于是"没纳管"的项目照样点得动「部署新版」，
     * 一路填完表单 → 提交 → 才吃后端 404「该环境尚未纳管此项目」。
     * 这正是"能勾选 ≠ 后端受理"那类假承诺，而且它恰好挡在用户建完环境的下一步上。
     */
    const linked = !!svcMap[env.key]?.[projectKey];

    // 没有权限的项一并置灰：与后端 requireEnvPerm 的判定保持一致，
    // 让用户在看到菜单时就明白"这个环境的这个动作我做不了"。
    //
    // 状态未知（集群读不到）时，"要落集群"的入口也一并置灰：
    // 部署 / 重新部署 / 回滚 / 设定副本数 / 下线 / 看 Pod 日志 全部要连同一个
    // kubeconfig、走同一条网络，读不到就写不成 —— 允许点击只是把失败推迟到提交之后，
    // 而那时用户已经填完表单、以为事情在进行。**只有读平台库的两项保留可用**
    // （Log 历程、预览参数），它们不依赖集群。
    const clusterOpsBlocked = st === 'unknown';
    const gate = (t: ActionType) => {
      if (clusterOpsBlocked && t !== 'logs' && t !== 'params') return disabledItem;
      if (t === 'deploy' && servicesKnown && !linked) return disabledItem;
      if (undeployed && t !== 'deploy') return disabledItem;
      if ((t === 'deploy' || t === 'redeploy' || t === 'scale' || t === 'offline') && !canDeploy) return disabledItem;
      if (t === 'rollback' && !canRollback) return disabledItem;
      return item;
    };

    /** 菜单项右侧那句小字：说明为什么可点 / 为什么灰。 */
    const kbd = (text: string) => (clusterOpsBlocked ? '状态未知' : text);

    const menu = (
      <div className="yn-topo-menu">
        <div className="yn-topo-menu__head">
          <div className="yn-topo-menu__title">
            <span className="yn-topo-node__dot" style={{ background: STATE_STYLE[st].solid }} />
            {env.name}
            <Tag
              style={{ marginLeft: 'auto', marginInlineEnd: 0 }}
              color={
                st === 'healthy'
                  ? 'success'
                  : st === 'error'
                    ? 'error'
                    : st === 'deploying'
                      ? 'processing'
                      : st === 'warning'
                        ? 'warning'
                        : 'default'
              }
            >
              {STATE_STYLE[st].label}
            </Tag>
          </div>
          <div className="yn-topo-menu__sub">
            {clusterKeyOf(env.cluster)} · ns/{env.namespace}
            {cv.own
              ? // 自带凭据：值来自环境自己的同步结果（不是注册表里同名集群的探测值），
                // 所以必须**标明凭据来源** —— 否则用户会问"为什么这里的版本与
                // 「系统管理 › 集群接入」里那条同名记录不一样"。
                `${cv.version ? ` · ${cv.version}` : ''}${
                  cv.nodes !== null ? ` · ${cv.nodes} 节点` : ''
                } · 自带凭据`
              : cv.reg
                ? `${cv.reg.k8sVersion ? ` · ${cv.reg.k8sVersion}` : ''}${
                    cv.reg.nodes !== null ? ` · ${cv.reg.nodes} 节点` : ''
                  }`
                : clusterRegistryKnown
                  ? ' · 该集群尚未接入平台'
                  : ' · 集群列表读取失败，未能确认接入状态'}
          </div>
          <div className="yn-topo-menu__sub">
            {st === 'unknown' ? '读取不到集群状态，无法确认当前镜像版本' : n.image || '尚未部署任何镜像版本'}
          </div>
          {/* 状态未知必须给出原因：只说"未知"等于把用户丢在死路口，
              而这些原因（未接入 / 凭据无权 / 网络不通）指向完全不同的排查方向。 */}
          {st === 'unknown' ? (
            <div className="yn-topo-menu__sub" style={{ color: '#ff6a00' }}>
              {liveOfEnv(env.key)?.message || '读取不到集群状态'}
            </div>
          ) : null}
          {/* 置灰必须给出路，而且是**能点的**出路：只说"未纳管"不说去哪儿纳，
              等于把用户丢在死路口。?link=1 让详情页直接弹开纳管弹窗。 */}
          {servicesKnown && !linked ? (
            <div
              className="yn-topo-menu__sub"
              style={{ color: '#ff6a00', cursor: 'pointer' }}
              onClick={() => navigate(`/cd/environments/${env.key}?link=1`)}
            >
              该项目尚未纳管到本环境 —— 点此去纳管后即可发布
            </div>
          ) : null}
        </div>

        <div
          className={gate('deploy') === item ? `${item} ${item}--primary` : disabledItem}
          onClick={gate('deploy') === item ? () => openAction('deploy', env.key) : undefined}
        >
          <span className="yn-topo-menu__icon">
            <RocketOutlined />
          </span>
          部署新版
          <span className="yn-topo-menu__kbd">
            {kbd(servicesKnown && !linked ? '未纳管' : canDeploy ? '主操作' : '无权限')}
          </span>
        </div>
        <div className={gate('redeploy')} onClick={gate('redeploy') === item ? () => openAction('redeploy', env.key) : undefined}>
          <span className="yn-topo-menu__icon">
            <ReloadOutlined />
          </span>
          重新部署
          <span className="yn-topo-menu__kbd">{kbd('当前版本')}</span>
        </div>
        <div className={gate('rollback')} onClick={gate('rollback') === item ? () => openAction('rollback', env.key) : undefined}>
          <span className="yn-topo-menu__icon">
            <RollbackOutlined />
          </span>
          回滚到历史版本
        </div>
        <div className="yn-topo-menu__sep" />
        <div className={gate('logs')} onClick={gate('logs') === item ? () => openAction('logs', env.key) : undefined}>
          <span className="yn-topo-menu__icon">
            <HistoryOutlined />
          </span>
          检视 Log 历程
        </div>
        <div className={gate('podlog')} onClick={gate('podlog') === item ? () => openAction('podlog', env.key) : undefined}>
          <span className="yn-topo-menu__icon">
            <FileTextOutlined />
          </span>
          查看 Pod 日志
        </div>
        <div className="yn-topo-menu__sep" />
        <div className={gate('scale')} onClick={gate('scale') === item ? () => openAction('scale', env.key) : undefined}>
          <span className="yn-topo-menu__icon">
            <SlidersOutlined />
          </span>
          设定副本数
        </div>
        <div className={gate('params')} onClick={gate('params') === item ? () => openAction('params', env.key) : undefined}>
          <span className="yn-topo-menu__icon">
            <SettingOutlined />
          </span>
          预览参数
        </div>
        <div className="yn-topo-menu__sep" />
        <div
          className={gate('offline') === item ? `${item} ${item}--danger` : disabledItem}
          onClick={gate('offline') === item ? () => openAction('offline', env.key) : undefined}
        >
          <span className="yn-topo-menu__icon">
            <CloudServerOutlined />
          </span>
          取消纳管
        </div>
      </div>
    );

    return (
      <TopoNode
        key={env.key}
        clusterLabel={clusterKeyOf(env.cluster)}
        clusterBadge={cv.badge}
        envName={env.name}
        stageLabel={stage.stage}
        version={vv.version}
        buildNo={vv.build}
        ready={n.ready}
        total={n.total}
        state={st}
        progress={n.progress}
        index={index}
        entering={false}
        active={activeEnv === env.key}
        onActiveChange={(open) => setActiveEnv(open ? env.key : null)}
        menu={menu}
      />
    );
  };

  const renderChain = (envs: Environment[]) => (
    <div className="yn-topo__chain">
      {envs.map((env, i) => {
        const n = nodeOf(env.key);
        const deployed = isDeployed(n);
        return (
          <React.Fragment key={env.key}>
            {i > 0 ? (
              <TopoLink gate={gateOf(env)} color={stageOf(env).color} active={deployed} index={i} />
            ) : null}
            {renderNode(env, i)}
          </React.Fragment>
        );
      })}
    </div>
  );

  /**
   * 集群视图：按集群分组。
   *
   * **自带凭据的环境单独分桶**：它们的 `env.cluster` 只是个名字标签，实际集群不是注册表里
   * 那条同名记录（见 `envClusterView`）。若与"引用平台集群"的环境混在一组，组头再贴上
   * 注册表的探测结果，等于对着一批别的集群做总结。
   * 组键加 `#own` 后缀把它隔开，展示时仍用用户填的标识（`label`）。
   */
  const clusterGroups = useMemo(() => {
    const map = new Map<string, { label: string; own: boolean; envs: Environment[] }>();
    envList.forEach((env) => {
      const label = clusterKeyOf(env.cluster);
      const own = isOwnCluster(env);
      const k = own ? `${label}#own` : label;
      const g = map.get(k) ?? { label, own, envs: [] };
      g.envs.push(env);
      map.set(k, g);
    });
    return [...map.entries()].map(([key, g]) => ({ key, ...g }));
  }, [envList]);

  // ------------------------------------------------------------------ 首屏状态
  const pageError = envsApi.error || projectsApi.error;
  const loading = projectsApi.loading || envsApi.loading;

  return (
    <>
      <PageHeader
        breadcrumb={[{ title: 'CD 环境部署' }, { title: '发布拓扑' }]}
        title="发布拓扑"
        subTitle="RELEASE TOPOLOGY"
        description="以环境为节点呈现项目在各集群的发布链路；圆圈即部署实例，点击可执行部署、回滚、扩缩容等操作。"
        tags={
          <Tag color="blue" style={{ marginInlineStart: 4 }}>
            图形化视图
          </Tag>
        }
        extra={
          <Space>
            {/* 与右边「同步状态」不是一回事，两个都要留：自动刷新只是**重新读一遍**
                （纯 GET，不改任何东西）；同步状态是让后端去集群核对并把差异**写回
                平台库**（纳管关系、实例清单那些），有副作用、也不能每 5 秒来一次。 */}
            <AutoRefreshButton
              value={autoRefreshMs}
              onChange={setAutoRefreshMs}
              updatedAt={servicesApi.updatedAt}
              errorText={servicesApi.silentError?.message}
            />
            <Tooltip title="重新读取各环境与服务的实际状态">
              <Button icon={<ReloadOutlined />} loading={syncing} onClick={syncAllClusters} disabled={!envList.length}>
                同步状态
              </Button>
            </Tooltip>
          </Space>
        }
      />

      {pageError ? (
        <Alert
          type="error"
          showIcon
          style={{ marginBottom: 16 }}
          message="拓扑数据加载失败"
          description={pageError.message}
          action={
            <Button size="small" onClick={refreshAll}>
              重试
            </Button>
          }
        />
      ) : null}

      {/* 控制条 */}
      <div className="yn-topo__bar">
        <div className="yn-topo__bar-left">
          <div className="yn-topo__picker">
            <span className="yn-topo__picker-label">项目</span>
            <Select
              showSearch
              style={{ width: 300 }}
              placeholder={projectsApi.loading ? '加载项目…' : '选择项目'}
              value={projectKey || undefined}
              onChange={pickProject}
              optionFilterProp="search"
              suffixIcon={<AppstoreOutlined style={{ color: '#b5bac4' }} />}
              options={projects.map((p) => ({
                value: p.key,
                search: `${p.name} ${p.key}`,
                label: (
                  <Space size={8}>
                    <Tag
                      color={p.type === 'frontend' ? 'blue' : 'purple'}
                      style={{ marginInlineEnd: 0, transform: 'scale(.86)' }}
                    >
                      {p.type === 'frontend' ? '前端' : '后端'}
                    </Tag>
                    <span>{p.name}</span>
                    <Text className="yn-mono" type="secondary" style={{ fontSize: 11 }}>
                      {p.key}
                    </Text>
                  </Space>
                ),
              }))}
            />
          </div>

          <Segmented
            value={layout}
            onChange={(v) => setLayout(v as 'pipeline' | 'cluster')}
            options={[
              { value: 'pipeline', label: '发布流水线', icon: <NodeIndexOutlined /> },
              { value: 'cluster', label: '按集群分组', icon: <ClusterOutlined /> },
            ]}
          />

          <Tooltip title="一次勾选多个微服务，读取最新构建版本后排队发布">
            <Button
              type="primary"
              icon={<RocketOutlined />}
              className="yn-topo__batch-btn"
              onClick={() => setBatchOpen(true)}
            >
              批量发布
            </Button>
          </Tooltip>

          {flow ? (
            <Button icon={<FileTextOutlined />} onClick={() => setFlowOpen(true)}>
              执行中：{flow.title}
            </Button>
          ) : null}
        </div>

        <div className="yn-topo__bar-right">
          <div className="yn-topo__legend">
            {LEGEND.map((l) => (
              <span key={l.state}>
                {l.state === 'undeployed' || l.state === 'unknown' ? (
                  <i className="hollow" />
                ) : (
                  <i style={{ background: STATE_STYLE[l.state].solid }} />
                )}
                {l.label}
              </span>
            ))}
          </div>
        </div>
      </div>

      {/* 项目元信息 */}
      {project ? (
        <div className="yn-topo__meta">
          {/*
            当前项目名：放在最前并加重显示，因为这一条是「你在看哪个项目」的唯一答案 ——
            拓扑页一眼扫过去就是一堆圆圈，项目认错了后面全白看。
            原先此处显示的是「负责人」（已按用户要求去掉）：负责人回答不了"我在看哪个项目"，
            切换项目时它还是同一个值，反而占着最该放项目名的注意力。
          */}
          <div className="yn-topo__meta-item">
            <b className="yn-topo__meta-project">{project.name}</b>
          </div>
          <div className="yn-topo__meta-item">
            <GithubOutlined style={{ color: '#8c8c95' }} />
            <b>{project.gitUrl || '—'}</b>
          </div>
          <div className="yn-topo__meta-item">
            默认分支 <span className="yn-topo__meta-tag">{project.defaultBranch}</span>
          </div>
          <div className="yn-topo__meta-item">
            最新镜像 <span className="yn-topo__meta-tag">{latest?.tag ?? '—'}</span>
          </div>
          <div className="yn-topo__meta-spacer" />
          <div className="yn-topo__meta-item">
            已覆盖环境 <b>{stats.deployed}</b> / {stats.total}
          </div>
          {stats.busy ? (
            <div className="yn-topo__meta-item">
              <ThunderboltOutlined style={{ color: '#1677ff' }} /> 进行中 <b>{stats.busy}</b>
            </div>
          ) : null}
          {stats.bad ? (
            <div className="yn-topo__meta-item">
              <ExclamationCircleFilled style={{ color: '#ff4d4f' }} /> 异常{' '}
              <b style={{ color: '#ff4d4f' }}>{stats.bad}</b>
            </div>
          ) : null}
        </div>
      ) : null}

      {/* 画布 */}
      {!envList.length && !loading ? (
        <SectionCard bodyStyle={{ padding: 48 }}>
          {/* 拓扑只做展示，建环境在环境管理 —— 这里给条路，别让用户在空页面上白找按钮 */}
          <Empty description="还没有环境。发布拓扑只做展示，环境在「环境管理」里创建">
            <Button type="primary" onClick={() => navigate('/cd/environments')}>
              去环境管理新建
            </Button>
          </Empty>
        </SectionCard>
      ) : !projectKey && !loading ? (
        <SectionCard bodyStyle={{ padding: 48 }}>
          <Empty description="还没有可发布的项目，请先在「CI 镜像构建 › 项目管理」创建项目" />
        </SectionCard>
      ) : (
        <div className="yn-topo__canvas-wrap">
          <div className="yn-topo__canvas" ref={canvasRef} onScroll={syncScrollHint}>
            <div className="yn-topo__canvas-head">
              <span>
                {layout === 'pipeline'
                  ? `发布链路 · 按发布阶段从左到右排列（共 ${envList.length} 个环境）`
                  : `按集群分组 · 共 ${clusterGroups.length} 个集群`}
              </span>
              <span className="yn-topo__flow">
                研发提交 → 自测 → 预发 → 灰度 → 生产
                <span className="yn-topo__flow-rail" />
              </span>
            </div>

            {layout === 'pipeline'
              ? renderChain(envList)
              : clusterGroups.map((g) => {
                  // 自带凭据的组**不查注册表**：那条同名记录是另一个集群（见 envClusterView）。
                  const cluster = g.own ? undefined : clusterByKey.get(g.key);
                  const svcCount = g.envs.filter((e) => isDeployed(nodeOf(e.key))).length;
                  // 自带凭据时组头只能**如实汇总**组内环境自己同步回来的值：
                  // 一个值就写这个值，多个值就说明有多个 —— 它们其实可能不是同一个集群，
                  // 只是用户给的名字标签恰好相同。编一个"这个集群的版本"是没有来源的。
                  const ownVersions = uniqOf(g.envs.map((e) => e.clusterVersion).filter(Boolean));
                  const ownNodes = uniqOf(
                    g.envs.filter((e) => e.lastSyncAt).map((e) => e.nodes),
                  );
                  const summed = (list: readonly (string | number)[]): string =>
                    list.length === 1 ? String(list[0]) : list.length ? `${list.length} 个值` : '—';
                  return (
                    <div className="yn-topo__lane" key={g.key}>
                      <div className="yn-topo__lane-head">
                        <div className="yn-topo__lane-provider">
                          <span className="yn-topo__lane-badge">
                            {g.own ? '自带凭据' : badgeOf(cluster)}
                          </span>
                          {g.own
                            ? `${g.label}（环境自带 kubeconfig）`
                            : cluster?.name ||
                              (clusterRegistryKnown ? '未接入平台' : '集群列表读取失败')}
                        </div>
                        <div className="yn-topo__lane-key">{g.label}</div>
                        <div className="yn-topo__lane-info">
                          {/* 只显示真实读得到的：云厂商/区域/可用区已删——
                              kubeconfig 里没有这些信息，原先那三行是前端编的。 */}
                          <div>
                            K8s 版本 <b>{g.own ? summed(ownVersions) : cluster?.k8sVersion || '—'}</b>
                          </div>
                          <div>
                            节点 <b>{g.own ? summed(ownNodes) : (cluster?.nodes ?? '—')}</b>
                            {!g.own && typeof cluster?.nodesReady === 'number'
                              ? ` · 就绪 ${cluster.nodesReady}`
                              : ''}
                          </div>
                          <div>
                            本项目已部署 <b>{svcCount}</b> / {g.envs.length} 个环境
                          </div>
                        </div>
                      </div>
                      <div className="yn-topo__lane-body">{renderChain(g.envs)}</div>
                    </div>
                  );
                })}
          </div>
          {scrollHint.right ? <div className="yn-topo__fade yn-topo__fade--right" /> : null}
          {scrollHint.left ? <div className="yn-topo__fade yn-topo__fade--left" /> : null}
        </div>
      )}

      {/* ------------------------------------------------------------ 编排执行日志 */}
      <Drawer
        open={flowOpen}
        onClose={() => setFlowOpen(false)}
        width={760}
        title={
          <Space>
            <ThunderboltOutlined style={{ color: '#1677ff' }} />
            {flow?.title ?? '执行日志'}
            {flow && !stream.ended ? <Tag color="processing">实时输出中</Tag> : null}
          </Space>
        }
        styles={{ body: { padding: 16, background: '#f4f6f9' } }}
      >
        {flow ? (
          <>
            <SectionCard bodyStyle={{ padding: '14px 20px' }} style={{ marginBottom: 12 }}>
              <Space direction="vertical" size={6} style={{ width: '100%' }}>
                <Space size={10}>
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    目标版本
                  </Text>
                  <span className="yn-mono">{flow.tag || '—'}</span>
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    副本
                  </Text>
                  <b>{flow.replicas}</b>
                  {stream.status?.stage ? <Tag>{stream.status.stage}</Tag> : null}
                </Space>
                <Progress
                  percent={stream.status?.progress ?? 0}
                  size="small"
                  status={stream.ended ? 'success' : 'active'}
                  strokeColor="#1677ff"
                />
              </Space>
            </SectionCard>
            {stream.error ? <Alert type="warning" showIcon style={{ marginBottom: 12 }} message={stream.error} /> : null}
            <LogPanel
              lines={stream.lines}
              height={380}
              streaming={false}
              live={!stream.ended}
              emptyText="等待日志输出…"
              title={flow.title}
            />
          </>
        ) : (
          <Empty description="当前没有正在执行的任务" />
        )}
      </Drawer>

      {/* ------------------------------------------------------------ 部署新版 */}
      <Modal
        open={action?.type === 'deploy'}
        title={
          <Space size={8}>
            <RocketOutlined style={{ color: '#1677ff' }} />
            部署新版 · {actionEnv?.name}
          </Space>
        }
        okText="提交部署"
        cancelText="取消"
        width={580}
        onCancel={closeAction}
        onOk={submitDeploy}
      >
        <Form form={deployForm} layout="vertical" style={{ marginTop: 16 }}>
          <Form.Item
            name="imageRecordId"
            label="镜像版本"
            rules={[{ required: true, message: '请选择要部署的镜像版本' }]}
            extra="来自该项目的平台镜像记录，按推送时间倒序（最多 20 条）"
          >
            <Select
              showSearch
              optionFilterProp="label"
              placeholder={imagesApi.loading ? '加载镜像列表…' : '选择镜像 tag'}
              loading={imagesApi.loading}
              notFoundContent={imagesApi.loading ? '加载中…' : '该项目暂无镜像产物，请先构建'}
              options={images.slice(0, 20).map((i) => ({
                value: i.id,
                label: `${i.tag}  ·  ${i.branch}  ·  ${(i.pushedAt || '').slice(5, 16)}`,
              }))}
            />
          </Form.Item>
          <Form.Item
            name="replicas"
            label="副本数"
            rules={[{ required: true, message: '请输入副本数' }]}
            extra={
              prefilledReplicas(actionRt) === undefined
                ? '读取不到集群当前副本数，请手动填写后再提交'
                : undefined
            }
          >
            <InputNumber min={1} max={50} style={{ width: 160 }} />
          </Form.Item>
          <Form.Item
            name="note"
            label="变更说明"
            rules={actionEnv?.needApproval ? [{ required: true, message: '需审批的环境必须填写变更说明' }] : []}
          >
            <Input.TextArea rows={2} placeholder="例如：修复订单超时未自动关闭的问题" maxLength={200} showCount />
          </Form.Item>
        </Form>
        {actionEnv?.needApproval ? (
          <Alert
            type="warning"
            showIcon
            message={`${actionEnv.name}为受控环境，提交后需 ${actionEnv.approvers.join('、') || '指定审批人'} 审批通过才会执行`}
          />
        ) : (
          <Alert type="info" showIcon message="该环境为免审批环境，提交后立即执行滚动更新" />
        )}
      </Modal>

      {/* ------------------------------------------------------------ 重新部署 */}
      <Modal
        open={action?.type === 'redeploy'}
        title={
          <Space size={8}>
            <ReloadOutlined style={{ color: '#1677ff' }} />
            重新部署 · {actionEnv?.name}
          </Space>
        }
        okText="确认滚动重启"
        width={520}
        onCancel={closeAction}
        onOk={submitRedeploy}
      >
        <p style={{ marginTop: 12, color: '#5c6470' }}>
          将使用当前镜像版本对该环境执行一次滚动更新，Pod 逐个重建，服务不中断。
        </p>
        <Descriptions column={1} size="small" bordered style={{ marginTop: 12 }}>
          <Descriptions.Item label="当前版本">
            <Text className="yn-mono">{actionRt.tag || '—'}</Text>
          </Descriptions.Item>
          <Descriptions.Item label="副本数">
            {prefilledReplicas(actionRt) === undefined ? (
              <Text type="secondary">{actionSvc ? liveFallbackText(actionSvc) : '—'}</Text>
            ) : (
              `${actionRt.replicas} 个（就绪 ${actionRt.ready}）`
            )}
          </Descriptions.Item>
          <Descriptions.Item label="滚动策略">maxSurge 25% / maxUnavailable 0</Descriptions.Item>
        </Descriptions>
      </Modal>

      {/* ------------------------------------------------------------ 回滚 */}
      <Modal
        open={action?.type === 'rollback'}
        title={
          <Space size={8}>
            <RollbackOutlined style={{ color: '#fa8c16' }} />
            回滚 · {actionEnv?.name}
          </Space>
        }
        okText="确认回滚"
        okButtonProps={{ danger: true, disabled: rollbackNoTarget }}
        width={580}
        onCancel={closeAction}
        onOk={submitRollback}
      >
        <Alert
          type="warning"
          showIcon
          style={{ margin: '12px 0 16px' }}
          message="回滚会以选中版本重建全部 Pod，并记录操作审计"
        />
        <Form form={rollbackForm} layout="vertical">
          {/* `key={actionSvcId}` 是这次修复的关键：换服务即重挂载，版本列表与预选值
              都不可能从上一次残留（原先靠 `destroyOnClose` 是补不上的，Modal 没开也
              不影响 hook 里那份旧 data）。 */}
          <RollbackTargetField
            key={actionSvcId ?? 'none'}
            serviceId={actionSvcId}
            currentImage={actionRt.image}
            form={rollbackForm}
            onNoTargetChange={onRollbackNoTargetChange}
          />
          <Form.Item name="reason" label="回滚原因" rules={[{ required: true, message: '回滚原因必填' }]}>
            <Input.TextArea rows={2} placeholder="例如：新版本下单接口错误率飙升，先回滚止血" maxLength={512} showCount />
          </Form.Item>
        </Form>
      </Modal>

      {/* ------------------------------------------------------------ 设定副本数 */}
      <Modal
        open={action?.type === 'scale'}
        title={
          <Space size={8}>
            <SlidersOutlined style={{ color: '#1677ff' }} />
            设定副本数 · {actionEnv?.name}
          </Space>
        }
        okText="应用"
        width={480}
        onCancel={closeAction}
        onOk={submitScale}
      >
        <Form form={scaleForm} layout="vertical" style={{ marginTop: 16 }}>
          <Form.Item
            name="replicas"
            label="副本数"
            rules={[{ required: true, message: '请输入副本数' }]}
            extra={
              prefilledReplicas(actionRt) === undefined
                ? '读取不到集群当前副本数，请手动填写'
                : '生产环境建议不低于 3'
            }
          >
            <InputNumber min={0} max={200} style={{ width: 160 }} />
          </Form.Item>
          <Form.Item name="reason" label="变更原因">
            <Input placeholder="如：大促前扩容" maxLength={255} />
          </Form.Item>
        </Form>
      </Modal>

      {/* ------------------------------------------------------------ 预览参数 */}
      <Modal
        open={action?.type === 'params'}
        title={
          <Space size={8}>
            <SettingOutlined style={{ color: '#1677ff' }} />
            预览参数 · {actionEnv?.name}
          </Space>
        }
        footer={<Button onClick={closeAction}>关闭</Button>}
        width={760}
      >
        <Descriptions column={2} size="small" bordered style={{ marginTop: 12 }}>
          <Descriptions.Item label="集群">{actionEnv ? clusterKeyOf(actionEnv.cluster) : '—'}</Descriptions.Item>
          <Descriptions.Item label="命名空间">{actionEnv?.namespace ?? '—'}</Descriptions.Item>
          <Descriptions.Item label="部署方式">
            {actionEnv ? DEPLOY_METHOD_LABEL[actionEnv.deployMethod] : '—'}
          </Descriptions.Item>
          <Descriptions.Item label="入口域名">{actionSvc?.entrypoint ?? '—'}</Descriptions.Item>
          <Descriptions.Item label="镜像" span={2}>
            {actionLive ? (
              <Text className="yn-mono" style={{ fontSize: 12 }}>
                {actionLive.image || '—'}
              </Text>
            ) : (
              <Text type="secondary" style={{ fontSize: 12 }}>
                {actionResFallback}
              </Text>
            )}
          </Descriptions.Item>
          <Descriptions.Item label="CPU 请求 / 上限">
            {actionLive ? `${actionLive.cpuRequest || '—'} / ${actionLive.cpuLimit || '—'}` : actionResFallback}
          </Descriptions.Item>
          <Descriptions.Item label="内存请求 / 上限">
            {actionLive ? `${actionLive.memRequest || '—'} / ${actionLive.memLimit || '—'}` : actionResFallback}
          </Descriptions.Item>
          <Descriptions.Item label="配置与密钥" span={2}>
            {(actionSvc?.configMaps ?? []).map((c) => (
              <Tag key={c} className="yn-mono">
                {c}
              </Tag>
            ))}
            {(actionSvc?.secrets ?? []).map((s) => (
              <Tag key={s} color="orange" className="yn-mono">
                {s}
              </Tag>
            ))}
            {!actionSvc?.configMaps?.length && !actionSvc?.secrets?.length ? <Text type="secondary">—</Text> : null}
          </Descriptions.Item>
        </Descriptions>
        <div style={{ marginTop: 14, fontSize: 12.5, color: '#5c6470', marginBottom: 6 }}>
          环境变量（密钥类已脱敏）
        </div>
        <div className="yn-topo__envvars">
          <div>
            {(actionSvc?.envVars ?? actionEnv?.vars ?? []).length ? (
              (actionSvc?.envVars ?? actionEnv?.vars ?? []).map((v) => (
                <div key={v.key} style={{ display: 'flex', gap: 12, fontSize: 12.5, lineHeight: 2 }} className="yn-mono">
                  <span style={{ width: 220, color: '#5c6470' }}>{v.key}</span>
                  <span style={{ color: v.secret ? '#fa8c16' : '#2c313a' }}>
                    {v.secret ? '••••••••（已加密）' : v.value}
                  </span>
                </div>
              ))
            ) : (
              <Text type="secondary" style={{ fontSize: 12.5 }}>
                未配置环境变量
              </Text>
            )}
          </div>
        </div>

        <div style={{ marginTop: 18, fontSize: 12.5, color: '#5c6470', marginBottom: 6 }}>参数变更历史</div>
        {paramHistApi.loading ? (
          <Text type="secondary">正在加载…</Text>
        ) : (paramHistApi.data ?? []).length ? (
          <Timeline
            items={(paramHistApi.data ?? []).slice(0, 5).map((p) => ({
              color: 'blue',
              children: (
                <div>
                  <div style={{ fontSize: 13 }}>
                    <b>第 {p.version} 次变更</b>
                    {p.note ? <span style={{ color: '#5c6470' }}> · {p.note}</span> : null}
                  </div>
                  <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 3 }}>
                    {p.changedAt} · {p.changedBy || '—'}
                    {p.diff?.length ? ` · ${p.diff.join('；')}` : ''}
                  </div>
                </div>
              ),
            }))}
          />
        ) : (
          <Text type="secondary" style={{ fontSize: 12.5 }}>
            暂无参数变更记录
          </Text>
        )}
      </Modal>

      {/* ------------------------------------------------------------ 取消纳管 */}
      <Modal
        open={action?.type === 'offline'}
        title={
          <Space size={8}>
            <ExclamationCircleFilled style={{ color: '#ff4d4f' }} />
            取消纳管 · {actionEnv?.name}
          </Space>
        }
        okText="确认取消纳管"
        okButtonProps={{ danger: true }}
        width={520}
        onCancel={closeAction}
        onOk={submitOffline}
      >
        {/* ⚠️ 这段原先写的是「将删除 …的 Deployment 与 Service，该环境将不再提供访问」——
            两句都不是事实：不但 Deployment/Service 从来没被删过，2026-10-14 起连副本事
            都不再改了。文案比行为更"狠"，用户就不敢用这个功能。 */}
        <Alert
          type="warning"
          showIcon
          style={{ marginTop: 12 }}
          message={`只解除平台对 ${actionEnv?.namespace}/${projectKey} 的纳管，集群里的服务不受影响`}
        />
        <p style={{ marginTop: 14, color: '#5c6470' }}>
          平台会删掉自己下发的变更单（AppRelease）并移除服务实例记录；Deployment 的镜像与副本数
          <b>保持不变</b>，业务不中断。停止服务请用「设定副本数」显式改成 0。
        </p>
        <p style={{ marginTop: 8, color: '#5c6470' }}>
          不会删除镜像与构建记录；想再次接管，重新纳管并部署一次即可。
        </p>
      </Modal>

      {/* ------------------------------------------------------------ Log 历程 */}
      <Drawer
        open={action?.type === 'logs'}
        onClose={closeAction}
        width={600}
        title={
          <Space size={8}>
            <HistoryOutlined style={{ color: '#1677ff' }} />
            {project?.name ?? projectKey} · {actionEnv?.name} 部署历程
          </Space>
        }
      >
        {timelineApi.loading ? (
          <Text type="secondary">正在加载部署记录…</Text>
        ) : (timelineApi.data?.list ?? []).length ? (
          <Timeline
            items={(timelineApi.data?.list ?? []).map((d: DeployRecord) => ({
              color: recordStatus(d.status).dot,
              children: (
                <div>
                  <div className="yn-topo__logline">
                    <b>部署单 {d.deployNo}</b>
                    <Tag color={recordStatus(d.status).tag} style={{ marginInlineEnd: 0 }}>
                      {recordStatus(d.status).text}
                    </Tag>
                  </div>
                  <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 4 }}>
                    {d.startedAt} · {d.operator || '—'} · 耗时 {recordDurationText(d)}
                  </div>
                  <div className="yn-mono" style={{ fontSize: 11.5, color: '#b5bac4', marginTop: 3 }}>
                    {d.image}
                  </div>
                </div>
              ),
            }))}
          />
        ) : (
          <Empty description="该环境还没有部署记录" />
        )}
      </Drawer>

      {/* ------------------------------------------------------------ Pod 日志 */}
      <Drawer
        open={action?.type === 'podlog'}
        onClose={closeAction}
        width={800}
        title={
          <Space size={8}>
            <FileTextOutlined style={{ color: '#1677ff' }} />
            {projectKey} · {actionEnv?.name} Pod 日志
          </Space>
        }
      >
        <Space style={{ marginBottom: 12 }} wrap>
          <Select
            size="small"
            style={{ width: 300 }}
            value={podName || undefined}
            onChange={setPodName}
            loading={podsApi.loading}
            placeholder={podsApi.loading ? '加载中…' : '选择 Pod'}
            options={(podsApi.data ?? []).map((p) => ({ value: p.name, label: `${p.name}（${p.status}）` }))}
            notFoundContent="该服务当前没有运行中的 Pod"
          />
          {podLogUrl && !podLogs.ended ? <Tag color="processing">实时跟随中</Tag> : null}
          <Tag className="yn-mono" style={{ marginInlineStart: 8 }}>
            {clusterKeyOf(actionEnv?.cluster ?? '')} / {actionEnv?.namespace}
          </Tag>
        </Space>
        {podLogs.error ? <Alert type="warning" showIcon style={{ marginBottom: 12 }} message={podLogs.error} /> : null}
        <LogPanel
          height={480}
          title={`pod: ${podName || '<pod>'}`}
          lines={podLogs.lines}
          live={!!podLogUrl && !podLogs.ended}
          emptyText={podName ? '暂无日志输出…' : '请先选择一个 Pod'}
        />
      </Drawer>


      {/* ------------------------------------------------------------ 批量发布 */}
      {/* 发布由 BatchDeployWizard 提交真实部署单（POST /deployments），
          日志来自后端 SSE；这里只负责在批次结束后刷新拓扑节点状态。 */}
      <BatchDeployWizard
        open={batchOpen}
        onClose={() => setBatchOpen(false)}
        // 默认带上"用户当前正在看的那个环境"：批量发布最常见的起点就是
        // "我在看测试区的拓扑，想把这一批服务一起发上去"。
        // 用 envList[0] 会让用户每次都先手动改一次环境。
        defaultEnvKey={activeEnv ?? envList[0]?.key}
        onServiceDeployed={() => {
          refreshAll();
        }}
        onCompleted={() => {
          refreshAll();
          message.success('批量发布批次已完成，拓扑节点已同步最新状态');
        }}
      />
    </>
  );
};

export default Topology;
