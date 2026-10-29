package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/2777484478/yunnian/internal/auth"
	"github.com/2777484478/yunnian/internal/model"
)

// TestNextRunAfter 钉住「下次什么时候跑」。
//
// 这个算错了，定时任务会静默地在错误的时间发版 —— 而且是"看起来正常"的那种错：
// 任务列表上有下次时间、也真的会跑，就是时间不对。
func TestNextRunAfter(t *testing.T) {
	// 固定一个基准：2026-10-29 15:30:00（周四）
	base := time.Date(2026, 10, 29, 15, 30, 0, 0, time.Local)

	cases := []struct {
		name string
		expr string
		from time.Time
		want time.Time
	}{
		{"每天 2 点 → 明天 2 点", "0 2 * * *", base,
			time.Date(2026, 10, 30, 2, 0, 0, 0, time.Local)},
		{"每天 2 点（凌晨 1 点问）→ 当天 2 点", "0 2 * * *",
			time.Date(2026, 10, 29, 1, 0, 0, 0, time.Local),
			time.Date(2026, 10, 29, 2, 0, 0, 0, time.Local)},
		{"每小时整点 → 16:00", "0 * * * *", base,
			time.Date(2026, 10, 29, 16, 0, 0, 0, time.Local)},
		{"每 15 分钟 → 15:45", "*/15 * * * *", base,
			time.Date(2026, 10, 29, 15, 45, 0, 0, time.Local)},
		{"工作日 9 点（周四下午问）→ 周五 9 点", "0 9 * * 1-5", base,
			time.Date(2026, 10, 30, 9, 0, 0, 0, time.Local)},
		{"工作日 9 点（周五晚问）→ 下周一 9 点", "0 9 * * 1-5",
			time.Date(2026, 10, 30, 20, 0, 0, 0, time.Local),
			time.Date(2026, 11, 2, 9, 0, 0, 0, time.Local)},
		{"每月 1 号 3 点（10-29 问）→ 11-01 3 点", "0 3 1 * *", base,
			time.Date(2026, 11, 1, 3, 0, 0, 0, time.Local)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := nextRunAfter(c.expr, c.from)
			if got == nil {
				t.Fatalf("表达式 %q 算不出下次时间", c.expr)
			}
			if !got.Equal(c.want) {
				t.Errorf("得到 %s，期望 %s", got.Format("2006-01-02 15:04"), c.want.Format("2006-01-02 15:04"))
			}
		})
	}

	t.Run("非法表达式返回 nil 而不是 panic", func(t *testing.T) {
		if got := nextRunAfter("0 25 * * *", base); got != nil {
			t.Errorf("25 点这种非法表达式应返回 nil，得到 %v", got)
		}
		if got := nextRunAfter("", base); got != nil {
			t.Errorf("空表达式应返回 nil，得到 %v", got)
		}
	})
}

// TestDecideDue 钉住「到点该跑还是该记错过」—— 这是用户最在意的那条规则。
//
// 「服务重启后不补跑」这件事，如果实现错了，表现是：
// 早上上班一开服务，半夜该发的部署立刻发到生产。这种错误不会报任何错，
// 只能靠这条测试挡住。
func TestDecideDue(t *testing.T) {
	base := time.Date(2026, 10, 29, 2, 0, 0, 0, time.Local) // 本该执行的时间

	cases := []struct {
		name       string
		expr       string
		nextRunAt  time.Time
		at         time.Time
		wantRun    bool
		wantMissed int
	}{
		{
			name:      "刚好到点（lag=0）→ 执行",
			expr:      "0 2 * * *",
			nextRunAt: base,
			at:        base,
			wantRun:   true,
		},
		{
			name:      "迟到 30 秒（tick 的正常误差）→ 仍执行",
			expr:      "0 2 * * *",
			nextRunAt: base,
			at:        base.Add(30 * time.Second),
			wantRun:   true,
		},
		{
			name:      "迟到 1 分 59 秒（正好在宽限内）→ 仍执行",
			expr:      "0 2 * * *",
			nextRunAt: base,
			at:        base.Add(119 * time.Second),
			wantRun:   true,
		},
		{
			// 服务停机 8 小时：绝不能补发到生产
			name:       "迟到 8 小时 → 不执行，记 1 次错过",
			expr:       "0 2 * * *",
			nextRunAt:  base,
			at:         base.Add(8 * time.Hour),
			wantRun:    false,
			wantMissed: 1,
		},
		{
			// 每小时一次、停机 5 小时 → 错过 5 次（02:00/03:00/04:00/05:00/06:00）
			name:       "每小时任务停机 5 小时 → 记 5 次错过",
			expr:       "0 * * * *",
			nextRunAt:  base,
			at:         base.Add(5 * time.Hour),
			wantRun:    false,
			wantMissed: 5,
		},
		{
			// 每天 2 点、停机 3 天 → 错过 3 次
			name:       "每天任务停机 3 天 → 记 3 次错过",
			expr:       "0 2 * * *",
			nextRunAt:  base,
			at:         base.Add(72 * time.Hour),
			wantRun:    false,
			wantMissed: 3,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decideDue(c.expr, c.nextRunAt, c.at)
			if got.Run != c.wantRun {
				t.Fatalf("Run=%v，期望 %v（lag=%s）", got.Run, c.wantRun, got.Lag)
			}
			if !c.wantRun && got.Missed != c.wantMissed {
				t.Errorf("Missed=%d，期望 %d", got.Missed, c.wantMissed)
			}
			if c.wantRun && got.Missed != 0 {
				t.Errorf("执行时不该有错过计数，得到 %d", got.Missed)
			}
		})
	}
}

// TestPreviewSchedule cron 预览：界面靠它让用户当场发现表达式写错。
func TestPreviewSchedule(t *testing.T) {
	s := &ScheduleService{base: &base{}}

	ok := s.PreviewSchedule("0 2 * * *")
	if !ok.Valid {
		t.Fatalf("合法表达式被判非法：%s", ok.Error)
	}
	if len(ok.Next) != 5 {
		t.Errorf("应预览 5 次，得到 %d", len(ok.Next))
	}
	for i := 1; i < len(ok.Next); i++ {
		if !ok.Next[i].After(ok.Next[i-1]) {
			t.Errorf("预览时间必须递增：%v 之后是 %v", ok.Next[i-1], ok.Next[i])
		}
	}

	bad := s.PreviewSchedule("0 25 * * *")
	if bad.Valid {
		t.Error("25 点这种非法表达式应判非法")
	}
	if bad.Error == "" {
		t.Error("非法表达式必须给出原因")
	}
	if !strings.Contains(bad.Error, "分 时 日 月 周") {
		t.Errorf("错误信息里应带格式说明，实际：%q", bad.Error)
	}
}

// TestScheduleNormalize 入参校验：这些拒绝理由决定了用户能不能自己改对。
func TestScheduleNormalize(t *testing.T) {
	db := newTestDB(t)
	s := &ScheduleService{base: &base{deps: Deps{DB: db}}}
	ctx := context.Background()

	if err := db.Create(&model.Project{ProjectKey: "demo", Name: "演示", Type: "backend"}).Error; err != nil {
		t.Fatalf("造项目失败：%v", err)
	}
	if err := db.Create(&model.Environment{EnvKey: "prod", Name: "生产"}).Error; err != nil {
		t.Fatalf("造环境失败：%v", err)
	}

	// 用「有全部权限」的 Caller：本测试只关心校验分支，
	// 权限分支在上面几个用例之外（它有专门的断言）
	caller := Caller{User: &auth.CurrentUser{ID: 1, Name: "管理员", Permissions: []string{"*"}}}

	// 分支必须写上：定时构建不靠项目默认分支（用户 2026-10-29 的明确要求）
	base := ScheduleInput{Name: "夜间构建", TaskType: "build", ProjectKey: "demo", Branch: "master", Cron: "0 2 * * *"}

	cases := []struct {
		name       string
		mutate     func(*ScheduleInput)
		wantErrHas string
	}{
		{"合法构建任务", func(*ScheduleInput) {}, ""},
		{"cron 非法", func(in *ScheduleInput) { in.Cron = "0 25 * * *" }, "cron 表达式不合法"},
		{"任务名为空", func(in *ScheduleInput) { in.Name = "  " }, "任务名不能为空"},
		{"项目不存在", func(in *ScheduleInput) { in.ProjectKey = "nope" }, "不存在"},
		{"部署缺环境", func(in *ScheduleInput) { in.TaskType = "deploy" }, "必须选择环境"},
		{"部署缺版本", func(in *ScheduleInput) {
			in.TaskType = "deploy"
			in.EnvKey = "prod"
		}, "还没选要发的版本"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := base
			c.mutate(&in)
			_, err := s.normalize(ctx, in, caller)
			if c.wantErrHas == "" {
				if err != nil {
					t.Fatalf("不该报错：%v", userMessage(err))
				}
				return
			}
			if err == nil {
				t.Fatalf("应该报错（含 %q），却通过了", c.wantErrHas)
			}
			if msg := userMessage(err); !strings.Contains(msg, c.wantErrHas) {
				t.Errorf("错误信息里没有 %q，实际：%q", c.wantErrHas, msg)
			}
		})
	}

	t.Run("构建任务会清掉部署字段", func(t *testing.T) {
		in := base
		in.EnvKey = "prod"
		in.ImageSource = "latest"
		out, err := s.normalize(ctx, in, caller)
		if err != nil {
			t.Fatalf("不该报错：%v", err)
		}
		if out.EnvKey != "" || out.ImageSource != "" {
			t.Errorf("build 任务不该保留 env/image 配置：%+v", out)
		}
	})
}

// TestStillRunning 上次还在跑就跳过 —— 用户拍板的并发策略。
//
// 关键在于它**读实际记录的 status**，而不是任务的 last_status：
// 后者在进程重启后可能是残留的 "running"，会让任务从此被永久跳过且不报错。
func TestStillRunning(t *testing.T) {
	db := newTestDB(t)
	s := &ScheduleService{base: &base{deps: Deps{DB: db}}}
	ctx := context.Background()

	b := &model.BuildRecord{ProjectKey: "demo", Status: "running"}
	if err := db.Create(b).Error; err != nil {
		t.Fatalf("造构建记录失败：%v", err)
	}
	task := &model.ScheduleTask{TaskType: "build", LastBuildID: b.ID}

	busy, reason := s.stillRunning(ctx, task)
	if !busy {
		t.Fatal("构建还在 running，应该判定为忙")
	}
	if !strings.Contains(reason, "没结束") || !strings.Contains(reason, "跳过") {
		t.Errorf("跳过原因应说清楚（还在跑 + 本次跳过），实际：%q", reason)
	}

	// 构建结束后应放行
	db.Model(&model.BuildRecord{}).Where("id = ?", b.ID).Update("status", "success")
	if busy, _ := s.stillRunning(ctx, task); busy {
		t.Error("构建已结束，不该再判定为忙")
	}

	// 部署同理：progressing 算忙、success 放行
	d := &model.DeployRecord{DeployNo: "DP1", EnvKey: "prod", ProjectKey: "demo", Status: "progressing"}
	if err := db.Create(d).Error; err != nil {
		t.Fatalf("造部署记录失败：%v", err)
	}
	dtask := &model.ScheduleTask{TaskType: "deploy", LastDeployID: d.ID}
	if busy, _ := s.stillRunning(ctx, dtask); !busy {
		t.Error("部署还在 progressing，应该判定为忙")
	}
	db.Model(&model.DeployRecord{}).Where("id = ?", d.ID).Update("status", "success")
	if busy, _ := s.stillRunning(ctx, dtask); busy {
		t.Error("部署已结束，不该再判定为忙")
	}

	// 从没跑过的任务不忙
	if busy, _ := s.stillRunning(ctx, &model.ScheduleTask{TaskType: "build"}); busy {
		t.Error("从没执行过的任务不该判定为忙")
	}
}

// TestBuildDeployInput 版本来源的三种情形。
func TestBuildDeployInput(t *testing.T) {
	db := newTestDB(t)
	s := &ScheduleService{base: &base{deps: Deps{DB: db}}}
	ctx := context.Background()

	rec := &model.ImageRecord{
		ProjectKey: "demo", Registry: "harbor.test", Repo: "escloud/demo",
		Tag: "1.2.0.100", Image: "harbor.test/escloud/demo:1.2.0.100",
	}
	if err := db.Create(rec).Error; err != nil {
		t.Fatalf("造镜像记录失败：%v", err)
	}

	t.Run("fixed + 平台构建记录 id", func(t *testing.T) {
		in, err := s.buildDeployInput(ctx, &model.ScheduleTask{
			TaskType: "deploy", EnvKey: "prod", ImageSource: "fixed",
		}, model.ScheduleTarget{ProjectKey: "demo", ImageRecordID: "42", ImageTag: "1.2.0.100"}, "")
		if err != nil {
			t.Fatalf("不该报错：%v", err)
		}
		if in.ImageSource != "platform" || in.ImageRecordID != "42" {
			t.Errorf("应走 platform 来源，实际 %+v", in)
		}
		if !strings.Contains(in.Remark, "定时任务") {
			t.Errorf("备注里应标出是定时任务，实际 %q", in.Remark)
		}
	})

	t.Run("fixed + 仓库标签", func(t *testing.T) {
		in, err := s.buildDeployInput(ctx, &model.ScheduleTask{
			TaskType: "deploy", EnvKey: "prod", ImageSource: "fixed",
		}, model.ScheduleTarget{ProjectKey: "demo", ImageTag: "1.2.0.999"}, "")
		if err != nil {
			t.Fatalf("不该报错：%v", err)
		}
		if in.ImageSource != "registry" || in.RegistryTag != "1.2.0.999" {
			t.Errorf("应走 registry 来源，实际 %+v", in)
		}
	})

	t.Run("latest 取平台最新构建", func(t *testing.T) {
		in, err := s.buildDeployInput(ctx, &model.ScheduleTask{
			TaskType: "deploy", EnvKey: "prod", ImageSource: "latest",
		}, model.ScheduleTarget{ProjectKey: "demo"}, "")
		if err != nil {
			t.Fatalf("不该报错：%v", err)
		}
		if in.ImageSource != "platform" {
			t.Errorf("latest 应走 platform，实际 %+v", in)
		}
		if in.ImageRecordID == "" {
			t.Error("latest 必须解析出具体的 imageRecordId，否则部署会退化成让后端再猜一次「最新」")
		}
		if !strings.Contains(in.Remark, "1.2.0.100") {
			t.Errorf("备注里应写明取到了哪个版本，实际 %q", in.Remark)
		}
	})

	t.Run("latest 但项目没有构建记录 → 明确报错", func(t *testing.T) {
		_, err := s.buildDeployInput(ctx, &model.ScheduleTask{
			TaskType: "deploy", EnvKey: "prod", ImageSource: "latest",
		}, model.ScheduleTarget{ProjectKey: "no-build-record"}, "")
		if err == nil {
			t.Fatal("没有构建记录时应报错")
		}
		if !strings.Contains(userMessage(err), "没有任何构建记录") {
			t.Errorf("错误信息应说清原因，实际：%q", userMessage(err))
		}
	})

	t.Run("fixed 但没给版本 → 报错而不是静默发最新", func(t *testing.T) {
		_, err := s.buildDeployInput(ctx, &model.ScheduleTask{
			TaskType: "deploy", EnvKey: "prod", ImageSource: "fixed",
		}, model.ScheduleTarget{ProjectKey: "demo"}, "")
		if err == nil {
			t.Fatal("没指定版本时应报错")
		}
	})
}

// ---------------------------------------------------------------------------
// 多目标（批量）
//
// 用户原话（2026-10-29）："定时任务为什么只能选一个项目？部署发布的时候
// 怎么可能只有 1 个服务？我肯定是批量的。"
//
// 这一组测试钉住三件事：
//   1. 目标清单**优先读 targets 列**，旧数据（只有镜像列）能还原成一条；
//   2. 重复项目、空项目、超上限在建任务时就被拦住，不留到执行时才发现；
//   3. 逐条校验（每个项目各自的分支 / 版本），不是只校验第一个。
// ---------------------------------------------------------------------------

// TestScheduleTargetsOf 目标来源的优先级。
func TestScheduleTargetsOf(t *testing.T) {
	t.Run("targets 列有两条就返回两条", func(t *testing.T) {
		task := &model.ScheduleTask{
			ProjectKey: "a",
			Targets:    encodeScheduleTargets([]model.ScheduleTarget{{ProjectKey: "a"}, {ProjectKey: "b"}}),
		}
		got := scheduleTargetsOf(task)
		if len(got) != 2 {
			t.Fatalf("应为 2 个目标，实际 %d 个：%+v", len(got), got)
		}
		if got[1].ProjectKey != "b" {
			t.Errorf("第二个目标应为 b，实际 %q", got[1].ProjectKey)
		}
	})

	t.Run("旧数据没有 targets 列 → 由镜像列还原一条", func(t *testing.T) {
		task := &model.ScheduleTask{ProjectKey: "legacy", Branch: "master"}
		got := scheduleTargetsOf(task)
		if len(got) != 1 || got[0].ProjectKey != "legacy" || got[0].Branch != "master" {
			t.Fatalf("旧任务应还原成 1 个目标，实际 %+v", got)
		}
	})

	t.Run("targets 是坏 JSON → 退回镜像列，不 panic", func(t *testing.T) {
		task := &model.ScheduleTask{ProjectKey: "keep", Targets: model.JSONText("{not json")}
		got := scheduleTargetsOf(task)
		if len(got) != 1 || got[0].ProjectKey != "keep" {
			t.Fatalf("坏 JSON 应退回镜像列，实际 %+v", got)
		}
	})

	t.Run("镜像列也空 → 返回空（由执行侧报错，不静默成功）", func(t *testing.T) {
		if got := scheduleTargetsOf(&model.ScheduleTask{}); len(got) != 0 {
			t.Fatalf("没有任何目标时应返回空，实际 %+v", got)
		}
	})
}

// TestScheduleRefsOf 上次记录的来源：last_ids 优先，退回单条 id。
func TestScheduleRefsOf(t *testing.T) {
	task := &model.ScheduleTask{
		LastBuildID: 7,
		LastIDs:     encodeScheduleRefs([]scheduleRef{{Kind: "build", ID: 7}, {Kind: "build", ID: 8}}),
	}
	if got := scheduleRefsOf(task); len(got) != 2 || got[1].ID != 8 {
		t.Fatalf("应取 last_ids 里的两条，实际 %+v", got)
	}
	// 旧数据：只有 last_build_id
	if got := scheduleRefsOf(&model.ScheduleTask{LastBuildID: 7}); len(got) != 1 || got[0].ID != 7 {
		t.Fatalf("应退回单条 id，实际 %+v", got)
	}
}

// TestNormalizeTargets 目标的归一化与拦截。
func TestNormalizeTargets(t *testing.T) {
	db := newTestDB(t)
	s := &ScheduleService{base: &base{deps: Deps{DB: db}}}
	ctx := context.Background()
	for _, k := range []string{"proj-a", "proj-b"} {
		if err := db.Create(&model.Project{ProjectKey: k, Name: k, BuildMethod: "dockerfile"}).Error; err != nil {
			t.Fatalf("造项目失败：%v", err)
		}
	}

	t.Run("多条目标原样保留（顺序不变）", func(t *testing.T) {
		got, _, err := s.normalizeTargets(ctx, ScheduleInput{Targets: []model.ScheduleTarget{
			{ProjectKey: "proj-b", Branch: "release"}, {ProjectKey: "proj-a", Branch: "master"},
		}})
		if err != nil {
			t.Fatalf("不该报错：%v", err)
		}
		if len(got) != 2 || got[0].ProjectKey != "proj-b" {
			t.Fatalf("顺序应原样保留（用户排的行序），实际 %+v", got)
		}
	})

	t.Run("旧写法（只有 ProjectKey）折叠成一条", func(t *testing.T) {
		got, _, err := s.normalizeTargets(ctx, ScheduleInput{ProjectKey: "proj-a", Branch: "dev"})
		if err != nil {
			t.Fatalf("不该报错：%v", err)
		}
		if len(got) != 1 || got[0].Branch != "dev" {
			t.Fatalf("应折叠成 1 条并带上分支，实际 %+v", got)
		}
	})

	t.Run("重复项目被拒", func(t *testing.T) {
		_, _, err := s.normalizeTargets(ctx, ScheduleInput{Targets: []model.ScheduleTarget{
			{ProjectKey: "proj-a"}, {ProjectKey: "proj-a"},
		}})
		if err == nil || !strings.Contains(userMessage(err), "重复") {
			t.Fatalf("重复项目应被拒，实际：%v", err)
		}
	})

	t.Run("项目不存在被拒", func(t *testing.T) {
		_, _, err := s.normalizeTargets(ctx, ScheduleInput{Targets: []model.ScheduleTarget{{ProjectKey: "nope"}}})
		if err == nil || !strings.Contains(userMessage(err), "不存在") {
			t.Fatalf("不存在的项目应被拒，实际：%v", err)
		}
	})

	t.Run("一条目标都没有被拒", func(t *testing.T) {
		if _, _, err := s.normalizeTargets(ctx, ScheduleInput{}); err == nil {
			t.Fatal("没有目标时应报错")
		}
	})
}

// TestNormalizeBuildBranch 分支必须由人指定（用户 2026-10-29 的明确要求）。
func TestNormalizeBuildBranch(t *testing.T) {
	db := newTestDB(t)
	s := &ScheduleService{base: &base{deps: Deps{DB: db}}}
	ctx := context.Background()

	if err := db.Create(&model.Project{ProjectKey: "plain", Name: "plain", BuildMethod: "dockerfile"}).Error; err != nil {
		t.Fatalf("造项目失败：%v", err)
	}
	if err := db.Create(&model.Project{ProjectKey: "jnk", Name: "jnk", BuildMethod: "jenkins", JenkinsJob: "folder/job"}).Error; err != nil {
		t.Fatalf("造 Jenkins 项目失败：%v", err)
	}

	baseIn := ScheduleInput{Name: "夜间打包", TaskType: "build", Cron: "0 2 * * *"}

	t.Run("平台构建不给分支 → 拒（不能靠默认值）", func(t *testing.T) {
		in := baseIn
		in.Targets = []model.ScheduleTarget{{ProjectKey: "plain"}}
		_, err := s.normalize(ctx, in, Caller{})
		if err == nil || !strings.Contains(userMessage(err), "没有指定分支") {
			t.Fatalf("应要求写明分支，实际：%v", err)
		}
	})

	t.Run("平台构建给了分支 → 通过", func(t *testing.T) {
		in := baseIn
		in.Targets = []model.ScheduleTarget{{ProjectKey: "plain", Branch: "release/1.2"}}
		out, err := s.normalize(ctx, in, Caller{})
		if err != nil {
			t.Fatalf("不该报错：%v", err)
		}
		if out.Targets[0].Branch != "release/1.2" {
			t.Errorf("分支应保留，实际 %+v", out.Targets[0])
		}
	})

	t.Run("批量里**任何一条**缺分支都拒（不能只校验第一条）", func(t *testing.T) {
		in := baseIn
		in.Targets = []model.ScheduleTarget{
			{ProjectKey: "plain", Branch: "master"},
			{ProjectKey: "plain2", Branch: ""},
		}
		if err := db.Create(&model.Project{ProjectKey: "plain2", Name: "p2", BuildMethod: "dockerfile"}).Error; err != nil {
			t.Fatalf("造项目失败：%v", err)
		}
		_, err := s.normalize(ctx, in, Caller{})
		if err == nil || !strings.Contains(userMessage(err), "plain2") {
			t.Fatalf("应指出是哪条缺分支，实际：%v", err)
		}
	})

	t.Run("Jenkins 给了分支但没给参数名 → 拒", func(t *testing.T) {
		in := baseIn
		in.Targets = []model.ScheduleTarget{{ProjectKey: "jnk", Branch: "master"}}
		_, err := s.normalize(ctx, in, Caller{})
		if err == nil || !strings.Contains(userMessage(err), "参数") {
			t.Fatalf("应说明要传给哪个参数，实际：%v", err)
		}
	})

	t.Run("Jenkins 给了分支 + 参数名 → 通过", func(t *testing.T) {
		in := baseIn
		in.Targets = []model.ScheduleTarget{{ProjectKey: "jnk", Branch: "master", BranchParam: "git_branch"}}
		out, err := s.normalize(ctx, in, Caller{})
		if err != nil {
			t.Fatalf("不该报错：%v", err)
		}
		if out.Targets[0].BranchParam != "git_branch" {
			t.Errorf("参数名应保留，实际 %+v", out.Targets[0])
		}
	})

	t.Run("Jenkins 没绑 Job → 建任务时就拒", func(t *testing.T) {
		if err := db.Create(&model.Project{ProjectKey: "jnk-nobind", Name: "x", BuildMethod: "jenkins"}).Error; err != nil {
			t.Fatalf("造项目失败：%v", err)
		}
		in := baseIn
		in.Targets = []model.ScheduleTarget{{ProjectKey: "jnk-nobind"}}
		_, err := s.normalize(ctx, in, Caller{})
		if err == nil || !strings.Contains(userMessage(err), "Job") {
			t.Fatalf("未绑 Job 应被拒，实际：%v", err)
		}
	})
}

// TestNormalizeDeployTargets 部署：环境只有一个，服务可以多个，版本逐条校验。
func TestNormalizeDeployTargets(t *testing.T) {
	db := newTestDB(t)
	s := &ScheduleService{base: &base{deps: Deps{DB: db}}}
	ctx := context.Background()

	if err := db.Create(&model.Environment{EnvKey: "pre", Name: "预发"}).Error; err != nil {
		t.Fatalf("造环境失败：%v", err)
	}
	for _, k := range []string{"svc-a", "svc-b", "svc-c"} {
		if err := db.Create(&model.Project{ProjectKey: k, Name: k}).Error; err != nil {
			t.Fatalf("造项目失败：%v", err)
		}
	}

	t.Run("三个服务 + 各自版本 → 全保留", func(t *testing.T) {
		out, err := s.normalize(ctx, ScheduleInput{
			Name: "发布预发", TaskType: "deploy", EnvKey: "pre", Cron: "0 3 * * *", ImageSource: "fixed",
			Targets: []model.ScheduleTarget{
				{ProjectKey: "svc-a", ImageTag: "1.0.1"},
				{ProjectKey: "svc-b", ImageRecordID: "12", ImageTag: "1.0.2"},
				{ProjectKey: "svc-c", ImageTag: "1.0.3"},
			},
		}, Caller{})
		if err != nil {
			t.Fatalf("不该报错：%v", err)
		}
		if len(out.Targets) != 3 {
			t.Fatalf("应保留 3 个目标，实际 %d", len(out.Targets))
		}
	})

	t.Run("fixed 时**某一个**没给版本 → 报出是哪个项目", func(t *testing.T) {
		_, err := s.normalize(ctx, ScheduleInput{
			Name: "发布预发", TaskType: "deploy", EnvKey: "pre", Cron: "0 3 * * *", ImageSource: "fixed",
			Targets: []model.ScheduleTarget{
				{ProjectKey: "svc-a", ImageTag: "1.0.1"},
				{ProjectKey: "svc-b"},
			},
		}, Caller{})
		if err == nil || !strings.Contains(userMessage(err), "svc-b") {
			t.Fatalf("应指出 svc-b 没选版本，实际：%v", err)
		}
	})

	t.Run("latest 时清掉目标上的固定版本（避免显示与行为不一致）", func(t *testing.T) {
		out, err := s.normalize(ctx, ScheduleInput{
			Name: "发布预发", TaskType: "deploy", EnvKey: "pre", Cron: "0 3 * * *", ImageSource: "latest",
			Targets: []model.ScheduleTarget{{ProjectKey: "svc-a", ImageTag: "9.9.9"}},
		}, Caller{})
		if err != nil {
			t.Fatalf("不该报错：%v", err)
		}
		if out.Targets[0].ImageTag != "" {
			t.Errorf("latest 不该留固定版本，实际 %+v", out.Targets[0])
		}
	})

	t.Run("部署目标不带分支", func(t *testing.T) {
		out, err := s.normalize(ctx, ScheduleInput{
			Name: "发布预发", TaskType: "deploy", EnvKey: "pre", Cron: "0 3 * * *", ImageSource: "latest",
			Targets: []model.ScheduleTarget{{ProjectKey: "svc-a", Branch: "master"}},
		}, Caller{})
		if err != nil {
			t.Fatalf("不该报错：%v", err)
		}
		if out.Targets[0].Branch != "" {
			t.Errorf("部署不该存分支，实际 %+v", out.Targets[0])
		}
	})
}

// TestJenkinsParamsBranch 分支参数必须压过任务级同名参数。
func TestJenkinsParamsBranch(t *testing.T) {
	got := jenkinsParams(map[string]string{"git_branch": "wrong", "env": "prod"},
		model.ScheduleTarget{Branch: "release/2.0", BranchParam: "git_branch"})

	var branch, env int
	for _, p := range got {
		switch p.Name {
		case "git_branch":
			branch++
			if p.Value != "release/2.0" {
				t.Errorf("分支应为 release/2.0，实际 %q", p.Value)
			}
		case "env":
			env++
		}
	}
	if branch != 1 {
		t.Errorf("git_branch 只应出现一次（被目标覆盖，不能再带任务级那份），实际出现 %d 次：%+v", branch, got)
	}
	if env != 1 {
		t.Errorf("其余参数应保留，实际 %+v", got)
	}

	t.Run("没有参数时返回 nil（= 全用 Jenkins 默认值）", func(t *testing.T) {
		if got := jenkinsParams(nil, model.ScheduleTarget{}); got != nil {
			t.Errorf("应为 nil，实际 %+v", got)
		}
	})
}

// TestAggregateScheduleStatus 批量执行的三档结果。
//
// 这一档判错的两种方式都很难受：全成功被报成失败（用户白查一遍），
// 或一半失败被报成成功（以为已经全量上线）。
func TestAggregateScheduleStatus(t *testing.T) {
	cases := []struct {
		total, failed int
		want          string
	}{
		{3, 0, "success"},
		{3, 1, "partial"},
		{3, 2, "partial"},
		{3, 3, "failed"},
		// 防御：目标数为 0 时不能算成功（那只是"什么都没做"）
		{0, 0, "failed"},
	}
	for _, c := range cases {
		if got := aggregateScheduleStatus(c.total, c.failed); got != c.want {
			t.Errorf("total=%d failed=%d → %q，期望 %q", c.total, c.failed, got, c.want)
		}
	}
}
