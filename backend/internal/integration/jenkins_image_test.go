package integration

import "testing"

// 这些用例直接来自目标 Jenkins 的真实日志（用户提供的片段），
// 以及几个"看起来能过、其实不该认"的反例。
func TestParsePushedImage(t *testing.T) {
	cases := []struct {
		name     string
		log      string
		wantRepo string
		wantRef  string
		wantTag  string
	}{
		{
			name: "真实日志片段：set -x 回显的 docker push",
			log: "++ cat /root/images/demowarning/ver.config\n" +
				"+ docker push es-repo.example.com:5000/escloud/demowarning:1.2.0.1058\n" +
				"The push refers to a repository [es-repo.example.com:5000/escloud/demowarning]\n",
			wantRepo: "demowarning",
			wantRef:  "es-repo.example.com:5000/escloud/demowarning:1.2.0.1058",
			wantTag:  "1.2.0.1058",
		},
		{
			name:     "没有 push 行 → 空（宁可留空也不要瞎猜）",
			log:      "+ mvn -q package\n+ cp target/app.jar /root/images/x/\nBUILD SUCCESS\n",
			wantRepo: "demowarning",
			wantRef:  "",
			wantTag:  "",
		},
		{
			name:     "变量没展开 → 不算（那是脚本原文，不是结果）",
			log:      "+ docker push $REGISTRY/$IMAGE:$TAG\n",
			wantRepo: "demowarning",
			wantRef:  "",
			wantTag:  "",
		},
		{
			name:     "不带 tag 的 push（推的是 latest）→ 不算版本号",
			log:      "+ docker push es-repo.example.com:5000/escloud/demowarning\n",
			wantRepo: "demowarning",
			wantRef:  "",
			wantTag:  "",
		},
		{
			name:     "只有 registry 端口、不带 tag → 不算",
			log:      "+ docker push es-repo.example.com:5000/escloud/demowarning\n",
			wantRepo: "demowarning",
			wantRef:  "",
			wantTag:  "",
		},
		{
			name:     "按 digest 推送 → 没有 tag",
			log:      "+ docker push es-repo.example.com:5000/escloud/demowarning@sha256:abc123\n",
			wantRepo: "demowarning",
			wantRef:  "",
			wantTag:  "",
		},
		{
			name: "中途推了别的镜像，产物在后面 → 取同名那个",
			log: "+ docker push es-repo.example.com:5000/library/build-base:2024.03\n" +
				"+ docker push es-repo.example.com:5000/escloud/demowarning:1.2.0.1058\n",
			wantRepo: "demowarning",
			wantRef:  "es-repo.example.com:5000/escloud/demowarning:1.2.0.1058",
			wantTag:  "1.2.0.1058",
		},
		{
			name: "产物先推、辅助镜像后推 → 仍取同名那个，不被后推的带偏",
			log: "+ docker push es-repo.example.com:5000/escloud/demowarning:1.2.0.1058\n" +
				"+ docker push es-repo.example.com:5000/library/cache:latest\n",
			wantRepo: "demowarning",
			wantRef:  "es-repo.example.com:5000/escloud/demowarning:1.2.0.1058",
			wantTag:  "1.2.0.1058",
		},
		{
			name: "同名推了多次 → 取最后一次（最终版本）",
			log: "+ docker push es-repo.example.com:5000/escloud/demowarning:1.2.0.1057-rc\n" +
				"+ docker push es-repo.example.com:5000/escloud/demowarning:1.2.0.1058\n",
			wantRepo: "escloud/demowarning",
			wantRef:  "es-repo.example.com:5000/escloud/demowarning:1.2.0.1058",
			wantTag:  "1.2.0.1058",
		},
		{
			name:     "没有同名时退回最后一个候选（不因为项目配错了就什么都不给）",
			log:      "+ docker push harbor.internal:8083/other/x:9.9.9\n",
			wantRepo: "demowarning",
			wantRef:  "harbor.internal:8083/other/x:9.9.9",
			wantTag:  "9.9.9",
		},
		{
			name:     "push 带选项",
			log:      "+ docker push --quiet es-repo.example.com:5000/escloud/demowarning:1.2.0.1058\n",
			wantRepo: "demowarning",
			wantRef:  "es-repo.example.com:5000/escloud/demowarning:1.2.0.1058",
			wantTag:  "1.2.0.1058",
		},
		{
			name:     "单段仓库名 + tag（本地 registry 常见写法）",
			log:      "+ docker push demowarning:1.2.0.1058\n",
			wantRepo: "demowarning",
			wantRef:  "demowarning:1.2.0.1058",
			wantTag:  "1.2.0.1058",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ref, tag := ParsePushedImage(c.log, c.wantRepo)
			if ref != c.wantRef || tag != c.wantTag {
				t.Fatalf("ParsePushedImage() = (%q, %q), 期望 (%q, %q)", ref, tag, c.wantRef, c.wantTag)
			}
		})
	}
}
