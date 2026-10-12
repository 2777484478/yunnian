package integration

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// 本文件是「集群接入」的真实底座：把一份 kubeconfig 变成平台能长期持有、能真拨通的凭据。
//
// 三条设计取舍，都写在这里，因为后来人最容易想"顺手改一下"：
//
//  1. **只存选定 context 的最小化 kubeconfig**，不存用户粘贴的原文件。
//     一份 kubeconfig 常常同时含 dev / staging / prod 三个 context。若把整份原样落库，
//     意味着一份"只该访问测试集群"的凭据同时握着生产集群的 client-key——
//     平台里的每一条记录都成了生产后门。最小化之后，每个集群行只带自己能用的那一份。
//
//  2. **接入成功的判据是真的拨通了一次集群 API**（读 /version，再列节点与命名空间）。
//     这不能由平台自己判断——平台说"连通"没有任何意义，结论必须由集群回答。
//     原先 LocalCluster 的 Ping 恒返回 nil，于是「测试连接」永远全绿，
//     这就是本次要根治的那个缺陷。
//
//  3. **读取失败不许用 0 兜底**。节点数读不到时返回 null 而不是 0：
//     "无权列节点"与"集群里一个节点都没有"是完全不同的两件事，
//     用 0 表示前者，页面就会显示"0 个节点"——那是编的（见 model.K8sCluster.Nodes 注释）。

// --------------------------------------------------------------------------- 解析

// KubeContext kubeconfig 里的一个 context。
type KubeContext struct {
	Context string `json:"context"`
	// Cluster kubeconfig 内部引用的集群名，与 Context 未必同名。
	Cluster   string `json:"cluster"`
	APIServer string `json:"apiServer"`
	Namespace string `json:"namespace"`
	User      string `json:"user"`
	// CredentialKind 凭据形态：token / token-file / client-cert / basic / exec / auth-provider / none。
	// 只作展示与排障用，平台不据此分支。
	CredentialKind string `json:"credentialKind"`
	// Problem 非空表示这个 context **不能**用来接入，值是具体原因。
	// 例如"client-key 缺失，只有证书没有私钥"。前端把它如实显示，而不是让用户
	// 选完之后在探测阶段收到一个看不懂的 TLS 错误。
	Problem string `json:"problem"`
}

// KubeconfigParseResult 解析结果。
//
// 解析**不落库**，纯粹把文件摊开给用户挑。一份文件里某个 context 不可用
// （user 不存在、私钥缺失）不影响其他 context，所以这里是"列表 + 逐项问题"，
// 而不是一个整体的是/否。
type KubeconfigParseResult struct {
	// CurrentContext kubeconfig 声明的 current-context，可能不在 Contexts 里。
	CurrentContext string        `json:"currentContext"`
	Contexts       []KubeContext `json:"contexts"`
}

// ParseKubeconfig 解析 kubeconfig 文本。
func ParseKubeconfig(raw string) (*KubeconfigParseResult, error) {
	cfg, err := loadKubeconfig(raw)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(cfg.Contexts))
	for n := range cfg.Contexts {
		names = append(names, n)
	}
	// map 遍历顺序随机，排一下序让同一份文件的解析结果稳定可复现
	// （前端勾选顺序、审计日志内容都跟着稳定）。
	sort.Strings(names)

	out := &KubeconfigParseResult{
		CurrentContext: cfg.CurrentContext,
		Contexts:       make([]KubeContext, 0, len(names)),
	}
	for _, n := range names {
		ctx := cfg.Contexts[n]
		item := KubeContext{
			Context:   n,
			Cluster:   ctx.Cluster,
			Namespace: ctx.Namespace,
			User:      ctx.AuthInfo,
		}
		// 先判集群引用，再判凭据，最后**合并**两处问题。
		//
		// 这里曾经写反过（直接 `item.CredentialKind, item.Problem = authKindOf(...)`），
		// 于是"context 引用的集群根本不存在"这个更致命的问题会被凭据判定返回的空串覆盖掉，
		// 页面上表现为一个看不出毛病的 context —— 用户选它、点接入，
		// 才在探测阶段收到一句莫名其妙的报错。两处都要说，缺一不可。
		if c, ok := cfg.Clusters[ctx.Cluster]; ok && c != nil {
			item.APIServer = c.Server
			if c.Server == "" {
				item.Problem = "该 context 指向的集群没有 server 地址"
			}
		} else {
			item.Problem = fmt.Sprintf("该 context 引用的集群 %q 在 kubeconfig 里不存在", ctx.Cluster)
		}
		kind, authProblem := authKindOf(cfg, ctx.AuthInfo)
		item.CredentialKind = kind
		switch {
		case item.Problem != "" && authProblem != "":
			item.Problem = item.Problem + "；" + authProblem
		case authProblem != "":
			item.Problem = authProblem
		}
		out.Contexts = append(out.Contexts, item)
	}
	return out, nil
}

// authKindOf 判断该 context 的鉴权信息是否完整，返回凭据形态与（可能的）缺失原因。
//
// 做成普通函数而不是 KubeContext 的方法：它需要整个 kubeconfig 才能顺着
// user 名查到 AuthInfo，挂在 context 项上会变成一个看着像"自己的属性"、
// 实际依赖外部参数的误导性方法。
func authKindOf(cfg *clientcmdapi.Config, authName string) (kind, problem string) {
	if authName == "" {
		return "none", "该 context 没有指定 user，无法鉴权"
	}
	u, ok := cfg.AuthInfos[authName]
	if !ok || u == nil {
		return "none", fmt.Sprintf("该 context 引用的 user %q 在 kubeconfig 里不存在", authName)
	}
	hasCert := u.ClientCertificate != "" || len(u.ClientCertificateData) > 0
	hasKey := u.ClientKey != "" || len(u.ClientKeyData) > 0
	switch {
	case u.Token != "":
		return "token", ""
	case u.TokenFile != "":
		return "token-file", ""
	case hasCert && hasKey:
		return "client-cert", ""
	case hasCert && !hasKey:
		return "client-cert", "只有客户端证书、缺少 client-key（私钥），无法完成双向认证"
	case !hasCert && hasKey:
		return "client-cert", "只有 client-key、缺少客户端证书"
	case u.Username != "" || u.Password != "":
		return "basic", ""
	case u.Exec != nil:
		return "exec", ""
	case u.AuthProvider != nil:
		return "auth-provider", ""
	default:
		return "none", "该 user 没有任何可用的凭据字段（token / 证书 / 用户名密码）"
	}
}

// loadKubeconfig 解析 YAML。错误信息统一加前缀，免得用户拿到一句裸的 yaml 报错。
func loadKubeconfig(raw string) (*clientcmdapi.Config, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("kubeconfig 内容为空")
	}
	cfg, err := clientcmd.Load([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("kubeconfig 解析失败：%w", err)
	}
	if cfg == nil || len(cfg.Contexts) == 0 {
		return nil, fmt.Errorf("kubeconfig 里没有任何 context，无法接入")
	}
	return cfg, nil
}

// ContextInfoOf 取指定 context 对应的 API Server 与 kubeconfig 内部集群名。
//
// 后者单独返回是因为它**与 context 名经常不同**：`kubectl config get-contexts`
// 的第一列（context）与第三列（cluster）在真实文件里大量不一致，
// 排障时"平台按哪个 context 连的、那个 context 又指向哪一组配置"必须都能看到。
// 解析失败或找不到时返回空串——留空比编一个名字诚实。
func ContextInfoOf(raw, contextName string) (apiServer, clusterName string) {
	cfg, err := clientcmd.Load([]byte(raw))
	if err != nil || cfg == nil {
		return "", ""
	}
	if contextName == "" {
		contextName = cfg.CurrentContext
	}
	ctx, ok := cfg.Contexts[contextName]
	if !ok || ctx == nil {
		return "", ""
	}
	clusterName = ctx.Cluster
	if c, ok := cfg.Clusters[ctx.Cluster]; ok && c != nil {
		apiServer = c.Server
	}
	return apiServer, clusterName
}

// MinimalKubeconfig 为指定 context 生成一份**只含它自己**的 kubeconfig。
//
// 这是真正落库保存的内容（加密后存 secret_credential）。
// 之所以同时保留 cluster / user / context 三段：clientcmd 的写盘与后续加载
// 都要求三段自洽，只留 context 会得到一个加载不了的半成品。
func MinimalKubeconfig(raw, contextName string) (string, error) {
	cfg, err := loadKubeconfig(raw)
	if err != nil {
		return "", err
	}
	if contextName == "" {
		contextName = cfg.CurrentContext
	}
	ctx, ok := cfg.Contexts[contextName]
	if !ok || ctx == nil {
		return "", fmt.Errorf("kubeconfig 里没有名为 %q 的 context", contextName)
	}
	cluster, ok := cfg.Clusters[ctx.Cluster]
	if !ok || cluster == nil {
		return "", fmt.Errorf("context %q 引用的集群 %q 在 kubeconfig 里不存在", contextName, ctx.Cluster)
	}

	out := clientcmdapi.NewConfig()
	out.Clusters[ctx.Cluster] = cluster
	if u, ok := cfg.AuthInfos[ctx.AuthInfo]; ok && u != nil {
		out.AuthInfos[ctx.AuthInfo] = u
	}
	out.Contexts[contextName] = ctx
	out.CurrentContext = contextName

	data, err := clientcmd.Write(*out)
	if err != nil {
		return "", fmt.Errorf("生成最小化 kubeconfig 失败：%w", err)
	}
	return string(data), nil
}

// --------------------------------------------------------------------------- 真实客户端

// NewRestConfig 由 kubeconfig 字节构造 rest.Config。
func NewRestConfig(kubeconfigData []byte, timeout time.Duration) (*rest.Config, error) {
	rc, err := clientcmd.RESTConfigFromKubeConfig(kubeconfigData)
	if err != nil {
		return nil, fmt.Errorf("按 kubeconfig 构造客户端配置失败：%w", err)
	}
	if timeout > 0 {
		rc.Timeout = timeout
	}
	return rc, nil
}

// Clientset 由 kubeconfig 字节构造客户端。
func Clientset(kubeconfigData []byte, timeout time.Duration) (kubernetes.Interface, error) {
	rc, err := NewRestConfig(kubeconfigData, timeout)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("创建 K8s 客户端失败：%w", err)
	}
	return cs, nil
}

// --------------------------------------------------------------------------- 探测

// ClusterProbe 一次真实探测的结论。
//
// 它**不是 error 的包装**：探测失败是常态（网络不通、凭据过期），
// 调用方要的是"失败原因 + 已经读到的部分"，而不是一个需要 errors.Is 的错误。
// 因此这里返回结构体本身，OK 字段承载成败——与 GitLab / Jenkins 的
// 测试连接接口同一约定：`ok=false` 也是 HTTP 200。
type ClusterProbe struct {
	OK        bool   `json:"ok"`
	Status    string `json:"status"`
	Message   string `json:"message"`
	LatencyMS int    `json:"latencyMs"`

	K8sVersion     string `json:"k8sVersion"`
	Nodes          *int   `json:"nodes"`
	NodesReady     *int   `json:"nodesReady"`
	NamespaceCount *int   `json:"namespaceCount"`

	// Warnings 「能连上但迟早出事」类提醒，与 OK 无关，必须原样展示。
	// 例如：凭据有效但无权列节点 —— 连通性没问题，可节点数就是读不到。
	Warnings []string `json:"warnings"`
}

// Probe 真拨一次集群：读 /version，再列节点与命名空间。
//
// 顺序是刻意的：先 /version。它是所有已认证接口里最轻的一个，
// 凭据错了会立刻在它上面 401，不必等到列节点才失败；
// 而且能连上时它给出 gitVersion，是"这是个什么集群"的第一个事实。
func Probe(ctx context.Context, kubeconfigData []byte, timeout time.Duration) *ClusterProbe {
	start := time.Now()
	p := &ClusterProbe{Status: "unreachable"}

	cs, err := Clientset(kubeconfigData, timeout)
	if err != nil {
		// 走到这里说明 kubeconfig 本身有问题（缺 server、证书字段坏了），
		// 不是网络问题。如实说明，别让用户去查网络。
		p.Message = err.Error()
		p.LatencyMS = elapsedMS(start)
		return p
	}

	ver, err := cs.Discovery().ServerVersion()
	if err != nil {
		p.Status, p.Message = classifyK8sError(err)
		p.LatencyMS = elapsedMS(start)
		return p
	}
	p.K8sVersion = strings.TrimSpace(ver.GitVersion)

	// 节点与命名空间分别读，各自失败互不影响：有权限读命名空间却没权限读节点
	// 是很常见的授权粒度，不能因为一个读不到就整条判定失败。
	if nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err != nil {
		p.Warnings = append(p.Warnings, nodeListWarning(err))
	} else {
		total := len(nodes.Items)
		ready := 0
		for i := range nodes.Items {
			if nodeIsReady(&nodes.Items[i]) {
				ready++
			}
		}
		p.Nodes, p.NodesReady = &total, &ready
	}

	if nss, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); err != nil {
		p.Warnings = append(p.Warnings, nsListWarning(err))
	} else {
		n := len(nss.Items)
		p.NamespaceCount = &n
	}

	p.OK = true
	p.Status = "connected"
	p.Message = "集群连通"
	p.LatencyMS = elapsedMS(start)
	return p
}

// nodeIsReady 判断节点是否 Ready。
//
// 判据是 NodeReady 这个 condition 的 status == True，**不看 condition 的 Reason**：
// Reason 在不同版本/不同发行版里会出现 KubeletReady、NodeReady、Ready 等多种写法，
// 拿它判断会在某些集群上恒为 false，表现为"所有节点都不就绪"。
func nodeIsReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if string(c.Type) == "Ready" {
			return string(c.Status) == "True"
		}
	}
	return false
}

func nodeListWarning(err error) string {
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return "当前凭据无权列出节点，节点数读不到（连通性不受影响）"
	}
	return "读取节点列表失败：" + shortErr(err)
}

func nsListWarning(err error) string {
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return "当前凭据无权列出命名空间，命名空间数读不到"
	}
	return "读取命名空间列表失败：" + shortErr(err)
}

// ListNamespaces 列命名空间名（供新建环境时选）。
func ListNamespaces(ctx context.Context, kubeconfigData []byte, timeout time.Duration) ([]string, error) {
	cs, err := Clientset(kubeconfigData, timeout)
	if err != nil {
		return nil, err
	}
	nss, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("读取命名空间列表失败：%w", err)
	}
	out := make([]string, 0, len(nss.Items))
	for i := range nss.Items {
		out = append(out, nss.Items[i].Name)
	}
	sort.Strings(out)
	return out, nil
}

// --------------------------------------------------------------------------- 错误分类

// classifyK8sError 把 K8s 客户端错误映射为前端认识的两种状态。
//
// 区分 unauthorized 与 unreachable 的价值在于排查方向完全不同：
// 前者要用户去换凭据，后者要去查网络/防火墙/网关。混成一句"连接失败"，
// 用户会把时间花在错误的地方。
func classifyK8sError(err error) (status, message string) {
	switch {
	case apierrors.IsUnauthorized(err):
		return "unauthorized", "凭据被集群拒绝（401）：token 过期、证书失效或被吊销，请重新导入 kubeconfig"
	case apierrors.IsForbidden(err):
		return "unauthorized", "凭据有效但权限不足（403）：请联系集群管理员给该账号最小读取权限"
	case apierrors.IsTimeout(err) || isTimeoutText(err):
		return "unreachable", "连接集群超时：" + shortErr(err)
	default:
		return "unreachable", "无法访问集群：" + shortErr(err)
	}
}

func isTimeoutText(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "timeout") || strings.Contains(s, "deadline exceeded") ||
		strings.Contains(s, "no route to host")
}

// shortErr 截断错误文本。
// 底层错误可能带一长串 TLS/证书链信息，而它要落到 last_probe_message（512 字节列），
// 不截断会被数据库截断在奇怪的位置。
func shortErr(err error) string {
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(err.Error())
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

func elapsedMS(start time.Time) int {
	ms := int(time.Since(start).Milliseconds())
	if ms <= 0 {
		ms = 1 // 本机/内网直连常在 1ms 内完成，显示 0ms 会被当成"没探测"
	}
	return ms
}
