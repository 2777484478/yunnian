package integration

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// 节点级明细里两个"错了也不报错"的判定：Ready 与角色。
//
// 为什么值得单独钉：
//   - Ready 判反了，界面上会出现"NotReady"的假警报 —— 运维会去查一台其实健康的机器；
//     反过来（把 Unknown 当 Ready）则会把一个真问题藏起来。
//   - 角色推错了会显示成一个集群里并不存在的角色名，而它看起来和真标签一模一样。
//
// 两个函数都是纯函数，所以这里穷举边界而不是靠 mock。

func TestNodeReady(t *testing.T) {
	cond := func(typ corev1.NodeConditionType, st corev1.ConditionStatus) corev1.NodeCondition {
		return corev1.NodeCondition{Type: typ, Status: st}
	}
	cases := []struct {
		name  string
		conds []corev1.NodeCondition
		want  bool
	}{
		{"Ready=True", []corev1.NodeCondition{cond(corev1.NodeReady, corev1.ConditionTrue)}, true},
		{"Ready=False", []corev1.NodeCondition{cond(corev1.NodeReady, corev1.ConditionFalse)}, false},
		// Unknown（kubelet 失联）不是就绪：这正是最该被看见的一种状态。
		{"Ready=Unknown", []corev1.NodeCondition{cond(corev1.NodeReady, corev1.ConditionUnknown)}, false},
		{"只有别的 condition", []corev1.NodeCondition{cond(corev1.NodeMemoryPressure, corev1.ConditionTrue)}, false},
		{"没有任何 condition", nil, false},
	}
	for _, c := range cases {
		n := corev1.Node{Status: corev1.NodeStatus{Conditions: c.conds}}
		if got := nodeReady(n); got != c.want {
			t.Errorf("%s: nodeReady = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestNodeRoles(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"控制面", map[string]string{"node-role.kubernetes.io/control-plane": ""}, "control-plane"},
		// 老集群用的是 master，两代都要认。
		{"老版本 master", map[string]string{"node-role.kubernetes.io/master": ""}, "master"},
		{"多个角色按字典序，输出稳定", map[string]string{
			"node-role.kubernetes.io/worker":  "",
			"node-role.kubernetes.io/ingress": "",
		}, "ingress,worker"},
		// 普通工作节点本来就没有这类标签 —— 编一个 "worker" 会让人以为它是登记过的角色。
		{"普通节点没有角色标签", map[string]string{"kubernetes.io/hostname": "n1"}, ""},
		{"空后缀不算角色", map[string]string{"node-role.kubernetes.io/": ""}, ""},
		{"没有标签", nil, ""},
	}
	for _, c := range cases {
		n := corev1.Node{}
		n.Labels = c.labels
		if got := nodeRoles(n); got != c.want {
			t.Errorf("%s: nodeRoles = %q，期望 %q", c.name, got, c.want)
		}
	}
}
