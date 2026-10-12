import { http } from './client';
import { unwrapList } from './shape';
import type {
  ClusterCreateResult,
  ClusterInput,
  ClusterParseResult,
  ClusterTestResult,
  K8sCluster,
} from '@/types';
// ---------------------------------------------------------------------------
// K8s 集群接入（对应后端 /clusters）
//
// 为什么需要这一层：平台原先**没有"集群"这个实体**。environment.cluster 只是一段
// 自由文本（后端 `binding:"required,max=128"`），新建环境时手输一个拼错的集群名
// 也能存进去，而没有任何代码拿它去建连接；执行侧是 LocalCluster 模拟器
// （Ping 恒真、Nodes 按集群名哈希编数），于是「测试连接」永远全绿。
// 后果还有一条很直观：同一个集群在库里能有两个名字——种子里 canary 是
// `k8s-prod-01`、prod 是 `k8s-prod-01（双可用区）`，前端只好靠正则裁括号来抹平。
//
// 本模块把集群变成**有凭据、有真实探测结果、可被环境引用**的实体，三条约定：
//
//   1. **kubeconfig 只进不出**。它含 client-key / token，属管理员级凭据。
//      接口只回 `hasKubeconfig` 布尔，明文没有任何读出口；
//      因此保存时 kubeconfig 留空表示"不修改"，而不是"清空"。
//
//   2. **「接入成功」的判据是真的拨通了一次 API**（读 /version + 列 Node），
//      探测不过就不算接入。这正是把"测试连接永远全绿"根治掉的地方——
//      它不能靠平台自己判断，只能靠集群回答。
//
//   3. **`ok=false` 不是请求失败**。探测接口永远返回 200，通不通看 `ok` 与
//      `message`（与 GitLab / Jenkins 的测试接口同一约定），调用方不能只 catch。
//
// ⚠️ 与 jenkins.ts 同一条纪律：**不做自动演示回退**。
// 后端尚未实现时返回的是真实错误，页面必须如实显示它——把 404 当成"后端还没做"
// 而悄悄换成一份假数据，会让"其实什么都没接上"看起来像"一切正常"
// （jenkins.ts 文件头记录了这次教训：`withDemoFallback` 已被删除，不要加回来）。
// 本文件内**不放任何示例数据**；需要评审界面时，由页面自己的预览开关提供样本，
// 并在页面上挂横幅写明"以下均为示例"（见 pages/system/ClusterAccess.tsx）。
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// 列表字段收口：JSON 的 `null` 一律折成 `[]`
//
// 为什么必须在**这一层**做：后端的列表字段是 Go 切片，nil 切片会被序列化成 `null`，
// 而这里声明的类型是 `string[]`。类型说"一定有数组"、数据说"可能是 null"——
// 这个不一致会在**渲染期间**爆成 TypeError，而 React 一旦在渲染中抛异常就会卸载整棵树，
// 结果是**整站白屏**（实测：集群接入页在"有集群但没被环境引用"时白屏，
// 因为 `usedByEnvs` 是 null 而页面写了 `c.usedByEnvs.length`）。
//
// 放在 client 与页面之间，等于让**类型声明变成真的**：页面可以放心按 `string[]` 用，
// 不必在每个读取点写 `?? []`（散落各处迟早会漏一个，漏掉的那一个就是下一次白屏）。
// 后端也已改成直接发 `[]`（见 service/cluster.go 的 nonNilSlice），这里是第二道保险。
// ---------------------------------------------------------------------------

const asList = <T,>(v: T[] | null | undefined): T[] => (Array.isArray(v) ? v : []);

const normalizeCluster = (c: K8sCluster): K8sCluster => ({
  ...c,
  usedByEnvs: asList(c.usedByEnvs),
});

export const clusterApi = {
  /**
   * 集群列表（只读，不含凭据内容）。
   *
   * 权限刻意比配置类接口低：新建环境时要在这里选集群，而建环境的是运维
   * （`env:write`）而不是系统管理员。所以列表接口按 `env:read` 放开，
   * 真正的接入/改凭据才需要 `user:manage`。
   */
  list: async (): Promise<K8sCluster[]> =>
    unwrapList<K8sCluster>(await http.get('/clusters')).map(normalizeCluster),

  /**
   * 解析一份 kubeconfig，列出其中的 context 供选择接入哪些。
   *
   * **不落库**，纯解析：一份 kubeconfig 常含多个集群（dev/staging/prod 各一个
   * context 很常见），所以导入是"先解析、再挑"，而不是"整份塞进去"。
   * 解析失败（格式错、证书字段缺失）时 ok=false + errors，**不抛异常**——
   * 页面据此在弹窗里内联列出原因，而不是弹一句就消失的 toast。
   * （后端此前走的是 40000 抛错，与这里的契约不一致，已按契约改回。）
   */
  parse: async (kubeconfig: string): Promise<ClusterParseResult> => {
    const res = await http.post<ClusterParseResult>('/clusters/parse', { kubeconfig });
    return { ...res, errors: asList(res.errors), contexts: asList(res.contexts) };
  },

  /**
   * 接入集群。可一次提交多个 context，各自成为一条集群记录。
   *
   * 后端会**逐个真探测**：成功的落库返回，失败的带 context 与原因回到 `errors`
   * （不能因为某一个连不上就把整批回滚——那样用户得挨个重试，
   * 也不能静默丢弃，否则用户不知道另外几个为什么没出现）。
   */
  create: async (clusters: ClusterInput[]): Promise<ClusterCreateResult> => {
    const res = await http.post<ClusterCreateResult>('/clusters', { clusters });
    return { ...res, clusters: asList(res.clusters), errors: asList(res.errors) };
  },

  /** 改名称/备注，或换一份 kubeconfig（kubeconfig 留空 = 不动凭据） */
  update: (key: string, body: Partial<ClusterInput>) =>
    http.put<K8sCluster>(`/clusters/${key}`, body),

  /**
   * 删除。**有环境正在引用时后端会拒绝**（返回引用它的环境列表），
   * 与"环境里有服务实例不许删环境"同一套口径——页面也要照此预先禁用。
   */
  remove: (key: string) => http.del<null>(`/clusters/${key}`),

  /** 连通性探测：真的拨一次集群，回填版本/节点数/命名空间数 */
  test: async (key: string): Promise<ClusterTestResult> => {
    const res = await http.post<ClusterTestResult>(`/clusters/${key}/test`);
    return { ...res, warnings: asList(res.warnings) };
  },

  /**
   * 列该集群的命名空间，供新建环境时选（避免手输 `yunnian-perf-sh` 这类名字写错）。
   *
   * **集群不可达时后端返回错误，不是空数组** —— 空数组会被当成"这个集群没有命名空间"，
   * 而事实是"没问到"。调用方要 catch，并在出错时退化成可自由输入，而不是把表单卡死。
   */
  namespaces: async (key: string): Promise<string[]> =>
    unwrapList<string>(await http.get(`/clusters/${key}/namespaces`)),
};
