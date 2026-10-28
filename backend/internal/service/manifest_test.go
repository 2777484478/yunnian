package service

import (
	"strings"
	"testing"
)

// TestParseManifest 钉住「保存时就拒」的那几条。
//
// 为什么这些值得逐条测：它们的失败都发生在**很久以后**——存进去的时候一切正常，
// 到后期 apply 才炸，而那时报错只会说"资源已存在"或"字段非法"，
// 根本看不出源头是当初存了一份什么。校验是这条链上唯一的止损点。
func TestParseManifest(t *testing.T) {
	const deployOK = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: aioitms
  namespace: default
spec:
  replicas: 2
`

	const serviceOK = `apiVersion: v1
kind: Service
metadata:
  name: aioitms
spec:
  ports:
    - port: 8080
`

	cases := []struct {
		name       string
		in         string
		wantKind   string
		wantName   string
		wantAPI    string
		wantErrHas string // 空 = 期望成功
	}{
		{name: "正常 Deployment", in: deployOK, wantKind: "Deployment", wantName: "aioitms", wantAPI: "apps/v1"},
		{name: "正常 Service", in: serviceOK, wantKind: "Service", wantName: "aioitms", wantAPI: "v1"},
		{
			name: "带注释和文档头",
			in:   "---\n# 这是注释\n" + deployOK,
			wantKind: "Deployment", wantName: "aioitms", wantAPI: "apps/v1",
		},
		{
			name:       "多个文档（--- 分隔）",
			in:         deployOK + "---\n" + serviceOK,
			wantErrHas: "多个 YAML 文档",
		},
		{
			name: "第二个文档为空也算多段",
			in:   deployOK + "---\n",
			// 空文档解出空 map，同样是"又出现一个文档" —— 用户的意图是多段，
			// 只是最后一段写空了，静默接受会让他以为都存进去了。
			wantErrHas: "多个 YAML 文档",
		},
		{
			name:       "kind: List（kubectl -o yaml 常见）",
			in:         "apiVersion: v1\nkind: List\nitems:\n  - apiVersion: v1\n    kind: Service\n    metadata:\n      name: a\n",
			wantErrHas: "kind: List",
		},
		{
			name:       "缺 apiVersion",
			in:         "kind: Deployment\nmetadata:\n  name: x\n",
			wantErrHas: "apiVersion",
		},
		{
			name:       "缺 kind",
			in:         "apiVersion: v1\nmetadata:\n  name: x\n",
			wantErrHas: "kind",
		},
		{
			name:       "缺 metadata.name",
			in:         "apiVersion: apps/v1\nkind: Deployment\nspec:\n  replicas: 1\n",
			wantErrHas: "metadata.name",
		},
		{
			name:       "metadata 不是对象",
			in:         "apiVersion: v1\nkind: Service\nmetadata: oops\n",
			wantErrHas: "metadata.name",
		},
		{
			name:       "kind 写成数字（YAML 合法但无意义）",
			in:         "apiVersion: v1\nkind: 123\nmetadata:\n  name: x\n",
			wantErrHas: "kind",
		},
		{
			name:       "空内容",
			in:         "   \n\n",
			wantErrHas: "不能为空",
		},
		{
			name:       "只有注释",
			in:         "# 什么都没写\n",
			wantErrHas: "内容为空",
		},
		{
			name:       "非法 YAML（缩进错）",
			in:         "apiVersion: v1\nkind: Service\n  metadata:\n name: x\n",
			wantErrHas: "语法错误",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseManifest(c.in)
			if c.wantErrHas != "" {
				if err == nil {
					t.Fatalf("期望报错（含 %q），实际通过了，解析结果 = %+v", c.wantErrHas, got)
				}
				if !strings.Contains(err.Error(), c.wantErrHas) {
					t.Fatalf("报错信息不含 %q，实际: %v", c.wantErrHas, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("期望成功，实际报错: %v", err)
			}
			if got.Kind != c.wantKind || got.Name != c.wantName || got.APIVersion != c.wantAPI {
				t.Fatalf("解析结果不符：期望 kind=%s name=%s apiVersion=%s，实际 %+v",
					c.wantKind, c.wantName, c.wantAPI, got)
			}
		})
	}
}

// TestParseManifest_KeepsContentIntact 确认正文原样保留的语义没有被"规范化"破坏。
//
// 这一条不是废话：如果哪天有人在 parseManifest 里顺手做了 yaml.Marshal 回写
// （"格式化一下更好看"），用户手写的注释、字段顺序、引号风格会全部被洗掉，
// 而这些往往是他们刻意保留的（比如注释里写着为什么这么配）。
func TestParseManifest_KeepsContentIntact(t *testing.T) {
	const in = `# 为什么 2 副本：单副本滚动更新时会有约 10 秒不可用
apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo
spec:
  replicas: 2
`
	got, err := parseManifest(in)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.Name != "demo" {
		t.Fatalf("name 解析错: %q", got.Name)
	}
	// parseManifest 只返回身份，不碰正文；正文由调用方原样写库。
}
