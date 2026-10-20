package api

import (
	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/httpx"
)

// requireEnvVisible 环境可见性门控。
//
// 为什么光过滤列表不够：`ListEnvironments` 只解决"下拉里看不到"。
// 而 `/environments/:key/services`、`/services/:id` 这类**按标识直连**的入口，
// 只要知道 key（从审计、从旧链接、从别人口述），照样能读到正式区的数据 ——
// 那就不是"看不到"，只是"没摆出来"。
//
// 这里只处理 URL 里**已经带 env key** 的路由（`/environments/:key/...`）。
// 按 service id / deployment id 访问的入口，要反查它属于哪个环境，
// 那一步在各自的服务方法里做（`AssertEnvVisibleByService` 等）。
func (h *handler) requireEnvVisible() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.Param("key")
		if key == "" {
			c.Next()
			return
		}
		if err := h.svc.CD.AssertEnvVisible(c.Request.Context(), key, caller(c)); err != nil {
			httpx.Fail(c, err)
			c.Abort()
			return
		}
		c.Next()
	}
}
