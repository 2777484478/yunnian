// Package queue 提供任务执行与互斥两件事。
//
// 为什么需要它（而不是直接 go func）：
//  1. **并发上限**：构建/部署都会拉起下游动作，不限并发会把 Runner 与集群打死；
//  2. **去重**：同一个构建/部署单不能被重复执行（重复提交、重试连点都会触发）；
//  3. **可取消**：构建跑到一半要能取消，必须有稳定的 cancel 通道；
//  4. **队列位置**：前端要显示"排在第 2 位"，需要有真实队列而不是立刻执行。
//
// 互斥（Locker）用于「同一环境同一服务不允许并发部署」——这是 docs/01 里
// `yn:lock:deploy:{env}:{svc}` 那个约定。没有 Redis 时用进程内实现，
// 单实例部署下语义完全一致；接上 Redis 后换成分布式实现即可跨实例生效。
package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrDuplicate 同一任务已在队列或执行中。
var ErrDuplicate = errors.New("任务已在执行中")

// ErrQueueFull 等待队列已满。
var ErrQueueFull = errors.New("任务队列已满")

// Task 一个可执行任务。必须响应 ctx 取消。
type Task func(ctx context.Context)

// 任务状态。
const (
	StateQueued  = "queued"
	StateRunning = "running"
)

// entry 队列中的一项。
type entry struct {
	id     uint64
	task   Task
	ctx    context.Context
	cancel context.CancelFunc
	state  string
}

// Pool 按类型的并发受限任务池。
//
// 同一时刻最多 size 个任务在执行；超出的进入 FIFO 等待队列。
// 任务以 id 去重：同 id 重复提交返回 ErrDuplicate。
type Pool struct {
	kind    string
	size    int
	maxWait int
	// autoWait 表示 maxWait 是"按 size 推出来的"，不是调用方指定的固定值。
	// 只有它才允许在 SetSize 时跟着重算 —— 显式传进来的上限不该被悄悄改掉。
	autoWait bool

	mu      sync.Mutex
	running int
	queue   []*entry
	items   map[uint64]*entry
}

// minMaxWait 自动推导等待队列时允许的最小值。
//
// 为什么要有下限：原来是死板的 `size * 20`，而默认并发是 1 —— 于是**队列只能
// 排 20 个**。批量发布一次建 20+ 个单（gcp 预生产那次就有 26 个），第 21 个
// 开始直接被拒，界面上是一串红色"部署队列已满"（2026-11-26 用户实际撞到）。
// 那些单不是执行失败，是**根本没排上**。
//
// 50 这个数的依据：一次批量发布/批量的典型规模是二三十个服务，加上同时有
// 人在做零散发布，预留一倍余量。再多也不该靠"把队列调大"解决 ——
// 那意味着用户要等几小时才能看到自己那一个。
const minMaxWait = 50

// NewPool 构造任务池。
//
// size<1 时按 1 处理；maxWait<1 时按 `max(size*20, 50)` 推导。
func NewPool(kind string, size, maxWait int) *Pool {
	if size < 1 {
		size = 1
	}
	auto := maxWait < 1
	if auto {
		maxWait = deriveMaxWait(size)
	}
	return &Pool{
		kind:     kind,
		size:     size,
		maxWait:  maxWait,
		autoWait: auto,
		items:    make(map[uint64]*entry),
	}
}

// deriveMaxWait 并发数 → 等待队列上限。
//
// 下限 50 见 minMaxWait 的注释；并发调大时按 20 倍放大 ——
// 并发 10 意味着同一时刻有 10 个在跑，排 200 个也合理。
func deriveMaxWait(size int) int {
	if size < 1 {
		size = 1
	}
	if n := size * 20; n > minMaxWait {
		return n
	}
	return minMaxWait
}

// Kind 池类型（build / deploy）。
func (p *Pool) Kind() string { return p.kind }

// Submit 提交任务。
//
// 返回排队位置：0 表示立即开始执行，>0 表示前面还有几个任务。
func (p *Pool) Submit(id uint64, task Task) (int, error) {
	if task == nil {
		return 0, fmt.Errorf("任务为空")
	}
	p.mu.Lock()
	if _, dup := p.items[id]; dup {
		p.mu.Unlock()
		return 0, ErrDuplicate
	}
	if len(p.queue) >= p.maxWait {
		p.mu.Unlock()
		return 0, ErrQueueFull
	}

	ctx, cancel := context.WithCancel(context.Background())
	e := &entry{id: id, task: task, ctx: ctx, cancel: cancel, state: StateQueued}
	p.items[id] = e
	p.queue = append(p.queue, e)
	p.dispatchLocked()

	// 排队位置必须在 dispatch **之后**算。
	//
	// 曾经这里写的是 `pos := len(p.queue)`（在 dispatch 之前），而那一刻任务
	// 自己刚被 append 进队列，于是空池提交也返回 1；上层据此推送
	// 「排队中（前面还有 0 个任务）」——明明立刻开跑却说在排队。
	// 语义以本函数注释为准：0 = 立即开始执行，>0 = 前面还有几个任务。
	pos := 0
	if e.state == StateQueued {
		pos = p.positionLocked(id)
	}
	p.mu.Unlock()
	return pos, nil
}

// positionLocked 返回任务在等待队列中的位置（1 起）；不在队列中返回 0。
// 调用方必须已持锁。
func (p *Pool) positionLocked(id uint64) int {
	for i, e := range p.queue {
		if e.id == id {
			return i + 1
		}
	}
	return 0
}

// Cancel 取消任务。正在执行的会收到 ctx 取消信号；排队中的直接被移除。
func (p *Pool) Cancel(id uint64) bool {
	_, ok := p.CancelState(id)
	return ok
}

// CancelState 取消任务，并返回它**被取消那一刻**所处的状态。
//
// 调用方需要这个状态来区分两种取消，两者的收尾责任完全不同：
//
//   - 执行中：任务会收到 ctx 取消信号，由任务自己写终态；
//   - 排队中：任务被移出队列后**再也不会被执行**，没有任何人会去写它的终态。
//
// 第二件事必须由上层补做，否则记录会永远停在"排队中"——批量打包一次提交
// 十几个任务时很容易撞上：前面几个还没跑完，用户就把后面排队的取消了。
func (p *Pool) CancelState(id uint64) (state string, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, exists := p.items[id]
	if !exists {
		return "", false
	}
	state = e.state
	e.cancel()
	if e.state == StateQueued {
		p.removeLocked(id)
		delete(p.items, id)
	}
	return state, true
}

// Size 当前并发上限。
func (p *Pool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.size
}

// SetSize 调整并发上限（运行期可变，供「运行参数」页在线修改）。
//
//	调大 → **立刻**把等待队列里的任务拉起来跑，不必等某个任务结束才补位；
//	调小 → **不打断已经在跑的**，只是不再补位，直到 running 降到新的上限以下。
//
// 这两条都是刻意的：用户改的是"以后最多同时跑几个"，不是"现在把正在跑的掐掉几个"。
// 掐掉正在跑的部署意味着一次进行到一半的滚动更新被中止 —— 那是完全没有预期的副作用。
//
// size < 1 仍按 1 处理（与 NewPool 同一口径）：0 会让池彻底不再调度，
// 表现为"提交成功但永远停在排队中"，比报错更难查。
func (p *Pool) SetSize(size int) {
	if size < 1 {
		size = 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.size == size {
		return
	}
	p.size = size
	// 自动推导的上限跟着并发走：并发从 1 调到 10 之后，队列还卡在 50
	// 就白调了 —— 用户以为改完能排更多，实际仍然一批就被拒。
	// 显式传过 maxWait 的池不动它（那是调用方的意图）。
	if p.autoWait {
		p.maxWait = deriveMaxWait(size)
	}
	p.dispatchLocked()
}

// Running 统计执行中任务数。
func (p *Pool) Running() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// Queued 统计排队中任务数。
func (p *Pool) Queued() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.queue)
}

// Position 查询任务排队位置：0=执行中或不存在，>0=前面还有几个。
func (p *Pool) Position(id uint64) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.positionLocked(id)
}

// IsActive 判断任务是否在跑或排队（用于「该服务有部署进行中」的检查）。
func (p *Pool) IsActive(id uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.items[id]
	return ok
}

// Snapshot 返回当前活跃任务 id 列表，仅用于诊断接口。
func (p *Pool) Snapshot() (running []uint64, queued []uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.items {
		if e.state == StateRunning {
			running = append(running, e.id)
		}
	}
	for _, e := range p.queue {
		queued = append(queued, e.id)
	}
	return running, queued
}

// dispatchLocked 在持有锁的情况下把等待队列填满执行槽。调用方必须已持锁。
func (p *Pool) dispatchLocked() {
	for p.running < p.size && len(p.queue) > 0 {
		e := p.queue[0]
		p.queue = p.queue[1:]
		e.state = StateRunning
		p.running++
		go p.run(e)
	}
}

// run 执行任务并在结束后回收槽位、继续调度。
func (p *Pool) run(e *entry) {
	defer func() {
		// 任务本身 panic 不能带走整池：交给 recover 兜住，池状态必须回收。
		if r := recover(); r != nil {
			_ = r
		}
		p.mu.Lock()
		p.running--
		delete(p.items, e.id)
		p.dispatchLocked()
		p.mu.Unlock()
	}()
	e.task(e.ctx)
}

// removeLocked 从等待队列移除。
func (p *Pool) removeLocked(id uint64) {
	for i, e := range p.queue {
		if e.id == id {
			p.queue = append(p.queue[:i], p.queue[i+1:]...)
			return
		}
	}
}

// --------------------------------------------------------------------------- 互斥锁

// Locker 分布式锁抽象。
//
// 语义：TryLock 拿不到锁时**立即返回 false**（不做阻塞等待）。
// 部署场景下"等锁"没有意义——等到了也可能因为前一次部署失败而需要重新决策，
// 直接告诉用户"该服务正在部署中"更清晰。
//
// 实现有且只有一处：cache.Cache（Redis 版 / 无 Redis 时退化为进程内版）。
// 这里只留接口，避免出现第二份"看起来能用但语义有差异"的实现——
// 锁这种东西，两份实现的不一致就是线上事故。
type Locker interface {
	// TryLock 尝试加锁，返回释放函数与是否成功。
	TryLock(ctx context.Context, key string, ttl time.Duration) (release func(), ok bool, err error)
	// Name 实现名。
	Name() string
}

// LockKey 生成部署互斥锁的键名，与 docs/02 §7 的 Redis Key 设计保持一致。
//
// 返回的键**不含全局前缀**：前缀由 cache 统一拼接（redis.prefix，默认 "yn:"），
// 最终落到的 Redis 键仍是 yn:lock:deploy:{env}:{svc}。
// 之所以不在这里写死前缀，是为了让"换前缀"这种运维动作只有一处生效点。
func LockKey(envKey, projectKey string) string {
	return "lock:deploy:" + envKey + ":" + projectKey
}
