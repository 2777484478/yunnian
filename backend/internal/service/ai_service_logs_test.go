package service

import (
	"io"
	"strings"
	"testing"
)

// javaishLog 一段仿真日志：正常行 + 一次带堆栈的报错 + 后面恢复正常。
func javaishLog() string {
	return strings.Join([]string{
		`2026-11-04 09:00:01 INFO  [main] Starting Application v1.2.0.3396`,
		`2026-11-04 09:00:02 INFO  [main] Tomcat started on port 8080`,
		`2026-11-04 09:01:10 WARN  [http-1] upstream slow: 1200ms`,
		`2026-11-04 09:01:11 ERROR [http-1] Failed to call mcp-server`,
		`java.net.SocketTimeoutException: Read timed out`,
		`	at okhttp3.internal.http2.Http2Stream.waitForIo(Http2Stream.java:605)`,
		`	at okhttp3.internal.http2.Http2Stream.takeHeaders(Http2Stream.java:154)`,
		`Caused by: java.io.InterruptedIOException: timeout`,
		`2026-11-04 09:01:12 INFO  [http-1] fallback to cached response`,
		`2026-11-04 09:02:00 INFO  [main] heartbeat ok`,
	}, "\n")
}

func TestScanLogText_HitsWithContext(t *testing.T) {
	hits, scanned := scanLogText("pod-a", javaishLog(), defaultLogPatterns, 2, 8)

	if scanned != 10 {
		t.Errorf("应扫过 10 行，实际 %d", scanned)
	}
	if len(hits) == 0 {
		t.Fatal("这段日志里明明有 ERROR，却一处都没命中")
	}
	// 第 4 行是 ERROR。多行堆栈会被合并成一段，所以这里只断言"第一条命中在第 4 行"，
	// 不断言总条数 —— 总条数由日志内容决定，钉死它等于把测试写成实现副本。
	first := hits[0]
	if first.Line != 4 {
		t.Errorf("第一条命中应在第 4 行，实际第 %d 行", first.Line)
	}
	if !strings.Contains(first.Text, "Failed to call mcp-server") {
		t.Errorf("命中行原文不对：%q", first.Text)
	}
	if first.Pod != "pod-a" {
		t.Errorf("Pod 名没带出来：%q", first.Pod)
	}
	// 上下文：前面 2 行（2、3），后面 2 行（5、6）
	if len(first.Before) != 2 {
		t.Errorf("应给 2 行上文，实际 %d：%v", len(first.Before), first.Before)
	}
	if len(first.After) != 2 {
		t.Errorf("应给 2 行下文，实际 %d：%v", len(first.After), first.After)
	}
	if !strings.Contains(strings.Join(first.After, "\n"), "SocketTimeoutException") {
		t.Error("下文里应该能看到异常类名 —— 那正是判断错误类型的关键行")
	}
}

// TestScanLogText_MergesAdjacentHits 堆栈相邻行不该被拆成好几条重叠片段。
//
// 一条 Java 报错里 `ERROR` / `Exception` / `Caused by` 连续好几行都命中，
// 每行单开一条 = 同一处问题回 4 条互相重叠的内容，把额度占光。
func TestScanLogText_MergesAdjacentHits(t *testing.T) {
	log := "ERROR boom\njava.lang.IllegalStateException: x\nCaused by: timeout\nCaused by: refused\n"
	hits, _ := scanLogText("p", log, defaultLogPatterns, 2, 8)
	if len(hits) != 1 {
		t.Fatalf("连续 4 行命中应合并成 1 段，实际 %d 段：%+v", len(hits), hits)
	}
}

func TestScanLogText_CapsHits(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString("ERROR line ")
		b.WriteString(strings.Repeat("x", 5))
		b.WriteString("\nok\n")
	}
	hits, scanned := scanLogText("p", b.String(), defaultLogPatterns, 0, 3)
	if len(hits) != 3 {
		t.Errorf("maxHits=3，实际返回 %d 条", len(hits))
	}
	// 截断时 scanned 仍报**总行数**：模型靠它判断"是不是没扫完"。
	if scanned != 80 {
		t.Errorf("应报总行数 80，实际 %d", scanned)
	}
}

// TestScanLogText_NoHitStillReportsScanned 0 命中也要有"扫了多少行"。
//
// 这是这个工具最容易被误读的地方：回一个空数组，模型会说"日志没有异常"。
// 有 scanned 在，模型才可能说出"扫了 N 行没匹配到"。
func TestScanLogText_NoHitStillReportsScanned(t *testing.T) {
	hits, scanned := scanLogText("p", "a\nb\nc\n", defaultLogPatterns, 2, 8)
	if len(hits) != 0 {
		t.Errorf("不该命中：%+v", hits)
	}
	if scanned != 3 {
		t.Errorf("0 命中时应报扫过 3 行，实际 %d", scanned)
	}
}

func TestScanLogText_Empty(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\n"} {
		hits, scanned := scanLogText("p", in, defaultLogPatterns, 2, 8)
		if len(hits) != 0 || scanned != 0 {
			t.Errorf("空日志应返回 0/0，得到 %d/%d（输入 %q）", len(hits), scanned, in)
		}
	}
}

// TestScanLogText_ChinesePatterns 中文日志也要能命中（不少服务在打中文）。
func TestScanLogText_ChinesePatterns(t *testing.T) {
	hits, _ := scanLogText("p", "开始处理\n连接数据库失败\n重试中\n", defaultLogPatterns, 1, 8)
	if len(hits) != 1 || !strings.Contains(hits[0].Text, "失败") {
		t.Fatalf("中文「失败」应命中一行，实际：%+v", hits)
	}
}

func TestSplitLogKeywords(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"NullPointer,504", []string{"nullpointer", "504"}},
		{"a，b、c;d", []string{"a", "b", "c", "d"}},
		{"  A   a  ", []string{"a"}}, // 去重 + 去空白 + 转小写
		{"", nil},
		{",,,", nil},
	}
	for _, c := range cases {
		got := splitLogKeywords(c.in)
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("splitLogKeywords(%q) = %v，期望 %v", c.in, got, c.want)
		}
	}
}

func TestLogLineMatches_CaseInsensitive(t *testing.T) {
	for _, line := range []string{"ERROR x", "Error x", "error x"} {
		if !logLineMatches(strings.ToLower(line), defaultLogPatterns) {
			t.Errorf("%q 应命中", line)
		}
	}
	if logLineMatches("all good", defaultLogPatterns) {
		t.Error("正常行不该命中")
	}
}

// TestReadLogCapped 读上限与"是否截断"的判定。
//
// 多读 1 字节的意义就在这里：刚好等于上限时必须判为**未截断**，
// 否则每次都会谎报 truncated=true，模型会以为日志被切过。
func TestReadLogCapped(t *testing.T) {
	t.Run("小于上限不截断", func(t *testing.T) {
		s, truncated := readLogCapped(io.NopCloser(strings.NewReader("abc")), 10)
		if s != "abc" || truncated {
			t.Errorf("得到 %q truncated=%v", s, truncated)
		}
	})
	t.Run("刚好等于上限也不截断", func(t *testing.T) {
		s, truncated := readLogCapped(io.NopCloser(strings.NewReader("abcde")), 5)
		if s != "abcde" || truncated {
			t.Errorf("得到 %q truncated=%v", s, truncated)
		}
	})
	t.Run("超过上限截断且保留头部", func(t *testing.T) {
		s, truncated := readLogCapped(io.NopCloser(strings.NewReader("abcdefg")), 5)
		if s != "abcde" {
			t.Errorf("应保留头部 5 字节，得到 %q", s)
		}
		if !truncated {
			t.Error("超长时应标记 truncated")
		}
	})
	t.Run("nil 流不 panic", func(t *testing.T) {
		if s, _ := readLogCapped(nil, 10); s != "" {
			t.Errorf("nil 流应返回空串，得到 %q", s)
		}
	})
}

// TestSearchServiceLogsToolDeclared 声明层面的检查（名字/必填入参/标签）。
//
// 精确清单在 ai_tools_test.go 与 ai_tools_readonly_test.go；
// 这里只钉这个工具自己的"契约"：两个必填参数 + 有前端标签。
func TestSearchServiceLogsToolDeclared(t *testing.T) {
	a := &AIService{}
	tool := a.findTool("search_service_logs")
	if tool == nil {
		t.Fatal("search_service_logs 没有注册进 aiTools()")
	}
	if strings.TrimSpace(tool.Description) == "" {
		t.Error("缺少描述：模型靠它决定什么时候调用")
	}
	// 只读边界：描述里若出现"可以帮你重启/回滚"这类承诺，就是假承诺。
	for _, bad := range []string{"重启", "回滚", "修改"} {
		if strings.Contains(tool.Description, bad) {
			t.Errorf("描述里出现写语义 %q —— 这个工具是只读的", bad)
		}
	}
	if label := aiToolLabel("search_service_logs"); strings.Contains(label, "search_service_logs") {
		t.Errorf("缺人话标签，前端会显示函数名：%q", label)
	}
	props, _ := tool.Parameters["properties"].(map[string]any)
	for _, k := range []string{"envKey", "projectKey", "keyword", "lines", "pod"} {
		if _, ok := props[k]; !ok {
			t.Errorf("入参缺少 %q", k)
		}
	}
	req, _ := tool.Parameters["required"].([]string)
	if strings.Join(req, ",") != "envKey,projectKey" {
		t.Errorf("required 应为 envKey,projectKey，实际 %v", req)
	}
}
