package service

import (
	"testing"

	"github.com/2777484478/yunnian/internal/integration"
)

// TestPercentOfNoBaseIsMinusOne 拿不到基准时必须返回 -1，**不是 0**。
//
// 这是本组改动里最容易写错、也最容易误导用户的一条：没有可比的上限/申请量时，
// "占用率"无从谈起。返回 0 会被界面画成一根空条，读起来是"这个服务几乎不占资源"。
func TestPercentOfNoBaseIsMinusOne(t *testing.T) {
	if got := percentOf(451, 0); got != -1 {
		t.Errorf("没有基准（base=0）时应返回 -1，实际 %d —— 0 会被画成“几乎没用”", got)
	}
	// 即使没有用量，也不许把"没有基准"说成 0%
	if got := percentOf(0, 0); got != -1 {
		t.Errorf("没有基准时应返回 -1，实际 %d", got)
	}
}

// TestPercentOfRounds 百分比四舍五入，且允许超过 100（真的超了就该看得见）。
func TestPercentOfRounds(t *testing.T) {
	cases := []struct {
		used, base int64
		want       int
	}{
		{451, 1000, 45},
		{455, 1000, 46},   // 45.5 向上
		{1200, 1000, 120}, // 超限要超出来，不能夹到 100
		{0, 1000, 0},      // 真的用到 0
		{1, 100000, 0},
	}
	for _, c := range cases {
		if got := percentOf(c.used, c.base); got != c.want {
			t.Errorf("percentOf(%d, %d) = %d，期望 %d", c.used, c.base, got, c.want)
		}
	}
}

// TestQuantityParsing CPU 与内存的单位必须各按各的口径解析。
//
// `ParseQuantity("1").MilliValue()` = 1000（1 核 = 1000 毫核），
// 而 `ParseQuantity("1Gi").Value()` = 1073741824（字节）。
// 用错任何一侧就是差 1000 倍或 1024 倍，且**不会报错**。
func TestQuantityParsing(t *testing.T) {
	cpuCases := map[string]int64{"1": 1000, "500m": 500, "2": 2000, "10m": 10, "": 0, "bogus": 0}
	for in, want := range cpuCases {
		if got := cpuQuantityMilli(in); got != want {
			t.Errorf("cpuQuantityMilli(%q) = %d，期望 %d", in, got, want)
		}
	}
	memCases := map[string]int64{"1Gi": 1 << 30, "512Mi": 512 << 20, "10Mi": 10 << 20, "": 0, "bogus": 0}
	for in, want := range memCases {
		if got := memQuantityBytes(in); got != want {
			t.Errorf("memQuantityBytes(%q) = %d，期望 %d", in, got, want)
		}
	}
}

// TestUsageBasePrefersLimit 两者都有时，基准取 limit（那才是危险线）。
func TestUsageBasePrefersLimit(t *testing.T) {
	base, pct := usageRatioCPU(500, "1", "100m")
	if base != "limit" {
		t.Errorf("limit 与 request 都有时应取 limit，实际 %q", base)
	}
	if pct != 50 {
		t.Errorf("500m/1 核 = 50%%，实际 %d", pct)
	}
}

// TestUsageBaseFallsBackToRequest limit 缺失时退回 request，并如实标注基准。
//
// 这条对应实测事实：这台集群里纳管的容器**只配了 requests 没配 limits**
// （`{"requests":{"cpu":"10m","memory":"10Mi"}}`）。若只认 limit，
// 负载列会永远算不出百分比 —— 功能等于没做。
// 但基准必须标出来：超 request 是常态（只是调度层面的申请量），
// 超 limit 才会被限流/OOM，两者不能混成一句"占用率"。
func TestUsageBaseFallsBackToRequest(t *testing.T) {
	base, pct := usageRatioCPU(451, "", "100m")
	if base != "request" {
		t.Errorf("没有 limit 时应退回 request，实际 %q", base)
	}
	if pct != 451 {
		t.Errorf("451m/100m = 451%%，实际 %d（这个数字大是正常的，request 不是上限）", pct)
	}

	mBase, mPct := usageRatioMem(1<<30, "", "100Mi")
	// 1Gi / 100Mi = 10.24 → 1024%（同样是"超过 request 很正常"的数字）
	if mBase != "request" || mPct != 1024 {
		t.Errorf("内存同口径：1Gi/100Mi = 1024%%，实际 %q / %d", mBase, mPct)
	}
}

// TestUsageBaseEmptyWhenNoDeclaration 什么都没声明时：基准为空、百分比 -1。
func TestUsageBaseEmptyWhenNoDeclaration(t *testing.T) {
	if base, pct := usageRatioCPU(451, "", ""); base != "" || pct != -1 {
		t.Errorf("CPU 没声明规格时基准应为空、百分比应为 -1，实际 %q / %d", base, pct)
	}
	if base, pct := usageRatioMem(1<<30, "", ""); base != "" || pct != -1 {
		t.Errorf("内存没声明规格时基准应为空、百分比应为 -1，实际 %q / %d", base, pct)
	}
}

// TestApplyUsageUnavailableKeepsDash 没读到用量时：不给用量、百分比 -1、基准为空。
func TestApplyUsageUnavailableKeepsDash(t *testing.T) {
	d := &ServiceLiveDTO{}
	applyUsage(d, integration.WorkloadUsage{Available: false, Pods: 2}, "10m", "", "10Mi", "")

	if d.MetricsAvailable {
		t.Error("Available=false 时 MetricsAvailable 必须为 false")
	}
	if d.CPUUsage != "" || d.MemUsage != "" {
		t.Errorf("没读到用量时不该填值，实际 %q / %q", d.CPUUsage, d.MemUsage)
	}
	if d.CPUUsagePercent != -1 || d.MemUsagePercent != -1 {
		t.Errorf("没读到用量时百分比应为 -1，实际 %d / %d", d.CPUUsagePercent, d.MemUsagePercent)
	}
	if d.CPUUsageBase != "" || d.MemUsageBase != "" {
		t.Errorf("没读到用量时不该报基准，实际 %q / %q", d.CPUUsageBase, d.MemUsageBase)
	}
}

// TestApplyUsageBaseIsPerResource CPU 与内存的基准**各自独立**判断。
//
// 只配了内存 limit（CPU 只有 request）时：内存按 limit、CPU 按 request。
// 写成"一个没有就两个都没有"会让本来能看的那个也消失。
func TestApplyUsageBaseIsPerResource(t *testing.T) {
	d := &ServiceLiveDTO{}
	applyUsage(d, integration.WorkloadUsage{
		Available: true, Pods: 1, UsagePods: 1,
		CPUMilli: 500, MemBytes: 1 << 30,
		CPU: "500m", Mem: "1.0Gi",
	}, "100m", "", "100Mi", "1Gi")

	if d.MemUsageBase != "limit" || d.MemUsagePercent != 100 {
		t.Errorf("内存应按 limit 算 100%%，实际 %q / %d", d.MemUsageBase, d.MemUsagePercent)
	}
	if d.CPUUsageBase != "request" || d.CPUUsagePercent != 500 {
		t.Errorf("CPU 只有 request 时应按它算 500%%，实际 %q / %d", d.CPUUsageBase, d.CPUUsagePercent)
	}
	if d.CPUUsage != "500m" || d.MemUsage != "1.0Gi" {
		t.Errorf("绝对值照旧要显示，实际 %q / %q", d.CPUUsage, d.MemUsage)
	}
}

// TestApplyUsageNormal 正常情况：绝对值 + 百分比 + 基准 + 两个 Pod 计数都填上。
func TestApplyUsageNormal(t *testing.T) {
	d := &ServiceLiveDTO{}
	applyUsage(d, integration.WorkloadUsage{
		Available: true, Pods: 2, UsagePods: 2,
		CPUMilli: 62, MemBytes: 512 << 20,
		CPU: "62m", Mem: "512Mi",
	}, "100m", "500m", "256Mi", "1Gi")

	if !d.MetricsAvailable {
		t.Error("读到用量时 MetricsAvailable 应为 true")
	}
	if d.CPUUsageBase != "limit" || d.CPUUsagePercent != 12 { // 62/500 = 12.4% → 12
		t.Errorf("CPU 应为 limit 基准 12%%，实际 %q / %d", d.CPUUsageBase, d.CPUUsagePercent)
	}
	if d.MemUsageBase != "limit" || d.MemUsagePercent != 50 {
		t.Errorf("内存应为 limit 基准 50%%，实际 %q / %d", d.MemUsageBase, d.MemUsagePercent)
	}
	if d.UsagePods != 2 || d.UsagePodTotal != 2 {
		t.Errorf("Pod 计数应为 2/2，实际 %d/%d", d.UsagePods, d.UsagePodTotal)
	}
}

// TestLiveOfNeverReturnsZeroPercentOnFailure 没读到集群状态时，百分比也不能是 0。
//
// DTO 的零值恰好是 0%，而 0% 的语义是"用量几乎为零"——
// 与"我们没读到"完全相反。三个失败分支都必须把它们设成 -1。
func TestLiveOfNeverReturnsZeroPercentOnFailure(t *testing.T) {
	dirs := map[string]*ServiceLiveDTO{
		"读不到集群":   liveOf(nil, integration.WorkloadUsage{}, errStub{}),
		"集群返回空":   liveOf(nil, integration.WorkloadUsage{}, nil),
		"工作负载不存在": liveOf(&integration.WorkloadState{Exists: false}, integration.WorkloadUsage{}, nil),
	}
	for name, d := range dirs {
		if d.CPUUsagePercent != -1 || d.MemUsagePercent != -1 {
			t.Errorf("%s：百分比应为 -1（界面显示 —），实际 %d / %d",
				name, d.CPUUsagePercent, d.MemUsagePercent)
		}
	}
}

// TestLiveOfFillsUsage 正常分支要把用量与声明规格都带出去。
func TestLiveOfFillsUsage(t *testing.T) {
	state := &integration.WorkloadState{
		Exists: true, Image: "repo/aioai:1.2.0", Tag: "1.2.0",
		Replicas: 1, ReadyReplicas: 1, ActualReplicas: 1, Status: "running",
		CPURequest: "10m", CPULimit: "1", MemRequest: "10Mi", MemLimit: "2Gi",
	}
	usage := integration.WorkloadUsage{
		Available: true, Pods: 1, UsagePods: 1,
		CPUMilli: 451, MemBytes: 1288490188, CPU: "451m", Mem: "1.2Gi",
	}
	d := liveOf(state, usage, nil)

	if d.CPUUsage != "451m" || d.MemUsage != "1.2Gi" {
		t.Errorf("用量应被带上，实际 %q / %q", d.CPUUsage, d.MemUsage)
	}
	if d.CPUUsageBase != "limit" || d.CPUUsagePercent != 45 {
		t.Errorf("451m/1 核应为 limit 基准 45%%，实际 %q / %d", d.CPUUsageBase, d.CPUUsagePercent)
	}
	if d.CPULimit != "1" || d.MemLimit != "2Gi" {
		t.Errorf("声明规格要照旧带上（界面上与用量分列），实际 %q / %q", d.CPULimit, d.MemLimit)
	}
}

// errStub 一个固定的错误，免得引 errors 只为造一个 error。
type errStub struct{}

func (errStub) Error() string { return "读集群失败" }
