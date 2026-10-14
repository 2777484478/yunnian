package api

import (
	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/service"
)

// Harbor 接入的接口。
//
// 与 Jenkins 同一处、同一套权限（user:manage）：配置与凭据属于
// 「系统管理 › 第三方集成」，不是日常运维动作。响应里带凭据元信息（有没有配、上次识别的账号），
// 所以读也要 user:manage —— 与 Jenkins 的口径一致。
//
// ⚠️ Harbor 有**两条互不相通的通道**（registry 组件走 /v2/、Core 走 /api/ 或 /api/v2.0/，
// 具体哪条路径由 Harbor 版本决定，平台自动识别），测试连接必须分别出结论：
// 把它们合成一句"连接失败"，最常见的那个错（地址根本不是一个 Harbor 入口）就没法定位了。

// getHarborSettings GET /system/integrations/harbor
func (h *handler) getHarborSettings(c *gin.Context) {
	dto, err := h.svc.System.HarborSettings(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// updateHarborSettings PUT /system/integrations/harbor
//
// password 留空表示"不修改"——前端回显的是掩码，拿不到明文。
// 要清空必须显式传 clearPassword，两者混用会被拒绝。
func (h *handler) updateHarborSettings(c *gin.Context) {
	var in service.HarborSettingsInput
	if !httpx.Bind(c, &in) {
		return
	}
	dto, err := h.svc.System.UpdateHarborSettings(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// testHarborConnection POST /system/integrations/harbor/test
//
// 允许空请求体：不带参数表示"测已保存的那份配置"。
// 带地址/账号/密码则是"测表单里还没保存的值"，支持先测再存。
// 注意 ok=false 是 200 响应而不是错误码 —— 见 service.TestHarborConnection 注释。
func (h *handler) testHarborConnection(c *gin.Context) {
	var in service.TestHarborInput
	_ = c.ShouldBindJSON(&in)
	res, err := h.svc.System.TestHarborConnection(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}
