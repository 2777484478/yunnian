import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Button,
  Input,
  Modal,
  Segmented,
  Select,
  Space,
  Spin,
  Steps,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  CheckCircleFilled,
  CloudDownloadOutlined,
  CloudServerOutlined,
  DeploymentUnitOutlined,
  ExclamationCircleFilled,
  ReloadOutlined,
  RocketOutlined,
  SearchOutlined,
  SwapOutlined,
} from '@ant-design/icons';
import { useNavigate } from 'react-router-dom';
import { BatchRunner } from './BatchRunner';
import type { BatchResult, BatchTaskDef, BatchTaskHandle, BatchTaskOutcome } from './BatchRunner';
import { ProjectTypeTag, ServiceStatusTag } from './StatusTag';
import { DisabledTip } from '@/components/DisabledTip';
import { sortByStage, stageOf } from '@/config/topology';
import { ApiError, deployApi, environmentApi, imageApi, registryApi, systemApi } from '@/api';
import type { RegistryTags } from '@/api/registry';
import { canDeployOn, hasPerm } from '@/auth';
import { useApi } from '@/hooks/useApi';
import { humanSize } from '@/utils/format';
import { liveFallbackText, liveReplicaTotal, liveState, liveStatusWord } from '@/utils/live';
import type { DeployStatus, Environment, EnvService, ImageRecord, ServiceLive } from '@/types';

const { Text } = Typography;

export interface BatchDeployWizardProps {
  open: boolean;
  onClose: () => void;
  /** 从拓扑页带入的默认环境 */
  defaultEnvKey?: string;
  presetKeys?: string[];
  /** 单服务发布完成（回写拓扑节点状态） */
  onServiceDeployed?: (envKey: string, projectKey: string, tag: string) => void;
  onCompleted?: (r: BatchResult) => void;
}

type FilterKey = 'all' | 'undeployed' | 'update' | 'same';

/**
 * 对比列最多展示几个环境（**含目标环境本身**）。
 *
 * 用户 2026-10-15 明确要求"只展示 2 个环境"：目标环境 + 链路里紧邻的前一个。
 * 做成常量而不是写死"前一个"，是因为这取舍是**版面**问题而不是逻辑问题 ——
 * 多一个环境就多一列版本号，横向立刻挤爆（版本 tag 本身就有 10 来个字符）。
 * 真要放开只改这个数字，不必碰取数逻辑。
 */
const MAX_COMPARE_ENVS = 2;

/**
 * 版本号单元格。三种变体对应这一屏里三种不同的语义：
 *
 *   `ref`  前一环境在跑的（参考，最轻的灰）
 *   `now`  目标环境当前的（现状，常规文字色）
 *   `pick` 本次要发布的（结果，主色 + 浅底）
 *
 * 不做成三种灰度的 `<Tag>`：标签自带边框和背景，三列并排会变成三块同质的色块，
 * 反而更难分辨谁是谁。这里只调**字色与底色**，版本号本体的字形始终一致 ——
 * 对比时眼睛只需要读数字，不用重新定位。
 */
const TagText: React.FC<{ value?: string; variant?: 'ref' | 'now' | 'pick' }> = ({
  value,
  variant = 'now',
}) =>
  value ? (
    <Tooltip title={value}>
      <span className={`yn-ver yn-ver--${variant}`}>
        <span className="yn-ver__tag">{value}</span>
      </span>
    </Tooltip>
  ) : (
    <Text type="secondary">—</Text>
  );

interface RowModel {
  projectKey: string;
  projectName: string;
  projectType: 'frontend' | 'backend';
  /** 当前环境下已部署的实例（未部署时为 undefined） */
  svc?: EnvService;
  /**
   * 该环境是否把这个项目列进了项目清单（后端 `project-candidates.linked`，
   * 数据源是 environment_projects 关联表）。
   *
   * ⚠️ **`linked` 不等于"能发布"**。`POST /deployments` 要求 (env, project) 已经存在
   * `service_instances` 行（`serviceByEnvProject`），否则报
   * 「该环境尚未纳管此项目：请先在环境详情里关联该项目」。
   * 正常走 `linkEnvProjects` 纳管时关联表与实例是一起建的，两者一致；
   * 但数据一旦漂移（历史导入、直接改库），就会出现 linked=true 却没有实例的行。
   * 判断"能不能发布"只认 `svc` 是否存在，不要认 `linked`。
   */
  linked: boolean;
  /** 该项目是否已有构建产物 —— 没有产物的项目部署必然失败 */
  hasBuild: boolean;
  /** 仓库中最新镜像 */
  latest?: ImageRecord;
  /** 可选版本（仓库里该项目的镜像，按推送时间倒序） */
  versions: ImageRecord[];
  /**
   * 该项目在**镜像仓库**（Harbor）里的标签。
   *
   * ⚠️ 三种状态必须分开，混成一种就会把"还没问"说成"仓库里没有"：
   *   `undefined` 还没去查（还没走到核对版本那一步）
   *   `null`      查了但这次没读到
   *   `{unavailable}` 读到了响应，但响应说读不到（没接仓库 / 找不到仓库 / 没权限）
   */
  registry?: RegistryTags | null;
  /** 该实例的集群实时状态（`EnvService.live` 原样带过来） */
  live?: ServiceLive | null;
  /** 集群状态是否**读到了**（`live.available`）。决定"当前版本/变更"两列能不能下结论 */
  currentKnown: boolean;
  /** 集群里这个工作负载是否存在（available && exists） */
  exists: boolean;
  /**
   * 集群里**实跑**的 tag。
   *
   * 不是平台库里的 `svc.tag`：那是纳管时写入、只在部署成功后才更新的"平台期望"，
   * 与集群实况会长期不一致（实测 demobasic 库值 1.2.0.2761 / 集群 1.2.0.2577）。
   * 这一列的表头写的是"当前版本"，就必须是集群里的那个。
   */
  currentTag?: string;
  /** 平台库里记的 tag —— 只用于判断"有没有可用镜像"，不作为"当前版本"展示 */
  platformTag?: string;
  undeployed: boolean;
  /** 集群里实跑的副本数；读不到时为 undefined（不拿平台库的值凑） */
  replicas?: number;
  /**
   * 该环境上这个服务是否已有**进行中**的部署单（queued / progressing / pending_approval）。
   *
   * 后端会拒绝并发发布（`该服务已有进行中的部署`），所以这不是"提示"而是"必然失败"。
   * 与其让用户勾中、走到第 3 步再报错，不如在第 0 步就禁掉并说明原因。
   */
  inflight?: { deployNo: string; status: DeployStatus };
}

/**
 * 批量发布向导：选环境与服务 → 核对目标版本 → 串行排队发布（实时日志）。
 *
 * 与「批量打包向导」同样的原则：界面上每一个数字、每一行日志都能在数据库里找到出处。
 * 因此这几件事被改掉了：
 *
 * 1. **没有假扫描动画**。早期版本用定时器逐行"读取 Harbor"，看着像在跑其实是演出。
 *    现在是一次真实的 `GET /images`，读回来多少条就显示多少条，失败就报失败。
 *
 * 2. **没有「注入 1 个失败」开关**。真实部署失败来自集群，不由前端开关决定。
 *    要看失败链路请用真实失败的服务（就绪探针超时等）。
 *
 * 3. **待审批与执行中是两个状态**。开启审批的环境，提交后单据状态是
 *    `pending_approval`，不会产生任何执行日志——批次面板里单列一栏，
 *    而不是让它挂在"执行中"让人以为已经发版。
 *
 * 4. **能勾选 == 后端受理**。勾选框的三条禁用条件逐条对应后端的拒绝理由：
 *    - `!hasBuild`  → 仓库里没有这个项目的镜像（`resolveDeployImage` 会拒）
 *    - `undeployed` → 该环境没有这个服务的实例（`serviceByEnvProject` 会 404）
 *    - `inflight`   → 已有 pending_approval/queued/progressing 的单（`StateConflict`）
 *    早先这三条里有两条只写在"提交后报错"里，用户勾得动、发得出、最后失败；
 *    现在一条条前置挡掉，并在行内/汇总处写明原因。
 *
 * 5. **「当前版本」读集群实况，不读平台库**。表头写的是"当前版本"，此前取的却是
 *    `service_instance.current_tag`——纳管时写入、只在部署成功后才更新，与集群实况会长期
 *    不一致（实测 demobasic：库值 1.2.0.2761 / 集群 1.2.0.2577）。同一屏的另一处
 *    （环境详情的服务表格）已经改成读集群，两处不一致比两处都错更糟。
 *    读不到集群时**不冒充**：既不显示库值，也不把"变更"列标成"已最新"，
 *    因为那是对一个我们并不知道的现状下的断言。
 */
/**
 * 目标版本的**选择标识**（两组候选共用一条 value 通道）。
 *
 * 候选来自两组：**仓库标签**（Harbor 里实际存在的，含别人推的）与
 * **平台构建**（image_record，带构建号 / commit / 推送时间可溯源）。
 * 两组里的 tag 可能**同名**（平台构建完推到仓库时就是同一个），但语义不同、
 * 后端走的是两条不同的取镜像链路。所以 value 必须带来源前缀 ——
 * 只存 tag 名的话，同名候选在下拉里长得一模一样，提交时也无从判断该走哪条。
 */
const SRC_REGISTRY = 'r:';
const SRC_PLATFORM = 'p:';

/** 一次从仓库读几个标签。批量场景每个服务一次请求，不宜取太多。 */
const REG_TAG_LIMIT = 8;

/** 从带前缀的选择标识里取裸 tag（`r:1.2.3` → `1.2.3`）。 */
function bareTagOf(v: string | undefined): string {
  if (!v) return '';
  return v.startsWith(SRC_REGISTRY) || v.startsWith(SRC_PLATFORM) ? v.slice(2) : v;
}

/**
 * 一行最终提交什么：**二选一，不能同时给**。
 *
 * 后端 `checkImageSource` 对"平台与仓库都给"直接拒（400），
 * 所以这里用联合类型把"两个都给"从类型上排除掉 —— 比事后加一句注释可靠。
 */
type DeployPick = { registryTag: string } | { platformTag: string };

/** 取出 DeployPick 里那个 tag（两种来源的字段名不同，只有这里知道怎么取）。 */
function pickTagOf(p: DeployPick | undefined): string {
  if (!p) return '';
  return 'registryTag' in p ? p.registryTag : p.platformTag;
}

/** 仓库标签的时间口径说明：Harbor v1 只能给构建时间，v2 才有推送时间。 */
function regTimeText(t: { time: string; timeKind: string }): string {
  if (!t.time) return '时间未知';
  const label = t.timeKind === 'build' ? '构建于' : t.timeKind === 'push' ? '推送于' : '';
  const when = t.time.slice(0, 19).replace('T', ' ');
  return label ? `${label} ${when}` : when;
}

const BatchDeployWizard: React.FC<BatchDeployWizardProps> = ({
  open,
  onClose,
  defaultEnvKey,
  presetKeys,
  onServiceDeployed,
  onCompleted,
}) => {
  const { message, notification } = AntApp.useApp();
  const navigate = useNavigate();

  const [step, setStep] = useState(0);
  const [envKey, setEnvKey] = useState<string>('');
  const [keys, setKeys] = useState<string[]>([]);
  const [filter, setFilter] = useState<FilterKey>('all');
  const [kw, setKw] = useState('');

  // 真实取数状态
  const [envs, setEnvs] = useState<Environment[]>([]);
  const [rows, setRows] = useState<RowModel[]>([]);
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState('');
  /** 读取进度（真实请求，不是演出）：读到哪一步就写到哪一步 */
  const [scanNote, setScanNote] = useState('');
  const [imageCount, setImageCount] = useState(0);

  /** 逐服务的目标版本（默认 = 仓库最新，值形如 `r:1.2.3` / `p:1.2.3`，见 SRC_*） */
  const [targets, setTargets] = useState<Record<string, string>>({});
  const [skipped, setSkipped] = useState<string[]>([]);
  /** 读仓库标签的进行中状态（每个服务一次 Harbor 请求）。 */
  const [regLoading, setRegLoading] = useState(false);
  /** 已经查过仓库的项目 —— 来回切步骤不重复请求。 */
  const regLoadedRef = useRef<Set<string>>(new Set());
  /** 用户手动改过目标版本的项目：仓库标签回来时**不许**覆盖他的选择。 */
  const targetTouchedRef = useRef<Set<string>>(new Set());

  const [tasks, setTasks] = useState<BatchTaskDef[]>([]);
  const [batchNo, setBatchNo] = useState('');
  const [round, setRound] = useState(0);
  const [result, setResult] = useState<BatchResult | null>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);

  const env = useMemo(() => envs.find((e) => e.key === envKey), [envs, envKey]);

  // ------------------------------------------------------------------ 环境列表

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    environmentApi
      .list({ pageSize: 100 })
      .then((page) => {
        if (cancelled) return;
        // approvers 在后端是切片：空切片序列化成 []，但一旦哪天变成 null，
        // 下面几处 .join('、') 就会白屏。在网络边界一次性兜平。
        setEnvs(page.list.map((e) => ({ ...e, approvers: e.approvers ?? [] })));
        setEnvKey((prev) => prev || defaultEnvKey || page.list[0]?.key || '');
      })
      .catch((e) => {
        if (!cancelled) setLoadError(e instanceof ApiError ? e.message : '环境列表加载失败');
      });
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, defaultEnvKey]);

  useEffect(() => {
    if (!open) return;
    setStep(0);
    setKeys(presetKeys ?? []);
    setFilter('all');
    setKw('');
    setTargets({});
    setSkipped([]);
    setImageCount(0);
    setScanNote('');
    setTasks([]);
    setResult(null);
    setLoadError('');
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  // ------------------------------------------------------------------ 行数据（真实）

  const loadRows = useCallback(
    async (key: string) => {
      if (!key) return;
      setLoading(true);
      setLoadError('');
      setRows([]);
      setTargets({});
      // 「重新读取」= 仓库结果全部作废（用户可能刚在 Harbor 推了新 tag），
      // 连"用户改过哪些行"也一并忘掉：这是一次彻底重来，不是增量刷新。
      regLoadedRef.current = new Set();
      targetTouchedRef.current = new Set();
      setScanNote('读取仓库镜像…');
      try {
        // 1) 仓库镜像：一次取回全平台镜像（一期总量在 200 以内），
        //    按项目分组、按推送时间倒序。比"每个项目发一次请求"省 19 次往返。
        const images = await imageApi.list({ pageSize: 200 });
        const byProject = new Map<string, ImageRecord[]>();
        images.list.forEach((i) => {
          const arr = byProject.get(i.projectKey) ?? [];
          arr.push(i);
          byProject.set(i.projectKey, arr);
        });
        byProject.forEach((arr) =>
          arr.sort((a, b) => String(b.pushedAt).localeCompare(String(a.pushedAt))),
        );
        setImageCount(images.total);
        setScanNote('读取环境纳管与部署实例…');

        // 2) 该环境的部署实例 + 项目候选（候选里带 linked / hasBuild 两个关键判断位）
        //    + 该环境的部署单（用来找"已有进行中的部署"，后端据此拒绝并发发布）
        const [svcs, cands, deploys] = await Promise.all([
          environmentApi.services(key),
          environmentApi.projectCandidates(key, {}),
          deployApi.list({ envKey: key, pageSize: 200 }).catch(() => null),
        ]);
        const svcByKey = new Map(svcs.map((s) => [s.projectKey, s]));

        // 进行中的口径与后端一致：非终态即进行中。终态是 success / failed / canceled / rolled_back。
        const INFLIGHT: DeployStatus[] = ['pending_approval', 'queued', 'progressing'];
        const inflightByKey = new Map<string, { deployNo: string; status: DeployStatus }>();
        deploys?.list.forEach((d) => {
          if (!INFLIGHT.includes(d.status)) return;
          // 列表按时间倒序，第一条就是最新的进行中单据
          if (!inflightByKey.has(d.projectKey)) {
            inflightByKey.set(d.projectKey, { deployNo: d.deployNo, status: d.status });
          }
        });

        const models: RowModel[] = cands.map((c) => {
          const svc = svcByKey.get(c.key);
          const versions = byProject.get(c.key) ?? [];
          const latest = versions[0];
          const live = svc?.live ?? null;
          // 三种情形分开：读到但集群里没有 / 确实读到了 / 没读到。
          // 只有第二种能给"当前版本"下结论；第一种能确定地说"集群里不存在"；
          // 第三种（未接入、凭据无权、网络不通）什么都不能说。
          const currentKnown = !!live && live.available;
          const exists = currentKnown && !!live!.exists;
          const currentTag = exists ? live!.tag : undefined;
          return {
            projectKey: c.key,
            projectName: c.name,
            projectType: c.type,
            svc,
            linked: c.linked,
            hasBuild: c.hasBuild || versions.length > 0,
            latest,
            versions,
            live,
            currentKnown,
            exists,
            currentTag,
            platformTag: svc?.tag,
            undeployed: !svc,
            // ⚠️ 这里**不再**算 hasUpdate：它现在由 `hasUpdateOf` 按**目标版本**现算。
            // 实例总数（不是期望数）：列名是"集群里实际在跑的个数"，
            // 滚动卡住时那个反复崩溃的实例也算在内，否则这一列会显示成"数量正常"。
            replicas: exists ? liveReplicaTotal(live!) : undefined,
            inflight: inflightByKey.get(c.key),
          };
        });

        setRows(models);
        // 勾选项里可能混着"不可发布"的（拓扑页带过来的 presetKeys、或上一步刚变成进行中）：
        // 在这里剪掉，别让它们一路带到第 3 步才炸。
        const selectable = new Set(
          models.filter((m) => m.hasBuild && !m.undeployed && !m.inflight).map((m) => m.projectKey),
        );
        setKeys((prev) => prev.filter((k) => selectable.has(k)));
        // 默认目标版本先给「平台最新」—— 它一定带可靠的推送时间，任何情况下都有值；
        // 等仓库标签读回来再升级成「仓库最新」（见 loadRegistryTags）。
        // 分两步的理由：仓库读不到时，这一行仍然站得住，不会变成一个空选择。
        setTargets(
          Object.fromEntries(
            models.filter((m) => m.latest).map((m) => [m.projectKey, SRC_PLATFORM + m.latest!.tag]),
          ),
        );
        setScanNote('');
      } catch (e) {
        setLoadError(e instanceof ApiError ? e.message : '环境数据加载失败');
        setScanNote('');
      } finally {
        setLoading(false);
      }
    },
    [],
  );

  /**
   * 只要弹窗打开、且已确定目标环境，就真实读一次数据。
   *
   * 这里曾经写成 `if (step !== 1) return`——理由是"第 1 步（核对版本）才需要数据"，
   * 但第 0 步（选择服务）的表格渲染的就是同一份 `rows`，于是选择页永远"暂无数据"，
   * 没有服务可勾选，向导死在第一步。数据只有一份，读在进去之前。
   */
  /**
   * 读勾选服务在**镜像仓库**里的标签。
   *
   * 为什么不在 loadRows 里一次全读：那要读**环境的全部项目**（实测 41 个），
   * 每个一次 Harbor 请求 —— 而用户这次可能只发 6 个。放在「核对版本」这一步、
   * 只读已勾选的，请求数与真实需要一致；来回切步骤也不重复（regLoadedRef 挡着）。
   *
   * ⚠️ 读不到**不冒充空列表**：空数组会被读成"这仓库没有标签"，
   * 而真实原因可能是"没接仓库 / 找不到对应仓库 / 没权限 / 仓库不通"，
   * 几种情况的处置完全不同。所以如实记成 null / unavailable，界面显示原因。
   */
  const loadRegistryTags = useCallback(async (projectKeys: string[]) => {
    const todo = projectKeys.filter((k) => !regLoadedRef.current.has(k));
    if (!todo.length) return;
    // 先登记再发请求：并发触发（切勾选、切步骤）时不会把同一个项目问两遍。
    todo.forEach((k) => regLoadedRef.current.add(k));
    setRegLoading(true);
    try {
      const results = await Promise.all(
        todo.map((k) =>
          registryApi.harborTags(k, REG_TAG_LIMIT).then(
            (r) => r,
            () => null, // 请求本身失败 → null，与"响应说读不到"（unavailable）分开
          ),
        ),
      );
      const byKey = new Map<string, RegistryTags | null>();
      todo.forEach((k, i) => byKey.set(k, results[i] ?? null));

      setRows((prev) =>
        prev.map((r) => (byKey.has(r.projectKey) ? { ...r, registry: byKey.get(r.projectKey)! } : r)),
      );

      // 默认目标版本升级成「仓库最新」—— 用户要的就是它。
      // ⚠️ 只动**用户没碰过**的行：覆盖用户刚才的选择是这里最容易犯的错，
      // 而且覆盖后界面看着很正常（值变了但没有任何提示）。
      setTargets((prev) => {
        const next = { ...prev };
        todo.forEach((k) => {
          if (targetTouchedRef.current.has(k)) return;
          const reg = byKey.get(k);
          const tag = reg && !reg.unavailable ? reg.tags[0]?.tag : undefined;
          if (tag) next[k] = SRC_REGISTRY + tag;
        });
        return next;
      });
    } finally {
      setRegLoading(false);
    }
  }, []);

  useEffect(() => {
    if (!open || !envKey) return;
    void loadRows(envKey);
  }, [open, envKey, loadRows]);

  /**
   * 走到「核对版本」这一步才读仓库标签（并只读已勾选的服务）。
   * `keys` 在依赖里：用户回头多勾一个服务，它也能补读那一个。
   */
  /**
   * 链路里**紧邻的前一个环境**。
   *
   * 按 `sortByStage`（优先用环境自带的链路序号，没有才回退到按 key 的默认阶段表）
   * 排一遍，取目标环境的前一个。这样"链路第几位"就是用户自己配的那个值，
   * 不被前端的映射表覆盖 —— 与拓扑页用的是同一个排序函数，两处不会给出不同的链路。
   *
   * ⚠️ 只取**紧邻**的一个，且 `MAX_COMPARE_ENVS` 限制了展示的环境数（含目标本身）。
   * 再往前取（开发 → 预发 → 生产）虽然也能取，但每多一个环境就多一列版本号，
   * 表会挤到读不了 —— 用户要的是"跟上一环对比"，不是全链路铺开。
   */
  const prevEnv = useMemo(() => {
    if (!env) return undefined;
    const chain = sortByStage(envs);
    const i = chain.findIndex((e) => e.key === env.key);
    // 目标环境在链路里找不到（理论上不会，envs 就是这一份）时不猜，直接不展示对比列。
    if (i <= 0) return undefined;
    return chain[i - 1];
  }, [envs, env]);

  /**
   * 前一环境的服务实时状态，按 projectKey 索引。
   *
   * ⚠️ 状态是**独立存的**，不是往 `rows` 里回写字段。第一版就是回写 rows，
   * 结果那一列整列显示 "—"：拉取这个 effect 只在"前一环境变了"时跑，
   * 而它第一次跑时 `rows` 还空着（数据还在路上）→ `rows.map` 是空转，
   * 之后 rows 到了、effect 却因去重不再重跑 —— 于是永远没有人给那些行填值。
   *
   * 派生出来就没这个问题：rows 什么时候到都能算，且不必在两次 setRows 之间对齐时序。
   *   undefined 还没读到（请求在路上）
   *   null      读取失败
   *   Map       读到了（里面缺某个 projectKey = 前一环境没纳管它）
   */
  const [prevSvcs, setPrevSvcs] = useState<Map<string, EnvService> | null | undefined>(undefined);
  /**
   * `targets` 的最新值（供 `syncFromPrev` 读）。
   *
   * 不把它写进 `useCallback` 的依赖再读闭包：那样每改一次版本都会重建回调，
   * 而它被挂在下拉的 onClick 上 —— 重建本身不致命，但"读到旧闭包里的 targets"
   * 会让同步动作基于上一帧的值计算，表现是**偶发少同步一行**。
   */
  const targetsRef = useRef(targets);
  targetsRef.current = targets;

  /** 已经为哪个环境读过服务列表 —— 同一次会话里不重复请求，换环境才重读。 */
  const lastPrevKeyRef = useRef<string>('');

  /**
   * 读**前一环境**的服务列表（一次请求覆盖所有行，不是每行一次）。
   *
   * 复用 `environmentApi.services`：它返回的 `live` 就是**该环境集群的实时状态**，
   * 与「当前版本」列用的是同一份口径 —— 两个环境放在一起比必须同口径，
   * 否则"两边版本不一样"可能只是两边取数方式不同造成的假象。
   */
  const loadPrevSvcs = useCallback(async (pk: string) => {
    try {
      const d = await environmentApi.services(pk);
      setPrevSvcs(new Map(d.map((s) => [s.projectKey, s])));
    } catch {
      // 整个前一环境的服务列表读不到：记成 null，界面显示"读取失败"。
      // **不静默留空** —— 留空会被读成"前一环境没有这些服务"，那是另一个结论。
      setPrevSvcs(null);
    }
  }, []);

  /**
   * 前一环境一旦确定就整表读一次。
   *
   * 不按勾选过滤：勾选会变，而这一列的价值正是"让我在勾选时就看到上一环跑的是什么"。
   */
  useEffect(() => {
    if (!open || !prevEnv) {
      setPrevSvcs(undefined);
      return;
    }
    // 换个前一环境就是另一份数据，必须重读；同一次会话里同一个环境不重复请求。
    if (lastPrevKeyRef.current === prevEnv.key) return;
    lastPrevKeyRef.current = prevEnv.key;
    setPrevSvcs(undefined);
    void loadPrevSvcs(prevEnv.key);
  }, [open, prevEnv, loadPrevSvcs]);

  /**
   * 逐行的前一环境版本 —— 由 `prevSvcs` 与当前 rows **派生**，不回写 rows。
   *
   * 三种"没有值"在这里分开（对应三种不同的处置）：
   *   还没读到（prevSvcs === undefined）→ 请求在路上，文案说"正在读取"
   *   读取失败（prevSvcs === null）     → 去查前一环境的集群通不通
   *   前一环境没纳管这个服务             → 去前一环境把它纳管进来
   */
  const prevInfoOf = useCallback(
    (projectKey: string): { tag?: string; reason?: string } => {
      if (!prevEnv) return { reason: '链路里没有更靠前的环境' };
      if (prevSvcs === undefined) return { reason: '正在读取前一环境…' };
      if (prevSvcs === null) return { reason: `读取 ${prevEnv.key} 失败` };
      const s = prevSvcs.get(projectKey);
      if (!s) return { reason: `${prevEnv.key} 未纳管此服务` };
      const live = s.live;
      if (!live || !live.available) return { reason: `读不到 ${prevEnv.key} 的集群状态` };
      if (!live.exists) return { reason: `${prevEnv.key} 集群里没有这个工作负载` };
      return { tag: live.tag || undefined, reason: live.tag ? undefined : '前一环境的版本号为空' };
    },
    [prevEnv, prevSvcs],
  );

  /**
   * 读仓库标签的**时机**。
   *
   * 数据一加载完，就先把**有平台产物的**项目读一轮（实测这类只有个位数到十几个）。
   * 目的是让第 1 步的「变更」列和第 2 步的默认目标版本用**同一个口径** ——
   * 否则同一件事在同一页里会有两个答案：一页"有更新"、下一页"没有变化"
   * （2026-10-15 实测撞到，见 hasUpdateOf 的注释）。
   *
   * 走到「核对版本」时再补一次 `keys`：那里可能多出"平台没产物、但用户手工勾上"的项目。
   * 读过的由 regLoadedRef 挡掉，不会重复请求。
   */
  useEffect(() => {
    if (!open || !rows.length) return;
    void loadRegistryTags(rows.filter((r) => r.latest).map((r) => r.projectKey));
  }, [open, rows, loadRegistryTags]);

  useEffect(() => {
    if (!open || step !== 1) return;
    void loadRegistryTags(keys);
  }, [open, step, keys, loadRegistryTags]);

  // ------------------------------------------------------------------ 派生

  /**
   * 「这次发布会变什么」的**唯一口径**。
   *
   * ⚠️ 必须按**目标版本**（targets）现算，不能用平台记录里的 latest：
   * 目标版本的默认值会从「平台最新」升级成「仓库最新」—— 而仓库里可能有平台
   * 根本不知道的 tag（Jenkins / 别人手工推的）。两个口径一旦分家，同一件事
   * 在同一页里就会有两个答案：第 1 步说"有更新"，第 2 步说"全部为同版本发布"。
   * （2026-10-15 实测撞到：平台最新是 2.2.0.1006，而仓库按构建时间最新是 1.2.0.3416，
   *   正是当前集群在跑的那个 —— 于是两页说法相反。）
   */
  const hasUpdateOf = useCallback(
    (r: RowModel) => {
      if (!r.exists) return false;
      const t = bareTagOf(targets[r.projectKey]) || r.latest?.tag;
      return !!t && t !== r.currentTag;
    },
    [targets],
  );

  const filtered = useMemo(
    () =>
      rows.filter((r) => {
        const k = kw.trim().toLowerCase();
        const hitKw = !k || r.projectName.toLowerCase().includes(k) || r.projectKey.includes(k);
        const hitFilter =
          filter === 'all' ||
          (filter === 'undeployed' && r.undeployed) ||
          (filter === 'update' && hasUpdateOf(r)) ||
          // 「已最新」只收**能确认**已最新的：读不到集群的行既不算有更新也不算已最新，
          // 否则"已最新 12"里会混进一堆我们根本不知道现状的服务。
          (filter === 'same' && r.currentKnown && !hasUpdateOf(r));
        return hitKw && hitFilter;
      }),
    [rows, filter, kw, hasUpdateOf],
  );

  const counts = useMemo(
    () => ({
      undeployed: rows.filter((r) => r.undeployed).length,
      update: rows.filter((r) => hasUpdateOf(r)).length,
      same: rows.filter((r) => !r.undeployed && r.currentKnown && !hasUpdateOf(r)).length,
    }),
    [rows, hasUpdateOf],
  );

  const picked = useMemo(
    () => rows.filter((r) => keys.includes(r.projectKey) && !skipped.includes(r.projectKey)),
    [rows, keys, skipped],
  );

  /**
   * 「同步版本号」：把目标版本一键对齐到**前一环境正在跑的版本**。
   *
   * 为什么要这个按钮：现在的流程是"152 发完，去阿里云按仓库最新再发一遍"，
   * 而两边选的其实应该是**同一个 tag**（同一份镜像逐环晋级）。手点 6 个下拉、
   * 每次都要认一遍版本号，既慢又容易点错 —— 而点错的后果是发一个别的版本上去。
   *
   * 三条边界，每条都对应一种真实情况：
   *
   *  1. **前一环境的版本不一定在仓库候选里**（用户已拍板：追加进候选并选中）。
   *     不追加的话，`Select` 会因为 value 不在 options 里而显示成空值 ——
   *     看着像"没选"，提交时却带着那个 tag，比直接报错更难发现。
   *     追加的候选会在 label 里标明来源是"前一环境在跑"，不与真正的仓库标签混淆。
   *
   *  2. **读不到的行保持原样**（用户已拍板），但必须在行内说明原因。
   *     静默跳过会让人以为"同步过了"，而实际上那一行还指着仓库最新。
   *
   *  3. **标记为"用户已选择"**：仓库标签可能还在回来的路上，它回来时只覆盖
   *     用户没碰过的行 —— 不同步标记的话，刚同步好的值会被仓库最新冲掉，
   *     而界面上没有任何提示。
   *
   * 返回同步/跳过/未动 的条数，供调用方提示 —— 只说"已同步"会掩盖后两类。
   */
  const syncFromPrev = useCallback(() => {
    let synced = 0;
    let unreadable = 0;
    let same = 0;
    // ⚠️ 读值放在 setTargets 的 updater **外面**：React 在开发模式下会重复调用
    // updater，把计数写在里面会让条数翻倍（表现是"已同步 12 个"而实际只有 6 个）。
    const next: Record<string, string> = { ...targetsRef.current };
    picked.forEach((r) => {
      const t = prevInfoOf(r.projectKey).tag;
      if (!t) {
        unreadable += 1;
        return;
      }
      if (bareTagOf(next[r.projectKey]) === t) {
        same += 1;
        return;
      }
      // 统一记成仓库来源：它与仓库标签是同一类东西（仓库里实际存在的 tag），
      // 后端走的也是同一条取镜像链路。只有"候选里没有它"这一点需要单独补进去。
      next[r.projectKey] = SRC_REGISTRY + t;
      targetTouchedRef.current.add(r.projectKey);
      synced += 1;
    });
    setTargets(next);
    return { synced, unreadable, same };
  }, [picked]);

  /**
   * "仓库里连一个镜像都没有"的服务。
   *
   * 判据用**平台库的 `svc.tag`**而不是集群实况：后端建单时走 `resolveDeployImage`
   * 从镜像记录里取版本，与集群读不读得到无关。这里若改用集群实况，
   * 集群一断就会把一批其实能发的服务误报成"提交必然失败"。
   */
  const noImage = picked.filter((r) => !r.latest && !r.platformTag);
  /**
   * 版本比对结果按三档统计。
   *
   * 读不到集群的服务既不能算"变更"也不能算"同版本"——那两种说法都是在断言
   * 我们并不知道的现状。单列一档，界面上如实说"无法比对"。
   */
  // 与「当前运行版本」比对时要去掉来源前缀（`r:1.2.3` → `1.2.3`），
  // 否则任何选择都会因为多了两个字符被判成"有变更"。
  const changeCount = picked.filter(
    (r) => r.currentKnown && bareTagOf(targets[r.projectKey]) !== r.currentTag,
  ).length;
  const sameCount = picked.filter(
    (r) => r.currentKnown && bareTagOf(targets[r.projectKey]) === r.currentTag,
  ).length;
  const unknownCount = picked.length - changeCount - sameCount;
  /** 该环境下已有进行中部署单的服务（后端会拒绝并发发布，这里提前挡掉） */
  const inflightRows = useMemo(() => rows.filter((r) => r.inflight), [rows]);
  /** 读不到集群状态、因而无法核对当前版本的服务 */
  const unknownRows = useMemo(() => picked.filter((r) => !r.currentKnown), [picked]);

  /**
   * 权限门控（「能勾选 == 后端受理」）。
   *
   * 本向导**只有一个目标环境**——上方那排环境卡片单选出 `envKey`，
   * 被勾选的所有服务都发往同一个环境，所以判据就是 `canDeployOn(envKey)`
   * （即权限点 `deploy:{envKey}`），与后端 `POST /deployments` handler 里的
   * `requireEnvPerm("deploy", envKey)` 一字不差。缺权限时后端回
   * 40300「缺少权限点：deploy:{env}」，不允许走到提交那一步才失败。
   */
  const canDeploy = !!envKey && canDeployOn(envKey);
  const deployPermTip = !envKey
    ? '请先选择目标环境'
    : canDeploy
      ? undefined
      : `当前账号缺少权限点 deploy:${envKey}，无法向「${env?.name ?? envKey}」发起部署；请联系管理员授予该环境的部署权限`;

  /**
   * 批次号的**展示形态**。
   *
   * BatchRunner 头部把它渲染成等宽蓝底 chip（styles/batch.css 的 `.yn-batch__no`），
   * 原生形态 `DP260929-123456` 与后端部署单号 `DP202609230013` 极像，会被
   * 误当成能在「部署记录」里检索的单号。这里前置定性文案，明确它只是本次界面标识。
   */
  const batchNoLabel = batchNo ? `本次批次（界面标识）${batchNo}` : '';

  // ------------------------------------------------------------------ 发布

  /**
   * 部署并发上限（生效值，来自「系统管理 › 平台参数 › 运行参数」）。
   *
   * ⚠️ 这里**不能写死 1**：后端部署池的大小就是按这个值设的，
   * 前端若固定 1 个 1 个提交，用户把并发调成 5 也照样一个个发 ——
   * 表面上是"界面慢"，实际是前端把后端的并发能力压掉了。
   *
   * 读不到（没权限 / 请求失败）时退回 1：串行是安全侧，
   * 而"以为在并发、其实后端在排队"只是慢，不会出错。
   */
  const runtimeApi = useApi(() => systemApi.runtime(), []);
  const deployConcurrency = Math.max(1, runtimeApi.data?.deployConcurrency ?? 1);

  /** 本批次的公共提交参数 */
  const startPayloadRef = useRef<{
    envKey: string;
    /** 每个项目最终提交什么（平台 tag 或仓库 tag，二选一，见 DeployPick） */
    pickOf: Record<string, DeployPick>;
    /** 该环境配置的审批人（来自环境 DTO，见下方 startTask 注释） */
    approvers: string[];
  }>({
    envKey: '',
    pickOf: {},
    approvers: [],
  });
  const lastOkRef = useRef<Set<string>>(new Set());
  const batchNoRef = useRef('');
  batchNoRef.current = batchNo;

  const start = () => {
    /**
     * **前端本次批次标识，不是后端单据号。**
     *
     * 后端没有"批次"实体：批量发布产生的是一批各自独立的部署单，每单有自己的
     * 部署单号（形如 DP202609230013）。这个 `DP…` 编号只存在于本次界面会话，
     * 唯一用途是拼幂等键保证「同一次点击不重复建单」。它**不可在「部署记录」里检索**，
     * 所以对外展示时必须写明是界面标识（见下方 batchNoLabel 与结束通知），
     * 否则它的形态会冒充后端部署单号。
     */
    const no = `DP${new Date().toISOString().slice(2, 10).replace(/-/g, '')}-${Date.now()
      .toString()
      .slice(-6)}`;
    const pickOf: Record<string, DeployPick> = {};
    picked.forEach((r) => {
      const v = targets[r.projectKey];
      // 兜底顺序：用户没选 → 平台最新 → 平台库里的 tag。
      // 兜底用 `platformTag` 而不是集群实况：后端是按 tag 去平台镜像记录里找版本的，
      // 集群里跑着什么对这次提交没有影响。
      const tag = bareTagOf(v) || r.latest?.tag || r.platformTag || '';
      // 按**选择的来源**分流，绝不同时带上两个字段（后端会拒）。
      pickOf[r.projectKey] = v?.startsWith(SRC_REGISTRY)
        ? { registryTag: tag }
        : { platformTag: tag };
    });

    startPayloadRef.current = { envKey, pickOf, approvers: env?.approvers ?? [] };
    lastOkRef.current = new Set();

    setBatchNo(no);
    setTasks(
      picked.map((r) => ({
        id: `bd_${r.projectKey}`,
        title: r.projectName,
        subtitle: `${r.projectKey} → ${env?.name ?? envKey}`,
        short: r.projectKey,
        projectKey: r.projectKey,
        imageHint: bareTagOf(targets[r.projectKey]) || r.latest?.tag || r.platformTag,
      })),
    );
    setResult(null);
    setRound((n) => n + 1);
    setConfirmOpen(false);
    setStep(2);
  };

  /**
   * 提交单个服务发布。
   *
   * 幂等键按「批次 + 环境 + 项目」生成 —— 同一次点击只落一个部署单。
   * 注意 **不传 replicas**：副本数由平台按环境与服务的现有配置决定，
   * 发布会把它当成"改副本"的操作（那属于服务参数变更，走另一个接口）。
   */
  const startTask = useCallback(async (task: BatchTaskDef): Promise<BatchTaskHandle> => {
    const { envKey: ek, pickOf, approvers } = startPayloadRef.current;
    const pick: DeployPick = pickOf[task.projectKey] ?? { platformTag: '' };
    const d = await deployApi.create({
      envKey: ek,
      projectKey: task.projectKey,
      // 二选一（DeployPick 就是联合类型）：选仓库时**不带 tag** ——
      // 两个都给会被后端 `checkImageSource` 拒成 400，而那条错误到第 3 步才出现，
      // 用户看到的会是"某个服务发布失败"，与真实原因（参数冲突）对不上。
      ...('registryTag' in pick
        ? { imageSource: 'registry' as const, registryTag: pick.registryTag }
        : { imageSource: 'platform' as const, tag: pick.platformTag }),
      strategy: 'RollingUpdate',
      // remark 会落到每张部署单上、在「部署记录」里可见，是各单与该次点击之间的关联线索，
      // 故这里写明是「界面批次」而不是后端批次单号
      remark: `批量发布（界面批次 ${batchNoRef.current}）`,
      idempotencyKey: `${batchNoRef.current}:${ek}:${task.projectKey}`,
    });
    return {
      runId: String(d.id),
      // 不用响应里的 logStreamUrl：相对路径，必须经 streamUrl() 拼令牌
      stream: deployStatusStream(d.id, d.status),
      runNo: d.deployNo,
      awaitingApproval: d.status === 'pending_approval',
      /**
       * 审批人优先用响应里的；响应里没有就退回环境配置的审批人。
       *
       * 后端 `createDeployPayload` 只在 `d.Approver != ""` 时才下发 `approvers`，
       * 而 `DeployDTO.Approver` 唯一的数据源是 `DeployRecord.ApproverName`——
       * 那个字段只在**审批通过**时才写（`approveDeployment`），建单时为空的。
       * 所以待审批单永远拿不到审批人，面板上只能写"等待审批"。
       * 环境 DTO 的 `approvers` 是同一份配置的权威来源，用它兜底，
       * 面板就能写出"等待 运维示例、林曦 审批"而不是一句没有主语的"等待审批"。
       */
      approvers: d.approvers?.length ? d.approvers : approvers.length ? approvers : undefined,
    };
  }, []);

  const fetchOutcome = useCallback(async (runId: string): Promise<BatchTaskOutcome> => {
    const d = await deployApi.get(runId);
    // rolled_back 也是"这次发布把环境改成了目标版本，只是标记为回滚来源"，
    // 对批量发布来说它是成功的；真正失败的是 failed / canceled。
    const ok = d.status === 'success' || d.status === 'rolled_back';
    return {
      ok,
      status: d.status,
      detail: ok
        ? `${d.deployNo} 已更新为 ${d.image.split(':').pop()}`
        : d.status === 'interrupted'
          ? // 中断的**不能**混进"部署失败"：没有任何失败原因可归因，
            // 这句话要能直接回答"那我现在该干什么"。
            `${d.deployNo} 因平台重启中断，未更新到目标版本；需重新发起`
          : d.aiDiagnosis?.reason || `部署${d.status === 'canceled' ? '已取消' : '失败'}`,
    };
  }, []);

  const cancelRun = useCallback(async (runId: string) => {
    await deployApi.cancel(runId, '用户在批量发布中取消');
  }, []);

  const handleFinish = (r: BatchResult) => {
    setResult(r);
    onCompleted?.(r);
    // interrupted 也算"未成功"：平台重启打断的服务**没有**变成目标版本，
    // 漏掉它会让一批跑了一半的发布弹出绿色的「批量发布完成」。
    const allOk = !r.failed && !r.canceled && !r.interrupted;
    notification[allOk ? 'success' : 'warning']({
      message: allOk ? '批量发布完成' : '批量发布结束（存在未成功项）',
      description: (
        <span>
          已向 <b>{env?.name ?? envKey}</b> 提交 <b>{r.total}</b> 个服务：成功{' '}
          <b style={{ color: '#52c41a' }}>{r.success}</b>
          {r.failed ? (
            <>
              ，失败 <b style={{ color: '#ff4d4f' }}>{r.failed}</b>（失败的服务保留旧版本）
            </>
          ) : null}
          {r.interrupted ? (
            <>
              ，已中断 <b style={{ color: '#fa8c16' }}>{r.interrupted}</b>（平台重启打断，未更新到目标版本，需重新发布）
            </>
          ) : null}
          {r.pending ? (
            <>
              ，待审批 <b style={{ color: '#fa8c16' }}>{r.pending}</b>
            </>
          ) : null}
          {r.canceled ? `，取消 ${r.canceled}` : null}。本次批次（界面标识）<b>{batchNo}</b>
          ——它只在本次界面内有效，不是后端部署单号；各服务的部署单号可在「部署记录」里对号查看。
        </span>
      ),
      placement: 'topRight',
      duration: 8,
    });
  };

  // ------------------------------------------------------------------ 列定义

  /**
   * 「当前版本」单元格。
   *
   * 四态必须分开说 —— 它们指向完全不同的下一步动作，混成一句"暂无数据"
   * 会让人往错的方向排查：
   *   未纳管       → 去「环境详情」把项目纳管进来；
   *   读取不到     → 去查平台到集群的连通与凭据（不是去纳管）；
   *   集群中不存在 → 平台有实例记录但集群里没有 Deployment，直接发布一次即可建出来；
   *   正常         → 展示集群里实跑的 tag 与实时状态。
   */
  const currentCell = (r: RowModel) => {
    if (r.undeployed) {
      /*
       * 这里以前分两档：linked=true 说"已纳管，发布后会自动创建"，linked=false 说"未纳管"。
       * 前半句是错的——后端建单时要求服务实例已经存在，没实例直接 404，
       * 不会"发布后自动创建"。既然两条路都发不出去，就不该给用户两种说法。
       */
      return (
        <Tooltip
          title={`${env?.name ?? ''} 还没有这个服务的实例，批量发布只能更新已纳管的服务。请先到「环境管理 → 环境详情」把「${r.projectName}」纳管进来（${r.linked ? '本环境已关联该项目，但缺少服务实例' : '本环境未关联该项目'}）。`}
        >
          <Tag style={{ marginInlineEnd: 0 }}>未纳管</Tag>
        </Tooltip>
      );
    }
    if (!r.currentKnown) {
      return (
        <Tooltip
          title={`读取不到集群状态：${r.svc ? liveFallbackText(r.svc) : '未取得集群状态'}。集群恢复前，这一列与右侧「变更」列都无法核对——不代表该服务没有部署。`}
        >
          <Tag style={{ marginInlineEnd: 0, cursor: 'help' }}>读取不到</Tag>
        </Tooltip>
      );
    }
    if (!r.exists) {
      return (
        <Tooltip
          title={`平台记录了该服务的实例，但集群 ${
            env?.namespace ?? ''
          } 命名空间里没有对应的 Deployment。可能被手工删除过，或命名空间 / 工作负载名与实际不一致；直接发布一次即可重新建出来。`}
        >
          <Tag color="orange" style={{ marginInlineEnd: 0, cursor: 'help' }}>
            集群中不存在
          </Tag>
        </Tooltip>
      );
    }
    return (
      <Space size={6} style={{ maxWidth: '100%' }}>
        <TagText value={r.currentTag} variant="now" />
        {r.live ? <ServiceStatusTag status={liveStatusWord(r.live.status)} /> : null}
      </Space>
    );
  };

  /**
   * 「前一环境当前版本」列 —— 第 1 步与第 2 步**共用同一个定义**。
   *
   * 为什么不各写一份：本项目的环境级动作曾经因为"定义在视图层、而视图有两个"
   * 漂移过一次（见 EnvironmentActions 的注释）。版本对比列比按钮更容易漂 ——
   * 一处按链路序号、另一处按默认阶段表，同一件事在两个步骤里就会给出不同的对照环境。
   * 要做成函数而不是常量：列头文案依赖 `prevEnv` / `env`，它们是运行期状态。
   */
  const prevColumn = (): ColumnsType<RowModel>[number] => ({
    // 列头只写**环境名**，不写"当前版本" —— 后者会让表头折成两行（环境名 + 4 个字，
    // 在 96px 里放不下），而"这一列是版本号"从单元格内容就看得出来。
    // 完整口径（含链路第几位）放 tooltip，不占版面。
    title: prevEnv ? (
      <Tooltip
        title={`${prevEnv.name}（${prevEnv.key}）正在跑的版本 · 链路第 ${stageOf(prevEnv).order} 位${
          prevEnv.stageLabel ? ` · ${prevEnv.stageLabel}` : ''
        }${
          typeof prevEnv.stageOrder !== 'number'
            ? '。该环境没有设置「链路序号」，是靠默认阶段表排到这里的 —— 若要固定顺序，请在「编辑环境」里给它设一个序号'
            : ''
        }　只展示 ${MAX_COMPARE_ENVS} 个环境（目标 + 前一环）`}
      >
        <span style={{ cursor: 'help' }}>{prevEnv.name}</span>
      </Tooltip>
    ) : (
      '前一环境'
    ),
    key: 'prev',
    // 114：够放 10 个字符的 tag（12.5px 等宽约 7.5px/字）；TagText 带 tooltip 补全。
    width: 114,
    className: 'yn-col-ref',
    render: (_, r) => {
      const p = prevInfoOf(r.projectKey);
      if (p.tag) return <TagText value={p.tag} variant="ref" />;
      // 读不到 / 未纳管 / 集群里没有：三种情况各有原因，原样说出来。
      // 留空会被读成"前一环境没有这个服务"，那是另一个结论。
      return (
        <Tooltip title={p.reason}>
          <Text type="secondary" style={{ fontSize: 12, cursor: 'help' }}>
            —
          </Text>
        </Tooltip>
      );
    },
  });

  const pickColumns: ColumnsType<RowModel> = [
    {
      title: '服务',
      key: 'svc',
      width: 252,
      render: (_, r) => (
        <div style={{ display: 'flex', alignItems: 'center', gap: 6, minWidth: 0 }}>
          <span style={{ display: 'inline-flex', flex: 'none' }}>
            <ProjectTypeTag type={r.projectType} />
          </span>
          <b className="yn-ellipsis" style={{ flex: '1 1 auto', minWidth: 0, fontWeight: 500 }}>
            {r.projectName}
          </b>
          <span className="yn-mono" style={{ flex: 'none', color: '#9aa0aa', fontSize: 11 }}>
            {r.projectKey}
          </span>
        </div>
      ),
    },
    prevColumn(),
    {
      title: `${env?.name ?? ''} 当前版本`,
      key: 'current',
      // 两列版本号并排后总宽会顶到容器外面，各自收窄；TagText 带 tooltip 补全全值。
      width: 168,
      render: (_, r) => currentCell(r),
    },
    {
      // ⚠️ 这一列是**平台构建记录**里的最新，不是镜像仓库里的最新。
      // 标题写"仓库最新版本"会被读成"镜像仓库"，从而以为它比目标版本更权威 ——
      // 仓库口径要到「核对版本」那一步才读（那时才有目标版本下拉可选）。
      title: '平台最新版本',
      key: 'latest',
      width: 178,
      render: (_, r) =>
        r.latest ? (
          <Space size={6} style={{ maxWidth: '100%' }}>
            <TagText value={r.latest.tag} variant="now" />
            <Tooltip
              // size 后端给的是**字节**，拼成 `${size}MB` 会差 6 个数量级；
              // 而且读不到时它是 null，会渲染成 "nullMB"。统一走 humanSize。
              title={`构建号 #${r.latest.buildNo} · 分支 ${r.latest.branch} · ${humanSize(r.latest.size)} · 推送于 ${r.latest.pushedAt}`}
            >
              <Text type="secondary" style={{ fontSize: 11, cursor: 'help', flex: 'none' }}>
                #{r.latest.buildNo}
              </Text>
            </Tooltip>
          </Space>
        ) : (
          <Text type="secondary">仓库中暂无镜像</Text>
        ),
    },
    {
      title: '变更',
      key: 'diff',
      width: 132,
      render: (_, r) => {
        // 「这个服务这次发布能变什么」始终要看得见——被并发发布挡住不代表版本状态不重要，
        // 所以两种情况都保留原标签，进行中只是叠在下面，而不是把它顶掉。
        // 没有服务实例的行不存在"变更"可谈（后端根本不受理），单独一个"需先纳管"。
        const change = r.undeployed ? (
          <Tag color="orange" style={{ marginInlineEnd: 0 }}>
            需先纳管
          </Tag>
        ) : !r.currentKnown ? (
          // 「已最新」是一个断言，而这里的现状我们并不知道 —— 不能默认成"没变化"，
          // 那会让"已经是最新了，不用发"的判断建立在一个没读到的值上。
          <Tooltip title="读取不到集群当前版本，无法判断这次发布是否构成变更（不代表没有变更）。">
            <Tag style={{ marginInlineEnd: 0, cursor: 'help' }}>
              无法比对
            </Tag>
          </Tooltip>
        ) : !r.exists ? (
          <Tooltip title="集群里目前没有这个 Deployment，本次发布会新建一个。">
            <Tag color="orange" style={{ marginInlineEnd: 0, cursor: 'help' }}>
              将新建
            </Tag>
          </Tooltip>
        ) : hasUpdateOf(r) ? (
          <Tag color="orange" style={{ marginInlineEnd: 0 }}>
            有更新
          </Tag>
        ) : (
          <Tag color="default" style={{ marginInlineEnd: 0 }}>
            已最新
          </Tag>
        );
        if (!r.inflight) return change;
        const pending = r.inflight.status === 'pending_approval';
        return (
          <Space direction="vertical" size={2}>
            {change}
            <Tooltip
              title={`已有进行中的部署单 ${r.inflight.deployNo}（${
                pending ? '待审批' : '执行中'
              }）。同一环境的同一服务不允许并发发布，请等它结束，或先在「部署记录」里取消。`}
            >
              <Tag color={pending ? 'warning' : 'processing'} style={{ marginInlineEnd: 0 }}>
                {pending ? '待审批中' : '发布中'}
              </Tag>
            </Tooltip>
          </Space>
        );
      },
    },
  ];

  const confirmColumns: ColumnsType<RowModel> = [
    {
      title: '服务',
      key: 'svc',
      width: 210,
      render: (_, r) => (
        <Space size={6}>
          <span style={{ fontWeight: 500 }}>{r.projectName}</span>
          <Text className="yn-mono" type="secondary" style={{ fontSize: 11 }}>
            {r.projectKey}
          </Text>
        </Space>
      ),
    },
    prevColumn(),
    {
      // 列头只写环境名 —— 两个环境名并排（前一环 / 目标），谁是参考、谁是目标一目了然。
      // 写"… 当前版本"会把 8 个汉字的环境名挤到换行（实测「机房测试环境152」折成两行），
      // 而"这一列是版本号"从单元格内容就看得出来。完整口径放 tooltip。
      title: (
        <Tooltip title={`${env?.name ?? ''}（${env?.key ?? ''}）集群里正在跑的版本`}>
          <span style={{ cursor: 'help' }}>{env?.name ?? '当前版本'}</span>
        </Tooltip>
      ),
      key: 'current',
      // 收窄到 158：列头是环境名，值靠 TagText 的 tooltip 补全。
      width: 158,
      render: (_, r) => currentCell(r),
    },
    {
      title: '确认发布版本（可修改）',
      key: 'target',
      // 收窄到 300（原 380）：下拉本身已收到 236，加上下面的来源标签与原因说明够用。
      // 用户 2026-10-15 明确要求"这一列展示小一点"——加了对比列之后总宽会顶出容器。
      width: 300,
      render: (_, r) => {
        const reg = r.registry;
        const regTags = reg && !reg.unavailable ? reg.tags : [];
        // 两组候选：**仓库在前**（用户要的正是它），平台构建在后。
        // 同名 tag 会在两组里各出现一次 —— 这是刻意的，语义不同：
        // 仓库那条是"仓库里确实存在这个 tag"（可能是别人推的），
        // 平台那条带 image_record（有构建号 / commit / 推送时间可溯源）。
        const groups: { label: string; options: { label: string; value: string }[] }[] = [];
        if (regTags.length) {
          groups.push({
            label: `仓库标签（最近 ${regTags.length} 个${
              reg!.timeKind === 'build' ? ' · 时间是镜像构建时间' : ''
            }）`,
            options: regTags.map((t, i) => ({
              label: `${t.tag}${i === 0 ? ' · 最新' : ''}　${regTimeText(t)}`,
              value: SRC_REGISTRY + t.tag,
            })),
          });
        }
        /**
         * 「同步版本号」送进来的前一环境版本，若**不在**上面的仓库候选里，补一条。
         *
         * 不补的话 `Select` 拿到一个不在 options 里的 value 会显示成**空**：
         * 用户看着"这一行没选版本"，提交时却带着那个 tag —— 比直接报错更难发现。
         * 标出来源是为了不把它与"仓库里真实存在的标签"混为一谈：
         * 它是一个**前一环境正在跑**的版本，仓库里现在还有没有它，平台没有验证过。
         */
        const chosen = bareTagOf(targets[r.projectKey]);
        const chosenIsRegistry = targets[r.projectKey]?.startsWith(SRC_REGISTRY);
        if (chosen && chosenIsRegistry && !regTags.some((t) => t.tag === chosen)) {
          const g = groups.find((x) => x.label.startsWith('仓库标签'));
          const opt = {
            label: `${chosen}　(前一环境在跑，仓库近期列表里没有它)`,
            value: SRC_REGISTRY + chosen,
          };
          if (g) g.options.unshift(opt);
          else groups.unshift({ label: '来自前一环境', options: [opt] });
        }
        const plat = r.versions.slice(0, 12).map((i, idx) => ({
          label: `${i.tag}　(#${i.buildNo}${idx === 0 ? ' · 最新' : ''})`,
          value: SRC_PLATFORM + i.tag,
        }));
        if (r.currentTag && !r.versions.some((i) => i.tag === r.currentTag)) {
          // 集群里在跑、但已不在平台的候选里：留着它，否则"再发一次当前版本"没有选项。
          plat.push({ label: `${r.currentTag}　(当前运行版本)`, value: SRC_PLATFORM + r.currentTag });
        }
        if (plat.length) groups.push({ label: '平台构建', options: plat });

        if (!groups.length) {
          return (
            <Text type="secondary" style={{ fontSize: 12 }}>
              {regLoading ? '正在读取仓库标签…' : '仓库中没有可用镜像，无法发布'}
            </Text>
          );
        }
        return (
          <Space direction="vertical" size={4} style={{ width: '100%' }}>
            <Space size={6} style={{ width: '100%' }} align="center">
              <Tooltip title={chosen || undefined}>
                <Select
                  className="yn-target-select"
                  showSearch
                  size="small"
                  // 收窄到 208（原 350）：加了「前一环境」列之后总宽会顶出容器，
                  // 而这一列的信息量主要在**选中的那个 tag**，靠 labelRender 只显示 tag，
                  // 完整说明（构建时间等）留在下拉里看。
                  style={{ width: 208 }}
                  // ⚠️ **不要**再传 optionLabelProp：它和 labelRender 同时给会让选中值
                  // 渲染出**带来源前缀的裸 value**（实测显示成 `r:1.2.0.3417`）——
                  // 那是内部标识泄漏到了界面上，用户会以为版本号真长这样。
                  // labelRender 已经能完全控制选中态的显示。
                  // 只显示裸 tag，不显示 label 里那一长串（"· 最新　构建于 …"）——
                  // 选中态是**结果**，不是候选列表，把说明塞进去会把列撑爆。
                  // 完整值走外层 Tooltip，来源（仓库 / 平台）走右边那个小标记。
                  labelRender={() => (
                    <span className="yn-ver__tag">{chosen || '选择版本'}</span>
                  )}
                  // 只有"仓库还没回来、且平台那组也没有内容"时才转圈；
                  // 平台组已经有了却一起转圈，会让用户以为下拉是空的、点不了。
                  loading={regLoading && !r.versions.length}
                  value={targets[r.projectKey]}
                  onChange={(v) => {
                    // 记下"这一行用户动过"：仓库标签后到时不再覆盖他的选择。
                    targetTouchedRef.current.add(r.projectKey);
                    setTargets((prev) => ({ ...prev, [r.projectKey]: v }));
                  }}
                  options={groups}
                  placeholder="选择版本"
                />
              </Tooltip>
              {/* 来源：同名 tag 在"仓库标签"和"平台构建"下走的是两条不同的取镜像链路，
                  事后查"这次发的是哪一份"时它是关键线索，所以常显、不藏进 tooltip。
                  用极轻的描边小签而不是实心 Tag：它是**说明**，不是状态。 */}
              {chosen ? (
                chosenIsRegistry ? (
                  <Tooltip title="取自镜像仓库里实际存在的标签（含别人推的、Jenkins 推的）">
                    <span className="yn-src-badge">仓库</span>
                  </Tooltip>
                ) : (
                  <Tooltip title="取自平台自己构建过的镜像记录（带构建号 / 提交号可溯源）">
                    <span className="yn-src-badge yn-src-badge--plat">平台</span>
                  </Tooltip>
                )
              ) : null}
            </Space>
            {/* 仓库读不到必须说明原因。否则"下拉里没有仓库那一组"会被读成
                "仓库里没有标签" —— 两者的处置完全不同。 */}
            {reg?.unavailable ? (
              <Tooltip title={reg.unavailable}>
                <Text
                  type="secondary"
                  className="yn-ellipsis"
                  style={{ fontSize: 11, display: 'block', maxWidth: 300, cursor: 'help' }}
                >
                  {reg.unavailable}
                </Text>
              </Tooltip>
            ) : r.registry === null ? (
              <Text type="secondary" style={{ fontSize: 11 }}>
                读仓库标签请求失败，仅列出平台构建的版本
              </Text>
            ) : null}
          </Space>
        );
      },
    },
    {
      title: '副本',
      key: 'replicas',
      width: 70,
      // 副本数是集群里实跑的个数；读不到就留白，不拿平台库的记录充数。
      // 本次提交也不带 replicas（后端按现有配置执行），所以这一列纯属核对用。
      render: (_, r) =>
        r.replicas === undefined ? (
          <Tooltip title="读取不到集群，无法取得当前副本数">
            <Text type="secondary" style={{ cursor: 'help' }}>
              —
            </Text>
          </Tooltip>
        ) : (
          <span>{r.replicas}</span>
        ),
    },
    {
      title: '操作',
      key: 'op',
      width: 90,
      render: (_, r) => (
        <Button
          type="link"
          size="small"
          danger
          onClick={() => setSkipped((prev) => [...prev, r.projectKey])}
        >
          移出批次
        </Button>
      ),
    },
  ];

  return (
    <Modal
      open={open}
      onCancel={onClose}
      width={1120}
      title={
        <Space size={10}>
          <RocketOutlined style={{ color: '#1677ff' }} />
          批量发布
          <Tag color="geekblue" style={{ marginInlineEnd: 0 }}>
            多服务一次发版 · 读取仓库版本 · 串行发布 · 实时日志
          </Tag>
        </Space>
      }
      footer={null}
      destroyOnClose
      maskClosable={false}
      className="yn-batch-modal"
    >
      <Steps
        size="small"
        current={step}
        className="yn-batch__steps"
        items={[
          { title: '选择服务', description: keys.length ? `已选 ${keys.length} 个` : '勾选要发布的微服务' },
          { title: '核对版本', description: '读取仓库镜像与当前实例' },
          { title: '排队发布', description: '逐服务滚动更新' },
        ]}
      />

      {/* ---------------------------------------------------------- 步骤 1 */}
      {step === 0 ? (
        <div className="yn-pick">
          <div className="yn-envpick">
            <span className="yn-envpick__label">目标环境</span>
            {envs.map((e) => {
              const on = e.key === envKey;
              return (
                <button
                  type="button"
                  key={e.key}
                  className={`yn-envpick__card ${on ? 'is-on' : ''}`}
                  onClick={() => setEnvKey(e.key)}
                >
                  <div className="yn-envpick__head">
                    <b>{e.name}</b>
                    <code>{e.key}</code>
                    {e.needApproval ? <Tag color="orange">需审批</Tag> : null}
                  </div>
                  <div className="yn-envpick__meta">
                    <span>
                      <CloudServerOutlined /> {e.cluster}
                    </span>
                    <span>
                      <DeploymentUnitOutlined /> {e.namespace}
                    </span>
                  </div>
                </button>
              );
            })}
          </div>

          {!loading && inflightRows.length ? (
            <Alert
              type="warning"
              showIcon
              style={{ marginBottom: 12 }}
              message={`${inflightRows.length} 个服务在 ${env?.name ?? envKey} 已有进行中的部署单，本次不可选择`}
              description={`${inflightRows
                .map(
                  (r) =>
                    `${r.projectKey}（${r.inflight!.deployNo}·${
                      r.inflight!.status === 'pending_approval' ? '待审批' : '执行中'
                    }）`,
                )
                .join('、')}。同一环境的同一服务不允许并发发布，请等待其结束，或先在「部署记录」中取消。`}
            />
          ) : null}

          {loadError ? (
            <Alert
              type="error"
              showIcon
              style={{ marginBottom: 12 }}
              message="数据加载失败"
              description={loadError}
              action={
                <Button size="small" onClick={() => void loadRows(envKey)}>
                  重试
                </Button>
              }
            />
          ) : null}

          <div className="yn-pick__bar">
            <Input
              allowClear
              size="small"
              prefix={<SearchOutlined style={{ color: '#b5bac4' }} />}
              placeholder="搜索服务名称 / 标识"
              style={{ width: 220 }}
              value={kw}
              onChange={(e) => setKw(e.target.value)}
            />
            <Segmented
              size="small"
              value={filter}
              onChange={(v) => setFilter(v as FilterKey)}
              options={[
                { label: `全部 ${rows.length}`, value: 'all' },
                { label: `有更新 ${counts.update}`, value: 'update' },
                { label: `未部署 ${counts.undeployed}`, value: 'undeployed' },
                { label: `已最新 ${counts.same}`, value: 'same' },
              ]}
            />
            <span className="yn-pick__bar-spacer" />
            <Space size={6}>
              <Button
                size="small"
                onClick={() =>
                  setKeys((prev) => [
                    ...new Set([
                      ...prev,
                      ...filtered
                        .filter((r) => r.hasBuild && !r.undeployed && !r.inflight)
                        .map((r) => r.projectKey),
                    ]),
                  ])
                }
              >
                全选当前 {filtered.length}
              </Button>
              <Button
                size="small"
                onClick={() =>
                  setKeys((prev) => [
                    ...new Set([
                      ...prev,
                      ...filtered
                        .filter((r) => hasUpdateOf(r) && r.hasBuild && !r.undeployed && !r.inflight)
                        .map((r) => r.projectKey),
                    ]),
                  ])
                }
              >
                仅选有更新 {counts.update}
              </Button>
              <Button size="small" onClick={() => setKeys([])} disabled={!keys.length}>
                清空
              </Button>
            </Space>
          </div>

          <div className="yn-pick__table">
            <Table<RowModel>
              rowKey={(r) => r.projectKey}
              size="small"
              loading={loading}
              dataSource={filtered}
              columns={pickColumns}
              scroll={{ y: 320, x: 900 }}
              pagination={false}
              rowSelection={{
                selectedRowKeys: keys,
                onChange: (ks) => setKeys(ks as string[]),
                // 三条硬约束：没有构建产物（无镜像）、没有服务实例（后端不受理）、已有进行中部署（后端会拒）
                getCheckboxProps: (r) => ({
                  disabled: !r.hasBuild || r.undeployed || !!r.inflight,
                }),
              }}
            />
          </div>

          <div className="yn-pick__summary">
            <Space size={8} wrap>
              <Text style={{ fontSize: 13 }}>
                已选择 <b style={{ color: '#1677ff' }}>{keys.length}</b> 个服务将发布到 <b>{env?.name}</b>
              </Text>
              {counts.update ? <Tag color="orange">其中 {counts.update} 个版本有更新</Tag> : null}
              {counts.undeployed ? (
                <Tooltip title="这些服务在本环境还没有服务实例，批量发布不受理。请先到「环境管理 → 环境详情」把它们纳管进来，之后再回来发布。">
                  <Tag color="default" style={{ marginInlineEnd: 0, cursor: 'help' }}>
                    {counts.undeployed} 个未纳管，需先纳管后才能发布
                  </Tag>
                </Tooltip>
              ) : null}
              {env?.needApproval ? (
                <Tag color="orange" style={{ marginInlineEnd: 0 }}>
                  该环境需审批，提交后由 {env.approvers.join('、')} 确认
                </Tag>
              ) : null}
            </Space>
          </div>
        </div>
      ) : null}

      {/* ---------------------------------------------------------- 步骤 2 */}
      {step === 1 ? (
        <div className="yn-scan">
          <div className={`yn-scan__panel ${!loading ? 'is-done' : ''}`}>
            <div className="yn-scan__head">
              <Space size={8}>
                <CloudDownloadOutlined style={{ color: loading ? '#1677ff' : '#52c41a' }} />
                <b>读取仓库镜像与当前部署实例</b>
                <Text type="secondary" style={{ fontSize: 12 }}>
                  {env?.registry || '镜像仓库'}
                </Text>
              </Space>
              <Tag color={loading ? 'processing' : 'success'} style={{ marginInlineEnd: 0 }}>
                {loading ? scanNote || '读取中' : `仓库镜像 ${imageCount} 条 · 已匹配 ${rows.length} 个服务`}
              </Tag>
            </div>
            <div className="yn-scan__lines">
              {loading ? (
                <div className="yn-scan__line">
                  <Spin size="small" />
                  <span className="yn-mono">{scanNote || '正在读取…'}</span>
                </div>
              ) : (
                <div className="yn-scan__line">
                  <CheckCircleFilled style={{ color: '#52c41a', fontSize: 11 }} />
                  <span className="yn-mono">
                    已读取 {imageCount} 条镜像记录，匹配到 {rows.filter((r) => r.latest).length} 个有产物的服务
                    {rows.filter((r) => !r.hasBuild).length
                      ? `；${rows.filter((r) => !r.hasBuild).length} 个服务暂无构建产物（已在列表中禁用勾选）`
                      : ''}
                  </span>
                </div>
              )}
            </div>
          </div>

          {!loading && !loadError ? (
            <>
              <div className="yn-scan__bar">
                <Space size={8} wrap>
                  <Text style={{ fontSize: 13 }}>
                    共 <b>{picked.length}</b> 个服务待发布到 <b>{env?.name}</b>
                  </Text>
                  {changeCount ? (
                    <Tag color="orange" style={{ marginInlineEnd: 0 }}>
                      {changeCount} 个版本发生变更
                    </Tag>
                  ) : unknownCount ? (
                    // 有读不到集群的行时**不能说"全部为同版本"** —— 那是在断言我们没读到的东西。
                    <Tooltip
                      title={`${unknownRows
                        .map((r) => r.projectKey)
                        .join('、')} 的集群状态读不到，无法核对当前版本，也就无法确认本次发布是否构成变更。`}
                    >
                      <Tag style={{ marginInlineEnd: 0, cursor: 'help' }}>
                        {unknownCount} 个无法比对版本
                      </Tag>
                    </Tooltip>
                  ) : (
                    <Tag style={{ marginInlineEnd: 0 }}>全部为同版本发布</Tag>
                  )}
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    请在下方逐条核对版本号后再提交
                  </Text>
                </Space>
                <Space size={8}>
                  {/* 一键把目标版本对齐到**前一环境正在跑的版本**：
                      同一份镜像逐环晋级时，两边本来就该是同一个 tag。
                      手动点 6 个下拉、每次都认一遍版本号，慢且容易点错。 */}
                  <DisabledTip
                    title={
                      !prevEnv
                        ? '链路里没有比当前环境更靠前的环境，无法同步。'
                        : !picked.length
                          ? '还没有勾选服务。'
                          : undefined
                    }
                  >
                    <Button
                      size="small"
                      icon={<SwapOutlined />}
                      disabled={!prevEnv || !picked.length}
                      onClick={() => {
                        const { synced, unreadable, same } = syncFromPrev();
                        if (synced) {
                          // 只用 success 会让人以为"全同步好了"；把没动的那两类一并说出来 ——
                          // 它们才是需要用户接手的地方。
                          notification.success({
                            message: `已同步 ${synced} 个服务的版本号`,
                            description: [
                              same ? `${same} 个本来就已经是前一环境的版本` : '',
                              unreadable
                                ? `${unreadable} 个读不到前一环境的状态，保持原样（行内已标出原因）`
                                : '',
                            ]
                              .filter(Boolean)
                              .join('；'),
                            duration: 6,
                          });
                        } else {
                          message.warning(
                            unreadable
                              ? `${unreadable} 个服务读不到前一环境的状态，没有可同步的版本`
                              : '所选服务已经都是前一环境的版本',
                          );
                        }
                      }}
                    >
                      同步版本号
                    </Button>
                  </DisabledTip>
                  <Button size="small" icon={<ReloadOutlined />} onClick={() => void loadRows(envKey)}>
                    重新读取
                  </Button>
                </Space>
              </div>

              {env?.needApproval ? (
                <Alert
                  type="warning"
                  showIcon
                  style={{ marginBottom: 10 }}
                  message={`${env.name} 开启了发布审批`}
                  description={`审批人：${env.approvers.join('、')}。提交后生成的是待审批单据，审批通过才会真正下发到集群；在此之前不会产生执行日志。`}
                />
              ) : null}

              {noImage.length ? (
                <Alert
                  type="error"
                  showIcon
                  style={{ marginBottom: 10 }}
                  message={`${noImage.length} 个所选服务在仓库中没有可用镜像`}
                  description={`${noImage.map((r) => r.projectKey).join('、')} 既没有历史版本也没有新构建产物，提交必然失败。请先把它们移出批次。`}
                />
              ) : null}

              <Table<RowModel>
                rowKey={(r) => r.projectKey}
                size="small"
                dataSource={picked}
                columns={confirmColumns}
                pagination={picked.length > 8 ? { pageSize: 8, size: 'small' } : false}
                // 列宽合计 942 < 弹窗内容区（实测 1057），**不设 x**：让表格宽度自适应，
                // 各列按比例分配剩余空间，不会在右边留一道空档。
                scroll={{ y: 280 }}
              />
            </>
          ) : null}
        </div>
      ) : null}

      {/* ---------------------------------------------------------- 步骤 3 */}
      {step === 2 ? (
        <div className="yn-batch__stage">
          <BatchRunner
            key={round}
            kind="cd"
            batchNo={batchNoLabel}
            tasks={tasks}
            concurrency={deployConcurrency}
            envLabel={env?.name ?? envKey}
            startTask={startTask}
            fetchOutcome={fetchOutcome}
            cancelRun={cancelRun}
            height={330}
            onTaskDone={(task, outcome) => {
              if (outcome.ok) {
                lastOkRef.current.add(task.id);
                const tag = pickTagOf(startPayloadRef.current.pickOf[task.projectKey]);
                if (tag) onServiceDeployed?.(startPayloadRef.current.envKey, task.projectKey, tag);
              } else {
                lastOkRef.current.delete(task.id);
              }
            }}
            onFinish={handleFinish}
            onOpenRecords={() => {
              onClose();
              navigate('/cd/deploys');
            }}
            onRetryFailed={() => {
              setKeys(picked.filter((r) => !lastOkRef.current.has(`bd_${r.projectKey}`)).map((r) => r.projectKey));
              setSkipped([]);
              setStep(1);
              setResult(null);
              message.info('已带出未成功的服务，核对版本后可重新提交');
            }}
          />
        </div>
      ) : null}

      {/* ---------------------------------------------------------- 底部操作 */}
      <div className="yn-batch__actions">
        <div className="yn-batch__actions-left">
          {step === 0 ? (
            <Text type="secondary" style={{ fontSize: 12 }}>
              {deployConcurrency > 1
                ? `提示：最多 ${deployConcurrency} 个服务同时发布（同一服务不允许并发），再多的排队等待`
                : '提示：发布按服务串行排队执行，同一环境不会并发更新，避免滚动发布互相抢占资源'}
              {hasPerm('user:manage')
                ? '。并发数可在「系统管理 › 第三方集成 › 平台参数 → 运行参数」调整，保存后立即生效'
                : ''}
            </Text>
          ) : null}
          {step === 1 && !loading ? (
            <Space size={8}>
              <ExclamationCircleFilled style={{ color: '#faad14' }} />
              <Text type="secondary" style={{ fontSize: 12 }}>
                目标环境：{env?.name} · 集群 {env?.cluster} · Namespace {env?.namespace}
              </Text>
            </Space>
          ) : null}
          {step === 2 && result ? (
            <Space size={8}>
              <CheckCircleFilled
                style={{ color: result.failed || result.interrupted ? '#faad14' : '#52c41a' }}
              />
              <Text style={{ fontSize: 12 }}>
                批次结束：成功 <b style={{ color: '#52c41a' }}>{result.success}</b>
                {result.failed ? (
                  <>
                    {' '}
                    · 失败 <b style={{ color: '#ff4d4f' }}>{result.failed}</b>
                  </>
                ) : null}
                {result.interrupted ? (
                  <>
                    {' '}
                    · 已中断 <b style={{ color: '#fa8c16' }}>{result.interrupted}</b>
                  </>
                ) : null}
                {result.pending ? (
                  <>
                    {' '}
                    · 待审批 <b style={{ color: '#fa8c16' }}>{result.pending}</b>
                  </>
                ) : null}{' '}
                · 总耗时 <b>{Math.round(result.ms / 1000)}s</b>
              </Text>
            </Space>
          ) : null}
        </div>

        <Space>
          {step === 0 ? (
            <>
              <Button onClick={onClose}>取消</Button>
              <Button type="primary" disabled={!keys.length} onClick={() => setStep(1)}>
                下一步（{keys.length}）
              </Button>
            </>
          ) : null}
          {step === 1 ? (
            <>
              <Button onClick={() => setStep(0)} disabled={loading}>
                上一步
              </Button>
              {/* 「发起变更」的按钮必须与后端受理口径一致：本批全部发往同一个 envKey，
                  缺 deploy:{envKey} 时后端必然 40300，这里前置禁用并说明缺哪条权限点。
                  disabled 按钮不派发鼠标事件，antd Tooltip 直包不生效，故用 DisabledTip。 */}
              <DisabledTip title={deployPermTip}>
                <Button
                  type="primary"
                  icon={<RocketOutlined />}
                  disabled={loading || !picked.length || !!noImage.length || !canDeploy}
                  onClick={() => setConfirmOpen(true)}
                >
                  {env?.needApproval ? '提交审批并发布' : '确认发布'}（{picked.length}）
                </Button>
              </DisabledTip>
            </>
          ) : null}
          {step === 2 ? (
            <>
              <Button onClick={onClose}>关闭</Button>
              <Button
                type="primary"
                ghost
                onClick={() => {
                  setStep(0);
                  setResult(null);
                }}
              >
                再发一批
              </Button>
            </>
          ) : null}
        </Space>
      </div>

      {/* 二次确认：发布是不可逆的线上变更，值得多一次点击 */}
      <Modal
        open={confirmOpen}
        title={`确认向 ${env?.name} 发布 ${picked.length} 个服务？`}
        okText="确认发布"
        cancelText="再核对一下"
        onCancel={() => setConfirmOpen(false)}
        onOk={start}
        width={560}
      >
        <Alert
          type={env?.needApproval ? 'warning' : 'info'}
          showIcon
          style={{ marginBottom: 12 }}
          message={
            env?.needApproval
              ? '该环境开启审批：提交后进入待审批，审批通过才会执行'
              : '发布将立即在目标环境滚动更新'
          }
          description={
            <span style={{ fontSize: 12 }}>
              版本变更 <b>{changeCount}</b> 个 / 同版本重复发布 <b>{sameCount}</b> 个
              {unknownCount ? (
                <>
                  {' '}
                  / <b>{unknownCount}</b> 个因读取不到集群而无法比对
                </>
              ) : null}
              。同版本发布等同于滚动重启。
            </span>
          }
        />
        <div style={{ maxHeight: 220, overflow: 'auto', fontSize: 12.5 }}>
          {picked.map((r) => (
            <div
              key={r.projectKey}
              style={{ display: 'flex', gap: 8, padding: '4px 0', borderBottom: '1px solid #f4f6f9' }}
            >
              <span style={{ width: 150, fontWeight: 500 }}>{r.projectName}</span>
              <span className="yn-mono" style={{ color: '#9aa0aa' }}>
                {r.currentTag ?? (r.currentKnown ? '集群中不存在' : '读取不到')}
              </span>
              <span style={{ color: '#1677ff' }}>→</span>
              <span className="yn-mono" style={{ color: '#1677ff' }}>
                {bareTagOf(targets[r.projectKey])}
              </span>
              {/* 来源必须显示出来：同名的 tag 在"平台构建"和"仓库标签"下走的是两条
                  不同的取镜像链路，事后查"这次发的是哪一份"时它是关键线索。 */}
              {targets[r.projectKey]?.startsWith(SRC_REGISTRY) ? (
                <Tag style={{ marginInlineEnd: 0, fontSize: 11 }}>仓库</Tag>
              ) : targets[r.projectKey]?.startsWith(SRC_PLATFORM) ? (
                <Tag style={{ marginInlineEnd: 0, fontSize: 11 }}>平台</Tag>
              ) : null}
            </div>
          ))}
        </div>
      </Modal>
    </Modal>
  );
};

/**
 * 部署的日志流地址。
 *
 * 待审批的部署单**没有**执行实体：后端只在审批通过后才 run 它，
 * 此时挂 SSE 会拿到一个空连接。所以待审批时返回空串，
 * BatchRunner 据 awaitingApproval 直接落"待审批"态而不挂流。
 */
function deployStatusStream(id: string, status: DeployStatus | string): string {
  if (status === 'pending_approval') return '';
  return deployApi.stream(String(id));
}

export default BatchDeployWizard;
