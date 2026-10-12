package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yunnian/yunnian/internal/config"
	"github.com/yunnian/yunnian/internal/integration"
	"github.com/yunnian/yunnian/internal/logstream"
	"github.com/yunnian/yunnian/internal/model"
)

// 本文件钉住「平台重启中断」这条通知链路。
//
// 为什么值得单独测：`interrupted` 是**唯一一个曾经完全没有告警的失败态**。
// 它是终态、不会自行恢复，界面上有筛选器也有重试入口 —— 但那些都是**等人来看**的。
// 平台在凌晨重启一次，没人打开列表，这件事就等于没发生过。
//
// 而它又是最容易"看起来修好了"的一类：通知代码跑通、接口返回 200、
// 日志里没有错误，但事件码写错一个字母、或者渠道没订阅上，结果依然是静默。
// 所以这里不只断言"发了"，还断言**发给了谁**（订阅过滤的反证）与**发了什么内容**。
//
// 用桩钉钉而不是真机器人：钉钉对失败也返回 HTTP 200，真发只能看群里有没有消息，
// 断不了"内容里到底是哪条记录"。桩能把请求体留下来逐字断言，且能长期复跑。

// --------------------------------------------------------------------------- 桩

// stubDingtalk 记下每次投递的路径与请求体。
//
// 记路径是为了做**订阅过滤的反证**：把两个渠道都指向同一个桩、但只让其中一个订阅
// 新事件，然后断言所有请求都打在那个路径上。少了这一点，"通知发了"只能证明
// 有人收到了，证明不了"不该收到的人没收到"。
type stubDingtalk struct {
	mu     sync.Mutex
	paths  []string
	bodies []string
}

func (s *stubDingtalk) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.paths = append(s.paths, r.URL.Path)
	s.bodies = append(s.bodies, string(body))
	s.mu.Unlock()
	// 必须回 errcode=0：钉钉无论成败都是 HTTP 200，真实结果在 body 里，
	// 回一个空 body 会让发送器走到"无法解析的响应"分支，把这条测试变成假红。
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
}

func (s *stubDingtalk) snapshot() ([]string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...), append([]string(nil), s.bodies...)
}

// waitFor 轮询直到 cond 成立；超时返回 false（由调用方给断言消息）。
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --------------------------------------------------------------------------- 端到端

// TestReconcileInterruptedDispatchesOnlyToSubscribers 走完整条链路：
// 库里有"重启那一刻还没结束"的记录 → 启动补偿 → 标 interrupted → 按订阅投递。
func TestReconcileInterruptedDispatchesOnlyToSubscribers(t *testing.T) {
	db := newTestDB(t)

	stub := &stubDingtalk{}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	t.Cleanup(srv.Close)

	mk := func(name, path, events string) model.NotifyChannel {
		return model.NotifyChannel{
			Name: name, Type: "dingtalk",
			Target: srv.URL + path + "?access_token=tk",
			Events: model.JSONText(events), Enabled: true,
		}
	}
	subscribed := mk("订阅了中断", "/sub", `["build_interrupted","deploy_interrupted"]`)
	other := mk("只订了失败的", "/other", `["build_failed"]`)
	for _, c := range []*model.NotifyChannel{&subscribed, &other} {
		if err := db.Create(c).Error; err != nil {
			t.Fatalf("写入通知渠道失败: %v", err)
		}
	}

	// 构建/部署队列都是**进程内**的，所以这两条记录在真实场景里就是"被重启留下来的孤儿"。
	started := time.Now().Add(-90 * time.Second)
	build := model.BuildRecord{
		BuildNo: 7, ProjectID: 1, ProjectKey: "demouser", Branch: "master",
		CommitID: "abcdef1234567890", TriggerName: "运维示例",
		Status: "queued", Stage: "排队中", StartedAt: &started, UpdatedAt: time.Now(),
	}
	if err := db.Create(&build).Error; err != nil {
		t.Fatalf("写入构建记录失败: %v", err)
	}
	deploy := model.DeployRecord{
		DeployNo: "DPL20261010099", EnvID: 1, EnvKey: "test",
		ProjectID: 1, ProjectKey: "demouser", ServiceID: 1,
		Image:  "es-repo.example.com:5000/escloudapi/demouser:master-abcdef1-20261010155000",
		Status: "progressing", OperatorName: "运维示例", StartedAt: &started, UpdatedAt: time.Now(),
	}
	if err := db.Create(&deploy).Error; err != nil {
		t.Fatalf("写入部署记录失败: %v", err)
	}

	cfg := config.Default()
	cfg.Notify.PublicBaseURL = "http://127.0.0.1:8080"
	svc := New(Deps{
		Cfg: cfg, DB: db, Log: discardLogger(),
		Stream: logstream.NewMemStream(), Up: &integration.Set{},
	})

	ctx := context.Background()
	bn, err := svc.CI.ReconcileInterrupted(ctx)
	if err != nil {
		t.Fatalf("构建侧启动补偿失败: %v", err)
	}
	dn, err := svc.CD.ReconcileInterrupted(ctx)
	if err != nil {
		t.Fatalf("部署侧启动补偿失败: %v", err)
	}
	if bn != 1 || dn != 1 {
		t.Errorf("应当各标记 1 条为中断，得到构建 %d / 部署 %d", bn, dn)
	}

	// 库必须写对：状态、错误码、人类可读原因。第三条尤其要紧 ——
	// 日志流是进程内 + TTL 的，重启后已经没了，error_message 是这次中断**唯一**的落库解释。
	var gotBuild model.BuildRecord
	if err := db.First(&gotBuild, build.ID).Error; err != nil {
		t.Fatalf("回读构建记录失败: %v", err)
	}
	if gotBuild.Status != "interrupted" {
		t.Errorf("构建状态应为 interrupted，得到 %q", gotBuild.Status)
	}
	if gotBuild.ErrorCode != "INTERRUPTED" {
		t.Errorf("构建错误码应为 INTERRUPTED，得到 %q", gotBuild.ErrorCode)
	}
	if strings.TrimSpace(gotBuild.ErrorMessage) == "" {
		t.Error("构建中断没有落 error_message：重启后这是唯一的解释来源")
	}
	var gotDeploy model.DeployRecord
	if err := db.First(&gotDeploy, deploy.ID).Error; err != nil {
		t.Fatalf("回读部署记录失败: %v", err)
	}
	if gotDeploy.Status != "interrupted" {
		t.Errorf("部署状态应为 interrupted，得到 %q", gotDeploy.Status)
	}

	// 投递是**后台 goroutine**（Dispatch 不阻塞调用方，这是设计要求），所以这里必须等。
	//
	// ⚠️ 等的是"桩收到 2 次请求 **且** last_sent_at 已回写"这两件事同时成立，
	// 而不是只等前者。发送器读到响应之后才写 last_sent_at，而桩在**处理请求时**就记下了 ——
	// 只等请求数会在回写之前就往下断言。这不是假设：本测试第一版就是这么写的，
	// 单独跑绿、全量跑红（下面那句 "投递成功却没写 last_sent_at" 就是这么来的）。
	done := waitFor(func() bool {
		if paths, _ := stub.snapshot(); len(paths) < 2 {
			return false
		}
		var c model.NotifyChannel
		if err := db.First(&c, subscribed.ID).Error; err != nil {
			return false
		}
		return c.LastSentAt != nil
	}, 5*time.Second)

	paths, bodies := stub.snapshot()
	if !done {
		var c model.NotifyChannel
		_ = db.First(&c, subscribed.ID).Error
		t.Fatalf("等满 5s 仍未完成「投递 + 回写」：收到 %d 次投递，lastSentAt=%v failCount=%d lastError=%q",
			len(paths), c.LastSentAt, c.FailCount, c.LastError)
	}
	if len(paths) != 2 {
		t.Errorf("期望恰好 2 次投递（构建 + 部署各一条），实际 %d 次：%v", len(paths), paths)
	}

	for _, p := range paths {
		if p != "/sub" {
			t.Errorf("投递打到了**未订阅**该事件的渠道：%s（订阅过滤失效，"+
				"后果是订阅了 build_failed 的运维群会被平台自身的重启刷屏）", p)
		}
	}
	joined := strings.Join(bodies, "\n---\n")
	for _, want := range []string{"构建被中断", "demouser #7", "部署被中断", "test/demouser"} {
		if !strings.Contains(joined, want) {
			t.Errorf("投递内容里缺少 %q；实际内容：\n%s", want, joined)
		}
	}
	// 措辞必须承认"是平台重启"，不能读成"这次构建/部署哪里写错了"。
	if !strings.Contains(joined, "平台重启") {
		t.Errorf("投递内容没说明成因是平台重启，收到的人会去查构建本身：\n%s", joined)
	}

	// 投递结果要回写，否则界面上的「最近投递」列只会显示"尚无成功投递"，
	// 用户配好了渠道却以为平台没发。
	var sub model.NotifyChannel
	if err := db.First(&sub, subscribed.ID).Error; err != nil {
		t.Fatalf("回读订阅渠道失败: %v", err)
	}
	if sub.LastSentAt == nil {
		t.Error("投递成功却没写 last_sent_at")
	}
	if sub.FailCount != 0 {
		t.Errorf("投递成功却留下 fail_count=%d", sub.FailCount)
	}

	var oth model.NotifyChannel
	if err := db.First(&oth, other.ID).Error; err != nil {
		t.Fatalf("回读未订阅渠道失败: %v", err)
	}
	if oth.LastSentAt != nil || oth.FailCount != 0 {
		t.Errorf("未订阅该事件的渠道不该有任何投递痕迹：lastSentAt=%v failCount=%d",
			oth.LastSentAt, oth.FailCount)
	}
}

// --------------------------------------------------------------------------- 消息构造

// TestInterruptedMessageShapes 钉住消息的几个关键取舍（不需要库）。
func TestInterruptedMessageShapes(t *testing.T) {
	svc := &NotifyService{deps: Deps{Cfg: &config.Config{
		Notify: config.NotifyConfig{PublicBaseURL: "http://127.0.0.1:8080"},
	}}}

	// 空切片：调用方（ReconcileInterrupted）可能一条都没有，此时不该投递。
	if m := svc.BuildInterruptedMessage(nil); len(m.Fields) != 0 {
		t.Errorf("没有中断记录时不该产生明细，得到 %d 行", len(m.Fields))
	}

	single := svc.BuildInterruptedMessage([]*model.BuildRecord{{
		ProjectKey: "demouser", BuildNo: 7, Branch: "master",
		CommitID: "abcdef1234567890", TriggerName: "运维示例",
	}})
	if single.Event != EventBuildInterrupted {
		t.Errorf("事件码必须是 build_interrupted，得到 %q（并进 build_failed 会把"+
			"平台重启说成构建写错了）", single.Event)
	}
	if !strings.Contains(single.Title, "demouser #7") {
		t.Errorf("单条中断时标题要点名是哪一条，得到 %q", single.Title)
	}
	if len(single.Fields) != 1 {
		t.Fatalf("单条中断应只有一行明细，得到 %d 行", len(single.Fields))
	}
	line := single.Fields[0].Value
	for _, want := range []string{"master", "@abcdef1", "运维示例"} {
		if !strings.Contains(line, want) {
			t.Errorf("明细缺少 %q：%q", want, line)
		}
	}

	// 没有提交号（webhook / 定时触发）时不能渲染出一个孤零零的 "@"。
	noCommit := svc.BuildInterruptedMessage([]*model.BuildRecord{{
		ProjectKey: "p", BuildNo: 1, Branch: "master",
	}})
	if v := noCommit.Fields[0].Value; strings.Contains(v, "@") {
		t.Errorf("没有提交号时不该出现 @：%q", v)
	}

	// 超过上限只列前 N 条 + 一句"其余"：队列容量是并发数 ×20，一次重启
	// 理论上能中断几十条，逐条发会把告警群刷屏 —— 而被刷屏的正是该看的那条。
	total := interruptedListLimit + 2
	many := make([]*model.BuildRecord, 0, total)
	for i := 0; i < total; i++ {
		many = append(many, &model.BuildRecord{ProjectKey: "p", BuildNo: uint32(i + 1), Branch: "master"})
	}
	agg := svc.BuildInterruptedMessage(many)
	if !strings.Contains(agg.Title, "共 "+strconv.Itoa(total)+" 个") {
		t.Errorf("多条中断的标题要报总数，得到 %q", agg.Title)
	}
	if len(agg.Fields) != interruptedListLimit+1 {
		t.Fatalf("应当列 %d 行明细 + 1 行其余，得到 %d", interruptedListLimit, len(agg.Fields))
	}
	if last := agg.Fields[len(agg.Fields)-1]; !strings.Contains(last.Value, "另有 2 个") {
		t.Errorf("最后一行要说明还剩多少个，得到 %q", last.Value)
	}
}

// TestSupportedNotifyEventsAreComplete 守住"能勾选 == 后端受理 == 真有触发点"。
//
// 这条测试的价值全在**新增事件码的时候**：往 SupportedNotifyEvents 里加一项，
// 前端选项与后端白名单会自动跟上，但如果忘了接触发点，用户就会勾到一个永远不响的框 ——
// 这正是 build_success / rollback_executed / env_unhealthy 那批码当年的问题。
func TestSupportedNotifyEventsAreComplete(t *testing.T) {
	// ⚠️ 人为维护：新增事件码时**必须**在这里登记，登记就等于声明"确认有代码会 Dispatch 它"。
	triggered := map[NotifyEvent]string{
		EventBuildFailed:          "ci.go finishBuild",
		EventBuildInterrupted:     "ci.go ReconcileInterrupted",
		EventDeployFailed:         "cd.go failDeploy",
		EventDeployInterrupted:    "cd.go ReconcileInterrupted",
		EventDeployPendingApprove: "cd.go CreateDeployment / ExecuteRollback",
	}

	seen := map[string]bool{}
	for _, e := range SupportedNotifyEvents {
		if seen[string(e)] {
			t.Errorf("事件码 %q 重复注册", e)
		}
		seen[string(e)] = true

		if label := eventLabel(e); label == "" || label == string(e) {
			t.Errorf("事件 %q 没有中文标签：界面上会原样显示一个机器码", e)
		}
		if _, ok := triggered[e]; !ok {
			t.Errorf("事件 %q 列进了 SupportedNotifyEvents（前端因此可勾选），"+
				"但没有登记触发点。要么补上触发它的代码，要么把它移出清单。", e)
		}
	}
	for e := range triggered {
		if !IsSupportedNotifyEvent(string(e)) {
			t.Errorf("触发点登记表里的 %q 不在 SupportedNotifyEvents 中："+
				"它会被触发、但用户根本勾不到", e)
		}
	}
}
