// 本文件是 K8sCluster 的**取证路径**：只读采集"这个工作负载为什么没起来"的证据。
//
// 与 integration_k8s_writes.go 的关系：那条路径负责"把变更做下去"，这条负责
// "做失败之后解释为什么"。两条路径的纪律正好相反 —— 写路径追求"确认真的生效"，
// 取证路径追求"**宁可少拿，不可编**"：拿不到就写进 Notes，绝不填一个像模像样的默认值。
//
// 三条约束（改动前请先读）：
//
//  1. **全程只读**。取证发生在一次已经失败的发布之后，此时任何写动作都是
//     把事故扩大 —— 用户要的是解释，不是平台再动一次手。
//  2. **读不到必须说成读不到**。日志为空与"日志里没有报错"在下游会导出相反结论，
//     所以取不到时进 Notes，而不是留个空串让人以为一切正常。
//  3. **不许 List 整个命名空间**（同 integration_k8s.go 的文件头）：事件按
//     involvedObject.name 逐个 Pod 查，不按 namespace 全量拉。
package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// evidenceLogLines 每个容器最多取多少**行**日志。取尾部：
	// 崩溃的根因几乎总在最后几十行，从头截只会留下 "Starting…" 这类没信息量的前缀。
	//
	// 为什么是 80 而不是更少：JVM 的异常链是**多层嵌套**的，越根本的原因越靠后
	// （`Caused by:` 一行压一行），每层后面还跟着十几行 `at ...` 帧。实测 demobasic
	// 的一次崩溃里，最内层那条 `Caused by: java.lang.IllegalArgumentException:
	// Could not resolve placeholder 'messagelibrary.mq.username'` 落在第 47 行 ——
	// 取 40 行就会把它切掉，只留下中间层那条又长又难读的 BeanCreationException。
	evidenceLogLines = 80
	// evidenceLogBytes 单个容器日志**最终保留**的字节上限。注意方向：保留**尾部**。
	//
	// 定成 32KB 是刻意的：实测一次 Java 崩溃的 80 行约 14.8KB，而有些 `at ...` 行
	// 能长到 300 字节（长类名 + 长 jar 路径）—— 32KB 保证 **80 行永远装得下**，
	// 于是行数（evidenceLogLines）成为唯一的截断依据，字节上限退化成兜底
	// "超长单行"的安全网。原实现是 8KB 且保头部，在 44 行处就断了，把最内层的
	// `Caused by:` 丢掉 —— 那不是"少了一点上下文"，是把答案本身删了。
	evidenceLogBytes = 32 << 10
	// evidenceLogReadCap 从 API **读**多少字节就停（防容器打了超长单行把内存吃光）。
	//
	// 它必须显著大于 evidenceLogBytes：读进来后还要再砍掉头部保尾部，
	// 如果这里就按 16KB 截，等于又在流的开头截了一次，白改。
	evidenceLogReadCap = 1 << 20
	// evidenceMaxPods 最多采集几个 Pod。卡住的 rollout 通常只有一两个新 Pod，
	// 但 Recreate 策略或反复重建时可能有一串 —— 截断，并把"截断了"写进 Notes。
	evidenceMaxPods = 3
	// evidenceMaxEvents 事件条数上限。
	evidenceMaxEvents = 8
)

// DiagnoseFailure 采集发布失败的集群侧证据。全程只读。
func (k *K8sCluster) DiagnoseFailure(ctx context.Context, ref WorkloadRef) (*FailureEvidence, error) {
	if err := ref.validate(); err != nil {
		return nil, err
	}
	cs, _, err := k.clientset(ctx, ref.ClusterRef)
	if err != nil {
		// 集群连不上属于"没读到"，必须报错：返回空证据等于告诉下游"应用没有异常"。
		return nil, err
	}
	out := &FailureEvidence{}

	d, err := cs.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Workload, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// 这一条是**事实**而不是故障：发布在建单到执行之间工作负载被删了，
		// 本身就是最可能的失败原因，值得原样告诉下游。
		out.Notes = append(out.Notes, fmt.Sprintf(
			"命名空间 %s 下没有名为 %s 的 Deployment —— 这次失败发生在工作负载存在之前",
			ref.Namespace, ref.Workload))
		return out, nil
	}
	if err != nil {
		return nil, describeErr(err, ref)
	}
	if d.Spec.Selector == nil {
		out.Notes = append(out.Notes, "该工作负载没有 selector，无法定位它名下的 Pod")
		return out, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("解析工作负载 %s 的选择器失败：%w", ref.Workload, err)
	}
	list, err := cs.CoreV1().Pods(ref.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, fmt.Errorf("读取命名空间 %s 的 Pod 失败：%w", ref.Namespace, err)
	}

	// 只取**未就绪**的 Pod：老的那一版通常还健康地跑着，把它一起喂给模型只会稀释证据，
	// 而且会诱导出"两个版本都正常"这种错误印象。
	notReady := make([]*corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		if !podAllReady(&list.Items[i]) {
			notReady = append(notReady, &list.Items[i])
		}
	}
	if len(notReady) == 0 {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"该工作负载当前 %d 个 Pod 全部就绪，没有采集到容器侧的失败证据"+
				"（这时失败更可能是平台侧等待超时，而不是容器起不来）", len(list.Items)))
		return out, nil
	}
	// 最新的排前面：卡住的 rollout 里，最新那个 Pod 才是本次发布的产物。
	sort.Slice(notReady, func(i, j int) bool {
		return notReady[i].CreationTimestamp.After(notReady[j].CreationTimestamp.Time)
	})
	if len(notReady) > evidenceMaxPods {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"未就绪的 Pod 共 %d 个，只采集了最新的 %d 个", len(notReady), evidenceMaxPods))
		notReady = notReady[:evidenceMaxPods]
	}

	for _, p := range notReady {
		ev, note := k.podEvidence(ctx, cs, ref, p)
		if note != "" {
			out.Notes = append(out.Notes, note)
		}
		out.Pods = append(out.Pods, ev)
	}
	events, note := k.podEvents(ctx, cs, ref, notReady)
	out.Events = events
	if note != "" {
		out.Notes = append(out.Notes, note)
	}
	return out, nil
}

// podAllReady 判断 Pod 是否**每个容器都就绪且在跑**。
//
// 不能只看 `status.phase`：CrashLoopBackOff 的 Pod phase 仍然是 Running，
// 只看 phase 会把一个正在反复崩溃的 Pod 当成健康的，于是取证结果为空。
func podAllReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning || len(p.Status.ContainerStatuses) == 0 {
		return false
	}
	for i := range p.Status.ContainerStatuses {
		cs := &p.Status.ContainerStatuses[i]
		if !cs.Ready || cs.State.Waiting != nil || cs.State.Terminated != nil {
			return false
		}
	}
	return true
}

// podEvidence 采集单个 Pod 的证据。第二个返回值是"这块没拿到"的说明（空串表示都拿到了）。
func (k *K8sCluster) podEvidence(
	ctx context.Context, cs kubernetes.Interface, ref WorkloadRef, p *corev1.Pod,
) (PodEvidence, string) {
	ev := PodEvidence{Name: p.Name, Phase: string(p.Status.Phase), Restarts: podRestarts(p)}

	if st := pickContainerStatus(p); st != nil {
		ev.Container = st.Name
		// 优先级：当前正在等待 > 当前已终止 > 上一次的终止。
		// "正在等待"排最前，因为那正是"起不来"本身；只看 Ready 会漏掉原因。
		switch {
		case st.State.Waiting != nil:
			ev.Reason, ev.Message = st.State.Waiting.Reason, st.State.Waiting.Message
		case st.State.Terminated != nil:
			ev.Reason, ev.Message = st.State.Terminated.Reason, st.State.Terminated.Message
			ev.ExitCode = st.State.Terminated.ExitCode
		case st.State.Running != nil:
			// Running 但没就绪：探针没过，或者还在启动。如实写出来，别省略 ——
			// 省略会让模型以为"没有任何状态信息"。
			ev.Reason = "Running 但未就绪"
		}
		if last := st.LastTerminationState.Terminated; last != nil {
			if ev.ExitCode == 0 {
				ev.ExitCode = last.ExitCode
			}
			if strings.TrimSpace(ev.Reason) == "" || ev.Reason == "Running 但未就绪" {
				ev.Reason, ev.Message = last.Reason, last.Message
			}
		}
	}

	logs, from, lerr := k.podLogsBestEffort(ctx, cs, ref.Namespace, p.Name, ev.Container)
	ev.Logs, ev.LogsFrom = logs, from
	if lerr != nil {
		// 日志是这套证据里信息密度最高的一块，取不到必须说出来 ——
		// 不然下游会把"没日志"理解成"日志里没有报错"。
		return ev, fmt.Sprintf("Pod %s 的容器日志没取到（%v）：这份证据会缺掉最关键的一块", p.Name, lerr)
	}
	return ev, ""
}

// pickContainerStatus 挑出最有诊断价值的容器状态。
//
// 不能直接取 `[0]`：多容器 Pod 里第一个往往是 sidecar（istio-proxy 之类），
// 它崩不崩与业务能不能起来是两件事。
func pickContainerStatus(p *corev1.Pod) *corev1.ContainerStatus {
	best, bestScore := -1, -1
	for i := range p.Status.ContainerStatuses {
		cs := &p.Status.ContainerStatuses[i]
		score := 0
		switch {
		case cs.State.Waiting != nil:
			score = 4
		case cs.State.Terminated != nil:
			score = 3
		case cs.LastTerminationState.Terminated != nil:
			score = 2
		case cs.State.Running != nil && !cs.Ready:
			score = 1
		}
		if score > 0 && cs.RestartCount > 0 {
			score++
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 {
		return nil
	}
	return &p.Status.ContainerStatuses[best]
}

// podLogsBestEffort 先取**上一次实例**的日志，没有内容再取当前的。
//
// 顺序不能反：CrashLoopBackOff 的容器往往在启动阶段就退了，当前实例可能一行都没输出，
// 而崩溃的现场（堆栈、缺哪个配置项）留在上一次那一轮里。
func (k *K8sCluster) podLogsBestEffort(
	ctx context.Context, cs kubernetes.Interface, ns, pod, container string,
) (string, string, error) {
	var lastErr error
	for _, prev := range []bool{true, false} {
		txt, err := k.containerLogTail(ctx, cs, ns, pod, container, prev)
		if err != nil {
			lastErr = err
			continue
		}
		if strings.TrimSpace(txt) == "" {
			continue
		}
		if prev {
			return txt, "previous", nil
		}
		return txt, "current", nil
	}
	if lastErr != nil {
		return "", "", lastErr
	}
	return "", "", nil
}

// containerLogTail 取一个容器的日志尾部。
//
// ⚠️ **这个函数只能保尾部，不能保头部**（2026-10-08 踩过，代价是一次错误的 AI 结论）。
// `io.LimitReader(rc, n)` 是从**流的开头**开始读的，直接拿它截断字节，等价于
// "留下最早的那 n 字节、丢掉最后的那几行" —— 而崩溃的根因（`Caused by:`、
// 退出码、缺哪个配置项）**恰恰在最后几行**。实测 demobasic：`TailLines=60` 给出
// 10418 字节，8KB 的 LimitReader 在第 44 行就停了，而最内层那条
// `Caused by: java.lang.IllegalArgumentException: Could not resolve placeholder
// 'messagelibrary.mq.username'` 在第 47 行（累计 8437 字节）—— **正好被丢掉**。
// 后果不是报错，而是模型拿着一堆 `at org.springframework...` 帧行编出一个
// "端口未监听" 这类看似合理的答案，比不给结论更糟。
//
// 所以：`TailLines` 负责行数上限，`evidenceLogReadCap` 只防超长单行的内存风险，
// 最后一步**必须**从尾部裁到 evidenceLogBytes。
func (k *K8sCluster) containerLogTail(
	ctx context.Context, cs kubernetes.Interface, ns, pod, container string, previous bool,
) (string, error) {
	pod = strings.TrimSpace(pod)
	if pod == "" {
		return "", errors.New("Pod 名为空")
	}
	limit := int64(evidenceLogLines)
	opts := &corev1.PodLogOptions{TailLines: &limit, Previous: previous}
	if container != "" {
		opts.Container = container
	}
	rc, err := cs.CoreV1().Pods(ns).GetLogs(pod, opts).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	buf, err := io.ReadAll(io.LimitReader(rc, evidenceLogReadCap))
	if err != nil {
		return "", err
	}
	return tailLogText(string(buf), evidenceLogBytes), nil
}

// tailLogText 保留日志文本的**尾部** limit 字节，并尽量从行边界开始。
//
// 按字节切可能落在多字节字符中间：ToValidUTF8 会把那半个字符整个删掉，
// 而不是留个乱码 —— 少一个字符远比出现 U+FFFD 强，后者会被模型当成
// "编码坏了" 而忽略整块证据。
func tailLogText(s string, limit int) string {
	s = strings.ToValidUTF8(s, "")
	if limit <= 0 || len(s) <= limit {
		return s
	}
	s = s[len(s)-limit:]
	// 从行边界开始，免得第一行是半截；找不到换行就原样保留
	// （宁可多留半行，也不要因为对齐而少一整行根因）。
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		s = s[i+1:]
	}
	return strings.ToValidUTF8(s, "")
}

// podEvents 采集这些 Pod 上的最近事件。
//
// 它是"Pod 压根没被调度起来"这类失败的**唯一**线索：那种情况下容器从没启动过，
// 日志是空的，只有事件说得清（FailedScheduling / 镜像拉取失败 / 配额不足）。
func (k *K8sCluster) podEvents(
	ctx context.Context, cs kubernetes.Interface, ref WorkloadRef, pods []*corev1.Pod,
) ([]string, string) {
	seen := map[string]bool{}
	out := make([]string, 0, evidenceMaxEvents)
	var lastErr error

	for _, p := range pods {
		// 按 involvedObject.name 逐个查，不 List 整个命名空间：环境 test 的
		// namespace 是 default，里面躺着 47 个与本平台无关的业务负载。
		evs, err := cs.CoreV1().Events(ref.Namespace).List(ctx, metav1.ListOptions{
			FieldSelector: "involvedObject.name=" + p.Name,
		})
		if err != nil {
			lastErr = err
			continue
		}
		sort.Slice(evs.Items, func(i, j int) bool {
			return eventTime(&evs.Items[i]).After(eventTime(&evs.Items[j]))
		})
		for i := range evs.Items {
			e := &evs.Items[i]
			msg := strings.TrimSpace(e.Message)
			if msg == "" {
				continue
			}
			line := fmt.Sprintf("[%s] %s", eventReason(e), msg)
			if seen[line] {
				continue
			}
			seen[line] = true
			out = append(out, line)
			if len(out) >= evidenceMaxEvents {
				return out, ""
			}
		}
	}
	if len(out) == 0 && lastErr != nil {
		// 一条都没拿到**而且**报过错：说清是读不到，而不是"没有事件"。
		return nil, fmt.Sprintf("读取 Pod 事件失败（%v）", lastErr)
	}
	return out, ""
}

// eventReason 取事件的原因词，缺失时退回类型（Normal / Warning）。
func eventReason(e *corev1.Event) string {
	if r := strings.TrimSpace(e.Reason); r != "" {
		return r
	}
	return e.Type
}

// eventTime 取事件时间。三种时间戳的优先级与 kubectl 的口径一致。
func eventTime(e *corev1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if !e.EventTime.IsZero() {
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}
