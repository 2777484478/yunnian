package api

import (
	"fmt"
	"net/url"

	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/service"
)

// 环境巡检报告。
//
// 与别的接口有一处不同：**下载走文件流**，不是 JSON。
// 报告 HTML 是自包含的，用户下载下来双击就能看，也可以直接转给别人 ——
// 这比"再登录平台看一次"实用得多，尤其在群里说"这台环境有问题"的时候。

// inspectEnvironment POST /inspection-reports
func (h *handler) inspectEnvironment(c *gin.Context) {
	var in service.InspectInput
	if !httpx.Bind(c, &in) {
		return
	}
	res, err := h.svc.CD.InspectEnvironment(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	// 列表/对话里只需要摘要，不必把整份 HTML 塞进 JSON（几十 KB 且用不上）。
	// 要看正文走 GET /:id 或 /:id/download。
	html := res.HTML
	res.HTML = ""
	httpx.OK(c, gin.H{
		"report":  res,
		"htmlLen": len(html),
	})
}

// listInspectionReports GET /inspection-reports?envKey=
func (h *handler) listInspectionReports(c *gin.Context) {
	list, err := h.svc.CD.ListInspectionReports(c.Request.Context(), c.Query("envKey"), caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list})
}

// getInspectionReport GET /inspection-reports/{id}
//
// 回 HTML 正文，供详情页直接渲染（前端用 iframe 或 dangerouslySetInnerHTML）。
func (h *handler) getInspectionReport(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	rep, err := h.svc.CD.InspectionReportDetail(c.Request.Context(), id, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{
		"id": fmt.Sprintf("%d", rep.ID), "envKey": rep.EnvKey, "envName": rep.EnvName,
		"score": rep.Score, "level": rep.Level, "summary": rep.Summary,
		"createdAt": rep.CreatedAt.Format("2006-01-02 15:04:05"),
		"html":      rep.HTML,
	})
}

// downloadInspectionReport GET /inspection-reports/{id}/download
//
// 文件名同时给 ASCII 与 RFC 5987 两种写法：
// 只给中文名在老浏览器上会变成一串问号，只给英文名又没法一眼看出是哪台环境。
func (h *handler) downloadInspectionReport(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	rep, err := h.svc.CD.InspectionReportDetail(c.Request.Context(), id, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	ascii := fmt.Sprintf("inspection-%s-%s.html", rep.EnvKey, rep.CreatedAt.Format("20060102-1504"))
	pretty := fmt.Sprintf("%s 巡检报告 %s.html", rep.EnvName, rep.CreatedAt.Format("2006-01-02 1504"))
	c.Header("Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, url.PathEscape(pretty)))
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.String(200, "%s", rep.HTML)
}
