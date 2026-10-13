package service

import (
	"strings"
	"testing"
	"time"
)

// renderTag 的「内联偏移」({buildNo+N}) 是"版本号不从 1 起"的唯一实现方式 ——
// 脚本项目的默认规则 `2.2.0.{buildNo+999}` 就靠它让第 1 次构建产出 2.2.0.1000。
// 把几种形态都钉住，避免以后改 renderTag 时把这个能力悄悄改没了。
func TestRenderTagBuildNoOffset(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		pattern string
		buildNo int64
		want    string
	}{
		{"v{buildNo}", 7, "v7"},
		{"2.2.0.{buildNo+999}", 1, "2.2.0.1000"},
		{"2.2.0.{buildNo+999}", 2, "2.2.0.1001"},
		{"2.2.0.{buildNo+995}", 5, "2.2.0.1000"},
		{"v{buildNo-1}", 3, "v2"},
		{"v{buildNo-5}", 3, "v0"},
		{"{buildNo+99}-{buildNo}", 1, "100-1"},
		{"{branch}-{buildNo+999}", 1, "master-1000"},
		{"", 4, "v4"},
	}
	for _, c := range cases {
		got := renderTag(c.pattern, "master", "abcdef1234567890", c.buildNo, at)
		if got != c.want {
			t.Errorf("renderTag(%q, buildNo=%d) = %q, want %q", c.pattern, c.buildNo, got, c.want)
		}
	}
}

// 渲染结果里不能再残留任何 `{...}` 占位符：漏掉一个会让 tag 里出现花括号，
// 而 Docker 会拒绝这种 tag，报错却完全指不到模板上去。
func TestRenderTagLeavesNoPlaceholder(t *testing.T) {
	got := renderTag("2.2.0.{buildNo+999}-{branch}-{commitId}-{date}", "master", "abcdef1234567890", 1, time.Now())
	if strings.ContainsAny(got, "{}") {
		t.Fatalf("渲染后仍含花括号: %q", got)
	}
}

// 按构建方式选默认规则：脚本项目从 2.2.0.1000 起，其余仍是 v{buildNo}。
func TestDefaultTagPatternByMethod(t *testing.T) {
	if got := defaultTagPattern("script"); got != "2.2.0.{buildNo+999}" {
		t.Errorf("script 默认规则 = %q", got)
	}
	if got := defaultTagPattern("dockerfile"); got != "v{buildNo}" {
		t.Errorf("dockerfile 默认规则 = %q", got)
	}
	if got := defaultTagPattern("jenkins"); got != "v{buildNo}" {
		t.Errorf("jenkins 默认规则 = %q", got)
	}
}
