// Package integration 封装平台与外部系统的交互边界。
//
// 为什么全部走接口：平台要接的下游（Harbor / GitLab / K8s / 构建执行）在不同环境里
// 可用性差别很大——本地开发没有这些服务，测试环境可能只有部分，生产环境才是全的。
// 把「有没有接上」收敛成配置开关，业务代码只依赖接口，带来三个好处：
//   - 没有下游时平台仍能完整跑通编排闭环（走 local 实现，返回结构化假数据）
//   - 接入真实系统只是换实现，service / handler 一行不改
//   - 单元测试可以直接注入桩实现，不需要起真实环境
package integration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yunnian/yunnian/internal/config"
)

// --------------------------------------------------------------------------- 镜像仓库

// TagInfo 仓库里的一个版本。
//
// 字段是"读到多少写多少"：读不到的留零值，不要用别处的推测填进来。
type TagInfo struct {
	Repo      string `json:"repo"`
	Tag       string `json:"tag"`
	Digest    string `json:"digest"`
	SizeBytes uint64 `json:"size"`
	// Arch 形如 linux/amd64；多架构镜像是逗号分隔的平台列表。
	Arch string `json:"arch"`
	// PushedAt 推送时间。只有 Harbor Core 的 artifacts 接口给得出，
	// registry v2 拿不到，所以走 registry 时它恒为零值 —— 别拿它当事实。
	PushedAt time.Time `json:"pushedAt"`
}

// Registry 镜像仓库能力（Harbor）。
type Registry interface {
	// ListTags 列出仓库里的 tag 名（不含 digest / 大小，明细请用 Resolve）。
	// **没有排序语义**：不要把第一项当"最新"。
	ListTags(ctx context.Context, repo string, limit int) ([]TagInfo, error)
	// Resolve 取某个 tag 的确定信息：digest、大小、架构。
	// 返回 (nil, nil) 表示这次不适用（未配置仓库 / repo 或 tag 为空），调用方应跳过。
	Resolve(ctx context.Context, repo, tag string) (*TagInfo, error)
	// Accepts 判断一个仓库地址是不是本实现管的那一个（写法差异会被归一）。
	// 调用方在拿日志/库里的 registry 值去问之前**必须先过这道闸**，
	// 见 registry_v2.go 里 Accepts 的注释：问错仓库会静默挂上别人的 digest。
	Accepts(registry string) bool
	// Ping 连通性测试。
	Ping(ctx context.Context) error
	// Name 实现名，用于在接口响应里标注数据来源。
	Name() string
}

// --------------------------------------------------------------------------- 代码托管

// Commit 一次提交。
type Commit struct {
	ID      string    `json:"id"`
	Message string    `json:"message"`
	Author  string    `json:"author"`
	At      time.Time `json:"at"`
}

// Repository 代码托管能力（GitLab）。
type Repository interface {
	// Branches 列出分支名。
	Branches(ctx context.Context, gitURL string) ([]string, error)
	// LatestCommit 取分支最新提交；分支不存在时返回错误。
	LatestCommit(ctx context.Context, gitURL, branch string) (*Commit, error)
	// Ping 连通性测试。
	Ping(ctx context.Context) error
	Name() string
}

// --------------------------------------------------------------------------- K8s 集群

// ClusterRef 定位一个集群，并说明**该用哪份凭据**。
//
// 凭据来源必须是显式入参，不能按集群名去反查平台集群表。原因：环境可以自带
// kubeconfig（environment.kubeconfig_credential_id），那时 Cluster 只是个展示名，
// 凭据并不来自平台集群表。若签名里只有集群名，自带凭据的环境会被拿去读**平台集群**——
// 拿到的是"真实的、但属于另一个集群"的数据。这比假数据更危险：假数据看得出是假的，
// 它看不出，而且一旦这个集群里恰好有同名工作负载，平台会理直气壮地报一份别人的状态。
type ClusterRef struct {
	// Cluster 平台「系统管理 › 集群接入」里的 cluster_key；环境自带凭据时仅作展示。
	Cluster string `json:"cluster"`
	// EnvID 非 nil 表示用该环境自带的 kubeconfig。传的是**环境行 id**而不是凭据 id：
	// 由来源方自己去环境上取 kubeconfig_credential_id 再解密，
	// 这样"随便塞一个凭据 id 就能读出来"这条越权路径不存在。
	EnvID *uint64 `json:"envId,omitempty"`
}

// WorkloadRef 定位「某个环境里的某个工作负载」。
type WorkloadRef struct {
	ClusterRef
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
}

// KubeconfigSource 按引用解析 kubeconfig 明文。
//
// 由装配层注入（读 secret_credential 并解密）。**实现不得缓存**：凭据轮换后仍用旧值
// 是这类系统最典型的故障，表现为"明明换了 kubeconfig 却一直连不上旧集群"。
type KubeconfigSource func(ctx context.Context, ref ClusterRef) ([]byte, error)

// WorkloadState 工作负载当前状态。
//
// 三种情形由**返回值组合**区分，缺一不可（把后两种混为一谈就会对用户撒谎）：
//   - `(state, nil)` 且 Exists=true —— 读到了真实状态；
//   - `(state, nil)` 且 Exists=false —— 确认集群里没有这个工作负载（404）；
//   - `(nil, err)` —— **没读到**（权限/网络/超时），必须把原因说出来，
//     不能显示成"不存在"。
type WorkloadState struct {
	// Exists 集群里是否真的有这个工作负载。false 时其余字段无意义。
	Exists bool `json:"exists"`
	// Image 完整镜像地址（含 tag），取自容器 spec。
	Image string `json:"image"`
	// Tag 从 Image 里解析出的 tag，便于直接展示。
	Tag string `json:"tag"`
	// Replicas 期望副本数（spec.replicas）。
	Replicas int `json:"replicas"`
	// ActualReplicas 集群里**实际存在**的副本数（status.replicas）。
	//
	// 必须与 Replicas 分开看，否则"发布失败"会被渲染成"一切正常"：
	// 滚动更新卡住时新 Pod 反复重启、旧 Pod 不被杀（1 副本时 maxUnavailable
	// 向下取整为 0），实际存在 2 个而期望只有 1 个 —— 实测 demobasic 就是
	// `spec=1 / status=2 / ready=1`。只按期望渲染就写成 `1/1`（还带个绿），
	// 把"有一个实例正在崩溃"整个抹掉；用户在自己的运维面板里看到的却是 `1/2`。
	ActualReplicas int `json:"actualReplicas"`
	// ReadyReplicas 就绪副本数（status.readyReplicas）。
	ReadyReplicas int `json:"readyReplicas"`
	// Status 平台口径的状态词：running / progressing / failed / stopped。
	Status string `json:"status"`
	// Revision 当前 Deployment 的 revision（来自 rollout 注解）。
	Revision int `json:"revision"`
	// CPURequest / CPULimit / MemRequest / MemLimit 是**容器声明的资源规格**，
	// 取自 Deployment 的容器 spec ——— 不是使用量（用量在 Pod 级 metrics 里）。
	// 模板没写就是空串，界面显示 "—"。
	CPURequest string `json:"cpuRequest"`
	CPULimit   string `json:"cpuLimit"`
	MemRequest string `json:"memRequest"`
	MemLimit   string `json:"memLimit"`
	// Message 补充说明：状态词之外的细节，或"不存在"的具体原因。
	Message string `json:"message"`
}

// ApplyRequest 一次镜像更新请求。
type ApplyRequest struct {
	WorkloadRef
	Image    string            `json:"image"`
	Replicas int               `json:"replicas"`
	Strategy string            `json:"strategy"`
	EnvVars  map[string]string `json:"envVars,omitempty"`
}

// ClusterState 集群容量概况。
type ClusterState struct {
	Nodes   int    `json:"nodes"`
	Version string `json:"version"`
	// MetricsAvailable 是否真的从 metrics.k8s.io 读到了用量。
	// false 时 CPUUsage / MemUsage 恒为 0 —— 那是"没读到"，不是"用量为 0"。
	// 集群没装 metrics-server、或凭据无权读它，都会落到这里。
	MetricsAvailable bool `json:"metricsAvailable"`
	// CPUCores / MemBytes 集群可分配总量（allocatable 之和），单位分别是核与字节。
	// 读不到时为零值，配合 MetricsAvailable 一起看。
	CPUCores float64 `json:"cpuCores"`
	MemBytes uint64  `json:"memBytes"`
	// CPUUsage / MemUsage 已用百分比（0-100）。MetricsAvailable=false 时无意义。
	CPUUsage uint8 `json:"cpuUsage"`
	MemUsage uint8 `json:"memUsage"`
}

// PodInfo 一个 Pod 的概要状态。
//
// 字段刻意做成"展示用字符串"（cpu/mem/age 是 string 而不是数值）：
// 平台不消费这些数字做计算，只把它们原样渲染到页面上。
// 用字符串可以避免"kubectl 里显示 1.2Gi、平台显示 1228Mi"这类换算不一致。
type PodInfo struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Node     string `json:"node"`
	Restarts int    `json:"restarts"`
	Age      string `json:"age"`
	IP       string `json:"ip"`
	CPU      string `json:"cpu"`
	Mem      string `json:"mem"`
}

// FailureEvidence 一次发布失败的**集群侧**证据。
//
// 为什么不复用 Pods：那个接口是给列表页用的，只有"有几个 Pod、重启几次"这类计数；
// 回答"应用为什么起不来"需要的是容器状态里的 waiting / terminated reason、退出码，
// 以及**崩溃前那一轮**的容器日志 —— 这三样在 Pods 里全都没有。
// 而平台自己的部署日志同样说不了：它只记录"下发成功 / 等待滚动 / 超时"这几个动作。
// 少了这份证据，任何"AI 诊断"都只能猜（实测：真因在 previous 日志里，平台日志什么都没写）。
//
// 采集全程**只读**，实现方不得因为取证而修改任何集群对象。
type FailureEvidence struct {
	// Pods 是**未能就绪**的 Pod；全部就绪时为空（那种情况下"失败"多半是平台侧超时
	// 而不是容器起不来，Notes 会写明）。
	Pods []PodEvidence `json:"pods"`
	// Events 是这些 Pod 上的最近事件（去重、按时间倒序）。
	// 它常常是"Pod 根本没被调度起来"这类失败的**唯一**线索 —— 那种情况下容器日志是空的。
	Events []string `json:"events"`
	// Notes 如实记录"没拿到哪一块、为什么"。证据不全时必须说得出是权限不足、
	// Pod 已被回收，还是工作负载根本不在了 —— 空证据和"查了没问题"是两回事。
	Notes []string `json:"notes,omitempty"`
}

// PodEvidence 单个未就绪 Pod 的证据。
type PodEvidence struct {
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Container string `json:"container"`
	Restarts  int    `json:"restarts"`
	// Reason 是容器 waiting / terminated 的 reason：
	// CrashLoopBackOff / OOMKilled / ImagePullBackOff / CreateContainerConfigError …
	// 它把处置方向直接分开了：补配置、加内存、修镜像凭据、补 ConfigMap —— 四件不同的事。
	Reason string `json:"reason"`
	// Message 是 kubelet 给的原文说明。
	Message string `json:"message"`
	// ExitCode 是上一次退出的退出码（137 = SIGKILL，常见于 OOM）。
	ExitCode int32 `json:"exitCode"`
	// Logs 是容器日志尾部。
	Logs string `json:"logs"`
	// LogsFrom 标明 Logs 取自哪一次实例：previous / current / ""（没取到）。
	// 必须区分：CrashLoopBackOff 时当前实例常常还没输出就退了，证据在 previous 里。
	LogsFrom string `json:"logsFrom"`
}

// ErrRolloutStuck 滚动更新被**集群自己**判定为卡死。
//
// 判据是 Deployment 的 `Progressing=False / reason=ProgressDeadlineExceeded`：
// Deployment 控制器在 `progressDeadlineSeconds`（未显式设置时 k8s 默认 600s）内
// 看不到任何进展时才会置上这个条件。它是集群的正式结论，不是"我们等烦了" ——
// 所以调用方可以把等待时间从自己的 deployTimeout 提前到这一刻，并且知道
// 这次失败**确定**不是"还在慢慢起"，而是"已经不可能自己好了"。
var ErrRolloutStuck = errors.New("滚动更新卡死：新 Pod 在进度截止时间内始终没有就绪")

// Cluster K8s 集群操作能力。
type Cluster interface {
	// Describe 查询工作负载当前状态。
	Describe(ctx context.Context, ref WorkloadRef) (*WorkloadState, error)
	// ApplyImage 更新镜像并等待滚动完成。
	// progress 回调用于把滚动进度实时推给 SSE —— 部署过程耗时最长，
	// 用户最需要看到的就是这一段。
	ApplyImage(ctx context.Context, req ApplyRequest, progress func(stage string, pct uint8) error) error
	// Scale 调整副本数。
	Scale(ctx context.Context, ref WorkloadRef, replicas int) error
	// Restart 滚动重启（不换镜像）。
	Restart(ctx context.Context, ref WorkloadRef) error
	// Pods 列出工作负载下的 Pod。
	Pods(ctx context.Context, ref WorkloadRef) ([]PodInfo, error)
	// PodLogs 读取某个 Pod 的日志（tail 最近 n 行），返回的流由调用方关闭。
	PodLogs(ctx context.Context, ref WorkloadRef, pod string, tailLines int) (io.ReadCloser, error)
	// DiagnoseFailure 采集"这个工作负载为什么没起来"的证据（只读）。
	//
	// 拿不到证据时必须返回 error，不能返回一个空结构体装作"没有异常证据"——
	// 下游对这两者的结论正好相反（"应用没报错" vs "我们没看到"）。
	DiagnoseFailure(ctx context.Context, ref WorkloadRef) (*FailureEvidence, error)
	// Nodes 查询集群节点与容量。
	Nodes(ctx context.Context, ref ClusterRef) (*ClusterState, error)
	// Ping 连通性测试。测的是**这个引用指向的集群**，所以需要 ref：
	// 无参版本在"环境自带凭据"的场景下无法表达"测哪一个"。
	Ping(ctx context.Context, ref ClusterRef) error
	Name() string
}

// --------------------------------------------------------------------------- 构建执行

// BuildRequest 一次构建请求。
type BuildRequest struct {
	BuildID uint64
	// BuildNo 本项目第几次构建。自定义脚本靠它做自增版本号（`v{buildNo}` 默认 tag 规则），
	// 所以必须传进来 —— 脚本没法自己去库里数。
	BuildNo    int64
	ProjectKey string
	GitURL     string
	Branch     string
	CommitID   string
	Method     string // dockerfile / jenkins / script
	Dockerfile string
	Context    string
	Image      string // 目标镜像完整地址 repo:tag
	Params     map[string]string
	Timeout    time.Duration

	// ---------------------------------------------------------------- 自建构建（dockerfile / script）

	// Platform 目标平台，如 linux/amd64。
	//
	// **必须显式传，不要靠默认值**：开发机是 arm64、目标服务跑在 x86_64 上。
	// 不指定平台会构建出 arm64 镜像，推到生产直接 `exec format error` ——
	// 而这个错要等到容器起不来才会被发现，那时已经推了一版坏镜像上去。
	Platform string

	// BuildScript 自定义脚本（Method=script）的正文。
	//
	// 它是**整个构建流程的定义**：拉代码、编译、打包、推送都写在里面，
	// 平台不解析、不干预，只把它交给构建机执行，并把输出收进构建日志。
	//
	// 平台注入的环境变量见 scriptEnv（YUNNIAN_REPO / YUNNIAN_IMAGE / …），
	// 默认脚本就是靠它们拿到"要构建哪个仓库、产出哪个镜像"的。
	BuildScript string

	// Registry / ImageName / Tag 供脚本注入 YUNNIAN_* 变量使用。
	// 分开传而不是让调用方自己拼 Image：脚本里要按需拼别的 tag（比如再推一个 latest），
	// 只给一个拼好的字符串，它就再也拆不回去了。
	Registry  string
	ImageName string
	Tag       string

	// GitUsername / GitPassword 检出代码用的凭据。
	//
	// 只在构建期存在，由调用方从加密凭据里解出后填入，**绝不能被日志记录**：
	// git 失败时会把完整 URL 打进 stderr，实现方必须把这两项登记进 Cmd.Secrets 做兜底替换。
	GitUsername string
	GitPassword string
}

// 构建结果的来源。如实标注，供前端与 AI 归因区分"真构建"与"本地模拟"。
const (
	// BuildSourceDocker 由构建机（或平台进程）**真的执行了** docker build / push。
	BuildSourceDocker = "docker"
	// BuildSourceLocal 本地模拟，**没有产出任何镜像**，因此不携带 digest。
	BuildSourceLocal = "local"
)

// BuildResult 构建产物信息。
//
// ⚠️ 每个字段都必须是"外部命令或仓库告诉我们的"，算不出来就留空。
// 曾经的实现拿 `sha256(镜像名 + 提交号)` 当 digest 写进镜像记录，那个值
// **看起来完全像真的**（长度、前缀都对），却永远对不上仓库里的任何一个镜像，
// 而且 `EnrichMissingImageMeta` 只补空值、不覆盖 —— 它会一直留在库里。
type BuildResult struct {
	Digest    string        `json:"digest"`
	SizeBytes uint64        `json:"size"`
	Duration  time.Duration `json:"duration"`
	// Source 见上面的常量。空值视为未知，调用方不得当成"真"。
	Source string `json:"source"`
	// Arch 产出的镜像架构（如 linux/amd64），**从刚推上去的那个本地镜像读出来**。
	//
	// 为什么不直接用请求里的 Platform：那是"我要什么"，不是"我得到了什么"。
	// 两者在正常情况下一致，而一旦不一致（比如服务端忽略了 --platform），
	// 写"我要的"就会把问题盖住 —— 而且盖得毫无痕迹。
	// 读不到就留空，由仓库回读补齐。
	Arch string `json:"arch"`
}

// Builder 构建执行能力。
// 生产环境由独立的 Runner Agent 实现（跑在构建机上，独占 docker daemon）；
// inline 模式下用本地实现，用于在没有构建机的环境里把编排链路跑通。
type Builder interface {
	Build(ctx context.Context, req BuildRequest, sink func(level, line string) error) (*BuildResult, error)
	Name() string
}

// --------------------------------------------------------------------------- 装配

// Set 聚合全部下游，由 service 层注入使用。
type Set struct {
	Registry   Registry
	Repository Repository
	Cluster    Cluster
	Builder    Builder
	// Jenkins 是可选的构建执行通道：未接入时它的 Client() 返回明确的错误，
	// 而不是一个能"跑通"的假实现——判断"有没有接 Jenkins"必须靠它，
	// 靠 Builder.Name() 是不行的（后者说的是本地/inline 执行）。
	Jenkins *JenkinsBridge
}

// NewSet 按配置装配下游实现。
//
// 未启用的下游一律走 local 实现——这不是"假数据糊弄"，而是刻意的降级设计：
// 缺少下游时平台仍然可用，只是产物来自本地模拟，接口响应里会带 source 字段标明来源。
//
// 唯一的例外是集群：LocalCluster **不模拟成功**（见 integration_local.go 的说明），
// 因为"部署显示已完成、集群毫无变化"不是降级，是一次假的写操作成功。
func NewSet(cfg *config.Config) *Set { return NewSetWithSources(cfg, nil, nil, nil) }

// NewSetWithGitLabSource 只注入 GitLab 的动态配置来源（保留给不需要 Jenkins 的调用方）。
func NewSetWithGitLabSource(cfg *config.Config, src GitLabConfigSource) *Set {
	return NewSetWithSources(cfg, src, nil, nil)
}

// NewSetWithSources 注入"运行期可变"的配置来源。
//
// 平台在「系统管理 › 第三方集成」页改写 GitLab / Jenkins 的地址与凭据后，
// 无需重启即可生效。任一 source 为 nil 时，该下游退化为只用 config.yaml
// 的静态配置（行为与加桥接之前一致）。
//
// k8sSrc 与上面两个不同：它不是"要不要用真实实现"的开关，而是**凭据从哪来**。
// 集群凭据（kubeconfig 内含 client-key）加密存在 secret_credential 表里，
// 只能由装配层用 db + sealer 解出来，integration 层自己够不着。
func NewSetWithSources(cfg *config.Config, glSrc GitLabConfigSource, jkSrc JenkinsConfigSource, k8sSrc KubeconfigSource) *Set {
	ic := cfg.Integration
	// 构建执行者按配置选，**选出来的名字必须如实反映它到底做了什么** ——
	// 界面上的「这次构建由谁执行」直接读 Builder.Name()，
	// 名字写错等于给用户一个错的答案。
	//
	// 为什么 mode=agent 时不是"什么都不装配"：那样构建会落回 LocalBuilder 打日志，
	// 于是用户在**以为**自己在用构建机的情况下拿到一条假成功记录。
	// 宁可明确地报"Agent 还没接上"。
	builder := Builder(NewLocalBuilder())
	switch {
	case strings.EqualFold(strings.TrimSpace(cfg.Runner.Mode), "agent"):
		builder = &UnavailableBuilder{Reason: "runner.mode=agent：任务需要由 Runner Agent 领取，但本平台尚未接入 Agent 通道。" +
			"请先新建并启动 Runner，或把 runner.mode 改回 inline。"}
	case strings.EqualFold(strings.TrimSpace(cfg.Runner.Executor), "simulated"):
		builder = NewLocalBuilder()
	default:
		builder = NewDockerBuilder("docker", cfg.Runner.BuildWorkRoot, cfg.Runner.BuildPlatform)
	}

	s := &Set{
		Registry: NewLocalRegistry(cfg),
		// GitLab 一律装配桥接，由它自己判断当前该用真实实现还是本地实现。
		// 若这里按启动时的 enabled 二选一，用户刚在界面上打开开关、
		// 页面却仍显示"本地模拟"，就只能靠重启来自证——那正是本次要消掉的体验。
		Repository: NewGitLabBridge(NewLocalRepository(), ic.GitLab, glSrc),
		Cluster:    NewLocalCluster(cfg),
		Builder:    builder,
		Jenkins:    NewJenkinsBridge(ic.Jenkins, jkSrc),
	}

	// Harbor 与 GitLab / Jenkins 不同：它**没有界面配置**，只读 config.yaml，
	// 所以这里按启动时的 enabled 直接二选一（改了配置要重启）。
	// 真接的对象是 /v2/（registry 协议），不是 /api/v2.0（Core）——原因见 registry_v2.go 文件头。
	if ic.Harbor.Enabled {
		s.Registry = NewHarborRegistry(ic.Harbor)
	}
	if ic.Kubernetes.Enabled {
		s.Cluster = NewK8sCluster(ic.Kubernetes, cfg.Runner.DeployTimeout.D(), k8sSrc)
	}
	return s
}

// Refresh 让"运行期可变"的下游重新解析配置。
//
// 读取实现名之前必须先 Refresh，否则 Name() 用的还是启动那一刻的结果。
func (s *Set) Refresh(ctx context.Context) {
	if b, ok := s.Repository.(*GitLabBridge); ok {
		b.Refresh(ctx)
	}
	if s.Jenkins != nil {
		s.Jenkins.Refresh(ctx)
	}
}

// Source 返回各下游当前的实现名，用于启动日志与健康检查接口，
// 让运维一眼看出"这个环境到底连上了哪些下游"。
func (s *Set) Source() map[string]string {
	out := map[string]string{
		"registry":   s.Registry.Name(),
		"repository": s.Repository.Name(),
		"cluster":    s.Cluster.Name(),
		"builder":    s.Builder.Name(),
	}
	if s.Jenkins != nil {
		out["jenkins"] = s.Jenkins.Name()
	}
	return out
}

// RepoIsSimulated 判断代码托管通道当前是不是**本地模拟**。
//
// ⚠️ 2026-10-10 起**本方法没有调用者**：编造来源已退役，不再需要这层兜底。
// 保留方法本身是因为它仍是"当前代码托管通道是不是模拟"的准确查询。
//
// 背景（保留，供理解它为什么曾经必须存在）：本地实现从前会**编一个提交号**出来
// （见 LocalRepository.LatestCommit），那个值在真实仓库里根本不存在。一旦交给构建器
// 去 `git fetch origin <sha>`，就必然报 `fatal: couldn't find remote ref` ——
// 用户看到的是"构建失败"，完全联想不到根因其实是"平台还没接 GitLab"。
//
// 现在提交号改由 `Set.ResolveCommit`（真跑 `git ls-remote`）取得，拿到的必然是真实 sha，
// 所以不再需要"本地模拟就置空"这层兜底；`LocalRepository.LatestCommit` 本身也已改成
// 如实报错，不会再产出任何值。
//
// 判断口径刻意写成"**只有** local 算模拟"，而不是"不是 gitlab 就算模拟"：
// 后者在接入 GitHub / Gitee 之后会把真实仓库误判成模拟，
// 于是提交号被丢掉、构建静默地跑在了分支 head 上 —— 那种错不报任何错。
func (s *Set) RepoIsSimulated() bool {
	return s.Repository == nil || s.Repository.Name() == "local"
}

// ProbeGit 真跑一次 `git ls-remote`，验证"这个地址 + 这套凭据，现在能不能读到代码"。
//
// 刻意不走 Repository.Branches：那条路走 GitLab REST API、用平台级令牌，
// 而这里要测的是**这个项目自己的账号密码能否拉到代码**。构建用的是 git 协议，
// 探测就必须也用 git 协议 —— 测另一条路只会给出"测试通过但构建 401"的假承诺。
func (s *Set) ProbeGit(ctx context.Context, req RepositoryProbe) ProbeResult {
	root, err := gitProbeRoot()
	if err != nil {
		return ProbeResult{OK: false, Message: err.Error()}
	}
	return GitProbe(ctx, ExecRunner, root, req)
}

// ResolveCommit 真跑 `git ls-remote`，取某个分支当前指向的提交号（用 req.Branch 指定分支）。
//
// 与 ProbeGit 同源：同一个工作目录、同一套凭据传递方式、同一条 git 协议。
// 刻意**不走** `Repository.LatestCommit` —— 那条路在未接 GitLab 时会落到
// LocalRepository 上，而它编出来的提交号在真实仓库里根本不存在，
// 拿它去 `git fetch origin <sha>` 必然报 `couldn't find remote ref`。
func (s *Set) ResolveCommit(ctx context.Context, req RepositoryProbe) (*Commit, error) {
	root, err := gitProbeRoot()
	if err != nil {
		return nil, err
	}
	return GitResolveCommit(ctx, ExecRunner, root, req)
}

// gitProbeRoot 探测用的临时工作目录；建不出来就返回带原因的错误。
//
// 目录**必须**先建出来：git 的 `cmd.Dir` 一旦指向不存在的目录，exec 会直接报
// `chdir ...: no such file or directory`，而那句话和"仓库连不上"毫无关系 ——
// 用户会以为地址写错了。实测踩过一次。
//
// 建不出来只能如实报（可能只是没权限），而不是让 git 去报一个指向别处的错。
func gitProbeRoot() (string, error) {
	root := filepath.Join(os.TempDir(), "yunnian-gitprobe")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("无法创建探测用的临时目录（%s）：%w", root, err)
	}
	return root, nil
}

// UpstreamError 包装下游错误，统一映射为 50200。
func UpstreamError(system string, err error) error {
	return fmt.Errorf("%s 调用失败: %w", system, err)
}
