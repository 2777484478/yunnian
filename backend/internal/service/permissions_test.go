package service

import (
	"context"
	"testing"

	"github.com/2777484478/yunnian/internal/model"
)

// TestListPermissions_IncludesAllEnvs 权限目录必须列出**所有**环境的 env:view:<key>。
//
// 2026-11-10 用户报："环境的权限我只能看到 2 个环境"（实际有 5 个）。
// 根因：`env:view:<envKey>` 是动态权限点，只在**保存角色权限时**才落库
// （见 UpdateRolePermissions），于是 permission 表里只有历史上被勾过的环境，
// 其余环境在角色页根本勾不到 —— 而用户完全看不出为什么。
//
// 这条测试钉住：不管表里有没有，接口都要按当前环境全量补齐。
func TestListPermissions_IncludesAllEnvs(t *testing.T) {
	db := newTestDB(t)
	s := &SystemService{base: &base{deps: Deps{DB: db}}}
	ctx := context.Background()

	envs := []model.Environment{
		{EnvKey: "test-152", Name: "机房测试环境152"},
		{EnvKey: "aliyun-prd", Name: "阿里云生产环境"},
		{EnvKey: "gcp-prd", Name: "gcp生产环境"},
	}
	for i := range envs {
		if err := db.Create(&envs[i]).Error; err != nil {
			t.Fatalf("造环境失败：%v", err)
		}
	}
	// 只预先存一条（模拟"历史上被勾过"的那个），另外两个必须靠动态补齐。
	if err := db.Create(&model.Permission{
		Code: "env:view:test-152", Name: "查看：机房测试环境152", Module: "environment",
	}).Error; err != nil {
		t.Fatalf("造权限点失败：%v", err)
	}

	got, err := s.ListPermissions(ctx)
	if err != nil {
		t.Fatalf("ListPermissions 失败：%v", err)
	}
	byCode := make(map[string]PermissionDTO, len(got))
	for _, p := range got {
		byCode[p.Code] = p
	}

	for _, e := range envs {
		code := "env:view:" + e.EnvKey
		p, ok := byCode[code]
		if !ok {
			t.Errorf("缺少 %s —— 这个环境在角色页里勾不到（正是用户报的问题）", code)
			continue
		}
		if p.Module != "environment" {
			t.Errorf("%s 的模块应为 environment，实际 %q", code, p.Module)
		}
		// 名字要读得懂：动态补齐时用环境名，而不是 key 原样
		if p.Name == "" || p.Name == code {
			t.Errorf("%s 的显示名没取到环境名：%q", code, p.Name)
		}
	}

	// 已存在的那条要保持**表里的**名字（管理员可能改过），不能被覆盖成环境名
	if p := byCode["env:view:test-152"]; p.Name != "查看：机房测试环境152" {
		t.Errorf("已有权限点的名字被覆盖了：%q", p.Name)
	}

	// 三个环境 + 一条其他权限都没有，总共应当正好 3 条 env:view:*
	n := 0
	for _, p := range got {
		if len(p.Code) > 9 && p.Code[:9] == "env:view:" {
			n++
		}
	}
	if n != 3 {
		t.Errorf("应当正好 3 条 env:view:*，实际 %d 条", n)
	}
}

// TestListPermissions_SkipsSoftDeletedEnv 已删除的环境不该出现在权限目录里。
func TestListPermissions_SkipsSoftDeletedEnv(t *testing.T) {
	db := newTestDB(t)
	s := &SystemService{base: &base{deps: Deps{DB: db}}}
	ctx := context.Background()

	keep := model.Environment{EnvKey: "test-152", Name: "机房测试环境152"}
	gone := model.Environment{EnvKey: "old-env", Name: "已下线环境"}
	if err := db.Create(&keep).Error; err != nil {
		t.Fatalf("造环境失败：%v", err)
	}
	if err := db.Create(&gone).Error; err != nil {
		t.Fatalf("造环境失败：%v", err)
	}
	if err := db.Delete(&gone).Error; err != nil {
		t.Fatalf("软删环境失败：%v", err)
	}

	got, err := s.ListPermissions(ctx)
	if err != nil {
		t.Fatalf("ListPermissions 失败：%v", err)
	}
	for _, p := range got {
		if p.Code == "env:view:old-env" {
			t.Error("已删除的环境不该出现在权限目录里（勾了也没有对象）")
		}
	}
}
