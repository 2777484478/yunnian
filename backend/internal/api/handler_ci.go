package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/buildscript"
	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/logstream"
	"github.com/2777484478/yunnian/internal/service"
)

// --------------------------------------------------------------------------- 项目

// listProjects GET /projects
func (h *handler) listProjects(c *gin.Context) {
	q := pageQuery(c)
	pq, list, total, err := h.svc.CI.ListProjects(c.Request.Context(), service.ProjectQuery{
		Keyword:  q.Keyword,
		Type:     c.Query("type"),
		Team:     c.Query("team"),
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

// getProject GET /projects/{key}
func (h *handler) getProject(c *gin.Context) {
	p, err := h.svc.CI.GetProject(c.Request.Context(), httpx.PathString(c, "key"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, p)
}

// createProject POST /projects
//
// 返回 201：前端据此区分"新建成功"与"编辑保存"，也便于接入网关做幂等缓存。
func (h *handler) createProject(c *gin.Context) {
	var in service.ProjectInput
	if !httpx.Bind(c, &in) {
		return
	}
	p, err := h.svc.CI.CreateProject(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	created(c, "项目已创建", p)
}

// updateProject PUT /projects/{key}
func (h *handler) updateProject(c *gin.Context) {
	var in service.ProjectInput
	if !httpx.Bind(c, &in) {
		return
	}
	p, err := h.svc.CI.UpdateProject(c.Request.Context(), httpx.PathString(c, "key"), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, p)
}

// deleteProject DELETE /projects/{key}（软删）
func (h *handler) deleteProject(c *gin.Context) {
	if err := h.svc.CI.DeleteProject(c.Request.Context(), httpx.PathString(c, "key"), caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"deleted": true})
}

// setProjectEnabled PATCH /projects/{key}/enabled
func (h *handler) setProjectEnabled(c *gin.Context) {
	var in struct {
		Enabled *bool `json:"enabled" binding:"required"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	if err := h.svc.CI.SetProjectEnabled(c.Request.Context(), httpx.PathString(c, "key"), *in.Enabled, caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"enabled": *in.Enabled})
}

// defaultBuildScript GET /projects/default-build-script?type=backend
//
// 给界面上的「恢复默认脚本」用。由后端下发而不是前端硬编码一份：
// 默认脚本要和构建机上的镜像地址、平台注入的变量名保持一致，
// 前端各留一份副本，迟早会出现"界面给的默认脚本跑不通"。
func (h *handler) defaultBuildScript(c *gin.Context) {
	httpx.OK(c, gin.H{"script": buildscript.DefaultScript(c.Query("type"))})
}

// testGitConnection POST /projects/test-connection
//
// 权限与"编辑项目"一致（project:write）：它会用到凭据去连外部系统，
// 不该让只读角色借这个接口去试别人的凭据。
func (h *handler) testGitConnection(c *gin.Context) {
	var in service.TestGitConnectionInput
	if !httpx.Bind(c, &in) {
		return
	}
	res, err := h.svc.CI.TestGitConnection(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

// projectBranches GET /projects/{key}/branches
func (h *handler) projectBranches(c *gin.Context) {
	branches, err := h.svc.CI.ProjectBranches(c.Request.Context(), httpx.PathString(c, "key"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": branches})
}

// validateProject POST /projects/{key}/validate
//
// 也支持"保存前预检"：请求体可带完整项目配置，此时按请求体校验而不是库里已有的记录。
// 没有请求体（或字段为空）则回退到库里已有的项目。
func (h *handler) validateProject(c *gin.Context) {
	key := httpx.PathString(c, "key")
	res, err := h.svc.CI.ValidateProject(c.Request.Context(), key, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

// listProjectBuilds GET /projects/{key}/builds
func (h *handler) listProjectBuilds(c *gin.Context) {
	q := pageQuery(c)
	pq, list, total, err := h.svc.CI.ListProjectBuilds(c.Request.Context(), httpx.PathString(c, "key"), service.BuildQuery{
		Status:   c.Query("status"),
		Branch:   c.Query("branch"),
		Keyword:  q.Keyword,
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

// listProjectImages GET /projects/{key}/images
func (h *handler) listProjectImages(c *gin.Context) {
	q := pageQuery(c)
	pq, list, total, err := h.svc.CI.ListProjectImages(c.Request.Context(), httpx.PathString(c, "key"), service.ImageQuery{
		Keyword:  q.Keyword,
		Env:      c.Query("env"),
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

// listProjectDeployments GET /projects/{key}/deployments
//
// 复用部署列表接口，把 projectKey 作为固定过滤条件。
// 单独开一个 service 方法只会多一份几乎相同的查询代码。
func (h *handler) listProjectDeployments(c *gin.Context) {
	q := pageQuery(c)
	pq, list, total, err := h.svc.CD.ListDeployments(c.Request.Context(), service.DeploymentQuery{
		ProjectKey: httpx.PathString(c, "key"),
		Status:     c.Query("status"),
		EnvKey:     c.Query("envKey"),
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

// --------------------------------------------------------------------------- 构建

// triggerBuild POST /builds
//
// 201 表示"新任务已创建"；命中幂等键时返回 40902 + 已有任务信息，
// 让前端可以提示"已存在同一次构建"并直接跳到那个构建，而不是报一个看不懂的错误。
func (h *handler) triggerBuild(c *gin.Context) {
	var in service.TriggerBuildInput
	if !httpx.Bind(c, &in) {
		return
	}
	b, err := h.svc.CI.TriggerBuild(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	created(c, "构建任务已创建", buildCreatedPayload(b))
}

// listBuilds GET /builds
func (h *handler) listBuilds(c *gin.Context) {
	q := pageQuery(c)
	pq, list, total, err := h.svc.CI.ListBuilds(c.Request.Context(), service.BuildQuery{
		ProjectKey:  c.Query("projectKey"),
		ProjectType: c.Query("projectType"),
		Status:      c.Query("status"),
		Branch:      c.Query("branch"),
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

// getBuild GET /builds/{id}
func (h *handler) getBuild(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	b, err := h.svc.CI.GetBuild(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, b)
}

// buildLogs GET /builds/{id}/logs?afterSeq=&limit=
//
// 与 SSE 用同一套 seq 语义：SSE 中途断开、或前端想直接拉全量时都走这里。
// 返回值是 {list,total,page,pageSize} 的分页壳，list 里每项是一行日志。
func (h *handler) buildLogs(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	limit := httpx.QueryInt(c, "limit", 1000)
	afterSeq := uint32(queryUint(c, "afterSeq", 0))
	if limit > 5000 {
		limit = 5000
	}
	lines, err := h.svc.CI.BuildLogs(c.Request.Context(), id, afterSeq, limit)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, httpx.PageData{List: lines, Total: int64(len(lines)), Page: 1, PageSize: limit})
}

// buildLogStream GET /builds/{id}/logs/stream（SSE）
func (h *handler) buildLogStream(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	// 先确认构建存在，再开流：否则不存在的构建会先建立 SSE 连接再发一条 end 事件，
	// 前端拿到的是 200 + 空流，排查时很难看出是"ID 写错了"。
	if _, err := h.svc.CI.GetBuild(c.Request.Context(), id); err != nil {
		httpx.Fail(c, err)
		return
	}
	// 带上 backlog：构建日志会落 build_log 表，缓冲区过期后仍能把历史补出来。
	streamLogs(c, h.svc.Stream(), service.BuildLogStreamKey(id), 0, h.buildLogBacklog(id))
}

// buildLogBacklog 构造"从库里补发构建历史日志"的回调。
func (h *handler) buildLogBacklog(id uint64) logBacklog {
	return func(ctx context.Context, afterSeq uint32, limit int) []logstream.Event {
		return h.svc.CI.BuildLogEvents(ctx, id, afterSeq, limit)
	}
}

// cancelBuild POST /builds/{id}/cancel
func (h *handler) cancelBuild(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	if err := h.svc.CI.CancelBuild(c.Request.Context(), id, caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"canceled": true})
}

// retryBuild POST /builds/{id}/retry
func (h *handler) retryBuild(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	b, err := h.svc.CI.RetryBuild(c.Request.Context(), id, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	created(c, "已重新提交构建", buildCreatedPayload(b))
}

// buildAIDiagnosis GET /builds/{id}/ai-diagnosis
func (h *handler) buildAIDiagnosis(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	d, err := h.svc.CI.BuildAIDiagnosis(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, d)
}

// --------------------------------------------------------------------------- 镜像

// listImages GET /images
func (h *handler) listImages(c *gin.Context) {
	q := pageQuery(c)
	pq, list, total, err := h.svc.CI.ListImages(c.Request.Context(), service.ImageQuery{
		ProjectKey: c.Query("projectKey"),
		Env:        c.Query("env"),
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

// getImage GET /images/{id}
func (h *handler) getImage(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	img, err := h.svc.CI.GetImage(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, img)
}

// imageUsedBy GET /images/{id}/used-by
func (h *handler) imageUsedBy(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	usage, err := h.svc.CI.ImageUsedBy(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, usage)
}

// deleteImage DELETE /images/{id}
func (h *handler) deleteImage(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	if err := h.svc.CI.DeleteImage(c.Request.Context(), id, caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"deleted": true})
}

// cleanupImages POST /images/cleanup（支持 dryRun）
func (h *handler) cleanupImages(c *gin.Context) {
	var in service.CleanupInput
	if !httpx.Bind(c, &in) {
		return
	}
	res, err := h.svc.CI.CleanupImages(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

// --------------------------------------------------------------------------- 响应组装

// created 写出 201 响应。
func created(c *gin.Context, message string, data any) {
	c.JSON(http.StatusCreated, httpx.Body{Code: 0, Message: message, Data: data, TraceID: httpx.TraceID(c)})
}

// buildCreatedPayload 收敛"任务刚创建"时前端需要的字段。
//
// 只挑这几个字段而不是整个 BuildDTO：创建响应的用途是"告诉前端去哪儿看进度"
// （logStreamUrl）+ 让用户确认这次提交的参数，多余字段会诱导前端从两处取同一份数据。
func buildCreatedPayload(b *service.BuildDTO) gin.H {
	return gin.H{
		"id":           b.ID,
		"buildNo":      b.BuildNo,
		"projectKey":   b.ProjectKey,
		"branch":       b.Branch,
		"commitId":     b.CommitID,
		"status":       b.Status,
		"stage":        b.Stage,
		"progress":     b.Progress,
		"logStreamUrl": "/api/v1/builds/" + b.ID + "/logs/stream",
		"startedAt":    b.StartedAt,
	}
}
