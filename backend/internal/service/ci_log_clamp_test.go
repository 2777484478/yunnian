package service

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 这些用例守的是一条实测踩到的缺陷：日志落库前用 `content[:2000]` 按**字节**截断，
// 切点落在中文（3 字节/字）中间会留下半个字符 → 非法 UTF-8 → MySQL utf8mb4 列
// 直接拒收整条 INSERT（Error 1366 Incorrect string value），而当时的实现批量写失败
// 就 `return`，结果那次构建的日志**一行都没落库**。
//
// 所以下面的断言不是"截断得好看"，而是：**任何输入都必须是合法 UTF-8**。
func TestClampLogLineAlwaysValidUTF8(t *testing.T) {
	cases := []struct {
		name string
		in   string
		max  int
	}{
		{"短 ASCII 原样", "hello world", 2000},
		{"刚好等于上限", strings.Repeat("a", 2000), 2000},
		{
			// 2000 字节的纯中文：1900/3 除不尽，切点必然落在字符中间
			"纯中文超长", strings.Repeat("构", 800), 2000,
		},
		{
			// 混合内容，切点随机落在某处
			"中英混排超长", strings.Repeat("构建失败：docker push error from registry ", 100), 2000,
		},
		{
			// 上游给的就是半截（docker 按块读，多字节字符会被切在两块之间）
			"尾部半截字符", "正常内容" + string([]byte{0xE6, 0x94}), 2000,
		},
		{
			"中间夹非法字节", "abc" + string([]byte{0xFF, 0xFE}) + "def", 2000,
		},
		{
			// 4 字节字符（emoji）也要照顾到
			"emoji 超长", strings.Repeat("🚀", 600), 2000,
		},
		{"空串", "", 2000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := clampLogLine(c.in, c.max)
			if !utf8.ValidString(got) {
				t.Fatalf("结果不是合法 UTF-8：%q", got)
			}
			if len(got) > c.max {
				t.Fatalf("结果超长：%d > %d", len(got), c.max)
			}
		})
	}
}

// 未超长的内容必须**一字不动**：日志是要给人看证据的，不能顺手改写。
func TestClampLogLineKeepsShortContentIntact(t *testing.T) {
	in := "===== [5/5] 推送镜像 =====\ndocker push es-repo.example.com:5000/escloud/app:v3"
	if got := clampLogLine(in, 2000); got != in {
		t.Fatalf("短内容被改动了：\nwant %q\ngot  %q", in, got)
	}
}

// 只有末尾那个被切断的字符该丢，前面的内容一个字节都不能少。
func TestClampLogLineDropsOnlyPartialRune(t *testing.T) {
	// 每个"构"是 3 字节；max=10 → 3+3+3=9 完整，第 10 字节是下一个字的第一个字节
	in := strings.Repeat("构", 10)
	got := clampLogLine(in, 10)
	if len(got) != 9 {
		t.Fatalf("应退到 9 字节的字符边界，实际 %d 字节", len(got))
	}
	if got != strings.Repeat("构", 3) {
		t.Fatalf("内容不对：%q", got)
	}
}
