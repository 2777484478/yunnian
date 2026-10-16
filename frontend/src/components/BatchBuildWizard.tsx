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
  ReloadOutlined,
  SearchOutlined,
  ThunderboltFilled,
  ThunderboltOutlined,
} from '@ant-design/icons';
import { useNavigate } from 'react-router-dom';
// 只借用它的结果类型：本向导已不再用 BatchRunner 驱动执行
// （那套串行编排是"关页面后只构建一个"的根因，见 submit 的注释）。
import type { BatchResult } from './BatchRunner';
import { KVFields } from './KVFields';
import { BuildStatusTag, MethodTag, ProjectTypeTag } from './StatusTag';
import { ApiError, buildApi, isGitParam, jenkinsApi, projectApi } from '@/api';
import type { BuildStatus, Project } from '@/types';

const { Text } = Typography;

export interface BatchBuildWizardProps {
  open: boolean;
  onClose: () => void;
  /** 由项目列表勾选带入的预选项 */
  presetKeys?: string[];
  onCompleted?: (r: BatchResult) => void;
}

/**
 * 每个项目的分支候选最多挂几个。
 *
 * ⚠️ 原来定的 40 **太小**：Jenkins Job 的分支参数实测有 230 个，
 * 卡在 40 会把用户要找的分支整个砍掉（`Release_aiops_1.63` 就排在第 40 之后），
 * 表现是"下拉里找不到它" —— 而候选其实有，只是被我们截了。
 * 下拉本身有虚拟滚动，挂几百个不会卡。
 */
const BRANCH_OPTION_LIMIT = 400;

/** 处于"未结束"的构建：需要连 SSE 实时日志，而不是读历史日志 */
const isLive = (s?: BuildStatus | string) => s === 'running' || s === 'queued';

/**
 * 已提交的一个构建单。
 *
 * ⚠️ 这是**建单结果**，不是执行结果 —— 必须分开：
 * 建单失败（分支不存在 / 项目停用 / 队列满）是**这次点击**的问题，用户当场得改；
 * 执行失败是**构建过程**的问题，去「构建记录」看日志与 AI 归因。
 * 混成一个"失败"，用户分不清该重试还是该改分支。
 */
interface SubmittedItem {
  projectKey: string;
  projectName: string;
  branch: string;
  /** 产物来源（Jenkins Job / 平台 Tag 规则） */
  source: string;
  buildId?: string;
  buildNo?: string;
  status?: BuildStatus;
  /** 建单失败的真因（原样透出后端文案） */
  error?: string;
}

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
  /**
   * 逐项目的分支候选（key → 分支列表）。
   *
   * 取代了原先的"所有项目共有版本（取交集）"。取交集的前提在实践中不成立：
   * 各项目的分支本来就不同（形如 `v_aiops_1.16.0109` 的版本分支往往只有部分项目有），
   * 交叉一遍只剩 master —— 而用户要发的恰恰不是 master。
   */
  const [branchOptions, setBranchOptions] = useState<Record<string, string[]>>({});
  /** 逐项目选中的分支（key → 分支名）。提交时按项目各取各的。 */
  const [branchOf, setBranchOf] = useState<Record<string, string>>({});
  const [branchLoading, setBranchLoading] = useState(false);
  /** 分支读不到的项目名 —— 如实说出来，不能把它们当成"没有分支" */
  const [unreadableProjects, setUnreadableProjects] = useState<string[]>([]);
  /** key → Jenkins 项目的批量计划（null = 不能进批量），只对 buildMethod=jenkins 的项有值 */
  const [jenkinsPlan, setJenkinsPlan] = useState<Record<string, JenkinsBatchPlan | null>>({});
  /** key → 不能进批量的原因，原样显示在卡片上（"勾不动"必须给出理由） */
  const [jenkinsBlocked, setJenkinsBlocked] = useState<Record<string, string>>({});
  const [planLoading, setPlanLoading] = useState(false);
  const [batchNo, setBatchNo] = useState('');
  // 一次性建单之后的新状态：已提交的构建单 + 正在建单 + 二次确认弹窗
  const [submitted, setSubmitted] = useState<SubmittedItem[]>([]);
  const [submitting, setSubmitting] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  /** 点「开始打包」的时刻，用来算整批墙钟耗时。 */
  const submitAtRef = useRef(Date.now());
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
    setSubmitted([]);
    setSubmitting(false);
    finishedRef.current = false;
    setKw('');
    setTypeFilter('all');
    setBranchOptions({});
    setBranchOf({});
    setUnreadableProjects([]);
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

  // ------------------------------------------------------------------ 逐项目分支
  //
  // 「分支」在两类项目里落到不同的地方：
  //   - Jenkins 项目 → 传给它的 git 参数（如 git_branch）；
  //   - 平台构建项目 → 传给 POST /builds 的 branch。
  //
  // ⚠️ 这里**不再取交集**。原先的算法是"只给所有选中项目都认识的版本"，
  // 理由是"避免挑到一个某项目没有的版本、构建必然失败"。听起来稳妥，实际不成立：
  // 各项目的分支本来就不同（版本分支 `v_aiops_1.16.0109` 往往只有部分项目有），
  // 一交叉就只剩 master —— 而用户那批要发的根本不是 master。
  // 更糟的是它还会"纠正"用户的选择：把预填的 master 换成一个交集里的值，
  // 用户看到的就是"我选的分支被页面改掉了"。
 //
  // 现在每个项目拉**自己的**分支，各选各的，谁的项目谁负责。
  useEffect(() => {
    if (step !== 1 || !keys.length) return;
    let cancelled = false;
    setBranchLoading(true);

    void (async () => {
      /**
       * 先拉一次历史构建记录，作为**兜底的候选来源**。
       *
       * 为什么需要它：另外两个来源在真实环境里经常拿不到 ——
       *   · Jenkins 项目的候选来自 Job 的 git 参数，Jenkins 没接通时整条读不出来；
       *   · 平台项目的候选来自仓库分支，而"仓库归 Jenkins Job 自己管"的项目
       *     `GitURL` 是空的，后端直接返回空数组。
       * 结果就是分支框成了**空下拉**：默认值显示得出来，点开却什么都没有 ——
       * 想改个分支无从下手，正是"不能下拉选择"的样子。
       *
       * 历史构建记录是**平台自己的数据**，永远读得到，而且它天然回答了
       * "这个项目平时发的是哪几个分支"。
       */
      const hist = await buildApi.list({ pageSize: 200 }).catch(() => null);
      if (cancelled) return;
      const histOf: Record<string, string[]> = {};
      (hist?.list ?? []).forEach((b) => {
        if (!b.branch) return;
        const arr = (histOf[b.projectKey] ??= []);
        // 列表按时间倒序，所以第一个出现的即"最近用过的那一个"。
        if (!arr.includes(b.branch)) arr.push(b.branch);
      });

      const rows = await Promise.all(
        keys.map(async (k) => {
          const jk = jenkinsPlan[k];
          try {
            if (jk) {
              // Jenkins 项目：可选值就是那个 Job 的 git 参数取值（每次都真去读一遍）
              const v = await jenkinsApi.paramValues(jk.job, jk.param);
              return { key: k, values: [...v.branches, ...v.tags, ...v.other] };
            }
            return { key: k, values: await projectApi.branches(k) };
          } catch {
            // 读不到**不能**当成"这个项目没有分支"——那会让人以为仓库是空的。
            // 记下来，最终若一个候选都凑不出来，界面上如实说明。
            return { key: k, values: [] as string[], failed: true };
          }
        }),
      );
      if (cancelled) return;

      const opts: Record<string, string[]> = {};
      const failed: string[] = [];
      rows.forEach((r) => {
        const p = projectMap[r.key];
        const isJenkins = !!jenkinsPlan[r.key];
        const hist = histOf[r.key] ?? [];

        /**
         * Jenkins 项目**只放 Job 的权威取值**，历史记录不掺进来。
         *
         * 否则会出现两个看着一样的分支：
         *   `origin/Release_aiops_1.63`  ← Job 提供的选项（git 参数取值）
         *   `Release_aiops_1.63`         ← 平台从自己的构建记录里学的
         * 用户不知道该选哪个（问过）。而且"项目编译"那个弹窗里**只有** `origin/` 这一种，
         * 两处应该一致。
         *
         * 平台自建项目没有这个问题：它的候选来自仓库分支，本来就是裸名，
         * 历史记录补进来只是增加几个"发过的分支"，写法一致。
         */
        const set = new Set<string>(
          isJenkins && r.values.length
            ? r.values
            : [...r.values, ...hist],
        );
        // 读不到远端时（候选为空）用历史/默认分支兜底 —— "有没有起点"
        // 不该由我们这次读取的成败决定。
        if (!set.size) {
          hist.forEach((b) => set.add(b));
          if (p?.defaultBranch) set.add(p.defaultBranch);
        }

        opts[r.key] = [...set].slice(0, BRANCH_OPTION_LIMIT);
        // 只有**最终一个候选都没有**才算"读不到"。从历史构建里补到分支的项目
        // 下拉是可用的，把它列进"读不到"只会误导。
        if (r.failed && !opts[r.key].length) failed.push(p?.name || r.key);
      });
      setBranchOptions(opts);
      setUnreadableProjects(failed);

      /**
       * 把平台记录里的分支名对齐到候选里的写法。
       *
       * Jenkins 项目要的是 `origin/Release_aiops_1.63`，而平台的构建记录里存的
       * 可能是裸的 `Release_aiops_1.63`（Jenkins 侧推上来什么就记什么）。
       * 两者指向同一个分支 —— 但默认值如果落在"不在选项里"的那个写法上，
       * 下拉打开时看不到它被选中，用户会以为没选。所以能对上就换成候选里的那个。
       *
       * 对不上时**原样返回**：不做臆测的改写（比如给所有名字加前缀），
       * 后端按原值提交，与用户在"项目编译"里手输的行为一致。
       */
      const alignToOptions = (name: string | undefined, list: string[]): string | undefined => {
        if (!name) return undefined;
        if (list.includes(name)) return name;
        const withOrigin = `origin/${name}`;
        if (list.includes(withOrigin)) return withOrigin;
        return name;
      };

      setBranchOf((prev) => {
        const next = { ...prev };
        keys.forEach((k) => {
          // 用户已经动过的**一律不动** —— 这是我们最容易犯的错
          //（覆盖之后界面看着很正常，用户却发现自己选的分支被改掉了）。
          if (next[k]) return;
          const p = projectMap[k];
          const list = opts[k] ?? [];
          // 默认值 = 该项目**最近一次构建用过的分支** → 回落项目默认分支 → 候选第一个。
          // 为什么用"最近构建过的"而不是 master：日常发版发的就是那些版本分支，
          // 每次都要求手动改回去，等于把"默认"这两个字废掉了。
          next[k] =
            alignToOptions(p?.lastBuild?.branch, list) ||
            alignToOptions(p?.defaultBranch, list) ||
            list[0] ||
            '';
        });
        return next;
      });
      setBranchLoading(false);
    })();

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

  /**
   * 「开始打包」= **一次性把全部构建单建出来**，然后就不管了。
   *
   * ⚠️ 与批量发布同源的一次修复（见 BatchDeployWizard 的 submit）：
   * 原来由前端（BatchRunner）在浏览器里逐个建单、等前一个跑完再建下一个，
   * 于是**关掉页面后，后面的项目根本没有被创建过** —— 用户看到的就是
   * "排队以后关闭页面，只执行一个，后面的不执行"。
   *
   * 而两条链路的建单都是**建单即入队**（`POST /builds` → 构建池；
   * Jenkins 触发 → Job 自己排），由平台按 `BuildConcurrency` 调度。
   * 单子一旦建出来，就与浏览器在不在、页面关没关完全无关。
   */
  const submit = async () => {
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

    setBatchNo(no);
    setConfirmOpen(false);
    setStep(2);
    setSubmitting(true);

    // 先把"要提交什么"算齐（纯计算），失败项也能原位显示出来。
    const pending: SubmittedItem[] = keys.map((k) => {
      const p = projectMap[k];
      const jk = jenkinsPlan[k];
      return {
        projectKey: k,
        projectName: p.name,
        // 分支**按项目各取各的**（见逐项目分支那段注释）
        branch: branchOf[k] || p.defaultBranch || '',
        // 只读展示产物从哪来。Jenkins 项目的镜像由 Job 自己构建推送，
        // 平台项目才由后端按 tagPattern 生成 —— 两者不能写成同一句话。
        source: jk ? `Jenkins Job ${jk.job}` : `${p.registry}/${p.imageName} · ${p.tagPattern}`,
      };
    });
    setSubmitted(pending);

    /**
     * 并发建单，而不是串行。
     *
     * 串行唯一的"优势"是前面的失败就不建后面的 —— 那恰恰是坏语义：
     * 勾了 6 个项目，凭什么因为第 2 个的分支不对就放弃后面 4 个？
     * 每个构建单独立成败，失败项原位标出来让用户单独处理。
     */
    const results = await Promise.all(
      keys.map(async (k, i): Promise<SubmittedItem> => {
        const base = pending[i];
        const jk = jenkinsPlan[k];
        const branch = base.branch;
        // 幂等键按「批次 + 项目」：后端对重复的 idempotencyKey 不报错，
        // 而是把已有构建单原样返回（见 service/ci.go 的 TriggerBuild）。
        const idem = `${no}:${k}`;
        try {
          if (jk) {
            /**
             * Jenkins 链路：把分支传给它的 git 参数。
             *
             * ⚠️ 必须分流。平台构建接口**不看 buildMethod**，会把"从 Jenkins
             * 分支下拉里选出来的值"当成平台自己的分支名，去代码托管查一个提交、
             * 再跑一次平台的 docker 构建 —— 最后成功落一条和 Jenkins 毫无关系的
             * 构建记录。那属于最难发现的那种错：一切看起来都对。
             */
            const res = await jenkinsApi.trigger(
              jk.job,
              branch ? [{ name: jk.param, value: branch }] : [],
              idem,
            );
            return {
              ...base,
              buildId: String(res.buildId),
              buildNo: `${k}#${res.buildNo}`,
              status: res.status === 'queued' ? 'queued' : 'running',
            };
          }
          const b = await buildApi.trigger({
            projectKey: k,
            branch,
            params,
            idempotencyKey: idem,
            triggerType: 'manual',
          });
          return {
            ...base,
            buildId: String(b.id),
            buildNo: `${k}#${b.buildNo}`,
            status: b.status === 'queued' ? 'queued' : 'running',
          };
        } catch (e) {
          // 提交失败必须给**真因**（分支不存在 / 项目停用 / 队列满各不一样），
          // 压成一句"提交失败"会让用户拿参数问题当系统故障去排查。
          return { ...base, error: e instanceof ApiError ? e.message : '提交失败' };
        }
      }),
    );

    setSubmitted(results);
    setSubmitting(false);

    const ok = results.filter((x) => !x.error);
    const bad = results.filter((x) => x.error);
    if (bad.length) {
      notification.warning({
        message: `已提交 ${ok.length} 个，${bad.length} 个未能建单`,
        description: `${bad.map((x) => x.projectKey).join('、')} 没有进入队列，原因见下方列表。其余项目已交给平台依次构建，与是否关闭本页面无关。`,
        duration: 8,
      });
    } else {
      notification.success({
        message: `已提交 ${ok.length} 个构建任务`,
        description: '平台会按构建池并发依次执行。现在就可以关闭本页面，构建不会中断。',
        duration: 6,
      });
    }
  };

  /**
   * 轮询这批构建的状态 —— **只为给人看**，不驱动任何执行。
   *
   * 与改造前的本质区别：这里即使一次都不跑（用户把页面关了），
   * 平台该构建的还是照跑。轮询失败也不改任何状态 —— 它只是显示层。
   */
  const refreshSubmitted = useCallback(async () => {
    try {
      const page = await buildApi.list({ pageSize: 100 });
      const byId = new Map(page.list.map((b) => [b.id, b]));
      setSubmitted((prev) =>
        prev.map((x) => {
          if (!x.buildId) return x;
          const b = byId.get(x.buildId);
          return b ? { ...x, status: b.status } : x;
        }),
      );
    } catch {
      // 读不到**不改状态**：把"我没读到"说成"构建失败了"，两者处置完全不同。
    }
  }, []);

  useEffect(() => {
    if (!open || step !== 2 || submitting) return;
    if (!submitted.some((x) => x.buildId)) return;
    const timer = window.setInterval(() => void refreshSubmitted(), 4000);
    void refreshSubmitted();
    return () => window.clearInterval(timer);
    // 依赖里**故意不放 submitted**：否则每次轮询都重建定时器，间隔被搅成不等。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, step, submitting, refreshSubmitted]);

  /** 本批概览（顶部与底部共用同一份数字，避免两处各算一次而分家）。 */
  const okCount = submitted.filter((x) => !x.error).length;
  const badCount = submitted.filter((x) => !!x.error).length;
  const runningCount = submitted.filter((x) => !x.error && isLive(x.status)).length;
  const successCount = submitted.filter((x) => x.status === 'success').length;
  const failedCount = submitted.filter((x) => x.status === 'failed' || !!x.error).length;
  const canceledCount = submitted.filter((x) => x.status === 'canceled').length;
  const interruptedCount = submitted.filter((x) => x.status === 'interrupted').length;

  /**
   * 全部构建停住后向父组件报一次结果。
   * ⚠️ `interrupted` 单列并且**绝不能**算进成功：漏掉它，一批"跑了一半就没了"
   * 的构建会弹出绿色的「全部成功」。
   */
  const finishedRef = useRef(false);
  useEffect(() => {
    if (step !== 2 || submitting || !submitted.length || finishedRef.current) return;
    if (!submitted.every((x) => !x.buildId || !isLive(x.status))) return;
    finishedRef.current = true;
    onCompleted?.({
      total: submitted.length,
      success: successCount,
      failed: failedCount,
      canceled: canceledCount,
      pending: 0,
      interrupted: interruptedCount,
      ms: Date.now() - submitAtRef.current,
    });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, submitting, submitted]);


  /** 第 1 步「选择项目」表的列：把卡片上原有的信息平铺成列，不丢字段。 */
  const pickColumns: ColumnsType<Project> = [
    {
      title: '项目',
      key: 'project',
      width: 288,
      render: (_, p) => (
        <div style={{ display: 'flex', alignItems: 'center', gap: 6, minWidth: 0 }}>
          <span style={{ display: 'inline-flex', flex: 'none' }}>
            <ProjectTypeTag type={p.type} />
          </span>
          <b className="yn-ellipsis" style={{ flex: '1 1 auto', minWidth: 0, fontWeight: 500 }}>
            {p.name}
          </b>
          <span className="yn-mono" style={{ flex: 'none', color: '#9aa0aa', fontSize: 11 }}>
            {p.key}
          </span>
        </div>
      ),
    },
    {
      title: '构建方式',
      key: 'method',
      width: 176,
      render: (_, p) => {
        const plan = p.buildMethod === 'jenkins' ? jenkinsPlan[p.key] : undefined;
        return (
          <Space size={4}>
            <MethodTag method={p.buildMethod} />
            {plan ? (
              <Tooltip title={`批量提交会把所选分支传给该 Job 的 ${plan.param} 参数`}>
                <Tag color="blue" style={{ marginInlineEnd: 0, cursor: 'help' }}>
                  可批量
                </Tag>
              </Tooltip>
            ) : null}
            {!p.enabled ? <Tag style={{ marginInlineEnd: 0 }}>已停用</Tag> : null}
          </Space>
        );
      },
    },
    {
      title: '最近构建',
      key: 'last',
      // 顺带把**分支**也放出来：第 2 步的默认分支取的就是它，
      // 在这里能提前看到"我大概会构建哪个分支"。
      width: 200,
      render: (_, p) =>
        p.lastBuild?.branch ? (
          <Space size={6}>
            <BuildStatusTag status={p.lastBuild.status} />
            <span className="yn-mono" style={{ fontSize: 11.5, color: '#8c8c95' }}>
              {p.lastBuild.branch}
            </span>
          </Space>
        ) : (
          <BuildStatusTag status={p.lastBuild?.status} />
        ),
    },
    {
      title: '负责人',
      key: 'owner',
      width: 110,
      render: (_, p) => <span style={{ fontSize: 12.5 }}>{p.owner}</span>,
    },
    {
      title: '说明',
      key: 'note',
      render: (_, p) => {
        const isJenkins = p.buildMethod === 'jenkins';
        const ready = isJenkins && !!jenkinsPlan[p.key];
        const blockReason = isJenkins ? (jenkinsBlocked[p.key] ?? '') : '';
        // 计划还没判定出来（参数正在读）—— 此时既不说"能"，也不说"不能"
        if (blockReason) {
          return (
            <Text type="warning" style={{ fontSize: 12 }}>
              不能批量构建：{blockReason}
            </Text>
          );
        }
        if (isJenkins && !ready) {
          return (
            <Text type="secondary" style={{ fontSize: 12 }}>
              正在读取该 Job 的参数…
            </Text>
          );
        }
        return (
          <Text type="secondary" style={{ fontSize: 12 }}>
            {ready ? `分支会传给 git 参数 ${jenkinsPlan[p.key]?.param}` : '按平台 Tag 规则生成镜像'}
          </Text>
        );
      },
    },
  ];

  /** 第 3 步「已提交」表的列 —— 只读，不承担任何执行职责。 */
  const submittedColumns: ColumnsType<SubmittedItem> = [
    {
      title: '项目',
      key: 'project',
      width: 250,
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
      title: '构建分支',
      key: 'branch',
      width: 200,
      // 每个项目**自己的**分支 —— 这一列正是这次改造的重点，所以要显眼。
      render: (_, r) => (
        <span className="yn-mono" style={{ fontSize: 12.5, color: '#1677ff' }}>
          {r.branch || '—'}
        </span>
      ),
    },
    {
      title: '构建单号',
      key: 'no',
      width: 160,
      render: (_, r) =>
        r.buildNo ? (
          <span className="yn-mono" style={{ fontSize: 12.5 }}>
            {r.buildNo}
          </span>
        ) : (
          // 没建单就没有单号 —— 不给占位号，那会让人去记录里搜一个不存在的号。
          <Text type="secondary" style={{ fontSize: 12 }}>
            —
          </Text>
        ),
    },
    {
      title: '状态',
      key: 'status',
      width: 110,
      render: (_, r) =>
        r.error ? (
          <Tag color="error" style={{ marginInlineEnd: 0 }}>
            未建单
          </Tag>
        ) : r.status ? (
          <BuildStatusTag status={r.status} />
        ) : (
          <Text type="secondary" style={{ fontSize: 12 }}>
            —
          </Text>
        ),
    },
    {
      title: '说明',
      key: 'note',
      render: (_, r) =>
        r.error ? (
          // 建单失败原样给出后端文案（分支不存在 / 项目停用 / 队列满各不一样），
          // 压成一句"失败"会让用户拿参数问题当系统故障去排查。
          <Text type="danger" style={{ fontSize: 12 }}>
            {r.error}
          </Text>
        ) : (
          <Text type="secondary" style={{ fontSize: 12 }} className="yn-ellipsis">
            {r.status === 'queued'
              ? '排队中，前面的任务结束就轮到它'
              : r.status === 'running'
                ? '构建中，详情见「构建记录」'
                : r.status === 'success'
                  ? '构建完成'
                  : r.status === 'interrupted'
                    ? '平台重启打断，未产出镜像，需重新打包'
                    : r.status === 'canceled'
                      ? '已取消'
                      : '未产出镜像，原因见该单日志与 AI 归因'}
          </Text>
        ),
    },
  ];

  // ------------------------------------------------------------------ 渲染

  const planColumns: ColumnsType<Project> = [
    {
      title: '项目',
      key: 'project',
      width: 300,
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
      title: '构建分支',
      key: 'branch',
      width: 250,
      // 每个项目一个下拉、**各选各的**。
      //
      // 原来这一列是只读的，渲染 `startPayloadRef.current.branch` —— 全局同一个值，
      // 因为那时"分支"是在左边选一次、全批共用的。而各项目的版本分支本来就不同名，
      // 硬要共用就得取交集，于是大部分候选被筛掉、只剩 master。
      render: (_, r) => {
        const opts = branchOptions[r.key] ?? [];
        const val = branchOf[r.key] ?? '';
        return (
          /**
           * 用 `Select` 而不是 `AutoComplete` —— 与「项目编译」里那个分支选择器**同一套写法**。
           *
           * 关键差别在**搜索词与已选值是否分离**：
           *   · `Select` + `showSearch` + `optionFilterProp="label"`：输入框里敲的是搜索词，
           *     已选值另存 —— 所以「输入 1.63 能联想出 origin/Release_aiops_1.63」是成立的。
           *   · `AutoComplete`：输入框内容**就是** value，两者是同一个东西。
           *     既拿它当"显示已选值"又拿它当"搜索词"，必然打架 ——
           *     我前两轮在这里反复翻车（先是被预填值过滤成空下拉，改成不过滤又丢了联想）。
           *
           * 代价是不能再自由输入分支名。这是有意的取舍：
           * 「能搜索、能选到」是日常主路径；自由输入只在"候选读不到"时才需要，
           * 而那种情况下面会单独提示（让它看起来像能输、其实输不进去更糟）。
           */
          <Select
            showSearch
            allowClear
            size="small"
            style={{ width: '100%' }}
            value={val || undefined}
            onChange={(v) => setBranchOf((prev) => ({ ...prev, [r.key]: v ?? '' }))}
            options={opts.map((b) => ({ label: b, value: b }))}
            // 按 label 过滤（label 就是分支名）—— 不设它的话 antd 会按 value 过，行为一样但显式更清楚
            optionFilterProp="label"
            loading={branchLoading}
            // 候选可能有几百个（实测某个 Job 有 230 个），限住下拉高度免得盖住底部按钮
            listHeight={280}
            placeholder={branchLoading ? '读取中…' : '选择分支'}
            notFoundContent={
              branchLoading ? (
                <div style={{ padding: 10, textAlign: 'center' }}>
                  <Spin size="small" />
                </div>
              ) : (
                <div style={{ padding: 10 }}>
                  <Text type="secondary" style={{ fontSize: 12 }}>
                    {opts.length ? '没有匹配的分支' : '没有读到该项目的分支候选'}
                  </Text>
                </div>
              )
            }
          />
        );
      },
    },
    {
      // 上一版构建用的是哪个分支。
      //
      // 为什么它必须在同一行里：这一列的**默认值就是从它来的**（见上面的
      // "默认值 = 最近一次构建用过的分支"）。把它显示出来，用户才能一眼对照
      // "我之前发的是哪个分支 / 现在要发的是哪个" —— 否则改分支时没有任何参照，
      // 只能凭记忆。
      title: '最近构建分支',
      key: 'lastBranch',
      width: 224,
      render: (_, r) =>
        r.lastBuild?.branch ? (
          <Space size={6}>
            {/* 列宽足够时就不用截断；仍留 tooltip 兜极长分支名。 */}
            <Tooltip title={r.lastBuild.branch}>
              <span className="yn-mono yn-ellipsis" style={{ fontSize: 12.5, color: '#8c8c95' }}>
                {r.lastBuild.branch}
              </span>
            </Tooltip>
            <BuildStatusTag status={r.lastBuild.status} />
          </Space>
        ) : (
          // 没构建过就说没构建过 —— 不给一个空的等宽块，那看着像"读不到"。
          <Text type="secondary" style={{ fontSize: 12 }}>
            无构建记录
          </Text>
        ),
    },
    {
      title: '产物来源',
      key: 'tag',
      // 明确给宽度：不给的话它会分到剩余空间，列一多就被挤到**表头竖排**。
      width: 210,
      // 两类项目的产物来源完全不同，必须分开写：
      // Jenkins 项目的镜像由 Job 自己构建推送，平台给的 tagPattern 对它没有意义。
      render: (_, r) =>
        r.buildMethod === 'jenkins' ? (
          <Tooltip title={`镜像的构建与推送由该 Jenkins Job 自己完成，平台只把分支参数传给它\nJob：${r.jenkinsJob || '（未绑定）'}`}>
            <span className="yn-src-badge yn-src-badge--jenkins">{r.jenkinsJob || '未绑定 Job'}</span>
          </Tooltip>
        ) : (
          <Tooltip title={`镜像地址：${r.registry}/${r.imageName}`}>
            <span className="yn-src-badge">{r.tagPattern}</span>
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
            /**
             * 用**表格**而不是卡片网格。
             *
             * 卡片是三列瀑布流、整张卡兼作 label —— 点卡片任何位置都会切换勾选。
             * 一次要勾几十个的时候，相邻两张卡极容易误点（想选 A 结果把 B 也选上，
             * 而 B 会不会被构建要等到第 3 步才发现）。表格一行一个勾选框，
             * 纵向扫下来目标稳定，也不存在"点到卡片就选中"的意外。
             */
            <div className="yn-pick__table">
              <Table<Project>
                rowKey={(p) => p.key}
                size="small"
                dataSource={list}
                columns={pickColumns}
                pagination={false}
                scroll={{ y: 340 }}
                rowSelection={{
                  selectedRowKeys: keys,
                  /**
                   * ⚠️ 不能直接 `setKeys(ks)`。
                   *
                   * antd 的 `onChange` 只给**当前 dataSource 里**选中的行，而这里的
                   * dataSource 是**搜索 / 筛选后**的列表 —— 于是"上一次搜索时勾的、
                   * 这次不在结果里"的项目会被整个丢掉。用户的实际体验：
                   * 搜 `aioitms` 勾上 → 再搜 `aioai` 勾一个 → 回头发现第一个没了，
                   * 而界面上"已选"始终显示 1 个，像是勾选没生效。
                   *
                   * 正解：**只替换可见部分的勾选状态，不在可见范围内的旧勾选原样保留**。
                   */
                  onChange: (ks) => {
                    const visible = list.map((p) => p.key);
                    setKeys((prev) => [
                      ...new Set([...prev.filter((k) => !visible.includes(k)), ...(ks as string[])]),
                    ]);
                  },
                  getCheckboxProps: (p) => {
                    // Jenkins 项目能不能勾，取决于平台能不能把「分支」传进去
                    // （Job 恰好一个 git 参数、且没有不认识的参数）。
                    // 做不到就**不给勾选框**并把原因写在行里 —— "能勾选"必须等于
                    // "后端会受理且出来的东西是对的"。
                    const isJenkins = p.buildMethod === 'jenkins';
                    const ready = isJenkins && !!jenkinsPlan[p.key];
                    return { disabled: isJenkins && !ready };
                  },
                }}
              />
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
            buildParams: [],
          }}
        >
          <Row>
            <Col span={24}>
              {/* 这里只留**状态与处置**，不再放"说明性"的两块（并发由谁决定 / Tag 由谁生成）——
                  那两块常驻在左边占掉近半宽度，而清单因此被挤到每列都截断。
                  分支读不到时的指引必须留着：它是"该怎么办"，不是背景知识。 */}
              {branchLoading ? (
                <Alert
                  type="info"
                  showIcon
                  style={{ marginBottom: 10 }}
                  message="正在读取各项目的分支…"
                  description="每个项目各自拉一次自己的分支列表。"
                />
              ) : unreadableProjects.length ? (
                <Alert
                  type="warning"
                  showIcon
                  style={{ marginBottom: 10 }}
                  message={`${unreadableProjects.length} 个项目的分支没读到：${unreadableProjects.join('、')}`}
                  description={
                    <span style={{ fontSize: 12 }}>
                      这些项目的下拉里只有各自配置里的默认分支。要构建别的分支，请先到
                      「项目管理 → 编译」里确认该项目的分支可选（那里读的是同一个来源）。
                    </span>
                  }
                />
              ) : null}

              <div className="yn-plan">
                <div className="yn-plan__head">
                  <Space size={8}>
                    <InfoCircleOutlined style={{ color: '#1677ff' }} />
                    <b>构建清单（{selected.length}）</b>
                    <Text type="secondary" style={{ fontSize: 12 }}>
                      每个项目按各自的分支构建；提交后立即入队，由平台构建池依次执行
                    </Text>
                  </Space>
                </div>
                <Table<Project>
                  rowKey={(r) => r.key}
                  size="middle"
                  dataSource={selected}
                  columns={planColumns}
                  pagination={selected.length > 8 ? { pageSize: 8, size: 'small' } : false}
                  scroll={{ y: 320 }}
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

      {/* ---------------------------------------------------------- 步骤 3
          这一步**只读**：不再驱动任何执行。构建单在点「开始打包」时就全部建出来了，
          由平台构建池按并发依次执行 —— 关掉页面、关掉浏览器都照常跑完。
          所以这里的轮询纯粹是为了让人看见进展，它挂了也不影响构建。 */}
      {step === 2 ? (
        <div className="yn-batch__stage yn-submitted">
          <div className="yn-submitted__head">
            <Space size={[10, 6]} wrap>
              <span className="yn-submitted__title">
                已提交 <b>{okCount}</b> 个构建任务
              </span>
              {runningCount ? <Tag color="processing">进行中 {runningCount}</Tag> : null}
              {successCount ? <Tag color="success">成功 {successCount}</Tag> : null}
              {failedCount ? <Tag color="error">失败 {failedCount}</Tag> : null}
              {canceledCount ? <Tag>已取消 {canceledCount}</Tag> : null}
              {interruptedCount ? <Tag color="warning">已中断 {interruptedCount}</Tag> : null}
              {badCount ? <Tag color="error">未建单 {badCount}</Tag> : null}
              {submitting ? <Tag color="processing">正在提交…</Tag> : null}
            </Space>
            <Button size="small" icon={<ReloadOutlined />} onClick={() => void refreshSubmitted()}>
              刷新
            </Button>
          </div>

          {/* 这句是这一步存在的理由：告诉用户"可以走了，不会断"。
              改之前做不到 —— 那时关页面就是真的不构了。 */}
          <Alert
            type={badCount ? 'warning' : 'success'}
            showIcon
            style={{ marginBottom: 10 }}
            message={badCount ? '部分项目未能建单，其余已进入队列' : '已进入平台构建队列，可以关闭本页面'}
            description={
              <span style={{ fontSize: 12 }}>
                每个项目的构建单已落库排队，平台按构建池并发依次执行。
                关闭弹窗、关闭浏览器都不会中断构建；进度可随时在「构建记录」里查看。
              </span>
            }
          />

          <Table<SubmittedItem>
            rowKey="projectKey"
            size="small"
            dataSource={submitted}
            columns={submittedColumns}
            pagination={submitted.length > 8 ? { pageSize: 8, size: 'small' } : false}
            scroll={{ y: 260 }}
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
          {step === 2 ? (
            <Space size={8}>
              <CheckCircleFilled style={{ color: failedCount || interruptedCount ? '#faad14' : '#52c41a' }} />
              <Text style={{ fontSize: 12 }}>
                {submitting
                  ? '正在提交…'
                  : runningCount
                    ? `构建队列执行中：还剩 ${runningCount} 个未完成（平台按构建池并发调度）`
                    : '本批已全部停住'}
                {batchNo ? <span style={{ color: '#9aa0aa' }}>（批次 {batchNo}）</span> : null}
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
              <Button
                type="primary"
                icon={<ThunderboltOutlined />}
                disabled={!selected.length || selected.some((p) => !branchOf[p.key])}
                onClick={() => setConfirmOpen(true)}
              >
                开始打包（{selected.length}）
              </Button>
            </>
          ) : null}
          {step === 2 ? (
            <>
              {/* 「关闭」不再有任何风险 —— 关不关都不影响构建，这正是本次改造的效果。 */}
              <Button onClick={onClose}>关闭</Button>
              <Button
                type="primary"
                onClick={() => {
                  onClose();
                  navigate('/ci/builds');
                }}
              >
                查看构建记录
              </Button>
              <Button
                ghost
                onClick={() => {
                  setStep(0);
                  setSubmitted([]);
                  finishedRef.current = false;
                }}
              >
                再打包一批
              </Button>
            </>
          ) : null}
        </Space>
      </div>

      {/* 二次确认：一次点击会真的触发这么多项目的构建，值得多一次点击把分支再核一遍
          （分支这次改成逐项目可调，最需要核对的恰恰是它）。 */}
      <Modal
        open={confirmOpen}
        title={`确认打包 ${selected.length} 个项目？`}
        okText="开始打包"
        cancelText="再核对一下"
        onCancel={() => setConfirmOpen(false)}
        onOk={() => void submit()}
        confirmLoading={submitting}
        width={560}
      >
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 12 }}
          message="每个项目按各自的分支构建，构建单会立即进入平台队列"
          description={
            <span style={{ fontSize: 12 }}>
              提交后由平台按构建池并发调度，<b>关闭本页面也不会中断</b>；
              进度可在「构建记录」里查看。
            </span>
          }
        />
        <div style={{ maxHeight: 240, overflow: 'auto', fontSize: 12.5 }}>
          {selected.map((p) => (
            <div
              key={p.key}
              style={{ display: 'flex', gap: 8, padding: '4px 0', borderBottom: '1px dashed #f0f0f0' }}
            >
              <span className="yn-ellipsis" style={{ flex: '1 1 auto', minWidth: 0 }}>
                {p.name}
              </span>
              <span className="yn-mono" style={{ flex: 'none', color: '#1677ff' }}>
                {branchOf[p.key] || '（未选分支）'}
              </span>
            </div>
          ))}
        </div>
      </Modal>
    </Modal>
  );
};

export default BatchBuildWizard;
