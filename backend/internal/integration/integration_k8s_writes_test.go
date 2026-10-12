package integration

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// 卡住的 rollout 提前判失败，判据只有一条：Deployment 自己置的
// `Progressing=False / reason=ProgressDeadlineExceeded`。
//
// 判据窄是有意的 —— Progressing=False 还有别的 reason（例如 Deployment 被 pause），
// 语义完全不同。放进来会把"暂停"报成"失败"，而失败的后续动作是回滚/换版本，
// 等于把一个只是被暂停的负载当成坏的推着人去处理。
func TestRolloutDeadlineExceeded(t *testing.T) {
	cases := []struct {
		name    string
		conds   []appsv1.DeploymentCondition
		wantOK  bool
		wantHas string
	}{
		{
			name: "集群判定无进展 → 提前失败",
			conds: []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
				Reason:  "ProgressDeadlineExceeded",
				Message: `ReplicaSet "demobasic-59b46b9bfb" has timed out progressing.`,
			}},
			wantOK:  true,
			wantHas: "demobasic-59b46b9bfb",
		},
		{
			name: "刚滚完（NewReplicaSetAvailable）→ 不算失败",
			conds: []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue,
				Reason: "NewReplicaSetAvailable",
			}},
			wantOK: false,
		},
		{
			name: "被 pause（reason 不同）→ 不算失败",
			conds: []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
				Reason: "DeploymentPaused",
			}},
			wantOK: false,
		},
		{
			name:   "没有 conditions → 不算失败",
			conds:  nil,
			wantOK: false,
		},
		{
			name: "message 为空时也要给出一句可读的原因",
			conds: []appsv1.DeploymentCondition{{
				Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse,
				Reason: "ProgressDeadlineExceeded",
			}},
			wantOK:  true,
			wantHas: "没有就绪",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dep := &appsv1.Deployment{Status: appsv1.DeploymentStatus{Conditions: tc.conds}}
			msg, ok := rolloutDeadlineExceeded(dep)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v，期望 %v（msg=%q）", ok, tc.wantOK, msg)
			}
			if tc.wantHas != "" && !strings.Contains(msg, tc.wantHas) {
				t.Errorf("原因里缺 %q：%q", tc.wantHas, msg)
			}
		})
	}
}

func TestProgressDeadlineSecondsOf(t *testing.T) {
	// 未显式设置时 k8s 默认 600s。显示成 "0s" 会让人以为"立即超时"，
	// 而真实语义是"10 分钟没进展才判死"。
	if got := progressDeadlineSecondsOf(&appsv1.Deployment{}); got != "600s（k8s 默认值）" {
		t.Errorf("未设置时 = %q", got)
	}

	seconds := int32(300)
	set := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{ProgressDeadlineSeconds: &seconds}}
	if got := progressDeadlineSecondsOf(set); got != "300s" {
		t.Errorf("显式设置时 = %q", got)
	}
}
