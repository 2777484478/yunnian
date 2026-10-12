package cache

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/yunnian/yunnian/internal/logstream"
)

// 本文件实现基于 Redis 的日志流，是 logstream.Stream 的分布式版本。
//
// 为什么不用 Redis Stream 的消费组（XGROUP/XREADGROUP）：
//   日志流是**广播**语义——同一条日志所有在线浏览器都要看到，
//   而消费组是"一条消息只投给一个消费者"。用朴素 XADD + XREAD（各自记 lastID）
//   才是对的模型，也让 SSE 断线续传天然成立。
//
// 键设计（prefix 默认 yn:，key 形如 build:1183:log）：
//
//	yn:seq:{key}      INCR 计数器     单调 seq，与 SSE 的 Last-Event-ID 对齐
//	yn:log:{key}      STREAM          日志条目，MAXLEN 限长 + TTL 过期
//	yn:status:{key}   STRING(JSON)    最新状态快照（重连时直接读它渲染进度）
//	yn:end:{key}      STRING          结束原因，存在即表示流已结束
//
// 这里刻意**不依赖 Redis 版本特性**（不用 XAUTOCLAIM / 消费组）：平台要能跑
// 在公司那台 4.0 的老 Redis 上，只要 XADD/XREAD 可用就够。

const (
	// streamMaxLen 单个任务的保留条目数。与进程内实现的上限保持一致，
	// 避免"换实现后客户端行为变了"这种隐蔽差异。
	streamMaxLen = 4000
	// streamReadBlock 阻塞读的超时。设短一点是为了及时响应 ctx 取消。
	streamReadBlock = 2 * time.Second
)

func (c *Cache) seqKey(key string) string    { return c.key("seq", key) }
func (c *Cache) streamKey(key string) string { return c.key("log", key) }
func (c *Cache) statusKey(key string) string { return c.key("status", key) }
func (c *Cache) endKey(key string) string    { return c.key("end", key) }

// Append 追加一行日志，返回单调递增的 seq。
func (c *Cache) Append(ctx context.Context, key string, level, line string, at time.Time) (uint32, error) {
	if !c.Enabled() {
		return 0, errors.New("redis 未启用")
	}
	// 先拿 seq：即使 XADD 失败，seq 也只是"跳号"，
	// 不会造成两条日志共用一个 seq（那才会破坏客户端的续传逻辑）。
	seq, err := c.rdb.Incr(ctx, c.seqKey(key)).Result()
	if err != nil {
		return 0, err
	}
	if seq == 1 {
		_ = c.rdb.Expire(ctx, c.seqKey(key), c.streamTTL).Err()
	}

	sk := c.streamKey(key)
	pipe := c.rdb.Pipeline()
	pipe.XAdd(ctx, &redis.XAddArgs{
		Stream: sk,
		MaxLen: streamMaxLen,
		Approx: true,
		Values: map[string]any{
			"seq":   strconv.FormatInt(seq, 10),
			"kind":  logstream.KindLog,
			"level": level,
			"line":  line,
			"at":    strconv.FormatInt(at.UnixMilli(), 10),
		},
	})
	pipe.Expire(ctx, sk, c.streamTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return uint32(seq), err
	}
	return uint32(seq), nil
}

// SetStatus 推送状态变更。
//
// 状态同时写「快照键」与「流条目」：
//   - 快照键让新连上来的客户端一次 GET 就拿到进度，不必翻整条流；
//   - 流条目让已连上的客户端按序收到变更。
func (c *Cache) SetStatus(ctx context.Context, key string, st logstream.Status) error {
	if !c.Enabled() {
		return errors.New("redis 未启用")
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	seq, _ := c.rdb.Get(ctx, c.seqKey(key)).Int64()

	pipe := c.rdb.Pipeline()
	pipe.Set(ctx, c.statusKey(key), raw, c.streamTTL)
	pipe.XAdd(ctx, &redis.XAddArgs{
		Stream: c.streamKey(key),
		MaxLen: streamMaxLen,
		Approx: true,
		Values: map[string]any{
			"seq":    strconv.FormatInt(seq, 10),
			"kind":   logstream.KindStatus,
			"status": string(raw),
			"at":     strconv.FormatInt(time.Now().UnixMilli(), 10),
		},
	})
	pipe.Expire(ctx, c.streamKey(key), c.streamTTL)
	_, err = pipe.Exec(ctx)
	return err
}

// Finish 标记流结束。幂等。
func (c *Cache) Finish(ctx context.Context, key string, reason string) error {
	if !c.Enabled() {
		return errors.New("redis 未启用")
	}
	// SETNX 保证"第一个结束原因"胜出：任务既被取消又刚好超时时，
	// 谁先到谁说了算，不会出现原因被后到的覆盖成另一个。
	ok, err := c.rdb.SetNX(ctx, c.endKey(key), reason, c.streamTTL).Result()
	if err != nil || !ok {
		return err
	}
	seq, _ := c.rdb.Get(ctx, c.seqKey(key)).Int64()
	pipe := c.rdb.Pipeline()
	pipe.XAdd(ctx, &redis.XAddArgs{
		Stream: c.streamKey(key),
		MaxLen: streamMaxLen,
		Approx: true,
		Values: map[string]any{
			"seq":    strconv.FormatInt(seq, 10),
			"kind":   logstream.KindEnd,
			"reason": reason,
			"at":     strconv.FormatInt(time.Now().UnixMilli(), 10),
		},
	})
	pipe.Expire(ctx, c.streamKey(key), c.streamTTL)
	_, err = pipe.Exec(ctx)
	return err
}

// Finished 查询结束状态。
func (c *Cache) Finished(ctx context.Context, key string) (bool, string) {
	if !c.Enabled() {
		return false, ""
	}
	reason, err := c.rdb.Get(ctx, c.endKey(key)).Result()
	if err != nil {
		return false, ""
	}
	return true, reason
}

// Snapshot 读取全量历史事件。
func (c *Cache) Snapshot(ctx context.Context, key string) []logstream.Event {
	if !c.Enabled() {
		return nil
	}
	entries, err := c.rdb.XRange(ctx, c.streamKey(key), "-", "+").Result()
	if err != nil {
		return nil
	}
	out := make([]logstream.Event, 0, len(entries))
	for _, e := range entries {
		if ev, ok := decodeEntry(e); ok {
			out = append(out, ev)
		}
	}
	return out
}

// Reset 清掉某个键上的全部痕迹（重跑同一任务时使用）。
func (c *Cache) Reset(ctx context.Context, key string) {
	if !c.Enabled() {
		return
	}
	_ = c.rdb.Del(ctx, c.seqKey(key), c.streamKey(key), c.statusKey(key), c.endKey(key)).Err()
}

// Subscribe 从 afterSeq 之后开始消费。
//
// 两段式：先补历史（XRANGE）、再转增量（XREAD BLOCK）。
// 分两段而不是直接 XREAD，是因为 XREAD 只能从"某个 ID 之后"开始，
// 而我们真正想表达的条件是"seq 大于 afterSeq"——历史上可能因为 MAXLEN
// 截断而缺号，用 ID 定位会比用 seq 定位更准。
func (c *Cache) Subscribe(ctx context.Context, key string, afterSeq uint32) (<-chan logstream.Event, func()) {
	ch := make(chan logstream.Event, 256)

	// 早退分支放在建子 context 之前：否则这里的 return 会丢掉 cancel，
	// 子 context 一直挂在父 context 上直到请求结束（vet 会报 context leak）。
	if !c.Enabled() {
		close(ch)
		return ch, func() {}
	}
	subCtx, cancel := context.WithCancel(ctx)

	go func() {
		defer close(ch)

		lastID := "0-0"
		// ---------------------------------------------------------------- 历史
		entries, err := c.rdb.XRange(subCtx, c.streamKey(key), "-", "+").Result()
		if err == nil {
			for _, e := range entries {
				ev, ok := decodeEntry(e)
				if !ok {
					continue
				}
				lastID = e.ID
				if ev.Kind == logstream.KindStatus {
					continue // 历史状态无意义，下面统一补一次当前状态
				}
				if ev.Kind == logstream.KindLog && ev.Seq <= afterSeq {
					continue
				}
				if !emit(subCtx, ch, ev) {
					return
				}
				if ev.Kind == logstream.KindEnd {
					return
				}
			}
		}

		// 补一次当前状态，让客户端一进来就能渲染进度条。
		if raw, err := c.rdb.Get(subCtx, c.statusKey(key)).Bytes(); err == nil {
			var st logstream.Status
			if json.Unmarshal(raw, &st) == nil {
				seq, _ := c.rdb.Get(subCtx, c.seqKey(key)).Int64()
				if !emit(subCtx, ch, logstream.Event{Seq: uint32(seq), Kind: logstream.KindStatus, At: time.Now(), Status: &st}) {
					return
				}
			}
		}

		// ---------------------------------------------------------------- 增量
		for {
			res, err := c.rdb.XRead(subCtx, &redis.XReadArgs{
				Streams: []string{c.streamKey(key), lastID},
				Count:   200,
				Block:   streamReadBlock,
			}).Result()

			switch {
			case errors.Is(err, redis.Nil):
				// 阻塞超时，没有新条目。顺手检查流是否已结束（可能被别的实例结束）。
				if done, reason := c.Finished(subCtx, key); done {
					_ = emit(subCtx, ch, logstream.Event{Seq: afterSeq, Kind: logstream.KindEnd, At: time.Now(), Reason: reason})
					return
				}
				if subCtx.Err() != nil {
					return
				}
				continue
			case err != nil:
				// 连接问题：不静默退出，让前端通过 SSE 断流感知并重连。
				return
			}

			for _, s := range res {
				for _, e := range s.Messages {
					ev, ok := decodeEntry(e)
					if !ok {
						continue
					}
					lastID = e.ID
					if !emit(subCtx, ch, ev) {
						return
					}
					if ev.Kind == logstream.KindEnd {
						return
					}
				}
			}
		}
	}()

	return ch, cancel
}

// emit 投递事件；ctx 取消时返回 false 表示调用方应退出。
func emit(ctx context.Context, ch chan<- logstream.Event, ev logstream.Event) bool {
	select {
	case ch <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// decodeEntry 把一条 Redis Stream 条目还原成事件。
func decodeEntry(e redis.XMessage) (logstream.Event, bool) {
	kind := strField(e, "kind")
	if kind == "" {
		return logstream.Event{}, false
	}
	seq, _ := strconv.ParseUint(strField(e, "seq"), 10, 32)
	at := time.Now()
	if ms, err := strconv.ParseInt(strField(e, "at"), 10, 64); err == nil && ms > 0 {
		at = time.UnixMilli(ms)
	}

	ev := logstream.Event{
		Seq:   uint32(seq),
		Kind:  kind,
		Level: strField(e, "level"),
		Line:  strField(e, "line"),
		At:    at,
	}
	switch kind {
	case logstream.KindStatus:
		var st logstream.Status
		if raw := strField(e, "status"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &st); err != nil {
				return ev, false
			}
		}
		ev.Status = &st
	case logstream.KindEnd:
		ev.Reason = strField(e, "reason")
	}
	return ev, true
}

func strField(e redis.XMessage, name string) string {
	v, ok := e.Values[name]
	if !ok || v == nil {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	case []byte:
		return string(s)
	default:
		return ""
	}
}

// 编译期确认实现关系，避免接口漂移后才在运行时才发现。
var _ logstream.Stream = (*Cache)(nil)
