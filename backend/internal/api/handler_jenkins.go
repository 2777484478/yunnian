package api

import (
	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/service"
)

// Jenkins 接入的接口。
//
// 为什么放在 /system/integrations 下而不是 /ci 下：**配置**与**凭据**属于
// 「系统管理 › 第三方集成」，与 GitLab 同一处、同一套权限（user:manage）；
// 而 Job 浏览与触发是日常操作，挂在 CI 页面上用（build:read / build:trigger）。
// 路径跟着资源走，权限跟着动作走——这两件事不要混在一起。
//
// 七个接口分三类权限：
//
//	配置读/写/测   user:manage  —— 响应里带凭据元信息（掩码、账号、到期），不是运行状态
//	Job 浏览       build:read   —— 看得到 job 列表与参数，但什么都不会被改动
//	触发构建       build:trigger—— 这是唯一会真的在 Jenkins 上跑一次流水线的动作
//
// 浏览与触发必须分开：能看 job 列表的人（只读同学、PM）不该顺手把生产流水线跑起来。
// 前端也按同一口径把按钮置灰（见 JenkinsJobList）。

// getJenkinsSettings GET /system/integrations/jenkins
func (h *handler) getJenkinsSettings(c *gin.Context) {
	dto, err := h.svc.System.JenkinsSettings(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// updateJenkinsSettings PUT /system/integrations/jenkins
//
// token 留空表示"不修改"——前端回显的是掩码，拿不到明文。
// 要清空必须显式传 clearToken，两者混用会被拒绝。
func (h *handler) updateJenkinsSettings(c *gin.Context) {
	var in service.JenkinsSettingsInput
	if !httpx.Bind(c, &in) {
		return
	}
	dto, err := h.svc.System.UpdateJenkinsSettings(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// testJenkinsConnection POST /system/integrations/jenkins/test
//
// 允许空请求体：不带参数表示"测已保存的那份配置"。
// 带 baseUrl/username/token 则是"测表单里还没保存的值"，支持先测再存。
// 注意 ok=false 是 200 响应而不是错误码——见 service.TestJenkinsConnection 注释。
func (h *handler) testJenkinsConnection(c *gin.Context) {
	var in service.TestJenkinsInput
	_ = c.ShouldBindJSON(&in)
	res, err := h.svc.System.TestJenkinsConnection(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

// listJenkinsJobs GET /system/integrations/jenkins/jobs?refresh=1
//
// refresh 不改变后端行为（每次都实时去 Jenkins 拉，平台这一层不做缓存），
// 保留它只是为了前端契约稳定，以及将来真需要缓存时有位置可落。
func (h *handler) listJenkinsJobs(c *gin.Context) {
	dto, err := h.svc.CI.ListJenkinsJobs(c.Request.Context(), httpx.QueryBool(c, "refresh", false))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, dto)
}

// jenkinsJobParams GET /system/integrations/jenkins/params?fullName=
//
// 只返回参数定义（纯读 job 配置，不碰远端仓库），适合构建弹窗一打开就调。
func (h *handler) jenkinsJobParams(c *gin.Context) {
	defs, err := h.svc.CI.JenkinsJobParams(c.Request.Context(), c.Query("fullName"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, defs)
}

// jenkinsParamValues GET /system/integrations/jenkins/param-values?fullName=&param=
//
// ⚠️ 这个接口**每次都会让 Jenkins 真的去读一遍远端仓库**（实测 0.6s 起、311 个值），
// 所以只能按需调：用户在构建弹窗里展开某个 GitParameter 时才调。
// 任何"进列表页就批量预取"的写法都会把 Jenkins 拖慢到不可用。
func (h *handler) jenkinsParamValues(c *gin.Context) {
	vals, err := h.svc.CI.JenkinsParamValues(c.Request.Context(), c.Query("fullName"), c.Query("param"))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, vals)
}

// triggerJenkinsBuild POST /system/integrations/jenkins/trigger
//
// ⚠️ 这是唯一会真的在 Jenkins 上触发一次构建的接口（真实执行，不是演习）。
// 返回的是**队列项 id 而不是构建号**：Jenkins 接受请求时只给队列项，
// 构建号要等执行器空闲才出现，由后台任务补齐并回写到构建记录。
func (h *handler) triggerJenkinsBuild(c *gin.Context) {
	var in service.JenkinsTriggerInput
	if !httpx.Bind(c, &in) {
		return
	}
	res, err := h.svc.CI.TriggerJenkinsBuild(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}
