package service

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/2777484478/yunnian/internal/model"
)

// TestAuditOperatorOverride 钉住「登录那条审计必须写明操作人」。
//
// 线上真实表现（2026-10-30 用户报）：操作审计里 admin 的登录记录，
// 操作人一列是「匿名」，而紧挨着的「退出登录」是「系统管理员」。
//
// 根因：登录成功那一刻 `Caller.User` 还是 nil —— 认证就发生在这段代码里，
// 审计照原样取操作人只能是"匿名"。可我们明明刚校验过密码、知道是谁。
// 事后想追"谁在几点登录过"，整列全是匿名，这条日志等于没写。
//
// 这个测试同时钉住反面：**没有 Operator 时仍然记匿名**。
// 登录失败（撞库、密码错）就该是匿名 —— 不能因为"知道了账号"就把
// 那次尝试算到那个账号头上，那会污染"这个用户做过什么"的判断。
func TestAuditOperatorOverride(t *testing.T) {
	db := newTestDB(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := &base{deps: Deps{DB: db, Log: log}}
	ctx := context.Background()

	t.Run("登录成功：操作人是刚通过校验的那个账号", func(t *testing.T) {
		b.audit(ctx, Caller{IP: "10.0.0.9", UA: "test"},
			auditInput{
				Module: "认证", Action: "login", TargetType: "user",
				TargetName: "系统管理员",
				Operator: &auditOperator{
					ID: 7, Name: "系统管理员", Username: "admin", RoleCode: "admin",
				},
				Detail: "登录成功", Result: "success",
			})

		var rec model.AuditLog
		if err := db.Order("id DESC").First(&rec).Error; err != nil {
			t.Fatalf("读审计记录失败：%v", err)
		}
		if rec.UserName != "系统管理员" {
			t.Errorf("操作人应为「系统管理员」，实际 %q —— 写成匿名就是这次要修的 bug", rec.UserName)
		}
		if rec.Username != "admin" {
			t.Errorf("操作账号应为 admin，实际 %q", rec.Username)
		}
		if rec.UserID == nil || *rec.UserID != 7 {
			t.Errorf("操作人 ID 应为 7，实际 %v", rec.UserID)
		}
		if rec.RoleCode != "admin" {
			t.Errorf("角色应一起记下来（列表要显示角色标签），实际 %q", rec.RoleCode)
		}
		// 操作对象不变：这条记录是"admin 登录了 admin 自己"
		if rec.TargetName != "系统管理员" || rec.TargetType != "user" {
			t.Errorf("操作对象不该被 Operator 覆盖，实际 %+v", rec)
		}
	})

	t.Run("没有 Operator（登录失败 / 撞库）：仍记匿名", func(t *testing.T) {
		b.audit(ctx, Caller{IP: "10.0.0.9"},
			auditInput{
				Module: "认证", Action: "login", TargetType: "user",
				TargetName: "admin",
				Detail:     "登录失败：账号或密码错误", Result: "failed", ErrMsg: "账号或密码错误",
			})

		var rec model.AuditLog
		if err := db.Order("id DESC").First(&rec).Error; err != nil {
			t.Fatalf("读审计记录失败：%v", err)
		}
		if rec.UserName != "匿名" {
			t.Errorf("登录失败应保持匿名（不能算到被尝试的那个账号头上），实际 %q", rec.UserName)
		}
		if rec.UserID != nil {
			t.Errorf("登录失败不该挂操作人 ID，实际 %v", *rec.UserID)
		}
		if rec.TargetName != "admin" {
			t.Errorf("被尝试的账号应记在操作对象上，实际 %q", rec.TargetName)
		}
	})
}
