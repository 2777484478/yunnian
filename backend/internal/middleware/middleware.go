// Package middleware 提供 HTTP 中间件：链路追踪、异常恢复、访问日志、跨域与认证鉴权。
//
// 认证采用「解析 + 校验分离」两段式：
//   - Auth 只负责「有令牌就解析并注入身份」，不阻断请求，这样公开接口与受保护接口
//     可以共用同一条中间件链；
//   - RequirePerm 决定「是否必须登录 / 是否具备权限点」，缺什么就报什么错码。
//
// 这样路由表上只需声明权限点，不必为公开接口单独维护白名单。
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/auth"
	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/httpx"
)

const (
	ctxUser    = "yn.user"
	ctxClaims  = "yn.claims"
	ctxAuthErr = "yn.authErr"
)

// TraceID 为每个请求确定链路 ID：优先复用网关透传的 X-Trace-Id，否则本地生成。
// 结果会写入响应头与日志，便于把前端报错与服务端日志对上。
func TraceID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := strings.TrimSpace(c.GetHeader("X-Trace-Id"))
		if id == "" || len(id) > 64 {
			id = randomHex(8)
		}
		httpx.SetTraceID(c, id)
		c.Header("X-Trace-Id", id)
		c.Next()
	}
}

// Recovery 捕获 panic，记录堆栈并返回 50000，避免单个请求打挂整个进程。
func Recovery(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("请求处理 panic",
					"traceId", httpx.TraceID(c),
					"method", c.Request.Method,
					"path", c.Request.URL.Path,
					"panic", fmt.Sprint(r),
					"stack", string(debug.Stack()),
				)
				httpx.Fail(c, errs.Internal(fmt.Errorf("panic: %v", r)))
			}
		}()
		c.Next()
	}
}

// AccessLog 输出结构化访问日志。4xx 记 warn、5xx 记 error，便于按级别筛选。
func AccessLog(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		attrs := []any{
			"traceId", httpx.TraceID(c),
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"latencyMs", time.Since(start).Milliseconds(),
			"ip", httpx.ClientIP(c),
			"size", c.Writer.Size(),
		}
		if u := CurrentUser(c); u != nil {
			attrs = append(attrs, "user", u.Username, "role", u.RoleCode)
		}
		if len(c.Errors) > 0 {
			attrs = append(attrs, "errors", c.Errors.String())
		}

		switch {
		case c.Writer.Status() >= http.StatusInternalServerError:
			log.Error("http", attrs...)
		case c.Writer.Status() >= http.StatusBadRequest:
			log.Warn("http", attrs...)
		default:
			log.Info("http", attrs...)
		}
	}
}

// CORS 跨域处理。
//
// 注意：回显 Origin 而不是返回 "*"，因为需要配合 Allow-Credentials；
// 浏览器规范不允许 "*" 与凭据同时生效。
func CORS(origins []string) gin.HandlerFunc {
	allowAll := false
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		o = strings.TrimSpace(o)
		if o == "*" {
			allowAll = true
			continue
		}
		if o != "" {
			allowed[o] = true
		}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" && (allowAll || allowed[origin]) {
			c.Header("Access-Control-Allow-Origin", origin)
			c.Header("Access-Control-Allow-Credentials", "true")
			c.Header("Vary", "Origin")
		}

		if c.Request.Method == http.MethodOptions {
			c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
			c.Header("Access-Control-Allow-Headers",
				"Origin,Content-Type,Accept,Authorization,X-Trace-Id,X-Requested-With")
			c.Header("Access-Control-Max-Age", "86400")
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// SessionChecker 会话有效性校验，由 cache.Cache 实现。
//
// 为什么要它：JWT 一旦签发就无法主动吊销，而平台有三个必须立即失效的场景——
// 管理员禁用账号、调整角色权限、用户主动登出。只靠签名校验，这些场景下
// 旧令牌会一直有效到过期（最长 2 小时）。加一层会话键即解决。
//
// 传 nil 表示不做二次校验（未启用 Redis 的单实例开发形态）。
type SessionChecker interface {
	SessionValid(ctx context.Context, userID uint64, tokenID string) bool
}

// Auth 解析 Bearer 令牌并注入身份；无令牌或令牌非法时**不阻断**请求。
// 是否必须登录由 RequirePerm 决定，因此这里把认证错误暂存到上下文。
func Auth(m *auth.Manager, sc SessionChecker) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := bearerToken(c)
		if raw == "" {
			c.Next()
			return
		}

		claims, err := m.Parse(raw)
		if err != nil {
			c.Set(ctxAuthErr, err)
			c.Next()
			return
		}
		// 刷新令牌不能用于调用业务接口
		if claims.Type != auth.TokenAccess {
			c.Set(ctxAuthErr, errs.Unauthorized("请使用访问令牌调用业务接口"))
			c.Next()
			return
		}
		// 会话二次校验：签名有效 ≠ 会话仍有效（见 SessionChecker 注释）
		if sc != nil && !sc.SessionValid(c.Request.Context(), claims.UserID, claims.ID) {
			c.Set(ctxAuthErr, errs.Unauthorized("登录状态已失效，请重新登录"))
			c.Next()
			return
		}

		c.Set(ctxUser, claims.ToUser())
		c.Set(ctxClaims, claims)
		c.Next()
	}
}

// RequirePerm 声明接口所需权限点：无参数表示只要求登录，不校验具体权限。
//
// 多个权限点之间是「或」的关系，便于一个接口接受多种权限（如部署可接受 deploy:test 或 deploy:*）。
func RequirePerm(codes ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		u := CurrentUser(c)
		if u == nil {
			// 有更具体的认证错误就上报它（如令牌过期 → 40101，前端可静默续期）
			if v, ok := c.Get(ctxAuthErr); ok {
				if e, ok := v.(error); ok {
					httpx.Fail(c, e)
					return
				}
			}
			httpx.Fail(c, errs.Unauthorized("请先登录"))
			return
		}
		if len(codes) == 0 {
			c.Next()
			return
		}
		if !u.HasAnyPerm(codes...) {
			httpx.Fail(c, errs.Newf(errs.CodeForbidden, "缺少权限点：%s", strings.Join(codes, " 或 ")))
			return
		}
		c.Next()
	}
}

// CurrentUser 读取当前登录用户；未登录返回 nil。
func CurrentUser(c *gin.Context) *auth.CurrentUser {
	if v, ok := c.Get(ctxUser); ok {
		if u, ok := v.(*auth.CurrentUser); ok {
			return u
		}
	}
	return nil
}

// CurrentClaims 读取令牌载荷；未登录返回 nil。
func CurrentClaims(c *gin.Context) *auth.Claims {
	if v, ok := c.Get(ctxClaims); ok {
		if cl, ok := v.(*auth.Claims); ok {
			return cl
		}
	}
	return nil
}

// TokenID 返回当前访问令牌的 jti，用于登出时精准吊销这一端会话。
func TokenID(c *gin.Context) string {
	if cl := CurrentClaims(c); cl != nil {
		return cl.ID
	}
	return ""
}

// bearerToken 提取请求携带的访问令牌，按优先级取：
//
//	1. Authorization 头（常规接口的唯一正道）
//	2. `?token=` 查询参数（**仅**为 SSE 兜底）
//
// 为什么需要第 2 条：浏览器原生的 EventSource 不支持自定义请求头，无法带 Authorization；
// 各日志流接口（/builds/{id}/logs/stream 等 6 个）只能把令牌放查询串。
// 保留 EventSource 的代价是令牌会出现在 URL 里（浏览器历史、代理日志），
// 因此仅在这一条链路上使用；平台的访问日志只记 URL.Path（不含查询串），令牌不会落库/落盘。
// 不要把它当通用方案——普通接口仍然只走 Authorization 头。
func bearerToken(c *gin.Context) string {
	if h := strings.TrimSpace(c.GetHeader("Authorization")); h != "" {
		const prefix = "Bearer "
		if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			return strings.TrimSpace(h[len(prefix):])
		}
		return h
	}
	return strings.TrimSpace(c.Query("token"))
}

// randomHex 生成 n 字节十六进制串（2n 个字符），用于链路 ID。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%016x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
