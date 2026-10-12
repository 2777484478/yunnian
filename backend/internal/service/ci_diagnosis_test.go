package service

import (
	"strings"
	"testing"

	"github.com/yunnian/yunnian/internal/model"
)

// 归因链路里唯一"模型说了算"的一段就是解析：模型输出格式漂移是常态，
// 而这里一旦把畸形输出当成结论下发，用户看到的就是一条像模型说的、其实是残句的原因。
// 所以这一层按"最不听话的模型"来测。

func TestParseModelDiagnosis(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantReason string
		wantConf   float64
		wantErr    bool
	}{
		{
			name:       "纯 JSON",
			in:         `{"reason":"npm 私服缺少 @yunnian/shared@2.6.1","suggestion":"回退到 2.6.0","confidence":0.92}`,
			wantReason: "npm 私服缺少 @yunnian/shared@2.6.1",
			wantConf:   0.92,
		},
		{
			name:       "带 json 语言标记的代码围栏",
			in:         "```json\n{\"reason\":\"依赖安装失败\",\"suggestion\":\"检查私服同步\",\"confidence\":0.8}\n```",
			wantReason: "依赖安装失败",
			wantConf:   0.8,
		},
		{
			name:       "无语言标记的代码围栏",
			in:         "```\n{\"reason\":\"编译失败\",\"suggestion\":\"补 import\",\"confidence\":0.7}\n```",
			wantReason: "编译失败",
			wantConf:   0.7,
		},
		{
			name:       "JSON 前有寒暄",
			in:         "好的，我来分析一下：\n{\"reason\":\"构建超时\",\"suggestion\":\"检查构建机网络\",\"confidence\":0.6}",
			wantReason: "构建超时",
			wantConf:   0.6,
		},
		{
			name:       "JSON 后有尾随说明",
			in:         "{\"reason\":\"内存不足\",\"suggestion\":\"调大构建机内存\",\"confidence\":0.5}\n希望以上分析对排查有帮助。",
			wantReason: "内存不足",
			wantConf:   0.5,
		},
		{
			name:       "reason 里自带右花括号不能被截断",
			in:         `{"reason":"Maven 报 Unresolved dependency {artifact}，坐标缺失","suggestion":"补齐坐标","confidence":0.66}`,
			wantReason: "Maven 报 Unresolved dependency {artifact}，坐标缺失",
			wantConf:   0.66,
		},
		{
			name:       "confidence 写成 90 要钳到 1 而不是抹成 0",
			in:         `{"reason":"原因","suggestion":"建议","confidence":90}`,
			wantReason: "原因",
			wantConf:   1,
		},
		{
			name:       "confidence 为负钳到 0",
			in:         `{"reason":"原因","suggestion":"建议","confidence":-3}`,
			wantReason: "原因",
			wantConf:   0,
		},
		{
			name:    "缺少 suggestion 视为无效",
			in:      `{"reason":"只给了原因","confidence":0.9}`,
			wantErr: true,
		},
		{
			name:    "原因与建议都是空白视为无效",
			in:      `{"reason":"  ","suggestion":"\n","confidence":0.9}`,
			wantErr: true,
		},
		{
			name:    "不是 JSON",
			in:      "构建失败了，可能是网络问题。",
			wantErr: true,
		},
		{
			name:    "空输出",
			in:      "   \n  ",
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseModelDiagnosis(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，实际拿到 %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got.Reason != c.wantReason {
				t.Errorf("reason = %q，期望 %q", got.Reason, c.wantReason)
			}
			if got.Confidence != c.wantConf {
				t.Errorf("confidence = %v，期望 %v", got.Confidence, c.wantConf)
			}
		})
	}
}

func TestStripCodeFence(t *testing.T) {
	// 不以围栏开头时必须原样返回：误删首行会把 JSON 的第一个字段吃掉。
	if got := stripCodeFence(`{"reason":"a"}`); got != `{"reason":"a"}` {
		t.Errorf("无围栏时被改动: %q", got)
	}
	if got := stripCodeFence("```json\n{\"a\":1}\n```"); got != `{"a":1}` {
		t.Errorf("围栏未剥净: %q", got)
	}
	// 只有开头围栏、结尾缺失（模型被 max_tokens 截断）时也要能处理。
	if got := stripCodeFence("```json\n{\"a\":1}"); got != `{"a":1}` {
		t.Errorf("只有起始围栏时未处理: %q", got)
	}
}

// 提示词必须把失败上下文与日志都带上，且带上错误码——
// 少了错误码，模型就只剩日志可看，规则表里那条最确定的线索反而丢了。
func TestBuildDiagnosisMessages(t *testing.T) {
	rec := &model.BuildRecord{
		BuildNo:     12,
		ProjectKey:  "demoitms",
		Branch:      "master",
		CommitID:    "a91f3c2d5e6f7788990011223344556677889900",
		Stage:       "构建",
		TriggerType: "manual",
		ErrorCode:   "NPM_INSTALL",
		JenkinsJob:  "镜像打包-demoitms",
	}
	msgs := buildDiagnosisMessages(rec, "npm ERR! 404 Not Found - GET /@yunnian/shared")

	if len(msgs) != 2 {
		t.Fatalf("期望 2 条消息（system + user），实际 %d", len(msgs))
	}
	if msgs[0].Role != "system" || !strings.Contains(msgs[0].Content, "不要") {
		t.Errorf("system 提示词缺失或未约束编造行为: %q", msgs[0].Content)
	}

	user := msgs[1].Content
	for _, want := range []string{"demoitms", "master", "#12", "NPM_INSTALL", "镜像打包-demoitms", "npm ERR! 404"} {
		if !strings.Contains(user, want) {
			t.Errorf("user 消息缺少 %q\n---\n%s", want, user)
		}
	}
	// commit 必须按短号给，40 位全量塞进提示词只是噪音。
	if strings.Contains(user, "a91f3c2d5e6f7788990011223344556677889900") {
		t.Error("user 消息带了完整 commit id，应取短号")
	}
}

// 降级路径：模型没参与时 source 必须是 rule，且结论不能为空——
// 界面上"规则匹配"也要有一句话，否则用户看到的是空白抽屉。
func TestRuleDiagnosisFallbacks(t *testing.T) {
	// 1) 库里有归因（种子/执行期写入）：直接用
	withStored := &model.BuildRecord{
		ProjectKey:  "demoitms",
		ErrorCode:   "NPM_INSTALL",
		AIDiagnosis: model.JSONText(`{"reason":"库里存的原因","suggestion":"库里存的建议","confidence":0.88}`),
	}
	got := ruleDiagnosis(withStored)
	if got.Source != diagnosisSourceRule {
		t.Errorf("source = %q，期望 %q", got.Source, diagnosisSourceRule)
	}
	if got.Reason != "库里存的原因" || got.Confidence != 0.88 {
		t.Errorf("未使用库里已有归因: %+v", got)
	}

	// 2) 库里没有、错误码能查表：走规则表
	byCode := ruleDiagnosis(&model.BuildRecord{ProjectKey: "demoitms", ErrorCode: "NPM_INSTALL"})
	if byCode.Reason == "" || byCode.Confidence == 0 {
		t.Errorf("错误码规则未生效: %+v", byCode)
	}

	// 3) 什么都识别不了：也要给一句能指路的话，而不是空
	unknown := ruleDiagnosis(&model.BuildRecord{ProjectKey: "demoitms", ErrorCode: "WHO_KNOWS"})
	if unknown.Reason == "" || unknown.Suggestion == "" {
		t.Errorf("未知错误码时结论为空: %+v", unknown)
	}
	if unknown.Confidence > 0.5 {
		t.Errorf("未知错误码不该给高置信: %v", unknown.Confidence)
	}
}

// 缓存字段与来源必须能往返：落库带着 source=model，读回来才知道"这条不用再算一遍"。
func TestDiagnosisDTORoundTrip(t *testing.T) {
	raw := model.JSONText(`{"reason":"r","suggestion":"s","confidence":0.9,"source":"model","model":"qwen-plus"}`)
	d := decodeAIDiagnosis(raw)
	if d == nil {
		t.Fatal("解析失败")
	}
	if d.Source != diagnosisSourceModel || d.Model != "qwen-plus" {
		t.Errorf("来源未往返: source=%q model=%q", d.Source, d.Model)
	}
	// 旧数据（没有 source 字段）必须仍能解析——它们会被当成 rule 结论使用。
	legacy := decodeAIDiagnosis(model.JSONText(`{"reason":"旧","suggestion":"旧","confidence":0.5}`))
	if legacy == nil || legacy.Source != "" {
		t.Errorf("旧数据兼容失败: %+v", legacy)
	}
}
