package service

import (
	"strings"
	"testing"
	"time"

	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/model"
)

func baseInspectData() inspectData {
	d := inspectData{
		Env:    &model.Environment{EnvKey: "test-152", Name: "机房测试环境152", Namespace: "default", Cluster: "c1"},
		Window: 168,
		Now:    time.Now(),
	}
	d.Svcs = []model.ServiceInstance{
		{ProjectKey: "svc-ok", Status: "running", Replicas: 1, ReadyReplicas: 1},
	}
	d.Nodes = &integration.ClusterState{
		Nodes: 1, MetricsAvailable: true,
		NodeList: []integration.NodeStat{
			{Name: "node152", Ready: true, HasMetrics: true,
				CPUCores: 8, CPUUsage: 0.5, MemBytes: 100 * 1024 * 1024 * 1024, MemUsage: 30 * 1024 * 1024 * 1024},
		},
	}
	return d
}

// TestScoreInspection_Healthy 全部正常时应当是满分、无 critical/warning。
func TestScoreInspection_Healthy(t *testing.T) {
	score, level, findings := scoreInspection(baseInspectData())
	if score != 100 || level != "healthy" {
		t.Fatalf("应当满分健康，得到 %d/%s", score, level)
	}
	for _, f := range findings {
		if f.Level != "info" {
			t.Errorf("健康数据不该出现 %s 级发现：%s", f.Level, f.Title)
		}
	}
}

// TestScoreInspection_NotReadyService 未就绪服务每个扣 15，最多扣 45。
func TestScoreInspection_NotReadyService(t *testing.T) {
	d := baseInspectData()
	d.NotReady = []model.ServiceInstance{
		{ProjectKey: "a", Status: "pending", Replicas: 1, ReadyReplicas: 0},
	}
	score, _, findings := scoreInspection(d)
	if score != 85 {
		t.Errorf("1 个未就绪应扣 15 分 → 85，实际 %d", score)
	}
	found := false
	for _, f := range findings {
		if f.Kind == "service_not_ready" {
			found = true
			if f.Penalty != 15 || f.Level != "critical" {
				t.Errorf("扣分/等级不对：penalty=%d level=%s", f.Penalty, f.Level)
			}
		}
	}
	if !found {
		t.Fatal("缺少 service_not_ready 这条发现 —— 分数变了但没说为什么")
	}

	// 5 个未就绪：扣分封顶 45
	d.NotReady = make([]model.ServiceInstance, 5)
	for i := range d.NotReady {
		d.NotReady[i] = model.ServiceInstance{ProjectKey: "s", Status: "pending", Replicas: 1}
	}
	if s, _, _ := scoreInspection(d); s != 55 {
		t.Errorf("5 个未就绪应封顶扣 45 → 55，实际 %d", s)
	}
}

// TestScoreInspection_StatusStale 副本齐但状态非 running：扣分要轻（不是故障）。
func TestScoreInspection_StatusStale(t *testing.T) {
	d := baseInspectData()
	d.Degraded = []model.ServiceInstance{
		{ProjectKey: "aiobasic", Status: "error", Replicas: 1, ReadyReplicas: 1},
	}
	score, _, findings := scoreInspection(d)
	if score != 95 {
		t.Errorf("记录未刷新应扣 5 → 95，实际 %d", score)
	}
	for _, f := range findings {
		if f.Kind == "status_stale" && f.Level != "warning" {
			t.Errorf("这类应是 warning 而不是 %s —— 报成故障会让人白跑一趟", f.Level)
		}
	}
}

// TestScoreInspection_NodeMemoryUsesPercent 节点内存必须按百分比判读。
//
// 这条是防回归的关键：NodeStat.MemUsage 是**字节**，直接拿去和 75 比
// 会把 30GB 当成 30% 用过 —— 那正是"集群看着很闲其实快满了"的来源。
func TestScoreInspection_NodeMemoryUsesPercent(t *testing.T) {
	d := baseInspectData()
	// 100GB 用 96GB = 96%
	d.Nodes.NodeList[0].MemUsage = 96 * 1024 * 1024 * 1024
	score, level, findings := scoreInspection(d)
	if score != 85 {
		t.Errorf("内存 96%% 应扣 15 → 85，实际 %d（若按字节比较则不会触发）", score)
	}
	if level != "warning" && level != "critical" {
		t.Errorf("内存 96%% 不可能是 %s", level)
	}
	found := false
	for _, f := range findings {
		if f.Kind == "node_memory" {
			found = true
			if !strings.Contains(f.Title, "96") {
				t.Errorf("标题应带真实百分比，实际 %q", f.Title)
			}
		}
	}
	if !found {
		t.Fatal("缺少 node_memory 发现")
	}

	// 30% 不该报
	d2 := baseInspectData()
	if _, _, fs := scoreInspection(d2); len(fs) != 0 {
		t.Errorf("内存 30%% 不该有任何发现，得到 %+v", fs)
	}
}

// TestScoreInspection_CertExpiring 证书即将到期要扣分并给出提醒。
func TestScoreInspection_CertExpiring(t *testing.T) {
	d := baseInspectData()
	now := time.Now()
	d.Certs = []integration.TLSCertInfo{
		{Secret: "tls-a", Hosts: []string{"a.example.com"}, NotAfter: now.Add(72 * time.Hour), DaysLeft: 3},
	}
	score, _, _ := scoreInspection(d)
	if score != 85 {
		t.Errorf("证书 3 天后到期应扣 15 → 85，实际 %d", score)
	}
	if certAlertLine(d) == "" {
		t.Error("快到期时置顶提醒不能为空 —— 用户要求「快到期要提醒」")
	}

	d.Certs[0].DaysLeft = -2
	score2, _, _ := scoreInspection(d)
	if score2 != 70 {
		t.Errorf("已过期应扣 30 → 70，实际 %d", score2)
	}
}

// TestScoreInspection_UnreadableCertIsReported 读不出来的证书要单独报，不能当"没有"。
func TestScoreInspection_UnreadableCertIsReported(t *testing.T) {
	d := baseInspectData()
	d.Certs = []integration.TLSCertInfo{{Secret: "tls-bad", Ingresses: []string{"web"}, Err: "Secret 不存在"}}
	_, _, findings := scoreInspection(d)
	found := false
	for _, f := range findings {
		if f.Kind == "cert_unreadable" {
			found = true
		}
	}
	if !found {
		t.Fatal("读不到证书必须单独报出来，否则会被读成「没有证书」")
	}
}

// TestRenderInspectionHTML_SelfContained 报告要能直接下载打开：不引外部资源。
func TestRenderInspectionHTML_SelfContained(t *testing.T) {
	d := baseInspectData()
	score, level, findings := scoreInspection(d)
	st := inspectStats(d)
	doc := renderInspectionHTML(d, score, level, "未发现异常", findings, st, Caller{})

	for _, bad := range []string{"http://", "https://", "<link", "<script src"} {
		if strings.Contains(doc, bad) {
			t.Errorf("报告里出现了外部依赖 %q —— 下载后断网就打不开了", bad)
		}
	}
	if !strings.Contains(doc, "<style>") {
		t.Error("缺少内联样式，报告会变成白底黑字")
	}
	// 未读到集群时要显式说明，不能渲染成 0%
	d2 := baseInspectData()
	d2.Nodes, d2.NodesErr = nil, "集群不可达"
	doc2 := renderInspectionHTML(d2, 100, "healthy", "未发现异常", nil, inspectStats(d2), Caller{})
	if !strings.Contains(doc2, "没读到集群资源") {
		t.Error("集群读不到时必须显式说明，不能渲染成 0%")
	}
}

// TestInspectSummary 一句话结论要能说清"几项、第一项是什么"。
func TestInspectSummary(t *testing.T) {
	s := inspectSummary([]InspectFinding{
		{Level: "critical", Title: "1 个服务未就绪"},
		{Level: "warning", Title: "证书 20 天后到期"},
	}, 62, "critical")
	if !strings.Contains(s, "1 项需处理") || !strings.Contains(s, "1 个服务未就绪") {
		t.Errorf("结论不对：%q", s)
	}
	if s := inspectSummary(nil, 100, "healthy"); s != "未发现异常" {
		t.Errorf("无发现时应说「未发现异常」，实际 %q", s)
	}
}

func TestTrimRunes_NoBrokenChinese(t *testing.T) {
	in := strings.Repeat("测试", 100)
	out := trimRunes(in, 5)
	if !strings.HasPrefix(out, "测试测试测") {
		t.Errorf("截断破坏了多字节字符：%q", out)
	}
}
