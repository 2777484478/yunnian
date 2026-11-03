package service

import (
	"context"
	"strings"
	"testing"

	"github.com/2777484478/yunnian/internal/model"
)

// 一份贴近真实的 Deployment：带人肉注释、带 sidecar、带行尾注释。
// 用户的 YAML 就长这样 —— 这份测试要证明"改完 tag，别的一个字符都不许动"。
const sampleDeployYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: aioai
  labels:
    app: aioai
spec:
  replicas: 1
  template:
    spec:
      containers:
        - name: master
          image: es-repo.digiwincloud.com.cn:5000/escloud/aioai:1.2.0.3390
          env:
            - name: OPENAI_API_KEY
              value: "sk-5dfb1e3b63bae42e"
#            - name: JAVA_OPTS
#              value: "-Xms256m -Xmx512m"
            - name: AREA
              value: "test"
        - name: log-sidecar
          image: es-repo.digiwincloud.com.cn:5000/infra/fluent-bit:2.1.8
`

// TestReplaceImageTag 钉住「只换 tag」这件事。
func TestReplaceImageTag(t *testing.T) {
	const repo = "es-repo.digiwincloud.com.cn:5000/escloud/aioai"

	t.Run("命中 repo 的 image 行换成新 tag，其余原样", func(t *testing.T) {
		got, changed := replaceImageTag(sampleDeployYAML, repo, "1.2.0.3396")
		if !changed {
			t.Fatal("应该被改（旧 tag 1.2.0.3390）")
		}
		if !strings.Contains(got, "escloud/aioai:1.2.0.3396") {
			t.Errorf("没换到新 tag：\n%s", got)
		}
		// sidecar 是另一个仓库，一个字符都不能动
		if !strings.Contains(got, "infra/fluent-bit:2.1.8") {
			t.Errorf("sidecar 的镜像被误改了：\n%s", got)
		}
		// 注释行必须原样留着（这是"文本替换而非重新序列化"的全部理由）
		for _, keep := range []string{
			"#            - name: JAVA_OPTS",
			`#              value: "-Xms256m -Xmx512m"`,
			`value: "sk-5dfb1e3b63bae42e"`,
		} {
			if !strings.Contains(got, keep) {
				t.Errorf("注释 / 其他字段被洗掉了：%q\n%s", keep, got)
			}
		}
		// 行数不能变（一变就说明重排了）
		if a, b := len(strings.Split(sampleDeployYAML, "\n")), len(strings.Split(got, "\n")); a != b {
			t.Errorf("行数从 %d 变成 %d，说明结构被动过", a, b)
		}
	})

	t.Run("已经是目标 tag → 不算改动（避免每次都写库刷 updated_at）", func(t *testing.T) {
		_, changed := replaceImageTag(sampleDeployYAML, repo, "1.2.0.3390")
		if changed {
			t.Error("tag 相同不该判为改动")
		}
	})

	t.Run("repo 对不上 → 一个字都不改", func(t *testing.T) {
		got, changed := replaceImageTag(sampleDeployYAML, "es-repo.digiwincloud.com.cn:5000/escloud/other", "9.9.9")
		if changed || got != sampleDeployYAML {
			t.Error("repo 不匹配时不该改动任何内容")
		}
	})

	t.Run("注释掉的行不会被复活", func(t *testing.T) {
		y := "      containers:\n#        image: es-repo.digiwincloud.com.cn:5000/escloud/aioai:0.0.1\n"
		if _, changed := replaceImageTag(y, repo, "1.2.0.3396"); changed {
			t.Error("被注释掉的 image 行不该被当成有效字段")
		}
	})

	t.Run("行尾注释与引号都保留", func(t *testing.T) {
		y := `          image: "es-repo.digiwincloud.com.cn:5000/escloud/aioai:1.0.0"  # 别乱改`
		got, changed := replaceImageTag(y, repo, "1.2.0.3396")
		if !changed {
			t.Fatal("应该被改")
		}
		if !strings.Contains(got, `"es-repo.digiwincloud.com.cn:5000/escloud/aioai:1.2.0.3396"`) {
			t.Errorf("引号丢了：%q", got)
		}
		if !strings.Contains(got, "# 别乱改") {
			t.Errorf("行尾注释丢了：%q", got)
		}
	})

	t.Run("imagePullPolicy 不会被当成 image", func(t *testing.T) {
		y := "          imagePullPolicy: IfNotPresent\n"
		if _, changed := replaceImageTag(y, repo, "1.2.0.3396"); changed {
			t.Error("imagePullPolicy 不该被匹配")
		}
	})
}

// TestSplitImageRepoTag registry 自带端口，不能取第一个冒号。
func TestSplitImageRepoTag(t *testing.T) {
	cases := []struct{ in, repo, tag string }{
		{"es-repo.digiwincloud.com.cn:5000/escloud/aioai:1.2.0.3396",
			"es-repo.digiwincloud.com.cn:5000/escloud/aioai", "1.2.0.3396"},
		// 没有 tag：只有 registry 的端口冒号
		{"es-repo.digiwincloud.com.cn:5000/escloud/aioai",
			"es-repo.digiwincloud.com.cn:5000/escloud/aioai", ""},
		{"nginx:1.25", "nginx", "1.25"},
		{"nginx", "nginx", ""},
		// digest 形式：不碰
		{"es-repo.x.com:5000/a/b@sha256:abcd", "", ""},
	}
	for _, c := range cases {
		repo, tag := splitImageRepoTag(c.in)
		if repo != c.repo || tag != c.tag {
			t.Errorf("%q → (%q,%q)，期望 (%q,%q)", c.in, repo, tag, c.repo, c.tag)
		}
	}
}

// TestSyncManifestImageTag 端到端（DB 层）：发布成功后，该环境那份 YAML 的 tag 被改，
// 同项目的其他环境、以及 Service 那份都不动。
func TestSyncManifestImageTag(t *testing.T) {
	db := newTestDB(t)
	s := &CDService{base: &base{deps: Deps{DB: db}}}
	ctx := context.Background()

	proj := model.Project{ProjectKey: "aioai", Name: "aioai"}
	if err := db.Create(&proj).Error; err != nil {
		t.Fatalf("造项目失败：%v", err)
	}

	deployContent := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: aioai
spec:
  template:
    spec:
      containers:
        - name: master
          image: es-repo.digiwincloud.com.cn:5000/escloud/aioai:1.2.0.3391
# 手工注释：这行别删，起 JAVA_OPTS 的时候要用
        - name: sidecar
          image: es-repo.digiwincloud.com.cn:5000/infra/fluent-bit:2.1.8
`
	serviceContent := "apiVersion: v1\nkind: Service\nmetadata:\n  name: aioai\nspec:\n  ports:\n    - port: 80\n"
	otherEnvContent := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: aioai
spec:
  template:
    spec:
      containers:
        - name: master
          image: es-repo.digiwincloud.com.cn:5000/escloud/aioai:1.2.0.3388
`
	mk := func(env, kind, name, content string) *model.ProjectManifest {
		m := &model.ProjectManifest{ProjectID: proj.ID, EnvKey: env, Kind: kind, Name: name, Content: content}
		if err := db.Create(m).Error; err != nil {
			t.Fatalf("造资源定义失败：%v", err)
		}
		return m
	}
	target := mk("test-152", "Deployment", "aioai", deployContent)
	svc := mk("test-152", "Service", "aioai", serviceContent)
	other := mk("pre-aliyun", "Deployment", "aioai", otherEnvContent)

	res := s.syncManifestImageTag(ctx, "test-152", "aioai",
		"es-repo.digiwincloud.com.cn:5000/escloud/aioai:1.2.0.3396")

	if res.Updated != 1 {
		t.Errorf("应只改 1 份文档，实际 %d（失败 %d）", res.Updated, res.Failed)
	}
	if len(res.From) != 1 || res.From[0] != "1.2.0.3391" {
		t.Errorf("旧 tag 应记为 1.2.0.3391，实际 %v", res.From)
	}

	var got model.ProjectManifest
	db.First(&got, target.ID)
	if !strings.Contains(got.Content, "escloud/aioai:1.2.0.3396") {
		t.Errorf("目标环境那份没改成新 tag：\n%s", got.Content)
	}
	if strings.Contains(got.Content, "1.2.0.3391") {
		t.Error("旧 tag 还留着")
	}
	if !strings.Contains(got.Content, "escloud/aioai:1.2.0.3396") ||
		!strings.Contains(got.Content, "# 手工注释：这行别删") {
		t.Errorf("注释被洗掉了：\n%s", got.Content)
	}
	if !strings.Contains(got.Content, "infra/fluent-bit:2.1.8") {
		t.Errorf("sidecar 镜像被误改：\n%s", got.Content)
	}

	// Service 没有 image 行，必须一字未动
	var svcRow model.ProjectManifest
	db.First(&svcRow, svc.ID)
	if svcRow.Content != serviceContent {
		t.Error("Service 那份不该被改动")
	}

	// 另一个环境的那份必须原样保留（只同步本次发布的环境）
	var otherRow model.ProjectManifest
	db.First(&otherRow, other.ID)
	if otherRow.Content != otherEnvContent {
		t.Error("别的环境的资源定义不该被改动")
	}

	t.Run("重复同步同一版本 → 不再判为改动（幂等）", func(t *testing.T) {
		res2 := s.syncManifestImageTag(ctx, "test-152", "aioai",
			"es-repo.digiwincloud.com.cn:5000/escloud/aioai:1.2.0.3396")
		if res2.Updated != 0 {
			t.Errorf("已经是目标版本，不该再写库，实际改了 %d 份", res2.Updated)
		}
	})

	t.Run("项目不存在 → 安静跳过，不报错", func(t *testing.T) {
		res3 := s.syncManifestImageTag(ctx, "test-152", "nope",
			"es-repo.digiwincloud.com.cn:5000/escloud/nope:1.0.0")
		if res3.Updated != 0 || res3.Failed != 0 {
			t.Error("项目不存在时应安静返回")
		}
	})
}
