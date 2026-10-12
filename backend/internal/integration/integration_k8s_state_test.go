package integration

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// 期望副本数与**实际存在**的副本数必须分开落进 WorkloadState。
//
// 这不是"多给一个字段"：滚动更新卡住时（新 Pod 反复崩溃，旧 Pod 因 1 副本下
// maxUnavailable 向下取整为 0 而不被杀），集群里实际有 2 个实例、期望仍是 1 个 ——
// 实测 demobasic 就是 `spec=1 / status=2 / ready=1`。少了 ActualReplicas，
// 界面只能按期望渲染出 `1/1`（还带个健康绿），把"有一个实例正在崩溃"整个抹掉；
// 用户在运维面板里看到的是 `1/2`。
func TestDeploymentState_SeparatesDesiredFromActual(t *testing.T) {
	one := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "demobasic", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "master",
					Image: "es-repo.example.com:5000/escloud/demobasic:1.2.0.2761",
				}}},
			},
		},
		Status: appsv1.DeploymentStatus{
			// 集群里真实存在 2 个 Pod：一个崩着的新实例 + 一个健康的老实例
			Replicas:          2,
			ReadyReplicas:     1,
			UpdatedReplicas:   1,
			AvailableReplicas: 1,
		},
	}

	st := deploymentState(dep)
	if st.Replicas != 1 {
		t.Errorf("Replicas 应当是期望数 1，得到 %d", st.Replicas)
	}
	if st.ReadyReplicas != 1 {
		t.Errorf("ReadyReplicas 应当是 1，得到 %d", st.ReadyReplicas)
	}
	if st.ActualReplicas != 2 {
		t.Errorf("ActualReplicas 应当是实际存在的 2，得到 %d —— 界面会渲染成 `1/1`，"+
			"把崩溃的那个实例藏起来", st.ActualReplicas)
	}
	if st.Tag != "1.2.0.2761" {
		t.Errorf("镜像 tag 解析错：%q", st.Tag)
	}
}

// 正常态（实际 == 期望）不能被新字段搞坏：仍然要给出 1/1。
func TestDeploymentState_HealthyStaysOneOfOne(t *testing.T) {
	one := int32(1)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "demouserapi", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "master", Image: "es-repo.example.com:5000/escloud/demouserapi:1.0.9",
				}}},
			},
		},
		Status: appsv1.DeploymentStatus{
			Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1,
		},
	}
	st := deploymentState(dep)
	if st.Replicas != 1 || st.ActualReplicas != 1 || st.ReadyReplicas != 1 {
		t.Errorf("正常态应当是 1/1/1，得到 desired=%d actual=%d ready=%d",
			st.Replicas, st.ActualReplicas, st.ReadyReplicas)
	}
}
