package queue

import (
	"context"
	"testing"
	"time"
)

// blockTask 返回一个一直阻塞到测试结束的任务，以及释放函数。
//
// 池是在 goroutine 里跑任务的，若不阻塞，任务可能在断言之前就结束、
// 槽位被回收，于是"排队"这件事根本不会发生，测试会变成假绿。
func blockTask(t *testing.T) (Task, func()) {
	t.Helper()
	done := make(chan struct{})
	task := func(ctx context.Context) { <-done }
	return task, func() { close(done) }
}

// waitRunning 等到池里真的有人开始跑（或超时），避免靠 sleep 猜时间。
func waitRunning(t *testing.T, p *Pool, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if p.Running() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待 running=%d 超时，当前 running=%d", want, p.Running())
}

// TestSubmitReportsZeroWhenItStartsImmediately 钉住排队位置的语义：
// 有空闲执行槽时任务会立刻开跑，此时**不是排队**，必须返回 0。
//
// 曾经的实现是在 dispatch 之前用 len(p.queue) 算位置，而那一刻任务自己
// 还在队列里，于是返回 1；上层据此推送「排队中（前面还有 0 个任务）」——
// 没有任何任务在等，却告诉用户"你在排队"。
func TestSubmitReportsZeroWhenItStartsImmediately(t *testing.T) {
	p := NewPool("build", 3, 10)
	task, release := blockTask(t)
	defer release()

	pos, err := p.Submit(1, task)
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	waitRunning(t, p, 1)

	if pos != 0 {
		t.Errorf("空池提交应立即开跑，pos 应为 0，实际 %d", pos)
	}
}

// TestSubmitReportsQueuePosition 第 2 个任务在只有 1 个槽时必须报告排队，
// 且位置是"前面还有几个"，不是"我在队列里排第几"。
func TestSubmitReportsQueuePosition(t *testing.T) {
	p := NewPool("build", 1, 10)
	t1, release := blockTask(t)
	defer release()

	if pos, err := p.Submit(1, t1); err != nil || pos != 0 {
		t.Fatalf("第 1 个任务应立刻开跑，pos=%d err=%v", pos, err)
	}
	waitRunning(t, p, 1)

	t2, release2 := blockTask(t)
	defer release2()
	pos, err := p.Submit(2, t2)
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if pos != 1 {
		t.Errorf("第 2 个任务前面应还有 1 个，pos=%d", pos)
	}

	t3, release3 := blockTask(t)
	defer release3()
	pos, err = p.Submit(3, t3)
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if pos != 2 {
		t.Errorf("第 3 个任务前面应还有 2 个，pos=%d", pos)
	}
}

// TestSubmitRejectsDuplicate 同一 id 不能入池两次：重复提交会并行跑两遍，
// 而调用方（构建/部署）都假设"同一个单只跑一次"。
func TestSubmitRejectsDuplicate(t *testing.T) {
	p := NewPool("build", 3, 10)
	task, release := blockTask(t)
	defer release()

	if _, err := p.Submit(7, task); err != nil {
		t.Fatalf("首次提交失败: %v", err)
	}
	if _, err := p.Submit(7, task); err != ErrDuplicate {
		t.Errorf("重复提交应返回 ErrDuplicate，实际 %v", err)
	}
}

// TestSubmitRejectsWhenWaitQueueFull 等待队列满必须明确拒绝，
// 而不是无限堆积——池满时上层要能回 429 让用户稍后重试。
func TestSubmitRejectsWhenWaitQueueFull(t *testing.T) {
	p := NewPool("build", 1, 2)
	t1, release := blockTask(t)
	defer release()
	if _, err := p.Submit(1, t1); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	waitRunning(t, p, 1)

	fillers := make([]func(), 0, 2)
	for id := uint64(2); id <= 3; id++ {
		tk, rl := blockTask(t)
		fillers = append(fillers, rl)
		if _, err := p.Submit(id, tk); err != nil {
			t.Fatalf("第 %d 个应能进入等待队列: %v", id, err)
		}
	}
	defer func() {
		for _, rl := range fillers {
			rl()
		}
	}()

	tk, rl := blockTask(t)
	defer rl()
	if _, err := p.Submit(4, tk); err != ErrQueueFull {
		t.Errorf("等待队列已满应返回 ErrQueueFull，实际 %v", err)
	}
}

// TestPositionReturnsZeroForRunningTask Position 只描述"在等待队列里的位置"，
// 执行中 / 不存在的任务必须返回 0，否则界面会把"正在跑"显示成"在排队"。
func TestPositionReturnsZeroForRunningTask(t *testing.T) {
	p := NewPool("build", 1, 10)
	t1, release := blockTask(t)
	defer release()
	if _, err := p.Submit(1, t1); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	waitRunning(t, p, 1)

	if got := p.Position(1); got != 0 {
		t.Errorf("执行中的任务 Position 应为 0，实际 %d", got)
	}
	if got := p.Position(999); got != 0 {
		t.Errorf("不存在的任务 Position 应为 0，实际 %d", got)
	}

	t2, release2 := blockTask(t)
	defer release2()
	if _, err := p.Submit(2, t2); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if got := p.Position(2); got != 1 {
		t.Errorf("排队中的任务 Position 应为 1，实际 %d", got)
	}
}
