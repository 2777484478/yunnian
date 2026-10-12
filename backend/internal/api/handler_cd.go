package api

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/service"
)

// --------------------------------------------------------------------------- 环境

// listEnvironments GET /environments
func (h *handler) listEnvironments(c *gin.Context) {
	q := pageQuery(c)
	pq, list, total, err := h.svc.CD.ListEnvironments(c.Request.Context(), service.EnvironmentQuery{
		Keyword:  q.Keyword,
		Status:   c.Query("status"),
		Enabled:  optBool(c, "enabled"),
		Page:     q.Page,
		PageSize: q.PageSize,
		SortBy:   q.SortBy,
		Order:    q.Order,
	})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, httpx.NewPageData(list, total, *pq))
}

// getEnvironment GET /environments/{key}
func (h *handler) getEnvironment(c *gin.Context) {
	e, err := h.svc.CD.GetEnvironment(c.Request.Context(), httpx.PathString(c, "key"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, e)
}

// createEnvironment POST /environments
func (h *handler) createEnvironment(c *gin.Context) {
	var in service.EnvironmentInput
	if !httpx.Bind(c, &in) {
		return
	}
	e, err := h.svc.CD.CreateEnvironment(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	created(c, "环境已创建", e)
}

// updateEnvironment PUT /environments/{key}
func (h *handler) updateEnvironment(c *gin.Context) {
	var in service.EnvironmentInput
	if !httpx.Bind(c, &in) {
		return
	}
	e, err := h.svc.CD.UpdateEnvironment(c.Request.Context(), httpx.PathString(c, "key"), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, e)
}

// deleteEnvironment DELETE /environments/{key}
func (h *handler) deleteEnvironment(c *gin.Context) {
	if err := h.svc.CD.DeleteEnvironment(c.Request.Context(), httpx.PathString(c, "key"), caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"deleted": true})
}

// envProjectCandidates GET /environments/{key}/project-candidates
func (h *handler) envProjectCandidates(c *gin.Context) {
	list, err := h.svc.CD.EnvProjectCandidates(c.Request.Context(), httpx.PathString(c, "key"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list})
}

// envSetProjects PUT /environments/{key}/projects
//
// 覆盖式纳管：请求体是"保存后该环境纳管哪些项目"（含 key 列表为空 = 全部取消纳管）。
// 与 PUT /environments/{key}/vars 同一个口径。
func (h *handler) envSetProjects(c *gin.Context) {
	var in service.EnvProjectsInput
	if !httpx.Bind(c, &in) {
		return
	}
	list, err := h.svc.CD.SetEnvProjects(c.Request.Context(), httpx.PathString(c, "key"), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list})
}

// envVars GET /environments/{key}/vars
func (h *handler) envVars(c *gin.Context) {
	vars, err := h.svc.CD.EnvVars(c.Request.Context(), httpx.PathString(c, "key"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": vars})
}

// saveEnvVars PUT /environments/{key}/vars
//
// 覆盖式保存：请求体就是"保存后应该长什么样"的完整集合。
// 密文项传掩码（******）时服务端保留原值，不会把掩码写进库。
func (h *handler) saveEnvVars(c *gin.Context) {
	var in service.EnvVarsInput
	if !httpx.Bind(c, &in) {
		return
	}
	vars, err := h.svc.CD.SaveEnvVars(c.Request.Context(), httpx.PathString(c, "key"), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, vars)
}

// envServices GET /environments/{key}/services
func (h *handler) envServices(c *gin.Context) {
	list, err := h.svc.CD.EnvServices(c.Request.Context(), httpx.PathString(c, "key"), service.EnvServiceQuery{
		Status:  c.Query("status"),
		Type:    c.Query("type"),
		Keyword: c.Query("keyword"),
	})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list})
}

// envResources GET /environments/{key}/resources
func (h *handler) envResources(c *gin.Context) {
	st, err := h.svc.CD.EnvResources(c.Request.Context(), httpx.PathString(c, "key"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, st)
}

// syncEnvironment POST /environments/{key}/sync
func (h *handler) syncEnvironment(c *gin.Context) {
	e, err := h.svc.CD.SyncEnvironment(c.Request.Context(), httpx.PathString(c, "key"), caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, e)
}

// envEvents GET /environments/{key}/events
//
// 未接入集群时恒为空数组：编造事件比返回空更糟——
// 运维会照着假事件去排查一个不存在的问题。
func (h *handler) envEvents(c *gin.Context) {
	list, err := h.svc.CD.EnvEvents(c.Request.Context(), httpx.PathString(c, "key"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list})
}

// testEnvConnection POST /environments/{key}/test-connection
func (h *handler) testEnvConnection(c *gin.Context) {
	res, err := h.svc.CD.TestEnvConnection(c.Request.Context(), httpx.PathString(c, "key"), caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

// --------------------------------------------------------------------------- 服务实例

// getService GET /services/{id}
func (h *handler) getService(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	s, err := h.svc.CD.GetService(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, s)
}

// servicePods GET /services/{id}/pods
func (h *handler) servicePods(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	pods, err := h.svc.CD.ServicePods(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": pods})
}

// serviceVersions GET /services/{id}/versions
//
// 回滚页面的数据源：历史镜像 + 部署人 + 是否当前版本 + 上一稳定版本标记。
func (h *handler) serviceVersions(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	versions, err := h.svc.CD.ServiceVersions(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": versions})
}

// serviceParamHistory GET /services/{id}/params/history
func (h *handler) serviceParamHistory(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	list, err := h.svc.CD.ServiceParamHistory(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list})
}

// podLogStream GET /services/{id}/pods/{podName}/logs/stream（SSE）
//
// 先把 Pod 日志灌进日志流（StreamPodLogs 负责采集），再用与构建日志相同的
// SSE 通道推给前端——这样"实时日志"在前后端只有一套实现。
func (h *handler) podLogStream(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	podName := httpx.PathString(c, "podName")
	if podName == "" {
		httpx.Fail(c, errValidation("缺少 Pod 名称"))
		return
	}
	tailLines := httpx.QueryInt(c, "tailLines", 500)
	if tailLines < 0 {
		tailLines = 0
	}
	if tailLines > 5000 {
		tailLines = 5000
	}

	key, err := h.svc.CD.StreamPodLogs(c.Request.Context(), id, podName, tailLines)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	// backlog 传 nil：Pod 日志是 Kubernetes 侧的实时 tail，平台没有它的历史副本，
	// 能补的只有流缓冲区里已有的部分——而那个 stream.Subscribe 自己会重放。
	streamLogs(c, h.svc.Stream(), key, 0, nil)
}

// updateServiceParams PUT /services/{id}/params
func (h *handler) updateServiceParams(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	if !h.guardServiceEnv(c, "deploy", id) {
		return
	}
	var in service.ServiceParamsInput
	if !httpx.Bind(c, &in) {
		return
	}
	svc, err := h.svc.CD.UpdateServiceParams(c.Request.Context(), id, in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	// 参数只落库不重启：容器里的环境变量与资源限制要等下次滚动更新才生效。
	// 返回 applyRequiresRestart 让前端能明确提示这一点，而不是让用户以为"保存完就生效了"。
	version := 0
	if hist, herr := h.svc.CD.ServiceParamHistory(c.Request.Context(), id); herr == nil && len(hist) > 0 {
		version = int(hist[0].Version)
	}
	httpx.OK(c, gin.H{
		"service":              svc,
		"paramVersion":         version,
		"applyRequiresRestart": true,
	})
}

// scaleService POST /services/{id}/scale
func (h *handler) scaleService(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	if !h.guardServiceEnv(c, "deploy", id) {
		return
	}
	var in struct {
		Replicas int    `json:"replicas" binding:"required,min=0,max=200"`
		Reason   string `json:"reason" binding:"omitempty,max=255"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	before, _ := h.svc.CD.GetService(c.Request.Context(), id)
	svc, err := h.svc.CD.ScaleService(c.Request.Context(), id, in.Replicas, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	prev := 0
	if before != nil {
		prev = before.Replicas
	}
	httpx.OK(c, gin.H{
		"replicas":         svc.Replicas,
		"previousReplicas": prev,
		"status":           svc.Status,
		"service":          svc,
	})
}

// restartService POST /services/{id}/restart（滚动重启）
func (h *handler) restartService(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	if !h.guardServiceEnv(c, "deploy", id) {
		return
	}
	if err := h.svc.CD.RestartService(c.Request.Context(), id, caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"restarted": true})
}

// redeployService POST /services/{id}/redeploy（当前镜像滚动更新）
func (h *handler) redeployService(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	if !h.guardServiceEnv(c, "deploy", id) {
		return
	}
	d, err := h.svc.CD.RedeployService(c.Request.Context(), id, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	createDeployPayload(c, "已提交重新部署", d)
}

// deleteService DELETE /services/{id}（下线）
func (h *handler) deleteService(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	if !h.guardServiceEnv(c, "deploy", id) {
		return
	}
	if err := h.svc.CD.DeleteService(c.Request.Context(), id, caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"deleted": true})
}

// --------------------------------------------------------------------------- 部署

// createDeployment POST /deployments
//
// 两种情况都是成功：直接开跑（201）与进入待审批（201）——
// "等审批"不是错误，前端按 status 渲染不同状态即可。
// 唯一键冲突（40901/40902）才走错误分支，并且带上已有任务 ID 方便前端跳转。
func (h *handler) createDeployment(c *gin.Context) {
	var in service.CreateDeploymentInput
	if !httpx.Bind(c, &in) {
		return
	}
	// 权限点按环境动态判定：deploy:test / deploy:prod …
	if !requireEnvPerm(c, "deploy", in.EnvKey) {
		return
	}
	d, err := h.svc.CD.CreateDeployment(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	createDeployPayload(c, "部署任务已创建", d)
}

// listDeployments GET /deployments
func (h *handler) listDeployments(c *gin.Context) {
	q := pageQuery(c)
	pq, list, total, err := h.svc.CD.ListDeployments(c.Request.Context(), service.DeploymentQuery{
		EnvKey:     c.Query("envKey"),
		ProjectKey: c.Query("projectKey"),
		Status:     c.Query("status"),
		Keyword:    q.Keyword,
		Page:       q.Page,
		PageSize:   q.PageSize,
		SortBy:     q.SortBy,
		Order:      q.Order,
	})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, httpx.NewPageData(list, total, *pq))
}

// getDeployment GET /deployments/{id}
func (h *handler) getDeployment(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	d, err := h.svc.CD.GetDeployment(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, d)
}

// deploymentLogs GET /deployments/{id}/logs
func (h *handler) deploymentLogs(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	limit := httpx.QueryInt(c, "limit", 1000)
	if limit > 5000 {
		limit = 5000
	}
	lines, err := h.svc.CD.DeploymentLogs(c.Request.Context(), id, uint32(queryUint(c, "afterSeq", 0)), limit)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, httpx.PageData{List: lines, Total: int64(len(lines)), Page: 1, PageSize: limit})
}

// deploymentLogStream GET /deployments/{id}/logs/stream（SSE）
func (h *handler) deploymentLogStream(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	if _, err := h.svc.CD.GetDeployment(c.Request.Context(), id); err != nil {
		httpx.Fail(c, err)
		return
	}
	// backlog 传 nil：部署日志没有落库表（设计如此，见 service.appendDeployLog），
	// 没有可补的持久化历史。与构建流的补发不同——构建日志有 build_log 表兜底。
	streamLogs(c, h.svc.Stream(), service.DeployLogStreamKey(id), 0, nil)
}

// approveDeployment POST /deployments/{id}/approve
func (h *handler) approveDeployment(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	var in struct {
		Remark string `json:"remark" binding:"omitempty,max=255"`
	}
	// 审批意见可以为空，所以这里不因绑定失败而中断（无请求体也不报错）。
	_ = c.ShouldBindJSON(&in)
	d, err := h.svc.CD.ApproveDeployment(c.Request.Context(), id, in.Remark, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, d)
}

// rejectDeployment POST /deployments/{id}/reject
//
// 驳回必须给理由：被驳回的人需要知道改什么，否则只能来问审批人，
// 而审批的意义就是把判断依据落到记录里。
func (h *handler) rejectDeployment(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	var in struct {
		Reason string `json:"reason" binding:"required,max=255"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	d, err := h.svc.CD.RejectDeployment(c.Request.Context(), id, in.Reason, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, d)
}

// cancelDeployment POST /deployments/{id}/cancel
func (h *handler) cancelDeployment(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	if !h.guardDeployEnv(c, "deploy", id) {
		return
	}
	if err := h.svc.CD.CancelDeployment(c.Request.Context(), id, caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"canceled": true})
}

// retryDeployment POST /deployments/{id}/retry
func (h *handler) retryDeployment(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	if !h.guardDeployEnv(c, "deploy", id) {
		return
	}
	d, err := h.svc.CD.RetryDeployment(c.Request.Context(), id, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	createDeployPayload(c, "已重新提交部署", d)
}

// pendingApprovals GET /deployments/pending-approval
func (h *handler) pendingApprovals(c *gin.Context) {
	list, err := h.svc.CD.PendingApprovals(c.Request.Context(), caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list})
}

// deploymentAIDiagnosis GET /deployments/{id}/ai-diagnosis
func (h *handler) deploymentAIDiagnosis(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	d, err := h.svc.CD.DeploymentAIDiagnosis(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, d)
}

// --------------------------------------------------------------------------- 回滚

// createRollback POST /rollbacks
//
// 回滚复用部署状态机（服务层实现），所以响应形状与部署单一致：
// 前端拿到的是同一个"任务"+ 同一个日志流地址，不需要为回滚写第二套进度视图。
func (h *handler) createRollback(c *gin.Context) {
	var in service.CreateRollbackInput
	if !httpx.Bind(c, &in) {
		return
	}
	svcID, err := httpx.ParseIDString(in.ServiceID)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	if !h.guardServiceEnv(c, "deploy", svcID) {
		return
	}
	// 回滚要同时具备 rollback:execute（路由层已判）与该环境的部署权限（上面已判，
	// 因为回滚最终就是一次部署），两道闸对应两种不同的授权意图，不能省。
	rb, err := h.svc.CD.CreateRollback(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	created(c, "回滚任务已提交", rollbackCreatedPayload(rb))
}

// listRollbacks GET /rollbacks
func (h *handler) listRollbacks(c *gin.Context) {
	q := pageQuery(c)
	pq, list, total, err := h.svc.CD.ListRollbacks(c.Request.Context(), service.DeploymentQuery{
		EnvKey:      c.Query("envKey"),
		ProjectKey:  c.Query("projectKey"),
		Status:      c.Query("status"),
		TriggerType: c.Query("triggerType"),
		Keyword:     q.Keyword,
		Page:        q.Page,
		PageSize:    q.PageSize,
		SortBy:      q.SortBy,
		Order:       q.Order,
	})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, httpx.NewPageData(list, total, *pq))
}

// getRollback GET /rollbacks/{id}
func (h *handler) getRollback(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	rb, err := h.svc.CD.GetRollback(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, rb)
}

// rollbackLogStream GET /rollbacks/{id}/logs/stream（SSE）
//
// 回滚的执行实体是它对应的那次部署单，日志流也就挂在部署单上——
// 这样"回滚进行中"的日志与普通部署完全一致，前端只有一个日志组件。
func (h *handler) rollbackLogStream(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	rb, err := h.svc.CD.GetRollback(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	deployID, perr := httpx.ParseIDString(rb.DeployID)
	if perr != nil || deployID == 0 {
		httpx.Fail(c, errValidation("该回滚记录没有关联的部署单，无法提供实时日志"))
		return
	}
	// 回滚的执行实体就是那次部署单，日志同源，因此与 deploymentLogStream 一样不补历史。
	streamLogs(c, h.svc.Stream(), service.DeployLogStreamKey(deployID), 0, nil)
}

// --------------------------------------------------------------------------- 响应组装

// createDeployPayload 部署单创建响应。
//
// 刻意只挑"发起者需要立刻知道"的字段：单号、环境、镜像、副本、状态、
// 审批人（待审批时）与日志流地址。完整步骤与日志由详情接口给。
func createDeployPayload(c *gin.Context, message string, d *service.DeployDTO) {
	data := gin.H{
		"id":           d.ID,
		"deployNo":     d.DeployNo,
		"envKey":       d.EnvKey,
		"envName":      d.EnvName,
		"projectKey":   d.ProjectKey,
		"image":        d.Image,
		"replicas":     d.Replicas,
		"status":       d.Status,
		"logStreamUrl": "/api/v1/deployments/" + d.ID + "/logs/stream",
		"createdAt":    d.StartedAt,
	}
	if d.Status == "pending_approval" {
		if d.Approver != "" {
			data["approvers"] = splitApprovers(d.Approver)
		}
		if message == "部署任务已创建" {
			message = "已提交审批，等待 " + d.Approver + " 确认"
		}
	}
	created(c, message, data)
}

// rollbackCreatedPayload 回滚创建响应。
func rollbackCreatedPayload(rb *service.RollbackDTO) gin.H {
	return gin.H{
		"id":           rb.ID,
		"rollbackId":   rb.ID,
		"deployNo":     rb.DeployNo,
		"envKey":       rb.EnvKey,
		"projectKey":   rb.ProjectKey,
		"fromImage":    rb.FromImage,
		"toImage":      rb.ToImage,
		"status":       rb.Status,
		"logStreamUrl": "/api/v1/rollbacks/" + rb.ID + "/logs/stream",
		"createdAt":    rb.StartedAt,
	}
}

// splitApprovers 把 "运维示例 / 林曦" 拆成数组。
func splitApprovers(raw string) []string {
	parts := strings.FieldsFunc(raw, func(r rune) bool { return r == '/' || r == ',' || r == '、' })
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}
