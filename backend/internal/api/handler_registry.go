package api

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/httpx"
)

// 镜像仓库（Harbor）的**只读**接口。
//
// 只做读：列 tag。推送与删除都不在平台职责内（镜像由各个构建平台自己推），
// 所以这里没有任何写方法 —— 与 integration/harbor_core.go 的纪律一致。

// listRegistryTags GET /registries/harbor/tags?projectKey=&limit=
//
// 给部署向导的「仓库 tag」那一组用：列出某项目对应仓库里最近的若干个 tag。
//
// ⚠️ **读不到时不返回空列表**，而是带 unavailable 说明真因
// （没接仓库 / 找不到对应仓库 / 没权限 / 仓库不通）。空列表会被读成"这仓库没 tag"，
// 那是个会把人引向错误方向的结论，见 service.RegistryTagsDTO 的注释。
//
// limit 缺省 5（用户口径），上限 50。
func (h *handler) listRegistryTags(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	dto, err := h.svc.CD.ListRegistryTags(c.Request.Context(), c.Query("projectKey"), limit)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}
