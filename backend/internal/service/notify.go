package service

// 通知**发送**链路。
//
// 此前平台只有「通知渠道的配置管理」：增删改与目标格式校验都落库生效，
// 但没有任何发送器 —— 界面上那个「测试」按钮会如实告诉你"尚未接入投递"。
// 本文件把这条链路补上，一期只接两个通道：**邮件（SMTP）** 与 **钉钉（自定义机器人）**。
//
// 五条设计约束，每条都对应一个真实会咬人的场景：
//
//  1. **绝不阻塞、绝不把异常抛给调用方**。调用点在构建/部署的收尾路径上
//     （finishBuild / failDeploy / 建单落 pending_approval）。通知发不出去是运维问题，
//     不能让"把终态写进库"这件事跟着失败或变慢，更不能因为报警群配置错了
//     就让一次部署卡住。
//
//  2. **后台 goroutine 不能用调用方的 ctx**。ctx 来自 HTTP 请求或任务执行体，
//     用户点完"部署"就关页面 → ctx 立刻 cancel → 还在退避重试的通知会以
//     "context canceled" 静默死掉。这恰好是最常见的路径。所以进 Dispatch 第一件事
//     就是剥掉取消信号（保留 trace 值），再套自己的超时预算。
//
//  3. **"没接发送器"不能静默成功**。渠道类型里有 wecom / feishu / webhook 三种
//     平台还没有实现的通道。它们必须**明确报错并记进 last_error**，
//     而不是安静地什么都不做 —— 一个"配了、启用了、却永远收不到"的渠道，
//     比一个报错的渠道难查得多。
//
//  4. **HTTP 200 不等于发送成功**。钉钉无论成败都返回 200，真实结果在 body 的
//     errcode 里。不解析它，就会把"签名错/被限流/关键词不匹配"记成发送成功，
//     顺手把 last_sent_at 刷新成"刚刚发过"。
//
//  5. **重试要有分寸**。网络抖动、5xx、钉钉限流值得退避重试；
//     凭据错、目标非法、收件人格式错重试一百次也是同一个结果，只是让用户多等。

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/yunnian/yunnian/internal/config"
	"github.com/yunnian/yunnian/internal/model"
)

// --------------------------------------------------------------------------- 事件

// NotifyEvent 通知事件码。与 notify_channel.events 里存的字符串是同一套值，
// 前端「订阅事件」勾选项就来自 SupportedNotifyEvents。
type NotifyEvent string

const (
	EventBuildFailed          NotifyEvent = "build_failed"
	EventBuildInterrupted     NotifyEvent = "build_interrupted"
	EventDeployFailed         NotifyEvent = "deploy_failed"
	EventDeployInterrupted    NotifyEvent = "deploy_interrupted"
	EventDeployPendingApprove NotifyEvent = "deploy_pending_approval"
)

// 「被平台重启中断」为什么不并进 `*_failed`：
//
// 这两个码对应的状态（`build_record.status = interrupted`）在库里就是**独立一档**，
// 当初特意没并进 failed / canceled（见 `ci.go` 的 `ReconcileInterrupted`）。
// 通知跟着状态机走：并进 `build_failed` 会把"平台重启"说成"这次构建哪里写错了"，
// 引着人去翻代码和依赖 —— 而真实要做的事只有一件：**重新触发**。
// 反过来，订阅了 `build_failed` 的告警群也未必想在凌晨被一条"平台自己重启了"叫醒。
// 两者是可分开订阅的，所以这里给两个独立的码。
const (
	// interruptedListLimit 一条中断汇总消息里最多列出几条明细。
	//
	// 为什么要设上限：构建/部署队列都是**进程内**的，等待队列容量是并发数 ×20
	// （`internal/queue.NewPool`，默认 3×20 = 60）。一次重启理论上能中断几十条记录，
	// 逐条发消息会把告警群刷屏 —— 而刷屏的后果是这一条真的告警被忽略掉。
	// 所以同类合并成一条，超出部分只报数量。
	interruptedListLimit = 5
)

// SupportedNotifyEvents 平台**真的会发出**的事件。
//
// ⚠️ 这份清单是「能勾选 == 后端受理」这条约定的落点。它故意只列已实现的五项：
// 界面上曾经可选 build_success / deploy_success / rollback_executed / env_unhealthy
// 这些码，但**没有任何代码会触发它们** —— 用户勾了、存库了、然后永远收不到，
// 而界面上那个勾还在，看起来一切正常。要么实现它，要么别让它出现在选项里。
//
// 后端的入参白名单（`validateNotifyEvents`）直接读这份清单，所以往这里加一项，
// 前端选项、后端受理、种子回归测试（`seed_notify_channels_test.go`）三处同时生效 ——
// 反过来，加了一项却没有任何触发点，那个测试会直接报红。
var SupportedNotifyEvents = []NotifyEvent{
	EventBuildFailed,
	EventBuildInterrupted,
	EventDeployFailed,
	EventDeployInterrupted,
	EventDeployPendingApprove,
}

// IsSupportedNotifyEvent 判断事件码是否是平台会发出的事件。
func IsSupportedNotifyEvent(code string) bool {
	for _, e := range SupportedNotifyEvents {
		if string(e) == code {
			return true
		}
	}
	return false
}

// eventLabel 事件码 → 中文（用于通知正文与错误提示）。
func eventLabel(code NotifyEvent) string {
	switch code {
	case EventBuildFailed:
		return "构建失败"
	case EventBuildInterrupted:
		return "构建被中断"
	case EventDeployFailed:
		return "部署失败"
	case EventDeployInterrupted:
		return "部署被中断"
	case EventDeployPendingApprove:
		return "部署待审批"
	}
	return string(code)
}

// --------------------------------------------------------------------------- 消息

// NotifyField 消息里的一行"名值对"。各渠道按自己的长相渲染（钉钉 markdown / 邮件纯文本）。
type NotifyField struct {
	Name  string
	Value string
}

// NotifyMessage 一条待投递的消息，**与渠道无关**。
//
// 渠道无关是刻意的：渠道的实现只关心"怎么把这段结构传出去"，
// 不需要知道这是构建还是部署、字段该有哪些 —— 那些在构造消息时就已经定好了。
type NotifyMessage struct {
	Event  NotifyEvent
	Title  string        // 一行标题：钉钉 markdown 的 title、邮件主题
	Fields []NotifyField // 结构化事实：项目、环境、操作人…
	Reason string        // 失败真因。可能为空 —— 早于"失败原因落库"的历史记录就没记下来
	Link   string        // 平台详情页地址；为空则不渲染链接
	Level  string        // error / warning，决定配色
}

// --------------------------------------------------------------------------- 发送器

// notifier 一个渠道的发送实现。
type notifier interface {
	Kind() string
	Send(ctx context.Context, ch *model.NotifyChannel, m NotifyMessage) error
}

// permanentNotifyError 标记「重试也不会改变结果」的失败。
//
// 区分它是为了省掉无意义的等待：凭据错、被机器人拒收、收件人格式非法 ——
// 这些重试几次只是把同一个错误再撞几次，还让用户在"测试"按钮上多等十几秒。
// 反过来，网络抖动、HTTP 5xx、钉钉限流是**值得重试**的，它们会自己好。
type permanentNotifyError struct{ msg string }

func (e *permanentNotifyError) Error() string { return e.msg }

func permanent(format string, a ...any) error {
	return &permanentNotifyError{msg: fmt.Sprintf(format, a...)}
}

// --------------------------------------------------------------------------- 服务

// NotifyService 通知投递服务。
type NotifyService struct {
	deps    Deps
	timeout time.Duration // 单次投递超时（配置里的值是 config.Duration，这里转成 time 用）
	retry   int           // 额外重试次数
	senders map[string]notifier
	sem     chan struct{} // 投递并发上限，避免一批事件同时把对端打满
}

func newNotifyService(d Deps) *NotifyService {
	limit := d.Cfg.Notify.Concurrency
	if limit <= 0 {
		limit = 4
	}
	timeout := time.Duration(d.Cfg.Notify.Timeout)
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	retry := d.Cfg.Notify.Retry
	if retry < 0 {
		retry = 0
	}
	return &NotifyService{
		deps:    d,
		timeout: timeout,
		retry:   retry,
		senders: map[string]notifier{
			"email":    &emailNotifier{cfg: d.Cfg.Notify},
			"dingtalk": &dingtalkNotifier{},
		},
		sem: make(chan struct{}, limit),
	}
}

func (s *NotifyService) log() *slog.Logger {
	return s.deps.Log.With("component", "notify")
}

// Dispatch 把一条消息投递给所有订阅了该事件的启用渠道。
//
// 同步部分只做「查渠道 + 过滤 + 起 goroutine」，**永不返回错误**：
// 调用方是构建/部署的收尾路径，那里不该出现"因为通知失败所以任务失败"。
func (s *NotifyService) Dispatch(ctx context.Context, m NotifyMessage) {
	// 剥掉取消信号（见文件头约束 2）。后面查库、重试都挂在这个 ctx 上。
	ctx = context.WithoutCancel(ctx)

	var chans []model.NotifyChannel
	if err := s.deps.DB.WithContext(ctx).Where("enabled = ?", true).Find(&chans).Error; err != nil {
		s.log().Warn("加载启用的通知渠道失败，本次事件不推送", "event", m.Event, "err", err)
		return
	}
	for i := range chans {
		ch := chans[i]
		if !channelSubscribes(ch, m.Event) {
			continue
		}
		go s.deliver(ctx, ch, m)
	}
}

// SendTest 给单个渠道发一条测试消息，**同步返回真实结果**。
//
// 与 Dispatch 不同，这里必须把结果如实带回给用户 —— 他们按下「测试」就是想
// 知道"到底通不通"。所以：真的投递、真的解析对端返回、失败原因原样回显。
//
// 结果同样要写回 last_sent_at / fail_count / last_error：用户在「最近投递」列上
// 看到的必须与刚刚那次测试的实际结果一致，否则这一列就成了摆设。
func (s *NotifyService) SendTest(ctx context.Context, ch *model.NotifyChannel) error {
	sender, ok := s.senders[ch.Type]
	if !ok {
		msg := fmt.Sprintf("渠道类型 %s 尚未接入发送器：平台当前只支持邮件与钉钉", ch.Type)
		s.recordFailure(ch.ID, msg)
		return permanent("%s", msg)
	}
	c, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	err := sender.Send(c, ch, NotifyMessage{
		Event:  "",
		Title:  "云辇平台 · 通知渠道测试",
		Fields: []NotifyField{{Name: "渠道", Value: ch.Name}, {Name: "类型", Value: ch.Type}},
		Reason: "这是一条测试消息：收到它说明该渠道的发送链路是通的。",
	})
	if err != nil {
		s.recordFailure(ch.ID, err.Error())
		return err
	}
	s.recordSuccess(ch.ID)
	return nil
}

// deliver 单个渠道的投递与重试。
func (s *NotifyService) deliver(ctx context.Context, ch model.NotifyChannel, m NotifyMessage) {
	sender, ok := s.senders[ch.Type]
	if !ok {
		// 不静默跳过（见文件头约束 3）：把结论写进 last_error，界面上能看到。
		s.recordFailure(ch.ID, fmt.Sprintf("渠道类型 %s 尚未接入发送器（平台当前只支持邮件与钉钉）", ch.Type))
		return
	}

	attempts := s.retry + 1
	if attempts < 1 {
		attempts = 1
	}
	// 一次投递的总预算：单次超时 × 次数 + 退避余量。到点就放弃，
	// 不让一个坏掉的对端把 goroutine 挂在那里等。
	total := time.Duration(attempts)*s.timeout + time.Duration(attempts)*3*time.Second
	ctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				s.recordFailure(ch.ID, "投递超时："+ctx.Err().Error())
				return
			case <-time.After(time.Duration(i) * 2 * time.Second):
			}
		}
		err := s.sendOnce(ctx, sender, &ch, m)
		if err == nil {
			s.recordSuccess(ch.ID)
			return
		}
		lastErr = err
		var pe *permanentNotifyError
		if errors.As(err, &pe) {
			break // 重试不会变好，别浪费用户的时间
		}
	}
	s.recordFailure(ch.ID, lastErr.Error())
}

// sendOnce 带并发闸门与单次超时的一次投递。
func (s *NotifyService) sendOnce(ctx context.Context, sender notifier, ch *model.NotifyChannel, m NotifyMessage) error {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	c, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return sender.Send(c, ch, m)
}

// recordSuccess 投递成功：刷新 last_sent_at、清掉失败计数与上次的错误。
//
// 清 fail_count 是刻意的 —— 它表达的是"当前是否处于异常状态"，
// 不是"历史上失败过几次"。一个已经恢复的渠道不该永远挂着 3 次失败。
func (s *NotifyService) recordSuccess(id uint64) {
	at := now()
	if err := s.deps.DB.Model(&model.NotifyChannel{}).Where("id = ?", id).Updates(map[string]any{
		"last_sent_at": &at, "fail_count": 0, "last_error": "",
	}).Error; err != nil {
		s.log().Warn("回写通知渠道投递结果失败", "channelId", id, "err", err)
	}
}

// recordFailure 投递失败：累计失败次数并记下原因。
//
// last_error 是"配了没收到"唯一的排查线索，所以**每条失败路径都必须走到这里**，
// 包括"渠道类型没接发送器"这种"根本没发起投递"的情况。
func (s *NotifyService) recordFailure(id uint64, msg string) {
	if err := s.deps.DB.Model(&model.NotifyChannel{}).Where("id = ?", id).Updates(map[string]any{
		"fail_count": gorm.Expr("fail_count + 1"),
		"last_error": clipRunes(msg, 512),
		"updated_at": now(),
	}).Error; err != nil {
		s.log().Warn("回写通知渠道失败状态失败", "channelId", id, "err", err)
	}
	s.log().Warn("通知投递失败", "channelId", id, "err", msg)
}

// channelSubscribes 该渠道是否订阅了这个事件。
func channelSubscribes(ch model.NotifyChannel, ev NotifyEvent) bool {
	for _, code := range decodeStringList(ch.Events) {
		if code == string(ev) {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------------------- 消息构造

func (s *NotifyService) link(path string) string {
	base := strings.TrimRight(strings.TrimSpace(s.deps.Cfg.Notify.PublicBaseURL), "/")
	if base == "" {
		// 没配地址就不给链接。一个点不开的链接比没有链接更让人困惑。
		return ""
	}
	return base + path
}

// BuildFailedMessage 构造「构建失败」通知。
func (s *NotifyService) BuildFailedMessage(rec *model.BuildRecord) NotifyMessage {
	m := NotifyMessage{
		Event:  EventBuildFailed,
		Title:  fmt.Sprintf("构建失败 · %s #%d", rec.ProjectKey, rec.BuildNo),
		Level:  "error",
		Reason: firstNonEmpty(rec.ErrorMessage, codeHint(rec.ErrorCode)),
		Fields: []NotifyField{
			{Name: "项目", Value: rec.ProjectKey},
			{Name: "构建号", Value: "#" + strconv.FormatUint(uint64(rec.BuildNo), 10)},
			{Name: "分支", Value: rec.Branch},
			{Name: "提交", Value: shortCommit(rec.CommitID)},
			{Name: "触发人", Value: rec.TriggerName},
		},
		Link: s.link("/#/ci/builds"),
	}
	if rec.ErrorCode != "" {
		m.Fields = append(m.Fields, NotifyField{Name: "错误码", Value: rec.ErrorCode})
	}
	return m
}

// DeployFailedMessage 构造「部署失败」通知。
func (s *NotifyService) DeployFailedMessage(rec *model.DeployRecord) NotifyMessage {
	m := NotifyMessage{
		Event:  EventDeployFailed,
		Title:  fmt.Sprintf("部署失败 · %s/%s", rec.EnvKey, rec.ProjectKey),
		Level:  "error",
		Reason: firstNonEmpty(rec.ErrorMessage, codeHint(rec.ErrorCode)),
		Fields: []NotifyField{
			{Name: "环境", Value: rec.EnvKey},
			{Name: "项目", Value: rec.ProjectKey},
			{Name: "部署单", Value: rec.DeployNo},
			{Name: "镜像", Value: rec.Image},
			{Name: "操作人", Value: rec.OperatorName},
		},
		Link: s.link("/#/cd/deploys"),
	}
	if rec.ErrorCode != "" {
		m.Fields = append(m.Fields, NotifyField{Name: "错误码", Value: rec.ErrorCode})
	}
	return m
}

// DeployPendingMessage 构造「部署待审批」通知。
func (s *NotifyService) DeployPendingMessage(rec *model.DeployRecord) NotifyMessage {
	return NotifyMessage{
		Event:  EventDeployPendingApprove,
		Title:  fmt.Sprintf("部署待审批 · %s/%s", rec.EnvKey, rec.ProjectKey),
		Level:  "warning",
		Reason: "该环境开启了部署审批，需要审批人确认后才会开始发布。",
		Fields: []NotifyField{
			{Name: "环境", Value: rec.EnvKey},
			{Name: "项目", Value: rec.ProjectKey},
			{Name: "部署单", Value: rec.DeployNo},
			{Name: "镜像", Value: rec.Image},
			{Name: "申请人", Value: rec.OperatorName},
		},
		Link: s.link("/#/cd/deploys"),
	}
}

// BuildInterruptedMessage 构造「构建被平台重启中断」的汇总通知。
//
// 为什么是**汇总**而不是一条记录一条消息：一次启动补偿里被中断的构建，在运维上是
// 同一件事（平台重启了一次）。分开发会让人以为出了很多个问题，而队列容量是并发数 ×20，
// 一次重启理论上能中断几十条 —— 逐条发只会把告警群刷屏，结果是真正该看的那条被划过去。
//
// 传空切片时不投递：返回的消息 Event 有值但没有字段，调用方需自行跳过（见 Dispatch 调用点）。
func (s *NotifyService) BuildInterruptedMessage(recs []*model.BuildRecord) NotifyMessage {
	m := NotifyMessage{
		Event: EventBuildInterrupted,
		// 用 warning 而不是 error：这里没有任何东西"做错了"，也不是构建本身的问题。
		// 与界面上一致 —— 构建列表里 interrupted 的失败原因用的是 warning 色。
		Level:  "warning",
		Reason: "平台重启时这些构建还没跑完，执行随进程中消失而被中断。这类记录不会自行恢复，需要重新触发。",
		Link:   s.link("/#/ci/builds"),
	}
	if len(recs) == 0 {
		return m
	}
	if len(recs) == 1 {
		m.Title = fmt.Sprintf("构建被中断 · %s #%d", recs[0].ProjectKey, recs[0].BuildNo)
	} else {
		m.Title = fmt.Sprintf("构建被中断 · 共 %d 个", len(recs))
	}
	for i, r := range recs {
		if i >= interruptedListLimit {
			break
		}
		m.Fields = append(m.Fields, NotifyField{
			Name:  fmt.Sprintf("%s #%d", r.ProjectKey, r.BuildNo),
			Value: joinNonEmpty(" · ", r.Branch, commitHint(r.CommitID), r.TriggerName),
		})
	}
	if n := len(recs) - interruptedListLimit; n > 0 {
		m.Fields = append(m.Fields, NotifyField{
			Name:  "其余",
			Value: fmt.Sprintf("另有 %d 个构建同样被中断，可在构建记录里按「已中断」筛选", n),
		})
	}
	return m
}

// DeployInterruptedMessage 构造「部署被平台重启中断」的汇总通知。理由同构建侧。
func (s *NotifyService) DeployInterruptedMessage(recs []*model.DeployRecord) NotifyMessage {
	m := NotifyMessage{
		Event:  EventDeployInterrupted,
		Level:  "warning",
		Reason: "平台重启时这些部署还没跑完，执行随进程中消失而被中断。这类记录不会自行恢复，可在部署记录里重试。",
		Link:   s.link("/#/cd/deploys"),
	}
	if len(recs) == 0 {
		return m
	}
	if len(recs) == 1 {
		m.Title = fmt.Sprintf("部署被中断 · %s/%s", recs[0].EnvKey, recs[0].ProjectKey)
	} else {
		m.Title = fmt.Sprintf("部署被中断 · 共 %d 个", len(recs))
	}
	for i, r := range recs {
		if i >= interruptedListLimit {
			break
		}
		m.Fields = append(m.Fields, NotifyField{
			Name:  fmt.Sprintf("%s/%s", r.EnvKey, r.ProjectKey),
			Value: joinNonEmpty(" · ", r.DeployNo, r.Image, r.OperatorName),
		})
	}
	if n := len(recs) - interruptedListLimit; n > 0 {
		m.Fields = append(m.Fields, NotifyField{
			Name:  "其余",
			Value: fmt.Sprintf("另有 %d 个部署同样被中断，可在部署记录里按「已中断」筛选", n),
		})
	}
	return m
}

// commitHint 把提交号渲染成 "@abc1234"；没有提交号时返回空串。
//
// 不能直接拼 "@" + shortCommit(id)：webhook / 定时触发的记录可能没有提交号，
// 那样会渲染出一个孤零零的 "@"。渲染层会跳过空值，所以这里返回空串即可。
func commitHint(id string) string {
	if v := strings.TrimSpace(id); v != "" {
		return "@" + shortCommit(v)
	}
	return ""
}

// joinNonEmpty 用 sep 连接非空片段；全空时返回空串。
//
// 用于拼"一行事实"（分支 · 提交 · 触发人）：钉钉与邮件两套渲染都会跳过空值字段，
// 所以拼的时候也不能留下多余的分隔符 —— 否则消息里会出现 " ·  · 运维示例" 这种东西。
func joinNonEmpty(sep string, parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return strings.Join(out, sep)
}

// codeHint 没有人类可读原因时的兜底：至少把机器码摆出来，但**标明它是码**。
//
// 早于"失败原因落库"的历史记录只有 error_code，没有 error_message。
// 直接显示一个 APPLY_FAILED 会被当成"系统说的话"，所以这里加了措辞区分。
func codeHint(code string) string {
	if code == "" {
		return "这条记录产生于失败原因落库之前，当时的原因没有保存下来"
	}
	return "这条记录产生于失败原因落库之前，平台只留下了错误码：" + code
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// --------------------------------------------------------------------------- 渲染

// renderPlainText 邮件正文。
func renderPlainText(m NotifyMessage) string {
	var b strings.Builder
	b.WriteString(m.Title + "\n")
	b.WriteString(strings.Repeat("=", 48) + "\n\n")
	for _, f := range m.Fields {
		if strings.TrimSpace(f.Value) == "" {
			continue
		}
		fmt.Fprintf(&b, "%s：%s\n", f.Name, f.Value)
	}
	if m.Reason != "" {
		fmt.Fprintf(&b, "\n原因：%s\n", m.Reason)
	}
	if m.Link != "" {
		fmt.Fprintf(&b, "\n查看详情：%s\n", m.Link)
	}
	b.WriteString("\n—— 云辇平台自动发送，请勿直接回复。\n")
	return b.String()
}

// renderDingtalkMarkdown 钉钉 markdown 正文。
//
// 钉钉的 markdown 是**子集**：支持 ### 标题、- 列表、**加粗**、> 引用、[文字](链接)。
// 用别的语法（表格、代码块高亮）不会报错，只会原样显示出符号，看着像坏掉了。
func renderDingtalkMarkdown(m NotifyMessage) string {
	var b strings.Builder
	b.WriteString("### " + m.Title + "\n\n")
	for _, f := range m.Fields {
		if strings.TrimSpace(f.Value) == "" {
			continue
		}
		fmt.Fprintf(&b, "- **%s**：%s\n", f.Name, f.Value)
	}
	if m.Reason != "" {
		fmt.Fprintf(&b, "\n> **原因**：%s\n", m.Reason)
	}
	if m.Link != "" {
		fmt.Fprintf(&b, "\n[查看详情](%s)\n", m.Link)
	}
	return b.String()
}

// --------------------------------------------------------------------------- 钉钉

const (
	// errCodeSendTooFast 钉钉机器人的发送频率限制（20 条/分钟）。属于"会自己好"的一类。
	errCodeSendTooFast = 410100
	// errCodeSendLimit 另一个限流相关码，钉钉在不同入口返回过它。
	errCodeSendLimit = 130101
)

type dingtalkNotifier struct{}

func (d *dingtalkNotifier) Kind() string { return "dingtalk" }

// Send 通过钉钉自定义机器人 Webhook 发送 markdown 消息。
//
// 两处必须处理的现实：
//
//  1. **加签**。机器人安全设置选「加签」时，URL 里只有 access_token 会被拒成
//     errcode=310000「机器人发送签名不匹配」。签名算法是：
//     sign = urlEncode(base64(HMAC-SHA256(timestamp + "\n" + secret, secret)))
//     加签密钥在钉钉后台是 `SEC` 开头的串，**不在** webhook URL 里。
//     平台的约定是把目标写成：
//     https://oapi.dingtalk.com/robot/send?access_token=xxx&secret=SECxxxx
//     发送前取出 secret 并**从请求 URL 里删掉** —— 钉钉不认这个参数，
//     但留着会把它带进访问日志。
//
//  2. **HTTP 200 也可能是失败**。钉钉无论成败都返回 200，真结果在 body 的 errcode。
//     不解析它就会把"签名错 / 关键词不匹配 / 被限流"一律记成发送成功，
//     渠道上还会刷新出一个漂亮的 last_sent_at。
func (d *dingtalkNotifier) Send(ctx context.Context, ch *model.NotifyChannel, m NotifyMessage) error {
	raw := strings.TrimSpace(ch.Target)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return permanent("钉钉机器人地址无法解析：%s", raw)
	}
	q := u.Query()
	secret := strings.TrimSpace(q.Get("secret"))
	if secret != "" {
		q.Del("secret")
		ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
		q.Set("timestamp", ts)
		q.Set("sign", dingtalkSign(ts, secret))
	}
	u.RawQuery = q.Encode()

	payload := map[string]any{
		"msgtype": "markdown",
		"markdown": map[string]string{
			"title": m.Title,
			"text":  renderDingtalkMarkdown(m),
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return permanent("构造钉钉消息失败：%v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return permanent("构造钉钉请求失败：%v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := notifyHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("请求钉钉失败：%w", err) // 网络类，可重试
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("钉钉返回 HTTP %d：%s", resp.StatusCode, clipRunes(string(respBody), 200))
	}

	var out struct {
		ErrCode int    `json:"errcode"`
		ErrMsg  string `json:"errmsg"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return fmt.Errorf("钉钉返回了无法解析的响应：%s", clipRunes(string(respBody), 200))
	}
	if out.ErrCode != 0 {
		if out.ErrCode == errCodeSendTooFast || out.ErrCode == errCodeSendLimit {
			return fmt.Errorf("钉钉限流：errcode=%d %s", out.ErrCode, out.ErrMsg)
		}
		// 其余都是"重试没意义"的：签名不匹配、关键词不在内容里、token 失效、机器人被移出群。
		return permanent("钉钉拒收了这条消息：errcode=%d %s", out.ErrCode, out.ErrMsg)
	}
	return nil
}

// dingtalkSign 按钉钉官方算法生成签名。
//
// ⚠️ 返回值**没有**做 URL 编码。调用方是通过 url.Values.Encode() 拼进 query 的，
// 那里会做一次标准转义 —— base64 里的 `+` `/` `=` 必须被转义，手工拼字符串
// 极容易漏掉（漏了就会得到 310000 签名不匹配，且看起来算法完全正确）。
func dingtalkSign(timestamp, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "\n" + secret))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// --------------------------------------------------------------------------- 邮件

type emailNotifier struct {
	cfg config.NotifyConfig
}

func (e *emailNotifier) Kind() string { return "email" }

// Send 通过 SMTP 投递一封纯文本通知邮件。
func (e *emailNotifier) Send(ctx context.Context, ch *model.NotifyChannel, m NotifyMessage) error {
	ec := e.cfg.Email
	if strings.TrimSpace(ec.Host) == "" {
		return permanent("平台没有配置邮件服务器（notify.email.host），无法投递邮件通知")
	}
	to := splitAddresses(ch.Target)
	if len(to) == 0 {
		return permanent("邮件渠道没有填写收件人")
	}
	from := strings.TrimSpace(ec.From)
	if from == "" {
		from = ec.Username
	}
	if from == "" {
		return permanent("平台没有配置发件人地址（notify.email.from）")
	}

	client, err := e.dial(ctx, ec)
	if err != nil {
		return fmt.Errorf("连接邮件服务器失败：%w", err) // 网络类，可重试
	}
	defer func() { _ = client.Close() }()

	if ec.Auth {
		if ok, _ := client.Extension("AUTH"); !ok {
			return permanent("邮件服务器未提供 AUTH，但配置要求鉴权（notify.email.auth）")
		}
		if err := client.Auth(smtp.PlainAuth("", ec.Username, ec.Password, ec.Host)); err != nil {
			return permanent("邮件服务器拒绝登录：%v（请核对 notify.email 里的账号与授权码）", err)
		}
	}
	if err := client.Mail(from); err != nil {
		return permanent("邮件服务器拒绝发件人 %s：%v", from, err)
	}
	for _, addr := range to {
		if err := client.Rcpt(addr); err != nil {
			return permanent("邮件服务器拒绝收件人 %s：%v", addr, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("开始写邮件正文失败：%w", err)
	}
	if _, err := w.Write(buildEmail(ec, from, to, m)); err != nil {
		_ = w.Close()
		return fmt.Errorf("写邮件正文失败：%w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("提交邮件正文失败：%w", err)
	}
	// 退出前主动 QUIT：不 QUIT 直接断连接，部分服务端会把这次会话记成异常，
	// 长期看会影响发信信誉。
	if err := client.Quit(); err != nil {
		var pe *permanentNotifyError
		if errors.As(err, &pe) {
			return err
		}
	}
	return nil
}

// dial 建立 SMTP 连接。
//
// 两种协议必须分开处理，配错的表现很难懂：
//   - smtps（465）：连上就是 TLS。若按明文连，对端在等 TLS 握手、你在等 220 问候，
//     最后只会看到一个没有上下文的超时或 EOF。
//   - starttls（587）：明文连上、发 STARTTLS、再升级。若按 smtps 连，
//     握手会失败在证书阶段。
func (e *emailNotifier) dial(ctx context.Context, ec config.NotifyEmailConfig) (*smtp.Client, error) {
	port := ec.Port
	if port == 0 {
		port = 465
	}
	addr := net.JoinHostPort(ec.Host, strconv.Itoa(port))

	// 用 DialContext + 手动握手，而不是 tls.DialWithDialer：
	// 后者不接受 context，超时只能靠系统默认，用户点了"测试"要干等很久。
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// SMTP 是请求-响应式的，给连接套一个 deadline 防止对端半死不活地挂着。
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	useTLS := !strings.EqualFold(ec.Protocol, "starttls")
	if useTLS {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName:         ec.Host,
			InsecureSkipVerify: ec.InsecureSkipVerify,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("TLS 握手失败：%w", err)
		}
		conn = tlsConn
	}

	// smtp.NewClient 会读 220 问候；对 *tls.Conn 会自动识别为已加密连接。
	c, err := smtp.NewClient(conn, ec.Host)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if !useTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			_ = c.Close()
			return nil, permanent("邮件服务器不支持 STARTTLS，但配置的协议是 starttls")
		}
		if err := c.StartTLS(&tls.Config{
			ServerName:         ec.Host,
			InsecureSkipVerify: ec.InsecureSkipVerify,
		}); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("STARTTLS 失败：%w", err)
		}
	}
	return c, nil
}

// buildEmail 拼一封 MIME 邮件。
//
// 两处编码不能省：
//   - 主题含中文必须做 encoded-word（=?UTF-8?B?...?=），否则在多数邮件客户端里是乱码。
//     这里用 mime.QEncoding 而不是手写，是因为它还会处理折行（长主题超过 76 字符要折）。
//   - 正文用 base64：SMTP 协议本身不允许 8bit 内容，直接塞 UTF-8 会在
//     部分服务端把正文截断在第一个非 ASCII 字节。
func buildEmail(ec config.NotifyEmailConfig, from string, to []string, m NotifyMessage) []byte {
	var b bytes.Buffer
	fromHeader := from
	if name := strings.TrimSpace(ec.FromName); name != "" {
		fromHeader = mime.QEncoding.Encode("UTF-8", name) + " <" + from + ">"
	}
	fmt.Fprintf(&b, "From: %s\r\n", fromHeader)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("UTF-8", "[云辇] "+m.Title))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")

	encoded := base64.StdEncoding.EncodeToString([]byte(renderPlainText(m)))
	for len(encoded) > 76 {
		b.WriteString(encoded[:76] + "\r\n")
		encoded = encoded[76:]
	}
	b.WriteString(encoded + "\r\n")
	return b.Bytes()
}

// splitAddresses 拆收件人。用户习惯用逗号，但也见过分号、换行甚至空格，
// 与其报一个"格式非法"，不如都认。
func splitAddresses(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if v := strings.TrimSpace(f); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// notifyHTTPClient 通知投递用的 HTTP 客户端。
//
// 刻意**不复用**平台其他下游的客户端：那些带重试、带日志脱敏、按下游超时调优，
// 而通知是一次性的旁路调用，超时由调用方的 ctx 决定。混用会让"通知慢"
// 变成"下游调用慢"。
var notifyHTTPClient = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 4,
	},
}
