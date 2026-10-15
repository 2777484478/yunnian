package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/config"
	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/logstream"
	"github.com/2777484478/yunnian/internal/model"
)

// 「发布成功」通知的投递链路（2026-10-15）。
//
// 与 `notify_success_test.go` 的分工：那边测**消息长得对不对**（字段、级别、渲染），
// 这里测**它到底发出去了没有、发给了谁**。
//
// 为什么这两件事必须分开测：通知最容易"看起来做好了"—— 代码跑通、接口 200、
// 日志没错误，但事件码写错一个字母、或者渠道根本没订阅上，结果依然是静默。
// 所以这里既断言"发了什么"，也断言"**没发给未订阅的渠道**"（用桩路径做反证）。

// waitDeliveries 等「桩收到 n 次请求 **且** last_sent_at 已回写」两件事同时成立。
//
// ⚠️ 不能只等请求数：发送器读到响应之后才写 last_sent_at，而桩在**处理请求时**
// 就记下了 —— 只等前者会在回写之前就往下断言，表现为"单独跑绿、全量跑红"。
// 这是 `notify_interrupted_test.go` 里已经踩过并记下来的坑，照抄同一套等法。
func waitDeliveries(t *testing.T, db *gorm.DB, stub *stubDingtalk, chID uint64, n int) ([]string, []string) {
	t.Helper()
	ok := waitFor(func() bool {
		if paths, _ := stub.snapshot(); len(paths) < n {
			return false
		}
		var c model.NotifyChannel
		if err := db.First(&c, chID).Error; err != nil {
			return false
		}
		return c.LastSentAt != nil
	}, 5*time.Second)

	paths, bodies := stub.snapshot()
	if !ok {
		var c model.NotifyChannel
		_ = db.First(&c, chID).Error
		t.Fatalf("等满 5s 仍未完成「投递 + 回写」：收到 %d 次投递，lastSentAt=%v failCount=%d lastError=%q",
			len(paths), c.LastSentAt, c.FailCount, c.LastError)
	}
	return paths, bodies
}

// newNotifyTestSvc 搭一个带测试库与桩投递的服务集合。
func newNotifyTestSvc(t *testing.T, db *gorm.DB) *Service {
	t.Helper()
	cfg := config.Default()
	cfg.Notify.PublicBaseURL = "http://127.0.0.1:8080"
	return New(Deps{
		Cfg: cfg, DB: db, Log: discardLogger(),
		Stream: logstream.NewMemStream(), Up: &integration.Set{},
	})
}

// TestDeploySucceededDispatchesOnlyToSubscribers 部署成功 / 回滚成功 → 只投给订阅者。
func TestDeploySucceededDispatchesOnlyToSubscribers(t *testing.T) {
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
	// 订阅成功的渠道，和"只订了失败"的渠道。两个都指向同一个桩，
	// 靠路径区分 —— 这正是订阅过滤的反证。
	subscribed := mk("运维群（收成功回执）", "/sub", `["deploy_succeeded"]`)
	other := mk("告警群（只收失败）", "/other", `["deploy_failed"]`)
	for _, c := range []*model.NotifyChannel{&subscribed, &other} {
		if err := db.Create(c).Error; err != nil {
			t.Fatalf("写入通知渠道失败: %v", err)
		}
	}

	dur := uint32(117)
	finished := time.Now()
	// 记录必须是**成功终态**：调用点就在终态落库之后（见 cd.go notifyDeploySucceeded），
	// 测试里直接构造终态，等价于"那个 tx 刚刚提交完"。
	normal := model.DeployRecord{
		DeployNo: "DP202610150001", EnvID: 1, EnvKey: "prod",
		ProjectID: 1, ProjectKey: "demo-web", ServiceID: 1,
		Image: "harbor.internal/demo-web:1.2.3", Replicas: 3,
		Status: "success", OperatorName: "运维示例",
		DurationSec: &dur, FinishedAt: &finished, UpdatedAt: finished,
	}
	if err := db.Create(&normal).Error; err != nil {
		t.Fatalf("写入部署记录失败: %v", err)
	}
	// 回滚产生的部署单：终态是 rolled_back，标题要写成「回滚成功」。
	parent := uint64(99)
	rollback := model.DeployRecord{
		DeployNo: "DP202610150002", EnvID: 1, EnvKey: "prod",
		ProjectID: 1, ProjectKey: "demo-web", ServiceID: 1,
		Image: "harbor.internal/demo-web:1.1.0", Replicas: 1,
		Status: "rolled_back", OperatorName: "运维示例", ParentDeploy: &parent,
		UpdatedAt: finished,
	}
	if err := db.Create(&rollback).Error; err != nil {
		t.Fatalf("写入回滚部署记录失败: %v", err)
	}

	svc := newNotifyTestSvc(t, db)
	ctx := context.Background()
	svc.CD.notifyDeploySucceeded(ctx, normal.ID)
	svc.CD.notifyDeploySucceeded(ctx, rollback.ID)

	paths, bodies := waitDeliveries(t, db, stub, subscribed.ID, 2)

	if len(paths) != 2 {
		t.Errorf("期望恰好 2 次投递（部署成功 + 回滚成功各一条），实际 %d 次：%v", len(paths), paths)
	}
	for _, p := range paths {
		if p != "/sub" {
			t.Errorf("投递打到了**未订阅**该项的渠道：%s（订阅过滤失效，"+
				"后果是只订了 deploy_failed 的告警群被每次发布的成功回执刷屏）", p)
		}
	}

	joined := strings.Join(bodies, "\n---\n")
	for _, want := range []string{
		"部署成功", "prod/demo-web", "harbor.internal/demo-web:1.2.3", "3 个", "1m57s",
		"回滚成功", "harbor.internal/demo-web:1.1.0",
		"新版本已发布成功", // Reason：明说"已就绪"，而不是只说流程结束
		"✅",        // 级别符号
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("投递内容里缺少 %q；实际内容：\n%s", want, joined)
		}
	}
	// 成功消息的表头必须是「说明」而不是「原因」——后者会被读成"为什么会成功"。
	if strings.Contains(joined, "**原因**") {
		t.Errorf("成功通知不该出现「原因」表头：\n%s", joined)
	}
}

// TestBuildSucceededDispatchesWithImage 构建成功 → 通知里带上**镜像地址**。
//
// 这条专门钉住那个时序坑：镜像地址写在 image_record 上，而 build_record 只有一个
// 外键指向它。而两条构建路径（runner / Jenkins）**都是在 finishBuild 之后**才把
// image_record_id 挂上去的 —— 通知若放在 finishBuild 里，那一行永远是空的，
// 而且不会报任何错。所以这里必须真的建出 image_record 再断言。
func TestBuildSucceededDispatchesWithImage(t *testing.T) {
	db := newTestDB(t)

	stub := &stubDingtalk{}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	t.Cleanup(srv.Close)

	ch := model.NotifyChannel{
		Name: "运维群", Type: "dingtalk",
		Target: srv.URL + "/sub?access_token=tk",
		Events: model.JSONText(`["build_succeeded"]`), Enabled: true,
	}
	if err := db.Create(&ch).Error; err != nil {
		t.Fatalf("写入通知渠道失败: %v", err)
	}

	dur := uint32(42)
	build := model.BuildRecord{
		BuildNo: 42, ProjectID: 1, ProjectKey: "demo-web", Branch: "release/1.2",
		CommitID: "abcdef1234567890", TriggerName: "运维示例",
		Status: "success", Stage: "已完成", Progress: 100,
		DurationSec: &dur, UpdatedAt: time.Now(),
	}
	if err := db.Create(&build).Error; err != nil {
		t.Fatalf("写入构建记录失败: %v", err)
	}
	// 镜像登记 + 回写外键。顺序与真实收尾一致（先建 image_record，再挂到构建上）。
	img := model.ImageRecord{
		ProjectID: 1, ProjectKey: "demo-web",
		Registry: "harbor.internal", Repo: "escloud/demo-web", Tag: "2.2.0.1009",
		Image:   "harbor.internal/escloud/demo-web:2.2.0.1009",
		BuildID: build.ID, BuildNo: 42, DeployedEnv: model.JSONText(`[]`),
	}
	if err := db.Create(&img).Error; err != nil {
		t.Fatalf("写入镜像记录失败: %v", err)
	}
	if err := db.Model(&model.BuildRecord{}).Where("id = ?", build.ID).
		Update("image_record_id", img.ID).Error; err != nil {
		t.Fatalf("回写镜像记录外键失败: %v", err)
	}

	svc := newNotifyTestSvc(t, db)
	svc.CI.notifyBuildSucceeded(context.Background(), build.ID)

	paths, bodies := waitDeliveries(t, db, stub, ch.ID, 1)
	if len(paths) != 1 {
		t.Errorf("期望 1 次投递，实际 %d 次：%v", len(paths), paths)
	}
	body := strings.Join(bodies, "\n")
	for _, want := range []string{
		"构建成功", "demo-web #42", "42s",
		// 这一条是重点：镜像地址必须从 image_record 回查出来。
		"harbor.internal/escloud/demo-web:2.2.0.1009",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("投递内容里缺少 %q；实际内容：\n%s", want, body)
		}
	}
}

// TestBuildSucceededStillNotifiesWithoutImage 「只编译不推镜像」的构建**照样通知**。
//
// Jenkins Job 里本来就有这种（平台读不到产物不等于构建失败）。这条通知的意义是
// "这轮发版结束了"，不该因为少一个字段就整条不发。
func TestBuildSucceededStillNotifiesWithoutImage(t *testing.T) {
	db := newTestDB(t)

	stub := &stubDingtalk{}
	srv := httptest.NewServer(http.HandlerFunc(stub.handler))
	t.Cleanup(srv.Close)

	ch := model.NotifyChannel{
		Name: "运维群", Type: "dingtalk",
		Target: srv.URL + "/sub?access_token=tk",
		Events: model.JSONText(`["build_succeeded"]`), Enabled: true,
	}
	if err := db.Create(&ch).Error; err != nil {
		t.Fatalf("写入通知渠道失败: %v", err)
	}

	build := model.BuildRecord{
		BuildNo: 3, ProjectID: 1, ProjectKey: "lib-common",
		Status: "success", Stage: "已完成", UpdatedAt: time.Now(),
	}
	if err := db.Create(&build).Error; err != nil {
		t.Fatalf("写入构建记录失败: %v", err)
	}

	svc := newNotifyTestSvc(t, db)
	svc.CI.notifyBuildSucceeded(context.Background(), build.ID)

	_, bodies := waitDeliveries(t, db, stub, ch.ID, 1)
	body := strings.Join(bodies, "\n")
	if !strings.Contains(body, "构建成功") || !strings.Contains(body, "lib-common #3") {
		t.Errorf("没有镜像也该发出「构建成功」通知；实际内容：\n%s", body)
	}
	// 空字段由渲染层跳过，不该出现一行空的「镜像」。
	if strings.Contains(body, "**镜像**：\n") || strings.Contains(body, "**镜像**：\\n") {
		t.Errorf("空镜像字段没有被跳过：\n%s", body)
	}
}
