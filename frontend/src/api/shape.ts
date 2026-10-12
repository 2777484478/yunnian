// ---------------------------------------------------------------------------
// 响应形状归一化
//
// 后端的列表接口存在三种形态，这是历史契约不是笔误，实测确认如下：
//   1. 标准分页  {list,total,page,pageSize}  —— /projects、/builds、/deployments、/audit-logs…
//   2. 裸列表    {list:[...]}                —— /environments/{k}/services、/services/{id}/pods、
//                                               /notify-channels、/models、/system/config…
//   3. 纯数组    [...]                       —— /roles、/permissions、/environments(部分)
//
// 页面不应该为这个差异写判断——那是把后端的实现细节泄漏到 UI 层。
// 这里统一收敛：需要分页的用 unwrapPage，只关心条目的用 unwrapList。
// 若后端将来统一了契约，只改本文件。
// ---------------------------------------------------------------------------

import type { PageResult } from '@/types';

/** 后端分页信封 */
export interface RawPage<T> {
  list: T[] | null;
  total: number;
  page: number;
  pageSize: number;
}

/** 后端「只有 list」的信封 */
export interface RawList<T> {
  list: T[] | null;
}

/** 统一成分页结果；缺 total 时以当前页条数兜底，避免页面出现 undefined */
export function unwrapPage<T>(data: RawPage<T> | T[] | null | undefined): PageResult<T> {
  if (Array.isArray(data)) {
    return { list: data, total: data.length, page: 1, pageSize: data.length };
  }
  const list = data?.list ?? [];
  return {
    list,
    total: data?.total ?? list.length,
    page: data?.page ?? 1,
    pageSize: data?.pageSize ?? list.length,
  };
}

/** 只取条目列表；三种形态都能吃 */
export function unwrapList<T>(data: RawList<T> | T[] | null | undefined): T[] {
  if (Array.isArray(data)) return data;
  return data?.list ?? [];
}

/**
 * 命名参数构造：把 undefined/''/空数组剔掉，避免拼出 `?envKey=&status=` 这类噪声。
 *
 * 参数类型用 `object` 而不是 `Record<string, unknown>`：后者要求调用方类型带索引签名，
 * 而普通的 interface（如 EnvQuery）在 TS 里是不满足索引签名的，会逼着每个查询类型
 * 都去写 `[key: string]: unknown`——那是把工具函数的约束泄漏给业务类型。
 */
export function params<T extends object>(obj: T): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(obj)) {
    if (v === undefined || v === null || v === '') continue;
    if (Array.isArray(v) && v.length === 0) continue;
    out[k] = v;
  }
  return out;
}
