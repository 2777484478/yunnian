package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/store"
)

// 运行参数（并发上限）的读写与应用。
//
// 三个使用点共用本文件：
//   - 「平台参数」编辑器保存后立刻应用（`UpdateConfig` 里的钩子）；
//   - 进程启动时应用一次（见 cmd/server/main.go）；
//   - 批量发布面板启动前读一次，决定前端一次提交几个（`GET /system/runtime`）。

// maxConcurrencyLimit 并发上限的硬上限。
//
// 不给"随便填"的自由：每个构建都在本机跑 docker build、每个部署都在对一个集群做滚动更新，
// 一起推几十个不是"快"，是把构建机与集群打满，然后集体超时失败 ——
// 那时候用户看到的是"批量发布全挂了"，而不是"并发填太大了"。
const maxConcurrencyLimit = 10

// RuntimeSettingsDTO 当前生效的运行参数 + 实况。
type RuntimeSettingsDTO struct {
	BuildConcurrency  int `json:"buildConcurrency"`
	DeployConcurrency int `json:"deployConcurrency"`
	// Source 这份值来自哪：`db`（界面上改过）/ `config`（config.yaml）。
	Source string `json:"source"`
	// MaxConcurrency 允许填的最大值，界面据此限制输入。
	MaxConcurrency int `json:"maxConcurrency"`
	// 当前实况：让用户在调之前先看到"现在堵了多少"。
	RunningBuilds  int    `json:"runningBuilds"`
	QueuedBuilds   int    `json:"queuedBuilds"`
	RunningDeploys int    `json:"runningDeploys"`
	QueuedDeploys  int    `json:"queuedDeploys"`
	UpdatedAt      string `json:"updatedAt"`
}

// RuntimeSettings 读取当前生效的运行参数。
func (s *SystemService) RuntimeSettings(ctx context.Context) (*RuntimeSettingsDTO, error) {
	cur, err := store.LoadRuntimeSettings(ctx, s.deps.DB, s.deps.Cfg.Runner)
	if err != nil {
		return nil, errs.Internalf(err, "读取运行参数失败")
	}
	out := &RuntimeSettingsDTO{
		BuildConcurrency:  cur.BuildConcurrency,
		DeployConcurrency: cur.DeployConcurrency,
		Source:            ternary(cur.FromDB, "db", "config"),
		MaxConcurrency:    maxConcurrencyLimit,
		UpdatedAt:         formatTime(cur.UpdatedAt),
	}
	if s.deps.Builds != nil {
		out.RunningBuilds, out.QueuedBuilds = s.deps.Builds.Running(), s.deps.Builds.Queued()
	}
	if s.deps.Deploys != nil {
		out.RunningDeploys, out.QueuedDeploys = s.deps.Deploys.Running(), s.deps.Deploys.Queued()
	}
	return out, nil
}

// ApplyRuntimeSettings 把库里当前的并发上限应用到任务池。
//
// 调用于两处：进程启动时一次；「平台参数」保存后一次 —— 所以**改完立刻生效，不必重启**。
// 这正是问题的关键：并发上限是"用着用着才发现要调"的旋钮，需要重启就等于它不存在。
//
// 失败不回滚配置：值已经在库里了，重启后会照样生效；
// 这里如实报错，是为了让调用方（保存接口）能告诉用户"这次没应用上"。
func (s *SystemService) ApplyRuntimeSettings(ctx context.Context) error {
	cur, err := store.LoadRuntimeSettings(ctx, s.deps.DB, s.deps.Cfg.Runner)
	if err != nil {
		return err
	}
	before := ""
	if s.deps.Builds != nil {
		before = fmt.Sprintf("build=%d", s.deps.Builds.Size())
		s.deps.Builds.SetSize(cur.BuildConcurrency)
	}
	if s.deps.Deploys != nil {
		if before != "" {
			before += " "
		}
		before += fmt.Sprintf("deploy=%d", s.deps.Deploys.Size())
		s.deps.Deploys.SetSize(cur.DeployConcurrency)
	}
	// 记一条日志：这是"保存后到底生效了没有"的唯一凭证 ——
	// 接口返回的是配置值，而这条记的是**池子实际被设成了几**。
	s.log("system").Info("并发上限已应用到任务池",
		"before", before, "build", cur.BuildConcurrency, "deploy", cur.DeployConcurrency, "source", ternary(cur.FromDB, "db", "config"))
	return nil
}

// checkRuntimeValue 校验运行参数的取值。
//
// 通用参数校验（validateConfigValue）只看"是不是数字"，而这两个键的语义是"并发数"：
// 0 会让任务池彻底不再调度（表现为"提交成功但一直排队"），
// 上千则会把构建机与集群打满。所以范围必须在这里单独拦。
func checkRuntimeValue(key, value string) error {
	if !store.IsRuntimeKey(key) {
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("应为整数（当前收到 %q）", value)
	}
	if n < 1 || n > maxConcurrencyLimit {
		return fmt.Errorf("并发数只能在 1 到 %d 之间（当前填的是 %d）——"+
			"它表示「最多几个任务同时执行」，不是「填得越大越快」", maxConcurrencyLimit, n)
	}
	return nil
}
