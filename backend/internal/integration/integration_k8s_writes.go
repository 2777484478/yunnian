// 本文件是 K8sCluster 的**写路径**：ApplyImage / Scale / Release / Restart。
//
// 具体"往哪写、为什么这么写"的约束全在 integration_apprelease.go 的文件头，
// 这里只放几个动作的编排：下发 → 等集群确认 → 校验结果真的落地。
//
// 几个动作的等待策略刻意不同，因为它们对用户的意义不同：
//
//	ApplyImage  发布新版本 —— 用户要的是"真的跑起来了"，因此**等到滚动完成**，并回验镜像；
//	Scale       扩缩容     —— 用户要的是"实例数变成 N"，因此等到集群**接受并观察到**新副本数，
//	                          不等 Pod 全部就绪（拉起新 Pod 受镜像拉取影响，那不是"失败了"）；
//	Release     解除纳管   —— 用户要的是"平台别再管它"，因此**只删变更单**、
//	                          不等任何东西：Deployment 一动都不动（见 Release 的注释）；
//	Restart     滚动重启   —— 用户要的是"这一轮重新起来"，等价于 kubectl rollout restart，
//	                          因此确认"滚动确实被触发"（generation 递增且被观察到）即可。
package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// ApplyImage 更新镜像并等待滚动完成。
//
// 语义是"把工作负载换成这个镜像，并且等到真的跑起来"：
// 只提交成功不算数 —— CR 被接受离 Pod 起来还差很远，而用户看到的必须是"新版已在跑"。
func (k *K8sCluster) ApplyImage(ctx context.Context, req ApplyRequest, progress func(string, uint8) error) error {
	if err := req.validateForWrite(); err != nil {
		return err
	}
	dyn, cs, err := k.crClients(ctx, req.ClusterRef)
	if err != nil {
		return err
	}

	handle, err := k.ensureAppRelease(ctx, dyn, cs, req.WorkloadRef)
	if err != nil {
		return err
	}
	if handle.Created {
		// 首次接管是实质性变更（这个工作负载从此由 operator 托管），必须留下痕迹。
		// 目前只落到服务端日志：progress 回调只能表达 (阶段, 百分比)，塞不进一句话，
		// 硬塞（例如 stage="")会让 service 层的 markStep 找不到步骤行、并写出非法进度值。
		slog.Info("本次发布首次接管工作负载", "appRelease", handle.View.Name,
			"namespace", handle.View.Namespace, "workload", req.Workload)
	}
	if handle.HPAName != "" && req.Replicas > 0 {
		// 不报错：镜像更新本身与 HPA 无关，不该因为副本数而挡住整次发布。
		// 但也不能装作写进去了 —— 副本数确实没有下发，记下来以备排查。
		slog.Warn("工作负载由 HPA 伸缩，本次未声明副本数",
			"namespace", req.WorkloadRef.Namespace, "workload", req.Workload,
			"hpa", handle.HPAName, "requestedReplicas", req.Replicas)
	}

	if err := k.mutateAppRelease(ctx, dyn, handle.View.Namespace, handle.View.Name,
		func(spec map[string]interface{}) error {
			spec["image"] = req.Image
			if req.Replicas > 0 && handle.HPAName == "" {
				spec["replicas"] = int64(req.Replicas)
			}
			if req.Strategy != "" {
				spec["strategy"] = req.Strategy
			}
			// 平台"服务参数"里的环境变量在这里落地。
			// listType=map（以 name 为键）的最小合并：只动这里的变量，其他管理者的变量不受影响。
			mergeEnvIntoSpec(spec, req.EnvVars)
			return nil
		}); err != nil {
		return fmt.Errorf("下发变更单 %s/%s 失败：%w", handle.View.Namespace, handle.View.Name, err)
	}

	// 变更单已被接受，operator 随即开始滚动 —— 这一步之后才是真正的拉镜像/换 Pod。
	notify(progress, "拉取镜像", 20)

	return k.waitRollout(ctx, dyn, cs, handle, req.WorkloadRef, req.Image, progress)
}

// waitRollout 轮询直到滚动完成、变更单进入终态失败、超时或 ctx 取消。
//
// 进度取自 Deployment（真相），**结论由 Deployment 与变更单共同裁决**（见 judgeRollout）：
// 只看 Deployment 会把"旧版本还就绪着"当成"我们要的那版好了"。
func (k *K8sCluster) waitRollout(
	ctx context.Context,
	dyn dynamic.Interface,
	cs kubernetes.Interface,
	handle *appReleaseHandle,
	ref WorkloadRef,
	wantImage string,
	progress func(string, uint8) error,
) error {
	deadline := time.Now().Add(k.deployTimeout)
	lastStage, lastPct := "", uint8(0)
	lastMsg := ""

	var crView *appReleaseView
	for {
		// 1) 先看变更单：终态失败要立刻中止，不能让用户等到超时。
		//    （TargetNotFound 之类可自愈的失败不算终态，见 terminalFailure。）
		//
		//    这一轮读到的 CR 要留着：判"完成"时得靠它的 observedGeneration 分辨
		//    "operator 还没处理这次下发"与"处理了、但被别人压回旧版"。
		crView = nil
		if view, err := k.getAppRelease(ctx, dyn, handle.View.Namespace, handle.View.Name); err == nil {
			if terminal, _ := view.terminalFailure(); terminal {
				return crPhaseFailedError(view)
			}
			crView = &view
		}

		// 2) 再看工作负载：进度与"完成"只认它。
		dep, err := cs.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Workload, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("滚动过程中工作负载 %s/%s 消失了", ref.Namespace, ref.Workload)
			}
			return fmt.Errorf("读取工作负载 %s/%s 状态失败：%w", ref.Namespace, ref.Workload, err)
		}

		stage, pct, _, msg := rolloutProgress(dep)
		lastMsg = msg

		// 3) 结论由 Deployment 与变更单**共同**裁决（见 judgeRollout）。
		//    ⚠️ 只看 Deployment 是错的：它说"全部就绪"时，就绪的可能是**旧版本**——
		//    operator 把新 spec 写进去之前，第一轮轮询就会判成完成，把一次成功的发布
		//    报成「已滚动完成，但镜像仍是旧 tag」（2026-10-14 实测）。
		verdict, vmsg, verr := judgeRollout(dep, crView, wantImage, handle.View.ContainerName)
		if verr != nil {
			return verr
		}
		switch verdict {
		case verdictComplete:
			notify(progress, "流量接入", 100)
			return nil
		case verdictReverted:
			// 变更单已被 operator 处理过，Deployment 却还不是期望镜像 —— 真失败。
			return errors.New(vmsg)
		case verdictAppReleasePending:
			// 还没轮到我们：覆盖掉 Deployment 那份"旧版本就绪"的进度文案，继续等。
			// 注意**不能 continue**：下面的超时判定必须照常跑，否则会无限空转。
			stage, pct = "等待下发", 20
			lastMsg = vmsg
		}
		if stage != lastStage || pct != lastPct {
			notify(progress, stage, pct)
			lastStage, lastPct = stage, pct
		}

		// 4) 集群自己已经判定"这次滚动不可能再自己好了" → 立刻失败，不必等满 deployTimeout。
		//    判据是 Deployment 的 `Progressing=False / reason=ProgressDeadlineExceeded`
		//    （见 ErrRolloutStuck）。放在结论判定**之后**是刻意的：一个已经真的滚完的
		//    Deployment 不该因为一条陈旧的条件被报成失败。
		//    在不自动回滚的前提下，这段等待是纯损失，没有理由不省。
		if stuckMsg, ok := rolloutDeadlineExceeded(dep); ok {
			return fmt.Errorf("%w（工作负载 %s/%s，progressDeadlineSeconds=%s）：%s",
				ErrRolloutStuck, ref.Namespace, ref.Workload,
				progressDeadlineSecondsOf(dep), stuckMsg)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("等待工作负载 %s/%s 滚动完成超时（%s）：%s",
				ref.Namespace, ref.Workload, k.deployTimeout, lastMsg)
		}
		if err := waitContext(ctx, appReleasePollInterval); err != nil {
			return err
		}
	}
}

// rolloutDeadlineExceeded 判断集群是否已把这次滚动判成"无进展"。
//
// 只认 `Progressing=False / reason=ProgressDeadlineExceeded` 这一种组合。
// 别的 reason 也可能是 False（例如 Deployment 被 pause），语义完全不同，
// 一起收进来会把"暂停"误判成"失败" —— 那是最难解释的一类误报。
func rolloutDeadlineExceeded(dep *appsv1.Deployment) (string, bool) {
	for i := range dep.Status.Conditions {
		c := &dep.Status.Conditions[i]
		if c.Type != appsv1.DeploymentProgressing || c.Status != corev1.ConditionFalse {
			continue
		}
		if c.Reason != "ProgressDeadlineExceeded" {
			continue
		}
		msg := strings.TrimSpace(c.Message)
		if msg == "" {
			msg = "新 Pod 在进度截止时间内始终没有就绪"
		}
		return msg, true
	}
	return "", false
}

// progressDeadlineSecondsOf 把截止秒数渲染成可读文本。
//
// 未显式设置时 k8s 默认 600s —— 这时**不能**显示 "0s"，那会让人以为"立即超时"，
// 而真实语义是"10 分钟没进展才判死"。把默认值写出来才不误导。
func progressDeadlineSecondsOf(dep *appsv1.Deployment) string {
	if d := dep.Spec.ProgressDeadlineSeconds; d != nil {
		return fmt.Sprintf("%ds", *d)
	}
	return "600s（k8s 默认值）"
}

// Scale 调整副本数。
//
// 与 ApplyImage 不同：它不换镜像、不等 Pod 就绪，只把"实例数变成 N"这件事落实到位。
// 副本数通过变更单声明 —— 声明之后 operator 会接管该字段，手工改会被拉回，
// 这是"平台纳管"应有的语义，也是后续扩缩容能生效的前提。
func (k *K8sCluster) Scale(ctx context.Context, ref WorkloadRef, replicas int) error {
	if replicas < 0 {
		return errors.New("副本数不能为负数")
	}
	dyn, cs, err := k.crClients(ctx, ref.ClusterRef)
	if err != nil {
		return err
	}
	handle, err := k.ensureAppRelease(ctx, dyn, cs, ref)
	if err != nil {
		return err
	}
	if handle.HPAName != "" {
		// 这里必须硬拒绝：声明 replicas 会与 HPA 互相覆盖，Pod 会反复重建，
		// 而且从两侧日志看都"正常"，是最难排查的一类。
		return fmt.Errorf(
			"工作负载 %s/%s 由 HPA %s 伸缩，平台不能直接声明副本数："+
				"声明后会与 HPA 互相覆盖、Pod 反复重建。"+
				"请改为调整该 HPA 的 min/max，或先移除 HPA",
			ref.Namespace, ref.Workload, handle.HPAName)
	}

	if err := k.mutateAppRelease(ctx, dyn, handle.View.Namespace, handle.View.Name,
		func(spec map[string]interface{}) error {
			spec["replicas"] = int64(replicas)
			return nil
		}); err != nil {
		return fmt.Errorf("下发变更单 %s/%s 失败：%w", handle.View.Namespace, handle.View.Name, err)
	}

	return k.waitScaled(ctx, dyn, cs, handle, ref, replicas)
}

// Release 解除纳管：删掉平台下发的变更单（AppRelease），**完全不碰 Deployment**。
//
// 依据 operator 源码（`apprelease_controller.go` 的 reconcileDelete）：AppRelease 带 finalizer，
// 但删除时的清理动作**只摘掉 Deployment 上的接管注解**（`apps.example.com/managed-by`），
// 既不删 Deployment，也不改它的镜像与副本 —— 所以"取消纳管"之后集群里那个服务照旧在跑。
//
// 这正是"平台只是发布通道"该有的语义：与 `Scale(ref, 0)` 的区别是本质的，
// 后者把副本声明成 0，是真的停服。
//
// 只认 `spec.targetDeployment` 反查出来的那一张（与 ensureAppRelease 同一套口径）：
//
//	0 张 —— 平台本来就没接管过它（手工建的服务实例、或变更单已被删）：无事可做，算成功；
//	多张 —— 两张单会互相覆盖且不报错，不知道删哪一张，硬拒绝让人处理。
//
// ⚠️ 已知假设：平台认"一个工作负载一张单"，且那张单是平台下发的。`ensureAppRelease`
// 接管时并不区分单是谁建的（同名同目标即认），所以这里删的也可能是别人建的同一张单；
// 在当前部署形态下平台是这张单唯一的写入方，先按此实现。要更严就得给变更单加归属标记，
// 并在接管时拒绝认领别人的单 —— 那是另一个决定，不在这里偷偷做。
func (k *K8sCluster) Release(ctx context.Context, ref WorkloadRef) error {
	if err := ref.validate(); err != nil {
		return err
	}
	dyn, _, err := k.crClients(ctx, ref.ClusterRef)
	if err != nil {
		return err
	}

	existing, err := k.findAppReleasesByTarget(ctx, dyn, ref.Namespace, ref.Workload)
	if err != nil {
		return err
	}
	switch len(existing) {
	case 0:
		return nil
	case 1:
	default:
		names := make([]string, 0, len(existing))
		for _, v := range existing {
			names = append(names, v.Namespace+"/"+v.Name)
		}
		return fmt.Errorf(
			"工作负载 %s/%s 被多个变更单同时接管（%s）：不知道该删哪一张，"+
				"请先人工合并或删除多余的变更单",
			ref.Namespace, ref.Workload, strings.Join(names, "、"))
	}

	cr := existing[0]
	if err := dyn.Resource(appReleaseGVR).Namespace(cr.Namespace).
		Delete(ctx, cr.Name, metav1.DeleteOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			// 并发：别人刚删掉。目标状态（这张单没了）已经达成。
			return nil
		}
		return fmt.Errorf("删除变更单 %s/%s 失败：%w", cr.Namespace, cr.Name, err)
	}
	slog.Info("已解除纳管：变更单已删除（Deployment 的镜像与副本保持不变）",
		"namespace", cr.Namespace, "appRelease", cr.Name, "workload", ref.Workload)
	return nil
}

// waitScaled 等到集群**接受并观察到**目标副本数。
//
// 只验 spec 层，不等 availableReplicas 到齐：扩到 N 副本时新 Pod 要拉镜像、过探针，
// 可能几分钟；把它算作"扩缩容失败"是错的，那只是"还在起来"。
// 副本起没起来是另一个可观测量，界面上读集群实时状态即可看到。
func (k *K8sCluster) waitScaled(
	ctx context.Context,
	dyn dynamic.Interface,
	cs kubernetes.Interface,
	handle *appReleaseHandle,
	ref WorkloadRef,
	replicas int,
) error {
	deadline := time.Now().Add(appReleaseScaleTimeout)
	last := ""
	for {
		if view, err := k.getAppRelease(ctx, dyn, handle.View.Namespace, handle.View.Name); err == nil {
			if terminal, _ := view.terminalFailure(); terminal {
				return crPhaseFailedError(view)
			}
		}

		dep, err := cs.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Workload, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("读取工作负载 %s/%s 状态失败：%w", ref.Namespace, ref.Workload, err)
		}
		cur := int32(-1)
		if dep.Spec.Replicas != nil {
			cur = *dep.Spec.Replicas
		}
		last = fmt.Sprintf("期望 %d，集群当前 %d（available=%d）", replicas, cur, dep.Status.AvailableReplicas)

		// spec 已到目标数，且 Deployment 控制器已经看到这次改动 → 变更确实落地了。
		if cur == int32(replicas) && dep.Status.ObservedGeneration >= dep.Generation {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待工作负载 %s/%s 副本数生效超时（%s）：%s",
				ref.Namespace, ref.Workload, appReleaseScaleTimeout, last)
		}
		if err := waitContext(ctx, appReleasePollInterval); err != nil {
			return err
		}
	}
}

// Restart 滚动重启（不换镜像）。
//
// ⚠️ 这是**唯一**不通过变更单下发、而是直接写 Deployment 的操作，原因与安全性如下：
//
//	AppRelease 的 spec 里没有"重启"这个字段（只有 targetDeployment / namespace / image /
//	replicas / containerName / env / strategy），所以走 CR 无从表达"重启一次"。
//	可行且标准的做法与 `kubectl rollout restart` 完全一致：改 Pod 模板上的
//	`kubectl.kubernetes.io/restartedAt` 注解，触发一次滚动更新。
//
//	它不会与 operator 打架，三条依据（改动前请逐条复核）：
//	  1. operator 的 SSA 只声明 spec.template.spec.containers（+ replicas / strategy）与三个
//	     apps.example.com 注解，**从不声明 spec.template.metadata**；
//	  2. operator 的漂移比对函数 deploymentDiffers 也不比对 Pod 模板注解
//	     → 这个注解不会被判成漂移而被拉回；
//	  3. 用 Merge Patch 只提交这一个 key，不覆盖 Deployment 上任何其他字段。
//
//	反过来：如果哪天 operator 开始接管 Pod 模板注解，这里必须改成走 CR。
//
// 这里**刻意不做"首次接管"**：重启不需要变更单，而因为一次重启就顺手创建 CR
// （等于接管 image / replicas / strategy）是远超用户意图的副作用。
func (k *K8sCluster) Restart(ctx context.Context, ref WorkloadRef) error {
	if err := ref.validate(); err != nil {
		return err
	}
	_, cs, err := k.crClients(ctx, ref.ClusterRef)
	if err != nil {
		return err
	}

	dep, err := cs.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Workload, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("集群里没有工作负载 %s/%s，无法重启（请确认环境 namespace 是否配对）",
				ref.Namespace, ref.Workload)
		}
		return fmt.Errorf("读取工作负载 %s/%s 失败：%w", ref.Namespace, ref.Workload, err)
	}
	genBefore := dep.Generation

	// RFC3339 秒级精度足够：同一秒内连点两次重启不会被当成两次不同变更，
	// 而这正是想要的（第二次点击本来就是幂等的）。
	stamp := time.Now().UTC().Format(time.RFC3339)
	patch := fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`,
		annotationRestartedAt, stamp)

	if _, err := cs.AppsV1().Deployments(ref.Namespace).
		Patch(ctx, ref.Workload, types.MergePatchType, []byte(patch),
			metav1.PatchOptions{FieldManager: fieldManagerPlatform}); err != nil {
		if apierrors.IsForbidden(err) {
			return fmt.Errorf("当前凭据无权修改工作负载 %s/%s（需要 deployments 的 patch 权限）：%w",
				ref.Namespace, ref.Workload, err)
		}
		return fmt.Errorf("触发滚动重启失败：%w", err)
	}

	// 等 Deployment 控制器确认：generation 递增且已被观察到。
	// 这一步是必要的 —— 注解写进去了不等于滚动更新被触发了（例如对象被 pause 时就不会）。
	deadline := time.Now().Add(appReleaseScaleTimeout)
	for {
		cur, err := cs.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Workload, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("读取工作负载 %s/%s 状态失败：%w", ref.Namespace, ref.Workload, err)
		}
		if cur.Generation > genBefore && cur.Status.ObservedGeneration >= cur.Generation {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf(
				"已写入重启注解，但工作负载 %s/%s 在 %s 内没有开始新的滚动（generation 仍是 %d）："+
					"请检查该 Deployment 是否被 pause 或有其他控制器在干预",
				ref.Namespace, ref.Workload, appReleaseScaleTimeout, cur.Generation)
		}
		if err := waitContext(ctx, appReleasePollInterval); err != nil {
			return err
		}
	}
}

// --------------------------------------------------------------------------- 内部

// getAppRelease 读一个变更单的当前视图。
func (k *K8sCluster) getAppRelease(ctx context.Context, dyn dynamic.Interface, namespace, name string) (appReleaseView, error) {
	obj, err := dyn.Resource(appReleaseGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return appReleaseView{}, err
	}
	return toAppReleaseView(obj), nil
}

// validateForWrite 校验写请求的定位与取值。
//
// 早失败：这些条件后端（CRD schema / apiserver）也会拒，
// 但在平台侧拒能给出"该改哪里"的话，而不是一串 schema 校验错误。
func (req ApplyRequest) validateForWrite() error {
	if err := req.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(req.Image) == "" {
		return errors.New("目标镜像为空")
	}
	if req.Replicas < 0 {
		return errors.New("副本数不能为负数")
	}
	switch req.Strategy {
	case "", strategyRollingUpdate, strategyRecreate:
	default:
		// CRD 的 strategy 是枚举（RollingUpdate / Recreate），写别的值会被 apiserver 拒掉。
		return fmt.Errorf("不支持的更新策略 %q（只能是 RollingUpdate 或 Recreate）", req.Strategy)
	}
	return nil
}
