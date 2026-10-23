package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/2777484478/yunnian/internal/config"
)

// K8sCluster 真实的 K8s 集群操作实现。
//
// 本文件是「部署执行」的唯一真实通道：平台里所有关于集群的读写都从这里出去。
//
// 三条贯穿始终的判据，改动前请先读：
//
//  1. **读必须精确 Get，不许 List 整个命名空间**。环境 test 的 namespace 是 default，
//     里面躺着 47 个与本平台无关的业务 Deployment。一次无过滤的 List 会让平台
//     突然"发现"47 个服务——那不是发现，是误报。
//  2. **读不到与不存在是两件事**。返回 `(state, nil)` 且 Exists=false 表示确认不存在（404）；
//     返回 `(nil, err)` 表示**没读到**（权限/网络/超时）。把后者渲染成"不存在"同样是骗人。
//  3. **读性能接受的代价是"每次重新解析凭据"**。凭据轮换后仍拿旧值去连旧集群，
//     是这类系统最典型的一类故障；为此这里刻意不做 clientset 缓存。
//     若日后列表页成为瓶颈，缓存键必须含凭据版本，而不是只有集群名。
type K8sCluster struct {
	// clusters 只作静态兜底：装配层没有提供 DB 来源时（单元测试、纯配置文件部署），
	// 从这里按 key 找 kubeconfig 文件路径或走 in-cluster。
	clusters []config.K8sCluster
	// readTimeout 单次读操作的超时，来自 integration.kubernetes.timeout。
	readTimeout time.Duration
	// deployTimeout 等待滚动完成的总超时，来自 runner.deployTimeout。
	deployTimeout time.Duration
	// source 运行期凭据来源。非 nil 时**它是唯一真相**，不再看 clusters。
	// 两种来源混用会让"这个集群到底从哪来的"变成一个要翻两处才能回答的问题。
	source KubeconfigSource
}

// NewK8sCluster 构造。
func NewK8sCluster(cfg config.KubernetesConfig, deployTimeout time.Duration, source KubeconfigSource) *K8sCluster {
	read := cfg.Timeout.D()
	if read <= 0 {
		read = 15 * time.Second
	}
	if deployTimeout <= 0 {
		deployTimeout = 15 * time.Minute
	}
	return &K8sCluster{clusters: cfg.Clusters, readTimeout: read, deployTimeout: deployTimeout, source: source}
}

// Name 实现名。
func (k *K8sCluster) Name() string { return "kubernetes" }

// --------------------------------------------------------------------------- 连接

// clientset 按引用构造客户端。
func (k *K8sCluster) clientset(ctx context.Context, ref ClusterRef) (kubernetes.Interface, *rest.Config, error) {
	rc, err := k.restConfigFor(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, nil, fmt.Errorf("创建 K8s 客户端失败：%w", err)
	}
	return cs, rc, nil
}

// restConfigFor 解析出该引用对应的连接配置。
func (k *K8sCluster) restConfigFor(ctx context.Context, ref ClusterRef) (*rest.Config, error) {
	if k.source != nil {
		raw, err := k.source(ctx, ref)
		if err != nil {
			return nil, err
		}
		if len(strings.TrimSpace(string(raw))) == 0 {
			return nil, fmt.Errorf("集群 %q 没有可用的 kubeconfig 凭据", ref.Cluster)
		}
		return NewRestConfig(raw, k.readTimeout)
	}
	return k.staticRestConfig(ref)
}

// staticRestConfig 没有 DB 来源时，退回 config.yaml 的集群条目。
func (k *K8sCluster) staticRestConfig(ref ClusterRef) (*rest.Config, error) {
	if ref.EnvID != nil {
		// 环境自带的凭据只存在平台库里，配置文件的静态路径够不着。
		return nil, errors.New("该环境使用独立 kubeconfig，需要平台数据库提供凭据来源")
	}
	var entry *config.K8sCluster
	for i := range k.clusters {
		if k.clusters[i].Key == ref.Cluster {
			entry = &k.clusters[i]
			break
		}
	}
	if entry == nil {
		return nil, fmt.Errorf("集群 %q 未在平台接入（config.yaml 的 integration.kubernetes.clusters 里也没有该 key）", ref.Cluster)
	}
	switch {
	case strings.TrimSpace(entry.Kubeconfig) != "":
		raw, err := os.ReadFile(entry.Kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("读取集群 %q 的 kubeconfig 文件失败：%w", ref.Cluster, err)
		}
		return NewRestConfig(raw, k.readTimeout)
	case strings.TrimSpace(entry.APIServer) != "":
		// 只填了地址没有凭据：连上去也会被 401 拒掉，不如把话说在前面。
		return nil, fmt.Errorf("集群 %q 只配置了 apiServer、没有 kubeconfig，无法鉴权", ref.Cluster)
	default:
		rc, err := rest.InClusterConfig()
		if err != nil {
			return nil, fmt.Errorf("集群 %q 未配置 kubeconfig，且当前不在集群内运行：%w", ref.Cluster, err)
		}
		rc.Timeout = k.readTimeout
		return rc, nil
	}
}

// --------------------------------------------------------------------------- 读

// Describe 查询工作负载当前状态。
//
// 用 Deployment 而不是 AppRelease CR：CR 的 status 只描述"operator 应用成功"，
// 不代表 Pod 起来了；而且集群里那批手工 kubectl apply 建的 Deployment 根本没有 CR。
// **Deployment 是真相，CR 只是变更通道。**
func (k *K8sCluster) Describe(ctx context.Context, ref WorkloadRef) (*WorkloadState, error) {
	if err := ref.validate(); err != nil {
		return nil, err
	}
	cs, _, err := k.clientset(ctx, ref.ClusterRef)
	if err != nil {
		return nil, err
	}
	d, err := cs.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Workload, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return &WorkloadState{
			Exists:  false,
			Message: fmt.Sprintf("命名空间 %s 下没有名为 %s 的 Deployment", ref.Namespace, ref.Workload),
		}, nil
	}
	if err != nil {
		return nil, describeErr(err, ref)
	}
	return deploymentState(d), nil
}

// Pods 列出工作负载下的 Pod。
//
// 先读 Deployment 的 selector 再按它过滤，而不是按名字前缀猜——
// 名字前缀匹配会把"刚好同前缀的另一个服务的 Pod"也算进来。
func (k *K8sCluster) Pods(ctx context.Context, ref WorkloadRef) ([]PodInfo, error) {
	if err := ref.validate(); err != nil {
		return nil, err
	}
	cs, _, err := k.clientset(ctx, ref.ClusterRef)
	if err != nil {
		return nil, err
	}
	d, err := cs.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Workload, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// 工作负载不在，就是没有 Pod。返回空而不是错误：这是事实，不是故障。
		return []PodInfo{}, nil
	}
	if err != nil {
		return nil, describeErr(err, ref)
	}
	if d.Spec.Selector == nil {
		return []PodInfo{}, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("解析工作负载 %s 的选择器失败：%w", ref.Workload, err)
	}
	list, err := cs.CoreV1().Pods(ref.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, fmt.Errorf("读取命名空间 %s 的 Pod 失败：%w", ref.Namespace, err)
	}

	// 用量只在装了 metrics-server 时才有。读不到就**留空字符串**，
	// 前端会显示 "—"；编一个数字出来等于让用户按假数据判断要不要扩容。
	usage := k.podUsage(ctx, cs, ref.Namespace)

	out := make([]PodInfo, 0, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		item := PodInfo{
			Name:     p.Name,
			Status:   podStatus(p),
			Node:     p.Spec.NodeName,
			Restarts: podRestarts(p),
			Age:      humanAge(time.Since(p.CreationTimestamp.Time)),
			IP:       p.Status.PodIP,
		}
		if u, ok := usage[p.Name]; ok {
			item.CPU, item.Mem = u.cpu, u.mem
		}
		out = append(out, item)
	}
	// 名字排序：否则同一批 Pod 每次刷新顺序都可能变，看着像在乱跳。
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// PodLogs 读取某个 Pod 的日志（tail 最近 n 行）。
func (k *K8sCluster) PodLogs(ctx context.Context, ref WorkloadRef, pod string, tailLines int) (io.ReadCloser, error) {
	if err := ref.validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(pod) == "" {
		return nil, errors.New("Pod 名不能为空")
	}
	cs, _, err := k.clientset(ctx, ref.ClusterRef)
	if err != nil {
		return nil, err
	}
	opts := &corev1.PodLogOptions{}
	if tailLines > 0 {
		n := int64(tailLines)
		opts.TailLines = &n
	}
	rc, err := cs.CoreV1().Pods(ref.Namespace).GetLogs(pod, opts).Stream(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("Pod %s 不存在（可能已被重建，请刷新后重选）", pod)
		}
		return nil, fmt.Errorf("读取 Pod %s 日志失败：%w", pod, err)
	}
	return rc, nil
}

// Nodes 查询集群节点与容量。
func (k *K8sCluster) Nodes(ctx context.Context, ref ClusterRef) (*ClusterState, error) {
	cs, _, err := k.clientset(ctx, ref)
	if err != nil {
		return nil, err
	}
	ver, err := cs.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("%s", classifyK8sErrText(err))
	}
	list, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			return nil, fmt.Errorf("当前凭据无权列出节点，无法读取集群容量")
		}
		return nil, fmt.Errorf("读取节点列表失败：%w", err)
	}

	out := &ClusterState{Nodes: len(list.Items), Version: strings.TrimSpace(ver.GitVersion)}
	// 容量分母用 allocatable 而不是 capacity：capacity 含系统预留，
	// 拿它算百分比会系统性偏低，看着"很闲"其实已经吃满。
	//
	// 逐节点收集而不是只累加总量：汇总回答不了"哪台压力大"，
	// 而那才是加节点 / 迁 Pod 时的依据（2026-10-15）。
	// metrics 同样按节点名索引 —— 整体读到了不等于每台都读到了。
	usageByNode, metricsOK := k.nodeUsageByNode(ctx, cs)

	var cpuCap, memCap, usedCPU, usedMem float64
	out.NodeList = make([]NodeStat, 0, len(list.Items))
	for i := range list.Items {
		n := list.Items[i]
		var ncpu float64
		var nmem uint64
		if q, ok := n.Status.Allocatable[corev1.ResourceCPU]; ok {
			ncpu = q.AsApproximateFloat64()
		}
		if q, ok := n.Status.Allocatable[corev1.ResourceMemory]; ok {
			nmem = uint64(q.Value())
		}
		cpuCap += ncpu
		memCap += float64(nmem)

		st := NodeStat{
			Name: n.Name, Ready: nodeReady(n), Roles: nodeRoles(n),
			CPUCores: ncpu, MemBytes: nmem,
		}
		if u, ok := usageByNode[n.Name]; ok {
			st.HasMetrics = true
			st.CPUUsage, st.MemUsage = u.cpu, uint64(u.mem)
			usedCPU += u.cpu
			usedMem += u.mem
		}
		out.NodeList = append(out.NodeList, st)
	}
	out.CPUCores, out.MemBytes = cpuCap, uint64(memCap)

	if metricsOK && cpuCap > 0 && memCap > 0 {
		out.MetricsAvailable = true
		out.CPUUsage = percent(usedCPU / cpuCap)
		out.MemUsage = percent(usedMem / memCap)
	}
	return out, nil
}

// nodeReady 读 NodeReady condition。
//
// 取不到该 condition 时返回 false 而不是 true：状态不明的节点标成"就绪"
// 会让界面上少一个该被看见的问题。
func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type != corev1.NodeReady {
			continue
		}
		// condition 的 status 是三值字符串（True/False/Unknown），只有 True 才算就绪。
		return c.Status == corev1.ConditionTrue
	}
	return false
}

// nodeRoles 从 `node-role.kubernetes.io/<role>` 标签推导角色。
//
// 推导不出就返回空串 —— 普通工作节点本来就没有这类标签，
// 编一个 "worker" 出来会让人以为它是集群里登记过的角色。
func nodeRoles(n corev1.Node) string {
	const prefix = "node-role.kubernetes.io/"
	roles := make([]string, 0, 2)
	for k := range n.Labels {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if r := strings.TrimPrefix(k, prefix); r != "" {
			roles = append(roles, r)
		}
	}
	if len(roles) == 0 {
		return ""
	}
	sort.Strings(roles)
	return strings.Join(roles, ",")
}

// Ping 连通性测试：真拨一次集群 API。
//
// 读 /version —— 它是最轻的已认证接口，凭据错了会立刻 401，
// 不必等到列节点才失败。平台说"连通"没有意义，结论必须由集群回答。
func (k *K8sCluster) Ping(ctx context.Context, ref ClusterRef) error {
	cs, _, err := k.clientset(ctx, ref)
	if err != nil {
		return err
	}
	if _, err := cs.Discovery().ServerVersion(); err != nil {
		return errors.New(classifyK8sErrText(err))
	}
	return nil
}

// --------------------------------------------------------------------------- 写
//
// ApplyImage / Scale / Restart 三个方法不在本文件：它们要经过 AppRelease 变更通道，
// 实现与约束都集中在 integration_k8s_writes.go 与 integration_apprelease.go 两个文件里。
// 放在一起是因为"往哪写、为什么往那写"比"怎么写"更容易被改错。

// --------------------------------------------------------------------------- 内部工具

// validate 校验定位参数。
//
// 空 namespace 会命中 client-go 的默认值（当前 context 的 namespace），
// 那可能悄悄读到一个完全不同的命名空间——必须显式拒绝。
func (ref WorkloadRef) validate() error {
	if ref.Namespace == "" {
		return errors.New("命名空间为空：环境未配置 namespace，无法定位工作负载")
	}
	if ref.Workload == "" {
		return errors.New("工作负载名为空：服务实例缺少 workload_name")
	}
	return nil
}

// deploymentState 把 Deployment 转成平台口径的状态。
func deploymentState(d *appsv1.Deployment) *WorkloadState {
	image := ""
	res := corev1.ResourceRequirements{}
	if n := len(d.Spec.Template.Spec.Containers); n > 0 {
		c := d.Spec.Template.Spec.Containers[0]
		image = c.Image
		res = c.Resources
	}
	desired := 0
	if d.Spec.Replicas != nil {
		desired = int(*d.Spec.Replicas)
	}
	st := &WorkloadState{
		Exists:        true,
		Image:         image,
		Tag:           imageTag(image),
		Replicas:      desired,
		ReadyReplicas: int(d.Status.ReadyReplicas),
		// 实际存在的 Pod 数（含正在崩溃、还没来得及被回收的那些）。
		// 与 desired 分开是有意的：两者不等时，"有几个实例、坏了几"才是能排查的信息。
		ActualReplicas: int(d.Status.Replicas),
		Revision:       revisionOf(d),
		CPURequest:     quantityText(res.Requests, corev1.ResourceCPU),
		CPULimit:       quantityText(res.Limits, corev1.ResourceCPU),
		MemRequest:     quantityText(res.Requests, corev1.ResourceMemory),
		MemLimit:       quantityText(res.Limits, corev1.ResourceMemory),
	}
	st.Status, st.Message = deploymentStatus(d, desired)
	return st
}

// quantityText 取容器声明的某个资源规格。没声明时返回空串——
// **不许用 0 或默认值顶替**：空串在界面上显示 "—"，含义是"容器没写这个规格"，
// 与"写了个 0"是两回事（后者会让调度器把它当成真的限额）。
func quantityText(list corev1.ResourceList, name corev1.ResourceName) string {
	q, ok := list[name]
	if !ok {
		return ""
	}
	return q.String()
}

// deploymentStatus 判定状态词与补充说明。
//
// 判据取自 status.conditions 而不是"副本数是否相等"：副本数相等只说明 Pod 数量对，
// 说明不了新版本是否已经替换旧版本（滚动中原旧副本并存时两者都相等）。
func deploymentStatus(d *appsv1.Deployment, desired int) (status, message string) {
	if desired == 0 {
		return "stopped", "副本数为 0（服务已下线或暂停）"
	}
	available := false
	progressing := false
	failedMsg := ""
	for _, c := range d.Status.Conditions {
		switch c.Type {
		case appsv1.DeploymentAvailable:
			available = c.Status == corev1.ConditionTrue
			if c.Status != corev1.ConditionTrue && c.Message != "" {
				message = c.Message
			}
		case appsv1.DeploymentProgressing:
			progressing = c.Status == corev1.ConditionTrue
			if c.Reason == "ProgressDeadlineExceeded" {
				failedMsg = stringOrText(c.Message, "滚动更新超时（ProgressDeadlineExceeded）")
			}
		}
	}
	if failedMsg != "" {
		return "failed", failedMsg
	}
	updated := int(d.Status.UpdatedReplicas)
	ready := int(d.Status.ReadyReplicas)
	if available && updated == desired && ready == desired {
		return "running", ""
	}
	if !progressing && !available {
		return "failed", stringOrText(message, "工作负载不可用")
	}
	return "progressing", fmt.Sprintf("滚动中：已更新 %d/%d，就绪 %d/%d", updated, desired, ready, desired)
}

func stringOrText(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// revisionOf 取 Deployment 的 rollout 版本号。
func revisionOf(d *appsv1.Deployment) int {
	raw := d.Annotations["deployment.kubernetes.io/revision"]
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}

// imageTag 从镜像地址里取 tag。
//
// 要处理两个坑：① 端口形式的仓库地址（host:5000/repo:tag）里冒号不止一个；
// ② digest 引用（repo@sha256:...）没有 tag。
func imageTag(image string) string {
	if image == "" {
		return ""
	}
	if i := strings.Index(image, "@"); i >= 0 {
		return "" // digest 引用，没有可展示的 tag
	}
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon > slash {
		return image[colon+1:]
	}
	return ""
}

// podStatus 取 Pod 的展示状态。
//
// 优先用容器级的 waiting reason（CrashLoopBackOff / ImagePullBackOff）：
// 那时 Pod 的 phase 可能还是 Running，只显示 Running 会让用户以为一切正常。
func podStatus(p *corev1.Pod) string {
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason
		}
		if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" && cs.State.Terminated.ExitCode != 0 {
			return cs.State.Terminated.Reason
		}
	}
	return string(p.Status.Phase)
}

func podRestarts(p *corev1.Pod) int {
	n := 0
	for _, cs := range p.Status.ContainerStatuses {
		n += int(cs.RestartCount)
	}
	return n
}

// humanAge 把时长渲染成 kubectl 风格的短文本。
//
// 只在"确实有位可进"时才带第二段单位：`128d0h` 这种把 0 也写出来的结果
// 既啰嗦、又会让人以为是格式算错了（Pod 列表上真出现过，一屏之内的
// `128d0h` 和 `26m` 摆在一起，很像前者的精度出了问题）。
func humanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		if m := int(d.Minutes()) % 60; m != 0 {
			return fmt.Sprintf("%dh%dm", int(d.Hours()), m)
		}
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		hours := int(d.Hours())
		if hours%24 != 0 {
			return fmt.Sprintf("%dd%dh", hours/24, hours%24)
		}
		return fmt.Sprintf("%dd", hours/24)
	}
}

// percent 把 0-1 的比例转成 0-100 的整数百分比。
func percent(ratio float64) uint8 {
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	return uint8(ratio*100 + 0.5)
}

// describeErr 把读工作负载时的错误说清楚。
func describeErr(err error, ref WorkloadRef) error {
	switch {
	case apierrors.IsForbidden(err):
		return fmt.Errorf("凭据无权读取 %s/%s：请给该账号最小读取权限", ref.Namespace, ref.Workload)
	case apierrors.IsUnauthorized(err):
		return fmt.Errorf("凭据被集群拒绝（401），请重新导入 kubeconfig")
	default:
		return fmt.Errorf("读取 %s/%s 失败：%w", ref.Namespace, ref.Workload, err)
	}
}

// classifyK8sErrText 复用 kubeconfig.go 的错误分类，返回一句可直接展示的文本。
func classifyK8sErrText(err error) string {
	_, msg := classifyK8sError(err)
	return msg
}

// --------------------------------------------------------------------------- metrics

// usagePair 一份用量（CPU 核 / 内存字节）。
type usagePair struct{ cpu, mem float64 }

// podUsageEntry 一个 Pod 的实时用量（**数值**，未格式化）。
type podUsageEntry struct{ cpuMilli, memBytes int64 }

// podUsageRaw 读命名空间下所有 Pod 的实时用量。
//
// ⚠️ Pod 级与节点级的 JSON 形状**不同**：节点级的用量在顶层 `usage`，
// Pod 级在 `containers[].usage` 里（实测：v1.22 集群返回 `{"containers":[{"name":"master","usage":{...}}]}`）。
// 照抄节点级的字段路径会永远解析出空值，而空值在界面上表现为"用量列一片空白"，
// 很容易被误判成"这个集群没装 metrics-server"——其实装了，是解析写错了。
//
// metrics.k8s.io 是 aggregated API，走 discovery 的 RESTClient 直读，
// 这样不必为它再构造一个 clientset。
//
// 第二个返回值是**这次查询本身成不成功**，与"有没有 Pod"无关：
// 装了 metrics-server 而命名空间下没有 Pod → `(空 map, true)`；
// 没装 / 凭据无权读 → `(空 map, false)`。上层必须靠它区分
// "用量是 0" 与 "读不到用量"——这两个结论对用户正好相反。
func (k *K8sCluster) podUsageRaw(ctx context.Context, cs kubernetes.Interface, namespace string) (map[string]podUsageEntry, bool) {
	out := map[string]podUsageEntry{}
	raw, err := cs.Discovery().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces/" + namespace + "/pods").
		DoRaw(ctx)
	if err != nil {
		return out, false
	}
	var parsed struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Containers []struct {
				Usage map[string]string `json:"usage"`
			} `json:"containers"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return out, false
	}
	for _, it := range parsed.Items {
		if it.Metadata.Name == "" {
			continue
		}
		// 一个 Pod 可能有多个容器（业务容器 + sidecar），用量要**累加**：
		// 只取第一个会系统性少算，用户按这个数判断要不要扩容就会判断错。
		var cpuMilli, memBytes int64
		for _, c := range it.Containers {
			if q, err := resource.ParseQuantity(c.Usage["cpu"]); err == nil {
				cpuMilli += q.MilliValue()
			}
			if q, err := resource.ParseQuantity(c.Usage["memory"]); err == nil {
				memBytes += q.Value()
			}
		}
		out[it.Metadata.Name] = podUsageEntry{cpuMilli: cpuMilli, memBytes: memBytes}
	}
	return out, true
}

// podUsage 读命名空间下所有 Pod 的实时用量（**显示形式**，供 Pod 明细逐条查）。
//
// 整体失败时返回空 map：没装 metrics-server 是很常见的情况，
// 不能让"看不了用量"升级成"看不了 Pod 列表"。需要区分
// "读不到"与"没有 Pod"的地方请直接用 podUsageRaw 的第二个返回值。
func (k *K8sCluster) podUsage(ctx context.Context, cs kubernetes.Interface, namespace string) map[string]struct{ cpu, mem string } {
	raw, _ := k.podUsageRaw(ctx, cs, namespace)
	out := make(map[string]struct{ cpu, mem string }, len(raw))
	for name, e := range raw {
		out[name] = struct{ cpu, mem string }{
			cpu: formatCPUMilli(e.cpuMilli),
			mem: formatMemBytes(e.memBytes),
		}
	}
	return out
}

// WorkloadUsages 取**点名的**那几个工作负载的实时用量（一次覆盖整个命名空间）。
//
// ⚠️ 这里对命名空间做了 List（Deployments / Pods），但**用法必须守住**：
// List 的结果**只用于查表**（按 workloads 取 selector、按 selector 把 Pod 归位），
// 绝不拿 List 到的名字去"发现服务"——环境 default 里躺着几十个与本平台无关的
// Deployment，一旦把它们当服务就是误报（见本文件开头的铁律 1）。
// 签名特意要 `workloads` 入参、返回值也**只含点名的那些**，就是为了让这条无法被误用。
//
// 为什么必须是批量：环境详情页一次要列 6~20 个服务，逐个查就是 N 倍请求；
// 而这里要的三份数据（Deployment 的 selector、Pod 的标签、Pod 的用量）
// **整个命名空间各只需一次**。
//
// 归位靠 selector 匹配 Pod 标签，**不靠 Pod 名字猜**：`<deploy>-<rs>-<hash>` 这种拼法
// 看着能猜，但 StatefulSet、自定义控制器、手工建的 Pod 都不遵守 ——
// 猜错的结果是把别人的用量算到你头上，而且不会报任何错。
//
// 读不到用量时每项 `Available=false`（不是 error）：没装 metrics-server 是常见情况，
// 不该让"看不了用量"升级成"服务列表打不开"。
func (k *K8sCluster) WorkloadUsages(ctx context.Context, ref ClusterRef, namespace string, workloads []string) (map[string]WorkloadUsage, error) {
	out := map[string]WorkloadUsage{}
	if strings.TrimSpace(namespace) == "" {
		return out, nil
	}
	want := make(map[string]struct{}, len(workloads))
	for _, w := range workloads {
		if w = strings.TrimSpace(w); w != "" {
			want[w] = struct{}{}
		}
	}
	if len(want) == 0 {
		return out, nil
	}

	cs, _, err := k.clientset(ctx, ref)
	if err != nil {
		return nil, err
	}
	deps, err := cs.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("读取命名空间 %s 的工作负载失败：%w", namespace, err)
	}
	pods, err := cs.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("读取命名空间 %s 的 Pod 失败：%w", namespace, err)
	}
	usage, metricsOK := k.podUsageRaw(ctx, cs, namespace)

	return aggregateUsage(deps.Items, pods.Items, usage, want, metricsOK), nil
}

// aggregateUsage 把「命名空间的工作负载列表 / Pod 列表 + 用量表」归并成
// 每个工作负载的合计用量。
//
// 抽成纯函数是为了**能测**：这段逻辑的正确性全在「归属」上，
// 而归属错了不会报任何错 —— 只会让两个数字看起来都合理
// （A 的用量记到 B 头上，两边都不像有问题）。
func aggregateUsage(
	deps []appsv1.Deployment,
	pods []corev1.Pod,
	usage map[string]podUsageEntry,
	want map[string]struct{},
	metricsOK bool,
) map[string]WorkloadUsage {
	out := make(map[string]WorkloadUsage, len(want))
	for i := range deps {
		d := &deps[i]
		// 白名单：只处理点名的工作负载。这是"List 只用于查表、不用于发现服务"
		// 那一条在代码里的落点 —— 命名空间里几十个与本平台无关的对象全部落在这。
		if _, ok := want[d.Name]; !ok {
			continue
		}
		u := WorkloadUsage{Available: metricsOK}
		// selector 为空的畸形对象：Pods 保持 0，界面显示"无 Pod"。
		// **不退回"按名字前缀匹配"去凑一个数** —— 那正是上面说的"看不出错的错"。
		if d.Spec.Selector != nil {
			if sel, err := metav1.LabelSelectorAsSelector(d.Spec.Selector); err == nil {
				for j := range pods {
					p := &pods[j]
					if !sel.Matches(labels.Set(p.Labels)) {
						continue
					}
					u.Pods++
					// 重启次数与 Pod 年龄都不来自 metrics，所以放在
					// `usage` 命中判断**之外** —— 没装 metrics-server 时
					// 这两个数字仍然应该看得见。
					for _, cs := range p.Status.ContainerStatuses {
						u.Restarts += int(cs.RestartCount)
					}
					if p.CreationTimestamp.Time.After(u.NewestPodAt) {
						u.NewestPodAt = p.CreationTimestamp.Time
					}
					if e, ok := usage[p.Name]; ok {
						u.UsagePods++
						u.CPUMilli += e.cpuMilli
						u.MemBytes += e.memBytes
					}
				}
			}
		}
		u.CPU = formatCPUMilli(u.CPUMilli)
		u.Mem = formatMemBytes(u.MemBytes)
		out[d.Name] = u
	}
	return out
}

// nodeUsageByNode 读各节点的实时用量，**按节点名索引**。
//
// 两个"没有数据"要分开：
//   - 第二个返回值为 false = 整体没读到（没装 metrics-server / 无权读该接口）；
//   - map 里缺某个节点 = 那一台没报数（刚加入、抓取持续失败）。
//
// 混为一谈的后果是具体的：拿"整体读到了"覆盖到每台，缺数据的那台会显示成 0% ——
// 那是"没读到"，不是"很闲"。
func (k *K8sCluster) nodeUsageByNode(ctx context.Context, cs kubernetes.Interface) (map[string]usagePair, bool) {
	raw, err := cs.Discovery().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/nodes").
		DoRaw(ctx)
	if err != nil {
		return nil, false
	}
	var parsed struct {
		Items []struct {
			// metadata.name 是"这份用量属于哪台节点"的唯一依据。
			// metrics 的 items 顺序不保证与节点列表一致，靠下标对齐一定会错位。
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Usage map[string]string `json:"usage"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Items) == 0 {
		return nil, false
	}
	out := make(map[string]usagePair, len(parsed.Items))
	for _, it := range parsed.Items {
		if strings.TrimSpace(it.Metadata.Name) == "" {
			continue
		}
		var u usagePair
		if q, err := resource.ParseQuantity(it.Usage["cpu"]); err == nil {
			u.cpu = q.AsApproximateFloat64()
		}
		if q, err := resource.ParseQuantity(it.Usage["memory"]); err == nil {
			u.mem = float64(q.Value())
		}
		out[it.Metadata.Name] = u
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// formatCPUMilli 把毫核渲染成展示文本（1000m → "1"，250m → "250m"）。
func formatCPUMilli(milli int64) string {
	if milli%1000 == 0 {
		return strconv.FormatInt(milli/1000, 10)
	}
	return strconv.FormatInt(milli, 10) + "m"
}

// formatMemBytes 把字节数渲染成 Mi/Gi。
//
// 与 kubectl 的显示口径对齐：kubectl 显示 1.2Gi 而平台显示 1228Mi，
// 会让用户以为是两个不同的值。
func formatMemBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMi", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fKi", float64(b)/(1<<10))
	default:
		return strconv.FormatInt(b, 10)
	}
}

// formatMem 把 K8s 数量文本渲染成 Mi/Gi（节点级用量用的是字符串入参）。
func formatMem(s string) string {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return ""
	}
	return formatMemBytes(q.Value())
}
