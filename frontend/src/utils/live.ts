import type { EnvService, ServiceLive, ServiceStatus } from '@/types';

/**
 * 「集群实时状态」的统一取用口径。
 *
 * 为什么要单独一个模块：拓扑圆圈、环境详情的服务表格、部署向导都在展示同一批值，
 * 三处各写一套判断，迟早会出现"圆圈说运行中、表格说运行异常"这种自相矛盾的界面。
 *
 * 贯穿本次改造的一条硬规则：**平台库里的 replicas / status / image 不是真相**。
 * 它们是纳管时按默认值写入的"平台期望"，之后只有部署成功才更新，
 * 直接显示就会得到"平台说 2/2、集群里其实只有 1/1"这类假数据。
 * 所以这里只认 `live`，读不到就如实说读不到，**不回退到库里的值**。
 */

/** 取服务在集群里的实时状态；读不到、或集群里不存在时返回 null。 */
export const liveState = (s: EnvService): ServiceLive | null =>
  s.live && s.live.available && s.live.exists ? s.live : null;

/**
 * 值显示不出来时的**统一说明**。
 *
 * 三种情形必须分开说（后端已经分开返回，前端不许再合并成一句"暂无数据"）：
 * - 没有 live → 该服务未纳管 / 接口没回；
 * - `available=false` → **没读到**集群，message 是原因（未接入 / 无权 / 网络）；
 * - `available=true && !exists` → **确认**集群里没有这个工作负载。
 *
 * 后两者的排查方向完全相反：一个是"去查平台到集群的连通与权限"，
 * 一个是"这个项目在这个环境确实还没部署过"。混成一句会让人往错的方向找。
 */
export const liveFallbackText = (s: EnvService): string => {
  const live = s.live;
  if (!live) return '未取得集群状态';
  if (!live.available) return live.message || '读取不到集群状态';
  if (!live.exists) return '集群中不存在';
  return '—';
};

/**
 * 后端集群状态词 → 前端服务状态词。
 *
 * 认不出的词一律返回 'unknown'，**不猜**：猜错会让一个异常的服务看起来正常。
 */
export const liveStatusWord = (s: string): ServiceStatus | 'unknown' => {
  switch (s) {
    case 'running':
      return 'running';
    case 'progressing':
      return 'deploying';
    case 'failed':
      return 'error';
    case 'stopped':
      return 'stopped';
    default:
      return 'unknown';
  }
};

/**
 * 服务在**集群里**的状态词；读不到一律 `'unknown'`。
 *
 * `'unknown'` 是**独立的一档**，既不算正常、也不算异常 —— 混进任何一边都是在拿
 * 没读到的值下结论：算进"正常"会盖住真故障，算进"异常"会凭空报一个假警报。
 * 所以统计口径一律用下面的 `isServiceAbnormal`，别自己写 `s.status !== 'running'`。
 *
 * ⚠️ **绝不能拿 `EnvService.status` 顶替**：那是平台库里的值，纳管时按默认值写入、
 * 之后只有部署成功才更新。实测某服务库里 `status=pending`、`image` 还是空串，
 * 集群里其实 running 1/1 —— 用它统计就会对着一个健康服务报"待关注"。
 */
export const liveWordOf = (s: EnvService): ServiceStatus | 'unknown' => {
  const lv = liveState(s);
  return lv ? liveStatusWord(lv.status) : 'unknown';
};

/**
 * 该服务是否**需要关注**：集群里确实存在、且状态不是 running。
 *
 * 读不到（`unknown`）**不算**需要关注 —— 那是"没读到"，不是"有问题"。
 * 全部展示口径（环境卡片、环境详情「待处理」、概览页汇总）都走这一个函数，
 * 否则同一个服务在三个页面会有三种数法。
 */
export const isServiceAbnormal = (s: EnvService): boolean => {
  const w = liveWordOf(s);
  return w !== 'running' && w !== 'unknown';
};

/**
 * 副本的显示口径：分子是就绪数，分母是**实例总数**。
 *
 * ⚠️ 分母**不能**只取 `replicas`（`spec.replicas`，平台期望数）：滚动更新卡住时
 * 新 Pod 反复崩溃、旧 Pod 不被杀（1 副本时 `maxUnavailable` 向下取整为 0），
 * 于是集群里实际有 2 个实例而期望仍是 1 个 —— 只按期望渲染就写成 `1/1`，
 * 把"有一个实例正在崩"整个抹掉。实测 demobasic `spec=1 / status=2 / ready=1`，
 * 用户在自己的运维面板里看到的是 `1/2`，而平台显示 `1/1`。
 *
 * 取两者较大值：期望高（刚扩容、还在拉起）与实际高（有残留实例）两种情况都不漏。
 * ⚠️ 老后端不下发 `actualReplicas` 时退化为 `replicas`，与旧行为一致
 * （直接用 `Math.max(replicas, undefined)` 会得到 `NaN`，那会渲染成 `1/NaN`）。
 */
export const liveReplicaTotal = (lv: ServiceLive): number =>
  Math.max(lv.replicas ?? 0, lv.actualReplicas ?? 0);

/**
 * 集群状态是否"确实健康"。
 *
 * ⚠️ **不能只看 `readyReplicas === replicas`**：滚动更新时新 Pod 起不来，
 * 老 Pod 会把就绪数撑满，于是这个比值算出来"健康"，而服务其实正在发布失败 ——
 * 实测 demobasic：`readyReplicas=1 / replicas=1` 渲染成绿色 `1/1`，
 * 同时新 Pod 已 CrashLoopBackOff 崩了 9 次、集群给的 `status` 明明白白是 `failed`。
 * 只看比值，界面就会主动报一个"一切正常"。
 *
 * 所以两件事都要满足：整体状态是 `running`，且**每个实例**都就绪
 * （分母用 `liveReplicaTotal`，与「副本」列显示的数字同一口径 ——
 * 否则会出现"数字写着 1/2、颜色却是绿的"这种自相矛盾的界面）。
 */
export const liveHealthy = (lv: ServiceLive): boolean =>
  liveStatusWord(lv.status) === 'running' && lv.readyReplicas >= liveReplicaTotal(lv);
