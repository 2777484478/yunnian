package integration

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// dep 造一个带 app 标签选择器的 Deployment。
func dep(name, appLabel string) appsv1.Deployment {
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": appLabel}},
		},
	}
}

func pod(name, appLabel string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"app": appLabel}},
	}
}

// TestAggregateUsageKeepsWorkloadsApart 两个工作负载各自的 Pod 用量不能串。
//
// 这是本函数唯一真正要保证的事：归属错了**不会报错**，
// 只会让 A 的用量记在 B 头上 —— 两边看起来都"合理"，谁也发现不了。
func TestAggregateUsageKeepsWorkloadsApart(t *testing.T) {
	deps := []appsv1.Deployment{dep("aioai", "aioai"), dep("aiobasic", "aiobasic")}
	pods := []corev1.Pod{pod("aioai-1", "aioai"), pod("aiobasic-1", "aiobasic")}
	usage := map[string]podUsageEntry{
		"aioai-1":    {cpuMilli: 451, memBytes: 1288490188},
		"aiobasic-1": {cpuMilli: 31, memBytes: 102760448},
	}
	got := aggregateUsage(deps, pods, usage, map[string]struct{}{"aioai": {}, "aiobasic": {}}, true)

	if got["aioai"].CPUMilli != 451 || got["aioai"].Pods != 1 {
		t.Errorf("aioai 应拿到自己的 451m/1 Pod，实际 %dm/%d Pod", got["aioai"].CPUMilli, got["aioai"].Pods)
	}
	if got["aiobasic"].CPUMilli != 31 || got["aiobasic"].Pods != 1 {
		t.Errorf("aiobasic 应拿到自己的 31m/1 Pod，实际 %dm/%d Pod", got["aiobasic"].CPUMilli, got["aiobasic"].Pods)
	}
	if got["aioai"].CPU == got["aiobasic"].CPU {
		t.Errorf("两个工作负载的用量被算成了同一个值 %q —— 十有八九是归属写错了", got["aioai"].CPU)
	}
}

// TestAggregateUsageOnlyReturnsNamedWorkloads 白名单外的对象不许出现在结果里。
//
// 这一条守的是平台的一贯纪律：命名空间里躺着几十个与本平台无关的 Deployment，
// 一旦它们进了结果，"服务"的集合就会被集群里的杂物撑大（那不是发现，是误报）。
func TestAggregateUsageOnlyReturnsNamedWorkloads(t *testing.T) {
	deps := []appsv1.Deployment{
		dep("aioai", "aioai"),
		dep("some-other-team-app", "some-other-team-app"),
		dep("another-legacy-thing", "another-legacy-thing"),
	}
	pods := []corev1.Pod{pod("some-other-team-app-1", "some-other-team-app")}
	got := aggregateUsage(deps, pods, map[string]podUsageEntry{}, map[string]struct{}{"aioai": {}}, true)

	if len(got) != 1 {
		t.Fatalf("只点名了 aioai，结果却回来了 %d 个：%v", len(got), got)
	}
	if _, ok := got["some-other-team-app"]; ok {
		t.Error("白名单外的工作负载出现在结果里 —— 服务列表会被集群里的杂物撑大")
	}
	if got["aioai"].Pods != 0 {
		t.Errorf("aioai 在自己没有 Pod 时 Pods 应为 0，实际 %d", got["aioai"].Pods)
	}
}

// TestAggregateUsageCountsPodsWithoutMetrics 有 Pod 但还没用量数据的时候，
// **Pod 数要照实算**，用量留空。
//
// 刚拉起来的 Pod 在 metrics-server 里要等几十秒才有数据，这时如果连 Pod 都不计，
// 界面就会显示"无 Pod"——而集群里明明有。两者必须分开表达。
func TestAggregateUsageCountsPodsWithoutMetrics(t *testing.T) {
	deps := []appsv1.Deployment{dep("aioai", "aioai")}
	pods := []corev1.Pod{pod("aioai-1", "aioai"), pod("aioai-2", "aioai")}
	// 只有第一个有指标
	usage := map[string]podUsageEntry{"aioai-1": {cpuMilli: 100, memBytes: 1048576}}

	got := aggregateUsage(deps, pods, usage, map[string]struct{}{"aioai": {}}, true)["aioai"]

	if got.Pods != 2 {
		t.Errorf("匹配到的 Pod 数应为 2（含还没指标的那个），实际 %d", got.Pods)
	}
	if got.UsagePods != 1 {
		t.Errorf("有指标的 Pod 数应为 1，实际 %d", got.UsagePods)
	}
	if got.CPUMilli != 100 {
		t.Errorf("合计应只累加有指标的 Pod，期望 100m，实际 %dm", got.CPUMilli)
	}
}

// TestAggregateUsageMarksUnavailable 读不到指标时 Available 必须是 false。
//
// 这是"没读到"与"用量为 0"的分界线：上层靠它决定显示 "—" 还是 0。
// 一旦这里返回 true，界面就会把"集群没装 metrics-server"显示成
// "所有服务都不占资源"——正好把用户引向相反的结论。
func TestAggregateUsageMarksUnavailable(t *testing.T) {
	deps := []appsv1.Deployment{dep("aioai", "aioai")}
	pods := []corev1.Pod{pod("aioai-1", "aioai")}

	got := aggregateUsage(deps, pods, map[string]podUsageEntry{}, map[string]struct{}{"aioai": {}}, false)["aioai"]

	if got.Available {
		t.Error("没读到指标时 Available 必须为 false —— 否则界面会把“读不到”画成“用量为 0”")
	}
	if got.CPUMilli != 0 || got.MemBytes != 0 {
		t.Errorf("没读到指标时数值应保持 0，实际 %d/%d", got.CPUMilli, got.MemBytes)
	}
	if got.Pods != 1 {
		t.Errorf("Pod 数来自 Pod 列表，与有没有指标无关，应为 1，实际 %d", got.Pods)
	}
}

// TestAggregateUsageSkipsWorkloadWithoutSelector 选择器缺失的畸形对象不许瞎猜归属。
func TestAggregateUsageSkipsWorkloadWithoutSelector(t *testing.T) {
	broken := appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "broken"}}
	pods := []corev1.Pod{pod("broken-1", "broken")}
	usage := map[string]podUsageEntry{"broken-1": {cpuMilli: 500}}

	got := aggregateUsage([]appsv1.Deployment{broken}, pods, usage, map[string]struct{}{"broken": {}}, true)["broken"]

	if got.Pods != 0 || got.CPUMilli != 0 {
		t.Errorf("没有 selector 就不该猜出归属，实际 %d Pod / %dm", got.Pods, got.CPUMilli)
	}
}

// TestAggregateUsageEmptyUsageShowsDash CPU 用量为 0 时显示成 "0" 而不是空串。
//
// formatCPUMilli(0) == "0"：这是"真的没用到"，与"没读到"（Available=false）是两回事。
func TestAggregateUsageEmptyUsageShowsDash(t *testing.T) {
	deps := []appsv1.Deployment{dep("aioai", "aioai")}
	pods := []corev1.Pod{pod("aioai-1", "aioai")}
	usage := map[string]podUsageEntry{"aioai-1": {cpuMilli: 0, memBytes: 0}}

	got := aggregateUsage(deps, pods, usage, map[string]struct{}{"aioai": {}}, true)["aioai"]

	if got.CPU != "0" {
		t.Errorf("CPU 真的用到 0 时应显示 0，实际 %q", got.CPU)
	}
	if !got.Available {
		t.Error("读到了指标（哪怕值是 0），Available 应为 true")
	}
}
