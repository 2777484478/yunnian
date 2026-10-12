// Package httpx 统一 HTTP 响应体、分页参数与请求绑定。
//
// 所有响应形状与 docs/03-API设计.md 的 0.1 / 0.2 / 0.3 节严格对齐：
// 前端只判断 `code`（0 为成功），不做 HTTP 状态码分支，因此这里的形状不能随意改。
package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"

	"github.com/2777484478/yunnian/internal/errs"
)

const ctxTraceID = "yn.traceId"

// Body 统一响应体。
type Body struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
	TraceID string `json:"traceId"`
}

// TraceID 读取当前请求的链路 ID（由 TraceID 中间件写入）。
func TraceID(c *gin.Context) string {
	if v, ok := c.Get(ctxTraceID); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// SetTraceID 由中间件写入链路 ID。
func SetTraceID(c *gin.Context, id string) { c.Set(ctxTraceID, id) }

// OK 返回成功响应。HTTP 状态码恒为 200，业务态由 code 表达。
func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Body{
		Code:    int(errs.CodeOK),
		Message: "ok",
		Data:    data,
		TraceID: TraceID(c),
	})
}

// Fail 返回失败响应。err 为 nil 时退化为成功响应，避免调用方漏判。
// 内部会 Abort，后续 handler 不再执行。
func Fail(c *gin.Context, err error) {
	e := errs.From(err)
	if e == nil {
		OK(c, nil)
		return
	}
	c.AbortWithStatusJSON(errs.HTTPStatus(e.Code), Body{
		Code:    int(e.Code),
		Message: e.Message,
		Data:    e.Data,
		TraceID: TraceID(c),
	})
}

// --------------------------------------------------------------------------- 分页

const (
	defaultPageSize = 20
	maxPageSize     = 200
)

// PageQuery 通用分页请求参数。
type PageQuery struct {
	Page     int    `form:"page"`
	PageSize int    `form:"pageSize"`
	SortBy   string `form:"sortBy"`
	Order    string `form:"order"`
	Keyword  string `form:"keyword"`
}

// Normalize 归一化分页参数并解析排序列。
//
// allowed 是「入参名 → 数据库列名」的白名单：只有白名单命中的 sortBy 才会被采用，
// 这是防 SQL 注入的关键——排序列无法参数化，必须用白名单。
// 未命中时回退到 fallbackColumn。order 非 asc 一律按 desc。
func (p *PageQuery) Normalize(allowed map[string]string, fallbackColumn string) (column string, desc bool) {
	if p.Page < 1 {
		p.Page = 1
	}
	if p.PageSize < 1 {
		p.PageSize = defaultPageSize
	}
	if p.PageSize > maxPageSize {
		p.PageSize = maxPageSize
	}
	column = fallbackColumn
	if col, ok := allowed[p.SortBy]; ok && col != "" {
		column = col
	}
	desc = !strings.EqualFold(strings.TrimSpace(p.Order), "asc")
	return column, desc
}

// Offset 计算 SQL OFFSET。
func (p PageQuery) Offset() int { return (p.Page - 1) * p.PageSize }

// PageData 分页响应体（docs/03 §0.2）。
type PageData struct {
	List     any   `json:"list"`
	Total    int64 `json:"total"`
	Page     int   `json:"page"`
	PageSize int   `json:"pageSize"`
}

// NewPageData 组装分页响应。
// 注意：list 为 nil 时会替换成空数组，避免前端拿到 null 而崩溃（前端一律按数组遍历）。
func NewPageData(list any, total int64, q PageQuery) PageData {
	if list == nil {
		list = []any{}
	}
	return PageData{List: list, Total: total, Page: q.Page, PageSize: q.PageSize}
}

// --------------------------------------------------------------------------- 绑定与校验

// Bind 绑定并校验请求体/查询参数。失败时直接写出 40000 响应并返回 false。
func Bind(c *gin.Context, obj any) bool {
	if err := c.ShouldBind(obj); err != nil {
		Fail(c, errs.Invalid("参数校验失败").WithData(gin.H{"errors": FieldErrors(err)}))
		return false
	}
	return true
}

// FieldErrors 把校验错误展开为字段级提示，对应 docs/03 §0.3 的 data.errors。
func FieldErrors(err error) []gin.H {
	var ve validator.ValidationErrors
	if errors.As(err, &ve) {
		out := make([]gin.H, 0, len(ve))
		for _, fe := range ve {
			out = append(out, gin.H{"field": toSnake(fe.Field()), "message": validationMessage(fe)})
		}
		return out
	}
	return []gin.H{{"field": "", "message": err.Error()}}
}

func validationMessage(fe validator.FieldError) string {
	switch fe.Tag() {
	case "required":
		return "该字段必填"
	case "email":
		return "邮箱格式不正确"
	case "url":
		return "URL 格式不正确"
	case "min":
		return "不能小于 " + fe.Param()
	case "max":
		return "不能大于 " + fe.Param()
	case "gte":
		return "不能小于 " + fe.Param()
	case "lte":
		return "不能大于 " + fe.Param()
	case "oneof":
		return "取值范围必须是 [ " + fe.Param() + " ]"
	case "alphanum":
		return "只允许字母与数字"
	case "hostname_port":
		return "格式应为 host:port"
	default:
		return "校验未通过（" + fe.Tag() + "）"
	}
}

// toSnake 把 Go 字段名转成下划线形式，与前端 JSON 字段名对齐。
func toSnake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// --------------------------------------------------------------------------- 参数读取

// PathString 读取路径参数并去除首尾空白。
func PathString(c *gin.Context, name string) string {
	return strings.TrimSpace(c.Param(name))
}

// PathUint64 读取路径参数并转成 uint64；非法时返回 40000 业务错误。
func PathUint64(c *gin.Context, name string) (uint64, error) {
	raw := c.Param(name)
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, errs.Invalidf("路径参数 %s 必须是数字，收到 %q", name, raw)
	}
	return v, nil
}

// QueryBool 读取布尔查询参数；未提供时返回 def。接受 1/0、true/false、yes/no、on/off。
func QueryBool(c *gin.Context, name string, def bool) bool {
	raw := strings.ToLower(strings.TrimSpace(c.Query(name)))
	switch raw {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// QueryInt 读取整型查询参数；未提供或非法时返回 def。
func QueryInt(c *gin.Context, name string, def int) int {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return v
}

// ClientIP 取真实客户端 IP，优先信任反向代理透传的头。
func ClientIP(c *gin.Context) string {
	if ip := strings.TrimSpace(c.GetHeader("X-Real-IP")); ip != "" {
		return ip
	}
	if xff := c.GetHeader("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	return c.ClientIP()
}

// UserAgent 取 UA，用于审计。
func UserAgent(c *gin.Context) string {
	ua := c.GetHeader("User-Agent")
	if len(ua) > 255 {
		ua = ua[:255]
	}
	return ua
}

// IDString 把自增主键序列化为字符串。
//
// 前端类型定义里所有 id 都是 string（mock 阶段用的是 "p-001" 这类字面量），
// 统一转字符串可避免前端在处理大整数时丢精度，也免去前端做类型转换。
func IDString(id uint64) string { return strconv.FormatUint(id, 10) }

// ParseIDString 解析前端回传的字符串 ID。
func ParseIDString(raw string) (uint64, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, errs.Invalidf("非法 ID：%q", raw)
	}
	return v, nil
}

// MustIDString 组合 IDString 与错误忽略，用于已确知合法的场景（如内存对象）。
func MustIDString(id uint64, err error) string {
	if err != nil {
		return ""
	}
	return strconv.FormatUint(id, 10)
}

var _ = fmt.Sprintf // 保留 fmt，后续扩展便捷格式化时使用
