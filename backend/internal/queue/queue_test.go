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

// --------------------------------------------------------------------------- 运行期调整并发上限

// TestSetSizeUpStartsWaitingTasksImmediately 调大并发**立刻**补位。
//
// 语义来自「运行参数」页：用户把部署并发从 1 调成 3，期望是"现在排队的那两个马上跑"，
// 而不是"等当前这个跑完再说"——后者会让用户以为没生效，然后再点一次保存。
func TestSetSizeUpStartsWaitingTasksImmediately(t *testing.T) {
	p := NewPool("deploy", 1, 10)

	t1, r1 := blockTask(t)
	defer r1()
	if _, err := p.Submit(1, t1); err != nil {
		t.Fatalf("提交第 1 个任务失败: %v", err)
	}
	waitRunning(t, p, 1)

	t2, r2 := blockTask(t)
	defer r2()
	if pos, err := p.Submit(2, t2); err != nil || pos != 1 {
		t.Fatalf("第 2 个任务应在排队（前面 1 个），实际 pos=%d err=%v", pos, err)
	}
	t3, r3 := blockTask(t)
	defer r3()
	if _, err := p.Submit(3, t3); err != nil {
		t.Fatalf("提交第 3 个任务失败: %v", err)
	}
	if p.Running() != 1 || p.Queued() != 2 {
		t.Fatalf("调大之前应为 1 跑 2 等，实际 %d 跑 %d 等", p.Running(), p.Queued())
	}

	p.SetSize(3)

	// 两个排队的都该被立刻拉起来
	waitRunning(t, p, 3)
	if p.Queued() != 0 {
		t.Errorf("调大后不该还有人排队，实际 %d", p.Queued())
	}
	if p.Size() != 3 {
		t.Errorf("Size 应为 3，实际 %d", p.Size())
	}
}

// TestSetSizeDownDoesNotInterruptRunning 调小并发**不打断已经在跑的**。
//
// 掐掉正在跑的部署 = 一次进行到一半的滚动更新被中止，那是完全没预期的副作用。
// 正确行为是"不再补位"：等在跑的结束，直到 running 降到新上限以下。
func TestSetSizeDownDoesNotInterruptRunning(t *testing.T) {
	p := NewPool("deploy", 3, 10)

	var releases []func()
	for i := uint64(1); i <= 3; i++ {
		task, rel := blockTask(t)
		releases = append(releases, rel)
		if _, err := p.Submit(i, task); err != nil {
			t.Fatalf("提交任务 %d 失败: %v", i, err)
		}
	}
	defer func() {
		for _, r := range releases {
			r()
		}
	}()
	waitRunning(t, p, 3)

	p.SetSize(1)

	if p.Running() != 3 {
		t.Fatalf("调小不该打断在跑的，实际 running=%d", p.Running())
	}
	// 新任务不能再被拉起来（槽位已"超员"）
	t4, r4 := blockTask(t)
	defer r4()
	if pos, err := p.Submit(4, t4); err != nil || pos != 1 {
		t.Fatalf("调小后新任务应排队，实际 pos=%d err=%v", pos, err)
	}
	time.Sleep(50 * time.Millisecond)
	if p.Running() != 3 {
		t.Errorf("调小后不该再补位，实际 running=%d", p.Running())
	}
}

// TestSetSizeIgnoresNonPositive 0 / 负数按 1 处理。
//
// 0 会让 dispatchLocked 永远不成立 —— 任务池从此不再调度，
// 表现为"提交成功但一直停在排队中"，比直接报错更难查。
func TestSetSizeIgnoresNonPositive(t *testing.T) {
	p := NewPool("deploy", 2, 10)
	p.SetSize(0)
	if p.Size() != 1 {
		t.Fatalf("0 应按 1 处理，实际 %d", p.Size())
	}
	p.SetSize(-5)
	if p.Size() != 1 {
		t.Fatalf("负数应按 1 处理，实际 %d", p.Size())
	}
}
