import { http } from './client';

// ---------------------------------------------------------------------------
// 镜像仓库（Harbor）只读接口
//
// 用途只有一个：部署向导里「仓库标签」那一组的候选列表。
//
// 与「平台构建」（/images）的分工：
//   /images              平台自己构建过的镜像记录（带 commit / 构建号 / 推送时间）
//   /registries/harbor   仓库里**实际存在**的标签（含别的平台推的）
//
// 两者**并列、二选一**，不是替换。
// ---------------------------------------------------------------------------

/** 仓库里的一个标签。 */
export interface RegistryTag {
  tag: string;
  /** RFC3339；仓库给不出时为 ''。 */
  time: string;
  /**
   * time 是什么时间，**界面必须按它措辞**：
   *   'push'  推送时间（Harbor v2）
   *   'build' 镜像构建时间（Harbor v1 只能给到这个）
   *   ''      拿不到时间
   */
  timeKind: 'push' | 'build' | '';
}

/** 一次「仓库标签」查询的结果。 */
export interface RegistryTags {
  projectKey: string;
  /** 仓库全名，如 escloud/aioai。 */
  repo: string;
  /** 拼好的镜像前缀，如 es-repo.example.com:5000/escloud/aioai（不含 :tag）。 */
  imageBase: string;
  /** 仓库是**怎么定位到的**：image_record（平台推过）/ search（仓库检索）。 */
  source: 'image_record' | 'search' | '';
  /** Harbor Core 接口版本（v1 / v2）。 */
  apiVersion: string;
  timeKind: 'push' | 'build' | '';
  /** 仓库里一共有多少个标签；tags 只是其中最近的 limit 个。 */
  total: number;
  limit: number;
  /** 给用户看的一句说明（时间口径 / 是否被截断 / 仓库怎么找到的）。 */
  note: string;
  /**
   * 非空表示这次**读不到**，值是可读的原因（没接仓库 / 找不到对应仓库 / 没权限 / 仓库不通）。
   *
   * ⚠️ 不能用「tags 为空」代替它：空数组会被读成"这仓库没有标签"，
   * 而真实原因可能完全是另一回事。
   */
  unavailable: string;
  tags: RegistryTag[];
}

export const registryApi = {
  /** 列某项目对应仓库里最近的若干个标签（缺省 5 个）。 */
  harborTags: (projectKey: string, limit = 5) =>
    http.get<RegistryTags>('/registries/harbor/tags', { projectKey, limit }),
};

export default registryApi;
