package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/yunnian/yunnian/internal/auth"
	"github.com/yunnian/yunnian/internal/errs"
	"github.com/yunnian/yunnian/internal/httpx"
	"github.com/yunnian/yunnian/internal/model"
)

// AuthService 认证与会话。
//
// 请求体结构体（LoginInput / RefreshInput …）直接定义在本包并带 binding 标签，
// 由 handler 直接 Bind。这是刻意的取舍：多一层 handler 专用 DTO 会让
//「一个字段改两处」成为常态，而对内的 service 入参与对外契约本来就同一个东西。
type AuthService struct {
	*base
}

// LoginInput 账号密码登录入参。
type LoginInput struct {
	Username string `json:"username" binding:"required,min=3,max=64"`
	Password string `json:"password" binding:"required,min=6,max=128"`
}

// RefreshInput 刷新令牌入参。
type RefreshInput struct {
	RefreshToken string `json:"refreshToken" binding:"required"`
}

// principal 身份聚合：用户本体 + 角色 + 权限点 + 团队。
//
// 权限点从 user_role → role → role_permission → permission 四级关联取，
// 每次登录/刷新时重新算一遍，保证管理员刚改的权限立即生效。
type principal struct {
	User     *model.User
	RoleCode string
	RoleName string
	Perms    []string
	Teams    []string
}

// Login 账号密码登录。
//
// 失败处理有两个容易做错的点，这里都显式处理了：
//  1. **不做账号枚举**：用户不存在与密码错误返回完全相同的提示，
//     否则攻击者可以靠错误文案差异枚举出有效账号。
//  2. **失败也要审计**：暴力破解的痕迹必须留在审计表里，否则事后无法追溯。
func (a *AuthService) Login(ctx context.Context, in LoginInput, c Caller) (*LoginResult, error) {
	username := strings.TrimSpace(in.Username)
	started := now()

	// 限流检查放在密码比对之前：既省 CPU（bcrypt 很贵），也避免被当作撞库跳板。
	if limit := a.deps.Cfg.Auth.LoginFailLimit; limit > 0 {
		if n := a.deps.Cache.LoginFailCount(ctx, username); n >= int64(limit) {
			a.audit(ctx, c, auditInput{
				Module: "认证", Action: "login", TargetType: "user", TargetName: username,
				Detail: "登录失败：失败次数超限，已临时锁定", Result: "failed",
				ErrMsg: "触发登录限流", StartedAt: started,
			})
			return nil, errs.RateLimited("失败次数过多，请稍后再试")
		}
	}

	var user model.User
	err := a.db(ctx).Where("username = ?", username).First(&user).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			a.recordLoginFailure(ctx, c, username, "账号或密码错误", started)
			return nil, errs.Unauthorized("账号或密码错误")
		}
		return nil, errs.Internalf(err, "查询用户失败")
	}

	if !user.Enabled {
		a.recordLoginFailure(ctx, c, username, "账号已停用", started)
		return nil, errs.Forbidden("账号已停用，请联系管理员")
	}

	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(in.Password)) != nil {
		a.recordLoginFailure(ctx, c, username, "账号或密码错误", started)
		return nil, errs.Unauthorized("账号或密码错误")
	}

	p, err := a.loadPrincipal(ctx, &user)
	if err != nil {
		return nil, err
	}

	result, err := a.issuePair(ctx, p)
	if err != nil {
		return nil, err
	}

	// 登录成功后的善后：清限流、记登录信息。
	// 这三步失败都不该让用户登录失败，所以只记日志。
	_ = a.deps.Cache.ResetLoginFail(ctx, username)
	ip := c.IP
	at := now()
	if err := a.db(ctx).Model(&model.User{}).Where("id = ?", user.ID).
		Updates(map[string]any{"last_login_at": at, "last_login_ip": ip}).Error; err != nil {
		a.log("auth").Warn("更新登录信息失败", "userId", user.ID, "err", err)
	}

	a.audit(ctx, c, auditInput{
		Module: "认证", Action: "login", TargetType: "user",
		TargetID: &user.ID, TargetName: user.Name,
		Detail: "登录成功", Result: "success", StartedAt: started,
	})
	return result, nil
}

// recordLoginFailure 记录一次登录失败：累加限流计数 + 写审计。
func (a *AuthService) recordLoginFailure(ctx context.Context, c Caller, username, reason string, started time.Time) {
	window := a.deps.Cfg.Auth.LoginFailWindow.D()
	if window <= 0 {
		window = 5 * time.Minute
	}
	if _, err := a.deps.Cache.IncLoginFail(ctx, username, window); err != nil {
		a.log("auth").Warn("登录失败计数写入失败", "username", username, "err", err)
	}
	a.audit(ctx, c, auditInput{
		Module: "认证", Action: "login", TargetType: "user", TargetName: username,
		Detail: "登录失败：" + reason, Result: "failed", ErrMsg: reason, StartedAt: started,
	})
}

// Me 返回当前用户完整信息（含权限点）。
//
// 不从令牌里取：令牌里的权限点是签发时的快照，而 /auth/me 是前端刷新页面后
// 重建界面的入口，必须给最新值。多一次数据库查询换来"改权限后刷新即生效"。
func (a *AuthService) Me(ctx context.Context, c Caller) (*CurrentUserDTO, error) {
	if c.User == nil {
		return nil, errs.Unauthorized("请先登录")
	}
	var user model.User
	if err := firstOrNotFound(a.db(ctx), c.User.ID, &user, "用户"); err != nil {
		return nil, err
	}
	if !user.Enabled {
		return nil, errs.Forbidden("账号已停用，请联系管理员")
	}
	// 这里刻意不用 c.User.ID 直接查：令牌里的 ID 一定存在，
	// 但如果账号被删了，First 会命中 404，语义比"返回空用户"正确。
	if user.ID != c.User.ID {
		return nil, errs.Unauthorized("登录状态已失效，请重新登录")
	}

	p, err := a.loadPrincipal(ctx, &user)
	if err != nil {
		return nil, err
	}
	dto := a.toCurrentUserDTO(p)
	return &dto, nil
}

// Refresh 用刷新令牌换一对新令牌。
//
// 刻意**重新读一遍用户与权限**而不是复用旧令牌里的快照：
// 刷新令牌有效期 14 天，如果中间管理员降了权，继续沿用旧快照就等于权限没改。
func (a *AuthService) Refresh(ctx context.Context, in RefreshInput) (*LoginResult, error) {
	claims, err := a.deps.JWT.Parse(strings.TrimSpace(in.RefreshToken))
	if err != nil {
		return nil, err
	}
	if claims.Type != auth.TokenRefresh {
		return nil, errs.Unauthorized("请使用刷新令牌续期")
	}

	var user model.User
	if err := a.db(ctx).First(&user, claims.UserID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.Unauthorized("用户不存在或已被删除")
		}
		return nil, errs.Internalf(err, "查询用户失败")
	}
	if !user.Enabled {
		return nil, errs.Forbidden("账号已停用，请联系管理员")
	}

	p, err := a.loadPrincipal(ctx, &user)
	if err != nil {
		return nil, err
	}
	return a.issuePair(ctx, p)
}

// Logout 登出：吊销当前这一端会话。
//
// 只删 tokenID 对应的会话，不影响该用户在其他设备上的登录——
// 这是"多端登录各自独立"的预期行为，也是会话键为什么要带 jti。
func (a *AuthService) Logout(ctx context.Context, c Caller, tokenID string) error {
	if c.User == nil {
		return nil // 已经没登录了，登出是幂等操作，不报错
	}
	if err := a.deps.Cache.DropSession(ctx, c.User.ID, tokenID); err != nil {
		a.log("auth").Warn("删除会话失败", "userId", c.User.ID, "err", err)
	}
	a.audit(ctx, c, auditInput{
		Module: "认证", Action: "logout", TargetType: "user",
		TargetID: c.UserID(), TargetName: c.Name(), Detail: "退出登录",
	})
	return nil
}

// OIDCAuthorize SSO 授权跳转地址。
//
// 未配置时返回明确错误而不是静默失败：前端据此把 SSO 入口置灰，
// 而不是把用户丢到一个打不开的地址上。
func (a *AuthService) OIDCAuthorize(_ context.Context) (string, error) {
	oc := a.deps.Cfg.Auth.OIDC
	if !oc.Enabled {
		return "", errs.NotFoundf("单点登录尚未接入（未配置 auth.oidc）")
	}
	return "", errs.NotFoundf("单点登录客户端尚未实现：需对接 %s（配置已就绪，缺少授权码换取逻辑）", oc.Issuer)
}

// OIDCCallback SSO 回调。同上，未接入时给出明确错误。
func (a *AuthService) OIDCCallback(_ context.Context, _ string) (*LoginResult, error) {
	oc := a.deps.Cfg.Auth.OIDC
	if !oc.Enabled {
		return nil, errs.NotFoundf("单点登录尚未接入（未配置 auth.oidc）")
	}
	return nil, errs.NotFoundf("单点登录回调处理尚未实现")
}

// --------------------------------------------------------------------------- 内部

// issuePair 签发访问/刷新令牌并登记会话。
func (a *AuthService) issuePair(ctx context.Context, p *principal) (*LoginResult, error) {
	cu := p.toCurrentUser()

	access, expiresIn, err := a.deps.JWT.Issue(cu, auth.TokenAccess)
	if err != nil {
		return nil, err
	}
	refresh, _, err := a.deps.JWT.Issue(cu, auth.TokenRefresh)
	if err != nil {
		return nil, err
	}

	// 会话键的 TTL 取访问令牌有效期：会话校验只在访问令牌上做，
	// 刷新令牌另有自己的有效期，没必要让会话键活得更久。
	//
	// jti 从刚签发的令牌里解析回来（而不是改 Issue 的返回值）：
	// 这样"令牌里到底有没有 jti、jti 是什么"由 auth 包一处决定，
	// 不会出现签名与登记用了两个不同 ID 的隐患。
	if claims, err := a.deps.JWT.Parse(access); err == nil && claims.ID != "" {
		if err := a.deps.Cache.SaveSession(ctx, p.User.ID, claims.ID, p.RoleCode, a.deps.JWT.AccessTTL()); err != nil {
			// 会话写不进去只影响"主动吊销"能力，不该阻断登录。
			a.log("auth").Warn("写入会话失败，本次登录不具备主动吊销能力", "userId", p.User.ID, "err", err)
		}
	}

	return &LoginResult{
		AccessToken:  access,
		RefreshToken: refresh,
		TokenType:    "Bearer",
		ExpiresIn:    expiresIn,
		User:         a.toCurrentUserDTO(p),
	}, nil
}

// loadPrincipal 装配身份聚合。
func (a *AuthService) loadPrincipal(ctx context.Context, u *model.User) (*principal, error) {
	p := &principal{User: u, Perms: []string{}, Teams: []string{}}

	// 角色（当前设计一个用户一个角色，取第一条即可）
	var role model.Role
	err := a.db(ctx).
		Joins("JOIN user_role ur ON ur.role_id = role.id").
		Where("ur.user_id = ?", u.ID).
		Order("role.sort ASC").
		First(&role).Error
	switch {
	case err == nil:
		p.RoleCode, p.RoleName = role.Code, role.Name
	case errors.Is(err, gorm.ErrRecordNotFound):
		// 没有角色 = 没有任何权限。不报错，让用户能看到"只读空白界面"，
		// 比登录直接失败更容易让管理员发现是配置漏了。
		return p, nil
	default:
		return nil, errs.Internalf(err, "查询用户角色失败")
	}

	if err := a.db(ctx).
		Table("permission").
		Select("permission.code").
		Joins("JOIN role_permission rp ON rp.permission_id = permission.id").
		Where("rp.role_id = ?", role.ID).
		Order("permission.module, permission.code").
		Pluck("permission.code", &p.Perms).Error; err != nil {
		return nil, errs.Internalf(err, "查询角色权限失败")
	}

	// 团队：从该用户负责的项目里取，去重。
	// 前端用户列表要显示团队标签，而 user 表本身不存团队——
	// 团队归属由项目负责人关系推导，避免"团队成员"与"项目负责人"两处维护不一致。
	if err := a.db(ctx).
		Table("project").
		Distinct("project.team").
		Where("project.owner_id = ? AND project.team <> '' AND project.deleted_at IS NULL", u.ID).
		Pluck("project.team", &p.Teams).Error; err != nil {
		return nil, errs.Internalf(err, "查询用户团队失败")
	}
	return p, nil
}

// toCurrentUser 转成令牌载荷用的身份快照。
func (p *principal) toCurrentUser() *auth.CurrentUser {
	return &auth.CurrentUser{
		ID:          p.User.ID,
		Username:    p.User.Username,
		Name:        p.User.Name,
		Email:       p.User.Email,
		RoleCode:    p.RoleCode,
		RoleName:    p.RoleName,
		Teams:       p.Teams,
		Permissions: p.Perms,
	}
}

// toCurrentUserDTO 转成前端 DTO。
func (a *AuthService) toCurrentUserDTO(p *principal) CurrentUserDTO {
	return CurrentUserDTO{
		ID:          httpx.IDString(p.User.ID),
		Username:    p.User.Username,
		Name:        p.User.Name,
		Email:       p.User.Email,
		Role:        RoleBrief{Code: p.RoleCode, Name: p.RoleName},
		RoleCode:    p.RoleCode,
		RoleName:    p.RoleName,
		Teams:       p.Teams,
		Permissions: p.Perms,
	}
}

// HashPassword 生成密码哈希，供用户创建/重置密码复用。
// 集中在这里，避免各处散落 bcrypt.GenerateFromPassword 而漏设 cost。
func (a *AuthService) HashPassword(plain string) (string, error) {
	cost := a.deps.Cfg.Auth.BcryptCost
	if cost < 10 || cost > 15 {
		cost = 10
	}
	b, err := bcrypt.GenerateFromPassword([]byte(plain), cost)
	if err != nil {
		return "", errs.Internalf(err, "生成密码哈希失败")
	}
	return string(b), nil
}
