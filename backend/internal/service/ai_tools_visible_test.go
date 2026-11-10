package service

import (
	"testing"

	"github.com/2777484478/yunnian/internal/auth"
)

// TestAssertToolEnvVisible 工具侧的环境可见性断言。
//
// 这条规则决定了「有 ai:query 的人能不能通过 AI 助手读到 prod」——
// 之前的实际情况是**能**：工具拿不到 Caller，一律不限制。
func TestAssertToolEnvVisible(t *testing.T) {
	cases := []struct {
		name   string
		perms  []string
		envKey string
		wantOK bool
	}{
		{"超管（*）不受限", []string{"*"}, "prod", true},
		{"env:* 不受限", []string{"env:*"}, "prod", true},
		{"没配可见性 → 全部不可见（2026-11-10 语义反转）", []string{"ai:query", "deploy:read"}, "prod", false},
		{"配了 test-152，查 test-152 放行", []string{"ai:query", "env:view:test-152"}, "test-152", true},
		{"配了 test-152，查 prod 拒绝", []string{"ai:query", "env:view:test-152"}, "prod", false},
		{"配了两个，第二个放行", []string{"env:view:test-152", "env:view:staging"}, "staging", true},
		{"配了两个，第三个拒绝", []string{"env:view:test-152", "env:view:staging"}, "prod", false},
		{"空 envKey 不算违规（工具允许不传）", []string{"env:view:test-152"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Caller{User: &auth.CurrentUser{Permissions: tc.perms}}
			err := assertToolEnvVisible(c, tc.envKey)
			if tc.wantOK && err != nil {
				t.Fatalf("期望放行，却拒绝了：%v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("期望拒绝，却放行了")
			}
		})
	}
}

// TestVisibleEnvKeys_NoConfigMeansNone 钉住新语义：
// **一个 env:view 都没配 = 全部不可见**（2026-11-10 语义反转，升级靠 MigrateEnvVisibility 兜底）。
func TestVisibleEnvKeys_NoConfigMeansNone(t *testing.T) {
	c := Caller{User: &auth.CurrentUser{Permissions: []string{"deploy:read", "ai:query"}}}
	allowed, restricted := c.VisibleEnvKeys()
	if !restricted || len(allowed) != 0 {
		t.Fatalf("没配 env:view 时应 restricted=true 且为空，实际 restricted=%v allowed=%v", restricted, allowed)
	}
}

// TestDeployableEnvKeys 发布拓扑的「可部署环境」判断：deploy:<env> 勾了才展示圈。
func TestDeployableEnvKeys(t *testing.T) {
	cases := []struct {
		name      string
		perms     []string
		wantAll   bool
		want      []string
	}{
		{"超管 deploy:* 全部可部署", []string{"deploy:*"}, true, nil},
		{"枚举 deploy:test,staging 拆成两个", []string{"deploy:test,staging"}, false, []string{"test", "staging"}},
		{"单个 deploy:test-152", []string{"deploy:test-152"}, false, []string{"test-152"}},
		{"功能点不算环境（deploy:read/restart）", []string{"deploy:read", "deploy:restart"}, false, nil},
		{"没配 → 空（拓扑不展示圈）", []string{"env:read", "log:read"}, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Caller{User: &auth.CurrentUser{Permissions: tc.perms}}
			allowed, restricted := c.DeployableEnvKeys()
			if tc.wantAll {
				if restricted {
					t.Fatalf("期望全部可部署（restricted=false），实际 restricted=true allowed=%v", allowed)
				}
				return
			}
			if !restricted {
				t.Fatalf("期望受限，实际 unrestricted")
			}
			if len(allowed) != len(tc.want) {
				t.Fatalf("期望 %v，实际 %v", tc.want, allowed)
			}
			for i := range tc.want {
				if allowed[i] != tc.want[i] {
					t.Fatalf("期望 %v，实际 %v", tc.want, allowed)
				}
			}
		})
	}
}
