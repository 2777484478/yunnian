package auth

import "testing"

// TestMatch_NewPermissionPoints 2026-11-10 新增的三个权限点与既有通配的关系。
//
// 这组用例的价值不是"Match 能跑"，而是**钉住命名决策**：
// K8s 资源的权限点为什么不能叫 `k8s:read`、重启为什么必须是 `deploy:restart`。
// 后来人想改名时，这些断言会立刻说明后果。
func TestMatch_NewPermissionPoints(t *testing.T) {
	cases := []struct {
		name  string
		perms []string
		want  string
		ok    bool
	}{
		// ---------- 重启：deploy:restart
		{"ops 的 deploy:* 覆盖 restart", []string{"deploy:*"}, "deploy:restart", true},
		{"admin 的 * 覆盖 restart", []string{"*"}, "deploy:restart", true},
		{"dev 的 deploy:test,staging 不覆盖 restart", []string{"deploy:test,staging"}, "deploy:restart", false},
		{"qa 的 deploy:test 不覆盖 restart", []string{"deploy:test"}, "deploy:restart", false},
		{"readonly 的 *:read 不覆盖 restart", []string{"*:read"}, "deploy:restart", false},
		{"显式授予 restart", []string{"deploy:restart"}, "deploy:restart", true},

		// ---------- K8s 资源：k8s:view / k8s:manage
		{"ops 显式拿到 k8s:view", []string{"k8s:view"}, "k8s:view", true},
		{"admin 的 * 覆盖 k8s:view", []string{"*"}, "k8s:view", true},
		// ⚠️ 这两条是命名决策的核心：若改叫 k8s:read，只读角色会被 `*:read` 放行，
		// 而用户明确要求"开发与普通用户都不能查看 K8s 资源"。
		{"readonly 的 *:read **不**覆盖 k8s:view", []string{"*:read"}, "k8s:view", false},
		{"dev/qa 的常规读权限不覆盖 k8s:view", []string{"project:read", "build:read", "log:read"}, "k8s:view", false},
		{"k8s:manage 不被 k8s:view 覆盖", []string{"k8s:view"}, "k8s:manage", false},
		{"k8s:manage 持有者同时可写", []string{"k8s:manage"}, "k8s:manage", true},

		// ---------- 对照组：确认 *:read 确实会覆盖常规读权限（说明上面的排除是刻意的）
		{"readonly 能看项目", []string{"*:read"}, "project:read", true},
		{"readonly 能看环境", []string{"*:read"}, "env:read", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Match(c.perms, c.want); got != c.ok {
				t.Fatalf("Match(%v, %q) = %v，期望 %v", c.perms, c.want, got, c.ok)
			}
		})
	}
}

// TestMatch_EnvViewIsExact 环境级查看权限是**精确匹配**，不会被 env:read 之类覆盖。
//
// 权限目录里 `env:view:<envKey>` 是按环境生成的；这里确认它的匹配语义没被
// 通配误伤（否则"只给看 152"会变成"能看全部"）。
func TestMatch_EnvViewIsExact(t *testing.T) {
	if !Match([]string{"env:view:test-152"}, "env:view:test-152") {
		t.Error("配了 test-152 应当放行 test-152")
	}
	if Match([]string{"env:view:test-152"}, "env:view:aliyun-prd") {
		t.Error("配了 test-152 不该放行 aliyun-prd")
	}
	if Match([]string{"env:read"}, "env:view:test-152") {
		t.Error("env:read 只表达「能进环境列表」，不该等价于「能看每一个环境」")
	}
	if !Match([]string{"env:*"}, "env:view:test-152") {
		t.Error("env:* 是模块级通配，应当覆盖 env:view:<key>")
	}
}
