import React, { useEffect, useMemo, useRef, useState } from 'react';
import {
  Alert,
  App as AntApp,
  AutoComplete,
  Badge,
  Button,
  Col,
  Divider,
  Drawer,
  Form,
  Input,
  Modal,
  Row,
  Segmented,
  Select,
  Space,
  Spin,
  Switch,
  Table,
  Tag,
  Tooltip,
  Typography,
} from 'antd';
import type { ColumnsType } from 'antd/es/table';
import {
  AppstoreOutlined,
  BranchesOutlined,
  CodeOutlined,
  DeleteOutlined,
  EditOutlined,
  ExportOutlined,
  HistoryOutlined,
  PlusOutlined,
  ReloadOutlined,
  SearchOutlined,
  ThunderboltOutlined,
} from '@ant-design/icons';
import { Link, useNavigate, useSearchParams } from 'react-router-dom';
import { PageHeader, SectionCard } from '@/components/ui';
import { KVFields } from '@/components/KVFields';
import AutoRefreshButton from '@/components/AutoRefreshButton';
import BatchBuildWizard from '@/components/BatchBuildWizard';
import {
  JenkinsGitParamSelect,
  JenkinsParamFields,
  collectJenkinsParams,
  defaultJenkinsSelection,
  useJenkinsParams,
} from '@/components/JenkinsBuildParams';
import {
  BuildStatusTag,
  DigestText,
  ImageText,
  MethodTag,
  ProjectTypeTag,
} from '@/components/StatusTag';
import { ApiError, buildApi, jenkinsApi, projectApi } from '@/api';
import type { GitConnectionResult } from '@/api';
import { getSession, hasPerm } from '@/auth';
import { useApi } from '@/hooks/useApi';
import { useAutoRefresh, useAutoRefreshPref } from '@/hooks/useAutoRefresh';
import { humanDuration } from '@/utils/format';
import type { Project, BuildStatus, BuildRecord } from '@/types';
import type { JenkinsJob } from '@/api';

const { Text } = Typography;

const TEAMS = ['用户增长组', '前端体验组', '交易中台组', '支付结算组', '基础架构组', '数据平台组'];
const OWNERS = ['张伟', '李静', '王强', '刘洋', '陈晨', '赵敏', '冯雪', '韩磊', '曹阳', '运维示例', '高鹏'];

// ---------------------------------------------------------------------------
// 新建项目的默认值（一期）
//
// 这四个字段一期都没有维护入口：团队没有主数据表（`project.team` 只是个自由
// 字符串），负责人是一份写死的名单，后端只校验名字能否在 `user` 表里查到。
// 让用户每次手选既没有信息量、又容易选错，所以直接给一组「对绝大多数项目都
// 成立」的默认值——类型与团队取现有纳管项目的取值，负责人取当前登录用户
// （谁建的项目谁负责）。二期做出团队/成员主数据后，这里的默认值改为从接口取。
// ---------------------------------------------------------------------------
const DEFAULT_TYPE = 'backend';
const DEFAULT_TEAM = '基础架构组';

/**
 * 默认 Tag 规则。必须与后端 `buildscript.DefaultTagPattern` 同值 ——
 * 两边不一致会让"新建时看到的预览"与"保存后真正的 tag"不同，
 * 而用户是照着预览去理解版本号的。
 */
const DEFAULT_TAG_PATTERN = 'v{buildNo}';

/**
 * 抽屉里「标签在左」布局的标签列宽度。
 *
 * 抽成常量是因为有**一处不属于任何字段**的控件：「测试连接」按钮。它要跟上面
 * 那些输入框的左边缘对齐（实测：横向布局下控件列起点 = 标签列宽度，两者之间
 * 没有额外间隙），而它又不能包在 `Form.Item label=" "` 里 —— 那个写法在
 * `requiredMark="optional"` 下会渲染出一个**空标签 + 「（可选）」**的怪东西，
 * 正好出现在用户最需要看明白的地方。
 */
const FORM_LABEL_WIDTH = 104;

/** Tag 规则可用变量。与后端 renderTag 支持的一一对应，多写一个就是一句假承诺。 */
const TAG_VARS: { name: string; desc: string }[] = [
  { name: '{buildNo}', desc: '本项目第几次构建，每次 +1（v1、v2、v3…）' },
  {
    // 这条不是"另一个占位符"，是 {buildNo} 的**偏移写法** —— 但必须列出来：
    // 不列，用户就只能靠猜 `{buildNo+995}` 这种写法是否合法（实测就是这么被问的：
    // 「你这里不显示，我怎么知道有什么规则？」）。
    name: '{buildNo+N}',
    desc: '给构建序号加固定偏移，版本号即可从某个数起步：{buildNo+999} → 首次构建就产出 1000，之后每次 +1（也支持 -N）',
  },
  { name: '{branch}', desc: '分支名（release/2.6 会写成 release-2.6）' },
  { name: '{commitId}', desc: '提交号前 8 位' },
  { name: '{timestamp}', desc: '构建时间 YYYYMMDDhhmmss' },
  { name: '{date}', desc: '构建日期 YYYYMMDD' },
];

/**
 * `{buildNo}` / `{buildNo+999}` / `{buildNo-1}` 的匹配。
 *
 * 与后端 `service.renderTag`（ci.go 的 `reBuildNo`）**是同一个正则**，
 * 改一处必须改两处，否则预览和真实 tag 会对不上。
 *
 * ⚠️ 只当 `.replace(...)` 的参数用（带 `g` 的正则配合 `test`/`exec` 会因 lastIndex
 * 残留而时对时错；`replace` 每次都会把 lastIndex 归零，所以这里安全）。
 */
const reBuildNo = /\{buildNo([+-]\d+)?\}/g;

/** 报错时列出来的可用写法。必须跟 TAG_VARS 里**真能用的**写法一致。 */
const TAG_VAR_HINT = '{buildNo} / {buildNo±N} / {branch} / {commitId} / {timestamp} / {date}';

/**
 * 找出 Tag 规则里**认不出来、会被原样拼进镜像 tag** 的花括号片段。
 *
 * 为什么必须拦：后端 `renderTag` 只替换它认识的那几个占位符，**剩下的原样保留** ——
 * 于是 `v{buildno}`（大小写写错）会拼出 `v{buildno}` 这个 tag，而 docker 不允许 tag 里
 * 有花括号，失败发生在 `docker build -t` 那一步、报的是 `invalid reference format`，
 * 与"你的 Tag 规则写错了"毫无关系。早一步说清楚，比让用户去查 docker 的报错划算。
 */
const unknownTagTokens = (pattern: string): string[] => {
  const rest = String(pattern || '')
    .replace(reBuildNo, '')
    .replace('{branch}', '')
    .replace('{commitId}', '')
    .replace('{timestamp}', '')
    .replace('{date}', '');
  if (!/[{}]/.test(rest)) return [];
  return rest.match(/\{[^}]*\}/g) ?? ['有未闭合的 { 或 }'];
};

/**
 * 预览「下次构建会产出的 tag」。
 *
 * ⚠️ 这里是**前端复刻**后端 `service.renderTag`（ci.go）的口径：`{buildNo}` 支持
 * `+N`/`-N` 内联偏移、负值夹到 0、其余占位符用固定的样例值。两边必须一致。
 *
 * 曾经这里只会 `.replace('{buildNo}', …)`，而它匹配不到 `{buildNo+995}`
 * （花括号位置不同），于是预览把 `2.2.0.{buildNo+995}` **原样**显示出来，
 * 看起来就像平台会产出一个带花括号的 tag。**改后端 renderTag 时务必回来改这里。**
 */
const previewTag = (pattern: string, nextNo: number, branch: string): string =>
  String(pattern || DEFAULT_TAG_PATTERN)
    .replace(reBuildNo, (_m, off: string | undefined) =>
      String(Math.max(0, nextNo + (off ? Number(off) : 0))),
    )
    .replace('{branch}', String(branch || 'master').replace(/[^A-Za-z0-9_.-]/g, '-'))
    .replace('{commitId}', 'a91f3c2')
    .replace('{timestamp}', '20260910163000')
    .replace('{date}', '20260910');

/** 脚本可用的环境变量。与后端 integration.scriptEnv 一一对应。 */
const SCRIPT_VARS: { name: string; desc: string }[] = [
  { name: 'YUNNIAN_REPO', desc: '代码仓库地址' },
  { name: 'YUNNIAN_BRANCH', desc: '本次构建分支' },
  { name: 'YUNNIAN_COMMIT', desc: '本次提交号' },
  { name: 'YUNNIAN_BUILD_NO', desc: '本项目第几次构建' },
  { name: 'YUNNIAN_IMAGE', desc: '本次要产出的完整镜像地址' },
  { name: 'YUNNIAN_REGISTRY', desc: '镜像仓库地址' },
  { name: 'YUNNIAN_IMAGE_NAME', desc: '镜像名（不含仓库与 tag）' },
  { name: 'YUNNIAN_TAG', desc: '本次 tag' },
  { name: 'YUNNIAN_PLATFORM', desc: '目标平台，如 linux/amd64' },
  { name: 'YUNNIAN_WORKDIR', desc: '本次构建的工作目录' },
];

/** 负责人候选：名单外的当前登录用户也要能出现，否则默认值会指向一个下拉里没有的项 */
const ownerOptions = () => {
  const me = getSession()?.name;
  const names = me && !OWNERS.includes(me) ? [...OWNERS, me] : OWNERS;
  return names.map((t) => ({ label: t, value: t }));
};

/** 表单里的 [{key,value}] → 接口要的 {key: value}；空键丢弃，避免后端收到无意义的空参数 */
const toParamsMap = (arr?: { key: string; value: string }[]): Record<string, string> =>
  Object.fromEntries((arr ?? []).filter((p) => p.key).map((p) => [p.key, p.value]));

/**
 * 拼接"仓库地址/镜像名"用于**展示**。
 *
 * 历史数据里两边都可能多带斜杠（一个项目 registry 结尾带 `/`、另一个 image_name
 * 开头带 `/`），直接 `${registry}/${imageName}` 会显示成 `:5000//escloud/xxx`。
 * 后端生成真正的镜像地址时会 trim，所以这里只是显示口径——
 * 但显示成双斜杠会让人以为平台生成的地址是坏的。
 */
const imageRepoOf = (registry?: string, imageName?: string) =>
  `${(registry ?? '').replace(/\/+$/, '')}/${(imageName ?? '').replace(/^\/+/, '')}`;

const ProjectList: React.FC = () => {
  const navigate = useNavigate();
  const { message, modal, notification } = AntApp.useApp();

  // 支持 ?keyword= 直接带筛选词进来：顶栏全局搜索回车后跳的就是这个地址，
  // 不解析的话用户会落在"已进入列表但筛选框是空的"的状态，很像没生效。
  const [searchParams, setSearchParams] = useSearchParams();
  const [keyword, setKeyword] = useState(() => searchParams.get('keyword') ?? '');
  const [type, setType] = useState<string>('all');
  const [enabledFilter, setEnabledFilter] = useState<string>('all');
  const [methodFilter, setMethodFilter] = useState<string | undefined>();
  const [selectedRowKeys, setSelectedRowKeys] = useState<React.Key[]>([]);

  /** 构建幂等键：打开弹窗时生成一次，提交时复用（防重复点击产生并行构建） */
  const idemKeyRef = useRef<string>('');

  // 批量打包向导
  const [batchOpen, setBatchOpen] = useState(false);

  // 筛选词回写地址栏（replace，不产生历史记录）：这样刷新页面筛选不丢，
  // 也能把"带筛选的项目列表"直接发给同事。
  useEffect(() => {
    const cur = searchParams.get('keyword') ?? '';
    if (cur === keyword) return;
    const next = new URLSearchParams(searchParams);
    if (keyword) next.set('keyword', keyword);
    else next.delete('keyword');
    setSearchParams(next, { replace: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [keyword]);

  // 编译弹窗
  const [buildTarget, setBuildTarget] = useState<Project | null>(null);
  const [buildForm] = Form.useForm();
  const [submitting, setSubmitting] = useState(false);

  /**
   * Jenkins 项目的构建参数（含"选哪个版本/分支"那个 git 参数）。
   *
   * 为什么不塞进 antd Form：这些参数最终要**一起**摊平成一次 Jenkins 提交
   * （见 collectJenkinsParams），值散在 Form 里再拼回来，最容易出现
   * "界面上明明填了、提交时漏了"——而这种漏报不会报错，只会构建出差一点的东西。
   */
  const [jkSelected, setJkSelected] = useState<Record<string, string | string[]>>({});

  /**
   * 该项目是否由 Jenkins 构建。它决定三件事：
   *   1) 分支从哪来——Jenkins 项目必须是绑定 Job 的真实版本标签/分支（GitParameter）；
   *   2) 提交打到哪个接口——Jenkins 触发接口，而不是平台的构建接口；
   *   3) 参数区长什么样——显示 Job 自身定义的参数，而不是平台那套 Key-Value。
   */
  const isJenkinsBuild = buildTarget?.buildMethod === 'jenkins';
  const jenkinsJobName = (buildTarget?.jenkinsJob ?? '').trim();
  const jk = useJenkinsParams(jenkinsJobName, !!buildTarget && isJenkinsBuild);

  /** 平台读得到默认值的参数预填出来；git 参数恒为 null，自然留空（= 不传该参数） */
  useEffect(() => {
    if (jk.defs) setJkSelected(defaultJenkinsSelection(jk.defs));
  }, [jk.defs]);

  /** 本次实际会发给 Jenkins 的参数。界面上"填了但没生效"这类问题靠它一眼看出 */
  const jenkinsWillSubmit = useMemo(
    () => collectJenkinsParams(jk.defs ?? [], jkSelected),
    [jk.defs, jkSelected],
  );

  /**
   * 非 Jenkins 项目的分支候选：来自项目仓库的**真实**分支列表。
   *
   * 之前这里是 4 个写死的名字（develop / release/1.9 …），是原型期留下的假数据——
   * 选项里既没有用户真正要的分支，又会让人以为"平台知道有哪些分支"。
   * 组件用 AutoComplete 而不是 Select：分支名可以是任意字符串，
   * 而 Select（不带 tags 模式）**只能从选项里挑**，"支持直接输入"是句空话。
   */
  const [branchOptions, setBranchOptions] = useState<string[]>([]);
  const [branchLoading, setBranchLoading] = useState(false);
  const [branchError, setBranchError] = useState('');

  /** 弹窗一开就按需拉一次分支：拉失败也只是"少了个便利"，输入框仍然能用 */
  useEffect(() => {
    if (!buildTarget || isJenkinsBuild) {
      setBranchOptions([]);
      setBranchError('');
      setBranchLoading(false);
      return;
    }
    let alive = true;
    setBranchLoading(true);
    setBranchError('');
    projectApi
      .branches(buildTarget.key)
      .then((list) => {
        if (alive) setBranchOptions(list ?? []);
      })
      .catch((e) => {
        if (!alive) return;
        setBranchOptions([]);
        setBranchError(e instanceof ApiError ? e.message : '读取项目分支失败');
      })
      .finally(() => {
        if (alive) setBranchLoading(false);
      });
    return () => {
      alive = false;
    };
  }, [buildTarget, isJenkinsBuild]);

  /** 候选分支：真实分支 + 项目默认分支（默认分支一定得能选到） */
  const branchCandidates = useMemo(() => {
    const all = [buildTarget?.defaultBranch ?? '', ...branchOptions].map((b) => b.trim()).filter(Boolean);
    return [...new Set(all)];
  }, [branchOptions, buildTarget?.defaultBranch]);

  const branchExtra = branchLoading
    ? '正在读取项目仓库的分支…'
    : branchError
      ? `读取分支失败（${branchError}）；仍可直接输入分支名`
      : `候选来自项目仓库的真实分支（${branchCandidates.length} 个）；也可直接输入不在列表里的分支名`;

  /**
   * Jenkins 项目的提交门控。禁用理由必须逐条对应真实后果，否则就是"点得动、发不出"：
   *   未绑定 Job        → 平台不知道该触发哪个 Job；
   *   参数还没读到      → 参数化 Job 不带参数触发，Jenkins 会用它自己的默认值构建，
   *                       很可能不是你想要的版本；
   *   读参数失败        → 同上，且不知道失败原因就提交等于盲构；
   *   有不认识的参数类型 → 送不齐参数，可能构建出错的东西。
   */
  const jenkinsBlockReason = !isJenkinsBuild
    ? ''
    : !jenkinsJobName
      ? '该项目还没绑定 Jenkins Job（在项目编辑里把构建方式设为 Jenkins Job 并选择），平台不知道该触发哪个 Job'
      : jk.loading
        ? '正在读取 Jenkins 构建参数…'
        : jk.error
          ? `读取 Jenkins 构建参数失败：${jk.error}`
          : jk.unknownDefs.length
            ? `该 Job 有 ${jk.unknownDefs.length} 个参数（${jk.unknownDefs
                .map((d) => d.name)
                .join('、')}）的类型平台还不支持，提交无法保证参数完整——请到 Jenkins 页面构建`
            : '';

  // 编辑抽屉
  const [editTarget, setEditTarget] = useState<Project | null>(null);
  const [drawerOpen, setDrawerOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  const [form] = Form.useForm();

  /**
   * 「构建方式」是表单里的一个字段（jenkins / dockerfile / script），不再是两个入口两种形态。
   *
   * 以前是在门外先选一次，而进去之后 buildMethod 是**禁用**的 —— 选错了只能退出来重进；
   * 编辑时更是完全不能换方式。现在门外不选（一个入口），门里三选一，**新建默认 Jenkins Job**。
   *
   * 渲染哪组字段由它决定（见 isJenkinsForm），但提交仍走同一个 POST / PUT /projects ——
   * 少一条路径就少一处会漂移的口径。
   */

  // Jenkins Job：本页只保留"项目表单里的 Job 下拉"。
  // Job 列表本身已经独立成「CI 镜像构建 › Jenkins Job」菜单页了——
  // 那是一个要搜索 / 筛选 / 翻页的正式列表，塞在抽屉里看不完也筛不动。
  const [jenkinsJobs, setJenkinsJobs] = useState<JenkinsJob[] | null>(null);
  const [jobsLoading, setJobsLoading] = useState(false);
  const [jobsError, setJobsError] = useState<string | null>(null);

  /**
   * 项目列表。
   *
   * 这里**一次拉全量（pageSize 200）并在前端筛选**，而不是把 keyword/type 交给后端：
   *   1. 一期的筛选标签要显示分类计数（全部 20 / 前端 10 / 后端 10），
   *      后端没有分组统计接口，分页查询拿不到这些数；
   *   2. 纳管项目是个位数到几十的量级，一次拉完比"每敲一个字请求一次"体验好。
   * 项目数上百时应改为后端筛选 + 分组统计接口——那时的取舍会反过来。
   */
  const projectsApi = useApi(() => projectApi.list({ pageSize: 200 }), []);
  const projects = useMemo(() => projectsApi.data?.list ?? [], [projectsApi.data]);
  /** 变更后重拉列表（建/改/删/停用都要调，否则界面与库里会各说各话） */
  const refresh = projectsApi.refresh;

  /**
   * 自动刷新。
   *
   * 构建状态是**别处**在改的（Runner / Jenkins / 部署执行器），页面自己不会变——
   * 用户不手点，这一列就会一直停在「构建中」，看着像卡死。
   *
   * 走 refreshSilent 而不是 refresh：每 3 秒闪一次加载遮罩比不刷新更难受；
   * 而且后台那一拍要是失败就把整页打成错误态，等于"网络抖一下，列表没了"。
   * 静默那拍失败只在刷新控件里留一句原因，下一次成功自动清掉。
   *
   * 切到后台会暂停（浏览器不会因为标签页不可见就停掉定时器），切回来立刻补一次。
   */
  const [autoRefreshMs, setAutoRefreshMs] = useAutoRefreshPref(3000);
  useAutoRefresh({
    enabled: autoRefreshMs !== null,
    intervalMs: autoRefreshMs ?? 3000,
    onTick: projectsApi.refreshSilent,
    // 上一拍还没回来就跳过这一拍，避免慢后端下滚出一串在飞请求
    isBusy: () => projectsApi.pendingRef.current,
  });

  // 写权限：没有就禁用按钮并说明原因，而不是让用户点下去吃一个 403
  const canWrite = hasPerm('project:write') || hasPerm('project:write:own');
  const canDelete = hasPerm('project:delete');

  const dataSource = useMemo(
    () =>
      projects.filter((p) => {
        const kw = keyword.trim().toLowerCase();
        const hitKw =
          !kw || p.name.toLowerCase().includes(kw) || p.key.toLowerCase().includes(kw) || p.owner.includes(kw);
        const hitType = type === 'all' || p.type === type;
        const hitEnabled =
          enabledFilter === 'all' || (enabledFilter === 'enabled' ? p.enabled : !p.enabled);
        const hitMethod = !methodFilter || p.buildMethod === methodFilter;
        return hitKw && hitType && hitEnabled && hitMethod;
      }),
    [projects, keyword, type, enabledFilter, methodFilter],
  );

  // 编译弹窗里的"最近构建"：按当前选中的项目拉 3 条（弹窗没打开时不发请求）
  const historyApi = useApi(
    () => buildApi.list({ projectKey: buildTarget!.key, pageSize: 3 }),
    [buildTarget?.key],
    { enabled: !!buildTarget },
  );
  const runningHistory = historyApi.data?.list ?? [];

  /** 表单里当前选的构建方式（jenkins / dockerfile / script），它决定渲染哪一组字段 */
  const buildMethodWatch = Form.useWatch('buildMethod', form);
  /** Jenkins 形态：仓库 / 构建上下文 / 镜像规则等"平台构建专属字段"一律不出现 */
  const isJenkinsForm = buildMethodWatch === 'jenkins';

  // 「测试连接」的状态。结果**留在页面上**而不是只弹一个 toast：
  // 失败原因（账号不对 / 仓库不存在 / 连不上）是用户接着要动手改哪一格的依据，
  // 一条 3 秒后消失的提示他可能还没读完。
  const [testing, setTesting] = useState(false);
  const [testRes, setTestRes] = useState<GitConnectionResult | null>(null);
  const [loadingDefaultScript, setLoadingDefaultScript] = useState(false);

  /**
   * 取平台默认脚本填进编辑器。
   *
   * 只在「脚本是空的」时才自动填：编辑一个已有脚本的项目时，进去就把它覆盖成默认值
   * 等于**偷偷丢掉用户写的东西**，而保存按钮还在那儿，看起来像是他自己改的。
   */
  const fillDefaultScript = async (force = false) => {
    if (!force && String(form.getFieldValue('buildScript') ?? '').trim()) return;
    setLoadingDefaultScript(true);
    try {
      const script = await projectApi.defaultBuildScript(String(form.getFieldValue('type') ?? 'backend'));
      if (script) form.setFieldsValue({ buildScript: script });
    } catch (e) {
      message.error(e instanceof ApiError ? e.message : '取默认脚本失败');
    } finally {
      setLoadingDefaultScript(false);
    }
  };

  // 抽屉打开、选了自定义脚本、而脚本还是空的 → 补一份默认的。
  // 不补的话用户会面对一个空白编辑器，而"空白脚本"在构建时是安静地什么都不做。
  useEffect(() => {
    if (drawerOpen && !isJenkinsForm && buildMethodWatch === 'script') {
      void fillDefaultScript(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [drawerOpen, buildMethodWatch, isJenkinsForm]);

  /**
   * 真去读一次仓库。
   *
   * 密码留空时后端会回落到已保存的凭据——界面上看不到密码原文，也不该逼用户
   * 为了"测一下"重新敲一遍。分支一起带上：远端没有这个分支是"构建必失败"的
   * 第一大来源，而它恰恰是**保存前就能发现**的。
   */
  const runTestConnection = async () => {
    const v = form.getFieldsValue(['gitUrl', 'gitUsername', 'gitPassword', 'defaultBranch']);
    setTesting(true);
    setTestRes(null);
    try {
      const res = await projectApi.testConnection({
        projectKey: editTarget?.key,
        gitUrl: String(v.gitUrl ?? ''),
        gitUsername: String(v.gitUsername ?? ''),
        gitPassword: String(v.gitPassword ?? ''),
        branch: String(v.defaultBranch ?? ''),
      });
      setTestRes(res);
    } catch (e) {
      setTestRes({
        ok: false,
        message: e instanceof ApiError ? e.message : '测试失败，请稍后重试',
        branches: [],
        // 本地兜底：请求根本没到后端，远端分支数无从得知 —— 给 0 而不是编一个。
        branchTotal: 0,
        elapsedMs: 0,
      });
    } finally {
      setTesting(false);
    }
  };

  /**
   * 拉取 Jenkins Job 列表。
   *
   * 只在真的选了 Jenkins 构建方式时才拉——没接 Jenkins 的项目不该为这个下拉
   * 白付一次请求。反过来，选中了却拉不到时，错误要留在下拉里说清楚，
   * 而不是退化成一个空的必填下拉，让人以为自己 Jenkins 里没有 job。
   */
  const loadJenkinsJobs = async () => {
    setJobsLoading(true);
    try {
      const res = await jenkinsApi.jobs({ refresh: true });
      setJenkinsJobs(res.jobs);
      setJobsError(null);
    } catch (e) {
      setJenkinsJobs([]);
      setJobsError(e instanceof ApiError ? e.message : '拉取 Jenkins Job 失败');
    } finally {
      setJobsLoading(false);
    }
  };

  // 打开编辑抽屉、且构建方式选了 Jenkins 时按需拉一次（已拉过不重复拉，下拉里有刷新）
  useEffect(() => {
    if (drawerOpen && buildMethodWatch === 'jenkins' && jenkinsJobs === null) {
      void loadJenkinsJobs();
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [drawerOpen, buildMethodWatch]);

  /**
   * Jenkins Job 下拉。新建（Jenkins 入口）与编辑两处共用同一份渲染：
   * 选项文案里的「已绑定 xxx」标记、拉取失败时的原因与重试、底部的刷新入口，
   * 这几处口径必须一致——各写一份，迟早只改一处，"编辑里能刷新、新建里不能"
   * 这类差异就是这么冒出来的。
   */
  const jenkinsJobSelect = (
    <Select
      showSearch
      allowClear
      placeholder={jobsLoading ? '正在拉取…' : '选择 Jenkins Job'}
      loading={jobsLoading}
      optionFilterProp="label"
      notFoundContent={
        jobsError ? (
          <div style={{ padding: '8px 4px', fontSize: 12 }}>
            <div style={{ color: '#cf1322' }}>{jobsError}</div>
            <Button type="link" size="small" style={{ padding: 0 }} onClick={() => void loadJenkinsJobs()}>
              重试
            </Button>
          </div>
        ) : (
          '没有可用的 Job，请先接入 Jenkins'
        )
      }
      options={(jenkinsJobs ?? []).map((j) => ({
        // 已被别的项目绑走的 Job 要标出来：同一个 Job 绑两个项目
        // 会让"改了构建参数却影响另一个项目"变得无法解释。
        label:
          j.boundProjectKey && j.boundProjectKey !== editTarget?.key
            ? `${j.fullName}（已绑定 ${j.boundProjectKey}）`
            : j.fullName,
        value: j.fullName,
      }))}
      dropdownRender={(menu) => (
        <>
          {menu}
          <Divider style={{ margin: '4px 0' }} />
          <div style={{ padding: '0 4px 4px' }}>
            <Button
              type="text"
              size="small"
              block
              icon={<ReloadOutlined />}
              loading={jobsLoading}
              onClick={() => void loadJenkinsJobs()}
            >
              刷新 Job 列表
            </Button>
          </div>
        </>
      )}
    />
  );

  /**
   * Jenkins 形态：只把项目**绑定**到一个 Jenkins Job（不含基本信息 —— 那是全表单共用的）。
   *
   * 为什么这里不再出现仓库与构建参数：这个项目的代码怎么拉、怎么构建、镜像推到哪儿，
   * 全写在那个 Jenkins Job 里。平台再让用户录一遍 Git 地址与 Tag 规则，
   * 只会得到一份**没有任何代码去读**的配置——它与 Job 里的真配置各说各话，
   * 半年后没人分得清哪个才算数。
   */
  const jenkinsFields = (
    <>
      <Row gutter={16}>
        <Col span={24}>
          <Form.Item
            name="jenkinsJob"
            label="Jenkins Job"
            rules={[{ required: true, message: '请选择 Jenkins Job' }]}
            tooltip="列表来自「系统管理 › 第三方集成」里配置的 Jenkins；带文件夹的 Job 显示完整路径"
          >
            {jenkinsJobSelect}
          </Form.Item>
        </Col>
      </Row>

      <Alert
        type="info"
        showIcon
        message="仓库地址与构建参数不需要在这里配"
        description={
          <>
            代码从哪拉、怎么构建、镜像推到哪个仓库，都由这个 Jenkins Job 自己定义；
            平台只负责触发它、展示构建日志，并从日志里读出本次产出的镜像版本（读不到就留空）。
          </>
        }
      />
    </>
  );

  /**
   * 平台构建的字段（自带 Dockerfile / 自定义脚本）。基本信息与「构建方式」由上层 projectForm 提供，
   * 这里只放"选了平台构建之后才需要填"的东西。
   *
   * 「构建上下文路径」「Dockerfile 路径」只在自带 Dockerfile 时出现：
   * 自定义脚本模式下 cd 到哪、在哪个目录 build 都由脚本自己定，平台无从也无需知道。
   */
  const platformFields = (
    <>
      <Divider orientation="left" plain style={{ marginTop: 0 }}>
        代码仓库与凭据
      </Divider>
      <Row gutter={16}>
        <Col span={16}>
          <Form.Item
            name="gitUrl"
            label="仓库地址"
            rules={[{ required: true, message: '请输入仓库地址' }]}
            extra="平台靠它列分支、收 Webhook 自动构建"
          >
            <Input prefix={<BranchesOutlined style={{ color: '#b5bac4' }} />} placeholder="http://gitlab.example.com/group/app.git" />
          </Form.Item>
        </Col>
        <Col span={8}>
          <Form.Item name="defaultBranch" label="默认分支" rules={[{ required: true }]}>
            <Input placeholder="master" />
          </Form.Item>
        </Col>
        <Col span={12}>
          <Form.Item name="gitUsername" label="账号" extra="有读权限即可；留空表示匿名拉取（公开仓库）">
            <Input placeholder="如 escloudapi" autoComplete="off" />
          </Form.Item>
        </Col>
        <Col span={12}>
          <Form.Item
            name="gitPassword"
            label="密码 / 令牌"
            extra={editTarget ? '已保存，留空表示不修改' : '加密存库，不回显'}
          >
            {/* 编辑时永远为空：任何"把密码回填进输入框"的做法，都等于把明文发到浏览器，
                而它会被浏览器的密码管理器、前端日志、截图各留一份。留空 = 不修改。 */}
            <Input.Password placeholder={editTarget ? '留空则不修改' : '仅用于拉取代码'} autoComplete="new-password" />
          </Form.Item>
        </Col>
        <Col span={24}>
          {/* 不用 Form.Item：见 FORM_LABEL_WIDTH 的注释。paddingLeft 与标签列同宽，
              左边缘和上面的输入框逐像素对齐。 */}
          <div style={{ paddingLeft: FORM_LABEL_WIDTH, marginBottom: 24 }}>
            <Space align="center" wrap>
              <Button onClick={() => void runTestConnection()} loading={testing}>
                测试连接
              </Button>
              {testRes && (
                <span style={{ fontSize: 12.5, color: testRes.ok ? '#389e0d' : '#cf1322' }}>
                  {/* 成功时不再拼上 message：后端成功时的 message 就是「连接成功」，
                      拼出来是「连接成功：连接成功（…）」—— 同一句话说两遍。 */}
                  {testRes.ok ? (
                    <>
                      连接成功
                      {/* 用 branchTotal（远端真实数）而不是 branches.length。
                          branches 会被后端截到 100，拿它的长度当"读到了多少分支"
                          是句假话：仓库有 229 个分支时会显示"读到 100 个分支"。
                          被截断时明说"已列出前 N 个"，别让用户以为漏读了几百个。 */}
                      {testRes.branchTotal > 0
                        ? `（读到 ${testRes.branchTotal} 个分支${
                            testRes.branches.length < testRes.branchTotal
                              ? `，已列出前 ${testRes.branches.length} 个`
                              : ''
                          }，用时 ${testRes.elapsedMs}ms）`
                        : ''}
                    </>
                  ) : (
                    <>连接失败：{testRes.message}</>
                  )}
                </span>
              )}
            </Space>
          </div>
        </Col>
        {testRes && !testRes.ok && testRes.detail && (
          <Col span={24}>
            <Alert
              type="error"
              showIcon
              message={testRes.message}
              description={
                <pre className="yn-mono" style={{ margin: 0, fontSize: 12, whiteSpace: 'pre-wrap', maxHeight: 140, overflow: 'auto' }}>
                  {testRes.detail}
                </pre>
              }
            />
          </Col>
        )}
      </Row>

      <Divider orientation="left" plain>
        镜像与版本
      </Divider>
      <Row gutter={16}>
        <Col span={12}>
          <Form.Item name="registry" label="镜像仓库地址" rules={[{ required: true }]}>
            <Input placeholder="harbor.yunnian.internal" />
          </Form.Item>
        </Col>
        <Col span={12}>
          <Form.Item name="imageName" label="镜像名称" rules={[{ required: true }]}>
            <Input placeholder="backend/user-service" />
          </Form.Item>
        </Col>
        <Col span={24}>
          <Form.Item
            name="tagPattern"
            label="Tag 规则"
            rules={[
              { required: true },
              {
                // 认不出的写法会被后端**原样**拼进镜像 tag，最终在 `docker build -t`
                // 那一步失败（invalid reference format）。能在这里说清楚就别留给 docker。
                validator: (_, v: string) => {
                  const bad = unknownTagTokens(v);
                  if (!bad.length) return Promise.resolve();
                  return Promise.reject(
                    new Error(`认不出的写法 ${bad.join('、')}；可用：${TAG_VAR_HINT}`),
                  );
                },
              },
            ]}
            extra={
              <div style={{ display: 'grid', gridTemplateColumns: 'repeat(2, minmax(0,1fr))', gap: '2px 12px' }}>
                {TAG_VARS.map((v) => (
                  <span key={v.name}>
                    <Text className="yn-mono" style={{ fontSize: 12 }}>{v.name}</Text>
                    <Text type="secondary" style={{ fontSize: 12 }}> {v.desc}</Text>
                  </span>
                ))}
              </div>
            }
          >
            <Input placeholder={DEFAULT_TAG_PATTERN} className="yn-mono" />
          </Form.Item>
        </Col>
        <Col span={24}>
          <div style={{ background: '#fafbfc', border: '1px solid #eceef2', borderRadius: 6, padding: '10px 14px', fontSize: 12.5, color: '#5c6470', marginBottom: 16 }}>
            <Form.Item noStyle shouldUpdate>
              {({ getFieldValue }) => {
                // 「下次构建会产出的镜像」。buildNo 预取 totalBuilds + 1，
                // 因为后端的下一号就是"已构建过的次数 + 1"。标成"预计"是因为
                // 它终究是算出来的，真正落库的以构建记录为准。
                //
                // 渲染交给 previewTag（它复刻后端 renderTag 的口径，含 {buildNo±N} 偏移）——
                // 别在这里就地写 replace，那是这份预览第二次出现两套口径的起点。
                const nextNo = (editTarget?.totalBuilds ?? 0) + 1;
                const tag = previewTag(
                  String(getFieldValue('tagPattern') || ''),
                  nextNo,
                  String(getFieldValue('defaultBranch') || 'master'),
                );
                return (
                  <span>
                    预计下次构建产出：<span className="yn-mono" style={{ color: '#1677ff' }}>
                      {getFieldValue('registry') || 'harbor.yunnian.internal'}/
                      {getFieldValue('imageName') || 'backend/user-service'}:{tag}
                    </span>
                  </span>
                );
              }}
            </Form.Item>
          </div>
        </Col>
      </Row>

      <Divider orientation="left" plain>
        {buildMethodWatch === 'script' ? '构建脚本' : 'Dockerfile'}
      </Divider>

      {buildMethodWatch === 'script' ? (
        <Form.Item
          name="buildScript"
          label="构建脚本"
          rules={[{ required: true, message: '自定义脚本不能为空（可点「恢复默认脚本」）' }]}
          extra={
            <div>
              <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'flex-start', gap: 12 }}>
                <span>
                  平台已 cd 到本次构建的空白工作目录，git 与 docker 可直接用，仓库凭据已配好
                  （直接写 clone 地址，不要在脚本里写账号密码）。
                </span>
                <Button
                  type="link"
                  size="small"
                  style={{ padding: 0, height: 'auto', whiteSpace: 'nowrap' }}
                  loading={loadingDefaultScript}
                  onClick={() => void fillDefaultScript(true)}
                >
                  恢复默认脚本
                </Button>
              </div>
              <div style={{ display: 'grid', gridTemplateColumns: 'repeat(2, minmax(0,1fr))', gap: '2px 12px', marginTop: 4 }}>
                {SCRIPT_VARS.map((v) => (
                  <span key={v.name}>
                    <Text className="yn-mono" style={{ fontSize: 12 }}>{v.name}</Text>
                    <Text type="secondary" style={{ fontSize: 12 }}> {v.desc}</Text>
                  </span>
                ))}
              </div>
            </div>
          }
        >
          <Input.TextArea
            rows={16}
            className="yn-mono"
            style={{ fontSize: 12.5, lineHeight: 1.7 }}
            placeholder="set -e&#10;git clone --depth 1 -b &quot;$YUNNIAN_BRANCH&quot; &quot;$YUNNIAN_REPO&quot; src&#10;…"
          />
        </Form.Item>
      ) : (
        <Row gutter={16}>
          <Col span={12}>
            <Form.Item name="dockerfilePath" label="Dockerfile 路径" extra="相对于仓库根目录">
              <Input placeholder="Dockerfile" className="yn-mono" />
            </Form.Item>
          </Col>
          <Col span={12}>
            <Form.Item name="buildContext" label="构建上下文路径" extra="docker build 的最后一个参数，默认仓库根目录">
              <Input placeholder="." className="yn-mono" />
            </Form.Item>
          </Col>
        </Row>
      )}

      <Divider orientation="left" plain>
        构建参数
      </Divider>
      <div style={{ color: '#5c6470', fontSize: 13, marginBottom: 14 }}>
        参数在构建时注入为环境变量（如 <Text className="yn-mono" style={{ fontSize: 12 }}>AREA=test</Text>），
        可在「一键编译」时临时覆盖。不需要就留空。
      </div>
      <Form.Item name="buildParams" noStyle>
        <KVFields name="buildParams" addText="添加参数" />
      </Form.Item>
    </>
  );

  /**
   * 项目表单（新建 / 编辑共用一张）。**一页到底**，关键字段都跟一句说明。
   *
   * 版式顺序：基本信息 → 构建方式 → （按构建方式展开的字段）。
   * 「构建方式」放在基本信息**之后**：先让用户把"这是哪个项目"填清楚，再决定它怎么构建；
   * 反过来先选构建方式，会让人在还没想好项目归属时就被一个技术选项拦住。
   *
   * 为什么合并成一张：以前是两个入口两套表单，用户得先在门外选一次，进去之后
   * 构建方式还是**禁用**的 —— 选错了只能退出来重进。现在门外不选、门里选。
   */
  const projectForm = (
    <>
      <Divider orientation="left" plain style={{ marginTop: 0 }}>
        基本信息
      </Divider>
      <Row gutter={16}>
        <Col span={12}>
          <Form.Item name="name" label="项目名称" rules={[{ required: true, message: '请输入项目名称' }]}>
            <Input placeholder="如 用户中心前端" />
          </Form.Item>
        </Col>
        <Col span={12}>
          <Form.Item
            name="key"
            label="项目标识"
            rules={[
              { required: true, message: '请输入项目标识' },
              { pattern: /^[a-z0-9-]+$/, message: '仅允许小写字母、数字与中划线' },
            ]}
            extra="全局唯一，用于镜像名称与部署服务名"
          >
            <Input placeholder="如 user-web" disabled={!!editTarget} addonBefore={<CodeOutlined />} />
          </Form.Item>
        </Col>
        <Col span={12}>
          <Form.Item name="type" label="项目类型" rules={[{ required: true }]} extra="新建脚本时按它给默认脚本">
            <Select
              options={[
                { label: '前端', value: 'frontend' },
                { label: '后端', value: 'backend' },
              ]}
            />
          </Form.Item>
        </Col>
        <Col span={12}>
          <Form.Item name="team" label="所属团队" rules={[{ required: true }]}>
            <Select showSearch options={TEAMS.map((t) => ({ label: t, value: t }))} />
          </Form.Item>
        </Col>
        <Col span={12}>
          <Form.Item name="owner" label="负责人" rules={[{ required: true }]}>
            <Select showSearch options={ownerOptions()} />
          </Form.Item>
        </Col>
        <Col span={12}>
          <Form.Item name="enabled" label="启用状态" valuePropName="checked">
            <Switch checkedChildren="启用" unCheckedChildren="停用" />
          </Form.Item>
        </Col>
        <Col span={24}>
          <Form.Item name="description" label="项目描述">
            <Input.TextArea rows={2} placeholder="简要说明项目用途" maxLength={200} showCount />
          </Form.Item>
        </Col>
      </Row>

      <Divider orientation="left" plain>
        构建方式
      </Divider>
      <Form.Item
        name="buildMethod"
        label="构建方式"
        rules={[{ required: true }]}
        // 三选一的差异必须在这里说清：选错一次，下面要填的整组字段都会跟着错。
        extra="Jenkins Job：代码怎么拉、怎么构建、镜像推到哪都由该 Job 定义，平台只触发并读日志；自带 Dockerfile：编译与打包写在你仓库的 Dockerfile 里；自定义脚本：整个构建流程由下方的脚本定义"
      >
        <Segmented
          options={[
            { label: 'Jenkins Job', value: 'jenkins' },
            { label: '自带 Dockerfile', value: 'dockerfile' },
            { label: '自定义脚本', value: 'script' },
          ]}
        />
      </Form.Item>

      {isJenkinsForm ? jenkinsFields : platformFields}
    </>
  );

  // ------------------------------------------------------------------- 操作

  const openBuild = (p: Project) => {
    setBuildTarget(p);
    idemKeyRef.current = `ui-${p.key}-${Date.now()}`;
    setJkSelected({});
    buildForm.setFieldsValue({
      // Jenkins 项目的分支**不预填**：平台读不到 GitParameter 的真实默认值
      // （插件不通过 API 暴露），拿项目的 defaultBranch 顶上就是"猜"——
      // 猜错会静默构建出另一个版本。留空 = 不传该参数 = Jenkins 用它自己的默认值。
      branch: p.buildMethod === 'jenkins' ? undefined : p.defaultBranch,
      buildParams: p.buildParams,
      tagOverride: '',
      notify: true,
    });
  };

  /**
   * 提交构建。**两条完全不同的链路，按项目的构建方式分流**：
   *
   *   - Jenkins 项目（buildMethod=jenkins）→ 打 Jenkins 触发接口。
   *     必须这样分流：平台的构建接口（下面那条）**不看 buildMethod**，它会把
   *     "刚从一个 Jenkins 版本标签下拉里选出来的值"当成平台自己的分支名，
   *     去代码托管查一个提交、再跑一次平台的 docker 构建——最后成功落一条
   *     和 Jenkins 毫无关系的构建记录。这属于最难发现的那种错：一切看起来都对。
   *
   *   - 其余项目 → 平台自己的构建链路，行为与之前完全一致。
   *
   * 幂等键在**打开弹窗时**生成、提交时复用：同一次"打开→提交"如果因为网络慢
   * 被点了两次，后端凭这个键返回同一张构建单，而不是排出两条并行构建。
   * 提交成功后才刷新键，下一次点击才算新任务。
   */
  const submitBuild = async () => {
    if (!buildTarget) return;
    const values = await buildForm.validateFields();
    if (jenkinsBlockReason) {
      message.warning(jenkinsBlockReason);
      return;
    }
    setSubmitting(true);
    try {
      if (isJenkinsBuild) {
        const res = await jenkinsApi.trigger(jenkinsJobName, jenkinsWillSubmit, idemKeyRef.current);
        setBuildTarget(null);
        idemKeyRef.current = '';
        notification.success({
          message: `已提交 Jenkins 构建 · ${jenkinsJobName}`,
          description: (
            <span>
              项目 <b>{buildTarget.name}</b> 的 Jenkins 构建已受理（队列项 #{res.queueId}）。
              {jenkinsWillSubmit.length
                ? ` 提交参数：${jenkinsWillSubmit.map((p) => `${p.name} = ${p.value}`).join('，')}。`
                : ' 未指定参数：按 Jenkins 侧配置构建。'}
              {res.message}构建号要等 Jenkins 分配后由平台补齐，可在「构建记录」看实时日志。
            </span>
          ),
          placement: 'topRight',
          duration: 8,
        });
        // 列表里的"最近构建"要跟着变，所以刷一次
        projectsApi.refresh();
        return;
      }

      const b = await buildApi.trigger({
        projectKey: buildTarget.key,
        branch: values.branch,
        params: toParamsMap(values.buildParams),
        idempotencyKey: idemKeyRef.current,
      });
      setBuildTarget(null);
      notification.success({
        message: '构建任务已提交',
        description: (
          <span>
            项目 <b>{buildTarget.name}</b> 的构建 <b>#{b.buildNo}</b> 已进入队列（分支：{b.branch}）。
            可在「构建记录」中查看实时日志。
          </span>
        ),
        placement: 'topRight',
        duration: 6,
      });
      // 列表里的"最近构建"要跟着变，所以刷一次
      projectsApi.refresh();
    } catch (e) {
      message.error(e instanceof ApiError ? e.message : '触发构建失败，请稍后重试');
    } finally {
      setSubmitting(false);
    }
  };

  const openCreate = () => {
    setEditTarget(null);
    form.resetFields();
    setTestRes(null);
    form.setFieldsValue({
      // 基本信息：一期没有维护入口，先给默认值，用户只填名称与标识即可提交
      type: DEFAULT_TYPE,
      team: DEFAULT_TEAM,
      owner: getSession()?.name ?? '',
      // 默认 Jenkins Job：公司既有资产是 Jenkins，多数项目挂过去就能构建，
      // 不必在平台上再配一套仓库 / 构建上下文 / 构建脚本。
      buildMethod: 'jenkins',
      registry: projects[0]?.registry ?? '',
      // 默认 Tag 规则与后端 buildscript.DefaultTagPattern 同值：v1、v2、v3…
      // 每构建一次自动递增一个版本号，而且一眼看得出哪个更新。
      tagPattern: DEFAULT_TAG_PATTERN,
      dockerfilePath: 'Dockerfile',
      buildContext: '.',
      defaultBranch: 'master',
      gitUsername: '',
      gitPassword: '',
      buildScript: '',
      enabled: true,
      // 构建参数**不预置**：预置一个没人读的值，用户会以为它已经生效了。
      // 需要的人自己加（如 AREA=test），它在构建时注入为环境变量。
      buildParams: [],
    });
    setDrawerOpen(true);
  };

  const openEdit = (p: Project) => {
    setEditTarget(p);
    // 构建方式由 form.setFieldsValue(p) 一并带进来（buildMethod 是项目字段），
    // 渲染哪组字段会自动跟着变 —— 也就是说**编辑时也能换构建方式**了。
    setTestRes(null);
    form.setFieldsValue(p);
    setDrawerOpen(true);
  };

  const submitProject = async () => {
    const values = await form.validateFields();
    const body: Partial<Project> = {
      ...values,
      buildParams: (values.buildParams as { key: string; value: string; secret?: boolean }[])?.filter(
        (p) => p.key,
      ),
    };
    // 选 Jenkins 时不渲染「仓库地址」与「构建参数」——它们由 Job 自己定义。
    // 后端对多数字段是"没传就沿用原值"，但这两项是**直接覆盖**（见 UpdateProject 的
    // git_url / build_params），不显式带回原值，一次编辑就会把它们抹掉。
    if (editTarget && values.buildMethod === 'jenkins') {
      body.gitUrl = editTarget.gitUrl;
      body.buildParams = editTarget.buildParams;
    }
    setSaving(true);
    try {
      if (editTarget) {
        await projectApi.update(editTarget.key, body);
        message.success(`项目「${values.name}」已保存`);
      } else {
        await projectApi.create(body);
        message.success(`项目「${values.name}」已创建`);
      }
      setDrawerOpen(false);
      refresh();
    } catch (e) {
      // 失败时**不关抽屉**：用户改了半天的表单不能因为一次网络抖动就丢掉
      message.error(e instanceof ApiError ? e.message : '保存失败，请稍后重试');
    } finally {
      setSaving(false);
    }
  };

  const removeProject = (p: Project) => {
    modal.confirm({
      title: `确认删除项目「${p.name}」？`,
      content: (
        <div style={{ fontSize: 13, color: '#5c6470' }}>
          删除后该项目的构建记录、镜像版本将不再展示，已部署的服务不受影响。
          <br />
          如需保留历史，建议改为「停用」。
        </div>
      ),
      okText: '确认删除',
      okButtonProps: { danger: true },
      cancelText: '取消',
      onOk: async () => {
        try {
          await projectApi.remove(p.key);
          message.success(`已删除项目 ${p.key}`);
          refresh();
        } catch (e) {
          message.error(e instanceof ApiError ? e.message : '删除失败，请稍后重试');
          // 抛出以阻止 Modal 关闭：失败了还关掉确认框，用户会以为删成功了
          throw e;
        }
      },
    });
  };

  /**
   * 导出当前筛选结果。
   *
   * 在浏览器本地拼 CSV——一期没有导出接口，而"把筛选后的清单发给同事"这个需求
   * 用一个 Blob 就解决了，不值得为它开一个服务端任务。
   * 加 UTF-8 BOM：不加的话 Excel 打开中文列名会乱码。
   */
  const exportCsv = () => {
    const head = ['项目标识', '项目名称', '类型', '团队', '负责人', '构建方式', '仓库', '镜像名', 'Tag 规则', '最近构建', '成功率', '状态'];
    const rows = dataSource.map((p) => [
      p.key,
      p.name,
      p.type === 'frontend' ? '前端' : '后端',
      p.team,
      p.owner,
      p.buildMethod,
      p.gitUrl,
      imageRepoOf(p.registry, p.imageName),
      p.tagPattern,
      p.lastBuild?.startedAt ?? '',
      `${p.successRate}%`,
      p.enabled ? '已启用' : '已停用',
    ]);
    const esc = (v: string) => `"${String(v ?? '').replace(/"/g, '""')}"`;
    const csv = [head, ...rows].map((r) => r.map(esc).join(',')).join('\r\n');
    const url = URL.createObjectURL(new Blob([`\uFEFF${csv}`], { type: 'text/csv;charset=utf-8' }));
    const a = document.createElement('a');
    a.href = url;
    a.download = `yunnian-projects-${new Date().toISOString().slice(0, 10)}.csv`;
    a.click();
    URL.revokeObjectURL(url);
    message.success(`已导出 ${dataSource.length} 个项目`);
  };

  /** 批量停用：逐个调接口，而不是"显示一条成功提示"。部分失败要如实报出来。 */  const batchDisable = async () => {
    const targets = projects.filter((p) => selectedRowKeys.includes(p.key) && p.enabled);
    if (!targets.length) {
      message.info('所选项目都已经是停用状态');
      return;
    }
    setSaving(true);
    const failed: string[] = [];
    for (const p of targets) {
      try {
        await projectApi.setEnabled(p.key, false);
      } catch {
        failed.push(p.key);
      }
    }
    setSaving(false);
    setSelectedRowKeys([]);
    refresh();
    if (failed.length) {
      message.warning(`已停用 ${targets.length - failed.length} 个，${failed.length} 个失败：${failed.join('、')}`);
    } else {
      message.success(`已停用 ${targets.length} 个项目`);
    }
  };

  // ------------------------------------------------------------------- 列定义

  const columns: ColumnsType<Project> = [
    {
      title: '项目名称',
      dataIndex: 'name',
      fixed: 'left',
      width: 210,
      render: (_, r) => (
        <div>
          <Link to={`/ci/projects/${r.key}`} style={{ fontWeight: 500 }}>
            {r.name}
          </Link>
          <div style={{ fontSize: 12, color: '#9aa0aa', marginTop: 2 }}>
            <span className="yn-mono">{r.key}</span>
            <Divider type="vertical" style={{ margin: '0 6px' }} />
            {r.team}
          </div>
        </div>
      ),
    },
    {
      title: '类型',
      dataIndex: 'type',
      width: 80,
      filters: [
        { text: '前端', value: 'frontend' },
        { text: '后端', value: 'backend' },
      ],
      onFilter: (v, r) => r.type === v,
      render: (v) => <ProjectTypeTag type={v} />,
    },
    {
      title: '构建方式',
      dataIndex: 'buildMethod',
      width: 118,
      render: (v, r) => (
        <Tooltip title={r.buildMethod === 'jenkins' ? r.jenkinsJob : `${r.dockerfilePath}`}>
          <span>
            <MethodTag method={v} />
          </span>
        </Tooltip>
      ),
    },
    {
      title: '最近构建状态',
      dataIndex: 'lastBuild',
      width: 128,
      render: (b?: BuildRecord) => (b ? <BuildStatusTag status={b.status} /> : <Text type="secondary">无记录</Text>),
      sorter: (a, b) => (a.lastBuild?.startedAt ?? '') > (b.lastBuild?.startedAt ?? '') ? 1 : -1,
    },
    {
      title: '最近镜像版本',
      dataIndex: 'lastBuild',
      width: 320,
      render: (b?: BuildRecord) =>
        b?.image ? (
          <div>
            <ImageText value={b.image} width={300} />
            <div style={{ marginTop: 3 }}>
              <DigestText value={b.imageDigest} width={280} />
            </div>
          </div>
        ) : (
          /* 没有镜像时给"—"并说明原因，而不是渲染空串：
             空串在表格里会缩成一个只有省略号的怪格子，看的人分不清是
             "没有产物"还是"还没加载出来"。Jenkins 构建的版本号只能从它的
             日志里读出来，读不到就如实留空。 */
          <Tooltip
            title={
              b
                ? b.status === 'success'
                  ? '这次构建的日志里没有可识别的镜像推送记录（例如该 Job 只做编译、不推镜像），平台不猜版本号'
                  : '最近一次构建没有成功，因此没有产物镜像'
                : '该项目还没有构建记录'
            }
          >
            <Text type="secondary">—</Text>
          </Tooltip>
        ),
    },
    {
      title: '最近构建时间',
      dataIndex: 'lastBuild',
      width: 168,
      render: (b?: BuildRecord) => (
        <div>
          <div className="yn-mono" style={{ color: '#5c6470' }}>{b?.startedAt ?? '—'}</div>
          {b && b.duration ? (
            <div style={{ fontSize: 12, color: '#9aa0aa' }}>耗时 {humanDuration(b.duration)}</div>
          ) : null}
        </div>
      ),
    },
    {
      title: '成功率',
      dataIndex: 'successRate',
      width: 92,
      sorter: (a, b) => a.successRate - b.successRate,
      render: (v: number) => (
        <span style={{ color: v >= 95 ? '#52c41a' : v >= 88 ? '#faad14' : '#ff4d4f', fontWeight: 500 }}>{v}%</span>
      ),
    },
    {
      title: '状态',
      dataIndex: 'enabled',
      width: 84,
      filters: [
        { text: '已启用', value: 'enabled' },
        { text: '已停用', value: 'disabled' },
      ],
      onFilter: (v, r) => (v === 'enabled' ? r.enabled : !r.enabled),
      render: (v: boolean) => <Badge status={v ? 'success' : 'default'} text={v ? '已启用' : '已停用'} />,
    },
    {
      title: '操作',
      key: 'action',
      fixed: 'right',
      width: 236,
      render: (_, r) => (
        <Space size={2}>
          <Button
            type="link"
            size="small"
            icon={<ThunderboltOutlined />}
            disabled={!r.enabled}
            onClick={() => openBuild(r)}
            style={{ paddingInline: 4 }}
          >
            编译
          </Button>
          <Button
            type="link"
            size="small"
            icon={<HistoryOutlined />}
            onClick={() => navigate(`/ci/projects/${r.key}?tab=builds`)}
            style={{ paddingInline: 4 }}
          >
            构建历史
          </Button>
          <Tooltip title={canWrite ? '编辑' : '需要 project:write 权限'}>
            <Button
              type="text"
              size="small"
              icon={<EditOutlined />}
              disabled={!canWrite}
              onClick={() => openEdit(r)}
            />
          </Tooltip>
          <Tooltip title={canDelete ? '删除' : '需要 project:delete 权限'}>
            <Button
              type="text"
              size="small"
              danger
              icon={<DeleteOutlined />}
              disabled={!canDelete}
              onClick={() => removeProject(r)}
            />
          </Tooltip>
        </Space>
      ),
    },
  ];

  return (
    <div>
      <PageHeader
        breadcrumb={[{ title: 'CI 镜像构建' }, { title: '项目管理' }]}
        title="项目管理"
        description="维护需要在平台构建镜像的前后端项目，配置 Git 仓库、构建方式与镜像 Tag 规则"
        extra={
          <Space>
            <Tooltip title="导出当前筛选结果为 CSV（浏览器本地生成，不落服务端）">
              <Button icon={<ExportOutlined />} onClick={exportCsv} disabled={!dataSource.length}>
                导出列表
              </Button>
            </Tooltip>
            <Button icon={<ReloadOutlined />} loading={projectsApi.loading} onClick={() => projectsApi.refresh()} />
            <AutoRefreshButton
              value={autoRefreshMs}
              onChange={setAutoRefreshMs}
              updatedAt={projectsApi.updatedAt}
              errorText={projectsApi.silentError?.message}
            />
            <Tooltip title={hasPerm('build:trigger') ? '' : '需要 build:trigger 权限'}>
              <Button
                type="primary"
                ghost
                icon={<AppstoreOutlined />}
                disabled={!hasPerm('build:trigger')}
                onClick={() => {
                  setSelectedRowKeys([]);
                  setBatchOpen(true);
                }}
              >
                批量打包
              </Button>
            </Tooltip>
            <Tooltip title={canWrite ? '' : '需要 project:write 权限'}>
              {/* 只有一个入口：构建方式在表单里选（默认 Jenkins Job）。
                  以前分成「新建项目（Dockerfile / 自定义脚本）」与「新建 Jenkins Job 项目」
                  两个菜单项，而那一次选择在表单里是锁死的，选错只能退出来重进。 */}
              <Button type="primary" icon={<PlusOutlined />} disabled={!canWrite} onClick={() => openCreate()}>
                新建项目
              </Button>
            </Tooltip>
          </Space>
        }
      />

      <SectionCard bodyStyle={{ padding: 0 }}>
        <div className="yn-filter-bar">
          <Space size={12} wrap>
            <Input
              allowClear
              prefix={<SearchOutlined style={{ color: '#b5bac4' }} />}
              placeholder="搜索项目名称 / 标识 / 负责人"
              style={{ width: 260 }}
              value={keyword}
              onChange={(e) => setKeyword(e.target.value)}
            />
            <Segmented
              value={type}
              onChange={(v) => setType(v as string)}
              options={[
                { label: `全部 ${projects.length}`, value: 'all' },
                { label: `前端 ${projects.filter((p) => p.type === 'frontend').length}`, value: 'frontend' },
                { label: `后端 ${projects.filter((p) => p.type === 'backend').length}`, value: 'backend' },
              ]}
            />
            <Select
              allowClear
              placeholder="构建方式"
              style={{ width: 148 }}
              value={methodFilter}
              onChange={setMethodFilter}
              options={[
                { label: 'Dockerfile', value: 'dockerfile' },
                { label: 'Jenkins Job', value: 'jenkins' },
                { label: '自定义脚本', value: 'script' },
              ]}
            />
            <Select
              style={{ width: 128 }}
              value={enabledFilter}
              onChange={setEnabledFilter}
              options={[
                { label: '全部状态', value: 'all' },
                { label: '已启用', value: 'enabled' },
                { label: '已停用', value: 'disabled' },
              ]}
            />
          </Space>
          <Text type="secondary" style={{ fontSize: 12 }}>
            共 {dataSource.length} 个项目
            {selectedRowKeys.length ? ` · 已选 ${selectedRowKeys.length} 项` : ''}
          </Text>
        </div>

        {selectedRowKeys.length > 0 ? (
          <div className="yn-bulk-bar">
            <Space size={12}>
              <Text style={{ fontSize: 13 }}>已选择 {selectedRowKeys.length} 个项目</Text>
              <Button size="small" type="primary" ghost icon={<AppstoreOutlined />} onClick={() => setBatchOpen(true)}>
                批量编译（排队 + 实时日志）
              </Button>
              <Button
                size="small"
                loading={saving}
                disabled={!canWrite}
                onClick={() => void batchDisable()}
              >
                批量停用
              </Button>
              <Button size="small" onClick={() => setSelectedRowKeys([])}>取消选择</Button>
            </Space>
          </div>
        ) : null}

        <Table<Project>
          // rowKey 用项目标识而不是自增 id：勾选的这批 key 会直接带进批量打包向导，
          // 而向导是按 `project.key` 匹配的（后端也只认 key）。
          // 早先用 id 时，勾了项目进向导会"一个都没选中"——静默丢选择，很难查。
          rowKey="key"
          size="middle"
          dataSource={dataSource}
          columns={columns}
          scroll={{ x: 1560 }}
          rowSelection={{ selectedRowKeys, onChange: setSelectedRowKeys }}
          pagination={{
            // 20：项目从 6 个涨到 14 个之后，10 条/页已经装不下（新项目排在前面，
            // 老项目被挤到第 2 页，用户找 aioai 时以为它不见了 —— 实测踩过）。
            // 接口那边是**一次拉全量**（pageSize 200，见上面的注释），所以放大这里不增加请求。
            pageSize: 20,
            showSizeChanger: true,
            showTotal: (t) => `共 ${t} 条`,
            size: 'default',
          }}
        />
      </SectionCard>

      {/* -------------------------------------------------- 一键编译弹窗 */}
      <Modal
        open={!!buildTarget}
        title={
          <Space>
            <ThunderboltOutlined style={{ color: '#1677ff' }} />
            编译 {buildTarget?.name}
          </Space>
        }
        width={680}
        onCancel={() => setBuildTarget(null)}
        onOk={submitBuild}
        okText="提交构建"
        cancelText="取消"
        confirmLoading={submitting}
        // 禁用理由会同时在表单里用 Alert 写清楚（见下方 jenkinsBlockReason），
        // 不做"灰着但不说话"的按钮
        okButtonProps={{ disabled: !!jenkinsBlockReason }}
        destroyOnClose
      >
        <div style={{ background: '#f7faff', border: '1px solid #d6e8ff', borderRadius: 6, padding: '10px 14px', marginBottom: 16, fontSize: 13 }}>
          <Space split={<Divider type="vertical" />} wrap size={4}>
            <span><Text type="secondary">项目标识：</Text>{buildTarget?.key}</span>
            <span><Text type="secondary">构建方式：</Text>{buildTarget && <MethodTag method={buildTarget.buildMethod} />}</span>
            {/* 这一条是给 Jenkins 项目加的关键信息：分支与参数都来自这个 Job，
                不写出来用户没法确认"我这一下到底触发了谁" */}
            {isJenkinsBuild ? (
              <span>
                <Text type="secondary">Jenkins Job：</Text>
                <span className="yn-mono">{jenkinsJobName || '未绑定'}</span>
              </span>
            ) : null}
            <span><Text type="secondary">目标镜像：</Text><span className="yn-mono">{imageRepoOf(buildTarget?.registry, buildTarget?.imageName)}</span></span>
          </Space>
        </div>

        {jenkinsBlockReason ? (
          <Alert
            type="warning"
            showIcon
            style={{ marginBottom: 16 }}
            message="暂时不能提交"
            description={jenkinsBlockReason}
          />
        ) : null}

        <Form form={buildForm} layout="vertical" requiredMark="optional">
          <Row gutter={16}>
            {/* Jenkins 项目把这一行占满：它的镜像 Tag 由 Jenkins 侧的打包脚本决定
                （日志里那句 docker push 推什么版本，平台就记什么版本），
                平台没有"按规则生成"的能力，留一个框在那儿只会让人以为填了有用。 */}
            <Col span={isJenkinsBuild ? 24 : 12}>
              {isJenkinsBuild ? (
                <Form.Item
                  label="构建分支"
                  tooltip={
                    jk.gitDefs[0]
                      ? `来自 Jenkins Job 的构建参数 ${jk.gitDefs[0].name}；不选则使用 Jenkins 侧默认值`
                      : '该 Job 没有可选的版本 / 分支参数'
                  }
                >
                  {/* Jenkins 项目的分支必须是 Job 的真实可选值：
                      手填一个 Jenkins 上不存在的引用，构建会在检出阶段失败，
                      而那种失败要到日志里才看得出来。 */}
                  <JenkinsGitParamSelect
                    param={jk.gitDefs[0] ?? null}
                    emptyPlaceholder={
                      jk.loading
                        ? '正在读取 Jenkins Job 的构建参数…'
                        : jk.error
                          ? '构建参数读取失败'
                          : '该 Job 没有可选的版本 / 分支参数'
                    }
                    values={jk.gitDefs[0] ? jk.values[jk.gitDefs[0].name] : undefined}
                    loading={jk.gitDefs[0] ? !!jk.loadingValues[jk.gitDefs[0].name] : jk.loading}
                    onReload={() => {
                      const d = jk.gitDefs[0];
                      if (d) jk.reloadValues(d);
                      else jk.reload();
                    }}
                    value={jk.gitDefs[0] ? jkSelected[jk.gitDefs[0].name] : undefined}
                    onChange={(v) => {
                      const d = jk.gitDefs[0];
                      if (!d) return;
                      setJkSelected((s) => ({ ...s, [d.name]: v }));
                    }}
                  />
                </Form.Item>
              ) : (
                <Form.Item
                  name="branch"
                  label="构建分支"
                  rules={[{ required: true, message: '请选择或输入分支' }]}
                  tooltip="候选来自项目仓库的真实分支；也可以直接输入分支名，例如 release/1.9"
                  extra={branchExtra}
                  validateStatus={branchError ? 'warning' : undefined}
                >
                  {/* AutoComplete 而不是 Select：Select 不带 tags 模式时**只能从选项里挑**，
                      "支持直接输入分支名"会变成一句做不到的承诺（新分支永远选不到）。 */}
                  <AutoComplete
                    placeholder="选择或输入分支"
                    options={branchCandidates.map((b) => ({ label: b, value: b }))}
                    notFoundContent={
                      branchLoading ? <Spin size="small" /> : '没有匹配的分支，可直接输入'
                    }
                  />
                </Form.Item>
              )}
            </Col>
            {isJenkinsBuild ? null : (
              <Col span={12}>
                <Form.Item
                  name="tagOverride"
                  label="镜像 Tag（留空按规则生成）"
                  tooltip={`规则：${buildTarget?.tagPattern ?? ''}`}
                >
                  <Input placeholder="如 v1.9.0-rc1" allowClear />
                </Form.Item>
              </Col>
            )}
          </Row>

          {isJenkinsBuild ? (
            /* Jenkins 项目不能给平台那套 Key-Value：那些键是发给平台自建构建的环境变量，
               Jenkins 根本不认识，填了只会静默无效。这里改成显示 Job 自身定义的参数，
               能填什么、会传什么，一目了然。 */
            <Form.Item
              label="构建参数"
              tooltip="参数来自该 Jenkins Job 自身定义；平台不猜默认值，留空即不传该参数"
            >
              {jk.loading ? (
                <Text type="secondary" style={{ fontSize: 12 }}>
                  正在读取该 Job 的构建参数…
                </Text>
              ) : jk.error ? (
                <Alert
                  type="error"
                  showIcon
                  message="读取构建参数失败"
                  description={jk.error}
                  action={
                    <Button size="small" onClick={jk.reload}>
                      重试
                    </Button>
                  }
                />
              ) : jk.otherDefs.length ? (
                <JenkinsParamFields
                  defs={jk.otherDefs}
                  value={jkSelected}
                  onChange={(name, v) => setJkSelected((s) => ({ ...s, [name]: v }))}
                />
              ) : (
                <Text type="secondary" style={{ fontSize: 12 }}>
                  该 Job 除版本 / 分支外没有其他参数。
                </Text>
              )}

              {/* 提交摘要：把"到底要发什么"摊开。留空的参数明确写成"不传"，
                  而不是让人以为选了 Jenkins 的默认值。 */}
              {!jk.loading && !jk.error ? (
                <div style={{ marginTop: 2, fontSize: 12 }}>
                  {jenkinsWillSubmit.length ? (
                    <>
                      <Text type="secondary">将提交 {jenkinsWillSubmit.length} 个参数：</Text>
                      <Text className="yn-mono" style={{ fontSize: 12 }}>
                        {jenkinsWillSubmit.map((p) => `${p.name} = ${p.value}`).join('，')}
                      </Text>
                    </>
                  ) : (
                    <Text type="secondary">
                      未指定任何参数：按 Jenkins 侧配置构建（不传参数 = 用 Jenkins 自己的默认值）
                    </Text>
                  )}
                </div>
              ) : null}
            </Form.Item>
          ) : (
            <Form.Item
              name="buildParams"
              label="构建参数（本次构建覆盖默认值）"
              tooltip="Key-Value 形式注入构建环境，例如 npm registry、Maven settings"
            >
              <KVFields name="buildParams" addText="添加构建参数" />
            </Form.Item>
          )}

          <Form.Item name="notify" label="构建完成后通知" valuePropName="checked">
            <Switch checkedChildren="开" unCheckedChildren="关" />
          </Form.Item>
        </Form>

        {runningHistory.length ? (
          <>
            <Divider style={{ margin: '4px 0 12px' }} orientation="left" plain>
              最近构建历史
            </Divider>
            <div>
              {runningHistory.map((h) => (
                <div key={h.id} style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', padding: '6px 0', fontSize: 13 }}>
                  <Space size={10}>
                    <span className="yn-mono" style={{ color: '#5c6470' }}>#{h.buildNo}</span>
                    <Tag style={{ marginInlineEnd: 0 }}>{h.branch}</Tag>
                    <span className="yn-mono" style={{ color: '#9aa0aa', fontSize: 12 }}>{h.commitId}</span>
                  </Space>
                  <Space size={12}>
                    <Text type="secondary" style={{ fontSize: 12 }}>{h.startedAt}</Text>
                    <BuildStatusTag status={h.status as BuildStatus} />
                  </Space>
                </div>
              ))}
            </div>
          </>
        ) : null}
      </Modal>

      {/* -------------------------------------------------- 新建 / 编辑抽屉 */}
      <Drawer
        open={drawerOpen}
        // 880 而不是 720：一页式表单把「标签在左」的列也放进来了，
        // 720 下控件只剩两百来像素，脚本编辑器和长仓库地址都看不全。
        width={880}
        title={editTarget ? `编辑项目 · ${editTarget.name}` : '新建项目'}
        onClose={() => setDrawerOpen(false)}
        extra={
          <Space>
            <Button onClick={() => setDrawerOpen(false)}>取消</Button>
            <Button type="primary" loading={saving} onClick={submitProject}>
              保存
            </Button>
          </Space>
        }
        destroyOnClose
      >
        <Form
          form={form}
          // 「标签在左」的横向布局：一页到底、每行控件旁紧跟一句说明。
          // 合并成一张表单后版式必须统一（以前 Jenkins 那套是纵向的），
          // 否则同一个抽屉在切换构建方式时会整体跳一下。
          layout="horizontal"
          labelCol={{ flex: `${FORM_LABEL_WIDTH}px` }}
          wrapperCol={{ flex: '1' }}
          requiredMark="optional"
        >
          {projectForm}
        </Form>
      </Drawer>

      {/* -------------------------------------------------- 批量打包向导 */}
      <BatchBuildWizard
        open={batchOpen}
        onClose={() => setBatchOpen(false)}
        presetKeys={selectedRowKeys as string[]}
        onCompleted={() => setSelectedRowKeys([])}
      />

    </div>
  );
};

export default ProjectList;
