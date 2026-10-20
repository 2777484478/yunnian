// Package service 承载平台的业务逻辑。
//
// 分层约定：
//   - handler 只做「解析请求 → 调 service → 写响应」，不写业务判断；
//   - service 负责业务规则、事务、审计与下游编排；
//   - store / integration / logstream 提供能力，service 通过接口依赖它们。
//
// 审计写入统一收口在 base.audit()：任何会改变平台状态的入口都必须留痕，
// 分散写容易漏，也容易漏字段（IP/UA/traceId）。
//
// 关于 base：各领域服务（AuthService / CIService / …）都嵌入 *base，
// 于是共用 audit / db / tx / log 这四个横切能力。用嵌入而不是给每个服务
// 复制一份，是为了让"审计怎么写"只有一个实现——改格式时只改一处。
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/auth"
	"github.com/2777484478/yunnian/internal/cache"
	"github.com/2777484478/yunnian/internal/config"
	"github.com/2777484478/yunnian/internal/crypto"
	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/logstream"
	"github.com/2777484478/yunnian/internal/model"
	"github.com/2777484478/yunnian/internal/queue"
)

// Deps 服务层依赖。
//
// 全部以接口/具体类型注入，service 自己不 new 任何东西——这样启动顺序、
// 连接复用、测试替换都只在 cmd 与测试里决定，业务代码不参与资源管理。
type Deps struct {
	Cfg    *config.Config
	DB     *gorm.DB
	Log    *slog.Logger
	JWT    *auth.Manager
	Stream logstream.Stream
	Up     *integration.Set
	Cache  *cache.Cache
	Sealer *crypto.Sealer

	// Builds / Deploys 是并发受限的任务池。构建可以并行，部署不行——
	// 部署并发受限是为了不把集群与镜像仓库打满，也便于出问题时定位。
	Builds  *queue.Pool
	Deploys *queue.Pool
	// Locker 部署互斥（同环境同服务）。Redis 可用时是分布式的。
	Locker queue.Locker

	// StartedAt 进程启动时间，用于 /healthz 的 uptime。
	StartedAt time.Time
}

// base 承载横切能力，被各领域服务嵌入。
type base struct {
	deps Deps

	// notify 通知投递链路。挂在 base 上而不是作为独立依赖注入，
	// 是因为 CI/CD/System 都会发通知，而它自己不再依赖任何领域服务 ——
	// 若走 Deps 注入，装配层要在 new 之前先构造它，反而把顺序暴露出去。
	notify *NotifyService
}

// Service 聚合各领域服务，由 HTTP 层注入。
type Service struct {
	Auth   *AuthService
	CI     *CIService
	CD     *CDService
	System *SystemService
	AI     *AIService
	Notify *NotifyService

	*base
}

// New 装配服务。
func New(d Deps) *Service {
	b := &base{deps: d}
	b.notify = newNotifyService(d)
	sys := &SystemService{base: b}
	return &Service{
		base: b,
		Auth: &AuthService{base: b},
		// CI 的失败归因要走模型通道，因此和 AI 服务一样复用下面这个 sys 实例。
		CI: &CIService{base: b, sys: sys},
		// CD 的失败诊断同样要走模型通道：它比 CI 更需要——部署失败几乎没有
		// 可机读的错误码能区分场景（都是"没起来"），真实原因只在容器侧证据里。
		CD:     &CDService{base: b, sys: sys},
		System: sys,
		// AI 服务复用 SystemService 的"默认模型通道"解析（含密钥解密），
		// 避免把同一段解密逻辑写两遍——那种重复最容易在换加密方案时漏改一处。
		AI:     &AIService{base: b, sys: sys},
		Notify: b.notify,
	}
}

// Stream 暴露日志流。
//
// SSE 接口需要直接持有它来订阅事件（订阅语义不强，包一层反而更难读），
// 因此这里只做一个最小暴露，而不是把 Stream 的每个方法再代理一遍。
func (s *Service) Stream() logstream.Stream { return s.deps.Stream }

// Caller 请求发起方信息，用于审计与「负责人」类判断。
// 由 HTTP 层从 gin.Context 构造后传入，避免 service 依赖框架类型。
type Caller struct {
	User    *auth.CurrentUser
	IP      string
	UA      string
	TraceID string
}

// UserID 返回用户 ID 指针（未登录为 nil）。
func (c Caller) UserID() *uint64 {
	if c.User == nil {
		return nil
	}
	id := c.User.ID
	return &id
}

// Name 返回展示用姓名。
func (c Caller) Name() string {
	if c.User == nil {
		return "匿名"
	}
	return c.User.Name
}

// Username 返回登录账号。
func (c Caller) Username() string {
	if c.User == nil {
		return ""
	}
	return c.User.Username
}

// RoleCode 返回角色标识。
func (c Caller) RoleCode() string {
	if c.User == nil {
		return ""
	}
	return c.User.RoleCode
}

// --------------------------------------------------------------------------- 横切能力

// auditInput 审计记录入参。
type auditInput struct {
	Module     string
	Action     string
	TargetType string
	TargetID   *uint64
	TargetName string
	Detail     string
	Result     string // success / failed
	ErrMsg     string
	StartedAt  time.Time
}

// audit 写审计日志。审计失败不影响主流程（只记录告警日志），
// 因为"操作已成功但审计写入失败"不应该反过来让用户的操作失败。
func (b *base) audit(ctx context.Context, c Caller, in auditInput) {
	if in.Result == "" {
		in.Result = "success"
	}
	rec := model.AuditLog{
		UserID:     c.UserID(),
		UserName:   c.Name(),
		RoleCode:   c.RoleCode(),
		Module:     in.Module,
		Action:     in.Action,
		TargetType: in.TargetType,
		TargetID:   in.TargetID,
		TargetName: in.TargetName,
		Detail:     in.Detail,
		Result:     in.Result,
		ErrorMsg:   in.ErrMsg,
		IP:         c.IP,
		UserAgent:  c.UA,
		TraceID:    c.TraceID,
		CreatedAt:  time.Now(),
	}
	if c.User != nil {
		rec.Username = c.User.Username
	}
	if !in.StartedAt.IsZero() {
		ms := uint32(time.Since(in.StartedAt).Milliseconds())
		rec.DurationMS = &ms
	}

	if err := b.deps.DB.WithContext(ctx).Create(&rec).Error; err != nil {
		b.deps.Log.Error("写审计日志失败", "err", err, "action", in.Action, "traceId", c.TraceID)
	}
}

// db 返回带上下文的会话。
func (b *base) db(ctx context.Context) *gorm.DB {
	return b.deps.DB.WithContext(ctx)
}

// tx 执行一个事务。service 内部的多步写入统一走它，保证「要么全成、要么全滚」。
func (b *base) tx(ctx context.Context, fn func(*gorm.DB) error) error {
	return b.deps.DB.WithContext(ctx).Transaction(fn)
}

// log 返回带组件标签的日志器。
func (b *base) log(component string) *slog.Logger {
	return b.deps.Log.With("component", component)
}

// --------------------------------------------------------------------------- 通用查询助手

// firstOrNotFound 按主键查询单条记录，未命中时返回 40400 业务错误。
//
// 主键是**必填参数**，这是刻意设计的：早期版本签名里没有主键，调用方
// 以为"传了 id 就该按 id 查"，实际执行的是 `db.First(&rec)` —— 拿回表里第一条记录。
// 这类缺陷编译期不报错、接口照样返回 200，只是数据全错
// （实测：查询构建 #136 得到的是构建 #1，且 status 恰好也是 success，肉眼极难发现）。
// 把 id 放进签名，漏传就直接编译不过。
//
// 第二个参数用 any 而不是 uint64：GORM 的 First 支持整型主键、字符串主键
// 与条件表达式（如 "id = ?"），保持同一套语义可以避免在这里再做一层转换。
func firstOrNotFound[T any](db *gorm.DB, id any, out *T, what string) error {
	err := db.First(out, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errs.NotFoundf("%s不存在", what)
	}
	if err != nil {
		return errs.Internalf(err, "查询%s失败", what)
	}
	return nil
}

// wrapDB 把数据库错误归一化为业务错误（避免把 SQL 细节透给前端）。
func wrapDB(err error, what string) error {
	if err == nil {
		return nil
	}
	// 已经是业务错误就别再包一层。事务体里常常混着参数校验（errs.Invalidf），
	// 无条件包成 Internalf 会同时造成两处失真：
	//   1. 把 40000（参数错，调用方可自行修正）变成 50000（服务端故障），
	//      前端据此提示"稍后重试"，而用户真正该做的是改输入；
	//   2. 丢掉具体原因（如 `配置项 x：应为数字，收到 "abc"`），
	//      只剩一句"更新系统配置失败"，排障时无从下手。
	var e *errs.Error
	if errors.As(err, &e) {
		return err
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errs.NotFoundf("%s不存在", what)
	}
	return errs.Internalf(err, "%s失败", what)
}

// now 返回当前时间，便于统一时间源（后续接时钟注入时只改这一处）。
func now() time.Time { return time.Now() }

// formatTime 输出文档约定的 ISO8601（带时区）。
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// formatTimePtr 可空时间的格式化。
func formatTimePtr(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// clipRunes 把字符串按**字符**（而非字节）截断到 n，超出部分丢弃。
//
// 失败原因要落库（build_record.error_message / deploy_record.error_message，
// varchar(512)），而它来自集群与下游的原始文本，长度不可控。MySQL 在严格模式下
// 会直接报 1406「Data too long」把整条 UPDATE 打回去 —— 结果是"失败时写终态失败"，
// 比丢一段文本糟糕得多。按 rune 截断是因为列上是字符数：按字节截会把一个汉字切成
// 两半，在库里留下乱码。
func clipRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// stateChangeError 状态机冲突。
func stateChangeError(format string, args ...any) error {
	return errs.Newf(errs.CodeStateConflict, format, args...)
}

// upstreamError 下游不可用。
func upstreamError(err error, format string, args ...any) error {
	return errs.Upstream(err, format, args...)
}

// notFoundErr 资源不存在。
func notFoundErr(format string, args ...any) error {
	return errs.NotFoundf(format, args...)
}

// ensurePositive 校验正整数参数。
func ensurePositive(v int, field string) error {
	if v <= 0 {
		return errs.Invalidf("%s 必须为正整数", field)
	}
	return nil
}

// stringOr 空字符串时取默认值。
func stringOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// idString 主键转字符串（前端所有 id 都是字符串）。
func idString(id uint64) string {
	return fmt.Sprintf("%d", id)
}

// splitCSV 逗号分隔的多值参数拆成切片，逐个 TrimSpace 并丢掉空项。
//
// 为什么要有它：列表接口的 status 这类筛选，前端常需要一次表达多个取值
// （如构建列表的「进行中」= running,queued）。让后端接受逗号分隔，
// 比让前端发两次请求再自己合并更省事，也避免了两份结果的分页对不齐。
// `a`、`a,`、` a , b ` 都得到期望结果，不会拼出 `IN (”)` 这种查不到东西的条件。
func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// VisibleEnvKeys 返回当前用户**能查看**的环境 key。
//
// 第二个返回值 restricted 表示"是否受限制"：
//
//	restricted=false → 全部可见（**没有配过可见性的人走这条**）
//	restricted=true  → 只看 allowed 里的；allowed 为空 = 一个都看不到
//
// ⚠️ 为什么"没配过 = 全部可见"，而不是"没配过 = 看不到"：
// 这条规则是**后加的**，存量角色（ops / dev / qa / readonly）身上都没有
// `env:view:*`。若按"无权限即不可见"处理，升级当天所有人打开平台会是一片空白 ——
// 那不是"更安全"，是把平台弄瞎了。要收紧就显式配，配了才生效。
//
// 权限点形态：
//   - `*` 或 `env:*`        → 全部可见
//   - `env:view:test` 等    → 只这些（可多个）
//   - 一个都没有             → 全部可见（见上）
func (c Caller) VisibleEnvKeys() (allowed []string, restricted bool) {
	if c.User == nil {
		return nil, false
	}
	if c.User.HasAnyPerm("*", "env:*") {
		return nil, false
	}
	var keys []string
	for _, p := range c.User.Permissions {
		if rest, ok := strings.CutPrefix(p, "env:view:"); ok && rest != "" {
			keys = append(keys, rest)
		}
	}
	if len(keys) == 0 {
		return nil, false
	}
	return keys, true
}
