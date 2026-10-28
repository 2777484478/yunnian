package api

import (
	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/service"
)

// --------------------------------------------------------------------------- 项目 k8s 资源定义
//
// 挂在 `/projects/{key}/manifests` 下，与 `/projects/{key}/builds`、`/images`
// 同一族：都是"某个项目的某类子资源"。
//
// 这一期只有增删改查，**没有任何写集群的入口** —— 详见 service/manifest.go 文件头的说明。

// pathManifestID 解析路径里的资源定义 id。
//
// 单拎出来是为了让四个 handler 的错误处理一致：id 不合法时统一 400，
// 而不是有的地方当成 0 去查库、最后报一个"资源定义不存在"这种答非所问的错。
func pathManifestID(c *gin.Context) (uint64, bool) {
	id, err := httpx.ParseIDString(httpx.PathString(c, "id"))
	if err != nil {
		httpx.Fail(c, err)
		return 0, false
	}
	return id, true
}

// listProjectManifests GET /projects/{key}/manifests?envKey=&kind=
//
// 不分页：一个项目在一个环境里通常 5~10 份资源，三个环境也就几十条。
// 分页会让"按环境看全貌"多一次翻页，得不偿失。
func (h *handler) listProjectManifests(c *gin.Context) {
	list, err := h.svc.CI.ListProjectManifests(c.Request.Context(), httpx.PathString(c, "key"),
		service.ManifestQuery{EnvKey: c.Query("envKey"), Kind: c.Query("kind")})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, list)
}

// getProjectManifest GET /projects/{key}/manifests/{id}
func (h *handler) getProjectManifest(c *gin.Context) {
	id, ok := pathManifestID(c)
	if !ok {
		return
	}
	dto, err := h.svc.CI.GetProjectManifest(c.Request.Context(), httpx.PathString(c, "key"), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// createProjectManifest POST /projects/{key}/manifests
func (h *handler) createProjectManifest(c *gin.Context) {
	var in service.ManifestInput
	if !httpx.Bind(c, &in) {
		return
	}
	dto, err := h.svc.CI.CreateProjectManifest(c.Request.Context(), httpx.PathString(c, "key"), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// updateProjectManifest PUT /projects/{key}/manifests/{id}
func (h *handler) updateProjectManifest(c *gin.Context) {
	id, ok := pathManifestID(c)
	if !ok {
		return
	}
	var in service.ManifestInput
	if !httpx.Bind(c, &in) {
		return
	}
	dto, err := h.svc.CI.UpdateProjectManifest(c.Request.Context(), httpx.PathString(c, "key"), id, in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// importProjectManifests POST /projects/{key}/manifests/import
//
// 收的是**整个文件原文**（可含多个 `---` 文档、也可以是 `kubectl get -o yaml`
// 导出的 `kind: List`），由服务端拆开逐条校验后落库。
//
// 为什么拆分放在后端而不是前端：拆分要和校验用同一套规则
// （什么算一个资源、缺 metadata.name 怎么算），放两处早晚会分家；
// 而且前端拿到的就是一段文本，它也没有必要理解 k8s 的文档结构。
func (h *handler) importProjectManifests(c *gin.Context) {
	var in service.ManifestImportInput
	if !httpx.Bind(c, &in) {
		return
	}
	res, err := h.svc.CI.ImportManifests(c.Request.Context(), httpx.PathString(c, "key"), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

// deleteProjectManifest DELETE /projects/{key}/manifests/{id}
//
// 只删平台里的记录，集群里的资源不动 —— 这一期平台没有写集群的能力。
func (h *handler) deleteProjectManifest(c *gin.Context) {
	id, ok := pathManifestID(c)
	if !ok {
		return
	}
	if err := h.svc.CI.DeleteProjectManifest(c.Request.Context(), httpx.PathString(c, "key"), id, caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"deleted": true})
}
