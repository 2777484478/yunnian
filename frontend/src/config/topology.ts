// ---------------------------------------------------------------------------
// 发布拓扑的展示元数据（**界面知识，不是业务数据**）
//
// 这个文件在 2026-09-29 有一次性质变化，值得写清楚——因为它以前事实上是"假数据的仓库"：
//
//   1. ~~CLUSTER_META~~ —— 集群的云厂商 / 区域 / 可用区，**已整块删除**。
//      它原本是前端写死的三条，与后端 configs/config.yaml 的两条、与库里的值
//      三边不一致（种子里 canary 是 k8s-prod-01、prod 是 k8s-prod-01（双可用区），
//      staging 那个集群 config.yaml 里压根没有）。
//      而且 **kubeconfig 本身也给不出云厂商与区域** —— 它只有 server / CA / context 名。
//      现在集群信息一律来自「系统管理 › 集群接入」（`GET /clusters`），
//      本文件不再持有任何集群数据，页面也不再自己编造云厂商。
//
//   2. ENV_STAGE —— 环境在拓扑里的横向顺序，降级为**回退默认值**。
//      阶段已改为环境自带字段（`environment.stageOrder` / `stageLabel`），
//      这样用户新建的「华东压测区」也能排进链路，而不是被扔到最末。
//      下面这五个 key 只是后端补列之前、让现有环境仍能正确排列的兜底。
//
//   3. gate（流转门禁）—— **已删除**。原先写死的「自动晋级 / 手动进入」是无源承诺：
//      后端没有任何"晋级门禁"概念，平台上的部署永远是人工发起的。
//      现改为由环境真实的 needApproval 推导（见 gateOf）：
//      开了审批显示「需审批」，否则显示「直接发布」——两句话都是后端真的会做的事。
// ---------------------------------------------------------------------------

import type { Environment } from '@/types';

// --------------------------------------------------------------------------- 集群标识归一化

/**
 * 去掉集群标识里的修饰后缀：`k8s-prod-01（双可用区）` → `k8s-prod-01`。
 *
 * 这是**给旧数据擦屁股**的：集群实体化之前，environment.cluster 是自由文本，
 * 同一个集群被存成过两个名字，前端只能靠裁括号把它们对上。
 * 新数据（从集群接入页选的）不会再出现后缀，这个函数留着是为了让存量环境仍能匹配到集群。
 */
export const clusterKeyOf = (cluster: string): string => cluster.replace(/[（(].*$/, '').trim();

// --------------------------------------------------------------------------- 集群归属

/**
 * 该环境的集群凭据是不是**自带的**（`clusterSource=own`）。
 *
 * 两个字段都认：`clusterSource` 是新建/编辑时提交的显式选择，`hasKubeconfig` 是后端回的
 * "确实存了一份凭据"。只看其一都会漏掉存量数据。
 *
 * 2026-10-10 从 `Topology.tsx` 提到这里：环境列表、环境详情、发布拓扑三处都要用，
 * 各写一份必然漂移。
 */
export const isOwnCluster = (env: Pick<Environment, 'clusterSource' | 'hasKubeconfig'>): boolean =>
  env.clusterSource === 'own' || !!env.hasKubeconfig;

/** 自带凭据时给集群标识挂的角标 */
export const OWN_CLUSTER_BADGE = '自带凭据';

/**
 * 自带凭据的环境，`env.cluster` 只是个**名字标签**、不是平台集群表里那条记录。
 *
 * 后端 `clusterRefOf`（`internal/service/cd.go`）在这种情况下**绕过**平台集群表、
 * 直接用环境自己那份 kubeconfig；注册表里同名的记录**是另一台机器**。
 * 实测（2026-10-10）：环境 `test-152` 自带凭据、实际集群 v1.23.10，
 * 而引用平台集群的环境 `test` 同名 `cluster-local`、版本 v1.22.12 ——
 * **两台不同的机器，在列表里都叫 cluster-local**。
 *
 * 根因是公司里 kubeadm 统一安装的集群，kubeconfig 里 cluster 名一律是 `cluster.local`，
 * 平台派生出的标识必然撞。所以这里的角标不是装饰，是让人别把两者当成同一台。
 */
export const OWN_CLUSTER_HINT =
  '该环境自带 kubeconfig：平台会绕过集群注册表、直接用这份凭据读集群。' +
  '因此集群标识此时只是标签 —— 注册表里同名的记录是另一台机器，版本 / 节点数不要互相参照。';

// --------------------------------------------------------------------------- 发布阶段

export interface StageMeta {
  /** 横向顺序，越小越靠左（越接近研发） */
  order: number;
  /** 阶段语义：自测联调 / 预发回归 / 生产发布 */
  stage: string;
  color: string;
}

/** stageOf / sortByStage 需要的最小环境形状（只读展示用，不依赖完整 Environment） */
export interface StageEnvLike {
  key: string;
  /** 环境自带的阶段序号（后端补列前恒为 undefined） */
  stageOrder?: number;
  stageLabel?: string;
  /** 用于推导链路门禁文案 */
  needApproval?: boolean;
}

/**
 * 后端 environment 表补上阶段字段之前，按 env key 的兜底排列。
 * 注意它只是**排得更整齐**，不代表平台认识这些名字——`perf` 在同 order 时
 * 靠稳定排序保持原相对顺序。
 */
export const ENV_STAGE: Record<string, StageMeta> = {
  test: { order: 1, stage: '自测联调', color: '#13c2c2' },
  perf: { order: 2, stage: '性能压测', color: '#722ed1' },
  staging: { order: 3, stage: '预发回归', color: '#1677ff' },
  canary: { order: 4, stage: '灰度放量', color: '#fa8c16' },
  prod: { order: 5, stage: '生产发布', color: '#f5222d' },
};

const CUSTOM_STAGE: StageMeta = { order: 90, stage: '自定义环境', color: '#8c8c8c' };

/**
 * 取环境的发布阶段：**优先用环境自带字段**，没有才回退到按 key 的默认映射。
 *
 * 为什么要这个顺序：用户可以把环境命名成任何样子（`perf-sh`、`uat`、`hotfix`），
 * 靠 key 匹配的阶段表天生覆盖不到；而后端补上 stageOrder 之后，
 * 「我把它放在链路第几位」就是用户自己的决定，不该被前端的映射表覆盖掉。
 */
export const stageOf = (env: StageEnvLike): StageMeta => {
  const def = ENV_STAGE[env.key] ?? CUSTOM_STAGE;
  const hasOrder = typeof env.stageOrder === 'number';
  const hasLabel = !!env.stageLabel;
  if (!hasOrder && !hasLabel) return def;
  return {
    order: hasOrder ? (env.stageOrder as number) : def.order,
    stage: hasLabel ? (env.stageLabel as string) : def.stage,
    color: def.color,
  };
};

/** 按发布阶段排序（稳定排序，同 order 保持原相对顺序） */
export const sortByStage = <T extends StageEnvLike>(list: readonly T[]): T[] =>
  [...list]
    .map((item, i) => ({ item, i }))
    .sort((a, b) => stageOf(a.item).order - stageOf(b.item).order || a.i - b.i)
    .map((x) => x.item);

/**
 * 链路门禁文案 —— 由环境真实的 needApproval 推导，不再是写死的"自动晋级"。
 *
 * 后端只有"这个环境的部署要不要审批"这一个真实约束（`environment.need_approval`
 * 决定建单后进 pending_approval 还是直接进队列）。所以链路标签只说这两件事之一，
 * 不承诺"上一环境成功就自动晋级"——平台没有这种机制。
 */
export const gateOf = (env: { needApproval?: boolean }): string =>
  env.needApproval ? '需审批' : '直接发布';

// --------------------------------------------------------------------------- 版本展示

export interface VersionView {
  /** 圆圈主版本行。**是真实 tag 的一段**，不是派生的语义版本号 */
  version: string;
  /** 圆圈构建号行。tag 里认不出构建号时为空串（界面不渲染这一行） */
  build: string;
}

const EMPTY_VERSION: VersionView = { version: '', build: '' };

/**
 * 把镜像 tag 拆成圆圈上的两行。**展示的每一个字符都取自真实 tag**，不做任何派生。
 *
 * 平台项目的镜像 tag 惯例是 `<major>.<minor>.<patch>.<build>`，例如集群实况里的
 * 1.2.0.2577、1.2.0.1100、1.2.0.1690 —— 所以：
 *
 *   主行（版本）= 去掉末尾构建号的部分，如 1.2.0
 *   副行（构建号）= 末尾那一段数字，如 2577
 *
 * 两行拼起来就是原 tag，用户核对时不会凭空多出一个数字。
 *
 * ⚠️ 这里**曾经**是哈希派生：把 tag 喂进哈希，吐出 `v1.9.23` 这种看着像版本号的假数字。
 * 它"同一 tag 稳定、不同 tag 不同"，所以经得起"稳不稳定"的检验，
 * 却经不起"是不是真的"的检验 —— 一旦有人拿它比较两个环境的版本先后，
 * 得到的会是一个编出来的结论。圆圈上的版本号就是会被这么用的，所以必须是真的。
 *
 * 不符合上面形状的 tag（latest、10、2.1、master-abc1234、
 * release-1.9-4f3a1b2-202609011530）**整串放主行、副行留空**：
 * 硬拆只会造出一个不存在的版本号。
 */
export function versionViewOf(tag?: string): VersionView {
  const t = (tag ?? '').trim();
  if (!t) return EMPTY_VERSION;

  const m = /^(.+)\.(\d+)$/.exec(t);
  // 只认"末尾数字前面还有三段及以上"的形状：`1.2.0` 的末段 0 是 patch 不是构建号，
  // 拆了就会显示成"版本 1.2 / 构建号 0"，反倒制造出一个错的版本。
  if (m && m[1].split('.').length >= 3) {
    return { version: m[1], build: m[2] };
  }
  return { version: t, build: '' };
}

// --------------------------------------------------------------------------- 集群状态文案

/**
 * 拓扑上给集群标的角标文案（原本这里是"云厂商缩写"）。
 *
 * 云厂商没有数据来源（kubeconfig 里没有这个信息），所以角标改说**平台真正知道的事**：
 * 这个集群接进来了没有、通不通。未接入的集群不再显示一个凭空猜的厂商名。
 */
export const clusterBadgeOf = (
  status?: 'connected' | 'unreachable' | 'unauthorized' | 'unprobed',
): string => {
  switch (status) {
    case 'connected':
      return '已接入';
    case 'unreachable':
    case 'unauthorized':
      return '异常';
    case 'unprobed':
      return '未探测';
    default:
      // 环境里填的集群标识在平台没有对应记录：这正是集群实体化之前的老数据的常态
      return '未接入';
  }
};
