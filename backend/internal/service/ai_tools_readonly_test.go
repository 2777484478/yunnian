package service

import (
	"sort"
	"strings"
	"testing"
)

// TestAIToolsAreReadOnly 钉住「AI 助手只能查、不能改」这条结构性保证。
//
// 为什么是白名单而不是黑名单（比如禁止名字里含 restart/scale）：
// 黑名单永远漏 —— 以后新增 `apply_release`、`patch_params` 这类名字，
// 只要不含被禁的词就绕过去了。白名单逼着**新增工具必须显式登记**，
// 登记的那一刻就会被迫回答这个问题："这个工具会改东西吗？"
//
// 这条测试的价值不在于"现在只有 8 个工具"，而在于：
// **将来有人想给 AI 加一个写能力时，CI 会先亮红灯**，而不是等它在生产上
// 把某个服务重启了才发现。
func TestAIToolsAreReadOnly(t *testing.T) {
	// 已登记的全部工具。新增任何一个都必须在这里加一行 —— 加的时候请自问：
	// 它会不会改变平台或集群的状态？会，就不该出现在这张表里。
	want := []string{
		"diagnose_service",
		"get_cluster_resources",
		"get_service_status",
		"list_builds",
		"list_deployments",
		"list_env_services",
		"list_environments",
		"list_projects",
	}

	a := &AIService{}
	var got []string
	for _, tool := range a.aiTools() {
		got = append(got, tool.Name)
	}
	sort.Strings(got)

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("AI 工具清单变了。\n"+
			"  期望：%v\n  实际：%v\n\n"+
			"如果这是**新增的只读工具**：把它加进 want 列表。\n"+
			"如果这是**写工具**（会改平台/集群状态）：不要加 —— AI 助手是只读的，\n"+
			"变更必须由人点按钮走正常 REST（带权限与审计），不能让模型代劳。",
			want, got)
	}
}
