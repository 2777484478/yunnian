package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/yunnian/yunnian/internal/integration"
	"github.com/yunnian/yunnian/internal/model"
)

// 本文件钉住三件"看不见但对用户很重要"的事：
//
//  1. **集群侧证据真的进了提示词** —— 否则所谓"接入 AI"就还是查表；
//  2. **模型没参与时必须如实标注** —— 把规则结论说成模型分析，比不给结论更糟；
//  3. **规则路径不许拿模型结论冒充自己** —— 用户会照着来源标签决定信不信。
//
// 不测"模型答得对不对"：那取决于模型，且要花真钱。这里测的是我们的契约。

func testDeployRecord() *model.DeployRecord {
	return &model.DeployRecord{
		ID: 5, DeployNo: "DP202610080005", EnvID: 25, EnvKey: "test",
		ProjectKey: "demobasic", ServiceID: 7,
		Image:    "es-repo.example.com:5000/escloud/demobasic:1.2.0.2761",
		Replicas: 1, Strategy: "RollingUpdate", Status: "failed", ErrorCode: "ROLLOUT_STUCK",
	}
}

func testDeploySvc() *model.ServiceInstance {
	return &model.ServiceInstance{
		ID: 7, ProjectKey: "demobasic", WorkloadName: "demobasic", Replicas: 1,
		CurrentImage: "es-repo.example.com:5000/escloud/demobasic:1.2.0.2577",
	}
}

func testDeployEnv() *model.Environment {
	return &model.Environment{ID: 25, EnvKey: "test", Namespace: "default", Cluster: "cluster-local"}
}

// demobasicEvidence 复刻实测那次失败（缺 nacos 配置导致 CrashLoopBackOff）的形状。
func demobasicEvidence() *integration.FailureEvidence {
	return &integration.FailureEvidence{
		Pods: []integration.PodEvidence{{
			Name: "demobasic-59b46b9bfb-gngg5", Phase: "Running", Container: "master",
			Restarts: 6, Reason: "CrashLoopBackOff", ExitCode: 1,
			Message:  "back-off 5m0s restarting failed container=master",
			Logs:     "Caused by: java.lang.IllegalArgumentException: Could not resolve placeholder 'messagelibrary.mq.username'",
			LogsFrom: "previous",
		}},
		Events: []string{"[BackOff] Back-off restarting failed container master"},
	}
}

func TestDeployEvidence_CarriesClusterFactsIntoPrompt(t *testing.T) {
	stub := &stubCluster{evidence: demobasicEvidence()}
	cd := newCDForTest(stub, nil)

	ev := cd.deployEvidence(context.Background(), testDeployRecord(), testDeploySvc(), testDeployEnv())
	if ev.Cluster == nil {
		t.Fatal("集群证据没进上下文：诊断会退化成查表")
	}

	// 平台侧日志在单元测试里是空的（MemStream 没有历史），这里正好覆盖
	// "日志流失后必须说明"这条：不能悄悄留个空串当作"日志里没报错"。
	if !strings.Contains(strings.Join(ev.Notes, "\n"), "取不到") {
		t.Errorf("平台日志缺失时没写进取证说明：%v", ev.Notes)
	}

	prompt := deployDiagnosisMessages(testDeployRecord(), testDeploySvc(), testDeployEnv(), ev)[1].Content
	for _, want := range []string{
		"CrashLoopBackOff",           // 容器状态
		"退出码 1",                      // 退出码
		"messagelibrary.mq.username", // 崩溃前那一轮日志里的真因
		"崩溃前那一轮",                     // 日志取自 previous，必须标明
		"Back-off restarting",        // Pod 事件
		"1.2.0.2577",                 // 变更前的版本，用来判断是不是本次发布引入的
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("提示词里缺 %q：\n%s", want, prompt)
		}
	}
}

func TestDeployEvidence_ClusterFailureBecomesNoteNotSilence(t *testing.T) {
	stub := &stubCluster{evidenceErr: errors.New("当前凭据无权读取 Pod 事件")}
	cd := newCDForTest(stub, nil)

	ev := cd.deployEvidence(context.Background(), testDeployRecord(), testDeploySvc(), testDeployEnv())
	if ev.Cluster != nil {
		t.Fatal("取证失败时不该给出证据对象")
	}
	joined := strings.Join(ev.Notes, "\n")
	if !strings.Contains(joined, "无权") {
		t.Errorf("取证失败的原因必须如实带出：%v", ev.Notes)
	}
	// 关键：提示词里必须说清"没采到"，而不是留白让模型以为是"没有异常"。
	prompt := deployDiagnosisMessages(testDeployRecord(), testDeploySvc(), testDeployEnv(), ev)[1].Content
	if !strings.Contains(prompt, "未采集到") {
		t.Errorf("提示词没说清证据缺失：\n%s", prompt)
	}
}

func TestFillDeployDiagnosis_DegradesHonestlyWhenModelUnavailable(t *testing.T) {
	// 不带 sys 的 CDService：deployModelDiagnosis 会以"模型通道未注入"失败。
	cd := newCDForTest(&stubCluster{}, nil)
	out := &DeployDiagnosis{}

	cd.fillDeployDiagnosis(context.Background(), out, testDeployRecord(), testDeploySvc(), testDeployEnv())

	if out.Source != diagnosisSourceRule {
		t.Errorf("source = %q，期望 rule", out.Source)
	}
	if out.Model != "" {
		t.Errorf("降级时不该声称用过模型：model=%q", out.Model)
	}
	// notice 必须同时包含"没用模型"这个结论**和**具体原因 —— 只说前者等于把
	// "为什么"藏起来，用户会去猜是不是自己配错了。
	if !strings.Contains(out.Notice, "未由模型分析") || !strings.Contains(out.Notice, "未启用") {
		t.Errorf("notice 必须说清为什么没用上模型，实际 %q", out.Notice)
	}
	// 仍然要给得出可用的规则结论 —— 降级不等于什么都不说。
	if !strings.Contains(out.Reason, "卡死") {
		t.Errorf("ROLLOUT_STUCK 的规则结论没生效：%q", out.Reason)
	}
}

func TestFillDeployDiagnosis_ModelCacheIsNotRecomputed(t *testing.T) {
	cd := newCDForTest(&stubCluster{}, nil)
	rec := testDeployRecord()
	rec.AIDiagnosis = model.JSONText(
		`{"reason":"模型看的真因","suggestion":"补 nacos 配置","confidence":0.88,"source":"model","model":"qwen-plus"}`)
	out := &DeployDiagnosis{}

	cd.fillDeployDiagnosis(context.Background(), out, rec, testDeploySvc(), testDeployEnv())

	if out.Source != diagnosisSourceModel || out.Model != "qwen-plus" {
		t.Fatalf("模型结论的缓存没命中：source=%q model=%q", out.Source, out.Model)
	}
	if out.Reason != "模型看的真因" || out.Confidence != 0.88 {
		t.Errorf("缓存内容没带出来：%+v", out)
	}
}

func TestFillDeployDiagnosis_NonFailedIsNotCalledAnomaly(t *testing.T) {
	cd := newCDForTest(&stubCluster{}, nil)
	rec := testDeployRecord()
	rec.Status = "success"
	out := &DeployDiagnosis{}

	cd.fillDeployDiagnosis(context.Background(), out, rec, testDeploySvc(), testDeployEnv())

	if out.Source != diagnosisSourceRule || out.Confidence != 1 {
		t.Errorf("成功单不该走模型：source=%q confidence=%v", out.Source, out.Confidence)
	}
	if !strings.Contains(out.Reason, "没有异常") {
		t.Errorf("reason = %q", out.Reason)
	}
}

func TestRuleDeployDiagnosis_NeverBorrowsModelText(t *testing.T) {
	rec := testDeployRecord()

	// 库里存的是**模型**结论时，规则路径必须跳过它，改用自己的错误码规则。
	// 否则降级发生时，用户会看到"本地规则"标签配着模型写的文案。
	rec.AIDiagnosis = model.JSONText(
		`{"reason":"模型结论","suggestion":"模型建议","confidence":0.9,"source":"model","model":"qwen-plus"}`)
	got := ruleDeployDiagnosis(rec)
	if got.Reason == "模型结论" {
		t.Fatal("规则路径拿了模型的结论冒充自己")
	}
	if !strings.Contains(got.Reason, "卡死") {
		t.Errorf("应当退回按错误码的规则结论，实际 %q", got.Reason)
	}

	// 库里存的是规则结论（failDeploy 写的，source 为空）时，直接复用它。
	rec2 := testDeployRecord()
	rec2.AIDiagnosis = model.JSONText(`{"reason":"落库的规则结论","suggestion":"s","confidence":0.5}`)
	if got := ruleDeployDiagnosis(rec2); got.Reason != "落库的规则结论" {
		t.Errorf("没复用已落库的规则结论：%q", got.Reason)
	}

	// 未知错误码也要给得出结论，不能返回 nil（那会让前端拿到空文案）。
	rec3 := testDeployRecord()
	rec3.ErrorCode = "SOMETHING_NEW"
	got3 := ruleDeployDiagnosis(rec3)
	if got3 == nil || got3.Reason == "" || got3.Suggestion == "" {
		t.Errorf("未知错误码必须兜底：%+v", got3)
	}
}

func TestDeployDiagnoseRule_CoversRolloutStuck(t *testing.T) {
	d := diagnoseDeploy("ROLLOUT_STUCK")
	if d == nil {
		t.Fatal("ROLLOUT_STUCK 没有规则兜底，模型不可用时用户什么都看不到")
	}
	// 它必须与 TIMEOUT 明显不同：一个要查"为什么起不来"，一个要查"为什么慢"。
	// 它**可以**在文案里提到「部署超时」做对比（那正是帮助用户区分的地方），
	// 但不能就是 TIMEOUT 那段话本身。
	timeout := diagnoseDeploy("TIMEOUT")
	if d.Reason == timeout.Reason || d.Suggestion == timeout.Suggestion {
		t.Errorf("ROLLOUT_STUCK 与 TIMEOUT 用了同一段兜底文案：%q", d.Reason)
	}
	if !strings.Contains(d.Reason, "卡死") {
		t.Errorf("文案要直接点名卡死：%q", d.Reason)
	}
	// 建议必须指到具体动作，否则"卡死"三个字对排查毫无帮助。
	for _, want := range []string{"CrashLoopBackOff", "OOMKilled", "ImagePullBackOff"} {
		if !strings.Contains(d.Suggestion, want) {
			t.Errorf("建议里缺 %q：%q", want, d.Suggestion)
		}
	}
}

// joinDeployLogLines 把日志行拼成一段文本，便于断言。
func joinDeployLogLines(lines []deployLogLine) string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, l.Text)
	}
	return strings.Join(out, "\n")
}

// 部署失败时写给**人**看的那一段日志：内容是原始证据，不是结论。
//
// 这里要钉死的是顺序与截断 —— 这两条错了，用户看到的一屏会正好把要排查的东西藏起来：
//
//	① 取证说明必须在**最前面**（它决定下面的证据能信几分，压到末尾会被长日志冲走）；
//	② 容器日志只留**尾部**（根因在最后几行，留头部等于把答案删掉）。
func TestDeployEvidenceLogLines_NotesFirstAndLogsFromTail(t *testing.T) {
	raw := make([]string, 0, 50)
	for i := 1; i <= 50; i++ {
		raw = append(raw, fmt.Sprintf("line-%02d", i))
	}
	ev := &integration.FailureEvidence{
		Notes: []string{"Pod 的容器日志没取到（forbidden）：这份证据会缺掉最关键的一块"},
		Pods: []integration.PodEvidence{{
			Name: "demobasic-59b46b9bfb-bxrzw", Phase: "Running", Container: "master",
			Restarts: 13, Reason: "CrashLoopBackOff", ExitCode: 1,
			Message: `back-off 5m0s restarting failed container=master`,
			Logs:    strings.Join(raw, "\n"), LogsFrom: "previous",
		}},
		Events: []string{
			"[BackOff] Back-off restarting failed container master",
			`[Unhealthy] Readiness probe failed: Get "http://10.233.90.102:9629/digiwin/info": dial tcp`,
		},
	}

	lines := deployEvidenceLogLines(ev)
	if len(lines) < 3 {
		t.Fatalf("产出的日志行太少：%d", len(lines))
	}
	// ① 标题之后紧跟取证说明
	if !strings.Contains(lines[1].Text, "取证说明") {
		t.Errorf("取证说明没有排在证据之前：%q", lines[1].Text)
	}

	joined := joinDeployLogLines(lines)
	for _, want := range []string{
		"demobasic-59b46b9bfb-bxrzw", // 哪个 Pod
		"CrashLoopBackOff",          // 坏在哪
		"退出码 1",                     // 怎么坏的
		"重启 13 次",                   // 坏了几回
		"[BackOff]",                 // 事件：用户截图里那一列
		"[Unhealthy]",               // 事件
		"Readiness probe failed",    // 事件原文（探针打不通的目标地址）
		"崩溃前那一轮",                    // 日志取自上一轮实例，必须标明
		"line-50",                   // 尾部最后一行必须在
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("日志里缺 %q：\n%s", want, joined)
		}
	}
	// ② 只留尾部：50 行取 20 行 → 最早应到 line-31，line-30 必须已被裁掉
	if strings.Contains(joined, "line-30") {
		t.Errorf("日志没有从尾部截断，开头的行还在：\n%s", joined)
	}
}

// 日志为空与"日志里没有报错"是两件事 —— 取不到时必须说出来，
// 否则读的人会正好反过来理解（把"我们没看到"当成"应用没问题"）。
func TestDeployEvidenceLogLines_EmptyLogsAreSaidOut(t *testing.T) {
	ev := &integration.FailureEvidence{
		Pods: []integration.PodEvidence{{
			Name: "demobasic-7f9c-x2vq", Phase: "Pending",
			Reason: "ImagePullBackOff", Restarts: 0,
		}},
	}
	joined := joinDeployLogLines(deployEvidenceLogLines(ev))
	if !strings.Contains(joined, "没取到") {
		t.Errorf("容器日志为空时没有如实说明：\n%s", joined)
	}
	if !strings.Contains(joined, "ImagePullBackOff") {
		t.Errorf("容器状态没进日志：\n%s", joined)
	}
	if strings.Contains(joined, "没有报错") && !strings.Contains(joined, "别把这条读成") {
		t.Errorf("措辞可能被误读：\n%s", joined)
	}
}

// 全部就绪时也要给结论方向：失败更像平台侧等待超时。沉默会让用户以为平台没查。
func TestDeployEvidenceLogLines_AllReadyPointsAtTimeout(t *testing.T) {
	ev := &integration.FailureEvidence{
		Notes: []string{"该工作负载当前 1 个 Pod 全部就绪，没有采集到容器侧的失败证据" +
			"（这时失败更可能是平台侧等待超时，而不是容器起不来）"},
	}
	joined := joinDeployLogLines(deployEvidenceLogLines(ev))
	if !strings.Contains(joined, "等待超时") {
		t.Errorf("全部就绪时没有指出更像超时：\n%s", joined)
	}
}
