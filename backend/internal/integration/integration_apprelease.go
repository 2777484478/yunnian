// 本文件实现平台的**集群写入通道**：通过 AppRelease（apps.example.com/v1alpha1）下发变更。
//
// # 为什么写走 CR，而不是直接改 Deployment
//
// 集群里跑着一个 apprelease-operator（apprelease-system/apprelease-operator-controller-manager），
// 它用 Server-Side Apply + **ForceOwnership**、field manager = `apprelease-controller`
// 接管了目标 Deployment 的这些字段：`spec.template.spec.containers`（只含被声明的容器的
// image/env）、`spec.replicas`、`spec.strategy`，以及三个 `apps.example.com/*` 注解。
//
// 于是：平台若直接改 Deployment 的同一批字段，只会与它互相覆盖
// —— 谁后写谁赢、双方日志都正常、现场再看已经变回去了，是最难排查的一类故障。
// CR 才是 operator 唯一接受的变更入口，写它等于"把意图交给唯一的管理者"。
//
// 读则相反：**必须读 Deployment**。CR 的 status 只说明"patch 被接受了"，不代表 Pod 起来了；
// 而且集群里那批手工 kubectl apply 建的 Deployment 根本没有 CR
// （实测 default 下 47 个业务 Deployment 只有 2 个带接管标记）。
//
// # 以下七条来自 operator 源码（api/v1alpha1 + internal/controller），改动前请先读
//
//  1. **operator 只 patch 不 create**。RBAC 对 deployments 只有 get;list;patch;watch，
//     源码里 Get 到 NotFound 只写 status=Failed(TargetNotFound) + RequeueAfter(1min)。
//     → 目标 Deployment 不存在时它永远不会帮你建。平台必须先确认 Deployment 存在，
//     不存在就如实报错，绝不能假装能部署（那会变成"平台说成功了、集群里什么都没有"）。
//
//  2. **Failed 分两类，处置完全不同**：
//     - `TargetNotFound`：**非终态**，它 Watch 了 Deployment，目标一出现就自愈；
//     - `InvalidSpec`（如 containerName 不存在）/ `ApplyFailed(Forbidden|Invalid|BadRequest)`：
//     终态，重试无用，必须改 spec。
//     → 界面上不能把 Failed 一律渲染成死路，得说清是哪一种。
//
//  3. **条件词与 reason 都是固定字符串**：type 只有 `Ready` / `Progressing`；
//     reason 有 `RolloutComplete` / `RolloutInProgress` / `TargetNotFound` / `InvalidSpec` / `ApplyFailed`。
//     ⚠️ 成功态的 reason 叫 **RolloutComplete**，不叫 Succeeded
//     —— `Succeeded` 只是 status.phase 的取值，两者不能混用。
//
//  4. **operator Watch 了 Deployment 并逐字段纠偏**（`deploymentDiffers` 比对
//     image / replicas / strategy / env / 接管标记）。所以：
//     - 手工 `kubectl set image` 会在秒级被拉回；
//     - **回滚也必须改 CR，不能用 `kubectl rollout undo`**（习惯性写法在这里是错的）；
//     - 反过来，它**不比对** `spec.template.metadata.annotations`，这是下面 Restart 敢直接
//     写 Deployment 的依据。
//
//  5. **不要用 Server-Side Apply 去改既有 CR**。CR 里没写出来的可选字段会被当成
//     "本 manager 不再需要它"而被清掉 —— 一次单字段 SSA 可能顺手抹掉 replicas / env。
//     本文件一律 `Get → 改字段 → Update`，并靠 `retry.RetryOnConflict` 处理并发。
//
//  6. **两个 CR 指向同一工作负载不会报错，只会无声互斗**（没有准入校验；
//     Deployment 上的 `managed-by` 注解只是漂移标记，被改错了 operator 会直接改回自己的）。
//     → 唯一性必须由平台自己保证：按 `(namespace, spec.targetDeployment)` 反查。
//     ⚠️ **不能假设 CR 名等于工作负载名**：集群里现成的 `apprelease-sample` 管的就是 `nginx-demo`。
//
//  7. **CRD 给 `spec.strategy` 设了 `+kubebuilder:default=RollingUpdate`**。
//     这意味着"不写 strategy"并不等于"不接管策略"—— apiserver 会补上 RollingUpdate，
//     而 `deploymentDiffers` 只判断 `spec.Strategy != ""`，于是原本是 Recreate 的 Deployment
//     会被强制改成 RollingUpdate。**首次接管时必须显式写上目标当前的真实策略**。
//
// # 首次接管（bootstrap）
//
// 平台纳管的 20 个项目在工作负载里都是手工建的，没有 CR。所以第一次下发前必须先建 CR，
// 且**按目标当前的真实值预填**（镜像 / 副本数 / 策略 / 容器名），使这次接管本身是零改动：
// apply 前后 Deployment 一个字节都不变。这是从 operator 仓库 `examples/client` 的
// `new` 子命令学来的做法 —— 它存在的唯一理由就是"别让模板里写死的假镜像一 apply 就改生产"。
//
// 建单这一动作会写集群，属实质性变更，因此 `Created=true` 会一路带回调用方用于审计与日志。
package integration

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	// appReleaseGroup / Version / Plural 对应 CRD 里的 GVK 与 spec.names.plural。
	// 动态客户端没有类型信息可用，三元组必须逐字写对（复数、小写）。
	appReleaseGroup   = "apps.example.com"
	appReleaseVersion = "v1alpha1"
	appReleaseKind    = "AppRelease"
	appReleasePlural  = "appreleases"

	// annotationManagedBy 是 operator 写在目标 Deployment 上的接管标记，
	// 值形如 "<CR 所在命名空间>/<CR 名>"。它让"这个工作负载归谁管"一眼可见。
	annotationManagedBy = "apps.example.com/managed-by"

	// annotationRestartedAt 是 kubectl rollout restart 约定的重启时间戳注解，
	// 写在 Pod 模板上 —— 改它会触发一次滚动更新。
	annotationRestartedAt = "kubectl.kubernetes.io/restartedAt"

	// fieldManagerPlatform 是平台侧写操作登记的 field manager 名。
	// 万一将来出现字段争抢，能从 Deployment 的 managedFields 里分辨是谁写的。
	fieldManagerPlatform = "yunnian-platform"

	// appReleasePollInterval 是等待滚动期间的轮询间隔。
	// 不用 Watch：部署这类操作的耗时在分钟级，轮询的请求量可以忽略，
	// 而 Watch 需要处理断线重连、resourceVersion 过期、事件乱序等一堆边界。
	appReleasePollInterval = 2 * time.Second

	// appReleaseScaleTimeout 是"等扩缩容生效"的上限。
	// 它只等集群**接受并观察到**新副本数，不等 Pod 全部就绪 ——
	// 拉起新 Pod 可能受镜像拉取影响耗时很久，那不是"扩缩容失败"。
	appReleaseScaleTimeout = 2 * time.Minute

	// strategyRollingUpdate / strategyRecreate 是 Deployment 更新策略的取值，
	// 也是 CRD 里 strategy 字段的枚举范围，与 model.DeployRecord.Strategy 的默认值一致。
	strategyRollingUpdate = "RollingUpdate"
	strategyRecreate      = "Recreate"
)

var appReleaseGVR = schema.GroupVersionResource{
	Group:    appReleaseGroup,
	Version:  appReleaseVersion,
	Resource: appReleasePlural,
}

// appReleaseView 是平台真正会用到的 CR 字段。
//
// 刻意不把 CRD 的完整结构在平台里复制一份：复制一份的代价是 operator 一加字段平台就要跟着改，
// 而平台只关心"谁管谁、期望是什么、现在卡在哪"。
type appReleaseView struct {
	Name      string
	Namespace string
	// TargetDeployment 是 spec.targetDeployment，唯一性判断的依据。
	TargetDeployment string
	Image            string
	// ContainerName 是 spec.containerName：镜像/env 作用到哪个容器。
	// 留空时 operator 取 Pod 模板的第一个容器 —— 多容器工作负载必须显式指定，
	// 因此完成校验（"镜像真的换成目标版本了吗"）时要按这个容器去比对。
	ContainerName string
	// Replicas 为 nil 表示 CR 没声明副本数 = operator 不接管（通常因为有 HPA）。
	Replicas *int32
	Strategy string
	Phase    string
	Message  string
	// ObservedGeneration 追上 Generation 说明当前 spec 已被成功下发过。
	Generation         int64
	ObservedGeneration int64
}

// terminalFailure 判断这个 CR 是否处于"重试也没用"的失败态。
//
// 依据 operator 源码：TargetNotFound 会每分钟重试且 Deployment 一出现就自愈，
// 不能当终态；InvalidSpec 与 ApplyFailed(Forbidden|Invalid|BadRequest) 才是终态。
func (v appReleaseView) terminalFailure() (bool, string) {
	if v.Phase != "Failed" {
		return false, ""
	}
	if strings.Contains(v.Message, "不存在") {
		// TargetNotFound：目标工作负载不在，属可自愈。
		return false, ""
	}
	return true, v.Message
}

// --------------------------------------------------------------------------- 客户端

// crClients 一次性构造两个客户端：动态客户端（读写 AppRelease CR）与核心客户端（读写 Deployment）。
//
// 必须**共用同一份 rest.Config**：分两次解析凭据，在"环境自带 kubeconfig"的场景下
// 有可能落到不同集群 —— 那就是"以为在改 A、其实改的是 B"。
func (k *K8sCluster) crClients(ctx context.Context, ref ClusterRef) (dynamic.Interface, kubernetes.Interface, error) {
	rc, err := k.restConfigFor(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, nil, fmt.Errorf("创建 K8s 客户端失败：%w", err)
	}
	dyn, err := dynamic.NewForConfig(rc)
	if err != nil {
		return nil, nil, fmt.Errorf("创建 AppRelease 客户端失败：%w", err)
	}
	return dyn, cs, nil
}

// --------------------------------------------------------------------------- 变更通道

// appReleaseHandle 是"这个工作负载该往哪个 CR 下发变更"的答案。
type appReleaseHandle struct {
	View appReleaseView
	// Created 表示本次调用新建了 CR（即平台首次接管这个工作负载）。
	// 调用方应当把这件事记进审计/部署日志 —— 它把一个手工维护的工作负载
	// 变成了由 operator 托管，是实质性变更。
	Created bool
	// HPAName 非空表示该工作负载由 HPA 伸缩。
	// 此时**绝不能声明 replicas**：operator 与 HPA 会互相覆盖，Pod 反复重建。
	HPAName string
}

// WorkloadNotFoundError 目标工作负载在集群里**不存在**。
//
// 为什么要单独一个类型，而不是一条普通错误：调用方必须能把「集群里**本来就没有**」
// 与「改不动」分开。最典型的是「下线服务」—— 它的目标是"这个服务不再有实例在跑"，
// 而目标工作负载不存在时这个状态**已经达成**了，不该因此把自己卡死。
//
// 实测就是被这条卡住的：项目删掉之后留下的「幽灵服务实例」（工作负载从来没建出来过），
// 因为下线永远失败而无法从环境里清掉，只能靠手工改库。
type WorkloadNotFoundError struct {
	Namespace string
	Workload  string
}

func (e *WorkloadNotFoundError) Error() string {
	return fmt.Sprintf(
		"集群里没有工作负载 %s/%s：变更通道只负责修改已存在的工作负载，"+
			"它不会创建 Deployment。请先确认环境 namespace 是否配对，或用运维手段把它部署出来",
		e.Namespace, e.Workload)
}

// ensureAppRelease 保证目标工作负载有且只有一个可用的 CR，返回它。
//
// 幂等：已有 CR 就直接用，不会重复创建。四种情形分别处置：
//   - 查到 0 个 → 首次接管，按当前真实值预填建单（零改动）；
//   - 查到 1 个 → 直接用（**不管它叫什么名字**）；
//   - 查到多个 → 拒绝。它们会无声互斗，必须先由人合并；
//   - Deployment 上已有别人的接管标记 → 拒绝，不去抢别人管的工作负载。
func (k *K8sCluster) ensureAppRelease(
	ctx context.Context,
	dyn dynamic.Interface,
	cs kubernetes.Interface,
	ref WorkloadRef,
) (*appReleaseHandle, error) {
	if err := ref.validate(); err != nil {
		return nil, err
	}

	dep, err := cs.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Workload, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// 这是 operator 的硬约束（只 patch 不 create），不是平台的临时限制。
			// 说得具体一点：用户需要的是"去把这个工作负载建出来"，而不是"再试一次"。
			// 用专门的类型返回，好让「下线服务」这类"目标状态其实已达成"的调用方能分辨。
			return nil, &WorkloadNotFoundError{Namespace: ref.Namespace, Workload: ref.Workload}
		}
		return nil, fmt.Errorf("读取工作负载 %s/%s 失败：%w", ref.Namespace, ref.Workload, err)
	}

	hpa, err := hpaFor(ctx, cs, ref.Namespace, ref.Workload)
	if err != nil {
		// HPA 探测失败不该阻断部署：它只影响"能不能声明副本数"这一件事，
		// 而漏探的后果（与 HPA 互斗）远小于"因此部署不了"的后果。
		// 但不能装作没发生 —— 记日志，并让调用方在需要时自己去看。
		slog.Warn("HPA 探测失败，将按无 HPA 处理", "namespace", ref.Namespace,
			"workload", ref.Workload, "err", err)
		hpa = ""
	}

	existing, err := k.findAppReleasesByTarget(ctx, dyn, ref.Namespace, ref.Workload)
	if err != nil {
		return nil, err
	}

	switch len(existing) {
	case 1:
		return &appReleaseHandle{View: existing[0], HPAName: hpa}, nil

	case 0:
		// 没有 CR。先看 Deployment 上有没有别人的接管标记 —— 有标记但查不到 CR，
		// 说明 CR 在别的命名空间、或已被删、或当前凭据读不到。无论哪种，都不该贸然接管。
		if owner := dep.Annotations[annotationManagedBy]; owner != "" {
			return nil, fmt.Errorf(
				"工作负载 %s/%s 已被 %q 接管，但平台在命名空间 %s 下查不到对应的 AppRelease。"+
					"请先在集群里确认该接管单的状态，不要重复接管",
				ref.Namespace, ref.Workload, owner, ref.Namespace)
		}
		view, err := k.createAppRelease(ctx, dyn, dep, ref, hpa)
		if err != nil {
			return nil, err
		}
		return &appReleaseHandle{View: *view, Created: true, HPAName: hpa}, nil

	default:
		names := make([]string, 0, len(existing))
		for _, v := range existing {
			names = append(names, v.Namespace+"/"+v.Name)
		}
		// 两个 CR 管同一个工作负载不报错，只互相覆盖，日志双方都正常 ——
		// 所以这里是"必须先人工处理"的硬拒绝，而不是警告。
		return nil, fmt.Errorf(
			"工作负载 %s/%s 被多个变更单同时接管（%s）：它们会互相覆盖且不会报错，"+
				"请先合并或删除多余的变更单",
			ref.Namespace, ref.Workload, strings.Join(names, "、"))
	}
}

// findAppReleasesByTarget 按 `spec.targetDeployment` 反查接管单。
//
// 只能 List 后在内存里过滤：CRD 自定义字段没有服务端字段索引，
// apiserver 不支持 `fieldSelector=spec.targetDeployment=...`。
// 同一个命名空间下的 CR 数量是"人维护的发布单"级别，全量 List 的成本可以接受。
func (k *K8sCluster) findAppReleasesByTarget(
	ctx context.Context,
	dyn dynamic.Interface,
	namespace, workload string,
) ([]appReleaseView, error) {
	list, err := dyn.Resource(appReleaseGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, k.appReleaseListError(ctx, dyn, namespace, err)
	}
	out := make([]appReleaseView, 0, 2)
	for i := range list.Items {
		v := toAppReleaseView(&list.Items[i])
		if v.TargetDeployment == workload {
			out = append(out, v)
		}
	}
	return out, nil
}

// appReleaseListError 把 List 失败翻译成"下一步该做什么"。
//
// 单独抽出来是因为 apiserver 对"资源不存在"和"命名空间不存在"**都返回 404**，
// 直接把 err 抛出去会让人以为 CRD 装了、只是命名空间写错，然后去翻半天配置。
// 再打一次全集群 List 就能分辨：能列出来 → CRD 在，是命名空间的问题。
func (k *K8sCluster) appReleaseListError(
	ctx context.Context,
	dyn dynamic.Interface,
	namespace string,
	err error,
) error {
	if apierrors.IsForbidden(err) {
		return fmt.Errorf(
			"当前凭据无权读取变更单（appreleases.apps.example.com）：请给该集群的 kubeconfig "+
				"授予 appreleases 的 get/list/watch/create/update/patch 权限：%w", err)
	}
	if !apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return fmt.Errorf("查询变更单失败：%w", err)
	}
	if _, probeErr := dyn.Resource(appReleaseGVR).List(ctx, metav1.ListOptions{Limit: 1}); probeErr == nil {
		return fmt.Errorf("命名空间 %q 不存在（变更单资源本身可读）：%w", namespace, err)
	}
	return fmt.Errorf(
		"集群里没有 AppRelease 资源（apps.example.com/%s）：该集群未安装 apprelease CRD，"+
			"平台的变更通道无法下发。安装方式见 operator 仓库的 config/crd：%w",
		appReleaseVersion, err)
}

// createAppRelease 首次接管：按目标工作负载**当前的真实值**建一个零改动的接管单。
//
// 每一项都刻意如此，改之前请想清楚后果：
//   - image / replicas / strategy / containerName 全部抄当前值 → 接管本身不改变任何东西；
//   - strategy 必须显式写（见文件头第 7 条：CRD 的 default 会把它补成 RollingUpdate）；
//   - replicas 在有 HPA 时**不写**（不写 = 不接管，HPA 可以自由伸缩）；
//   - env 一概不写 → 不接管现有环境变量，Deployment 上的变量分毫不动。
func (k *K8sCluster) createAppRelease(
	ctx context.Context,
	dyn dynamic.Interface,
	dep *appsv1.Deployment,
	ref WorkloadRef,
	hpaName string,
) (*appReleaseView, error) {
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		return nil, fmt.Errorf("工作负载 %s/%s 的 Pod 模板里没有任何容器，无法接管",
			ref.Namespace, ref.Workload)
	}

	spec := map[string]interface{}{
		"targetDeployment": ref.Workload,
		"namespace":        ref.Namespace,
		// CR 名沿用工作负载名，让"谁管谁"一眼可见。已存在同名但指向别处的 CR 会被下面拦住。
		"containerName": dep.Spec.Template.Spec.Containers[0].Name,
		"image":         dep.Spec.Template.Spec.Containers[0].Image,
	}
	if st := string(dep.Spec.Strategy.Type); st != "" {
		spec["strategy"] = st
	}
	// 副本数：有 HPA 时**整项不写**。不写 = 不接管，HPA 才能自由伸缩；
	// 一旦写上，operator 会把 HPA 刚拉起来的副本数改回去，Pod 反复重建。
	if hpaName == "" {
		if dep.Spec.Replicas != nil {
			spec["replicas"] = int64(*dep.Spec.Replicas)
		} else {
			spec["replicas"] = int64(1)
		}
	}

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": appReleaseGroup + "/" + appReleaseVersion,
		"kind":       appReleaseKind,
		"metadata": map[string]interface{}{
			"name":      ref.Workload,
			"namespace": ref.Namespace,
		},
		"spec": spec,
	}}

	created, err := dyn.Resource(appReleaseGVR).Namespace(ref.Namespace).
		Create(ctx, obj, metav1.CreateOptions{FieldManager: fieldManagerPlatform})
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			// 并发：别的请求刚建好。重新反查一次，能查到就用它，查不到说明同名 CR 指向别处。
			found, ferr := k.findAppReleasesByTarget(ctx, dyn, ref.Namespace, ref.Workload)
			if ferr != nil {
				return nil, ferr
			}
			if len(found) == 1 {
				v := found[0]
				return &v, nil
			}
			return nil, fmt.Errorf(
				"命名空间 %s 下已存在名为 %q 的变更单，但它没有指向工作负载 %s。"+
					"请改名或先处理该变更单",
				ref.Namespace, ref.Workload, ref.Workload)
		}
		if apierrors.IsForbidden(err) {
			return nil, fmt.Errorf(
				"当前凭据无权创建变更单（appreleases.apps.example.com）：请给该集群的 kubeconfig "+
					"授予 appreleases 的 create 权限：%w", err)
		}
		return nil, fmt.Errorf("创建变更单失败：%w", err)
	}

	v := toAppReleaseView(created)
	slog.Info("平台首次接管工作负载：已创建零改动变更单",
		"namespace", ref.Namespace, "workload", ref.Workload,
		"appRelease", v.Name, "image", v.Image, "replicas", spec["replicas"])
	return &v, nil
}

// mutateAppRelease 执行一次"读—改—写"，并用 conflict 重试兜住并发。
//
// 为什么不用 SSA：见文件头第 5 条 —— 局部 apply 会把没写出来的可选字段清掉。
// 为什么必须重试：Update 是整对象覆盖，若期间 operator 或别人改过对象，
// apiserver 会回 409；不重试就会把一个"其实只是并发"的情况报成部署失败。
func (k *K8sCluster) mutateAppRelease(
	ctx context.Context,
	dyn dynamic.Interface,
	namespace, name string,
	mutate func(spec map[string]interface{}) error,
) error {
	res := dyn.Resource(appReleaseGVR).Namespace(namespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj, err := res.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		spec, ok := obj.Object["spec"].(map[string]interface{})
		if !ok || spec == nil {
			return fmt.Errorf("变更单 %s/%s 的 spec 为空，结构异常", namespace, name)
		}
		// mutate 返回的非 conflict 错误不会被 RetryOnConflict 重试（它只认 409），
		// 因此参数校验类错误可以直接从这里返回。
		if err := mutate(spec); err != nil {
			return err
		}
		obj.Object["spec"] = spec
		if _, err := res.Update(ctx, obj, metav1.UpdateOptions{FieldManager: fieldManagerPlatform}); err != nil {
			return err
		}
		return nil
	})
}

// --------------------------------------------------------------------------- 工具

// toAppReleaseView 从 unstructured 里取出平台关心的字段。
// 取不到的字段留零值 —— 不猜、不用别处的值补。
func toAppReleaseView(obj *unstructured.Unstructured) appReleaseView {
	v := appReleaseView{
		Name:               obj.GetName(),
		Namespace:          obj.GetNamespace(),
		Generation:         obj.GetGeneration(),
		TargetDeployment:   nestedString(obj, "spec", "targetDeployment"),
		Image:              nestedString(obj, "spec", "image"),
		ContainerName:      nestedString(obj, "spec", "containerName"),
		Strategy:           nestedString(obj, "spec", "strategy"),
		Phase:              nestedString(obj, "status", "phase"),
		Message:            nestedString(obj, "status", "message"),
		ObservedGeneration: nestedInt64(obj, "status", "observedGeneration"),
	}
	if r, found, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas"); found {
		rr := int32(r)
		v.Replicas = &rr
	}
	return v
}

func nestedString(obj *unstructured.Unstructured, fields ...string) string {
	s, _, _ := unstructured.NestedString(obj.Object, fields...)
	return s
}

func nestedInt64(obj *unstructured.Unstructured, fields ...string) int64 {
	n, _, _ := unstructured.NestedInt64(obj.Object, fields...)
	return n
}

// hpaFor 查有没有 HPA 在伸缩该工作负载，返回 HPA 名（没有则空串）。
//
// 必须查：一旦平台声明了 replicas，operator 与 HPA 就会互相覆盖，Pod 反复重建。
// 用 autoscaling/v1 而不是 v2 —— v1 自 K8s 1.0 就存在，本集群是 1.22，两者都支持，
// 但 v1 更不容易在别的老集群上失效。
func hpaFor(ctx context.Context, cs kubernetes.Interface, namespace, workload string) (string, error) {
	if cs == nil || namespace == "" || workload == "" {
		return "", nil
	}
	list, err := cs.AutoscalingV1().HorizontalPodAutoscalers(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", err
	}
	for i := range list.Items {
		ref := list.Items[i].Spec.ScaleTargetRef
		// HPA v1 里 Kind 留空等同于 Deployment
		if ref.Name == workload && (ref.Kind == "" || ref.Kind == "Deployment") {
			return list.Items[i].Name, nil
		}
	}
	return "", nil
}

// crPhaseFailedError 把 CR 的终态失败翻译成可操作的话。
func crPhaseFailedError(v appReleaseView) error {
	if strings.Contains(v.Message, "不存在") {
		return fmt.Errorf("变更单 %s/%s 失败：%s（目标工作负载出现后会自动恢复）",
			v.Namespace, v.Name, v.Message)
	}
	if strings.Contains(v.Message, "不存在名为") || strings.Contains(v.Message, "Spec") {
		return fmt.Errorf("变更单 %s/%s 的期望状态被集群拒绝：%s（需修正发布参数后重试）",
			v.Namespace, v.Name, v.Message)
	}
	return fmt.Errorf("变更单 %s/%s 下发失败：%s", v.Namespace, v.Name, v.Message)
}

// mergeEnvIntoSpec 把平台的服务参数合并进 spec.env。
//
// 用 listType=map（以 name 为键）的合并语义：只新增或覆盖这里列出的变量，
// Deployment 上其他管理者注入的变量不受影响。
// 这**不是**全量替换 —— 平台没管的变量不会被删掉。
func mergeEnvIntoSpec(spec map[string]interface{}, envVars map[string]string) {
	if len(envVars) == 0 {
		return
	}
	current, _, _ := unstructured.NestedSlice(spec, "env")
	byName := make(map[string]map[string]interface{}, len(current))
	order := make([]string, 0, len(current))
	for _, raw := range current {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		n, _ := m["name"].(string)
		if n == "" {
			continue
		}
		if _, seen := byName[n]; !seen {
			order = append(order, n)
		}
		byName[n] = m
	}
	for name, value := range envVars {
		if _, seen := byName[name]; !seen {
			order = append(order, name)
		}
		byName[name] = map[string]interface{}{"name": name, "value": value}
	}
	// 顺序按"原有顺序 + 新增追加"，避免每次下发都因为 map 遍历顺序不同而改动 CR、
	// 无谓地推进 generation。
	out := make([]interface{}, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	_ = unstructured.SetNestedSlice(spec, out, "env")
}

// rolloutProgress 从 Deployment 的 status 推出"走到哪一步了"。
//
// 进度**只取自 Deployment**（它是真相）。CR 的 phase 只用来判断终态/失败，
// 因为"patch 被接受"离"Pod 起来了"还差得很远。
//
// 阶段名必须与部署单预置的四个步骤逐字一致（拉取镜像 / 更新工作负载 / 就绪探针 / 流量接入），
// 否则 service 层的 markStep 找不到对应的行，界面上会一直卡在第一步。
func rolloutProgress(dep *appsv1.Deployment) (stage string, pct uint8, done bool, msg string) {
	desired := int32(1)
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}

	if dep.Status.ObservedGeneration < dep.Generation {
		return "更新工作负载", 20, false,
			fmt.Sprintf("等待 Deployment 控制器处理新 spec（generation=%d, observedGeneration=%d）",
				dep.Generation, dep.Status.ObservedGeneration)
	}
	if dep.Status.UpdatedReplicas < desired {
		if dep.Status.UpdatedReplicas == 0 {
			return "拉取镜像", 30, false,
				fmt.Sprintf("正在拉起新副本（已更新 0/%d，通常在拉取镜像）", desired)
		}
		return "更新工作负载", 55, false,
			fmt.Sprintf("已更新 %d/%d 个副本", dep.Status.UpdatedReplicas, desired)
	}
	if dep.Status.Replicas > dep.Status.UpdatedReplicas {
		return "更新工作负载", 65, false,
			fmt.Sprintf("正在终止 %d 个旧副本", dep.Status.Replicas-dep.Status.UpdatedReplicas)
	}
	if dep.Status.AvailableReplicas < dep.Status.UpdatedReplicas {
		return "就绪探针", 80, false,
			fmt.Sprintf("可用副本 %d/%d，等待新副本通过就绪探针",
				dep.Status.AvailableReplicas, dep.Status.UpdatedReplicas)
	}
	if dep.Status.AvailableReplicas < desired {
		return "就绪探针", 90, false,
			fmt.Sprintf("可用副本 %d/%d", dep.Status.AvailableReplicas, desired)
	}
	return "流量接入", 100, true,
		fmt.Sprintf("全部 %d 个副本均已更新并可用", dep.Status.UpdatedReplicas)
}

// rolloutVerdict 一轮轮询的结论。
type rolloutVerdict int

const (
	// verdictWaiting：还没到能收工的时候（副本没齐 / 还在拉镜像 / 镜像没换）。
	verdictWaiting rolloutVerdict = iota
	// verdictAppReleasePending：**Deployment 说自己全部就绪，但那是旧版本的就绪** ——
	// 变更单刚下发、operator 还没处理，我们要的那版镜像还不在 spec 里。继续等，不是失败。
	verdictAppReleasePending
	// verdictComplete：期望镜像已在 spec 里，且副本全部更新并可用。
	verdictComplete
	// verdictReverted：变更单**已被 operator 处理过**，Deployment 却仍不是期望镜像 ——
	// 有人把它改回去了，这时候报成功就是假成功。
	verdictReverted
)

/*
judgeRollout 由 Deployment 与变更单**共同**裁决这一轮该怎么判。抽成纯函数是因为
最容易错的那一处恰好没有集群也能测：

	`Deployment.status` 只说"某个版本全部就绪"，而**旧版本一直就绪**。于是只看它的话，
	在 operator 把新 spec 写进 Deployment 之前，第一轮轮询就会判成"完成" → 紧接着比对镜像
	发现是旧 tag → 报失败。实测（2026-10-14）：首次接管一个已在跑的服务，部署单 300ms 就
	报「已滚动完成，但镜像仍是旧 tag」，而变更单其实已下发、几秒后真的滚成了新版 ——
	用户看到的就是"报失败但其实部署成功"。

分辨"还没轮到我们"与"被压回旧版"的依据只能取变更单自己：operator 有没有读进这次下发
（`status.observedGeneration` 追上 `metadata.generation`）。
*/
func judgeRollout(
	dep *appsv1.Deployment,
	cr *appReleaseView,
	wantImage string,
	containerName string,
) (rolloutVerdict, string, error) {
	_, got, err := deploymentImage(dep, containerName)
	if err != nil {
		return verdictWaiting, "", err
	}
	if got == wantImage {
		// 镜像对了，剩下的交给 Deployment 自己：副本没齐就继续等。
		if _, _, done, _ := rolloutProgress(dep); done {
			return verdictComplete, "", nil
		}
		return verdictWaiting, "", nil
	}
	// 镜像还不是期望值：看变更单被处理过没有。
	if cr == nil || cr.ObservedGeneration < cr.Generation {
		return verdictAppReleasePending, fmt.Sprintf(
			"变更单已下发，等待 operator 应用（工作负载当前仍是 %s）", got), nil
	}
	return verdictReverted, fmt.Sprintf(
		"工作负载 %s/%s 已滚动完成，但镜像仍是 %s（期望 %s）："+
			"请检查是否有其他管理者在改这个工作负载",
		dep.Namespace, dep.Name, got, wantImage), nil
}

func deploymentImage(dep *appsv1.Deployment, containerName string) (string, string, error) {

	containers := dep.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return "", "", fmt.Errorf("工作负载 %s/%s 的 Pod 模板里没有任何容器",
			dep.Namespace, dep.Name)
	}
	if containerName == "" {
		return containers[0].Name, containers[0].Image, nil
	}
	for i := range containers {
		if containers[i].Name == containerName {
			return containers[i].Name, containers[i].Image, nil
		}
	}
	names := make([]string, 0, len(containers))
	for i := range containers {
		names = append(names, containers[i].Name)
	}
	return "", "", fmt.Errorf("工作负载 %s/%s 里没有名为 %q 的容器（现有：%s）",
		dep.Namespace, dep.Name, containerName, strings.Join(names, "、"))
}

// notify 安全地回调进度。返回 true 表示应当继续。
//
// 进度**回写失败不中断部署**：集群侧的滚动更新不会因为我们这边写日志失败而停手，
// 中断只会让平台与集群状态失配 —— 那比丢几条进度更糟。
// 真正的取消走 ctx（service 层据此把部署单判为 canceled）。
func notify(progress func(stage string, pct uint8) error, stage string, pct uint8) bool {
	if progress == nil {
		return true
	}
	if err := progress(stage, pct); err != nil {
		slog.Warn("部署进度回写失败（不影响集群侧继续执行）", "stage", stage, "err", err)
	}
	return true
}

// waitContext 等待一个轮询间隔，或在 ctx 结束时立刻返回 ctx 的错误。
// 用 select 而不是 time.Sleep，部署取消才能被及时感知。
func waitContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
