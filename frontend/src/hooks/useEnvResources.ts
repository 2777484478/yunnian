import { environmentApi } from '@/api';
import type { ApiError } from '@/api';
import type { EnvResources } from '@/api/cd';
import { useApi } from '@/hooks/useApi';
import type { Environment } from '@/types';

// ---------------------------------------------------------------------------
// useEnvResources —— 一批环境的**集群实时资源**（GET /environments/{key}/resources）
//
// 为什么必须有这一个共用入口：环境快照（`environment.cpu_usage / mem_usage /
// health_status`）只在「同步集群」时写一次，之后一动不动。界面直接拿它当现状展示，
// 就会出现「卡片说内存 96%、点进去实时是 93%」这种对不上的观感 ——
// 两个数**都没算错**，差的只是时间点，但用户只会得出"有一个是假的"。
//
// 更难发现的一例：同一个响应里徽标按快照判"资源紧张"、旁边的数字却是实时值，
// 卡片自己跟自己矛盾。
//
// 所以**凡是展示环境资源的地方都走这里**：阈值、降级口径、状态推导只有一份实现。
// 已有前车之鉴 —— `utils/live.ts` 的注释里写着"三处各写一套判断，迟早会出现
// 圆圈说运行中、表格说运行异常"，这次是同一件事的另一半。
// ---------------------------------------------------------------------------

/** 用量翻成状态词的阈值，与后端 `cd.go` 的 `SyncEnvironment` **逐字一致**。 */
export const ENV_ERROR_AT = 90;
export const ENV_WARNING_AT = 75;

/**
 * 把一次实时结果规范化成"**真的读到了用量**"的形态；否则一律 null。
 *
 * **不给"退回快照"的退路**：那样就会造出"数字是实时的、徽标是几小时前的"这种
 * 自相矛盾的卡片，而两者本来必须同源。读不到就如实说读不到。
 */
export const envUsage = (r: EnvResources | null | undefined): EnvResources | null =>
  // `metricsAvailable === false` 时那两个 0 是"没有值"，不是"用量为 0"：
  // 集群没装 metrics-server、或当前凭据无权读它，都会落到这里。
  r && r.metricsAvailable !== false ? r : null;

/** 批量版：按环境标识取。 */
export const envUsageOf = (
  map: Map<string, EnvResources | null> | null,
  envKey: string,
): EnvResources | null => envUsage(map?.get(envKey));

/**
 * 一次实时结果 → 环境资源状态。
 *
 * 「环境详情」手里已经有自己那一份（`/environments/{key}/resources` 的单环境结果），
 * 用它即可，不必再套一层 Map。
 */
export const envResStatus = (r: EnvResources | null | undefined): Environment['status'] => {
  const u = envUsage(r);
  if (!u) return 'unknown';
  if (u.cpuUsage >= ENV_ERROR_AT || u.memUsage >= ENV_ERROR_AT) return 'error';
  if (u.cpuUsage >= ENV_WARNING_AT || u.memUsage >= ENV_WARNING_AT) return 'warning';
  return 'healthy';
};

/** 批量版：按环境标识取状态。 */
export const envResStatusOf = (
  map: Map<string, EnvResources | null> | null,
  envKey: string,
): Environment['status'] => envResStatus(map?.get(envKey));

export interface EnvResourcesResult {
  /** 环境标识 → 实时结果；**读不到的是 null**（不是 0，也不是快照值） */
  map: Map<string, EnvResources | null> | null;
  /** 环境标识 → 读不到的原因（"集群读取超时（8 秒）"）。读到了就没有这一项。 */
  errors: Map<string, string> | null;
  loading: boolean;
  error: ApiError | null;
  refresh: () => void;
}

/**
 * 批量取环境实时资源。
 *
 * 只在进页面 / 显式 `refresh()` 时打一次，**不挂自动刷新**：这是列表与汇总视图，
 * 不该按秒去读每个集群（「环境详情」那种单环境视图才配 5 秒轮询）。
 *
 * 单个环境读不到不影响其余环境（各自 try/catch）：一个集群不可达不该把整页打成错误态。
 */
export function useEnvResources(envKeys: string[]): EnvResourcesResult {
  // ⚠️ 依赖传的是**字符串**而不是 `envKeys` 数组本身：调用方每次渲染都会造一个
  // 新数组（`envs.map(e => e.key)`），直接当依赖会让 useApi 每渲染打一次请求。
  // 排序 + 去重后 join，值稳定，环境增减时才真的变化。
  const key = [...new Set(envKeys.filter(Boolean))].sort().join(',');

  const api = useApi(
    async () => {
      const keys = key ? key.split(',') : [];
      if (!keys.length) {
        return { map: new Map<string, EnvResources | null>(), errs: new Map<string, string>() };
      }
      // 一次请求拿全部：**不要**回到 Promise.all(keys.map(...))。
      // 那样每个环境占一个浏览器连接，5 个环境就把同域 6 个并发连接吃干，
      // 页面上 /stats、/projects 这些本该 50ms 的请求只能排队等集群超时。
      // 服务端已经改成并发查 + 单环境 8 秒超时（cd.go EnvResourcesBatch）。
      const res = await environmentApi.resourcesBatch(keys);
      const map = new Map<string, EnvResources | null>();
      const errs = new Map<string, string>();
      for (const k of keys) map.set(k, null);
      for (const it of res.items ?? []) {
        map.set(it.envKey, it.ok ? (it.data ?? null) : null);
        if (!it.ok && it.error) errs.set(it.envKey, it.error);
      }
      return { map, errs };
    },
    [key],
    { enabled: !!key },
  );

  return {
    map: api.data?.map ?? null,
    // 每个环境各自的失败原因（"集群超时（8 秒）"这类），给卡片显示用。
    // 整体 error 只覆盖"这一个请求本身失败"，单个环境读不到走 errors。
    errors: api.data?.errs ?? null,
    loading: api.loading,
    error: api.error,
    refresh: api.refresh,
  };
}
