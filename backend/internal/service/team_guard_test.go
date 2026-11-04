package service

import (
	"context"
	"strings"
	"testing"

	"github.com/2777484478/yunnian/internal/model"
)

// TestAssertTeamAssignable 钉住「项目只能挂到真实存在且启用的团队」。
//
// 这条校验是 2026-11-04 team 收口的核心：`project.team` 存的是**名字**，
// 而名字由 `/teams` 维护。没有校验时，一条直接打 API 的请求就能写进一个
// 不存在的团队名 —— 后果是静默的：团队页的项目数不含它、按团队反查的数据权限
// 漏掉它、项目详情却照常显示那个没人认识的名字。
func TestAssertTeamAssignable(t *testing.T) {
	db := newTestDB(t)
	s := &CIService{base: &base{deps: Deps{DB: db}}}
	ctx := context.Background()

	mk := func(name string, enabled bool) {
		// ⚠️ GORM 陷阱：`team.enabled` 带 `default:true`，而 bool 的零值 false
		// 会被当成"没给"，于是 Create{Enabled:false} 存进去仍是 true。
		// 想造"停用"的数据必须再显式 Update 一次。
		// （业务侧不受影响：停用走的是 UpdateTeam 的 Updates(map)，显式给了 false。）
		tm := model.Team{Name: name, Enabled: true}
		if err := db.Create(&tm).Error; err != nil {
			t.Fatalf("造团队失败：%v", err)
		}
		if !enabled {
			if err := db.Model(&model.Team{}).Where("id = ?", tm.ID).
				Update("enabled", false).Error; err != nil {
				t.Fatalf("停用团队失败：%v", err)
			}
		}
	}
	mk("基础架构组", true)
	mk("已解散组", false)

	cases := []struct {
		name    string
		team    string
		oldTeam string
		wantErr bool
	}{
		{"存在的团队 → 通过", "基础架构组", "", false},
		{"不存在的团队 → 拒", "不存在的组", "", true},
		{"空值 → 拒（项目必须有团队）", "", "", true},
		{"只有空白 → 拒", "   ", "", true},
		{"已停用的团队 → 拒（不许再往里加项目）", "已解散组", "", true},
		// 关键的一条：**原值不改时，即使团队已停用也放行**。
		// 否则停用一个团队会把已挂在上面的项目锁死 —— 改一句描述都保存不了。
		{"保持原值 + 该团队已停用 → 通过", "已解散组", "已解散组", false},
		{"从停用团队改到启用团队 → 通过", "基础架构组", "已解散组", false},
		{"从启用团队改到停用团队 → 拒", "已解散组", "基础架构组", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := s.assertTeamAssignable(ctx, c.team, c.oldTeam)
			if c.wantErr && err == nil {
				t.Fatalf("team=%q old=%q 应被拒，却放行了", c.team, c.oldTeam)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("team=%q old=%q 应放行，却被拒：%v", c.team, c.oldTeam, err)
			}
		})
	}
}

// TestAssertTeamAssignable_MessageIsActionable 拒绝时要说清"怎么办"。
//
// "团队不存在"这种话对用户没用 —— 他不知道该去哪个页面建。
// 这条断言故意只检查是否提到入口，不锁死整句文案。
func TestAssertTeamAssignable_MessageIsActionable(t *testing.T) {
	db := newTestDB(t)
	s := &CIService{base: &base{deps: Deps{DB: db}}}
	err := s.assertTeamAssignable(context.Background(), "查无此组", "")
	if err == nil {
		t.Fatal("不存在的团队应被拒")
	}
	msg := err.Error()
	// errs.Error.Error() 的格式是 `code=NNNNN 人话: 因`，人话在最前面，
	// 所以这里只要确认关键字在，就说明用户能看到引导。
	for _, want := range []string{"团队", "不存在"} {
		if !strings.Contains(msg, want) {
			t.Errorf("拒绝原因里缺少 %q，用户不知道该去哪创建：%s", want, msg)
		}
	}
	if !strings.Contains(msg, "系统管理") {
		t.Errorf("应告诉用户去哪个页面创建团队：%s", msg)
	}
}
