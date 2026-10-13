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
		// ---- 证据②：整行就是一个完整镜像引用（Job 没开 set -x 时的唯一线索）----
		{
			name: "真实日志：没有 set -x，只有一行完整镜像地址 → 必须认出来",
			log: "937c0a24da44: Layer already exists\n" +
				"16a9861a9747: Layer already exists\n" +
				"8dcb357d1df3: Pushed\n" +
				"ebe10f29c4bd: Pushed\n" +
				"1.2.0.3411: digest: sha256:c59a68d94eca3f358004215c6706f0b75fff5f489801fe46baab63de8b576bc5 size: 2001\n" +
				"es-repo.example.com:5000/escloud/aio-mcp-server:1.2.0.3411\n",
			wantRepo: "escloud/aio-mcp-server",
			wantRef:  "es-repo.example.com:5000/escloud/aio-mcp-server:1.2.0.3411",
			wantTag:  "1.2.0.3411",
		},
		{
			name:     "整行地址带首尾空白也认（日志里常有缩进）",
			log:      "   es-repo.example.com:5000/escloud/aio-mcp-server:1.2.0.3411   \n",
			wantRepo: "aio-mcp-server",
			wantRef:  "es-repo.example.com:5000/escloud/aio-mcp-server:1.2.0.3411",
			wantTag:  "1.2.0.3411",
		},
		{
			name: "两种证据混排：保持日志顺序，同名取最后一次",
			log: "+ docker push es-repo.example.com:5000/escloud/demowarning:1.2.0.1057\n" +
				"es-repo.example.com:5000/escloud/demowarning:1.2.0.1058\n",
			wantRepo: "demowarning",
			wantRef:  "es-repo.example.com:5000/escloud/demowarning:1.2.0.1058",
			wantTag:  "1.2.0.1058",
		},
		{
			name: "整行引用与辅助镜像混排：仍取与项目同名的那个",
			log: "es-repo.example.com:5000/library/build-base:2024.03\n" +
				"es-repo.example.com:5000/escloud/demowarning:1.2.0.1058\n",
			wantRepo: "demowarning",
			wantRef:  "es-repo.example.com:5000/escloud/demowarning:1.2.0.1058",
			wantTag:  "1.2.0.1058",
		},
		// ---- 反例：日志里"看着像镜像"但其实不是的东西，一个都不能认 ----
		{
			name:     "反例：时间戳不是镜像（没有斜杠）",
			log:      "11:37:35\n11:37:19\n",
			wantRepo: "aio-mcp-server",
			wantRef:  "",
			wantTag:  "",
		},
		{
			name:     "反例：URL 不是镜像（带 scheme）",
			log:      "http://47.100.239.15:22691/job/x/40/console\n",
			wantRepo: "aio-mcp-server",
			wantRef:  "",
			wantTag:  "",
		},
		{
			name:     "反例：绝对路径不是镜像（斜杠在行首）",
			log:      "/root/images/demowarning/ver.config\n",
			wantRepo: "demowarning",
			wantRef:  "",
			wantTag:  "",
		},
		{
			name:     "反例：push 的摘要行不是镜像（行内有空白）",
			log:      "1.2.0.3411: digest: sha256:c59a68d94eca3f358004215c6706f0b75fff5f489801fe46baab63de8b576bc5 size: 2001\n",
			wantRepo: "aio-mcp-server",
			wantRef:  "",
			wantTag:  "",
		},
		{
			name:     "反例：The push refers to a repository [...] 不带 tag，不是版本号",
			log:      "The push refers to a repository [es-repo.example.com:5000/escloud/aio-mcp-server]\n",
			wantRepo: "aio-mcp-server",
			wantRef:  "",
			wantTag:  "",
		},
		{
			name:     "反例：单独一行完整地址但不带 tag（推的是 latest）",
			log:      "es-repo.example.com:5000/escloud/aio-mcp-server\n",
			wantRepo: "aio-mcp-server",
			wantRef:  "",
			wantTag:  "",
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
