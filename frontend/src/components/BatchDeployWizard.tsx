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
} from '@ant-design/icons';
import { useNavigate } from 'react-router-dom';
import { BatchRunner } from './BatchRunner';
import type { BatchResult, BatchTaskDef, BatchTaskHandle, BatchTaskOutcome } from './BatchRunner';
import { ProjectTypeTag, ServiceStatusTag } from './StatusTag';
import { DisabledTip } from '@/components/DisabledTip';
import { ApiError, deployApi, environmentApi, imageApi } from '@/api';
import { canDeployOn } from '@/auth';
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

/** 版本 tag 很长（分支-commit-时间戳），列内截断 + tooltip 补全 */
const TagText: React.FC<{ value?: string; color?: string }> = ({ value, color = '#5c6470' }) =>
  value ? (
    <Tooltip title={value}>
      <span
        className="yn-mono yn-ellipsis"
        style={{ display: 'inline-block', maxWidth: '100%', color, fontWeight: 500, fontSize: 12.5 }}
      >
        {value}
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
  hasUpdate: boolean;
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

  /** 逐服务的目标版本（默认 = 仓库最新） */
  const [targets, setTargets] = useState<Record<string, string>>({});
  const [skipped, setSkipped] = useState<string[]>([]);

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
            hasUpdate: exists && !!latest && latest.tag !== currentTag,
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
        // 默认目标版本 = 仓库最新
        setTargets(
          Object.fromEntries(models.filter((m) => m.latest).map((m) => [m.projectKey, m.latest!.tag])),
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
  useEffect(() => {
    if (!open || !envKey) return;
    void loadRows(envKey);
  }, [open, envKey, loadRows]);

  // ------------------------------------------------------------------ 派生

  const filtered = useMemo(
    () =>
      rows.filter((r) => {
        const k = kw.trim().toLowerCase();
        const hitKw = !k || r.projectName.toLowerCase().includes(k) || r.projectKey.includes(k);
        const hitFilter =
          filter === 'all' ||
          (filter === 'undeployed' && r.undeployed) ||
          (filter === 'update' && r.hasUpdate) ||
          // 「已最新」只收**能确认**已最新的：读不到集群的行既不算有更新也不算已最新，
          // 否则"已最新 12"里会混进一堆我们根本不知道现状的服务。
          (filter === 'same' && r.currentKnown && !r.hasUpdate);
        return hitKw && hitFilter;
      }),
    [rows, filter, kw],
  );

  const counts = useMemo(
    () => ({
      undeployed: rows.filter((r) => r.undeployed).length,
      update: rows.filter((r) => r.hasUpdate).length,
      same: rows.filter((r) => !r.undeployed && r.currentKnown && !r.hasUpdate).length,
    }),
    [rows],
  );

  const picked = useMemo(
    () => rows.filter((r) => keys.includes(r.projectKey) && !skipped.includes(r.projectKey)),
    [rows, keys, skipped],
  );

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
  const changeCount = picked.filter((r) => r.currentKnown && targets[r.projectKey] !== r.currentTag).length;
  const sameCount = picked.filter((r) => r.currentKnown && targets[r.projectKey] === r.currentTag).length;
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

  /** 本批次的公共提交参数 */
  const startPayloadRef = useRef<{
    envKey: string;
    tagOf: Record<string, string>;
    /** 该环境配置的审批人（来自环境 DTO，见下方 startTask 注释） */
    approvers: string[];
  }>({
    envKey: '',
    tagOf: {},
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
    const tagOf: Record<string, string> = {};
    picked.forEach((r) => {
      // 兜底顺序里用 `platformTag` 而不是集群实况：后端 `resolveDeployImage` 是从
      // **平台镜像记录**里按 tag 找版本的，集群里跑着什么对这次提交没有影响。
      tagOf[r.projectKey] = targets[r.projectKey] || r.latest?.tag || r.platformTag || '';
    });

    startPayloadRef.current = { envKey, tagOf, approvers: env?.approvers ?? [] };
    lastOkRef.current = new Set();

    setBatchNo(no);
    setTasks(
      picked.map((r) => ({
        id: `bd_${r.projectKey}`,
        title: r.projectName,
        subtitle: `${r.projectKey} → ${env?.name ?? envKey}`,
        short: r.projectKey,
        projectKey: r.projectKey,
        imageHint: tagOf[r.projectKey],
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
    const { envKey: ek, tagOf, approvers } = startPayloadRef.current;
    const d = await deployApi.create({
      envKey: ek,
      projectKey: task.projectKey,
      tag: tagOf[task.projectKey],
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
        <TagText value={r.currentTag} />
        {r.live ? <ServiceStatusTag status={liveStatusWord(r.live.status)} /> : null}
      </Space>
    );
  };

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
    {
      title: `${env?.name ?? ''} 当前版本`,
      key: 'current',
      width: 258,
      render: (_, r) => currentCell(r),
    },
    {
      title: '仓库最新版本',
      key: 'latest',
      width: 258,
      render: (_, r) =>
        r.latest ? (
          <Space size={6} style={{ maxWidth: '100%' }}>
            <TagText value={r.latest.tag} color="#1677ff" />
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
        ) : r.hasUpdate ? (
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
    {
      title: `${env?.name ?? ''} 当前版本`,
      key: 'current',
      width: 250,
      render: (_, r) => currentCell(r),
    },
    {
      title: '确认发布版本（可修改）',
      key: 'target',
      width: 380,
      render: (_, r) => {
        const options = r.versions.slice(0, 12).map((i, idx) => ({
          label: `${i.tag}　(#${i.buildNo}${idx === 0 ? ' · 最新' : ''})`,
          value: i.tag,
        }));
        if (r.currentTag && !options.some((o) => o.value === r.currentTag)) {
          options.push({ label: `${r.currentTag}　(当前运行版本)`, value: r.currentTag });
        }
        return options.length ? (
          <Tooltip title={targets[r.projectKey]}>
            <Select
              showSearch
              size="small"
              style={{ width: 350 }}
              value={targets[r.projectKey]}
              onChange={(v) => setTargets((prev) => ({ ...prev, [r.projectKey]: v }))}
              options={options}
              placeholder="选择版本"
            />
          </Tooltip>
        ) : (
          <Text type="secondary" style={{ fontSize: 12 }}>
            仓库中没有可用镜像，无法发布
          </Text>
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
                        .filter((r) => r.hasUpdate && r.hasBuild && !r.undeployed && !r.inflight)
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
                <Button size="small" icon={<ReloadOutlined />} onClick={() => void loadRows(envKey)}>
                  重新读取
                </Button>
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
            concurrency={1}
            envLabel={env?.name ?? envKey}
            startTask={startTask}
            fetchOutcome={fetchOutcome}
            cancelRun={cancelRun}
            height={330}
            onTaskDone={(task, outcome) => {
              if (outcome.ok) {
                lastOkRef.current.add(task.id);
                const tag = startPayloadRef.current.tagOf[task.projectKey];
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
              提示：发布按服务串行排队执行，同一环境不会并发更新，避免滚动发布互相抢占资源
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
                {targets[r.projectKey]}
              </span>
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
