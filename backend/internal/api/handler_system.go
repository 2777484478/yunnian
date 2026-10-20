package api

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/service"
)

// auditExportTTL 导出结果的回查有效期。
//
// 取 10 分钟：足够前端走完「提交 → 轮询 → 下载」，又不至于让一堆 CSV
// 长期占着缓存（单次导出上限 5 万行，量不小）。
const auditExportTTL = 10 * time.Minute

// --------------------------------------------------------------------------- 概览与集成

// systemStats GET /system/stats
func (h *handler) systemStats(c *gin.Context) {
	st, err := h.svc.System.Stats(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, st)
}

// systemIntegrations GET /system/integrations
//
// 返回各下游的"是否启用 + 实现来源"。前端据此显示"当前为本地模拟"这类标记——
// 运维最怕的是把模拟当真实，所以来源必须显式暴露，而不是藏在启动日志里。
func (h *handler) systemIntegrations(c *gin.Context) {
	list, err := h.svc.System.Integrations(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list})
}

// --------------------------------------------------------------------------- GitLab 接入配置
//
// 这三个接口解决一个具体问题：GitLab 的地址与令牌原先只能写进 configs/config.yaml，
// 改一次要重启一次平台。而"平台读不到仓库"这件事（列分支、校验 Dockerfile、
// 取提交号）恰恰是配置项目时最先撞上的，需要能当场改、当场验。
//
// 令牌只回掩码，明文永不出库；保存与测试都要求 user:manage。

// getGitLabSettings GET /system/integrations/gitlab
func (h *handler) getGitLabSettings(c *gin.Context) {
	dto, err := h.svc.System.GitLabSettings(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// updateGitLabSettings PUT /system/integrations/gitlab
//
// token 留空表示"不修改"——前端回显的是掩码，拿不到明文。
func (h *handler) updateGitLabSettings(c *gin.Context) {
	var in service.GitLabSettingsInput
	if !httpx.Bind(c, &in) {
		return
	}
	dto, err := h.svc.System.UpdateGitLabSettings(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// testGitLabConnection POST /system/integrations/gitlab/test
//
// 允许空请求体：不带参数表示"测已保存的那份配置"。
// 带 baseUrl/token 则是"测表单里还没保存的值"，支持先测再存。
// 注意 ok=false 是 200 响应而不是错误码——见 service.TestGitLabConnection 注释。
func (h *handler) testGitLabConnection(c *gin.Context) {
	var in service.TestGitLabInput
	_ = c.ShouldBindJSON(&in)
	res, err := h.svc.System.TestGitLabConnection(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

// --------------------------------------------------------------------------- 系统配置

// listConfig GET /system/config?group=
func (h *handler) listConfig(c *gin.Context) {
	list, err := h.svc.System.ListConfig(c.Request.Context(), c.Query("group"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list})
}

// getRuntimeSettings GET /system/runtime
//
// 并发上限的**生效值** + 当前在跑/排队实况。
//
// 为什么单独开一个接口，而不是复用 GET /system/config：那个要 user:manage
// （它列的是平台全部参数），而批量发布页只需要知道"一次能提交几个"，
// 用它的角色是发版的人（deploy:read）——为一个数字把整份参数表开放给他们不值当。
func (h *handler) getRuntimeSettings(c *gin.Context) {
	dto, err := h.svc.System.RuntimeSettings(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// updateConfig PUT /system/config
func (h *handler) updateConfig(c *gin.Context) {
	var in service.UpdateConfigInput
	if !httpx.Bind(c, &in) {
		return
	}
	list, err := h.svc.System.UpdateConfig(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list})
}

// --------------------------------------------------------------------------- 审计日志

// listAuditLogs GET /audit-logs
//
// 支持 module / user / action / result / keyword / startTime / endTime 联合过滤。
// 时间参数用 startTime / endTime（前端日期选择器的字段名），
// 而 service 内部叫 StartAt / EndAt——映射只在这一处做。
func (h *handler) listAuditLogs(c *gin.Context) {
	q := pageQuery(c)
	pq, list, total, err := h.svc.System.ListAuditLogs(c.Request.Context(), service.AuditQuery{
		Module:   c.Query("module"),
		User:     c.Query("user"),
		Action:   c.Query("action"),
		Result:   c.Query("result"),
		Keyword:  q.Keyword,
		StartAt:  firstNonEmpty(c.Query("startTime"), c.Query("startAt")),
		EndAt:    firstNonEmpty(c.Query("endTime"), c.Query("endAt")),
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

// getAuditLog GET /audit-logs/{id}
func (h *handler) getAuditLog(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	log, err := h.svc.System.GetAuditLog(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, log)
}

// exportAuditLogs POST /audit-logs/export
//
// 同步生成 CSV（当前数据量下只要几十毫秒），同时把结果按 taskId 缓存 10 分钟，
// 这样 GET /audit-logs/export/{taskId} 也能拿到——前端如果按文档写成"提交后轮询"，
// 不用改代码就能工作。
func (h *handler) exportAuditLogs(c *gin.Context) {
	var in service.AuditExportInput
	// 允许空请求体：不带过滤条件的导出是合法需求（导出全量）。
	_ = c.ShouldBindJSON(&in)

	res, err := h.svc.System.ExportAuditLogs(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	h.cacheExportResult(c, res)
	httpx.OK(c, res)
}

// auditExportResult GET /audit-logs/export/{taskId}
func (h *handler) auditExportResult(c *gin.Context) {
	taskID := httpx.PathString(c, "taskId")
	if taskID == "" {
		httpx.Fail(c, errValidation("缺少导出任务号"))
		return
	}
	// 未启用 Redis 时没有回查能力，直接说明原因，别让调用方以为是"任务号写错了"。
	if !h.cache.Enabled() {
		httpx.Fail(c, errValidation("未启用 Redis，导出结果不支持回查，请直接使用导出响应里的 content 字段"))
		return
	}
	raw, ok := h.cache.Get(c.Request.Context(), auditExportKey(taskID))
	if !ok {
		httpx.Fail(c, errValidation("导出结果已过期（保留 10 分钟），请重新导出"))
		return
	}
	c.Data(200, "text/csv; charset=utf-8", []byte(raw))
}

// --------------------------------------------------------------------------- 通知渠道

// listNotifyChannels GET /notify-channels
func (h *handler) listNotifyChannels(c *gin.Context) {
	list, err := h.svc.System.ListNotifyChannels(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list})
}

// createNotifyChannel POST /notify-channels
func (h *handler) createNotifyChannel(c *gin.Context) {
	var in service.NotifyInput
	if !httpx.Bind(c, &in) {
		return
	}
	ch, err := h.svc.System.CreateNotifyChannel(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	created(c, "通知渠道已创建", ch)
}

// updateNotifyChannel PUT /notify-channels/{id}
func (h *handler) updateNotifyChannel(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	var in service.NotifyInput
	if !httpx.Bind(c, &in) {
		return
	}
	ch, err := h.svc.System.UpdateNotifyChannel(c.Request.Context(), id, in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, ch)
}

// deleteNotifyChannel DELETE /notify-channels/{id}
func (h *handler) deleteNotifyChannel(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	if err := h.svc.System.DeleteNotifyChannel(c.Request.Context(), id, caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"deleted": true})
}

// testNotifyChannel POST /notify-channels/{id}/test
func (h *handler) testNotifyChannel(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	res, err := h.svc.System.TestNotifyChannel(c.Request.Context(), id, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

// setNotifyEnabled PATCH /notify-channels/{id}/enabled
func (h *handler) setNotifyEnabled(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	var in struct {
		Enabled *bool `json:"enabled" binding:"required"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	if err := h.svc.System.SetNotifyEnabled(c.Request.Context(), id, *in.Enabled, caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"enabled": *in.Enabled})
}

// --------------------------------------------------------------------------- 模型通道

// listModelProviders GET /models
func (h *handler) listModelProviders(c *gin.Context) {
	list, err := h.svc.System.ListModelProviders(c.Request.Context(), service.ModelQuery{
		Keyword: c.Query("keyword"),
		Vendor:  c.Query("vendor"),
		Enabled: optBool(c, "enabled"),
	})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list})
}

// createModelProvider POST /models
func (h *handler) createModelProvider(c *gin.Context) {
	var in service.ModelInput
	if !httpx.Bind(c, &in) {
		return
	}
	m, err := h.svc.System.CreateModelProvider(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	created(c, "模型通道已创建", m)
}

// updateModelProvider PUT /models/{id}
//
// apiKey 留空表示"不修改"——否则前端每次保存表单都要把密钥再发一遍，
// 而它根本读不回明文（接口只回掩码）。
func (h *handler) updateModelProvider(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	var in service.ModelInput
	if !httpx.Bind(c, &in) {
		return
	}
	m, err := h.svc.System.UpdateModelProvider(c.Request.Context(), id, in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, m)
}

// deleteModelProvider DELETE /models/{id}
func (h *handler) deleteModelProvider(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	if err := h.svc.System.DeleteModelProvider(c.Request.Context(), id, caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"deleted": true})
}

// testModelProvider POST /models/{id}/test
func (h *handler) testModelProvider(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	res, err := h.svc.System.TestModelProvider(c.Request.Context(), id, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

// setDefaultModelProvider POST /models/{id}/default
func (h *handler) setDefaultModelProvider(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	m, err := h.svc.System.SetDefaultModelProvider(c.Request.Context(), id, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, m)
}

// --------------------------------------------------------------------------- 探针

// health GET /healthz
//
// 存活探针只反映"进程还能响应"，不检查 DB/Redis/下游：
// 一旦下游抖动就把 Pod 判死重启，只会让故障面扩大（重启并不能修好下游）。
// 依赖就绪的判断在 /readyz。
func (h *handler) health(c *gin.Context) {
	httpx.OK(c, h.svc.System.Health())
}

// ready GET /readyz
//
// 就绪探针检查依赖。未就绪时返回 503（这里必须用 HTTP 状态码，
// 因为 K8s 探针只看状态码，不看业务响应体）。
func (h *handler) ready(c *gin.Context) {
	ok, items := h.svc.System.Readiness(c.Request.Context())
	body := httpx.Body{Code: 0, Message: "ok", Data: gin.H{"items": items}, TraceID: httpx.TraceID(c)}
	if !ok {
		body.Code = 50000
		body.Message = "依赖未就绪"
		c.JSON(503, body)
		return
	}
	c.JSON(200, body)
}

// --------------------------------------------------------------------------- 内部助手

// auditExportKey 导出结果的缓存键。
func auditExportKey(taskID string) string { return "audit:export:" + taskID }

// cacheExportResult 把导出结果按 taskId 缓存 10 分钟。
//
// 为什么要缓存而不是做成异步任务：当前量级（万级审计）同步生成 CSV 只要几十毫秒，
// 引入任务表 + 对象存储的复杂度不划算。缓存只是为了让「提交 → 轮询 → 下载」
// 这套前端写法也能工作，超时后重新导出即可。
func (h *handler) cacheExportResult(c *gin.Context, res *service.AuditExportResult) {
	if res == nil || res.TaskID == "" || res.Content == "" {
		return
	}
	if err := h.cache.Set(c.Request.Context(), auditExportKey(res.TaskID), res.Content, auditExportTTL); err != nil {
		// 缓存失败不影响导出结果本身：调用方已经拿到 Content 了。
		h.log.Warn("缓存审计导出结果失败", "err", err, "taskId", res.TaskID)
	}
}

// firstNonEmpty 返回第一个非空字符串，用于兼容同一语义的两种参数名。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// deleteUser DELETE /users/:id
func (h *handler) deleteUser(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	if err := h.svc.System.DeleteUser(c.Request.Context(), id, caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"deleted": true})
}
