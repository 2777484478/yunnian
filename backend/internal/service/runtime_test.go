package service

import (
	"context"
	"strings"
	"testing"

	"github.com/2777484478/yunnian/internal/config"
	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/logstream"
	"github.com/2777484478/yunnian/internal/queue"
	"github.com/2777484478/yunnian/internal/store"
)

// 本文件盯的是「并发上限在界面上改完之后，任务池**真的**跟着变了」。
//
// 只断言"配置存进去了"是不够的：这一改动的全部价值就在于**不必重启**，
// 而"存了但没应用"会表现成"改了没反应"，用户大概率会再点一次保存然后怀疑平台坏了。

func TestApplyRuntimeSettingsResizesPools(t *testing.T) {
	db := newTestDB(t)
	builds := queue.NewPool("build", 1, 0)
	deploys := queue.NewPool("deploy", 1, 0)

	cfg := &config.Config{}
	cfg.Runner.BuildConcurrency = 1
	cfg.Runner.DeployConcurrency = 1

	svc := New(Deps{
		Cfg:     cfg,
		DB:      db,
		Log:     discardLogger(),
		Stream:  logstream.NewMemStream(),
		Up:      &integration.Set{},
		Builds:  builds,
		Deploys: deploys,
	})

	// 库里没有这两个键时：沿用 config.yaml 的值，不回退成 0
	cur, err := store.LoadRuntimeSettings(context.Background(), db, cfg.Runner)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if cur.FromDB || cur.BuildConcurrency != 1 || cur.DeployConcurrency != 1 {
		t.Fatalf("库里没键时应回退配置文件，实际 %+v", cur)
	}

	// 界面上改成 3 / 4 后，池子必须跟着变
	if err := store.SaveRuntimeSettings(context.Background(), db, 3, 4, nil); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if err := svc.System.ApplyRuntimeSettings(context.Background()); err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	if builds.Size() != 3 || deploys.Size() != 4 {
		t.Fatalf("池子没跟着改：build=%d deploy=%d", builds.Size(), deploys.Size())
	}

	// 接口读到的生效值也要一致（批量发布页就是按它决定一次提交几个）
	rt, err := svc.System.RuntimeSettings(context.Background())
	if err != nil {
		t.Fatalf("读运行参数失败: %v", err)
	}
	if rt.BuildConcurrency != 3 || rt.DeployConcurrency != 4 || rt.Source != "db" {
		t.Fatalf("生效值与库不一致：%+v", rt)
	}
	if rt.MaxConcurrency != maxConcurrencyLimit {
		t.Errorf("应回传上限 %d，实际 %d", maxConcurrencyLimit, rt.MaxConcurrency)
	}
}

// TestRuntimeEnsureSeedsKeysForEditor 「补键」是这次能被用户看见的前提。
//
// 通用参数编辑器**只显示库里已有的键** —— 不补的话，用户在界面上根本找不到这个旋钮，
// 而那正是要解决的问题（"有没有地方设置队列数量"）。
func TestRuntimeEnsureSeedsKeysForEditor(t *testing.T) {
	db := newTestDB(t)
	cfg := config.RunnerConfig{BuildConcurrency: 3, DeployConcurrency: 1}

	if err := store.EnsureRuntimeSettings(context.Background(), db, cfg); err != nil {
		t.Fatalf("补键失败: %v", err)
	}
	// 幂等：再跑一次不该报错，也不该覆盖用户改过的值
	if err := store.SaveRuntimeSettings(context.Background(), db, 5, 6, nil); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	if err := store.EnsureRuntimeSettings(context.Background(), db, cfg); err != nil {
		t.Fatalf("二次补键失败: %v", err)
	}
	cur, err := store.LoadRuntimeSettings(context.Background(), db, cfg)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if cur.BuildConcurrency != 5 || cur.DeployConcurrency != 6 {
		t.Fatalf("补键不该覆盖用户改过的值，实际 %+v", cur)
	}
	if !store.IsRuntimeKey(store.KeyRunnerDeployConcurrency) || store.IsRuntimeKey("integration.harbor.enabled") {
		t.Error("IsRuntimeKey 判定不对：它决定保存后要不要立刻应用")
	}
}

// TestCheckRuntimeValue 并发数不是随意填的。
//
// 通用校验只看"是不是数字"：0 会让池彻底不再调度（表现为"提交成功但一直排队"），
// 上千会把构建机与集群打满 —— 两个都不该放过去。
func TestCheckRuntimeValue(t *testing.T) {
	key := store.KeyRunnerDeployConcurrency
	cases := []struct {
		value   string
		wantErr string
	}{
		{"1", ""},
		{"3", ""},
		{"10", ""},
		{"0", "只能在 1 到 10 之间"},
		{"11", "只能在 1 到 10 之间"},
		{"-2", "只能在 1 到 10 之间"},
		{"abc", "应为整数"},
		{"", "应为整数"},
	}
	for _, c := range cases {
		err := checkRuntimeValue(key, c.value)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("填 %q 应通过，实际：%v", c.value, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("填 %q 应被拒，实际通过了", c.value)
			continue
		}
		if !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("填 %q 的错误文案应含 %q，实际：%v", c.value, c.wantErr, err)
		}
	}
	// 非运行参数的键不受这套范围限制（它们走各自的校验）
	if err := checkRuntimeValue("integration.harbor.timeout", "999"); err != nil {
		t.Errorf("非运行参数的键不该被这套规则拦：%v", err)
	}
}
