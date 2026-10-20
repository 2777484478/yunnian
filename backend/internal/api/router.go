package api

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/auth"
	"github.com/2777484478/yunnian/internal/cache"
	"github.com/2777484478/yunnian/internal/config"
	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/middleware"
	"github.com/2777484478/yunnian/internal/service"
)

// Deps HTTP 层依赖。全部由 cmd/server 装配后注入——api 包自己不读配置、不建连接。
type Deps struct {
	Cfg   *config.Config
	Svc   *service.Service
	JWT   *auth.Manager
	Cache *cache.Cache
	// Sessions 会话二次校验（可为 nil，表示未启用 Redis 的单实例形态）。
	Sessions middleware.SessionChecker
	Log      *slog.Logger
}

// handler 承载全部 HTTP 处理函数所需的依赖。
//
// 用结构体方法而不是包级函数：handler 需要 svc/cache/cfg 三样东西，
// 包级函数就得把它们挂在 `package api` 的全局变量上，
// 那样并发测试与多实例（如将来同时跑管理端与 OpenAPI）都会互相污染。
type handler struct {
	svc   *service.Service
	cache *cache.Cache
	cfg   *config.Config
	log   *slog.Logger
}

// NewRouter 装配 gin 引擎：中间件链 + 路由表 + 静态资源。
func NewRouter(d Deps) *gin.Engine {
	if d.Cfg.IsProd() {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	// 关掉自动重定向：/projects/ 与 /projects 是两个不同语义的地址，
	// 静默 301 会让前端在带 Authorization 头时收到跨域重定向错误。
	r.RedirectTrailingSlash = false

	r.Use(
		middleware.TraceID(),
		middleware.Recovery(d.Log),
		middleware.AccessLog(d.Log),
		middleware.CORS(d.Cfg.App.CORSOrigins),
	)

	h := &handler{svc: d.Svc, cache: d.Cache, cfg: d.Cfg, log: d.Log}

	// 探针与指标挂在根路径（不进 /api/v1）：K8s 探针与 Prometheus 抓取端不认业务前缀。
	r.GET("/healthz", h.health)
	r.GET("/readyz", h.ready)
	r.GET("/metrics", h.metrics)

	// 业务路由统一挂在 basePath 下。
	// Auth 只解析身份、不阻断请求——是否必须登录由每条路由的 RequirePerm 决定，
	// 于是公开接口（登录、SSO 回调、Webhook）与受保护接口可以共用同一条链。
	g := r.Group(d.Cfg.App.BasePath, middleware.Auth(d.JWT, d.Sessions))

	h.registerAuth(g)
	h.registerUsers(g)
	h.registerProjects(g)
	h.registerBuilds(g)
	h.registerImages(g)
	h.registerRegistries(g)
	h.registerEnvironments(g)
	h.registerServices(g)
	h.registerDeployments(g)
	h.registerRollbacks(g)
	h.registerClusters(g)
	h.registerAudit(g)
	h.registerNotify(g)
	h.registerModels(g)
	h.registerAI(g)
	h.registerSystem(g)
	h.registerWebhooks(g)

	h.mountStatic(r)

	return r
}

// --------------------------------------------------------------------------- 各模块路由
//
// 路由表刻意写成「一行一条 + 权限点紧邻路径」：docs/03 的接口清单是按模块排的，
// 这里保持同样的分组与顺序，评审时可以直接左右对照，不用在代码里翻找。

func (h *handler) registerAuth(g *gin.RouterGroup) {
	a := g.Group("/auth")
	// 公开：登录与续期
	a.POST("/login", h.login)
	a.POST("/refresh", h.refresh)
	a.GET("/oidc/authorize", h.oidcAuthorize)
	a.GET("/oidc/callback", h.oidcCallback)
	// 登录态
	a.POST("/logout", middleware.RequirePerm(), h.logout)
	a.GET("/me", middleware.RequirePerm(), h.me)
	// 改自己：不需要 user:manage —— 那几个权限改的是**别人**。
	a.PATCH("/me", middleware.RequirePerm(), h.updateSelf)
	a.PUT("/me/password", middleware.RequirePerm(), h.changeSelfPassword)
	a.POST("/me/avatar", middleware.RequirePerm(), h.uploadSelfAvatar)
}

func (h *handler) registerUsers(g *gin.RouterGroup) {
	// 「选人」名录（环境的负责人 / 审批人下拉）：只回 id/name/username，权限对齐 env:write。
	//
	// 刻意**不复用** `GET /users` —— 那要 `user:manage`，会把真正维护环境的人（ops）
	// 挡在门外：抽屉能打开，下拉却一个候选都没有。
	g.GET("/users/options", middleware.RequirePerm("env:write"), h.listUserOptions)
	g.GET("/users", middleware.RequirePerm("user:manage"), h.listUsers)
	g.POST("/users/invite", middleware.RequirePerm("user:manage"), h.inviteUser)
	g.PATCH("/users/:id", middleware.RequirePerm("user:manage"), h.updateUser)
	g.POST("/users/:id/reset-password", middleware.RequirePerm("user:manage"), h.resetPassword)

	g.GET("/roles", middleware.RequirePerm("user:manage"), h.listRoles)
	// 团队主数据。读用 team:read（ops/dev/qa 都有），写用 team:manage（只有 admin）。
	g.GET("/teams", middleware.RequirePerm("team:read"), h.listTeams)
	g.POST("/teams", middleware.RequirePerm("team:manage"), h.createTeam)
	g.PATCH("/teams/:id", middleware.RequirePerm("team:manage"), h.updateTeam)
	g.DELETE("/teams/:id", middleware.RequirePerm("team:manage"), h.deleteTeam)
	// 权限点清单给"角色权限编辑"页面用，同属角色管理
	g.GET("/permissions", middleware.RequirePerm("role:manage", "user:manage"), h.listPermissions)
	g.PUT("/roles/:code/permissions", middleware.RequirePerm("role:manage"), h.updateRolePermissions)
}

func (h *handler) registerProjects(g *gin.RouterGroup) {
	p := g.Group("/projects")
	p.GET("", middleware.RequirePerm("project:read"), h.listProjects)
	p.POST("", middleware.RequirePerm("project:write"), h.createProject)
	// 两个**静态**路径必须排在 /:key 之前读起来才不别扭。gin 的路由树是
	// 静态优先于参数匹配的，所以 /projects/test-connection 不会被当成
	// key=test-connection —— 但这一点靠的是框架行为，写在注释里免得后人误改。
	p.GET("/default-build-script", middleware.RequirePerm("project:read"), h.defaultBuildScript)
	p.POST("/test-connection", middleware.RequirePerm("project:write"), h.testGitConnection)
	p.GET("/:key", middleware.RequirePerm("project:read"), h.getProject)
	p.PUT("/:key", middleware.RequirePerm("project:write", "project:write:own"), h.updateProject)
	p.DELETE("/:key", middleware.RequirePerm("project:delete"), h.deleteProject)
	p.PATCH("/:key/enabled", middleware.RequirePerm("project:write", "project:write:own"), h.setProjectEnabled)
	p.GET("/:key/branches", middleware.RequirePerm("project:read"), h.projectBranches)
	p.POST("/:key/validate", middleware.RequirePerm("project:write"), h.validateProject)
	p.GET("/:key/builds", middleware.RequirePerm("build:read"), h.listProjectBuilds)
	p.GET("/:key/images", middleware.RequirePerm("image:read"), h.listProjectImages)
	p.GET("/:key/deployments", middleware.RequirePerm("deploy:read"), h.listProjectDeployments)
}

func (h *handler) registerBuilds(g *gin.RouterGroup) {
	b := g.Group("/builds")
	b.POST("", middleware.RequirePerm("build:trigger"), h.triggerBuild)
	b.GET("", middleware.RequirePerm("build:read"), h.listBuilds)
	b.GET("/:id", middleware.RequirePerm("build:read"), h.getBuild)
	b.GET("/:id/logs", middleware.RequirePerm("log:read"), h.buildLogs)
	b.GET("/:id/logs/stream", middleware.RequirePerm("log:read"), h.buildLogStream)
	b.POST("/:id/cancel", middleware.RequirePerm("build:cancel"), h.cancelBuild)
	b.POST("/:id/retry", middleware.RequirePerm("build:trigger"), h.retryBuild)
	b.GET("/:id/ai-diagnosis", middleware.RequirePerm("ai:query"), h.buildAIDiagnosis)
}

func (h *handler) registerImages(g *gin.RouterGroup) {
	i := g.Group("/images")
	// ⚠️ 静态段先注册：/images/harbor 若在 /images/:id 之后，会被 :id 当成 "harbor" 吃掉。
	i.GET("/harbor", middleware.RequirePerm("image:read"), h.listHarborTags)
	i.GET("/harbor/repos", middleware.RequirePerm("image:read"), h.listHarborRepos)
	i.GET("/harbor/projects", middleware.RequirePerm("image:read"), h.listHarborProjects)
	i.GET("/harbor/meta", middleware.RequirePerm("image:read"), h.getHarborMeta)
	i.GET("", middleware.RequirePerm("image:read"), h.listImages)
	i.POST("/cleanup", middleware.RequirePerm("image:delete"), h.cleanupImages)
	// 注意：静态段 /cleanup 必须在 /:id 之前注册，否则会被当成 id=cleanup。
	i.GET("/:id", middleware.RequirePerm("image:read"), h.getImage)
	i.GET("/:id/used-by", middleware.RequirePerm("image:read"), h.imageUsedBy)
	i.DELETE("/:id", middleware.RequirePerm("image:delete"), h.deleteImage)
}

// registerRegistries 镜像仓库（Harbor）的只读接口。
//
// 权限刻意是 **image:read 或 deploy:read 二者之一**（RequirePerm 是"任一满足"）：
// 用它的地方是**部署向导**里的「仓库 tag」选择器，而发版的人不一定有 image:read
// （镜像版本页是另一套权限）。只给 image:read 会让"能发版却选不了版本"。
func (h *handler) registerRegistries(g *gin.RouterGroup) {
	r := g.Group("/registries/harbor")
	r.GET("/tags", middleware.RequirePerm("image:read", "deploy:read"), h.listRegistryTags)
}

func (h *handler) registerEnvironments(g *gin.RouterGroup) {
	e := g.Group("/environments")
	e.GET("", middleware.RequirePerm("env:read"), h.listEnvironments)
	e.POST("", middleware.RequirePerm("env:write"), h.createEnvironment)
	e.GET("/:key", middleware.RequirePerm("env:read"), h.getEnvironment)
	e.PUT("/:key", middleware.RequirePerm("env:write"), h.updateEnvironment)
	e.DELETE("/:key", middleware.RequirePerm("env:write"), h.deleteEnvironment)
	e.GET("/:key/project-candidates", middleware.RequirePerm("env:read"), h.envProjectCandidates)
	e.PUT("/:key/projects", middleware.RequirePerm("env:write"), h.envSetProjects)
	e.GET("/:key/vars", middleware.RequirePerm("env:read"), h.envVars)
	e.PUT("/:key/vars", middleware.RequirePerm("env:write"), h.saveEnvVars)
	e.GET("/:key/services", middleware.RequirePerm("deploy:read"), h.envServices)
	e.GET("/:key/resources", middleware.RequirePerm("env:read"), h.envResources)
	e.POST("/:key/sync", middleware.RequirePerm("env:write"), h.syncEnvironment)
	e.GET("/:key/events", middleware.RequirePerm("env:read"), h.envEvents)
	e.POST("/:key/test-connection", middleware.RequirePerm("env:write"), h.testEnvConnection)
}

func (h *handler) registerServices(g *gin.RouterGroup) {
	s := g.Group("/services")
	s.GET("/:id", middleware.RequirePerm("deploy:read"), h.getService)
	s.GET("/:id/pods", middleware.RequirePerm("deploy:read"), h.servicePods)
	s.GET("/:id/versions", middleware.RequirePerm("rollback:read"), h.serviceVersions)
	s.GET("/:id/params/history", middleware.RequirePerm("deploy:read"), h.serviceParamHistory)
	s.GET("/:id/pods/:podName/logs/stream", middleware.RequirePerm("log:read"), h.podLogStream)

	// 变更类：权限点按环境动态判定（deploy:test / deploy:prod），
	// 所以这里只要求登录，真正的校验在 handler 里做（见 guardServiceEnv）。
	s.PUT("/:id/params", middleware.RequirePerm(), h.updateServiceParams)
	s.POST("/:id/scale", middleware.RequirePerm(), h.scaleService)
	s.POST("/:id/restart", middleware.RequirePerm(), h.restartService)
	s.POST("/:id/redeploy", middleware.RequirePerm(), h.redeployService)
	s.DELETE("/:id", middleware.RequirePerm(), h.deleteService)
}

func (h *handler) registerDeployments(g *gin.RouterGroup) {
	d := g.Group("/deployments")
	// 创建部署单的权限点是 deploy:{env}，环境在请求体里，只能进 handler 后判（见 requireEnvPerm）。
	d.POST("", middleware.RequirePerm(), h.createDeployment)
	d.GET("", middleware.RequirePerm("deploy:read"), h.listDeployments)
	// 静态段先注册（同 images/cleanup）
	d.GET("/pending-approval", middleware.RequirePerm("deploy:approve"), h.pendingApprovals)
	// ⚠️ 静态段先于 /:id 注册，否则 "risk" 会被当成 id。
	d.GET("/risk", middleware.RequirePerm("deploy:read"), h.deployRisk)
	d.GET("/:id", middleware.RequirePerm("deploy:read"), h.getDeployment)
	d.GET("/:id/logs", middleware.RequirePerm("log:read"), h.deploymentLogs)
	d.GET("/:id/logs/stream", middleware.RequirePerm("log:read"), h.deploymentLogStream)
	d.POST("/:id/approve", middleware.RequirePerm("deploy:approve"), h.approveDeployment)
	d.POST("/:id/reject", middleware.RequirePerm("deploy:approve"), h.rejectDeployment)
	d.POST("/:id/cancel", middleware.RequirePerm(), h.cancelDeployment)
	d.POST("/:id/retry", middleware.RequirePerm(), h.retryDeployment)
	d.GET("/:id/ai-diagnosis", middleware.RequirePerm("ai:query"), h.deploymentAIDiagnosis)
}

func (h *handler) registerRollbacks(g *gin.RouterGroup) {
	r := g.Group("/rollbacks")
	r.POST("", middleware.RequirePerm("rollback:execute"), h.createRollback)
	r.GET("", middleware.RequirePerm("rollback:read"), h.listRollbacks)
	r.GET("/:id", middleware.RequirePerm("rollback:read"), h.getRollback)
	r.GET("/:id/logs/stream", middleware.RequirePerm("log:read"), h.rollbackLogStream)
}

func (h *handler) registerAudit(g *gin.RouterGroup) {
	a := g.Group("/audit-logs")
	a.GET("", middleware.RequirePerm("audit:read"), h.listAuditLogs)
	// 静态段先注册
	a.POST("/export", middleware.RequirePerm("audit:read"), h.exportAuditLogs)
	a.GET("/export/:taskId", middleware.RequirePerm("audit:read"), h.auditExportResult)
	a.GET("/:id", middleware.RequirePerm("audit:read"), h.getAuditLog)
}

func (h *handler) registerNotify(g *gin.RouterGroup) {
	n := g.Group("/notify-channels", middleware.RequirePerm("notify:manage"))
	n.GET("", h.listNotifyChannels)
	n.POST("", h.createNotifyChannel)
	n.PUT("/:id", h.updateNotifyChannel)
	n.DELETE("/:id", h.deleteNotifyChannel)
	n.POST("/:id/test", h.testNotifyChannel)
	n.PATCH("/:id/enabled", h.setNotifyEnabled)
}

func (h *handler) registerModels(g *gin.RouterGroup) {
	// 模型通道属于「系统管理」下的平台能力配置，权限点复用 user:manage。
	// 路径同时注册 /models 与 /model-providers：前者与前端导航（/system/models）对齐，
	// 后者语义更完整，两边都指向同一组 handler，避免前端改路径时后端要跟着改。
	for _, path := range []string{"/models", "/model-providers"} {
		m := g.Group(path, middleware.RequirePerm("user:manage"))
		m.GET("", h.listModelProviders)
		m.POST("", h.createModelProvider)
		m.PUT("/:id", h.updateModelProvider)
		m.DELETE("/:id", h.deleteModelProvider)
		m.POST("/:id/test", h.testModelProvider)
		m.POST("/:id/default", h.setDefaultModelProvider)
	}
}

func (h *handler) registerAI(g *gin.RouterGroup) {
	a := g.Group("/ai", middleware.RequirePerm("ai:query"))
	a.POST("/chat", h.aiChat)
	a.GET("/conversations", h.aiConversations)
	a.GET("/conversations/:id", h.aiConversation)
	a.DELETE("/conversations/:id", h.aiDeleteConversation)
	a.POST("/analyze-build/:buildId", h.aiAnalyzeBuild)
	a.POST("/analyze-deploy/:deployId", h.aiAnalyzeDeploy)
}

func (h *handler) registerSystem(g *gin.RouterGroup) {
	s := g.Group("/system")
	s.GET("/stats", middleware.RequirePerm(), h.systemStats)
	s.GET("/integrations", middleware.RequirePerm(), h.systemIntegrations)

	// GitLab 接入配置：读写都要求 user:manage。
	// 读也收紧权限，是因为响应里带着令牌掩码、令牌账号与到期时间——
	// 那是凭据的元信息，不是普通运行状态，不该让所有人都能看。
	s.GET("/integrations/gitlab", middleware.RequirePerm("user:manage"), h.getGitLabSettings)
	s.PUT("/integrations/gitlab", middleware.RequirePerm("user:manage"), h.updateGitLabSettings)
	s.POST("/integrations/gitlab/test", middleware.RequirePerm("user:manage"), h.testGitLabConnection)

	// Jenkins 接入。权限分三档，因为三类动作的风险差别很大：
	//   - 配置读/写/测要 user:manage：响应里带着凭据元信息，不是普通运行状态；
	//   - Job 浏览只要 build:read：看列表与参数不会改动任何东西；
	//   - 触发构建要 build:trigger：这是唯一会真的在 Jenkins 上跑一次流水线的动作，
	//     "能看"与"能跑"必须分开，否则只读同学也能把生产流水线点起来。
	s.GET("/integrations/jenkins", middleware.RequirePerm("user:manage"), h.getJenkinsSettings)
	s.PUT("/integrations/jenkins", middleware.RequirePerm("user:manage"), h.updateJenkinsSettings)
	s.POST("/integrations/jenkins/test", middleware.RequirePerm("user:manage"), h.testJenkinsConnection)
	// Harbor 接入：与 Jenkins 同一套权限（配置 + 凭据，属「第三方集成」页维护）。
	// 两条通道（registry /v2/ 与 Core /api/v2.0/）在测试接口里分别出结论。
	s.GET("/integrations/harbor", middleware.RequirePerm("user:manage"), h.getHarborSettings)
	s.PUT("/integrations/harbor", middleware.RequirePerm("user:manage"), h.updateHarborSettings)
	s.POST("/integrations/harbor/test", middleware.RequirePerm("user:manage"), h.testHarborConnection)
	s.GET("/integrations/jenkins/jobs", middleware.RequirePerm("build:read"), h.listJenkinsJobs)
	s.GET("/integrations/jenkins/params", middleware.RequirePerm("build:read"), h.jenkinsJobParams)
	s.GET("/integrations/jenkins/param-values", middleware.RequirePerm("build:read"), h.jenkinsParamValues)
	s.POST("/integrations/jenkins/trigger", middleware.RequirePerm("build:trigger"), h.triggerJenkinsBuild)

	s.GET("/config", middleware.RequirePerm("user:manage"), h.listConfig)
	s.PUT("/config", middleware.RequirePerm("user:manage"), h.updateConfig)

	// 运行参数（并发上限）的生效值。读只要 deploy:read 或 user:manage：
	// 批量发布页要拿它决定一次提交几个，而发版的人不一定有 user:manage。
	// 写仍然走 /system/config（user:manage），不在这里另开一个写接口 ——
	// 同一个值两个写入口，迟早出现"从哪个页面改的"说不清。
	s.GET("/runtime", middleware.RequirePerm("deploy:read", "user:manage"), h.getRuntimeSettings)
}

func (h *handler) registerClusters(g *gin.RouterGroup) {
	// 集群接入。权限分两档，理由见 handler_cluster.go 文件头。
	//
	// 注意 `/clusters/parse` 这个静态段必须**在 `/:key` 之前**注册，
	// 否则 /clusters/parse 会被当成 key=parse 命中 PUT/DELETE 那条路由
	// （images/cleanup、deployments/pending-approval 都有同样的要求）。
	cl := g.Group("/clusters")
	cl.GET("", middleware.RequirePerm("env:read", "env:write"), h.listClusters)
	cl.POST("/parse", middleware.RequirePerm("user:manage"), h.parseClusterKubeconfig)
	cl.POST("", middleware.RequirePerm("user:manage"), h.createClusters)
	cl.PUT("/:key", middleware.RequirePerm("user:manage"), h.updateCluster)
	cl.DELETE("/:key", middleware.RequirePerm("user:manage"), h.deleteCluster)
	cl.POST("/:key/test", middleware.RequirePerm("user:manage"), h.testCluster)
	cl.GET("/:key/namespaces", middleware.RequirePerm("env:read", "env:write"), h.clusterNamespaces)
}

// registerWebhooks GitLab / GitHub push 回调。
//
// 这两个接口不带 JWT（调用方是代码托管平台），改用共享密钥签名校验，
// 所以它们只挂 Auth 中间件（不阻断）而不加 RequirePerm。
func (h *handler) registerWebhooks(g *gin.RouterGroup) {
	w := g.Group("/webhooks")
	w.POST("/gitlab", h.webhookGitLab)
	w.POST("/github", h.webhookGitHub)
}

// mountStatic 托管前端单文件产物。
//
// 静态目录为空则完全不挂载——生产上一般由 Nginx 直接托管前端，
// 平台只提供 /api/v1，硬塞一个空的静态路由只会让 404 更难排查。
func (h *handler) mountStatic(r *gin.Engine) {
	dir := h.cfg.App.StaticDir
	if dir == "" {
		return
	}
	r.Static("/assets", dir+"/assets")
	// 用户头像。独立于前端产物目录 —— 那里每次构建都会被整体替换。
	// 相对运行目录（backend/），实际位置 backend/data/uploads。
	r.Static("/uploads", "data/uploads")
	r.StaticFile("/", dir+"/index.html")
	// 浏览器无条件请求 /favicon.ico，而前端产物里是 favicon.svg（矢量、带品牌色）。
	// 两个路径都注册：不注册 .ico 会在每次访问时留下一条无意义的 404。
	r.StaticFile("/favicon.ico", dir+"/favicon.ico")
	r.StaticFile("/favicon.svg", dir+"/favicon.svg")
	r.NoRoute(func(c *gin.Context) {
		// 单页应用：非 API 路径一律回 index.html，交给前端路由处理。
		if len(c.Request.URL.Path) >= len(h.cfg.App.BasePath) &&
			c.Request.URL.Path[:len(h.cfg.App.BasePath)] == h.cfg.App.BasePath {
			httpx.Fail(c, notFoundPath(c.Request.URL.Path))
			return
		}
		c.File(dir + "/index.html")
	})
}

// metrics 暴露一份精简的 Prometheus 文本指标。
//
// 不引第三方 prometheus client：一期需要的是「进程活着没、队列堵没堵」这两件事，
// 用运行时自身的数据就够了，多一个依赖就多一份版本维护成本。
func (h *handler) metrics(c *gin.Context) {
	body := h.svc.System.MetricsText()
	c.Data(http.StatusOK, "text/plain; version=0.0.4; charset=utf-8", []byte(body))
}
