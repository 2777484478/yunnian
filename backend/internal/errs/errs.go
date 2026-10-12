// Package errs 定义平台统一业务错误码与错误类型。
//
// 错误码取值与 docs/03-API设计.md 的 0.3 节严格一致——前端只认 code 字段做提示与跳转，
// 因此这里的数字不能随意调整。
package errs

import (
	"errors"
	"fmt"
	"net/http"
)

// Code 业务错误码。
type Code int

// 平台错误码。命名与文档表格逐行对应。
const (
	CodeOK Code = 0 // 成功

	CodeInvalidParam       Code = 40000 // 参数校验失败
	CodeUnauthorized       Code = 40100 // 未登录 / Token 失效
	CodeTokenExpired       Code = 40101 // Token 过期，请用 refresh_token 续期
	CodeForbidden          Code = 40300 // 无权限（缺少权限点）
	CodeEnvDisabled        Code = 40301 // 环境已停用，禁止部署
	CodeNotFound           Code = 40400 // 资源不存在
	CodeConflict           Code = 40900 // 唯一键冲突
	CodeStateConflict      Code = 40901 // 状态冲突（如该服务有部署进行中）
	CodeIdempotentConflict Code = 40902 // 幂等冲突（重复提交返回已有任务）
	CodeRateLimited        Code = 42900 // 触发频率超限
	CodeInternal           Code = 50000 // 服务端异常
	CodeUpstreamDown       Code = 50200 // 上游系统不可用（GitLab/Harbor/K8s）
)

// Error 带业务码的错误。服务层统一返回它，HTTP 层再翻译成响应体。
type Error struct {
	Code    Code
	Message string
	Data    any
	cause   error
}

// Error 实现 error 接口；同时打印业务码与底层原因，便于日志排查。
func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("code=%d %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("code=%d %s", e.Code, e.Message)
}

// Unwrap 支持 errors.Is / errors.As 沿链查找。
func (e *Error) Unwrap() error { return e.cause }

// New 构造业务错误。
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Newf 构造带格式化提示的业务错误。
func Newf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap 附加底层原因（用于日志），对外只暴露 message。
func Wrap(code Code, cause error, message string) *Error {
	return &Error{Code: code, Message: message, cause: cause}
}

// WithData 挂载附加数据（如字段级校验错误）。
func (e *Error) WithData(data any) *Error {
	e.Data = data
	return e
}

// ---------------------------------------------------------------- 常用快捷构造

// Invalid 参数不合法。
func Invalid(message string) *Error { return New(CodeInvalidParam, message) }

// Invalidf 参数不合法（格式化）。
func Invalidf(format string, args ...any) *Error { return Newf(CodeInvalidParam, format, args...) }

// Unauthorized 未登录或凭证无效。
func Unauthorized(message string) *Error { return New(CodeUnauthorized, message) }

// TokenExpired 令牌过期。
func TokenExpired() *Error { return New(CodeTokenExpired, "登录已过期，请重新登录") }

// Forbidden 缺权限点。
func Forbidden(message string) *Error { return New(CodeForbidden, message) }

// NotFoundf 资源不存在。
func NotFoundf(format string, args ...any) *Error { return Newf(CodeNotFound, format, args...) }

// Conflict 唯一键冲突。
func Conflict(message string) *Error { return New(CodeConflict, message) }

// StateConflict 状态机冲突。
func StateConflict(message string) *Error { return New(CodeStateConflict, message) }

// RateLimited 频率超限。
func RateLimited(message string) *Error { return New(CodeRateLimited, message) }

// Internal 服务端内部错误；cause 只进日志，不下发给前端。
func Internal(cause error) *Error {
	return Wrap(CodeInternal, cause, "服务端内部错误，请联系管理员")
}

// Internalf 服务端内部错误（自定义对外文案）。
func Internalf(cause error, format string, args ...any) *Error {
	return Wrap(CodeInternal, cause, fmt.Sprintf(format, args...))
}

// Upstream 上游系统不可用。
func Upstream(cause error, format string, args ...any) *Error {
	return Wrap(CodeUpstreamDown, cause, fmt.Sprintf(format, args...))
}

// From 把任意 error 归一化为 *Error。
// 非业务错误一律按 50000 处理，并保留原始错误用于日志；err 为 nil 时返回 nil。
func From(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Internal(err)
}

// HTTPStatus 业务码 → HTTP 状态码。
func HTTPStatus(code Code) int {
	switch code {
	case CodeOK:
		return http.StatusOK
	case CodeInvalidParam:
		return http.StatusBadRequest
	case CodeUnauthorized, CodeTokenExpired:
		return http.StatusUnauthorized
	case CodeForbidden, CodeEnvDisabled:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict, CodeStateConflict, CodeIdempotentConflict:
		return http.StatusConflict
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeUpstreamDown:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}
