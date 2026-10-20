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
		{"没配可见性 → 全部可见（兼容存量角色）", []string{"ai:query", "deploy:read"}, "prod", true},
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

// TestVisibleEnvKeys_NoConfigMeansAll 钉住那条保命条款：
// **一个 env:view 都没配 = 全部可见**。改成"没配就看不到"会让升级当天全员空白。
func TestVisibleEnvKeys_NoConfigMeansAll(t *testing.T) {
	c := Caller{User: &auth.CurrentUser{Permissions: []string{"deploy:read", "ai:query"}}}
	allowed, restricted := c.VisibleEnvKeys()
	if restricted {
		t.Fatalf("没配 env:view 时不应限制，实际 restricted=true allowed=%v", allowed)
	}
}
