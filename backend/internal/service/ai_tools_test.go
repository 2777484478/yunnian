package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestAITools_OnlyReadOnlyTools 工具集**逐字**固定。
//
// 为什么是精确集合而不是"包含"断言：这个包的安全边界是"模型能调到的函数里没有写操作"。
// 将来有人顺手加一个 restart_service，只要它进了 aiTools()，这条断言就会红 ——
// 而那正是需要有人在代码评审里被拦下来的时刻。写成 Contains 就等于放它过去。
//
// 与之配套的是 ai.go 顶部那句「变更靠卡片」：写操作只能以 action 卡片形式给出，
// 由用户在界面上点，走正常 REST（带权限校验与审计）。
func TestAITools_OnlyReadOnlyTools(t *testing.T) {
	a := &AIService{}
	want := []string{
		"list_environments",
		"list_projects",
		"list_env_services",
		"get_service_status",
		"diagnose_service",
		"list_deployments",
		"list_builds",
		"get_cluster_resources",
	}
	got := make([]string, 0, len(want))
	for _, tl := range a.aiTools() {
		got = append(got, tl.Name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("工具集变了，必须人工确认新工具是只读的:\n  得到 %v\n  期望 %v", got, want)
	}

	// 名字层面的兜底：即便上面那份清单被同步修改，写语义的词也不该出现。
	for _, name := range got {
		for _, bad := range []string{"apply", "scale", "restart", "rollback", "delete", "update", "create", "deploy_"} {
			if strings.Contains(name, bad) {
				t.Errorf("工具名 %q 含写语义关键词 %q —— 只读边界被破坏", name, bad)
			}
		}
	}
}

// TestAITools_DeclsCarrySchema 每个工具都要有可用的入参 schema。
//
// 网关对 tools[].function.parameters 为 null 会直接判 400；
// 而无入参的工具（list_environments）最容易写成 nil。
func TestAITools_DeclsCarrySchema(t *testing.T) {
	a := &AIService{}
	decls := a.aiToolDecls()
	if len(decls) == 0 {
		t.Fatal("没有工具声明")
	}
	for _, d := range decls {
		if d.Type != "function" {
			t.Errorf("%s: type 应为 function，得到 %q", d.Function.Name, d.Type)
		}
		if strings.TrimSpace(d.Function.Description) == "" {
			t.Errorf("%s: 缺少描述 —— 模型只能靠描述决定要不要调它", d.Function.Name)
		}
		raw, err := json.Marshal(d.Function.Parameters)
		if err != nil {
			t.Errorf("%s: 参数 schema 无法序列化: %v", d.Function.Name, err)
			continue
		}
		if string(raw) == "null" {
			t.Errorf("%s: 参数 schema 是 null（无入参也要给 {}）", d.Function.Name)
		}
	}
}

// TestCallTool_UnknownNameIsFedBack 调不存在的工具要回灌成可读结果，而不是抛错。
//
// 抛错会让整条对话崩掉；而"静默忽略"更糟 —— 模型会一直重试同一个幻觉函数。
// 正确做法是告诉它有哪些函数可用。
func TestCallTool_UnknownNameIsFedBack(t *testing.T) {
	a := &AIService{}
	res := a.callTool(context.Background(), "restart_service", `{}`)
	if res.Error == "" {
		t.Error("未知工具应记录 error")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(res.Result), &got); err != nil {
		t.Fatalf("结果必须是合法 JSON: %v", err)
	}
	if got["ok"] != false {
		t.Errorf("应回 ok=false，得到 %v", got["ok"])
	}
	if s, _ := got["error"].(string); !strings.Contains(s, "list_environments") {
		t.Errorf("应把可用工具列出来，得到 %q", s)
	}
}

// TestCallTool_BadArgsAreFedBack 参数不是合法 JSON 时同样回灌。
//
// 这是模型的锅，但必须让**它自己**看到并改 —— 报错给用户毫无意义，
// 用户既不知道发生了什么，也无法修正。
func TestCallTool_BadArgsAreFedBack(t *testing.T) {
	a := &AIService{}
	res := a.callTool(context.Background(), "get_service_status", `{"envKey": "test", `)
	var got map[string]any
	if err := json.Unmarshal([]byte(res.Result), &got); err != nil {
		t.Fatalf("结果必须是合法 JSON: %v", err)
	}
	if got["ok"] != false {
		t.Errorf("应回 ok=false，得到 %v", got["ok"])
	}
	if s, _ := got["error"].(string); !strings.Contains(s, "JSON") {
		t.Errorf("应说明是 JSON 问题，得到 %q", s)
	}
}

// TestArgHelpers 参数解析要认两种来源：JSON 数字与模型写成的字符串。
//
// 只认 float64 的话，模型把 limit 写成 "10" 时会**静默**退回默认值 ——
// 用户看到的是"我说了 20 条却只给 5 条"，且没有任何报错可查。
func TestArgHelpers(t *testing.T) {
	args := map[string]any{
		"envKey": "  test  ",
		"num":    float64(7),
		"strnum": "12",
		"junk":   "abc",
		"big":    float64(9999),
		"neg":    float64(-5),
	}
	if got := argStr(args, "envKey"); got != "test" {
		t.Errorf("argStr 应去空白，得到 %q", got)
	}
	if got := argStr(args, "missing"); got != "" {
		t.Errorf("缺失键应返回空串，得到 %q", got)
	}
	if got := argInt(args, "num", 5, 1, 20); got != 7 {
		t.Errorf("数字参数解析错: %d", got)
	}
	if got := argInt(args, "strnum", 5, 1, 20); got != 12 {
		t.Errorf("字符串数字也要认: %d", got)
	}
	if got := argInt(args, "junk", 5, 1, 20); got != 5 {
		t.Errorf("无法解析时退回默认值: %d", got)
	}
	if got := argInt(args, "big", 5, 1, 20); got != 20 {
		t.Errorf("超过上限要夹住: %d", got)
	}
	if got := argInt(args, "neg", 5, 1, 20); got != 1 {
		t.Errorf("低于下限要夹住: %d", got)
	}
	if got := argInt(args, "missing", 5, 1, 20); got != 5 {
		t.Errorf("缺失键用默认值: %d", got)
	}
}

// TestSchema_NilPropsBecomesEmptyObject nil 入参要变成 {}，不能是 null。
func TestSchema_NilPropsBecomesEmptyObject(t *testing.T) {
	raw, _ := json.Marshal(schema(nil, nil))
	// 比结构而不是比字符串：Go 的 map 序列化按 key 字典序，
	// 写死字面量会让这条断言在无关改动下误红。
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("不是合法 JSON: %v", err)
	}
	if got["type"] != "object" {
		t.Errorf("type 应为 object，得到 %v", got["type"])
	}
	props, ok := got["properties"].(map[string]any)
	if !ok || len(props) != 0 {
		t.Errorf("properties 应是空对象，得到 %v", got["properties"])
	}
	if _, ok := got["required"]; ok {
		t.Error("没有 required 时不该写出该字段")
	}
}

// TestTailLines 只保留末尾若干行，且不因结尾换行多出一行。
func TestTailLines(t *testing.T) {
	if got := tailLines("a\nb\nc\n", 2); got != "b\nc" {
		t.Errorf("应取末两行且忽略结尾换行，得到 %q", got)
	}
	if got := tailLines("a\nb", 5); got != "a\nb" {
		t.Errorf("不足 n 行时原样返回，得到 %q", got)
	}
}

// TestAirToolLabelEveryToolHasLabel 每个工具都要有给人看的步骤名。
//
// 界面上写的是"正在读取环境名录"而不是"list_environments"——
// 漏一个就会在界面上冒出裸函数名。
func TestAIToolLabelEveryToolHasLabel(t *testing.T) {
	a := &AIService{}
	for _, tl := range a.aiTools() {
		label := aiToolLabel(tl.Name)
		if strings.Contains(label, tl.Name) {
			t.Errorf("工具 %s 没有专属中文步骤名（会显示成函数名）", tl.Name)
		}
	}
}

// TestFailResultAlwaysCarriesOKFalse failResult 是失败回灌的唯一出口。
//
// 少一个 ok 字段，模型就会把"查不到"读成"没问题"—— 结论正好相反。
func TestFailResultAlwaysCarriesOKFalse(t *testing.T) {
	raw, _ := json.Marshal(failResult(context.DeadlineExceeded))
	var got map[string]any
	_ = json.Unmarshal(raw, &got)
	if got["ok"] != false || got["error"] == nil {
		t.Errorf("failResult 必须带 ok=false 与 error，得到 %s", raw)
	}
}
