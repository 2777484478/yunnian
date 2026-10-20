package api

import (
	"encoding/base64"
	"strings"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/middleware"
	"github.com/2777484478/yunnian/internal/service"
)

// --------------------------------------------------------------------------- 认证

// login POST /auth/login
//
// 登录成功返回 access/refresh 双令牌 + 用户信息与权限点。
// 权限点在登录时算好并下发，前端据此隐藏无权入口——
// 注意这只是体验优化，真正的准入仍在服务端（每个接口都带权限点）。
func (h *handler) login(c *gin.Context) {
	var in service.LoginInput
	if !httpx.Bind(c, &in) {
		return
	}
	res, err := h.svc.Auth.Login(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

// refresh POST /auth/refresh
//
// 用刷新令牌换一对新令牌。旧会话不会被吊销（同一账号多端登录互不影响），
// 需要"踢掉全部设备"时走管理员改角色/停用，那条路径会批量清会话键。
func (h *handler) refresh(c *gin.Context) {
	var in service.RefreshInput
	if !httpx.Bind(c, &in) {
		return
	}
	res, err := h.svc.Auth.Refresh(c.Request.Context(), in)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

// logout POST /auth/logout
//
// 吊销「当前这一个」令牌对应的会话（按 jti）。这是 JWT 方案下必须补的一环：
// 签名有效的令牌本身无法作废，只能靠服务端的会话键让它立刻失效。
func (h *handler) logout(c *gin.Context) {
	if err := h.svc.Auth.Logout(c.Request.Context(), caller(c), middleware.TokenID(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"loggedOut": true})
}

// me GET /auth/me
func (h *handler) me(c *gin.Context) {
	u, err := h.svc.Auth.Me(c.Request.Context(), caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, u)
}

// updateSelf PATCH /auth/me
//
// 改**自己**的基本信息。刻意不走 /users/:id（那个要 user:manage）：
// 每个人都能改自己的姓名/邮箱/电话/头像，但改不了角色与启用状态。
func (h *handler) updateSelf(c *gin.Context) {
	var in service.UpdateSelfInput
	if err := c.ShouldBindJSON(&in); err != nil {
		httpx.Fail(c, errValidation("请求参数不合法："+err.Error()))
		return
	}
	u, err := h.svc.Auth.UpdateSelf(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	// 回最新的 /auth/me —— 前端拿到就能直接替换全局用户态，不必再请求一次。
	me, err := h.svc.Auth.Me(c.Request.Context(), caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	_ = u
	httpx.OK(c, me)
}

// changeSelfPassword PUT /auth/me/password
func (h *handler) changeSelfPassword(c *gin.Context) {
	var in struct {
		OldPassword string `json:"oldPassword" binding:"required"`
		NewPassword string `json:"newPassword" binding:"required,min=8,max=72"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		httpx.Fail(c, errValidation("请求参数不合法："+err.Error()))
		return
	}
	if err := h.svc.Auth.ChangeSelfPassword(c.Request.Context(), in.OldPassword, in.NewPassword, caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"changed": true})
}

// uploadSelfAvatar POST /auth/me/avatar
//
// 收 **base64 JSON**（`{fileName, data}`）而不是 multipart：
// 前端那一层 http 封装固定发 JSON，走 multipart 要绕开它（连带的令牌与错误处理都会丢）。
// 头像本身 ≤1MB，base64 膨胀 33% 也在可接受范围。
func (h *handler) uploadSelfAvatar(c *gin.Context) {
	var in struct {
		FileName string `json:"fileName" binding:"required"`
		Data     string `json:"data" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		httpx.Fail(c, errValidation("请求参数不合法："+err.Error()))
		return
	}
	raw := in.Data
	// 允许前端直接塞 `data:image/png;base64,xxx`（FileReader 的原样输出）。
	if i := strings.Index(raw, "base64,"); i >= 0 {
		raw = raw[i+len("base64,"):]
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		httpx.Fail(c, errValidation("头像数据不是合法的 base64"))
		return
	}
	url, err := h.svc.Auth.SaveSelfAvatar(c.Request.Context(), in.FileName, data, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"avatar": url})
}

// oidcAuthorize GET /auth/oidc/authorize
//
// 返回跳转地址而不是 302：前端需要把 state 存起来做校验，
// 直接重定向会让前端拿不到 state 而无法比对回调结果。
func (h *handler) oidcAuthorize(c *gin.Context) {
	url, err := h.svc.Auth.OIDCAuthorize(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"authorizeUrl": url})
}

// oidcCallback GET /auth/oidc/callback?code=xxx&state=xxx
func (h *handler) oidcCallback(c *gin.Context) {
	code := c.Query("code")
	if code == "" {
		httpx.Fail(c, errValidation("缺少 code 参数"))
		return
	}
	res, err := h.svc.Auth.OIDCCallback(c.Request.Context(), code)
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, res)
}

// --------------------------------------------------------------------------- 用户与角色

// listUsers GET /users
func (h *handler) listUsers(c *gin.Context) {
	q := pageQuery(c)
	pq, list, total, err := h.svc.System.ListUsers(c.Request.Context(), service.UserQuery{
		Keyword:  q.Keyword,
		Role:     c.Query("role"),
		Enabled:  optBool(c, "enabled"),
		Page:     q.Page,
		PageSize: q.PageSize,
	})
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, httpx.NewPageData(list, total, *pq))
}

// listUserOptions GET /users/options
//
// 「选人」下拉专用：只回 id / name / username，权限对齐 `env:write`。
//
// 与 `GET /users` 分开是刻意的 —— 那个接口带着角色、邮箱、启停状态，属用户管理，要 `user:manage`；
// 把它的读权限放宽等于把全量用户名单开放给环境维护者。这里只放行"选人"所需的最小集合。
func (h *handler) listUserOptions(c *gin.Context) {
	list, err := h.svc.System.ListUserOptions(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, list)
}

// inviteUser POST /users/invite
func (h *handler) inviteUser(c *gin.Context) {
	var in service.InviteUserInput
	if !httpx.Bind(c, &in) {
		return
	}
	u, err := h.svc.System.InviteUser(c.Request.Context(), in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	c.JSON(http.StatusCreated, httpx.Body{Code: 0, Message: "用户已创建", Data: u, TraceID: httpx.TraceID(c)})
}

// updateUser PATCH /users/{id}
func (h *handler) updateUser(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	var in service.UpdateUserInput
	if !httpx.Bind(c, &in) {
		return
	}
	u, err := h.svc.System.UpdateUser(c.Request.Context(), id, in, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, u)
}

// resetPassword POST /users/{id}/reset-password
//
// 只接受"管理员指定新密码"，不生成随机密码返回明文——
// 随机密码在浏览器/日志/聊天记录里传播一圈之后，等于把账号交出去。
func (h *handler) resetPassword(c *gin.Context) {
	id, ok := pathUint(c, "id")
	if !ok {
		return
	}
	var in struct {
		Password string `json:"password" binding:"required,min=8,max=128"`
	}
	if !httpx.Bind(c, &in) {
		return
	}
	if _, err := h.svc.System.ResetPassword(c.Request.Context(), id, in.Password, caller(c)); err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, gin.H{"reset": true})
}

// listRoles GET /roles
func (h *handler) listRoles(c *gin.Context) {
	roles, err := h.svc.System.ListRoles(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, roles)
}

// listPermissions GET /permissions
func (h *handler) listPermissions(c *gin.Context) {
	perms, err := h.svc.System.ListPermissions(c.Request.Context())
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, perms)
}

// updateRolePermissions PUT /roles/{code}/permissions
//
// allowEmpty 由 ?confirmEmpty=true 表达：把角色权限清空是危险操作
// （该角色下的用户会瞬间失去所有菜单），所以要求调用方显式确认，
// 而不是在服务端猜"用户是不是手滑了"。
func (h *handler) updateRolePermissions(c *gin.Context) {
	code := httpx.PathString(c, "code")
	if code == "" {
		httpx.Fail(c, errValidation("缺少角色标识"))
		return
	}
	var in service.UpdateRolePermissionsInput
	if !httpx.Bind(c, &in) {
		return
	}
	allowEmpty := httpx.QueryBool(c, "confirmEmpty", false)
	role, err := h.svc.System.UpdateRolePermissions(c.Request.Context(), code, in, allowEmpty, caller(c))
	if err != nil {
		httpx.Fail(c, err)
		return
	}
	httpx.OK(c, role)
}
