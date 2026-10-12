//go:build live

// 集群写通道的真实集成测试。
//
// 默认**不参与构建**：它会真的往集群里写东西，只有显式带 -tags live 才会编译进来。
//
// 运行方式：
//
//	cd backend
//	YN_LIVE_KUBECONFIG=$HOME/.kube/config-192 \
//	go test -tags live -v -timeout 20m ./internal/integration/ -run TestLiveAppReleaseWrite
//
// 可选环境变量：
//
//	YN_LIVE_CLUSTER_KEY  集群 key（默认 live-cluster）
//	YN_LIVE_NAMESPACE    namespace（默认 default）
//	YN_LIVE_SANDBOX      已由 operator 接管、可安全折腾的工作负载（默认 nginx-demo）
//	YN_LIVE_PROBE        测试自己创建/销毁的临时工作负载名（默认 yn-write-probe）
//
// 三条安全边界，务必保持：
//
//  1. **只动 YN_LIVE_SANDBOX 与 YN_LIVE_PROBE**。前者是 operator 自带的演示工作负载
//     （nginx-demo，20 天前就停在 nginx:1.25）；后者由测试创建、测试结束时删除。
//     集群里那 47 个业务 Deployment 一个都不能碰。
//  2. 每个会改状态的小节结束时都把状态**还原**（副本数、镜像），
//     避免留下"跑过一次测试就多 1 个副本"这种说不清的现场。
//  3. **不依赖外网**。本机到 Docker Hub / gcr.io 不通（代理未开时 registry-1.docker.io
//     直接解析失败），所以"换镜像"一节用的是当前镜像的**等价写法**（见 equivalentImageRef），
//     不拉取任何新内容；换一个真的新版本需要先确认代理已开启。
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/yunnian/yunnian/internal/config"
)

// --------------------------------------------------------------------------- 夹具

type liveEnv struct {
	cluster    *K8sCluster
	kubeconfig string
	namespace  string
	sandbox    string
	probe      string
}

func liveFixture(t *testing.T) *liveEnv {
	t.Helper()
	kc := envOr("YN_LIVE_KUBECONFIG", os.Getenv("HOME")+"/.kube/config-192")
	if _, err := os.Stat(kc); err != nil {
		t.Skipf("没有可用的 kubeconfig（%s），跳过真实集群测试", kc)
	}

	// 用 config.yaml 里的静态集群条目指向一个 kubeconfig 文件，source 传 nil。
	// 这里刻意**不复刻**"平台从 DB 解密凭据"那条路径 —— 那属于装配层的职责，
	// 本测试只验写通道本身，混在一起会让失败原因变模糊。
	env := &liveEnv{
		cluster: NewK8sCluster(config.KubernetesConfig{
			Enabled: true,
			Timeout: config.Duration(20 * time.Second),
			Clusters: []config.K8sCluster{{
				Key:        envOr("YN_LIVE_CLUSTER_KEY", "live-cluster"),
				Name:       "live",
				Kubeconfig: kc,
			}},
		}, 3*time.Minute, nil),
		kubeconfig: kc,
		namespace:  envOr("YN_LIVE_NAMESPACE", "default"),
		sandbox:    envOr("YN_LIVE_SANDBOX", "nginx-demo"),
		probe:      envOr("YN_LIVE_PROBE", "yn-write-probe"),
	}
	return env
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func (e *liveEnv) ref(workload string) WorkloadRef {
	return WorkloadRef{
		ClusterRef: ClusterRef{Cluster: e.cluster.clusters[0].Key},
		Namespace:  e.namespace,
		Workload:   workload,
	}
}

func (e *liveEnv) clients(t *testing.T) (dynamic.Interface, kubernetes.Interface) {
	t.Helper()
	dyn, cs, err := e.cluster.crClients(context.Background(), e.ref("").ClusterRef)
	if err != nil {
		t.Fatalf("构造集群客户端失败：%v", err)
	}
	return dyn, cs
}

// kubectl 只在**准备、清理与旁证**阶段用（建/删临时工作负载、看 rollout），
// 被测代码本身一律走平台自己的客户端 —— 否则测的就不是平台了。
func (e *liveEnv) kubectl(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("kubectl", append([]string{"--request-timeout=60s"}, args...)...)
	cmd.Env = append(os.Environ(),
		"KUBECONFIG="+e.kubeconfig,
		"NO_PROXY=*",
		"HTTP_PROXY=", "HTTPS_PROXY=", "http_proxy=", "https_proxy=", "ALL_PROXY=")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// kubectlApply 用 stdin 灌一个清单进去（建立"带 env + 就绪探针"的靶子时用）。
// 与 kubectl 一样只在准备/清理阶段使用。
func (e *liveEnv) kubectlApply(t *testing.T, manifest string) {
	t.Helper()
	cmd := exec.Command("kubectl", "--request-timeout=60s", "-n", e.namespace, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	cmd.Env = append(os.Environ(),
		"KUBECONFIG="+e.kubeconfig,
		"NO_PROXY=*",
		"HTTP_PROXY=", "HTTPS_PROXY=", "http_proxy=", "https_proxy=", "ALL_PROXY=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("apply 靶子清单失败：%v\n%s", err, out)
	}
}

// cleanupWorkload 删除任意临时工作负载及其接管单（先删 Deployment 再删 CR）。
func (e *liveEnv) cleanupWorkload(t *testing.T, name string) {
	t.Helper()
	if out, err := e.kubectl(t, "-n", e.namespace, "delete", "deployment", name,
		"--ignore-not-found", "--wait=false"); err != nil {
		t.Logf("清理临时工作负载 %s/%s 失败：%v\n%s", e.namespace, name, err, out)
	}
	e.deleteCR(t, name)
}

// envNames 取出环境变量名，仅用于日志（值可能含密钥，不打出来）。
func envNames(vars []corev1.EnvVar) []string {
	names := make([]string, 0, len(vars))
	for i := range vars {
		names = append(names, vars[i].Name)
	}
	return names
}

func (e *liveEnv) getDeployment(t *testing.T, cs kubernetes.Interface, name string) *appsv1.Deployment {
	t.Helper()
	dep, err := cs.AppsV1().Deployments(e.namespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("读取工作负载 %s/%s 失败：%v", e.namespace, name, err)
	}
	return dep
}

// --------------------------------------------------------------------------- 主流程

func TestLiveAppReleaseWrite(t *testing.T) {
	env := liveFixture(t)
	ctx := context.Background()
	dyn, cs := env.clients(t)

	t.Logf("目标集群条目=%s namespace=%s 沙箱=%s 探针=%s",
		env.cluster.clusters[0].Key, env.namespace, env.sandbox, env.probe)

	t.Run("扩缩容", func(t *testing.T) {
		before := env.getDeployment(t, cs, env.sandbox)
		original := int32(1)
		if before.Spec.Replicas != nil {
			original = *before.Spec.Replicas
		}
		t.Logf("起始副本数 %d（image=%s generation=%d）",
			original, before.Spec.Template.Spec.Containers[0].Image, before.Generation)

		target := original + 1
		if err := env.cluster.Scale(ctx, env.ref(env.sandbox), int(target)); err != nil {
			t.Fatalf("扩容到 %d 失败：%v", target, err)
		}
		assertReplicas(t, env.getDeployment(t, cs, env.sandbox), target, "扩容后")
		t.Logf("扩容生效：集群 spec.replicas=%d", target)

		if err := env.cluster.Scale(ctx, env.ref(env.sandbox), int(original)); err != nil {
			t.Fatalf("缩回 %d 失败：%v", original, err)
		}
		assertReplicas(t, env.getDeployment(t, cs, env.sandbox), original, "缩回后")
		t.Logf("已还原到 %d 个副本", original)
	})

	t.Run("滚动重启", func(t *testing.T) {
		before := env.getDeployment(t, cs, env.sandbox)
		genBefore := before.Generation

		if err := env.cluster.Restart(ctx, env.ref(env.sandbox)); err != nil {
			t.Fatalf("滚动重启失败：%v", err)
		}
		after := env.getDeployment(t, cs, env.sandbox)
		if after.Generation <= genBefore {
			t.Fatalf("重启后 generation 没变（%d → %d）：注解写进去了但滚动没被触发",
				genBefore, after.Generation)
		}
		stamp := after.Spec.Template.Annotations[annotationRestartedAt]
		if stamp == "" {
			t.Fatalf("重启后 Pod 模板上没有 %s 注解", annotationRestartedAt)
		}
		t.Logf("重启已触发：generation %d → %d，%s=%s",
			genBefore, after.Generation, annotationRestartedAt, stamp)

		// 旁证：等这一轮滚动收敛。这是 k8s 侧行为，超时只记日志不判失败（拉镜像耗时不可控）。
		if out, err := env.kubectl(t, "-n", env.namespace, "rollout", "status",
			"deploy/"+env.sandbox, "--timeout=150s"); err != nil {
			t.Logf("旁证：%s（rollout status 未在 150s 内收敛：%v）", out, err)
		} else {
			t.Logf("旁证：%s", out)
		}
	})

	t.Run("换镜像", func(t *testing.T) {
		before := env.getDeployment(t, cs, env.sandbox)
		origImage := before.Spec.Template.Spec.Containers[0].Image

		// 目标镜像 = 当前镜像的**全限定等价写法**。
		//
		// 为什么不换一个"真的新版本"：本机到 Docker Hub / gcr.io 完全不通
		// （代理未开时连 registry-1.docker.io 都解析失败），任何需要拉新内容的镜像
		// 都会让 Pod 停在 ImagePullBackOff，于是 ApplyImage 等滚动超时 ——
		// 那是**环境的限制**，不是写通道的缺陷，不该把它记成测试失败。
		//
		// 而 containerd 的镜像存储键是**规范化后的全限定名**：
		// `nginx:1.25` 与 `docker.io/library/nginx:1.25` 指向同一条本地记录
		// （实测两者解析出同一个 imageID，Pod 2 秒内 Ready）。
		// 于是这一节验的是：**平台下发的镜像字符串确实经由变更单落到了集群**、
		// 且滚动真的跑完 —— 全程零外网。
		target := equivalentImageRef(origImage)
		if target == "" {
			t.Skipf("沙箱镜像 %q 是私有仓库地址，没有「内容相同但写法不同」的等价写法，跳过换镜像小节", origImage)
		}
		t.Logf("把镜像从 %s 换成它的全限定等价写法 %s（同一镜像内容，不触发拉取）", origImage, target)

		var stages []string
		if err := env.cluster.ApplyImage(ctx, ApplyRequest{
			WorkloadRef: env.ref(env.sandbox),
			Image:       target,
		}, func(stage string, pct uint8) error {
			stages = append(stages, fmt.Sprintf("%s@%d%%", stage, pct))
			return nil
		}); err != nil {
			t.Fatalf("换镜像失败：%v", err)
		}
		t.Logf("进度回调共 %d 次：%s", len(stages), strings.Join(stages, " → "))
		if n := len(stages); n == 0 || !strings.HasPrefix(stages[n-1], "流量接入@100") {
			t.Fatalf("进度回调没有以「流量接入@100%%」收尾：%v", stages)
		}

		// 集群侧：Deployment 真的换成了下发的那个字符串
		after := env.getDeployment(t, cs, env.sandbox)
		if got := after.Spec.Template.Spec.Containers[0].Image; got != target {
			t.Fatalf("Deployment 镜像没换成目标值：期望 %s，实际 %s", target, got)
		}
		t.Logf("集群已接受下发的镜像：spec.image=%s", target)

		// 变更单侧：这必须是经由 CR 下发的，而不是平台偷偷直改 Deployment。
		// 这一条是本节的核心 —— operator 会把手改的镜像秒级拉回，
		// 只有 CR 里的值才是「这次发布真的走了变更通道」的证据。
		crs, err := env.cluster.findAppReleasesByTarget(ctx, dyn, env.namespace, env.sandbox)
		if err != nil {
			t.Fatalf("反查变更单失败：%v", err)
		}
		if len(crs) != 1 {
			t.Fatalf("沙箱 %s/%s 的变更单数量异常：%d（期望恰好 1 个）",
				env.namespace, env.sandbox, len(crs))
		}
		if crs[0].Image != target {
			t.Fatalf("变更单 %s/%s 的镜像没更新：CR=%s 期望=%s",
				crs[0].Namespace, crs[0].Name, crs[0].Image, target)
		}
		t.Logf("变更单 %s/%s 已记录镜像 %s", crs[0].Namespace, crs[0].Name, crs[0].Image)

		// 还原同样走变更单：用 `kubectl set image` 会被 operator 拉回去。
		if err := env.cluster.ApplyImage(ctx, ApplyRequest{
			WorkloadRef: env.ref(env.sandbox),
			Image:       origImage,
		}, nil); err != nil {
			t.Fatalf("还原镜像到 %s 失败：%v", origImage, err)
		}
		final := env.getDeployment(t, cs, env.sandbox)
		if got := final.Spec.Template.Spec.Containers[0].Image; got != origImage {
			t.Fatalf("镜像未还原：期望 %s，实际 %s", origImage, got)
		}
		t.Logf("镜像已还原为 %s", origImage)
	})

	t.Run("首次接管与唯一性守卫", func(t *testing.T) {
		// 平台里 20 个纳管服务在工作负载里都是手工建的、没有 CR，
		// 所以"首次接管"才是主路径，必须单独验。这里用一个自己创建、
		// 测试结束就删掉的临时工作负载，不占用沙箱。
		env.cleanupProbe(t)
		t.Cleanup(func() { env.cleanupProbe(t) })

		if out, err := env.kubectl(t, "-n", env.namespace, "create", "deployment", env.probe,
			"--image=nginx:1.25", "--replicas=1"); err != nil {
			t.Fatalf("创建临时工作负载失败：%v\n%s", err, out)
		}
		t.Logf("已创建临时工作负载 %s/%s（测试结束会删除）", env.namespace, env.probe)

		// 等它真的跑起来再取基线：Deployment 控制器是异步建 RS/Pod 的，
		// 建单那一刻就取，RS 与 Pod 集合都是空的 —— "接管后没有新增 RS" 会变成空断言。
		env.waitDeploymentAvailable(t, cs, env.probe, 90*time.Second)
		depBefore := env.getDeployment(t, cs, env.probe)
		rsBefore := env.replicaSetNames(t, cs, env.probe)
		podsBefore := env.podIDs(t, cs, env.probe)
		t.Logf("接管前基线：generation=%d replicas=%d RS=%v Pod 数=%d",
			depBefore.Generation, int32Or(depBefore.Spec.Replicas, -1), rsBefore, len(podsBefore))

		// 1) 首次接管：建出 CR，且必须是"零改动"的（抄当前值）
		h, err := env.cluster.ensureAppRelease(ctx, dyn, cs, env.ref(env.probe))
		if err != nil {
			t.Fatalf("首次接管失败：%v", err)
		}
		if !h.Created {
			t.Fatalf("预期本次为首次接管（Created=true），实际没有新建 CR")
		}
		if h.View.TargetDeployment != env.probe {
			t.Fatalf("新建接管单的目标不对：%q", h.View.TargetDeployment)
		}
		t.Logf("首次接管控件 %s/%s：image=%s replicas=%d strategy=%s container=%s",
			h.View.Namespace, h.View.Name, h.View.Image, int32Or(h.View.Replicas, -1),
			h.View.Strategy, h.View.ContainerName)

		if h.View.Image != depBefore.Spec.Template.Spec.Containers[0].Image {
			t.Fatalf("接管单没有抄当前镜像：CR=%s 工作负载=%s",
				h.View.Image, depBefore.Spec.Template.Spec.Containers[0].Image)
		}
		if h.View.Replicas == nil || depBefore.Spec.Replicas == nil ||
			*h.View.Replicas != *depBefore.Spec.Replicas {
			t.Fatalf("接管单没有抄当前副本数：CR=%v 工作负载=%v", h.View.Replicas, depBefore.Spec.Replicas)
		}
		if h.View.Strategy != string(depBefore.Spec.Strategy.Type) {
			t.Fatalf("接管单没有抄当前更新策略：CR=%q 工作负载=%q",
				h.View.Strategy, depBefore.Spec.Strategy.Type)
		}

		// 2) 幂等：再调一次不该重复创建
		h2, err := env.cluster.ensureAppRelease(ctx, dyn, cs, env.ref(env.probe))
		if err != nil {
			t.Fatalf("第二次接管调用失败：%v", err)
		}
		if h2.Created {
			t.Fatalf("第二次调用不该再建 CR（幂等被破坏）")
		}

		// 3) 零改动：接管之后工作负载的**期望状态**一个字节都不该变。
		//    这是整个设计最要紧的性质 —— 接管不能等于"顺手改一下生产"。
		//
		//    ⚠️ 这里比的是 spec，**不是** generation。operator 用 SSA 写入时，
		//    即使 spec 逐字段完全一致，也会因为 listType=map 的 $setElementOrder
		//    而推进 generation（实测 1 → 2）—— 那只是记账，不是变更。
		//    "真的没改动"由三条证据共同证明：spec 深比对相等、ReplicaSet 没有新增、
		//    Pod 没有被重建。少任何一条都可能把一个真改动放过去。
		view := env.waitAppReleaseActed(t, dyn, env.probe, 30*time.Second)
		t.Logf("operator 已处理接管单：phase=%s observedGeneration=%d/%d message=%q",
			view.Phase, view.ObservedGeneration, view.Generation, view.Message)

		depAfter := env.waitDeploymentSettled(t, cs, env.probe, 30*time.Second)
		rsAfter := env.replicaSetNames(t, cs, env.probe)
		podsAfter := env.podIDs(t, cs, env.probe)

		if !equality.Semantic.DeepEqual(depBefore.Spec, depAfter.Spec) {
			t.Fatalf("接管改动了工作负载的 spec：\n接管前 %s\n接管后 %s",
				mustJSON(depBefore.Spec), mustJSON(depAfter.Spec))
		}
		if !equalStringSet(rsBefore, rsAfter) {
			t.Fatalf("接管新建了 ReplicaSet（%v → %v）：接管必须是零改动，不该触发任何滚动",
				rsBefore, rsAfter)
		}
		if !equalStringSet(podsBefore, podsAfter) {
			t.Fatalf("接管重建了 Pod（%v → %v）：接管必须是零改动，不该重启任何 Pod",
				podsBefore, podsAfter)
		}
		t.Logf("接管零改动已验证：spec 深比对一致、RS 仍为 %v、Pod 未重建"+
			"（generation %d → %d 只是 operator 记账）", rsAfter, depBefore.Generation, depAfter.Generation)

		// 4) 唯一性守卫：再造一个 CR 指向同一工作负载，平台必须拒绝，而不是随便挑一个用。
		//    两个 CR 管同一工作负载时不会报错、只会无声互斗，所以这里是硬拒绝。
		dup := env.probe + "-dup"
		env.makeDuplicateCR(t, dyn, dup, depAfter)
		t.Cleanup(func() { env.deleteCR(t, dup) })

		if _, err := env.cluster.ensureAppRelease(ctx, dyn, cs, env.ref(env.probe)); err == nil {
			t.Fatalf("两个 CR 指向同一工作负载时应当拒绝，但调用成功了")
		} else {
			t.Logf("唯一性守卫生效，拒绝理由：%v", err)
		}
	})

	t.Run("首次接管不碰 env 与就绪探针", func(t *testing.T) {
		// 这一节的靶子刻意照着**真实业务工作负载**的形态做（以 demobasic 为样本）：
		// 带一串环境变量（nacos 那类），带就绪探针。前面那节用的 nginx-demo 两样都没有，
		// 恰好把最可能出事的两处漏掉了 —— 而"接管把生产的环境变量清空"是那种
		// 上线当天才会炸、且事后极难归因的事故。
		//
		// 依据（operator 源码）：`env` 为空时**故意不声明该字段**，且漂移比对只回撤
		// 它自己写过的变量（靠 last-applied-env 注解）。这条测试就是把源码结论钉在真机上。
		const name = "yn-env-probe"
		env.cleanupWorkload(t, name)
		t.Cleanup(func() { env.cleanupWorkload(t, name) })

		manifest := fmt.Sprintf(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: %s
  labels: {app: %s}
spec:
  replicas: 1
  selector:
    matchLabels: {app: %s}
  template:
    metadata:
      labels: {app: %s}
    spec:
      containers:
        - name: master
          image: nginx:1.25
          ports: [{containerPort: 80}]
          env:
            - name: NACOS_ADDR
              value: nacos.infra:8848
            - name: NACOS_NAMESPACE
              value: prod-ns
            - name: SPRING_PROFILES_ACTIVE
              value: prod
            - name: LOG_LEVEL
              value: warn
          readinessProbe:
            httpGet: {path: /, port: 80}
            initialDelaySeconds: 1
            periodSeconds: 3
`, name, name, name, name)
		env.kubectlApply(t, manifest)
		env.waitDeploymentAvailable(t, cs, name, 120*time.Second)

		depBefore := env.getDeployment(t, cs, name)
		envBefore := depBefore.Spec.Template.Spec.Containers[0].Env
		probeBefore := depBefore.Spec.Template.Spec.Containers[0].ReadinessProbe
		if len(envBefore) != 4 || probeBefore == nil {
			t.Fatalf("靶子没按预期建出来：env=%d 项，readinessProbe=%v", len(envBefore), probeBefore != nil)
		}
		t.Logf("接管前：env=%s，就绪探针=%s",
			mustJSON(envNames(envBefore)), probeBefore.HTTPGet.Path)

		h, err := env.cluster.ensureAppRelease(ctx, dyn, cs, env.ref(name))
		if err != nil {
			t.Fatalf("首次接管失败：%v", err)
		}
		if !h.Created {
			t.Fatal("预期本次为首次接管，实际没有新建 CR")
		}
		// 接管单里**不许**有 env —— 它一旦声明，operator 就接管环境变量，
		// 之后再改 CR 就可能把业务侧注入的变量当成漂移清掉。
		crs, err := env.cluster.findAppReleasesByTarget(ctx, dyn, env.namespace, name)
		if err != nil || len(crs) != 1 {
			t.Fatalf("反查接管单失败（%d 个）：%v", len(crs), err)
		}
		raw, _ := dyn.Resource(appReleaseGVR).Namespace(env.namespace).Get(ctx, crs[0].Name, metav1.GetOptions{})
		if _, found, _ := unstructured.NestedSlice(raw.Object, "spec", "env"); found {
			t.Fatalf("接管单声明了 spec.env（%s）：接管不该接管业务侧注入的环境变量",
				mustJSON(raw.Object["spec"]))
		}

		view := env.waitAppReleaseActed(t, dyn, name, 30*time.Second)
		depAfter := env.waitDeploymentSettled(t, cs, name, 30*time.Second)
		t.Logf("operator 已处理：phase=%s observedGeneration=%d/%d",
			view.Phase, view.ObservedGeneration, view.Generation)

		envAfter := depAfter.Spec.Template.Spec.Containers[0].Env
		if !equality.Semantic.DeepEqual(envBefore, envAfter) {
			t.Fatalf("接管改动了环境变量：\n接管前 %s\n接管后 %s",
				mustJSON(envBefore), mustJSON(envAfter))
		}
		if !equality.Semantic.DeepEqual(probeBefore, depAfter.Spec.Template.Spec.Containers[0].ReadinessProbe) {
			t.Fatalf("接管改动了就绪探针：\n接管前 %s\n接管后 %s", mustJSON(probeBefore),
				mustJSON(depAfter.Spec.Template.Spec.Containers[0].ReadinessProbe))
		}
		if !equality.Semantic.DeepEqual(depBefore.Spec, depAfter.Spec) {
			t.Fatalf("接管改动了工作负载 spec：\n接管前 %s\n接管后 %s",
				mustJSON(depBefore.Spec), mustJSON(depAfter.Spec))
		}
		t.Logf("已确认：%d 个环境变量与就绪探针在首次接管后分毫未动", len(envAfter))
	})

	t.Run("目标不存在必须报错", func(t *testing.T) {
		err := env.cluster.ApplyImage(ctx, ApplyRequest{
			WorkloadRef: env.ref("yn-definitely-not-exists"),
			Image:       "nginx:1.25",
		}, nil)
		if err == nil {
			t.Fatalf("目标工作负载不存在时应当报错")
		}
		if !strings.Contains(err.Error(), "没有工作负载") {
			t.Fatalf("错误信息没有说清目标工作负载不存在：%v", err)
		}
		if !strings.Contains(err.Error(), "yn-definitely-not-exists") {
			t.Fatalf("错误信息没有带上目标工作负载的名字，用户无法据此定位：%v", err)
		}
		t.Logf("已按预期拒绝：%v", err)
	})
}

// --------------------------------------------------------------------------- 断言与清理

func assertReplicas(t *testing.T, dep *appsv1.Deployment, want int32, when string) {
	t.Helper()
	got := int32(-1)
	if dep.Spec.Replicas != nil {
		got = *dep.Spec.Replicas
	}
	if got != want {
		t.Fatalf("%s副本数不符：期望 %d，实际 %d", when, want, got)
	}
}

// makeDuplicateCR 造一个指向同一工作负载的第二个 CR，用来验证唯一性守卫。
// 它的 spec 刻意与既有接管单**完全一致**（同镜像、同副本、同策略、不声明 env），
// 这样即便 operator 真的处理它，也不会造成任何可见变化 —— 要验的是
// "平台会不会拒绝"，而不是"两个 CR 打起来会怎样"。
func (e *liveEnv) makeDuplicateCR(t *testing.T, dyn dynamic.Interface, name string, dep *appsv1.Deployment) {
	t.Helper()
	replicas := int64(1)
	if dep.Spec.Replicas != nil {
		replicas = int64(*dep.Spec.Replicas)
	}
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": appReleaseGroup + "/" + appReleaseVersion,
		"kind":       appReleaseKind,
		"metadata":   map[string]interface{}{"name": name, "namespace": e.namespace},
		"spec": map[string]interface{}{
			"targetDeployment": e.probe,
			"namespace":        e.namespace,
			"containerName":    dep.Spec.Template.Spec.Containers[0].Name,
			"image":            dep.Spec.Template.Spec.Containers[0].Image,
			"replicas":         replicas,
			"strategy":         string(dep.Spec.Strategy.Type),
		},
	}}
	if _, err := dyn.Resource(appReleaseGVR).Namespace(e.namespace).
		Create(context.Background(), obj, metav1.CreateOptions{}); err != nil {
		t.Fatalf("创建重复接管单失败：%v", err)
	}
	t.Logf("已创建重复接管单 %s/%s（验证拒绝逻辑用，随后删除）", e.namespace, name)
}

func (e *liveEnv) deleteCR(t *testing.T, name string) {
	t.Helper()
	dyn, _, err := e.cluster.crClients(context.Background(), e.ref("").ClusterRef)
	if err != nil {
		t.Logf("清理 %s/%s 时构造客户端失败：%v", e.namespace, name, err)
		return
	}
	ctx := context.Background()
	res := dyn.Resource(appReleaseGVR).Namespace(e.namespace)
	if err := res.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		t.Logf("删除接管单 %s/%s 失败：%v", e.namespace, name, err)
		return
	}
	// CR 上有 finalizer：删完先进 Terminating，等 operator 清理完注解才真正消失。
	for i := 0; i < 45; i++ {
		if _, err := res.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			t.Logf("接管单 %s/%s 已彻底删除", e.namespace, name)
			return
		}
		time.Sleep(time.Second)
	}
	t.Logf("接管单 %s/%s 仍在 Terminating（finalizer 未摘），请人工确认", e.namespace, name)
}

func (e *liveEnv) cleanupProbe(t *testing.T) {
	t.Helper()
	if out, err := e.kubectl(t, "-n", e.namespace, "delete", "deployment", e.probe,
		"--ignore-not-found", "--wait=false"); err != nil {
		t.Logf("清理临时工作负载 %s/%s 失败：%v\n%s", e.namespace, e.probe, err, out)
	}
	// 顺带把它的接管单也删掉（首次接管小节会在删除工作负载之前清，这里做兜底）
	e.deleteCR(t, e.probe)
}

// --------------------------------------------------------------------------- 等待

// waitAppReleaseActed 等 operator 对这张变更单做出反应（即写上 status）。
//
// 不等它就断言"接管零改动"，读到的可能只是"operator 还没动"的中间态 —— 那是**假绿**：
// 改动其实发生了，只是测试跑得比 operator 快。所以必须先确认它已经处理过。
func (e *liveEnv) waitAppReleaseActed(
	t *testing.T, dyn dynamic.Interface, name string, timeout time.Duration,
) appReleaseView {
	t.Helper()
	var last appReleaseView
	deadline := time.Now().Add(timeout)
	for {
		if v, err := e.cluster.getAppRelease(context.Background(), dyn, e.namespace, name); err == nil {
			last = v
			if v.Phase != "" {
				return v
			}
		}
		if time.Now().After(deadline) {
			t.Logf("等待 operator 处理变更单 %s/%s 超时（%s）：phase=%q（继续比对，失败时据此判断）",
				e.namespace, name, timeout, last.Phase)
			return last
		}
		time.Sleep(time.Second)
	}
}

// waitDeploymentSettled 等 Deployment 控制器消化完最新一次 spec 改动
// （observedGeneration 追上 generation）。
//
// 没有这一步，读到的可能是改动尚未生效的中间态，"spec 没变"于是被误判为通过。
func (e *liveEnv) waitDeploymentSettled(
	t *testing.T, cs kubernetes.Interface, name string, timeout time.Duration,
) *appsv1.Deployment {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		dep, err := cs.AppsV1().Deployments(e.namespace).Get(context.Background(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("读取工作负载 %s/%s 失败：%v", e.namespace, name, err)
		}
		if dep.Status.ObservedGeneration >= dep.Generation {
			return dep
		}
		if time.Now().After(deadline) {
			t.Logf("等待 %s/%s 的 observedGeneration 追上 generation 超时（%s）：%d/%d",
				e.namespace, name, timeout, dep.Status.ObservedGeneration, dep.Generation)
			return dep
		}
		time.Sleep(time.Second)
	}
}

// waitDeploymentAvailable 等一个工作负载至少有一个可用副本。
//
// 取"接管前基线"之前必须等它跑起来：Deployment 控制器是异步建 RS/Pod 的，
// 刚 create 完就取，RS 与 Pod 集合都是空的，"接管后没新增 RS" 就成了空断言。
func (e *liveEnv) waitDeploymentAvailable(
	t *testing.T, cs kubernetes.Interface, name string, timeout time.Duration,
) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		dep, err := cs.AppsV1().Deployments(e.namespace).Get(context.Background(), name, metav1.GetOptions{})
		if err == nil && dep.Status.AvailableReplicas >= 1 {
			return
		}
		if time.Now().After(deadline) {
			msg := "Deployment 不存在"
			if err != nil {
				msg = err.Error()
			} else {
				msg = fmt.Sprintf("availableReplicas=%d", dep.Status.AvailableReplicas)
			}
			t.Fatalf("等待临时工作负载 %s/%s 就绪超时（%s）：%s", e.namespace, name, timeout, msg)
		}
		time.Sleep(time.Second)
	}
}

// --------------------------------------------------------------------------- 观测量

// replicaSetNames 返回一个工作负载当前的 ReplicaSet 名字集合（已排序）。
// 接管若真的"零改动"，这个集合不该新增 —— 新增 RS 就意味着触发了滚动更新。
func (e *liveEnv) replicaSetNames(t *testing.T, cs kubernetes.Interface, workload string) []string {
	t.Helper()
	dep, err := cs.AppsV1().Deployments(e.namespace).Get(context.Background(), workload, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("读取工作负载 %s/%s 失败：%v", e.namespace, workload, err)
	}
	list, err := cs.AppsV1().ReplicaSets(e.namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(dep.Spec.Selector.MatchLabels).String(),
	})
	if err != nil {
		t.Fatalf("列出 %s/%s 的 ReplicaSet 失败：%v", e.namespace, workload, err)
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}
	sort.Strings(names)
	return names
}

// podIDs 返回一个工作负载当前 Pod 的 UID 集合（已排序）。
//
// 用 UID 而不是名字：同名 Pod 被删掉重建是可能发生的，UID 才能分辨"重建过"。
func (e *liveEnv) podIDs(t *testing.T, cs kubernetes.Interface, workload string) []string {
	t.Helper()
	dep, err := cs.AppsV1().Deployments(e.namespace).Get(context.Background(), workload, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("读取工作负载 %s/%s 失败：%v", e.namespace, workload, err)
	}
	list, err := cs.CoreV1().Pods(e.namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(dep.Spec.Selector.MatchLabels).String(),
	})
	if err != nil {
		t.Fatalf("列出 %s/%s 的 Pod 失败：%v", e.namespace, workload, err)
	}
	ids := make([]string, 0, len(list.Items))
	for i := range list.Items {
		ids = append(ids, string(list.Items[i].UID))
	}
	sort.Strings(ids)
	return ids
}

func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// int32Or 解引用可选副本数，nil 时返回 def。
// 直接 %v 一个 *int32 会打出地址（0x140...），日志里毫无意义。
func int32Or(p *int32, def int32) int32 {
	if p == nil {
		return def
	}
	return *p
}

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<序列化失败：%v>", err)
	}
	return string(b)
}

// equivalentImageRef 返回 image 的另一种写法，指向**同一份镜像内容**。
//
// 依据：containerd 的镜像存储键是规范化后的全限定名 —— 默认 registry 是 docker.io、
// 官方镜像补齐 library/。所以下面两种改写都不会改变实际解析到的镜像：
//
//	docker.io/library/nginx:1.25  →  library/nginx:1.25
//	nginx:1.25                    →  docker.io/library/nginx:1.25
//
// 返回空串表示改写不安全，调用方应当跳过：例如私有仓库地址
// （es-repo.example.com:5000/escloud/xxx）动一个字符就可能指向别的镜像。
func equivalentImageRef(image string) string {
	if image == "" {
		return ""
	}
	// 先剥掉 tag / digest，只看仓库部分 —— 否则 `nginx:1.25` 里的冒号会被误判成 registry 端口。
	repo := image
	if i := strings.IndexByte(repo, '@'); i >= 0 {
		repo = repo[:i]
	} else if i := strings.LastIndexByte(repo, ':'); i >= 0 && !strings.Contains(repo[i:], "/") {
		repo = repo[:i]
	}
	if strings.HasPrefix(repo, "docker.io/") {
		return strings.Replace(image, "docker.io/", "", 1)
	}
	first := ""
	if i := strings.IndexByte(repo, '/'); i >= 0 {
		first = repo[:i]
	}
	if first == "" || (!strings.ContainsAny(first, ".:") && first != "localhost") {
		return "docker.io/library/" + image
	}
	return ""
}
