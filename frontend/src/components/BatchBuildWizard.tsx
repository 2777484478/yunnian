import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  Alert,
  App as AntApp,
  Button,
  Checkbox,
  Col,
  Collapse,
  Form,
  Input,
  Modal,
  Row,
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
  ClusterOutlined,
  InfoCircleOutlined,
  SearchOutlined,
  ThunderboltFilled,
  ThunderboltOutlined,
} from '@ant-design/icons';
import { useNavigate } from 'react-router-dom';
import { BatchRunner } from './BatchRunner';
import type { BatchResult, BatchTaskDef, BatchTaskHandle, BatchTaskOutcome } from './BatchRunner';
import { KVFields } from './KVFields';
import { BuildStatusTag, MethodTag, ProjectTypeTag } from './StatusTag';
import { ApiError, buildApi, isGitParam, jenkinsApi, projectApi } from '@/api';
import type { Project } from '@/types';

const { Text } = Typography;

export interface BatchBuildWizardProps {
  open: boolean;
  onClose: () => void;
  /** 由项目列表勾选带入的预选项 */
  presetKeys?: string[];
  onCompleted?: (r: BatchResult) => void;
}

const CONCURRENCY_OPTIONS = [
  { label: '1（串行排队）', value: 1 },
  { label: '2', value: 2 },
  { label: '3（推荐）', value: 3 },
  { label: '4', value: 4 },
];

/** 分支并集里最多挂几个选项，避免 20 个项目的分支全堆进下拉框 */
const BRANCH_OPTION_LIMIT = 40;

/**
 * 一个 Jenkins 项目进批量打包所需的绑定信息。
 *
 * 批量打包的语义是「这一批用同一个版本」，而版本要靠 Job 的 git 参数传进去，
 * 所以只有**恰好一个** git 参数、且没有平台不认识的参数时，才敢把项目放进来。
 */
interface JenkinsBatchPlan {
  /** 要触发的 Job 全名 */
  job: string;
  /** 承载「版本」的 git 参数名（如 git_branch） */
  param: string;
}

/**
 * 批量 CI 镜像打包向导：选项目 → 构建配置 → 排队打包（实时日志）。
 *
 * 四处刻意的取舍，都是被后端契约定的：
 *
 * 1. **没有「Tag 生成方式 / 统一 Tag」**。`POST /builds` 的入参只有
 *    {projectKey, branch, params, idempotencyKey, ...}，镜像名与 tag 由后端按
 *    项目自身的 `tagPattern` + 提交号 + 时间戳生成，前端给不了 tag。
 *    所以这里把各项目的 tag 规则**只读展示**出来，而不是摆一个点了不生效的下拉框。
 *    「一次发版共用一个版本号」需要后端支持 tag 覆盖，列入二期。
 *
 * 2. **Jenkins 项目也进得来，但走的是另一条链路**（`buildMethod=jenkins`）。
 *    平台不会替 Jenkins 构建：批量提交时对它打的是 Jenkins 触发接口，把所选版本
 *    传给 Job 的 git 参数。**绝不能**让它走 `POST /builds`——那个接口不看
 *    buildMethod，会把"从 Jenkins 版本下拉里选出来的值"当成平台自己的分支名，
 *    去跑一次平台构建，最后成功落一条与 Jenkins 毫无关系的记录。
 *
 * 3. **版本候选是"所有选中项目都认识的"（交集），不是并集**。
 *    并集允许用户挑到一个某个项目根本没有的版本，那次构建必然失败，
 *    而失败原因要到日志里才看得出来。取不到候选时如实说明并允许手工输入。
 *
 * 4. **提交即真实建单**。每个任务都是一个真实的构建单，日志来自后端；
 *    弹窗关掉也不会中断执行（任务在服务端队列里）。面板上的状态**跟随后端**——
 *    后端在排队就显示"排队中"，不写死"执行中"。
 */
const BatchBuildWizard: React.FC<BatchBuildWizardProps> = ({
  open,
  onClose,
  presetKeys,
  onCompleted,
}) => {
  const { message, notification } = AntApp.useApp();
  const navigate = useNavigate();

  const [step, setStep] = useState(0);
  const [keys, setKeys] = useState<string[]>([]);
  const [kw, setKw] = useState('');
  const [typeFilter, setTypeFilter] = useState('all');
  const [projects, setProjects] = useState<Project[]>([]);
  const [loadingProjects, setLoadingProjects] = useState(false);
  const [loadError, setLoadError] = useState('');
  const [versionOptions, setVersionOptions] = useState<string[]>([]);
  const [versionLoading, setVersionLoading] = useState(false);
  const [versionError, setVersionError] = useState('');
  /** 预填版本被自动纠正时的说明（不是错误，只是告诉用户"值变了，为什么"） */
  const [versionNote, setVersionNote] = useState('');
  /**
   * 已对哪一批选择做过"预填纠正"。用 ref 记住，**同一批只纠正一次**——
   * 否则 effect 因依赖变化重跑时会再次覆盖，把用户手工改的版本顶掉。
   */
  const versionFixedForRef = useRef('');
  /** key → Jenkins 项目的批量计划（null = 不能进批量），只对 buildMethod=jenkins 的项有值 */
  const [jenkinsPlan, setJenkinsPlan] = useState<Record<string, JenkinsBatchPlan | null>>({});
  /** key → 不能进批量的原因，原样显示在卡片上（"勾不动"必须给出理由） */
  const [jenkinsBlocked, setJenkinsBlocked] = useState<Record<string, string>>({});
  const [planLoading, setPlanLoading] = useState(false);
  const [tasks, setTasks] = useState<BatchTaskDef[]>([]);
  const [batchNo, setBatchNo] = useState('');
  const [round, setRound] = useState(0);
  const [result, setResult] = useState<BatchResult | null>(null);
  /** 并发数：提交时从表单读出后冻结，避免执行中途改表单影响调度 */
  const [concurrency, setConcurrency] = useState(3);
  const [form] = Form.useForm();

  // ------------------------------------------------------------------ 取数

  const loadProjects = useCallback(async () => {
    setLoadingProjects(true);
    setLoadError('');
    try {
      // 20 个项目一次取完：向导里要"全选/反选"，分页取会让语义变得莫名其妙
      const page = await projectApi.list({ pageSize: 100 });
      setProjects(page.list);
    } catch (e) {
      setLoadError(e instanceof ApiError ? e.message : '项目列表加载失败');
    } finally {
      setLoadingProjects(false);
    }
  }, []);

  useEffect(() => {
    if (!open) return;
    setStep(0);
    setKeys(presetKeys ?? []);
    setTasks([]);
    setResult(null);
    setKw('');
    setTypeFilter('all');
    setVersionOptions([]);
    setVersionError('');
    setVersionNote('');
    versionFixedForRef.current = '';
    setJenkinsPlan({});
    setJenkinsBlocked({});
    void loadProjects();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open]);

  /**
   * 为列表里的 Jenkins 项目逐个读 **Job 参数定义**，判断它能不能进批量打包。
   *
   * 只读参数定义（轻量：纯读 job 配置，不碰仓库），所以可以一次全读；
   * 可选值要另外读，那个每次都会真去仓库拉一遍，只对**选中的**项目读。
   *
   * 为什么不"先让用户勾、提交时再说"：批量打包的前提是这一批用同一个版本，
   * 而版本要靠 Job 的 git 参数传进去。Job 没有 git 参数、或有多 git 参数、
   * 或有平台不认识的参数时，这个项目就没法按同一个版本构建——那属于
   * "勾得动、出来的东西不对"，比直接勾不动更糟。所以原因写在卡片上。
   */
  useEffect(() => {
    if (!open || !projects.length) return;
    const jenkinsProjects = projects.filter((p) => p.buildMethod === 'jenkins');
    if (!jenkinsProjects.length) {
      setJenkinsPlan({});
      setJenkinsBlocked({});
      return;
    }
    let cancelled = false;
    setPlanLoading(true);
    void Promise.all(
      jenkinsProjects.map(async (p) => {
        const job = (p.jenkinsJob ?? '').trim();
        if (!job) {
          return {
            key: p.key,
            plan: null as JenkinsBatchPlan | null,
            why: '该项目还没绑定 Jenkins Job，平台不知道该触发哪个 Job',
          };
        }
        try {
          const defs = await jenkinsApi.params(job);
          const gitDefs = defs.filter((d) => isGitParam(d.kind));
          if (!gitDefs.length) {
            return {
              key: p.key,
              plan: null,
              why: `Job「${job}」没有版本/分支参数，批量选的版本传不进去——请到该项目的「编译」里由平台触发`,
            };
          }
          if (gitDefs.length > 1) {
            return {
              key: p.key,
              plan: null,
              why: `Job「${job}」有 ${gitDefs.length} 个版本/分支参数（${gitDefs
                .map((d) => d.name)
                .join('、')}），平台无法确定该往哪个传版本`,
            };
          }
          const unknown = defs.filter((d) => d.kind === 'unknown');
          if (unknown.length) {
            return {
              key: p.key,
              plan: null,
              why: `Job「${job}」有 ${unknown.length} 个平台不认识的参数（${unknown
                .map((d) => d.name)
                .join('、')}），提交无法保证参数完整`,
            };
          }
          return { key: p.key, plan: { job, param: gitDefs[0].name }, why: '' };
        } catch (e) {
          return {
            key: p.key,
            plan: null,
            why: `读取 Job「${job}」的参数失败：${e instanceof ApiError ? e.message : '未知错误'}`,
          };
        }
      }),
    )
      .then((rows) => {
        if (cancelled) return;
        const plan: Record<string, JenkinsBatchPlan | null> = {};
        const blocked: Record<string, string> = {};
        for (const r of rows) {
          plan[r.key] = r.plan;
          if (!r.plan) blocked[r.key] = r.why;
        }
        setJenkinsPlan(plan);
        setJenkinsBlocked(blocked);
      })
      .finally(() => {
        if (!cancelled) setPlanLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [open, projects]);

  /**
   * 项目数据到位后，把**不能进批量**的项目从勾选里剔除。
   *
   * presetKeys 是从项目列表页带过来的，而那张表允许勾选任何项目；
   * 不筛的话会出现"向导里明明勾着一个项目，但它发不出去"——正是本页最忌讳的
   * "勾得动、发不出"。返回 prev 是为了在没变化时不触发多余渲染。
   */
  useEffect(() => {
    if (!projects.length) return;
    setKeys((prev) => {
      const next = prev.filter((k) => {
        const p = projects.find((x) => x.key === k);
        if (!p) return false;
        return p.buildMethod !== 'jenkins' || !!jenkinsPlan[k];
      });
      return next.length === prev.length ? prev : next;
    });
  }, [projects, jenkinsPlan]);

  const projectMap = useMemo(
    () => Object.fromEntries(projects.map((p) => [p.key, p])),
    [projects],
  );

  const list = useMemo(
    () =>
      projects.filter((p) => {
        const k = kw.trim().toLowerCase();
        const hitKw =
          !k || p.name.toLowerCase().includes(k) || p.key.includes(k) || (p.owner ?? '').includes(k);
        const hitType = typeFilter === 'all' || p.type === typeFilter;
        return hitKw && hitType;
      }),
    [projects, kw, typeFilter],
  );

  /**
   * 可勾选的项目。
   *
   * Jenkins 项目**进得来**——批量提交时对它们打的是 Jenkins 触发接口，不是平台构建接口。
   * 前提是平台能把「版本」传进去（Job 恰好有一个 git 参数、且没有不认识的参数）。
   * 做不到就挡住并把理由写在卡片上："能勾选"必须等于"后端会受理且出来的东西是对的"。
   */
  const pickable = useMemo(
    () => list.filter((p) => p.buildMethod !== 'jenkins' || !!jenkinsPlan[p.key]),
    [list, jenkinsPlan],
  );

  /** 当前筛选结果里因 Job 参数不合适而进不了批量的 Jenkins 项目 */
  const blockedList = useMemo(
    () => list.filter((p) => p.buildMethod === 'jenkins' && !!jenkinsBlocked[p.key]),
    [list, jenkinsBlocked],
  );

  const toggle = (key: string, on: boolean) =>
    setKeys((prev) => (on ? [...new Set([...prev, key])] : prev.filter((k) => k !== key)));

  // ------------------------------------------------------------------ 版本候选
  //
  // 「版本」在两类项目里落到不同的地方：
  //   - Jenkins 项目 → 传给它的 git 参数（如 git_branch）；
  //   - 平台构建项目 → 传给 POST /builds 的 branch。
  //
  // 批量构建的语义是"这一批用同一个版本"，所以候选值必须是**所有选中项目都认识的**
  // ——取**交集**而不是并集。并集允许用户挑到一个某个项目根本没有的版本，
  // 那次构建必然失败，而失败原因要到日志里才看得出来。
  //
  // 取不到就退回项目自身默认版本 + 手工输入，并如实说明是谁没读到。
  useEffect(() => {
    if (step !== 1 || !keys.length) return;
    let cancelled = false;
    setVersionLoading(true);
    setVersionError('');
    setVersionNote('');

    const jenkinsKeys = keys.filter((k) => projectMap[k]?.buildMethod === 'jenkins');
    const platformKeys = keys.filter((k) => projectMap[k]?.buildMethod !== 'jenkins');

    // Jenkins 侧：只对选中的项目读可选值（这个接口每次都真去仓库拉一遍）。
    const jenkinsValues = Promise.all(
      jenkinsKeys.map(async (k) => {
        const plan = jenkinsPlan[k];
        if (!plan) return { key: k, values: null as string[] | null };
        try {
          const v = await jenkinsApi.paramValues(plan.job, plan.param);
          return { key: k, values: [...v.tags, ...v.branches, ...v.other] };
        } catch {
          // 读不到**不能**当成"没有可选值"——后者会让人以为仓库里没版本。
          return { key: k, values: null };
        }
      }),
    );

    // 平台侧：沿用原来的分支读取（GitLab 未启用真实实现时整体降级）。
    const platformBranches = platformKeys.length
      ? Promise.all(platformKeys.map((k) => projectApi.branches(k).catch(() => null)))
      : Promise.resolve([] as string[][] | null[]);

    Promise.all([jenkinsValues, platformBranches])
      .then(([jk, pf]) => {
        if (cancelled) return;

        const sets: Set<string>[] = [];
        const unreadable: string[] = [];

        for (const row of jk) {
          if (row.values === null) unreadable.push(projectMap[row.key]?.name || row.key);
          else sets.push(new Set(row.values));
        }

        if (platformKeys.length) {
          const readable = (pf as (string[] | null)[]).filter((x): x is string[] => Array.isArray(x));
          if (!readable.length) {
            // 代码托管可能未启用真实实现——如实说明，并让用户手工填
            unreadable.push(...platformKeys.map((k) => projectMap[k]?.name || k));
          } else {
            const union = new Set<string>();
            readable.flat().forEach((b) => b && union.add(b));
            // 项目自己的默认分支也放进来（平台侧原本就是这么做的：至少给用户一个起点）
            platformKeys.forEach((k) => {
              const d = projectMap[k]?.defaultBranch;
              if (d) union.add(d);
            });
            sets.push(union);
          }
        }

        let common: string[] = [];
        if (sets.length) {
          common = [...sets[0]].filter((v) => sets.every((s) => s.has(v)));
        }

        // 兜底：把各项目的默认版本也放进来（交集为空时用户至少有个起点），
        // 但**只在它是共有值时才放**——否则等于偷偷放宽了"共有"这个前提。
        if (!common.length) {
          const defaults = new Set(
            keys.map((k) => projectMap[k]?.defaultBranch).filter((b): b is string => !!b),
          );
          if (defaults.size && !sets.length) common = [...defaults];
        }

        setVersionOptions(common.slice(0, BRANCH_OPTION_LIMIT));

        // 预填的版本来自"列表里第一个项目的默认分支"，它**未必**是所有选中项目
        // 都认识的（Jenkins Job 的 git 参数里可能根本没有它）。留着它等于把
        // "只给共有版本"这个前提偷偷放掉了——用户看不出哪个项目会构建失败。
        // 所以只要它不在候选里就换成候选的第一个，并把这次改动明确说出来。
        const picked = form.getFieldValue('branch') as string | undefined;
        const selKey = keys.join(',');
        if (common.length && picked && !common.includes(picked) && versionFixedForRef.current !== selKey) {
          versionFixedForRef.current = selKey;
          form.setFieldValue('branch', common[0]);
          setVersionNote(
            `预填的「${picked}」不是所有选中项目都有的版本，已改为候选里的「${common[0]}」`,
          );
        }

        if (unreadable.length) {
          setVersionError(
            `未能读取以下项目的可选版本：${unreadable.join('、')}。候选里只剩读到的那些，` +
              `不能保证每个项目都有——请自行确认，或直接在输入框里填版本名`,
          );
        }
      })
      .catch(() => {
        if (cancelled) return;
        setVersionError('未能读取版本候选（代码托管 / Jenkins 可能未启用真实实现），请手工输入版本名');
        const set = new Set<string>();
        keys.forEach((k) => projectMap[k]?.defaultBranch && set.add(projectMap[k].defaultBranch!));
        setVersionOptions([...set]);
      })
      .finally(() => {
        if (!cancelled) setVersionLoading(false);
      });

    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, keys.join(','), projectMap, jenkinsPlan]);

  // ------------------------------------------------------------------ 表单联动

  const selected: Project[] = useMemo(
    () => keys.map((k) => projectMap[k]).filter(Boolean) as Project[],
    [keys, projectMap],
  );

  // ------------------------------------------------------------------ 启动打包

  /** 上一轮里成功的任务 id（重试时只带失败项） */
  const lastOkRef = useRef<Set<string>>(new Set());
  /** 本批次的公共提交参数（版本 / 参数） */
  const startPayloadRef = useRef<{ branch: string; params?: Record<string, string> }>({
    branch: 'master',
  });
  /**
   * 本批次里走 Jenkins 链路的项目 → 它报名时定下的 Job 与 git 参数名。
   * 提交时按项目 key 取用；不在这里的走平台构建链路。
   */
  const jenkinsExecRef = useRef<Record<string, JenkinsBatchPlan>>({});

  const start = async () => {
    const v = await form.validateFields();
    const no = `BT${new Date().toISOString().slice(2, 10).replace(/-/g, '')}-${Date.now()
      .toString()
      .slice(-6)}`;
    const params = (v.buildParams as { key: string; value: string }[] | undefined)?.reduce<
      Record<string, string>
    >((acc, kv) => {
      if (kv?.key) acc[kv.key] = kv.value ?? '';
      return acc;
    }, {});

    const built: BatchTaskDef[] = keys.map((k) => {
      const p = projectMap[k];
      const jk = jenkinsPlan[k];
      return {
        id: `bt_${k}`,
        title: p.name,
        subtitle: `${p.key} · ${p.type === 'frontend' ? '前端' : '后端'} · ${v.branch}`,
        short: p.key,
        projectKey: k,
        // 只读展示本次产物从哪来。Jenkins 项目的镜像由 Job 自己构建推送，
        // 平台项目才由后端按 tagPattern 生成——两者不能写成同一句话。
        imageHint: jk ? `Jenkins Job ${jk.job}` : `${p.registry}/${p.imageName} · ${p.tagPattern}`,
      };
    });

    // 本次要走的 Jenkins 链路参数：job + 承接版本的 git 参数名。
    // 放在 ref 里而不是 BatchTaskDef 上——task 定义只说"是什么"，不说"怎么跑"。
    const exec: Record<string, JenkinsBatchPlan> = {};
    for (const k of keys) {
      const jk = jenkinsPlan[k];
      if (jk) exec[k] = jk;
    }
    jenkinsExecRef.current = exec;

    startPayloadRef.current = { branch: v.branch, params };
    lastOkRef.current = new Set();

    setBatchNo(no);
    setTasks(built);
    setResult(null);
    setConcurrency(v.concurrency ?? 3);
    setRound((r) => r + 1);
    setStep(2);
  };

  /**
   * 提交单个任务。**两条完全不同的链路，按项目的构建方式分流**：
   *
   *   - Jenkins 项目 → 打 Jenkins 触发接口，把所选版本传给 Job 的 git 参数。
   *     必须这样分流：平台构建接口（下面那条）**不看 buildMethod**，它会把
   *     "刚从 Jenkins 版本下拉里选出来的值"当成平台自己的分支名，去代码托管
   *     查一个提交、再跑一次平台的 docker 构建——最后成功落一条和 Jenkins
   *     毫无关系的构建记录。这属于最难发现的那种错：一切看起来都对。
   *
   *   - 其余项目 → 平台自己的构建链路，行为与之前完全一致。
   *
   * 幂等键按「批次 + 项目」生成——同一次点击只会产生一个构建单；
   * 后端对重复的 idempotencyKey **不报错**，而是把已有构建单原样返回
   * （幂等语义，见 service/ci.go 的 TriggerBuild），所以这里无需处理 409。
   */
  const startTask = useCallback(
    async (task: BatchTaskDef): Promise<BatchTaskHandle> => {
      const { branch, params } = startPayloadRef.current;
      const idem = `${batchNo}:${task.projectKey}`;
      const jk = jenkinsExecRef.current[task.projectKey];

      if (jk) {
        // 只传用户明确选过的参数：不传 → Jenkins 用它自己配置的默认值，
        // 与在 Jenkins 页面上直接点 Build 一致；传一个平台猜的值才可能构建错版本。
        const res = await jenkinsApi.trigger(
          jk.job,
          branch ? [{ name: jk.param, value: branch }] : [],
          idem,
        );
        return {
          runId: String(res.buildId),
          // 两类构建的日志流 key 是同一个（BuildLogStreamKey），所以订阅方式一致。
          // 不用响应里的 logStreamUrl：它是相对路径，EventSource 不能带请求头，
          // 必须经 buildApi.stream() 把令牌拼进查询串。
          stream: buildApi.stream(String(res.buildId)),
          runNo: `${task.projectKey}#${res.buildNo}`,
          initialStatus: res.status === 'queued' ? 'queued' : 'running',
          initialStage: res.stage,
        };
      }

      const b = await buildApi.trigger({
        projectKey: task.projectKey,
        branch,
        params,
        idempotencyKey: idem,
        triggerType: 'manual',
      });
      return {
        runId: String(b.id),
        stream: buildApi.stream(String(b.id)),
        runNo: `${task.projectKey}#${b.buildNo}`,
        // 后端刚建单时是 queued（构建池满会一直在排队）。如实上报，
        // 面板据此显示"排队中"，而不是一律显示"执行中"。
        initialStatus: b.status === 'queued' ? 'queued' : 'running',
        initialStage: b.stage,
      };
    },
    [batchNo],
  );

  const fetchOutcome = useCallback(async (runId: string): Promise<BatchTaskOutcome> => {
    const b = await buildApi.get(runId);
    const ok = b.status === 'success';
    return {
      ok,
      status: b.status,
      detail: ok
        ? b.stage || '构建完成'
        : // 中断的**不能**走 AI 归因那条路：它没有失败原因可归因（后端也只对 failed 受理），
          // 落回 `b.stage`（启动补偿写的是"已中断"）会得到一句没有下文的"已中断"。
          b.status === 'interrupted'
          ? '因平台重启中断，未产出镜像；需重新打包'
          : b.aiDiagnosis?.reason || b.stage || '构建失败',
    };
  }, []);

  const cancelRun = useCallback(async (runId: string) => {
    await buildApi.cancel(runId);
  }, []);

  const handleFinish = (r: BatchResult) => {
    setResult(r);
    onCompleted?.(r);
    // interrupted 也算未成功：漏掉它会让"跑了一半就没了"的一批弹出绿色「全部成功」。
    const allOk = !r.failed && !r.canceled && !r.interrupted;
    notification[allOk ? 'success' : 'warning']({
      message: allOk ? '批量打包全部成功' : '批量打包完成（存在未成功项）',
      description: (
        <span>
          批次 <b>{batchNo}</b>：成功 <b style={{ color: '#52c41a' }}>{r.success}</b> 个
          {r.failed ? (
            <>
              ，失败 <b style={{ color: '#ff4d4f' }}>{r.failed}</b> 个
            </>
          ) : null}
          {r.interrupted ? (
            <>
              ，已中断 <b style={{ color: '#fa8c16' }}>{r.interrupted}</b> 个（平台重启打断，未产出镜像，需重新打包）
            </>
          ) : null}
          {r.canceled ? `，取消 ${r.canceled} 个` : null}。构建记录已落库，可在「构建记录」查看详情与日志。
        </span>
      ),
      placement: 'topRight',
      duration: 8,
    });
  };

  // ------------------------------------------------------------------ 渲染

  const planColumns: ColumnsType<Project> = [
    {
      title: '项目',
      key: 'project',
      width: 252,
      render: (_, r) => (
        <div style={{ display: 'flex', alignItems: 'center', gap: 6, minWidth: 0 }}>
          <span style={{ display: 'inline-flex', flex: 'none' }}>
            <ProjectTypeTag type={r.type} />
          </span>
          <b className="yn-ellipsis" style={{ flex: '1 1 auto', minWidth: 0, fontWeight: 500 }}>
            {r.name}
          </b>
          <span className="yn-mono" style={{ flex: 'none', color: '#9aa0aa', fontSize: 11 }}>
            {r.key}
          </span>
        </div>
      ),
    },
    {
      title: '构建版本',
      key: 'branch',
      width: 108,
      render: () => <span className="yn-mono">{startPayloadRef.current.branch}</span>,
    },
    {
      title: '产物来源',
      key: 'tag',
      // 两类项目的产物来源完全不同，必须分开写：
      // Jenkins 项目的镜像由 Job 自己构建推送，平台给的 tagPattern 对它没有意义。
      render: (_, r) =>
        r.buildMethod === 'jenkins' ? (
          <Tooltip title="镜像的构建与推送由该 Jenkins Job 自己完成，平台只把版本参数传给它">
            <span className="yn-mono yn-ellipsis" style={{ display: 'block', color: '#d46b08' }}>
              {r.jenkinsJob || '（未绑定 Jenkins Job）'}
            </span>
          </Tooltip>
        ) : (
          <Tooltip title={`镜像地址：${r.registry}/${r.imageName}`}>
            <span className="yn-mono yn-ellipsis" style={{ color: '#1677ff', display: 'block' }}>
              {r.tagPattern}
            </span>
          </Tooltip>
        ),
    },
  ];

  const selectedFail = selected.filter((p) => p.lastBuild?.status === 'failed').length;

  return (
    <Modal
      open={open}
      onCancel={onClose}
      width={1100}
      title={
        <Space size={10}>
          <ThunderboltFilled style={{ color: '#1677ff' }} />
          批量 CI 镜像打包
          <Tag color="blue" style={{ marginInlineEnd: 0 }}>
            一次提交多个项目 · 排队构建 · 实时日志
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
          { title: '选择项目', description: keys.length ? `已选 ${keys.length} 个` : '勾选要打包的项目' },
          { title: '构建配置', description: '分支与构建参数' },
          { title: '排队打包', description: '实时日志' },
        ]}
      />

      {/* ---------------------------------------------------------- 步骤 1 */}
      {step === 0 ? (
        <div className="yn-pick">
          <div className="yn-pick__bar">
            <Input
              allowClear
              size="small"
              prefix={<SearchOutlined style={{ color: '#b5bac4' }} />}
              placeholder="搜索项目名称 / 标识 / 负责人"
              style={{ width: 240 }}
              value={kw}
              onChange={(e) => setKw(e.target.value)}
            />
            <Segmented
              size="small"
              value={typeFilter}
              onChange={(v) => setTypeFilter(String(v))}
              options={[
                { label: '全部', value: 'all' },
                { label: '前端', value: 'frontend' },
                { label: '后端', value: 'backend' },
              ]}
            />
            <span className="yn-pick__bar-spacer" />
            <Space size={6}>
              <Button size="small" onClick={() => setKeys((prev) => [...new Set([...prev, ...pickable.map((p) => p.key)])])}>
                全选当前 {pickable.length}
              </Button>
              <Button
                size="small"
                onClick={() =>
                  setKeys((prev) => {
                    const toAdd = pickable.filter((p) => !prev.includes(p.key)).map((p) => p.key);
                    return toAdd.length
                      ? [...new Set([...prev, ...toAdd])]
                      : prev.filter((k) => !pickable.some((p) => p.key === k));
                  })
                }
              >
                反选
              </Button>
              <Button size="small" onClick={() => setKeys([])} disabled={!keys.length}>
                清空
              </Button>
            </Space>
          </div>

          {loadError ? (
            <Alert
              type="error"
              showIcon
              style={{ marginBottom: 12 }}
              message="项目列表加载失败"
              description={loadError}
              action={
                <Button size="small" onClick={() => void loadProjects()}>
                  重试
                </Button>
              }
            />
          ) : null}

          {loadingProjects ? (
            <div style={{ padding: '60px 0', textAlign: 'center' }}>
              <Spin />
            </div>
          ) : (
            <div className="yn-pick__grid">
              {list.map((p) => {
                const on = keys.includes(p.key);
                // Jenkins 项目能不能勾，取决于平台能不能把「版本」传进去
                // （Job 恰好一个 git 参数、且没有不认识的参数）。
                // 做不到就**不给勾选框**并把原因写在卡片上——比"勾了再失败"清楚得多，
                // 也比"勾了却构建出别的版本"安全。
                const isJenkins = p.buildMethod === 'jenkins';
                const jenkinsReady = isJenkins && !!jenkinsPlan[p.key];
                const blockReason = isJenkins ? (jenkinsBlocked[p.key] ?? '') : '';
                // 计划还没判定出来（参数正在读）——此时既不说"能"，也不说"不能"
                const checking = isJenkins && !jenkinsReady && !blockReason;
                return (
                  <label key={p.key} className={`yn-pick__card ${on ? 'is-on' : ''}`}>
                    <Checkbox
                      checked={on}
                      disabled={isJenkins && !jenkinsReady}
                      onChange={(e) => toggle(p.key, e.target.checked)}
                    />
                    <div className="yn-pick__card-body">
                      <div className="yn-pick__card-head">
                        <b>{p.name}</b>
                        <code>{p.key}</code>
                      </div>
                      <div className="yn-pick__card-tags">
                        <ProjectTypeTag type={p.type} />
                        <MethodTag method={p.buildMethod} />
                        {!p.enabled ? <Tag color="default">已停用</Tag> : null}
                        {jenkinsReady ? <Tag color="blue">走 Jenkins 构建</Tag> : null}
                      </div>
                      {blockReason ? (
                        <div style={{ fontSize: 11.5, color: '#d46b08' }}>不能批量构建：{blockReason}</div>
                      ) : null}
                      {checking ? (
                        <div style={{ fontSize: 11.5, color: '#8c8c95' }}>正在读取该 Job 的参数…</div>
                      ) : null}
                      {jenkinsReady ? (
                        <div style={{ fontSize: 11.5, color: '#8c8c95' }}>
                          批量提交会把所选版本传给{' '}
                          <span className="yn-mono">{jenkinsPlan[p.key]?.param}</span>
                        </div>
                      ) : null}
                      <div className="yn-pick__card-foot">
                        <span>
                          最近构建 <BuildStatusTag status={p.lastBuild?.status} />
                        </span>
                        <em>{p.owner}</em>
                      </div>
                    </div>
                    {on ? <CheckCircleFilled className="yn-pick__card-tick" /> : null}
                  </label>
                );
              })}
            </div>
          )}

          <div className="yn-pick__summary">
            <Space size={8} wrap>
              <Text style={{ fontSize: 13 }}>
                已选择 <b style={{ color: '#1677ff' }}>{keys.length}</b> 个项目
              </Text>
              {selectedFail ? (
                <Tag color="error" style={{ marginInlineEnd: 0 }}>
                  其中 {selectedFail} 个最近构建失败
                </Tag>
              ) : null}
              {planLoading ? (
                <Tag color="default" style={{ marginInlineEnd: 0 }}>
                  正在检查 Jenkins Job 能否批量构建…
                </Tag>
              ) : null}
              {blockedList.length ? (
                <Tooltip
                  title={
                    <div>
                      {blockedList.map((p) => (
                        <div key={p.key}>
                          <b>{p.name}</b>：{jenkinsBlocked[p.key]}
                        </div>
                      ))}
                    </div>
                  }
                >
                  <Tag color="orange" style={{ marginInlineEnd: 0 }}>
                    {blockedList.length} 个暂时不能批量构建（悬停看原因）
                  </Tag>
                </Tooltip>
              ) : null}
              <Text type="secondary" style={{ fontSize: 12 }}>
                全平台共 {projects.length} 个项目（前端 {projects.filter((p) => p.type === 'frontend').length} / 后端{' '}
                {projects.filter((p) => p.type === 'backend').length}）
              </Text>
            </Space>
          </div>
        </div>
      ) : null}

      {/* ---------------------------------------------------------- 步骤 2 */}
      {step === 1 ? (
        <Form
          form={form}
          layout="vertical"
          className="yn-batch__form"
          initialValues={{
            branch: selected[0]?.defaultBranch ?? 'master',
            concurrency: 3,
            buildParams: [],
          }}
        >
          <Row gutter={20}>
            <Col span={9}>
              <Form.Item
                label="构建版本"
                name="branch"
                rules={[{ required: true, message: '请选择或输入版本' }]}
                extra={
                  versionLoading ? (
                    '正在读取所选项目的版本候选…'
                  ) : (
                    <>
                      {versionError ? (
                        versionError
                      ) : (
                        <>候选是所有选中项目都认识的版本（取交集）；Jenkins 项目由它的 git 参数接收，平台项目作为分支</>
                      )}
                      {versionNote ? <div style={{ color: '#d46b08' }}>{versionNote}</div> : null}
                    </>
                  )
                }
                validateStatus={versionError ? 'warning' : undefined}
              >
                <Select
                  showSearch
                  // 不设 mode=tags：版本名可以是任意字符串，但下拉里选出错的选项比打错字更常见，
                  // 这里允许直接输入（showSearch 支持），也允许清空重填
                  options={versionOptions.map((b) => ({ label: b, value: b }))}
                  placeholder="选择或输入版本"
                  notFoundContent={versionLoading ? <Spin size="small" /> : '无共有候选版本，请直接输入'}
                />
              </Form.Item>

              <Form.Item
                label="并发任务数"
                name="concurrency"
                tooltip="超出并发数的任务留在「排队中」，前面的任务结束后自动补位；后端构建池也会按自己的并发上限排队，实际状态以后端为准"
              >
                <Select options={CONCURRENCY_OPTIONS} />
              </Form.Item>

              <Form.Item label="镜像 Tag 由谁生成">
                {selected.some((p) => p.buildMethod === 'jenkins') ? (
                  <Alert
                    type="warning"
                    showIcon
                    message="本批含 Jenkins 项目：它们的镜像 Tag 由各自的 Job 决定"
                    description={
                      <span style={{ fontSize: 12 }}>
                        平台只把所选版本传给 Job 的 git 参数，
                        <b>不代为生成 tag、也不登记镜像版本</b>
                        （Jenkins 侧推了什么 tag，平台读构建日志才知道）。
                        {selected.some((p) => p.buildMethod !== 'jenkins')
                          ? ' 平台构建的项目仍按各项目的 Tag 规则 + 提交号 + 时间戳生成。'
                          : ''}
                      </span>
                    }
                  />
                ) : (
                  <Alert
                    type="info"
                    showIcon
                    message="镜像 Tag 由平台按项目规则生成"
                    description={
                      <span style={{ fontSize: 12 }}>
                        后端按各项目的 Tag 规则 + 提交号 + 时间戳生成，本次构建无法在前端指定统一版本号。
                        「一次发版统一 Tag」需要后端支持 tag 覆盖，已列入二期。
                      </span>
                    }
                  />
                )}
              </Form.Item>
            </Col>

            <Col span={15}>
              <div className="yn-plan">
                <div className="yn-plan__head">
                  <Space size={8}>
                    <InfoCircleOutlined style={{ color: '#1677ff' }} />
                    <b>构建清单（{selected.length}）</b>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      提交前请核对分支与各项目的 Tag 规则
                    </Text>
                  </Space>
                </div>
                <Table
                  rowKey={(r) => r.key}
                  size="small"
                  dataSource={selected}
                  columns={planColumns}
                  pagination={selected.length > 8 ? { pageSize: 8, size: 'small' } : false}
                  scroll={{ y: 300 }}
                />
              </div>
            </Col>
          </Row>

          <Collapse
            ghost
            bordered={false}
            size="small"
            className="yn-batch__adv"
            items={[
              {
                key: 'params',
                label: (
                  <Space size={8}>
                    <span style={{ fontSize: 13 }}>构建参数（统一追加 / 覆盖）</span>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      作为环境变量注入构建过程，留空则沿用各项目自身配置
                    </Text>
                  </Space>
                ),
                children: (
                  <Form.Item name="buildParams" noStyle>
                    <KVFields name="buildParams" addText="添加构建参数" />
                  </Form.Item>
                ),
              },
            ]}
          />
        </Form>
      ) : null}

      {/* ---------------------------------------------------------- 步骤 3 */}
      {step === 2 ? (
        <div className="yn-batch__stage">
          <BatchRunner
            key={round}
            kind="ci"
            batchNo={batchNo}
            tasks={tasks}
            concurrency={concurrency}
            startTask={startTask}
            fetchOutcome={fetchOutcome}
            cancelRun={cancelRun}
            onTaskDone={(task, outcome) => {
              if (outcome.ok) lastOkRef.current.add(task.id);
              else lastOkRef.current.delete(task.id);
            }}
            onFinish={handleFinish}
            height={330}
            onOpenRecords={() => {
              onClose();
              navigate('/ci/builds');
            }}
            onRetryFailed={() => {
              // 只把失败项带进新一轮，成功的任务不再重跑（重跑成功任务没有意义）
              const failedKeys = tasks
                .filter((t) => !lastOkRef.current.has(t.id))
                .map((t) => t.projectKey);
              setKeys(failedKeys);
              setStep(1);
              setResult(null);
              message.info(`已带出 ${failedKeys.length} 个失败项目，确认配置后可重新提交`);
            }}
          />
        </div>
      ) : null}

      {/* ---------------------------------------------------------- 底部操作 */}
      <div className="yn-batch__actions">
        <div className="yn-batch__actions-left">
          {step === 0 ? (
            <Text type="secondary" style={{ fontSize: 12 }}>
              提示：按各项目的构建方式分派（Jenkins / 平台自建），提交后可在「构建记录」查看日志
            </Text>
          ) : null}
          {step === 1 ? (
            <Space size={8}>
              <ClusterOutlined style={{ color: '#8c8c95' }} />
              <Text type="secondary" style={{ fontSize: 12 }}>
                将提交 {selected.length} 个构建任务 · 超出并发的任务排队等待
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
                ) : null}{' '}
                · 总耗时 <b>{Math.round(result.ms / 1000)}s</b> · 记录已写入「构建记录」
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
              <Button onClick={() => setStep(0)}>上一步</Button>
              <Button type="primary" icon={<ThunderboltOutlined />} onClick={start}>
                开始打包（{selected.length}）
              </Button>
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
                再打包一批
              </Button>
            </>
          ) : null}
        </Space>
      </div>
    </Modal>
  );
};

export default BatchBuildWizard;
