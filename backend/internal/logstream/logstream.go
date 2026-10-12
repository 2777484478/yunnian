// Package logstream 提供任务日志的实时通道，是 SSE 接口的数据底座。
//
// 为什么抽象成接口：Redis Stream 与进程内缓冲在能力上差异很大，
// 但 SSE 消费方只关心三件事——「追加一行」「从某个 seq 续传」「感知流结束」。
// 把差异收敛在这里，带来两个好处：
//   - 没有 Redis 的环境（本地开发、单机部署）平台依然可用；
//   - 有 Redis 时换成共享通道，多实例部署下 Runner 与平台不在同一进程也能推流。
//
// seq 语义与 docs/03 §0.4 的 SSE 约定对齐：seq 是**单调递增**的行号，
// 客户端断线后用 Last-Event-ID 带上最后收到的 seq，服务端从 seq+1 继续推送。
package logstream

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrClosed 表示流已结束。
var ErrClosed = errors.New("日志流已结束")

// 事件类型，对应 SSE 的 event 字段。
const (
	KindLog    = "log"
	KindStatus = "status"
	KindEnd    = "end"
)

// Status 一次状态变更，对应 SSE 的 status 事件。
type Status struct {
	Status   string `json:"status"`
	Progress uint8  `json:"progress"`
	Stage    string `json:"stage,omitempty"`
	Duration uint32 `json:"duration,omitempty"`
	Image    string `json:"image,omitempty"`
}

// Event 流中的一条事件。
type Event struct {
	Seq    uint32
	Kind   string
	Level  string // info/warn/error/cmd，仅 KindLog 有效
	Line   string
	At     time.Time
	Status *Status
	Reason string // 仅 KindEnd 有效：finished/canceled/timeout
}

// Stream 日志流接口。
type Stream interface {
	// Append 追加一行日志，返回该行的 seq。
	Append(ctx context.Context, key string, level, line string, at time.Time) (uint32, error)

	// SetStatus 推送一次状态变更（不占 seq）。
	SetStatus(ctx context.Context, key string, st Status) error

	// Finish 标记流结束并写明原因。幂等。
	Finish(ctx context.Context, key string, reason string) error

	// Finished 查询流是否已结束，返回结束原因。
	Finished(ctx context.Context, key string) (bool, string)

	// Subscribe 从 afterSeq 之后开始消费。
	// 返回的事件 channel 会在流结束（收到 end 事件）后关闭；cancel 用于提前退出。
	Subscribe(ctx context.Context, key string, afterSeq uint32) (<-chan Event, func())

	// Snapshot 一次性读取已缓冲的历史事件，用于「流已结束，直接返回全量」的场景。
	Snapshot(ctx context.Context, key string) []Event

	// Reset 清空某个 key 的缓冲（重新执行同一任务时使用）。
	Reset(ctx context.Context, key string)
}

// --------------------------------------------------------------------------- 进程内实现

// memBufferSize 单个任务保留的日志行数上限。
// 超出后丢弃最早的行——SSE 场景下客户端只关心最近的输出与断线续传的连续性，
// 完整日志以数据库的 build_log 为准。
const memBufferSize = 4000

// MemStream 进程内实现。适用于单实例部署与本地开发。
type MemStream struct {
	mu     sync.RWMutex
	topics map[string]*topic
}

type topic struct {
	mu       sync.Mutex
	events   []Event
	seq      uint32
	finished bool
	reason   string
	status   Status
	subs     map[int]chan Event
	nextSub  int
}

// NewMemStream 构造进程内日志流。
func NewMemStream() *MemStream {
	return &MemStream{topics: make(map[string]*topic)}
}

func (m *MemStream) topic(key string) *topic {
	m.mu.RLock()
	t, ok := m.topics[key]
	m.mu.RUnlock()
	if ok {
		return t
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok = m.topics[key]; ok {
		return t
	}
	t = &topic{subs: make(map[int]chan Event), status: Status{Status: "queued", Progress: 0}}
	m.topics[key] = t
	return t
}

// Append 追加一行日志。
func (m *MemStream) Append(_ context.Context, key string, level, line string, at time.Time) (uint32, error) {
	t := m.topic(key)
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.finished {
		return t.seq, ErrClosed
	}
	t.seq++
	ev := Event{Seq: t.seq, Kind: KindLog, Level: level, Line: line, At: at}
	t.push(ev)
	return t.seq, nil
}

// SetStatus 推送状态变更。
func (m *MemStream) SetStatus(_ context.Context, key string, st Status) error {
	t := m.topic(key)
	t.mu.Lock()
	defer t.mu.Unlock()

	t.status = st
	// 状态事件不占 seq：它不属于日志行序列，断线续传时只需要最后一次状态，
	// 而最后一次状态可以从任务记录里重新读到（见 service 层）。
	t.push(Event{Seq: t.seq, Kind: KindStatus, At: time.Now(), Status: &st})
	return nil
}

// Finish 结束流。
func (m *MemStream) Finish(_ context.Context, key string, reason string) error {
	t := m.topic(key)
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.finished {
		return nil
	}
	t.finished = true
	t.reason = reason
	t.push(Event{Seq: t.seq, Kind: KindEnd, At: time.Now(), Reason: reason})
	// 广播完毕即可关闭订阅者：push 已经把事件投递到各自的 channel
	for id, ch := range t.subs {
		close(ch)
		delete(t.subs, id)
	}
	return nil
}

// Finished 查询结束状态。
func (m *MemStream) Finished(_ context.Context, key string) (bool, string) {
	t := m.topic(key)
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.finished, t.reason
}

// Snapshot 读取历史事件副本。
func (m *MemStream) Snapshot(_ context.Context, key string) []Event {
	t := m.topic(key)
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Event, len(t.events))
	copy(out, t.events)
	return out
}

// Subscribe 订阅增量事件。
func (m *MemStream) Subscribe(ctx context.Context, key string, afterSeq uint32) (<-chan Event, func()) {
	t := m.topic(key)
	ch := make(chan Event, 256)

	t.mu.Lock()
	// 先补历史：断线续传时把漏掉的部分一次给出，避免客户端出现日志空洞
	backlog := make([]Event, 0, len(t.events))
	for _, ev := range t.events {
		if ev.Kind == KindLog && ev.Seq <= afterSeq {
			continue
		}
		if ev.Kind == KindStatus {
			continue // 历史状态无意义，只补日志与结束标记
		}
		backlog = append(backlog, ev)
	}
	// 未结束时补一次当前状态，让客户端一进来就能渲染进度
	if !t.finished {
		backlog = append(backlog, Event{Seq: t.seq, Kind: KindStatus, At: time.Now(), Status: ptrStatus(t.status)})
	}

	alreadyFinished, reason := t.finished, t.reason
	if !alreadyFinished {
		id := t.nextSub
		t.nextSub++
		t.subs[id] = ch

		var once sync.Once
		cancel := func() {
			once.Do(func() {
				t.mu.Lock()
				if c, ok := t.subs[id]; ok {
					delete(t.subs, id)
					close(c)
				}
				t.mu.Unlock()
			})
		}
		t.mu.Unlock()

		go func() {
			for _, ev := range backlog {
				select {
				case ch <- ev:
				case <-ctx.Done():
					cancel()
					return
				}
			}
		}()
		return ch, cancel
	}

	// 已经结束：补完全部历史后立即关闭，让客户端拿到完整日志
	t.mu.Unlock()
	go func() {
		defer close(ch)
		for _, ev := range backlog {
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
		ch <- Event{Seq: t.seq, Kind: KindEnd, At: time.Now(), Reason: reason}
	}()
	return ch, func() {}
}

// Reset 清空缓冲。
func (m *MemStream) Reset(_ context.Context, key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.topics[key]; ok {
		t.mu.Lock()
		for id, ch := range t.subs {
			close(ch)
			delete(t.subs, id)
		}
		t.mu.Unlock()
	}
	delete(m.topics, key)
}

// push 在持锁状态下把事件写入缓冲并广播。调用方必须已持有 t.mu。
func (t *topic) push(ev Event) {
	t.events = append(t.events, ev)
	if len(t.events) > memBufferSize {
		// 丢弃最早的一批，保留最近窗口
		drop := len(t.events) - memBufferSize
		t.events = append(t.events[:0], t.events[drop:]...)
	}
	for _, ch := range t.subs {
		select {
		case ch <- ev:
		default:
			// 订阅者消费过慢时丢弃该条，避免阻塞任务执行线程。
			// 客户端可用 seq 缺口感知并重新拉取全量日志。
		}
	}
}

func ptrStatus(s Status) *Status { return &s }

// Key 生成日志流的键名，与 docs/02 §7 的 Redis Key 设计保持一致：
// build:1183:log / deploy:47:log。
func Key(kind string, id uint64) string {
	if kind == "" {
		kind = "build"
	}
	return kind + ":" + itoa(id) + ":log"
}

func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
