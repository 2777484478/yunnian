// ---------------------------------------------------------------------------
// 展示格式化工具
//
// 从 mock/_utils.ts 抽出来的纯函数。原因：这些是**展示逻辑**，不是假数据，
// 接上真实接口后仍然需要（后端下发 ISO 时间串，页面要显示"3 分钟前"）。
// 继续从 @/mock 里 import 工具函数会让"删掉 mock"变成不可能。
// ---------------------------------------------------------------------------

/** 时间输入：后端下发的是 ISO 字符串（如 `2026-09-23T14:19:14+08:00`） */
export type TimeInput = string | number | Date;

const toDate = (input: TimeInput): Date => (input instanceof Date ? input : new Date(input));

export const pad2 = (n: number) => String(n).padStart(2, '0');

/** 格式化为 `2026-09-23 14:19:14` */
export function fmt(input: TimeInput, withSeconds = true): string {
  const d = toDate(input);
  if (Number.isNaN(d.getTime())) return '-';
  const base = `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(
    d.getHours(),
  )}:${pad2(d.getMinutes())}`;
  return withSeconds ? `${base}:${pad2(d.getSeconds())}` : base;
}

/**
 * 相对时间：刚刚 / 3 分钟前 / 2 小时前 / 3 天前。
 *
 * 注意这里必须用**真实的当前时间**做基准。原型阶段的 mock 版本锚在一个写死的
 * NOW 常量上（为了让假数据稳定），接到真实数据后如果沿用，所有"x 分钟前"
 * 都会以那个历史时刻为参照，越用越离谱。
 */
export function ago(input: TimeInput): string {
  const d = toDate(input);
  if (Number.isNaN(d.getTime())) return '-';
  const diff = Date.now() - d.getTime();
  const m = Math.floor(diff / 60_000);
  if (m < 0) return fmt(d); // 未来时间（时钟漂移）直接显示绝对时间，别显示"-3 分钟前"
  if (m < 1) return '刚刚';
  if (m < 60) return `${m} 分钟前`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h} 小时前`;
  const days = Math.floor(h / 24);
  return days < 30 ? `${days} 天前` : fmt(d, false);
}

/** 耗时秒 → `1m32s` / `42s` */
export function humanDuration(sec: number | null | undefined): string {
  if (sec === null || sec === undefined || !Number.isFinite(sec) || sec < 0) return '-';
  if (sec < 60) return `${Math.round(sec)}s`;
  const m = Math.floor(sec / 60);
  const s = Math.round(sec % 60);
  return s ? `${m}m${s}s` : `${m}m`;
}

/** 字节 → 人类可读（镜像体积展示用） */
export function humanSize(bytes: number | null | undefined): string {
  if (bytes === null || bytes === undefined || !Number.isFinite(bytes) || bytes < 0) return '-';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let v = bytes;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)}${units[i]}`;
}

/** 由镜像完整地址推导 tag：`harbor.x/a/b:master-abc-20260923` → `master-abc-20260923` */
export function tagOf(image: string): string {
  const i = image.lastIndexOf(':');
  return i >= 0 ? image.slice(i + 1) : image;
}

/** 取值失败时的占位符，避免页面出现 undefined / null 字样 */
export const DASH = '-';
