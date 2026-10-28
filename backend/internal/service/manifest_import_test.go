package service

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/2777484478/yunnian/internal/auth"
	"github.com/2777484478/yunnian/internal/model"
)

// newManifestTestSvc 建一个只带 DB 的 CIService + 一个项目/环境。
//
// 这几个用例只走库，不碰集群 —— ImportManifests 的职责就是"把文件拆开、校验、落库"，
// 写集群是下一期的事。deps 里给一个 nil 的 Cluster 反而能把这条边界钉住：
// 万一哪天有人在导入路径里加了集群调用，测试会直接 panic 而不是"悄悄地通过"。
func newManifestTestSvc(t *testing.T) (*CIService, string) {
	t.Helper()
	db := newTestDB(t)
	s := &CIService{base: &base{deps: Deps{DB: db}}}
	if err := db.Create(&model.Project{ProjectKey: "demo", Name: "演示项目", Type: "backend"}).Error; err != nil {
		t.Fatalf("造项目失败：%v", err)
	}
	if err := db.Create(&model.Environment{EnvKey: "test-env", Name: "测试环境"}).Error; err != nil {
		t.Fatalf("造环境失败：%v", err)
	}
	return s, "demo"
}

func mfCaller() Caller { return Caller{User: &auth.CurrentUser{ID: 1, Username: "u1"}} }

// mustParseID DTO 里的 id 是字符串（避免 JS 精度问题），测试里要转回 uint64。
func mustParseID(t *testing.T, s string) uint64 {
	t.Helper()
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatalf("id %q 解析失败：%v", s, err)
	}
	return v
}

// TestSplitManifests 钉住「一份文件怎么拆成多个资源」的边界。
//
// 这段逻辑直接影响"用户把 kubectl apply -f 用的文件拖进来会得到几条记录"，
// 而且它的错法都很安静：多切一刀 = 多出一条残缺记录，
// 少切一刀 = 整段报"缺 metadata.name"，用户完全看不出是自己文件里的分隔符写法问题。
func TestSplitManifests(t *testing.T) {
	const deploy = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: aioa
`
	const svc = `apiVersion: v1
kind: Service
metadata:
  name: aio-svc
`

	cases := []struct {
		name      string
		in        string
		wantKinds []string // 依次期望的 kind（比只看条数更能发现"切错位置"）
	}{
		{name: "单文档", in: deploy, wantKinds: []string{"Deployment"}},
		{name: "两个文档用 --- 分隔", in: deploy + "---\n" + svc, wantKinds: []string{"Deployment", "Service"}},
		// 手写文件常见在开头放一个 ---，不应产生空段
		{name: "开头就有 ---", in: "---\n" + deploy + "---\n" + svc, wantKinds: []string{"Deployment", "Service"}},
		// 结尾的 --- 同样不该产生空段
		{name: "结尾有 ---", in: deploy + "---\n" + svc + "---\n", wantKinds: []string{"Deployment", "Service"}},
		// `--- # 注释` 是合法分隔符
		{name: "分隔符带注释", in: deploy + "--- # 下面是 Service\n" + svc, wantKinds: []string{"Deployment", "Service"}},
		// 内容里出现 --- 字样（比如注解值）不该被当分隔符
		{
			name:      "文档内容里出现 --- 字样不切",
			in:        strings.Replace(deploy, "  name: aioa", "  name: aioa\n  annotations:\n    note: \"a---b\"", 1),
			wantKinds: []string{"Deployment"},
		},
		{name: "空内容", in: "", wantKinds: []string{}},
		{name: "只有注释", in: "# 什么都没写\n# 第二行\n", wantKinds: []string{}},
		{
			// kubectl get -o yaml 的导出形式：一份 List 要展开
			name: "kind List 展开成多个",
			in: `apiVersion: v1
kind: List
items:
  - apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: d1
  - apiVersion: v1
    kind: Service
    metadata:
      name: s1
`,
			wantKinds: []string{"Deployment", "Service"},
		},
		{
			// List 里混着普通文档：List 展开，普通文档原样保留
			name: "List 与普通文档混排",
			in: `apiVersion: v1
kind: List
items:
  - apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: d1
---
` + svc,
			wantKinds: []string{"Deployment", "Service"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitManifests(c.in)
			if len(got) != len(c.wantKinds) {
				t.Fatalf("拆出 %d 段，期望 %d 段。结果：%q", len(got), len(c.wantKinds), got)
			}
			for i, wantKind := range c.wantKinds {
				pm, err := parseManifest(got[i])
				if err != nil {
					t.Fatalf("第 %d 段解析失败：%v\n内容：%s", i+1, err, got[i])
				}
				if pm.Kind != wantKind {
					t.Errorf("第 %d 段 kind=%q，期望 %q", i+1, pm.Kind, wantKind)
				}
			}
		})
	}
}

// TestSplitManifestsKeepsOriginalText 平台存的是"原文"，不是"重新编码后的等价物"。
//
// 用 yaml.Decoder 解码再 Marshal 回去，注释、字段顺序、引号风格全都会变 ——
// 用户下次打开看到的就不是他写的那份东西了。这条单独钉一次。
func TestSplitManifestsKeepsOriginalText(t *testing.T) {
	const src = `# 这是用户的注释，必须留着
apiVersion: apps/v1
kind: Deployment
metadata:
  name: keep-me
spec:
  replicas: 2   # 行尾注释也要留
  template:
    spec:
      containers:
        - name: c
          image: "repo/x:1.0"
`
	got := splitManifests(src)
	if len(got) != 1 {
		t.Fatalf("期望 1 段，得到 %d 段", len(got))
	}
	for _, want := range []string{"# 这是用户的注释", "# 行尾注释也要留", `image: "repo/x:1.0"`} {
		if !strings.Contains(got[0], want) {
			t.Errorf("拆出来的文本丢了 %q\n实际：\n%s", want, got[0])
		}
	}
}

// TestImportManifests 钉住导入的行为：逐条独立、已存在默认跳过、可选覆盖。
func TestImportManifests(t *testing.T) {
	const deploy = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: imp-a
spec:
  replicas: 1
`
	const svc = `apiVersion: v1
kind: Service
metadata:
  name: imp-svc
spec:
  ports:
    - port: 80
`
	ctx := context.Background()

	t.Run("两个文档都新建成功", func(t *testing.T) {
		s, key := newManifestTestSvc(t)
		res, err := s.ImportManifests(ctx, key, ManifestImportInput{
			EnvKey:  "test-env",
			Content: deploy + "---\n" + svc,
		}, mfCaller())
		if err != nil {
			t.Fatalf("导入报错：%v", err)
		}
		if res.Total != 2 || res.Created != 2 || res.Failed != 0 || res.Skipped != 0 {
			t.Fatalf("结果不对：%+v", res)
		}
		if len(res.Items) != 2 {
			t.Fatalf("明细应有 2 条，得到 %d", len(res.Items))
		}
		if res.Items[0].Kind != "Deployment" || res.Items[0].Action != "created" {
			t.Errorf("第 1 条：%+v", res.Items[0])
		}
		if res.Items[1].Kind != "Service" || res.Items[1].Action != "created" {
			t.Errorf("第 2 条：%+v", res.Items[1])
		}
	})

	t.Run("重复导入默认跳过，不覆盖用户编辑", func(t *testing.T) {
		s, key := newManifestTestSvc(t)
		if _, err := s.ImportManifests(ctx, key, ManifestImportInput{
			EnvKey: "test-env", Content: deploy,
		}, mfCaller()); err != nil {
			t.Fatalf("第一次导入失败：%v", err)
		}
		list, err := s.ListProjectManifests(ctx, key, ManifestQuery{EnvKey: "test-env"})
		if err != nil || len(list) != 1 {
			t.Fatalf("列表异常：%v / %d 条", err, len(list))
		}
		id := mustParseID(t, list[0].ID)
		// 模拟用户在界面上把副本数改成 9
		edited := strings.Replace(deploy, "replicas: 1", "replicas: 9", 1)
		if _, err := s.UpdateProjectManifest(ctx, key, id, ManifestInput{
			EnvKey: "test-env", Content: edited,
		}, mfCaller()); err != nil {
			t.Fatalf("编辑失败：%v", err)
		}

		// 再导入一次原文件 → 应跳过，内容仍是用户改过的 9
		res, err := s.ImportManifests(ctx, key, ManifestImportInput{
			EnvKey: "test-env", Content: deploy,
		}, mfCaller())
		if err != nil {
			t.Fatalf("第二次导入报错：%v", err)
		}
		if res.Skipped != 1 || res.Updated != 0 || res.Created != 0 {
			t.Fatalf("应跳过 1 条，实际 %+v", res)
		}
		got, err := s.GetProjectManifest(ctx, key, id)
		if err != nil {
			t.Fatalf("取详情失败：%v", err)
		}
		if !strings.Contains(got.Content, "replicas: 9") {
			t.Errorf("跳过时不该覆盖用户编辑，实际内容：\n%s", got.Content)
		}
	})

	t.Run("勾选覆盖时写入文件内容", func(t *testing.T) {
		s, key := newManifestTestSvc(t)
		if _, err := s.ImportManifests(ctx, key, ManifestImportInput{
			EnvKey: "test-env", Content: deploy,
		}, mfCaller()); err != nil {
			t.Fatalf("第一次导入失败：%v", err)
		}
		newer := strings.Replace(deploy, "replicas: 1", "replicas: 5", 1)
		res, err := s.ImportManifests(ctx, key, ManifestImportInput{
			EnvKey: "test-env", Content: newer, OnConflict: "overwrite",
		}, mfCaller())
		if err != nil {
			t.Fatalf("覆盖导入报错：%v", err)
		}
		if res.Updated != 1 {
			t.Fatalf("应更新 1 条，实际 %+v", res)
		}
		list, _ := s.ListProjectManifests(ctx, key, ManifestQuery{EnvKey: "test-env"})
		if len(list) != 1 || !strings.Contains(list[0].Content, "replicas: 5") {
			t.Errorf("覆盖后内容不对：%+v", list)
		}
	})

	t.Run("一条坏的不影响其他条", func(t *testing.T) {
		s, key := newManifestTestSvc(t)
		bad := "apiVersion: v1\nkind: Service\nspec:\n  ports: []\n" // 缺 metadata.name
		res, err := s.ImportManifests(ctx, key, ManifestImportInput{
			EnvKey: "test-env", Content: bad + "---\n" + deploy,
		}, mfCaller())
		if err != nil {
			t.Fatalf("导入报错（不该整体失败）：%v", err)
		}
		if res.Failed != 1 || res.Created != 1 {
			t.Fatalf("期望 1 失败 1 成功，实际 %+v", res)
		}
		if res.Items[0].Action != "failed" {
			t.Errorf("第 1 条应失败：%+v", res.Items[0])
		}
		if !strings.Contains(res.Items[0].Reason, "metadata.name") {
			t.Errorf("失败原因应说清缺什么，实际：%q", res.Items[0].Reason)
		}
		// 失败项的原因**不能带内部错误码**（用户看不懂 code=40001）
		if strings.Contains(res.Items[0].Reason, "code=") {
			t.Errorf("原因里漏出了内部错误码：%q", res.Items[0].Reason)
		}
	})

	t.Run("空文件被拒", func(t *testing.T) {
		s, key := newManifestTestSvc(t)
		if _, err := s.ImportManifests(ctx, key, ManifestImportInput{
			EnvKey: "test-env", Content: "# 只有注释\n",
		}, mfCaller()); err == nil {
			t.Fatal("空文件应该报错")
		}
	})

	t.Run("环境不存在被拒", func(t *testing.T) {
		s, key := newManifestTestSvc(t)
		if _, err := s.ImportManifests(ctx, key, ManifestImportInput{
			EnvKey: "no-such-env", Content: deploy,
		}, mfCaller()); err == nil {
			t.Fatal("环境不存在应该报错")
		}
	})
}
