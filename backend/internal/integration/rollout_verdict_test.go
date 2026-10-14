package integration

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// 完成判定必须**同时**看 Deployment 与变更单。
//
// 回归的是 2026-10-14 实测到的那次"报失败但其实部署成功"：
// 首次接管一个已经在跑的服务时，旧版本本来就"全部就绪"，于是在 operator 把新 spec
// 写进 Deployment 之前，只看 Deployment 的写法就会判成"完成"，紧接着发现镜像是旧 tag
// → 报「已滚动完成，但镜像仍是旧 tag」。实际部署单只跑了 305ms，而成功的要跑 1~2 分钟。
func TestJudgeRollout(t *testing.T) {
	const (
		oldImage = "es-repo.example.com:5000/escloud/aiomail:1.2.0.1421"
		newImage = "es-repo.example.com:5000/escloud/aiomail:1.2.0.1422"
		ctr      = "aiomail"
	)

	// dep 造一个"副本全部就绪"的 Deployment（镜像与 observedGeneration 可变）。
	dep := func(image string, replicas, ready int32, observed int64) *appsv1.Deployment {
		observedCopy := observed
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "aiomail", Namespace: "default", Generation: 7},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: ctr, Image: image}}},
				},
			},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration: observedCopy,
				Replicas:           replicas,
				UpdatedReplicas:    ready,
				AvailableReplicas:  ready,
			},
		}
	}

	cases := []struct {
		name string
		dep  *appsv1.Deployment
		cr   *appReleaseView
		want rolloutVerdict
		// wantMsg 非空时校验错误/进度文案里必须出现的片段。
		wantMsg string
	}{
		{
			// ⚠️ 这就是本次的回归点：旧版本就绪 + operator 还没读进我们这次下发。
			name:    "旧版本全部就绪但变更单还没被处理 → 继续等，不许报失败",
			dep:     dep(oldImage, 2, 2, 7),
			cr:      &appReleaseView{Generation: 5, ObservedGeneration: 4},
			want:    verdictAppReleasePending,
			wantMsg: "等待 operator 应用",
		},
		{
			// 读不到变更单（网络/权限/时机）也不能判失败：宁可等到超时，也不能把
			// 一次成功的发布报成"别人改回去了"。
			name: "变更单读不到 + 旧版本就绪 → 继续等",
			dep:  dep(oldImage, 2, 2, 7),
			cr:   nil,
			want: verdictAppReleasePending,
		},
		{
			name:    "变更单已被处理、镜像却还是旧的 → 真失败（有人压回去了）",
			dep:     dep(oldImage, 2, 2, 7),
			cr:      &appReleaseView{Generation: 5, ObservedGeneration: 5},
			want:    verdictReverted,
			wantMsg: "请检查是否有其他管理者在改这个工作负载",
		},
		{
			name: "期望镜像已在 spec 且副本齐全 → 完成",
			dep:  dep(newImage, 2, 2, 7),
			cr:   &appReleaseView{Generation: 5, ObservedGeneration: 4},
			want: verdictComplete,
		},
		{
			name: "期望镜像已在 spec 但副本还没起来 → 继续等",
			dep:  dep(newImage, 2, 0, 7),
			cr:   &appReleaseView{Generation: 5, ObservedGeneration: 5},
			want: verdictWaiting,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, msg, err := judgeRollout(c.dep, c.cr, newImage, ctr)
			if err != nil {
				t.Fatalf("不该报错：%v", err)
			}
			if got != c.want {
				t.Fatalf("判定错了：got=%d want=%d（msg=%q）", got, c.want, msg)
			}
			if c.wantMsg != "" && !strings.Contains(msg, c.wantMsg) {
				t.Fatalf("文案里应含 %q，实际 %q", c.wantMsg, msg)
			}
			// 失败文案必须同时给出"看到的"与"期望的"，否则用户无从判断该查哪里。
			if c.want == verdictReverted && (!strings.Contains(msg, oldImage) || !strings.Contains(msg, newImage)) {
				t.Fatalf("失败文案缺少 实际/期望 镜像：%q", msg)
			}
		})
	}
}

// 容器名对不上时不能"判成等待"：那会一直等到超时，把一个配置错误报成网络问题。
func TestJudgeRolloutContainerMissing(t *testing.T) {
	replicas := int32(1)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "aiomail", Namespace: "default", Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "x:1"}}},
			},
		},
	}
	if _, _, err := judgeRollout(d, nil, "x:2", "not-there"); err == nil {
		t.Fatal("容器名不存在时必须报错")
	}
}
