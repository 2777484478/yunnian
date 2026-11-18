package api

import (
	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/service"
)

// listTeams GET /teams
func (h *handler) listTeams(c *gin.Context) {
	list, err := h.svc.System.ListTeams(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list})
}

// createTeam POST /teams
func (h *handler) createTeam(c *gin.Context) {
	var in struct {
		Name        string `json:"name" binding:"required,max=64"`
		Description string `json:"description" binding:"omitempty,max=255"`
		Sort        int    `json:"sort"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		httpx.Fail(c, errValidation("请求参数不合法："+err.Error()))
		return
	}
	t, err := h.svc.System.CreateTeam(c.Request.Context(), in.Name, in.Description, in.Sort)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, t)
}

// updateTeam PATCH /teams/:id
func (h *handler) updateTeam(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	var in service.UpdateTeamInput
	if err := c.ShouldBindJSON(&in); err != nil {
		httpx.Fail(c, errValidation("请求参数不合法："+err.Error()))
		return
	}
	t, err := h.svc.System.UpdateTeam(c.Request.Context(), id, in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, t)
}

// deleteTeam DELETE /teams/:id
func (h *handler) deleteTeam(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	if err := h.svc.System.DeleteTeam(c.Request.Context(), id); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"deleted": true})
}

// --------------------------------------------------------------------------- 团队成员 / 项目

// teamMembers GET /teams/:id/members
func (h *handler) teamMembers(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	list, err := h.svc.System.TeamMembers(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list})
}

// setTeamMembers PUT /teams/:id/members
//
// 覆盖式：传的名单就是最终名单（空数组 = 清空）。
func (h *handler) setTeamMembers(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	var in struct {
		UserIDs []string `json:"userIds"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		httpx.Fail(c, errValidation("请求参数不合法："+err.Error()))
		return
	}
	if err := h.svc.System.SetTeamMembers(c.Request.Context(), id, in.UserIDs, caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	list, err := h.svc.System.TeamMembers(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list})
}

// teamProjects GET /teams/:id/projects
//
// 返回**全部项目**及各自当前归属（不是只返回本团队的）：这块的语义是"迁入"，
// 只列本团队的就没法迁入了。前端按 inTeam 勾选。
func (h *handler) teamProjects(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	list, err := h.svc.System.TeamProjectOptions(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"list": list})
}

// moveTeamProjects PUT /teams/:id/projects
func (h *handler) moveTeamProjects(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	var in struct {
		ProjectKeys []string `json:"projectKeys"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		httpx.Fail(c, errValidation("请求参数不合法："+err.Error()))
		return
	}
	n, err := h.svc.System.MoveProjectsToTeam(c.Request.Context(), id, in.ProjectKeys, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"moved": n})
}
