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

	base := ScheduleInput{Name: "夜间构建", TaskType: "build", ProjectKey: "demo", Cron: "0 2 * * *"}

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
		}, "要指定发哪个版本"},
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
	if !strings.Contains(reason, "还在执行中") {
		t.Errorf("跳过原因应说清楚，实际：%q", reason)
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
			TaskType: "deploy", EnvKey: "prod", ProjectKey: "demo",
			ImageSource: "fixed", ImageRecordID: "42", ImageTag: "1.2.0.100",
		}, "")
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
			TaskType: "deploy", EnvKey: "prod", ProjectKey: "demo",
			ImageSource: "fixed", ImageTag: "1.2.0.999",
		}, "")
		if err != nil {
			t.Fatalf("不该报错：%v", err)
		}
		if in.ImageSource != "registry" || in.RegistryTag != "1.2.0.999" {
			t.Errorf("应走 registry 来源，实际 %+v", in)
		}
	})

	t.Run("latest 取平台最新构建", func(t *testing.T) {
		in, err := s.buildDeployInput(ctx, &model.ScheduleTask{
			TaskType: "deploy", EnvKey: "prod", ProjectKey: "demo", ImageSource: "latest",
		}, "")
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
			TaskType: "deploy", EnvKey: "prod", ProjectKey: "no-build-record", ImageSource: "latest",
		}, "")
		if err == nil {
			t.Fatal("没有构建记录时应报错")
		}
		if !strings.Contains(userMessage(err), "没有任何构建记录") {
			t.Errorf("错误信息应说清原因，实际：%q", userMessage(err))
		}
	})

	t.Run("fixed 但没给版本 → 报错而不是静默发最新", func(t *testing.T) {
		_, err := s.buildDeployInput(ctx, &model.ScheduleTask{
			TaskType: "deploy", EnvKey: "prod", ProjectKey: "demo", ImageSource: "fixed",
		}, "")
		if err == nil {
			t.Fatal("没指定版本时应报错")
		}
	})
}
