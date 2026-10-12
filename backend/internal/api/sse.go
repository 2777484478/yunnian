// Package api 承载 HTTP 层：路由表、请求解析与响应写出。
//
// 分层约定：handler 里**不写业务判断**。它只做三件事：
//  1. 绑定/校验入参（结构体上的 binding 标签）
//  2. 组装 service.Caller（用户/IP/UA/traceId）
//  3. 把 service 的返回值或错误统一写出
//
// 这样 service 层可以完全不依赖 gin，单测时不需要起 HTTP 服务。
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/logstream"
	"github.com/2777484478/yunnian/internal/middleware"
	"github.com/2777484478/yunnian/internal/service"
)

// sseHeartbeat SSE 心跳间隔。
//
// 为什么必须有：Nginx / 各类网关对"长时间无数据的连接"有默认空闲超时（常见 60s），
// 日志安静的构建（比如正在装依赖）几十秒没有输出是常态，
// 不发心跳就会被动断连，前端表现为"日志卡住然后重连"。
const sseHeartbeat = 20 * time.Second

// sseRetryMs 告诉浏览器断线后多久重连（毫秒）。前端不写死这个值，
// 因为重连节奏属于服务端与网关的约定（docs/03 §0.4）。
const sseRetryMs = 3000

// caller 组装 service 调用方信息。
//
// 每个 handler 都要构造它，单独抽出来是为了保证「审计四要素」不会在某个 handler 里漏掉。
func caller(c *gin.Context) service.Caller {
	return service.Caller{
		User:    middleware.CurrentUser(c),
		IP:      httpx.ClientIP(c),
		UA:      httpx.UserAgent(c),
		TraceID: httpx.TraceID(c),
	}
}

// --------------------------------------------------------------------------- SSE 写出

// sseStream 封装 SSE 响应：负责响应头、帧格式与 flush。
//
// 日志流（有 seq、可续传）与 AI 流式回答（无 seq、不可续传）共用它，
// 差别只在于是否带 id 字段——带 id 是给浏览器 EventSource 做 Last-Event-ID 用的。
type sseStream struct {
	c       *gin.Context
	started bool
}

// newSSEStream 写好响应头并返回写出器。
//
// 注意：这里**不设置 Content-Length**，也**不能**再调用 c.JSON——
// 一旦开始写 SSE，HTTP 状态码就已经是 200 了，出错只能靠 event: error 表达。
func newSSEStream(c *gin.Context) *sseStream {
	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache, no-transform")
	c.Header("Connection", "keep-alive")
	// 告诉 Nginx 不要缓冲响应——否则日志会攒一批才吐出来，
	// 用户看到的是"每隔十几秒刷一大段"而不是实时滚动。
	c.Header("X-Accel-Buffering", "no")
	c.Status(http.StatusOK)

	s := &sseStream{c: c}
	// retry 行：断线后浏览器按这个间隔重连
	_, _ = c.Writer.WriteString("retry: " + strconv.Itoa(sseRetryMs) + "\n\n")
	c.Writer.Flush()
	s.started = true
	return s
}

// send 写出一条不带 id 的事件。
func (s *sseStream) send(event string, payload any) bool {
	return s.write("", event, payload)
}

// sendSeq 写出一条带 id 的事件（日志流用，id 即 seq）。
func (s *sseStream) sendSeq(seq uint32, event string, payload any) bool {
	return s.write(strconv.FormatUint(uint64(seq), 10), event, payload)
}

// write 组装并写出一个 SSE 帧。返回 false 表示连接已断，调用方应立即停止。
func (s *sseStream) write(id, event string, payload any) bool {
	raw, err := json.Marshal(payload)
	if err != nil {
		// payload 由本包构造，理论上不会失败；真失败就发一条 error 事件而不是静默吞掉。
		raw = []byte(`{"message":"事件序列化失败"}`)
		event = "error"
	}
	var b strings.Builder
	if id != "" {
		b.WriteString("id: ")
		b.WriteString(id)
		b.WriteByte('\n')
	}
	b.WriteString("event: ")
	b.WriteString(event)
	b.WriteString("\ndata: ")
	// data 必须是单行：内容里有换行时按 SSE 规范要拆成多个 data 行，
	// 这里的 payload 是 JSON（换行已被转义成 \n），所以直接写。
	b.Write(raw)
	b.WriteString("\n\n")

	if _, err := s.c.Writer.WriteString(b.String()); err != nil {
		return false
	}
	s.c.Writer.Flush()
	return true
}

// ping 发送心跳注释行。SSE 注释（以冒号开头）会被客户端忽略，但足以保活链路。
func (s *sseStream) ping() bool {
	if _, err := s.c.Writer.WriteString(": ping\n\n"); err != nil {
		return false
	}
	s.c.Writer.Flush()
	return true
}

// --------------------------------------------------------------------------- 日志流

// logBacklog 读取已落库的历史日志，用于订阅建立时补发。
// 返回的事件按 seq 升序，且都是 seq > afterSeq 的行。
type logBacklog func(ctx context.Context, afterSeq uint32, limit int) []logstream.Event

// sseBacklogLimit 单次补发的历史日志上限。
// 2000 行足够覆盖一次构建的完整输出（典型 60~200 行），
// 又不会让首屏被一个异常长的日志拖住；超出的部分前端可用 /logs 接口翻。
const sseBacklogLimit = 2000

// streamLogs 把一个日志流按 SSE 推给客户端。
//
// 协议要点（docs/03 §0.4）：
//   - 每条日志带 `id: {seq}`，客户端断线后把最后收到的 seq 放进
//     `Last-Event-ID` 请求头，服务端从 seq+1 续传，日志不会出现空洞；
//   - `event:` 区分 log / status / end 三类。
//
// backlog 用于在接入实时流之前，先把**已落库**的历史推给客户端。
// 为什么需要：日志流的缓冲区是双份易失的——进程内实现只留最近 4000 行，
// Redis Stream 带 TTL，且平台一重启就全丢。而 build_log 表是完整的。
// 不补的话会出现一种很难解释的现象：一条"构建中"的记录（例如上次进程被中断
// 留下的），点开日志抽屉是**空的**，可日志明明在库里。
func streamLogs(c *gin.Context, stream logstream.Stream, key string, afterSeq uint32, backlog logBacklog) {
	// 从 Last-Event-ID 恢复续传位置（浏览器 EventSource 断线重连时会自动带上）。
	if raw := c.GetHeader("Last-Event-ID"); raw != "" {
		if v, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 32); err == nil {
			afterSeq = uint32(v)
		}
	}

	s := newSSEStream(c)
	ctx := c.Request.Context()

	if backlog != nil {
		for _, ev := range backlog(ctx, afterSeq, sseBacklogLimit) {
			if !s.sendLogEvent(ev) {
				return
			}
			if ev.Seq > afterSeq {
				afterSeq = ev.Seq
			}
		}
	}

	// 用补发后的 afterSeq 订阅：日志流自身也会重放缓冲里的历史，
	// 但它按 `seq <= afterSeq` 过滤，所以不会和上面补发的行重复。
	// 反过来说，这一步也堵住了"补发与订阅之间新产生的行"这个窗口——
	// 那些行的 seq 大于 afterSeq，会被流的历史重放带出来。
	events, cancel := stream.Subscribe(ctx, key, afterSeq)
	defer cancel()

	ticker := time.NewTicker(sseHeartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.ping() {
				return
			}
		case ev, ok := <-events:
			if !ok {
				return
			}
			if !s.sendLogEvent(ev) {
				return
			}
		}
	}
}

// sendLogEvent 把一条日志流事件翻译成 SSE 帧。
func (s *sseStream) sendLogEvent(ev logstream.Event) bool {
	switch ev.Kind {
	case logstream.KindStatus:
		payload := map[string]any{"seq": ev.Seq}
		if ev.Status != nil {
			payload["status"] = ev.Status.Status
			payload["progress"] = ev.Status.Progress
			if ev.Status.Stage != "" {
				payload["stage"] = ev.Status.Stage
			}
			if ev.Status.Duration > 0 {
				payload["duration"] = ev.Status.Duration
			}
			if ev.Status.Image != "" {
				payload["image"] = ev.Status.Image
			}
		}
		return s.writeSeq(ev.Seq, logstream.KindStatus, payload)

	case logstream.KindEnd:
		return s.sendSeq(ev.Seq, logstream.KindEnd, map[string]any{"reason": ev.Reason})

	default:
		// 状态事件不占 seq，所以这里对 0 值做兜底，避免前端拿到 id: 0。
		payload := map[string]any{
			"seq":   ev.Seq,
			"level": ev.Level,
			"line":  ev.Line,
			"ts":    ev.At.Format(time.RFC3339),
		}
		if ev.Seq == 0 {
			return s.send(logstream.KindLog, payload)
		}
		return s.sendSeq(ev.Seq, logstream.KindLog, payload)
	}
}

// writeSeq 与 write 相同，但强制带 id（即使 seq 为 0）。
func (s *sseStream) writeSeq(seq uint32, event string, payload any) bool {
	return s.write(strconv.FormatUint(uint64(seq), 10), event, payload)
}
