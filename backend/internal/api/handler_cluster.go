package api

import (
	"github.com/gin-gonic/gin"

	"github.com/yunnian/yunnian/internal/httpx"
	"github.com/yunnian/yunnian/internal/service"
)

// --------------------------------------------------------------------------- K8s 集群接入
//
// 这套接口解决一个具体问题：在此之前平台**没有"集群"这个实体**。
// environment.cluster 是一段自由文本，执行侧是 LocalCluster 模拟器
// （Ping 恒返回 nil），于是「测试连接」永远全绿——填一个拼错的集群名也绿。
// 现在集群有凭据、有真实探测结果，环境只能引用它。
//
// 权限分两档，与 GitLab / Jenkins 的配置接口同一口径：
//   - 列表与命名空间只要 env:read / env:write：新建环境时要在这两处选集群与命名空间，
//     而建环境的是运维，不是系统管理员；
//   - 接入 / 改凭据 / 探测 / 删除要 user:manage：解析 kubeconfig 会暴露集群地址清单，
//     改删凭据直接影响部署目标，风险等级与"改 GitLab 令牌"相同。
//
// 安全约束：**任何响应都不含 kubeconfig**（明文或密文），只有一个 hasKubeconfig 布尔。

// listClusters GET /clusters
func (h *handler) listClusters(c *gin.Context) {
	list, err := h.svc.System.ListClusters(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, list)
}

// parseClusterKubeconfig POST /clusters/parse
//
// 纯解析、不落库：一份 kubeconfig 常含多个 context（dev/staging/prod 各一个很常见），
// 所以导入是"先解析、再挑"。
//
// 解析失败返回 40000（输入问题，用户改内容即可），而不是一个 ok=false 的 200——
// 与"探测不通"不同：那是业务结论，这是请求内容不合法。
func (h *handler) parseClusterKubeconfig(c *gin.Context) {
	var in service.ClusterParseInput
	if !httpx.Bind(c, &in) {
		return
	}
	res, err := h.svc.System.ParseClusterKubeconfig(c.Request.Context(), in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

// createClusters POST /clusters
//
// **部分成功**：逐个真探测，成功的落库返回，失败的带 context 名与原因回到 errors。
// 注意这里不是事务性的整批成功/失败——理由见 service.CreateClusters 注释。
func (h *handler) createClusters(c *gin.Context) {
	var in service.ClusterCreateInput
	if !httpx.Bind(c, &in) {
		return
	}
	res, err := h.svc.System.CreateClusters(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

// updateCluster PUT /clusters/{key}
//
// kubeconfig 留空表示"不修改凭据"——前端只看得到「已配置」，回传不了原文。
func (h *handler) updateCluster(c *gin.Context) {
	key := httpx.PathString(c, "key")
	var in service.ClusterUpdateInput
	if !httpx.Bind(c, &in) {
		return
	}
	dto, err := h.svc.System.UpdateCluster(c.Request.Context(), key, in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// deleteCluster DELETE /clusters/{key}
//
// 被环境引用时返回 40901，且消息里带上引用它的环境列表——
// 只说"删不掉"会让用户无从下手。
func (h *handler) deleteCluster(c *gin.Context) {
	key := httpx.PathString(c, "key")
	if err := h.svc.System.DeleteCluster(c.Request.Context(), key, caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, nil)
}

// testCluster POST /clusters/{key}/test
//
// 真的拨一次集群（读 /version + 列节点与命名空间）并回填结果。
// ok=false 是 200 响应而不是错误码——与 GitLab / Jenkins 的测试接口同一约定，
// 前端不能只靠 catch 判断。
func (h *handler) testCluster(c *gin.Context) {
	key := httpx.PathString(c, "key")
	res, err := h.svc.System.TestCluster(c.Request.Context(), key, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

// clusterNamespaces GET /clusters/{key}/namespaces
//
// 供新建环境时选命名空间，避免手输写错。集群不可达时返回 50200（上游不可用），
// **不返回空数组**：空数组会被前端理解成"这个集群没有命名空间"，
// 而事实是"没问到"。前端在出错时会退化成自由输入，不会把表单卡死。
func (h *handler) clusterNamespaces(c *gin.Context) {
	key := httpx.PathString(c, "key")
	ns, err := h.svc.System.ListClusterNamespaces(c.Request.Context(), key)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, ns)
}
