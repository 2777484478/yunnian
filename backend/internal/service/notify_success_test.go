package service

import (
	"strings"
	"testing"

	"github.com/2777484478/yunnian/internal/config"
	"github.com/2777484478/yunnian/internal/model"
)

// 成功通知（2026-10-15 新增）的构造与渲染。
//
// 与 `notify_interrupted_test.go` 的 `TestSupportedNotifyEventsAreComplete` 是**两张不同的表**，
// 各守一件事，别合并：
//
//   - 那张表守「**有代码会触发它**」—— 加码却忘了接触发点，它报红；
//   - 这张表守「**触发时构造得出来**」—— 加码却忘了写构造器 / 级别写错 / 标题是空的，它报红。
//
// 两张表都在加码时提醒人，但提醒的是不同的疏漏。当年 build_success 那批码
// 恰好**两样都缺**，所以两道都得有。

// notifySvc 构造一个只够跑消息构造器的服务。
//
// 需要 `Cfg`：`link()` 会读 `Cfg.Notify.PublicBaseURL` 拼详情链接，
// 而 `Deps.Cfg` 是**指针**——零值会直接 nil 解引用 panic（第一版就是这么挂的）。
// 默认配置里 PublicBaseURL 为空，`link()` 返回空串，正文里就不渲染链接行。
func notifySvc(t *testing.T) *NotifyService {
	t.Helper()
	return &NotifyService{deps: Deps{Cfg: config.Default()}}
}

// TestEverySupportedEventIsBuildable 每个对外公布的事件码，都必须能构造出消息。
func TestEverySupportedEventIsBuildable(t *testing.T) {
	svc := notifySvc(t)

	build := &model.BuildRecord{ProjectKey: "demo-web", BuildNo: 7, Branch: "main",
		CommitID: "abcdef1234567890", TriggerName: "运维示例"}
	deploy := &model.DeployRecord{EnvKey: "prod", ProjectKey: "demo-web",
		DeployNo: "DP202610150001", Image: "harbor/demo-web:1.2.3", Replicas: 2,
		OperatorName: "运维示例"}

	// 表里每一行 = "这个码由哪个构造器产出、期望什么级别"。
	// `wantLevel` 一并写死，是因为级别决定了标题前缀符号（❌/⚠️/✅），
	// 而那是收件人扫群消息列表时唯一的区分手段。
	cases := []struct {
		code      NotifyEvent
		wantLevel string
		msg       NotifyMessage
	}{
		{EventBuildFailed, "error", svc.BuildFailedMessage(build)},
		{EventBuildInterrupted, "warning", svc.BuildInterruptedMessage([]*model.BuildRecord{build})},
		{EventBuildSucceeded, "success", svc.BuildSucceededMessage(build, "harbor/demo-web:1.2.3")},
		{EventDeployFailed, "error", svc.DeployFailedMessage(deploy)},
		{EventDeployInterrupted, "warning", svc.DeployInterruptedMessage([]*model.DeployRecord{deploy})},
		{EventDeployPendingApprove, "warning", svc.DeployPendingMessage(deploy)},
		{EventDeploySucceeded, "success", svc.DeploySucceededMessage(deploy)},
	}

	seen := map[NotifyEvent]bool{}
	for _, c := range cases {
		seen[c.code] = true

		if c.msg.Event != c.code {
			t.Errorf("构造器产出的 Event=%q，登记表写的是 %q：消息会被投递到错误的订阅者那里",
				c.msg.Event, c.code)
		}
		if c.msg.Level != c.wantLevel {
			t.Errorf("%s 的 Level=%q，期望 %q", c.code, c.msg.Level, c.wantLevel)
		}
		if c.msg.Title == "" {
			t.Errorf("%s 没有标题：群里只会看到一条空白消息", c.code)
		}
	}

	// 反向：清单里的每一项都必须在上表登记过。
	for _, code := range SupportedNotifyEvents {
		if !seen[code] {
			t.Errorf("事件码 %q 在 SupportedNotifyEvents 里对外公布，却没有登记构造器："+
				"要么补上构造器与触发点，要么把它从清单里去掉 —— "+
				"留在界面上的勾选框等于一句承诺。", code)
		}
	}
	if len(seen) != len(SupportedNotifyEvents) {
		t.Errorf("登记表 %d 项，SupportedNotifyEvents %d 项", len(seen), len(SupportedNotifyEvents))
	}
}

// TestBuildSucceededMessageFields 构建成功通知带的是"可以拿去部署"的事实。
func TestBuildSucceededMessageFields(t *testing.T) {
	svc := notifySvc(t)
	dur := uint32(117)
	rec := &model.BuildRecord{
		ProjectKey: "demo-web", BuildNo: 42, Branch: "release/1.2",
		CommitID: "abcdef1234567890", TriggerName: "运维示例", DurationSec: &dur,
	}

	m := svc.BuildSucceededMessage(rec, "harbor.internal/demo-web:2.2.0.1009")

	if m.Level != "success" {
		t.Fatalf("级别应为 success，实际 %q", m.Level)
	}
	got := map[string]string{}
	for _, f := range m.Fields {
		got[f.Name] = f.Value
	}
	// 镜像是这条通知里最有价值的字段：收件人拿它去部署。少了它这条消息只剩"完成了"。
	if got["镜像"] != "harbor.internal/demo-web:2.2.0.1009" {
		t.Errorf("镜像字段不对：%q", got["镜像"])
	}
	if got["构建号"] != "#42" {
		t.Errorf("构建号应为 #42，实际 %q", got["构建号"])
	}
	if got["耗时"] != "1m57s" {
		t.Errorf("耗时应为 1m57s，实际 %q", got["耗时"])
	}
	if got["提交"] != "abcdef1" {
		t.Errorf("提交应截成 7 位短号，实际 %q", got["提交"])
	}
}

// TestBuildSucceededMessageWithoutImage 没登记到镜像时**照样发**，只是不显示那一行。
//
// Jenkins Job 里本来就有只做编译、不推镜像的，那些构建也是成功的。
// 这里同时钉住"字段留空"而不是"编一个地址出来"。
func TestBuildSucceededMessageWithoutImage(t *testing.T) {
	svc := notifySvc(t)
	m := svc.BuildSucceededMessage(&model.BuildRecord{ProjectKey: "lib-common", BuildNo: 3}, "")

	if m.Event != EventBuildSucceeded {
		t.Fatalf("事件码应为 %s，实际 %s", EventBuildSucceeded, m.Event)
	}
	for _, f := range m.Fields {
		if f.Name == "镜像" && f.Value != "" {
			t.Errorf("没有镜像就应该留空，实际写了 %q —— 这是编出来的事实", f.Value)
		}
	}
	// 渲染层会跳过空值，正文里不该出现一行光秃秃的「镜像：」。
	if strings.Contains(renderPlainText(m), "镜像：\n") {
		t.Error("渲染结果里出现了空的「镜像：」行，渲染层应跳过空值字段")
	}
}

// TestDeploySucceededMessageRollbackTitle 回滚产生的部署单也走同一个事件码，标题要区分。
func TestDeploySucceededMessageRollbackTitle(t *testing.T) {
	svc := notifySvc(t)

	normal := svc.DeploySucceededMessage(&model.DeployRecord{
		EnvKey: "prod", ProjectKey: "demo-web", DeployNo: "DP1", Replicas: 2, Image: "harbor/x:1",
	})
	if !strings.HasPrefix(normal.Title, "部署成功") {
		t.Errorf("普通部署单标题应为「部署成功…」，实际 %q", normal.Title)
	}

	parent := uint64(99)
	rollback := svc.DeploySucceededMessage(&model.DeployRecord{
		EnvKey: "prod", ProjectKey: "demo-web", DeployNo: "DP2", Replicas: 1,
		Image: "harbor/x:0", ParentDeploy: &parent,
	})
	if !strings.HasPrefix(rollback.Title, "回滚成功") {
		t.Errorf("回滚单标题应为「回滚成功…」，实际 %q", rollback.Title)
	}
	// 事件码必须是同一个：订阅者关心的是"这次发布成功了"，不是"走的是哪条链路"。
	if rollback.Event != EventDeploySucceeded {
		t.Errorf("回滚单也应用 %s，实际 %s", EventDeploySucceeded, rollback.Event)
	}
}

// TestDurationHint 耗时格式化。
//
// `nil` → 空串而不是 "0s"：duration_sec 在终态落库前是 NULL，
// 而 "0s" 会让人以为这次构建秒过。
func TestDurationHint(t *testing.T) {
	sec := func(v uint32) *uint32 { return &v }
	for _, c := range []struct {
		in   *uint32
		want string
	}{
		{nil, ""},
		{sec(0), "0s"},
		{sec(42), "42s"},
		{sec(59), "59s"},
		{sec(60), "1m0s"},
		{sec(117), "1m57s"},
		{sec(3725), "62m5s"},
	} {
		if got := durationHint(c.in); got != c.want {
			t.Errorf("durationHint(%v) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestNotifyTitleCarriesLevelIcon 级别 → 标题前缀。
//
// 三类消息会进**同一个群**：失败（要人来看）、被中断（要人去重跑）、成功（可以验收了）。
// 一个符号是群里扫列表时唯一的区分手段 —— 钉钉的 markdown 没有颜色语法。
func TestNotifyTitleCarriesLevelIcon(t *testing.T) {
	svc := notifySvc(t)

	if got := notifyTitle(svc.BuildSucceededMessage(&model.BuildRecord{ProjectKey: "p", BuildNo: 1}, "img")); !strings.HasPrefix(got, "✅ ") {
		t.Errorf("成功消息标题应带 ✅，实际 %q", got)
	}
	if got := notifyTitle(svc.DeployFailedMessage(&model.DeployRecord{EnvKey: "e", ProjectKey: "p"})); !strings.HasPrefix(got, "❌ ") {
		t.Errorf("失败消息标题应带 ❌，实际 %q", got)
	}
	if got := notifyTitle(svc.DeployPendingMessage(&model.DeployRecord{EnvKey: "e", ProjectKey: "p"})); !strings.HasPrefix(got, "⚠️ ") {
		t.Errorf("待审批消息标题应带 ⚠️，实际 %q", got)
	}
	// 「测试渠道」这类没有级别的一次性消息不加符号 —— 它不是七类事件里的任何一个。
	if got := notifyTitle(NotifyMessage{Title: "云辇平台 · 通知渠道测试"}); got != "云辇平台 · 通知渠道测试" {
		t.Errorf("无级别的消息不该被加符号，实际 %q", got)
	}
}

// TestSuccessReasonLabelIsNotCause 成功消息的 Reason 标的是「说明」不是「原因」。
//
// 成功消息里那一栏写的是"这意味着什么"（新版本已发布成功、Pod 全部就绪）。
// 标成「原因」会被读成"为什么会成功"，语义反了。
func TestSuccessReasonLabelIsNotCause(t *testing.T) {
	svc := notifySvc(t)
	m := svc.DeploySucceededMessage(&model.DeployRecord{EnvKey: "prod", ProjectKey: "p", Replicas: 1})

	text := renderPlainText(m)
	if !strings.Contains(text, "说明：新版本已发布成功：滚动更新完成，Pod 全部就绪") {
		t.Errorf("邮件正文里成功消息应为「说明：新版本已发布成功…」，实际正文：\n%s", text)
	}
	if strings.Contains(text, "原因：") {
		t.Errorf("成功消息不该用「原因」做表头：\n%s", text)
	}

	md := renderDingtalkMarkdown(m)
	if !strings.Contains(md, "✅ ") {
		t.Errorf("钉钉正文标题应带 ✅：\n%s", md)
	}
	if !strings.Contains(md, "> **说明**：") {
		t.Errorf("钉钉正文里成功消息的 Reason 应为「说明」：\n%s", md)
	}

	// 失败侧不受影响。
	failMD := renderDingtalkMarkdown(svc.BuildFailedMessage(&model.BuildRecord{ProjectKey: "p", ErrorCode: "NPM_INSTALL"}))
	if !strings.Contains(failMD, "> **原因**：") {
		t.Errorf("失败消息的 Reason 仍应为「原因」：\n%s", failMD)
	}
	if !strings.HasPrefix(failMD, "### ❌ ") {
		t.Errorf("失败消息标题应带 ❌：\n%s", failMD)
	}
}
