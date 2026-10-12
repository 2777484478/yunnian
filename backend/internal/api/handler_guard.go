package api

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/middleware"
)

// --------------------------------------------------------------------------- 通用错误

// notFoundPath 统一 404 文案，避免把请求路径原样回显（回显路径在反射型探测里是免费情报）。
func notFoundPath(_ string) error {
	return errs.NotFoundf("接口不存在")
}

// errValidation 构造 40000 参数错误。
//
// 存在的意义是让 handler 文件不必逐个 import errs——
// handler 只需要"报参数错"这一个动作，具体错误码由本包统一决定。
func errValidation(message string) error { return errs.Invalid(message) }

// --------------------------------------------------------------------------- 权限

// requireLogin 校验登录态。未登录时写出 401 并返回 false。
//
// 与 middleware.RequirePerm 的区别：本函数用于 **handler 内部**，
// 在已经绑定请求体、需要拿到资源归属后才能判断权限的场景（如按环境判 deploy:xxx）。
func requireLogin(c *gin.Context) bool {
	if middleware.CurrentUser(c) == nil {
		httpx.Fail(c, errs.Unauthorized("请先登录"))
		return false
	}
	return true
}

// requirePerm 校验当前用户是否持有指定权限点之一。
//
// 权限点写法与 auth.Match 一致：deploy:test 精确、deploy:* 模块通配、
// ...:*:read 只读通配都由 Match 负责展开，这里不做二次解析。
func requirePerm(c *gin.Context, codes ...string) bool {
	u := middleware.CurrentUser(c)
	if u == nil {
		httpx.Fail(c, errs.Unauthorized("请先登录"))
		return false
	}
	if len(codes) == 0 {
		return true
	}
	if !u.HasAnyPerm(codes...) {
		httpx.Fail(c, errs.Newf(errs.CodeForbidden, "缺少权限点：%s", strings.Join(codes, " 或 ")))
		return false
	}
	return true
}

// requireEnvPerm 按「环境」校验权限（deploy:test / deploy:prod …）。
//
// 为什么不能写在路由表上：环境标识在请求体或资源 ID 里，路由中间件拿不到；
// 硬写成 RequirePerm("deploy:test","deploy:prod",...) 又会把"只能发测试区"的人
// 直接放行到正式区——那等于没有这道闸。
func requireEnvPerm(c *gin.Context, action, envKey string) bool {
	if strings.TrimSpace(envKey) == "" {
		httpx.Fail(c, errs.Invalid("缺少环境标识，无法校验环境级权限"))
		return false
	}
	return requirePerm(c, action+":"+envKey)
}

// guardServiceEnv 取服务实例所属环境并校验环境级权限。
//
// 会多查一次服务（真正的变更操作内部还会再查一次）。
// 换来的是一次性拿到准确的 envKey，进而给出「你在正式区没有部署权限」这种
// 能直接定位问题的提示，而不是含糊的 403。服务详情查询走主键，成本可忽略。
func (h *handler) guardServiceEnv(c *gin.Context, action string, id uint64) bool {
	svc, err := h.svc.CD.GetService(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return false
	}
	return requireEnvPerm(c, action, svc.EnvKey)
}

// guardDeployEnv 取部署单所属环境并校验环境级权限。
func (h *handler) guardDeployEnv(c *gin.Context, action string, id uint64) bool {
	d, err := h.svc.CD.GetDeployment(c.Request.Context(), id)
	if err != nil {
		httpx.Fail(c, err)
		return false
	}
	return requireEnvPerm(c, action, d.EnvKey)
}

// --------------------------------------------------------------------------- 参数读取

// pageQuery 读取通用分页参数。sortBy / order 由各 service 的白名单收敛，
// 这里只负责原样透传，不做校验——白名单在 service 侧，避免两处各写一份。
func pageQuery(c *gin.Context) httpx.PageQuery {
	return httpx.PageQuery{
		Page:     httpx.QueryInt(c, "page", 1),
		PageSize: httpx.QueryInt(c, "pageSize", 20),
		SortBy:   c.Query("sortBy"),
		Order:    c.Query("order"),
		Keyword:  c.Query("keyword"),
	}
}

// optBool 读取可空布尔查询参数。区分「没传」与「传了 false」——
// 用 bool 会把"未筛选"误判成"筛选已停用"，列表页就会莫名少一半数据。
func optBool(c *gin.Context, name string) *bool {
	raw := strings.ToLower(strings.TrimSpace(c.Query(name)))
	switch raw {
	case "1", "true", "yes", "on":
		v := true
		return &v
	case "0", "false", "no", "off":
		v := false
		return &v
	default:
		return nil
	}
}

// pathUint 读取数字型路径参数；非法时写出 40000 并返回 false。
func pathUint(c *gin.Context, name string) (uint64, bool) {
	v, err := httpx.PathUint64(c, name)
	if err != nil {
		httpx.Fail(c, err)
		return 0, false
	}
	return v, true
}

// queryUint 读取数字型 Query 参数；缺省或非法时返回 def。
func queryUint(c *gin.Context, name string, def uint64) uint64 {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return def
	}
	return v
}
