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
