package service

import (
	"context"
	"strings"
	"testing"
)

// TestValidateManifestStructure 钉住「存之前就把写错的 YAML 挡住」。
//
// 为什么这些用例值得逐条写：它们的失败都发生在**很久以后** ——
// 保存成功、界面一切正常，到 apply 那一刻 k8s 才拒，而那时平台只会说
// "应用失败"，用户得回头翻几十行 YAML 找哪一行有问题。
// 严格解码是这条链上唯一能在"存之前"说话的环节。
func TestValidateManifestStructure(t *testing.T) {
	const okDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: ok
spec:
  replicas: 2
  selector:
    matchLabels:
      app: ok
  template:
    metadata:
      labels:
        app: ok
    spec:
      containers:
        - name: c
          image: nginx:1.0
`

	cases := []struct {
		name string
		in   string
		// wantErrHas 空 = 期望通过（可能带 warn）
		wantErrHas string
		wantWarn   bool
	}{
		{
			name: "合法 Deployment 通过",
			in:   okDeployment,
		},
		{
			// 字段名拼错是最高频的错误，而且严格解码能精确到数组下标
			name: "字段名拼错 port",
			in: strings.Replace(okDeployment,
				"          image: nginx:1.0", "          image: nginx:1.0\n          port: 80", 1),
			wantErrHas: "字段名不对",
		},
		{
			name: "replicas 写成字符串",
			in: strings.Replace(okDeployment,
				"  replicas: 2", `  replicas: "两个"`, 1),
			wantErrHas: "类型不对",
		},
		{
			// k8s 硬性要求，没有 selector 一定 apply 失败
			name: "Deployment 缺 selector",
			in: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: no-selector
spec:
  template:
    spec:
      containers:
        - name: c
          image: nginx:1.0
`,
			wantErrHas: "缺少 spec.selector",
		},
		{
			name: "容器没有 image",
			in: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: no-image
spec:
  selector:
    matchLabels:
      app: x
  template:
    spec:
      containers:
        - name: c
`,
			wantErrHas: "没有 image",
		},
		{
			name: "没有任何容器",
			in: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: no-container
spec:
  selector:
    matchLabels:
      app: x
  template:
    metadata:
      labels:
        app: x
    spec: {}
`,
			wantErrHas: "至少要有一个容器",
		},
		{
			// CRD 是平台的合法内容，平台只是不认识它的字段 —— 必须放行 + 警告
			name: "自定义资源（CRD）放行并给出警告",
			in: `apiVersion: example.com/v1
kind: MyCustomThing
metadata:
  name: crd1
spec:
  foo: bar
`,
			wantWarn: true,
		},
		{
			name: "Service 通过",
			in: `apiVersion: v1
kind: Service
metadata:
  name: s1
spec:
  selector:
    app: s1
  ports:
    - port: 80
      targetPort: 8080
`,
		},
		{
			name: "ConfigMap data 写成数组",
			in: `apiVersion: v1
kind: ConfigMap
metadata:
  name: cm1
data:
  key: [1,2,3]
`,
			wantErrHas: "类型不对",
		},
		{
			// CronJob 的容器路径多两层，漏了会误报"没有容器"
			name: "CronJob 容器正常",
			in: `apiVersion: batch/v1
kind: CronJob
metadata:
  name: cj1
spec:
  schedule: "*/5 * * * *"
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: c
              image: busybox:1.36
`,
		},
		{
			name: "CronJob 容器缺 image",
			in: `apiVersion: batch/v1
kind: CronJob
metadata:
  name: cj2
spec:
  schedule: "*/5 * * * *"
  jobTemplate:
    spec:
      template:
        spec:
          containers:
            - name: c
`,
			wantErrHas: "没有 image",
		},
		{
			name: "YAML 缩进错误报行号",
			in: `apiVersion: v1
kind: ConfigMap
metadata:
   name: bad
  namespace: default
`,
			wantErrHas: "行",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			warn, err := validateManifestStructure(c.in)
			switch {
			case c.wantErrHas != "":
				if err == nil {
					t.Fatalf("应该报错（含 %q），却通过了；warn=%q", c.wantErrHas, warn)
				}
				msg := userMessage(err)
				if !strings.Contains(msg, c.wantErrHas) {
					t.Errorf("错误信息里没有 %q，实际：%q", c.wantErrHas, msg)
				}
				// 报错必须是人话：不能把 k8s 的英文黑话直接端给用户
				for _, jargon := range []string{"strict decoding error", "unknown field", "cannot unmarshal"} {
					if strings.Contains(msg, jargon) {
						t.Errorf("错误信息里漏出了英文黑话 %q：%q", jargon, msg)
					}
				}
			default:
				if err != nil {
					t.Fatalf("不该报错：%v（%s）", err, userMessage(err))
				}
				if c.wantWarn && warn == "" {
					t.Error("期望有警告（平台校验不到这类资源），实际为空")
				}
				if !c.wantWarn && warn != "" {
					t.Errorf("不该有警告：%q", warn)
				}
			}
		})
	}
}

// TestCreateManifestRejectsBadYAML 确认单条保存路径真的接上了结构校验。
//
// 光测 validateManifestStructure 不够 —— 那条函数可能压根没被调用，
// 测试照样绿。这里走真实的 CreateProjectManifest。
func TestCreateManifestRejectsBadYAML(t *testing.T) {
	s, key := newManifestTestSvc(t)
	bad := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: typo
spec:
  replicas: "两个"
  selector:
    matchLabels:
      app: typo
  template:
    spec:
      containers:
        - name: c
          image: nginx:1.0
`
	if _, err := s.CreateProjectManifest(context.Background(), key, ManifestInput{
		EnvKey: "test-env", Content: bad,
	}, mfCaller()); err == nil {
		t.Fatal("字段类型写错的 YAML 被放行了")
	}

	// 合法内容必须能存
	good := strings.Replace(bad, `replicas: "两个"`, "replicas: 2", 1)
	if _, err := s.CreateProjectManifest(context.Background(), key, ManifestInput{
		EnvKey: "test-env", Content: good,
	}, mfCaller()); err != nil {
		t.Fatalf("合法内容被拒了：%v", err)
	}
}

// TestImportManifestsRejectsBadStructure 导入路径同样要校验（逐条报告）。
func TestImportManifestsRejectsBadStructure(t *testing.T) {
	s, key := newManifestTestSvc(t)
	bad := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: typo
spec:
  selector:
    matchLabels:
      app: typo
  template:
    spec:
      containers:
        - name: c
          image: nginx:1.0
          port: 80
`
	good := `apiVersion: v1
kind: ConfigMap
metadata:
  name: ok-cm
data:
  a: b
`
	res, err := s.ImportManifests(context.Background(), key, ManifestImportInput{
		EnvKey: "test-env", Content: bad + "---\n" + good,
	}, mfCaller())
	if err != nil {
		t.Fatalf("导入不该整体失败：%v", err)
	}
	if res.Failed != 1 || res.Created != 1 {
		t.Fatalf("期望 1 失败 1 成功，实际 %+v", res)
	}
	if !strings.Contains(res.Items[0].Reason, "字段名不对") {
		t.Errorf("失败原因应指出字段名问题，实际：%q", res.Items[0].Reason)
	}
	if !strings.Contains(res.Items[0].Reason, "port") {
		t.Errorf("失败原因应指出具体是哪个字段，实际：%q", res.Items[0].Reason)
	}
}
