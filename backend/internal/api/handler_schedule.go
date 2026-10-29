package api

import (
	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/middleware"
	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/service"
)

// --------------------------------------------------------------------------- 定时任务
//
// 挂在 `/schedules` 顶层而不是 `/projects/{key}/schedules`：
// 定时任务横跨 CI 和 CD（一条任务要么构建要么部署），挂到项目下会让"所有项目的
// 定时任务总览"变成 N 次请求；而运维最常问的恰恰是"现在有哪些定时任务在跑"。
//
// 权限：读用 build:read / deploy:read（任一即可，因为任务跨两个域）；
// 写用 build:trigger / deploy:read 进门，**具体动作由服务层按任务类型再判一次**
// —— 配定时构建要有 build:trigger，配定时部署要有该环境的 deploy 权限。
// 路由层无法表达"按 payload 里的 taskType/envKey 动态校验"。

func (h *handler) registerSchedules(g *gin.RouterGroup) {
	s := g.Group("/schedules")
	// 预览 cron 不需要写权限：它只做纯计算，不落库。
	// 放在最前面，避免 /:id 那条把 "preview" 当成 id 解析。
	s.GET("/preview", middleware.RequirePerm("build:read", "deploy:read"), h.previewSchedule)

	s.GET("", middleware.RequirePerm("build:read", "deploy:read"), h.listSchedules)
	s.GET("/:id", middleware.RequirePerm("build:read", "deploy:read"), h.getSchedule)
	s.POST("", middleware.RequirePerm("build:trigger", "deploy:read"), h.createSchedule)
	s.PUT("/:id", middleware.RequirePerm("build:trigger", "deploy:read"), h.updateSchedule)
	s.DELETE("/:id", middleware.RequirePerm("build:trigger", "deploy:read"), h.deleteSchedule)
	s.POST("/:id/enabled", middleware.RequirePerm("build:trigger", "deploy:read"), h.setScheduleEnabled)
	// 立即执行一次：调试用。它**不推进下次计划时间**，
	// 否则"点一下试试"会把当晚的执行吃掉。
	s.POST("/:id/run", middleware.RequirePerm("build:trigger", "deploy:read"), h.runScheduleNow)
}

// previewSchedule GET /schedules/preview?cron=0 2 * * *
func (h *handler) previewSchedule(c *gin.Context) {
	httpx.OK(c, h.svc.Schedule.PreviewSchedule(c.Query("cron")))
}

// listSchedules GET /schedules?taskType=&projectKey=
func (h *handler) listSchedules(c *gin.Context) {
	list, err := h.svc.Schedule.ListSchedules(c.Request.Context(), c.Query("taskType"), c.Query("projectKey"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, list)
}

// getSchedule GET /schedules/{id}
func (h *handler) getSchedule(c *gin.Context) {
	id, ok := pathScheduleID(c)
	if !ok {
		return
	}
	dto, err := h.svc.Schedule.GetSchedule(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// createSchedule POST /schedules
func (h *handler) createSchedule(c *gin.Context) {
	var in service.ScheduleInput
	if !httpx.Bind(c, &in) {
		return
	}
	dto, err := h.svc.Schedule.CreateSchedule(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// updateSchedule PUT /schedules/{id}
func (h *handler) updateSchedule(c *gin.Context) {
	id, ok := pathScheduleID(c)
	if !ok {
		return
	}
	var in service.ScheduleInput
	if !httpx.Bind(c, &in) {
		return
	}
	dto, err := h.svc.Schedule.UpdateSchedule(c.Request.Context(), id, in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// deleteSchedule DELETE /schedules/{id}
func (h *handler) deleteSchedule(c *gin.Context) {
	id, ok := pathScheduleID(c)
	if !ok {
		return
	}
	if err := h.svc.Schedule.DeleteSchedule(c.Request.Context(), id, caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"deleted": true})
}

// setScheduleEnabled POST /schedules/{id}/enabled  body: {"enabled":true}
//
// 单列一个动作而不是"编辑里勾选"：停用是运维动作
// （正在发版、先停掉定时），不该陪着一起改 cron、版本这些配置项。
func (h *handler) setScheduleEnabled(c *gin.Context) {
	id, ok := pathScheduleID(c)
	if !ok {
		return
	}
	var in struct {
		Enabled bool `json:"enabled"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	dto, err := h.svc.Schedule.SetScheduleEnabled(c.Request.Context(), id, in.Enabled, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// runScheduleNow POST /schedules/{id}/run
func (h *handler) runScheduleNow(c *gin.Context) {
	id, ok := pathScheduleID(c)
	if !ok {
		return
	}
	dto, err := h.svc.Schedule.RunScheduleNow(c.Request.Context(), id, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// pathScheduleID 解析路径里的任务 id。
func pathScheduleID(c *gin.Context) (uint64, bool) {
	id, err := httpx.ParseIDString(httpx.PathString(c, "id"))
	if err != nil {
		httpx.Fail(c, err)
		return 0, false
	}
	return id, true
}
