// Package auth 负责身份令牌的签发/校验与权限点匹配。
//
// 设计要点：
//   - 令牌用 HS256 签名的 JWT，payload 里带权限点快照，避免每个请求都回查数据库；
//   - 角色权限变更时通过 Redis 会话键主动失效（见 service/auth.go），权限快照随重新登录刷新；
//   - 权限点匹配支持通配与枚举，规则来自 docs/02 的权限点初始化数据。
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/2777484478/yunnian/internal/config"
	"github.com/2777484478/yunnian/internal/errs"
)

// 令牌类型。
const (
	TokenAccess  = "access"
	TokenRefresh = "refresh"
)

// CurrentUser 认证后挂在请求上下文里的身份快照。
// 字段名与 docs/03 §1 的登录响应保持一致。
type CurrentUser struct {
	ID          uint64   `json:"id"`
	Username    string   `json:"username"`
	Name        string   `json:"name"`
	Email       string   `json:"email"`
	RoleCode    string   `json:"roleCode"`
	RoleName    string   `json:"roleName"`
	Teams       []string `json:"teams"`
	Permissions []string `json:"permissions"`
}

// HasPerm 判断是否具备某权限点。
func (u *CurrentUser) HasPerm(code string) bool { return Match(u.Permissions, code) }

// HasAnyPerm 判断是否具备其中任一权限点。
func (u *CurrentUser) HasAnyPerm(codes ...string) bool { return HasAny(u.Permissions, codes...) }

// Claims JWT 载荷。
type Claims struct {
	jwt.RegisteredClaims
	Type        string   `json:"typ"`
	UserID      uint64   `json:"uid"`
	Username    string   `json:"usr"`
	Name        string   `json:"nam"`
	RoleCode    string   `json:"rol"`
	Permissions []string `json:"prm,omitempty"`
}

// ToUser 由载荷还原身份快照。
func (c *Claims) ToUser() *CurrentUser {
	return &CurrentUser{
		ID:          c.UserID,
		Username:    c.Username,
		Name:        c.Name,
		RoleCode:    c.RoleCode,
		Permissions: c.Permissions,
	}
}

// Manager 令牌签发与校验器。
type Manager struct {
	secret     []byte
	issuer     string
	accessTTL  time.Duration
	refreshTTL time.Duration
}

// NewManager 由配置构造。
func NewManager(cfg config.AuthConfig) *Manager {
	return &Manager{
		secret:     []byte(cfg.JWTSecret),
		issuer:     cfg.Issuer,
		accessTTL:  cfg.AccessTokenTTL.D(),
		refreshTTL: cfg.RefreshTokenTTL.D(),
	}
}

// AccessTTL 返回访问令牌有效期，用于登录响应的 expiresIn。
func (m *Manager) AccessTTL() time.Duration { return m.accessTTL }

// Issue 签发令牌，返回 (令牌串, 有效秒数, 错误)。
func (m *Manager) Issue(u *CurrentUser, tokenType string) (string, int, error) {
	ttl := m.accessTTL
	if tokenType == TokenRefresh {
		ttl = m.refreshTTL
	}
	now := time.Now()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    m.issuer,
			Subject:   strconv.FormatUint(u.ID, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now.Add(-time.Second)),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
			ID:        randomHex(8),
		},
		Type:     tokenType,
		UserID:   u.ID,
		Username: u.Username,
		Name:     u.Name,
		RoleCode: u.RoleCode,
	}
	// 权限点只放进访问令牌：刷新令牌仅用于换新，体积越小越好
	if tokenType == TokenAccess {
		claims.Permissions = u.Permissions
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(m.secret)
	if err != nil {
		return "", 0, errs.Internalf(err, "签发令牌失败")
	}
	return signed, int(ttl.Seconds()), nil
}

// Parse 校验并解析令牌。
// 过期与非法分别返回 40101 / 40100，前端据此决定是静默续期还是跳登录页。
func (m *Manager) Parse(raw string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("非预期的签名算法：%v", t.Header["alg"])
		}
		return m.secret, nil
	}, jwt.WithIssuer(m.issuer), jwt.WithLeeway(30*time.Second))

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, errs.TokenExpired()
		}
		return nil, errs.Unauthorized("登录凭证无效，请重新登录")
	}
	if claims.Type == "" {
		claims.Type = TokenAccess
	}
	return claims, nil
}

// --------------------------------------------------------------------------- 权限点匹配

// Match 判断权限集合 perms 是否覆盖目标权限点 want。
//
// 支持的形态（与 docs/02 的权限点初始化数据对应）：
//
//	"*"                      全部权限
//	"env:*"                  模块级通配（匹配 env:read / env:write …）
//	"build:*"                同上
//	"deploy:test,staging"    枚举多个环境（匹配 deploy:test 与 deploy:staging）
//	"project:read"           精确匹配
//
// want 为空表示无需权限，直接放行。
func Match(perms []string, want string) bool {
	want = strings.TrimSpace(want)
	if want == "" {
		return true
	}
	for _, p := range perms {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// 全权
		if p == "*" {
			return true
		}
		// 精确匹配
		if p == want {
			return true
		}
		// 模块级通配：env:* / deploy:*
		if strings.HasSuffix(p, ":*") {
			prefix := strings.TrimSuffix(p, "*")
			if strings.HasPrefix(want, prefix) {
				return true
			}
		}
		// 后缀通配：*:read
		// 只读角色用一条规则覆盖所有模块的读权限（docs/02 里 readonly → *:read）。
		if strings.HasPrefix(p, "*:") {
			if strings.HasSuffix(want, p[1:]) {
				return true
			}
		}
		// 枚举形态：deploy:test,staging
		if i := strings.IndexByte(p, ':'); i > 0 && strings.Contains(p[i+1:], ",") {
			module := p[:i]
			for _, item := range strings.Split(p[i+1:], ",") {
				if module+":"+strings.TrimSpace(item) == want {
					return true
				}
			}
		}
	}
	return false
}

// HasAny 判断权限集合是否覆盖 wants 中任意一项。
// 一个接口允许"多选一"权限时使用（如部署接口可接受 deploy:test 或 deploy:*）。
func HasAny(perms []string, wants ...string) bool {
	if len(wants) == 0 {
		return true
	}
	for _, w := range wants {
		if Match(perms, w) {
			return true
		}
	}
	return false
}

// randomHex 生成 n 字节的十六进制串（2n 个字符）。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 在现代系统上不会失败；退化为时间戳，保证功能不中断
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf)
}
