package service

import (
	"encoding/json"
	"math"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/integration"
	"github.com/2777484478/yunnian/internal/model"
)

// 本文件是「数据库模型 → 前端视图模型」的转换层。
//
// 为什么不直接把 model 序列化出去：
//  1. **字段形状不同**：前端 Project.owner 是姓名（不是 ownerId）、lastBuild 是内嵌对象、
//     BuildRecord.image 来自 image_record 表。硬塞 model 会让前端做二次拼装。
//  2. **敏感字段必须白名单**：model 里有 PasswordHash / Cipher / GitCredential 等，
//     靠 `json:"-"` 漏一个就是数据泄露。DTO 是白名单，默认什么都不暴露。
//  3. **契约可测**：DTO 与 frontend/src/types/index.ts 一一对应，改字段时两边都必须动。
//
// 命名与类型严格对齐 frontend/src/types/index.ts。

// --------------------------------------------------------------------------- 通用

// AIDiagnosisDTO AI 归因结果。
//
// Source / Model 是后加的（旧数据没有，omitempty 兼容）：归因可能是模型给的，
// 也可能是错误码查表查出来的，列表页与详情页都得能分开说。
type AIDiagnosisDTO struct {
	Reason     string  `json:"reason"`
	Suggestion string  `json:"suggestion"`
	Confidence float64 `json:"confidence"`
	Source     string  `json:"source,omitempty"`
	Model      string  `json:"model,omitempty"`
}

// decodeAIDiagnosis 解析库里存的 AI 归因 JSON。解析不出来返回 nil（不报错）：
// 归因是附加信息，坏数据不该让整个列表接口 500。
func decodeAIDiagnosis(raw model.JSONText) *AIDiagnosisDTO {
	if raw.Empty() {
		return nil
	}
	var out AIDiagnosisDTO
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	if out.Reason == "" && out.Suggestion == "" {
		return nil
	}
	return &out
}

// decodeStringList 解析 JSON 数组列（deployedEnvs / buildParams…）。
func decodeStringList(raw model.JSONText) []string {
	if raw.Empty() {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

// --------------------------------------------------------------------------- 项目 / 构建 / 镜像

// BuildParamDTO 构建参数。
type BuildParamDTO struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Desc   string `json:"desc,omitempty"`
	Secret bool   `json:"secret,omitempty"`
}

// ProjectDTO 项目（CI 配置）。
type ProjectDTO struct {
	ID             string `json:"id"`
	Key            string `json:"key"`
	Name           string `json:"name"`
	Type           string `json:"type"`
	GitProvider    string `json:"gitProvider"`
	GitURL         string `json:"gitUrl"`
	GitUsername    string `json:"gitUsername"`
	DefaultBranch  string `json:"defaultBranch"`
	BuildMethod    string `json:"buildMethod"`
	JenkinsJob     string `json:"jenkinsJob,omitempty"`
	DockerfilePath string `json:"dockerfilePath"`
	BuildContext   string `json:"buildContext"`

	// BuildScript 「自定义脚本」模式下那段 shell 的正文。
	//
	// 与其它自建构建字段一样**无条件下发**（不按 buildMethod 过滤）：
	// 界面上切换构建方式时要能回填已写过的内容，过滤掉会让用户一切换就白写。
	BuildScript string `json:"buildScript"`

	Registry   string `json:"registry"`
	ImageName  string `json:"imageName"`
	TagPattern string `json:"tagPattern"`

	BuildParams []BuildParamDTO `json:"buildParams"`
	Owner       string          `json:"owner"`
	Team        string          `json:"team"`
	Description string          `json:"description"`
	Enabled     bool            `json:"enabled"`
	CreatedAt   string          `json:"createdAt"`
	UpdatedAt   string          `json:"updatedAt"`
	LastBuild   *BuildDTO       `json:"lastBuild,omitempty"`
	TotalBuilds int64           `json:"totalBuilds"`
	SuccessRate float64         `json:"successRate"`
	AvgDuration int64           `json:"avgDuration"`
}

// ProjectStats 项目维度的统计，由聚合查询一次算出，避免列表页 N+1。
type ProjectStats struct {
	Total       int64
	Success     int64
	AvgDuration float64
}

// BuildDTO 构建记录。
type BuildDTO struct {
	ID          string `json:"id"`
	BuildNo     uint32 `json:"buildNo"`
	ProjectKey  string `json:"projectKey"`
	ProjectName string `json:"projectName"`
	ProjectType string `json:"projectType"`
	Branch      string `json:"branch"`
	CommitID    string `json:"commitId"`
	CommitMsg   string `json:"commitMsg"`
	Image       string `json:"image"`
	ImageDigest string `json:"imageDigest"`
	// ImageSize 是**指针**：读不到仓库元数据时库里存的就是 NULL，此时必须下发 null
	// 而不是 0。下发 0 会让界面显示"0 B" —— 那是一句假话，它和"真的读到了 0 字节"
	// 完全不是一回事。（前端 humanSize(null) 渲染成 `-`。）
	ImageSize   *uint64 `json:"imageSize"`
	Status      string  `json:"status"`
	Duration    int64   `json:"duration"`
	Progress    uint8   `json:"progress"`
	Stage       string  `json:"stage"`
	Trigger     string  `json:"trigger"`
	TriggerType string  `json:"triggerType"`
	StartedAt   string  `json:"startedAt"`
	FinishedAt  string  `json:"finishedAt,omitempty"`
	// ErrorCode 机器码（用于筛选/统计），ErrorMessage 是给人看的真因。
	// 两者都下发：只有机器码解释不了现场，只有文本又没法筛选。
	ErrorCode    string          `json:"errorCode"`
	ErrorMessage string          `json:"errorMessage"`
	AIDiagnosis  *AIDiagnosisDTO `json:"aiDiagnosis,omitempty"`
	Logs         []string        `json:"logs"`
	// JenkinsJob / JenkinsBuildNo 只在 buildMethod=jenkins 的构建上有值。
	// 有了它们，界面上才能给出"在 Jenkins 中打开这次构建"的直达入口——
	// 平台同步的日志是转写过来的，排查时经常还是要回原站看。
	JenkinsJob     string `json:"jenkinsJob,omitempty"`
	JenkinsBuildNo uint32 `json:"jenkinsBuildNo,omitempty"`
}

// ImageDTO 镜像版本。
type ImageDTO struct {
	ID          string `json:"id"`
	ProjectKey  string `json:"projectKey"`
	ProjectName string `json:"projectName"`
	Tag         string `json:"tag"`
	Image       string `json:"image"`
	Digest      string `json:"digest"`
	// Size 可空，理由见 BuildDTO.ImageSize：读不到就是 null，不是 0。
	Size         *uint64  `json:"size"`
	Branch       string   `json:"branch"`
	CommitID     string   `json:"commitId"`
	BuildNo      uint32   `json:"buildNo"`
	Registry     string   `json:"registry"`
	PushedAt     string   `json:"pushedAt"`
	PullCount    uint32   `json:"pullCount"`
	DeployedEnvs []string `json:"deployedEnvs"`
}

// buildDTO 组装构建记录。proj / img 可能为 nil（项目已删除、尚未产出镜像）。
func buildDTO(b *model.BuildRecord, proj *model.Project, img *model.ImageRecord, logs []string) BuildDTO {
	out := BuildDTO{
		ID:           httpx.IDString(b.ID),
		BuildNo:      b.BuildNo,
		ProjectKey:   b.ProjectKey,
		Branch:       b.Branch,
		CommitID:     b.CommitID,
		CommitMsg:    b.CommitMsg,
		Status:       b.Status,
		Progress:     b.Progress,
		Stage:        b.Stage,
		Trigger:      b.TriggerName,
		TriggerType:  b.TriggerType,
		StartedAt:    formatTimePtr(b.StartedAt),
		FinishedAt:   formatTimePtr(b.FinishedAt),
		ErrorCode:    b.ErrorCode,
		ErrorMessage: b.ErrorMessage,
		AIDiagnosis:  decodeAIDiagnosis(b.AIDiagnosis),
		Logs:         logs,
		JenkinsJob:   b.JenkinsJob,
	}
	if b.JenkinsBuildNo != nil {
		out.JenkinsBuildNo = *b.JenkinsBuildNo
	}
	if out.Logs == nil {
		out.Logs = []string{}
	}
	if b.DurationSec != nil {
		out.Duration = int64(*b.DurationSec)
	} else if b.StartedAt != nil {
		end := now()
		if b.FinishedAt != nil {
			end = *b.FinishedAt
		}
		out.Duration = int64(end.Sub(*b.StartedAt).Seconds())
	}
	if proj != nil {
		out.ProjectName = proj.Name
		out.ProjectType = proj.Type
	}
	if img != nil {
		out.Image = img.Image
		out.ImageDigest = img.Digest
		out.ImageSize = img.SizeBytes
	}
	return out
}

func imageDTO(img *model.ImageRecord, projectName string) ImageDTO {
	out := ImageDTO{
		ID:           httpx.IDString(img.ID),
		ProjectKey:   img.ProjectKey,
		ProjectName:  projectName,
		Tag:          img.Tag,
		Image:        img.Image,
		Digest:       img.Digest,
		Branch:       img.Branch,
		CommitID:     img.CommitID,
		BuildNo:      img.BuildNo,
		Registry:     img.Registry,
		PushedAt:     formatTime(img.PushedAt),
		PullCount:    img.PullCount,
		DeployedEnvs: decodeStringList(img.DeployedEnv),
		// 直接把指针带过去：库里 NULL 就下发 null。见字段上的注释。
		Size: img.SizeBytes,
	}
	return out
}

// --------------------------------------------------------------------------- 环境 / 服务

// EnvVarDTO 环境变量。密文只回掩码。
type EnvVarDTO struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Secret bool   `json:"secret,omitempty"`
}

// EnvironmentDTO 环境。
type EnvironmentDTO struct {
	ID             string      `json:"id"`
	Key            string      `json:"key"`
	Name           string      `json:"name"`
	Cluster        string      `json:"cluster"`
	ClusterVersion string      `json:"clusterVersion"`
	Namespace      string      `json:"namespace"`
	DeployMethod   string      `json:"deployMethod"`
	ProjectKeys    []string    `json:"projectKeys"`
	Vars           []EnvVarDTO `json:"vars"`
	Registry       string      `json:"registry"`
	IngressDomain  string      `json:"ingressDomain"`
	// 发布阶段与链路序号（环境自带）：拓扑按 StageOrder 升序排链路，nil 排最后
	StageLabel string `json:"stageLabel"`
	StageOrder *int   `json:"stageOrder"`
	// HasKubeconfig：该环境是否配了自带的 kubeconfig。
	// **只回布尔、不回任何内容**（连 context 名都不回）——它是管理员级凭据，
	// 界面只需知道"有没有配"，回显细节只会扩大暴露面。
	HasKubeconfig bool     `json:"hasKubeconfig"`
	Owner         string   `json:"owner"`
	Description   string   `json:"description"`
	Enabled       bool     `json:"enabled"`
	NeedApproval  bool     `json:"needApproval"`
	Approvers     []string `json:"approvers"`
	// ApproverUsernames 与 Approvers **一一对应**（同一批人），只是换了个标识：
	// Approvers 是展示名（界面直接 `.join('、')` 给人看），这里是用户名。
	//
	// 为什么两套都要有：编辑表单提交的必须是**用户名**（后端 `resolveUsers` 按
	// name/username 解析成 ID），而只回展示名时表单没法预填 —— 于是"用户没重选"
	// 就等价于把审批人清空。分成两个字段而不是改 `Approvers` 的含义，
	// 是为了不动那十几处展示点。
	ApproverUsernames []string `json:"approverUsernames"`
	CreatedAt         string   `json:"createdAt"`
	UpdatedAt         string   `json:"updatedAt"`
	Status            string   `json:"status"`
	CPUUsage          uint8    `json:"cpuUsage"`
	MemUsage          uint8    `json:"memUsage"`
	Nodes             uint32   `json:"nodes"`
	LastSyncAt        string   `json:"lastSyncAt"`
}

// ServicePodDTO Pod 信息。
type ServicePodDTO struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Node     string `json:"node"`
	Restarts int    `json:"restarts"`
	Age      string `json:"age"`
	IP       string `json:"ip"`
	CPU      string `json:"cpu"`
	Mem      string `json:"mem"`
}

// DeployVersionDTO 历史版本（服务详情页的版本列表）。
type DeployVersionDTO struct {
	Tag        string `json:"tag"`
	Image      string `json:"image"`
	DeployedAt string `json:"deployedAt"`
	Operator   string `json:"operator"`
	Status     string `json:"status"`
	Note       string `json:"note,omitempty"`
}

// EnvServiceDTO 环境内的服务实例。
type EnvServiceDTO struct {
	ID            string      `json:"id"`
	EnvKey        string      `json:"envKey"`
	ProjectKey    string      `json:"projectKey"`
	ProjectName   string      `json:"projectName"`
	ProjectType   string      `json:"projectType"`
	Kind          string      `json:"kind"`
	Image         string      `json:"image"`
	Tag           string      `json:"tag"`
	Replicas      int         `json:"replicas"`
	ReadyReplicas int         `json:"readyReplicas"`
	Status        string      `json:"status"`
	CPURequest    string      `json:"cpuRequest"`
	CPULimit      string      `json:"cpuLimit"`
	MemRequest    string      `json:"memRequest"`
	MemLimit      string      `json:"memLimit"`
	EnvVars       []EnvVarDTO `json:"envVars"`
	ConfigMaps    []string    `json:"configMaps"`
	Secrets       []string    `json:"secrets"`
	Entrypoint    string      `json:"entrypoint"`
	UpdatedAt     string      `json:"updatedAt"`
	UpdatedBy     string      `json:"updatedBy"`
	// 这里曾有一个 `health: {liveness, readiness}`，2026-10-09 删除。
	// 它的数据源 `service_instance.liveness_ok / readiness_ok` 是**种子数据按平台 status
	// 反推**的（`seed_ci.go`：`liveness = status != "error"`、新建服务时写死 true），
	// 与集群里的探针**毫无关系**：实测 demobasic 库内两项皆 true、界面显示"通过"，
	// 而集群里新 Pod 正在 CrashLoopBackOff。留着它等于在同一个响应里给两个
	// 互相矛盾的健康口径。要健康状态就看下面的 `live`（读不到就如实说读不到）。
	Pods    []ServicePodDTO    `json:"pods"`
	History []DeployVersionDTO `json:"history"`

	// Live 集群实时状态。**界面上应当优先相信这一份**。
	//
	// 上面那些 replicas / status / image / tag 是平台库里的值，含义是"平台打算让它跑成什么样"
	// （纳管时按默认值写入，之后只有部署成功才更新），不代表集群里现在是什么样。
	// 此前界面直接显示它们，于是出现"平台说 2/2 运行中、集群里其实只有 1/1"这类假数据。
	Live *ServiceLiveDTO `json:"live"`
}

// ServiceLiveDTO 工作负载在集群里的实时状态。
//
// 三种情形必须分清——把它们混为一谈就是对用户撒谎：
//   - Available=true 且 Exists=true  → 读到了，字段即事实；
//   - Available=true 且 Exists=false → **确认**集群里没有这个工作负载（404）；
//   - Available=false               → **没读到**，Message 说明原因
//     （未接入集群 / 权限不足 / 网络超时）。这一种最容易被误渲染成"不存在"。
type ServiceLiveDTO struct {
	Available bool   `json:"available"`
	Exists    bool   `json:"exists"`
	Image     string `json:"image"`
	Tag       string `json:"tag"`
	Replicas  int    `json:"replicas"`
	// ActualReplicas 集群里实际存在的实例数（status.replicas）。
	//
	// 与 Replicas（期望数）分开：滚动更新卡住时新 Pod 反复崩溃、旧 Pod 不被杀，
	// 实际有 2 个而期望只有 1 个 —— 实测 demobasic `spec=1 / status=2 / ready=1`。
	// 界面必须按 `ready / max(期望, 实际)` 渲染，否则会写成 `1/1`
	// 把"有一个实例正在崩"抹掉（用户在自己的运维面板里看到的是 `1/2`）。
	ActualReplicas int    `json:"actualReplicas"`
	ReadyReplicas  int    `json:"readyReplicas"`
	Status         string `json:"status"`
	Revision       int    `json:"revision"`
	// 容器声明的资源规格（**不是使用量**）。空串表示容器没写，界面显示 "—"。
	// 使用量看下面那组 —— 两者必须分列展示，把声明值当用量看就是编数据。
	CPURequest string `json:"cpuRequest"`
	CPULimit   string `json:"cpuLimit"`
	MemRequest string `json:"memRequest"`
	MemLimit   string `json:"memLimit"`

	// ---------------------------------------------------------------- 实时用量
	//
	// MetricsAvailable=false 表示**没读到**用量：集群没装 metrics-server，
	// 或凭据无权读。此时 CPUUsage/MemUsage 为空串、两个百分比为 -1，界面显示 "—"。
	// **绝不能显示成 0** —— 那会被读成"这个服务几乎不占资源"，与事实正好相反。
	MetricsAvailable bool `json:"metricsAvailable"`
	// CPUUsage / MemUsage 该工作负载名下所有 Pod 的**合计**用量（CPU 毫核、内存 Mi/Gi）。
	CPUUsage string `json:"cpuUsage"`
	MemUsage string `json:"memUsage"`
	// CPUUsagePercent / MemUsagePercent 合计用量占**基准**的百分比（可能 >100）。
	//
	// -1 表示**算不出来**（limit 与 request 都没有，或都解析不了）。
	// 界面据此**不画进度条** —— 画一根 0% 的条会被读成"几乎没用"。
	CPUUsagePercent int `json:"cpuUsagePercent"`
	MemUsagePercent int `json:"memUsagePercent"`
	// CPUUsageBase / MemUsageBase 上面那个百分比**按什么算的**："limit" / "request" / ""。
	//
	// 必须跟着百分比一起回给前端，因为两者含义完全不同：
	//   limit   —— 超了会被限流 / OOM Kill，是"危险线"；
	//   request —— 只是调度申请量，**超了是常态**，不代表有问题。
	// 混成一句"占用率"会让人对着一个 400% 的数字去排查不存在的问题。
	//
	// 取值规则：优先 limit；没有 limit 退回 request（实测集群里这批容器
	// **只配了 requests 没配 limits**，只认 limit 的话这一列会全是 "—"）；
	// 两者都没有才是 ""，此时百分比为 -1。
	CPUUsageBase string `json:"cpuUsageBase"`
	MemUsageBase string `json:"memUsageBase"`
	// UsagePods 真的有用量数据的 Pod 数；UsagePodTotal 匹配到的 Pod 总数。
	//
	// 刚拉起来的 Pod 在 metrics-server 里要等几十秒才有数据，所以
	// UsagePods < UsagePodTotal 是常态 —— 这时上面的合计是**偏小**的，
	// 界面必须把"几个 Pod 里有几个算进去了"讲出来，否则它是个静默偏小的数字。
	UsagePods     int    `json:"usagePods"`
	UsagePodTotal int    `json:"usagePodTotal"`
	Message       string `json:"message"`
}

// liveOf 把集群读到的结果转成 DTO。
//
// err != nil 表示**没读到**，绝不能落进"不存在"那一支：
// 前者要说"为什么读不到"，后者要说"集群里确实没有"。
//
// usage 是**该工作负载的实时用量**（见 integration.WorkloadUsage）。它由调用方
// 批量取来后按名字取出（列表页一次覆盖整个命名空间；详情页点名单个），
// 不在这里自己去查 —— 这个函数是纯转换，不该有下游调用。
func liveOf(state *integration.WorkloadState, usage integration.WorkloadUsage, err error) *ServiceLiveDTO {
	if err != nil {
		return &ServiceLiveDTO{Available: false, Message: err.Error(), CPUUsagePercent: -1, MemUsagePercent: -1}
	}
	if state == nil {
		return &ServiceLiveDTO{Available: false, Message: "未取得集群状态", CPUUsagePercent: -1, MemUsagePercent: -1}
	}
	if !state.Exists {
		// 集群里没有这个工作负载 —— 谈用量没有意义，留 -1 让界面显示 "—"。
		return &ServiceLiveDTO{
			Available: true, Exists: false,
			Message:         stringOr(state.Message, "集群中不存在该工作负载"),
			CPUUsagePercent: -1, MemUsagePercent: -1,
		}
	}
	dto := &ServiceLiveDTO{
		Available: true, Exists: true,
		Image: state.Image, Tag: state.Tag,
		Replicas: state.Replicas, ReadyReplicas: state.ReadyReplicas,
		ActualReplicas: state.ActualReplicas,
		Status:         state.Status, Revision: state.Revision,
		CPURequest: state.CPURequest, CPULimit: state.CPULimit,
		MemRequest: state.MemRequest, MemLimit: state.MemLimit,
		Message: state.Message,
	}
	applyUsage(dto, usage, state.CPURequest, state.CPULimit, state.MemRequest, state.MemLimit)
	return dto
}

// applyUsage 把集群读到的用量填进 DTO，并算出占 limit 的百分比。
//
// 用量与百分比**必须一起定**，不能各写各的：百分比要有"基准"，基准就是容器声明的 limit。
func applyUsage(d *ServiceLiveDTO, u integration.WorkloadUsage, cpuReq, cpuLimit, memReq, memLimit string) {
	if !u.Available {
		// 没读到：留空 + -1，界面显示 "—"。**不填 0**。
		d.CPUUsagePercent, d.MemUsagePercent = -1, -1
		return
	}
	d.MetricsAvailable = true
	d.CPUUsage, d.MemUsage = u.CPU, u.Mem
	d.UsagePods, d.UsagePodTotal = u.UsagePods, u.Pods
	d.CPUUsageBase, d.CPUUsagePercent = usageRatioCPU(u.CPUMilli, cpuLimit, cpuReq)
	d.MemUsageBase, d.MemUsagePercent = usageRatioMem(u.MemBytes, memLimit, memReq)
}

// usageRatioCPU 选一个可用基准算 CPU 占用率，并**回报用的是哪个基准**。
//
// 优先 limit、退回 request：实测这台集群里纳管的容器**只配了 requests 没配 limits**
// （`{"requests":{"cpu":"10m","memory":"10Mi"}}`），只认 limit 的话百分比永远算不出来，
// 这一列就废了。但两者不是一回事（超 request 只是调度层面的事，超 limit 才会被限流），
// 所以基准必须跟着数字一起回给界面，由界面写明，不能合成一句"占用率"。
func usageRatioCPU(usedMilli int64, limit, request string) (string, int) {
	if b := cpuQuantityMilli(limit); b > 0 {
		return "limit", percentOf(usedMilli, b)
	}
	if b := cpuQuantityMilli(request); b > 0 {
		return "request", percentOf(usedMilli, b)
	}
	return "", -1
}

// usageRatioMem 同 usageRatioCPU，内存版（单位是字节）。
func usageRatioMem(usedBytes int64, limit, request string) (string, int) {
	if b := memQuantityBytes(limit); b > 0 {
		return "limit", percentOf(usedBytes, b)
	}
	if b := memQuantityBytes(request); b > 0 {
		return "request", percentOf(usedBytes, b)
	}
	return "", -1
}

// percentOf 算 used 占 base 的百分比（四舍五入，可能 >100）。
//
// base <= 0 时返回 **-1**，不是 0：它表示"没有可比的上限"（容器没配 limit 的话，
// 集群根本不会限制它，谈占用率没有意义），界面据此不画进度条。
// 给 0 会被画成一根空条，读起来是"几乎没用"，与事实不符。
func percentOf(used, base int64) int {
	if base <= 0 {
		return -1
	}
	return int(math.Round(float64(used) / float64(base) * 100))
}

// cpuQuantityMilli 把资源声明文本（如 "1"、"500m"）解析成毫核；解析不了返回 0。
//
// ⚠️ CPU 与内存的解析**必须分开**：`ParseQuantity("1").MilliValue()` = 1000（1 核 = 1000 毫核），
// 而 `ParseQuantity("1Gi").Value()` = 1073741824（字节）。用错一侧就是差 1000 倍，
// 而且不会报错 —— 只会给出一个看起来很正常的百分比。
func cpuQuantityMilli(q string) int64 {
	v, err := resource.ParseQuantity(strings.TrimSpace(q))
	if err != nil {
		return 0
	}
	return v.MilliValue()
}

// memQuantityBytes 把资源声明文本（如 "1Gi"、"512Mi"）解析成字节；解析不了返回 0。
func memQuantityBytes(q string) int64 {
	v, err := resource.ParseQuantity(strings.TrimSpace(q))
	if err != nil {
		return 0
	}
	return v.Value()
}

// envToDTO 组装环境。projectKeys / vars 由调用方预取（避免 N+1）。
func envToDTO(e *model.Environment, ownerName string, approvers envApprovers, projectKeys []string, vars []EnvVarDTO) EnvironmentDTO {
	if projectKeys == nil {
		projectKeys = []string{}
	}
	if approvers.Names == nil {
		approvers.Names = []string{}
	}
	if approvers.Usernames == nil {
		approvers.Usernames = []string{}
	}
	if vars == nil {
		vars = []EnvVarDTO{}
	}
	return EnvironmentDTO{
		ID:             httpx.IDString(e.ID),
		Key:            e.EnvKey,
		Name:           e.Name,
		Cluster:        e.Cluster,
		ClusterVersion: e.ClusterVer,
		Namespace:      e.Namespace,
		DeployMethod:   e.DeployMethod,
		ProjectKeys:    projectKeys,
		Vars:           vars,
		Registry:       e.Registry,
		IngressDomain:  e.IngressDomain,
		StageLabel:     e.StageLabel,
		StageOrder:     e.StageOrder,
		// 只回"配没配"，不回任何内容——kubeconfig 是管理员级凭据
		HasKubeconfig:     e.KubeCredID != nil,
		Owner:             ownerName,
		Description:       e.Description,
		Enabled:           e.Enabled,
		NeedApproval:      e.NeedApproval,
		Approvers:         approvers.Names,
		ApproverUsernames: approvers.Usernames,
		CreatedAt:         formatTime(e.CreatedAt),
		UpdatedAt:         formatTime(e.UpdatedAt),
		Status:            e.HealthStatus,
		CPUUsage:          e.CPUUsage,
		MemUsage:          e.MemUsage,
		Nodes:             e.NodeCount,
		LastSyncAt:        formatTimePtr(e.LastSyncAt),
	}
}

// serviceToDTO 组装服务实例。
func serviceToDTO(s *model.ServiceInstance, proj *model.Project, updaterName string, pods []ServicePodDTO, history []DeployVersionDTO, live *ServiceLiveDTO) EnvServiceDTO {
	out := EnvServiceDTO{
		ID:            httpx.IDString(s.ID),
		EnvKey:        s.EnvKey,
		ProjectKey:    s.ProjectKey,
		Kind:          s.WorkloadKind,
		Image:         s.CurrentImage,
		Tag:           s.CurrentTag,
		Replicas:      s.Replicas,
		ReadyReplicas: s.ReadyReplicas,
		Status:        s.Status,
		CPURequest:    s.CPURequest,
		CPULimit:      s.CPULimit,
		MemRequest:    s.MemRequest,
		MemLimit:      s.MemLimit,
		EnvVars:       decodeEnvVars(s.EnvVars),
		ConfigMaps:    decodeStringList(s.ConfigMaps),
		Secrets:       decodeStringList(s.Secrets),
		Entrypoint:    s.Entrypoint,
		UpdatedAt:     formatTime(s.UpdatedAt),
		UpdatedBy:     updaterName,
		Pods:          pods,
		History:       history,
		Live:          live,
	}
	if out.EnvVars == nil {
		out.EnvVars = []EnvVarDTO{}
	}
	if out.Pods == nil {
		out.Pods = []ServicePodDTO{}
	}
	if out.History == nil {
		out.History = []DeployVersionDTO{}
	}
	if proj != nil {
		out.ProjectName = proj.Name
		out.ProjectType = proj.Type
	}
	return out
}

// decodeEnvVars 解析服务实例上的环境变量 JSON。
//
// 密文项（secret=true）实时替换成掩码：库里存的是明文（服务变量需要真实注入），
// 但接口一律不回明文——前端只需要知道"有这么一个变量、值已配置"。
func decodeEnvVars(raw model.JSONText) []EnvVarDTO {
	if raw.Empty() {
		return nil
	}
	var list []EnvVarDTO
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	for i := range list {
		if list[i].Secret {
			list[i].Value = maskValue(list[i].Value)
		}
	}
	return list
}

// maskValue 生成"已配置"的掩码。保留长度信息对排障有用（能看出是不是被截断了）。
func maskValue(v string) string {
	if strings.TrimSpace(v) == "" {
		return ""
	}
	return "******"
}

// --------------------------------------------------------------------------- 部署 / 回滚

// DeployStepDTO 部署步骤。
type DeployStepDTO struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	At     string `json:"at,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// DeployDTO 部署记录。
type DeployDTO struct {
	ID          string `json:"id"`
	DeployNo    string `json:"deployNo"`
	EnvKey      string `json:"envKey"`
	EnvName     string `json:"envName"`
	ProjectKey  string `json:"projectKey"`
	ProjectName string `json:"projectName"`
	Service     string `json:"service"`
	Image       string `json:"image"`
	// ImageDigest 这次部署实际用的那份镜像的摘要。从「仓库标签」发起的部署没有
	// image_record 可查，这个字段就是"当时发的是哪一份"的唯一答案（同 tag 被重推过
	// 就指向别的内容了）。仓库读不到时为空 —— 不拿 tag 或时间凑。
	ImageDigest string          `json:"imageDigest,omitempty"`
	Replicas    int             `json:"replicas"`
	Status      string          `json:"status"`
	Progress    uint8           `json:"progress"`
	Steps       []DeployStepDTO `json:"steps"`
	Operator    string          `json:"operator"`
	Approver    string          `json:"approver,omitempty"`
	StartedAt   string          `json:"startedAt"`
	FinishedAt  string          `json:"finishedAt,omitempty"`
	Duration    int64           `json:"duration"`
	// ErrorCode 机器码 / ErrorMessage 人类可读真因，见 BuildDTO 同名注释。
	ErrorCode    string `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
	// RecoveredAt 判失败**之后**集群自己就绪的时间（只有 ROLLOUT_STUCK 会填）。
	// 与 Status 并存而不是替代它：失败是当时的事实，这条是事后事实，
	// 前端据此标「失败后已就绪」——否则用户看到红色失败会以为服务还挂着，又重发一次。
	RecoveredAt string `json:"recoveredAt,omitempty"`
	RollbackOf  string `json:"rollbackOf,omitempty"`
	AIDiagnosis  *AIDiagnosisDTO `json:"aiDiagnosis,omitempty"`
	Logs         []string        `json:"logs"`
}

// RollbackDTO 回滚记录。
type RollbackDTO struct {
	ID         string `json:"id"`
	DeployID   string `json:"deployId"`
	EnvKey     string `json:"envKey"`
	EnvName    string `json:"envName"`
	ProjectKey string `json:"projectKey"`
	Service    string `json:"service"`
	FromImage  string `json:"fromImage"`
	ToImage    string `json:"toImage"`
	Reason     string `json:"reason"`
	Operator   string `json:"operator"`
	Status     string `json:"status"`
	StartedAt  string `json:"startedAt"`
	Duration   int64  `json:"duration"`
	// DeployNo 是业务单号（D2026092200049），DeployID 才是部署单主键。
	// 回滚的执行实体就是那次部署，前端「查看实时日志 / 详情」都要靠 DeployID 定位，
	// 所以两个都下发：DeployNo 给人看，DeployID 给接口用。
	DeployNo    string  `json:"deployNo"`
	TriggerType string  `json:"triggerType"`
	AIConfid    float64 `json:"-"`
}

// deployDTO 组装部署记录。steps / logs 由调用方按需预取（列表页不带日志）。
func deployDTO(d *model.DeployRecord, envName, projectName string, steps []DeployStepDTO, logs []string) DeployDTO {
	if steps == nil {
		steps = []DeployStepDTO{}
	}
	if logs == nil {
		logs = []string{}
	}
	out := DeployDTO{
		ID:           httpx.IDString(d.ID),
		DeployNo:     d.DeployNo,
		EnvKey:       d.EnvKey,
		EnvName:      envName,
		ProjectKey:   d.ProjectKey,
		ProjectName:  projectName,
		Service:      projectName,
		Image:        d.Image,
		ImageDigest:  d.ImageDigest,
		Replicas:     d.Replicas,
		Status:       d.Status,
		Progress:     d.Progress,
		Steps:        steps,
		Operator:     d.OperatorName,
		Approver:     d.ApproverName,
		StartedAt:    formatTimePtr(d.StartedAt),
		FinishedAt:   formatTimePtr(d.FinishedAt),
		ErrorCode:    d.ErrorCode,
		ErrorMessage: d.ErrorMessage,
		RecoveredAt:  formatTimePtr(d.RecoveredAt),
		AIDiagnosis:  decodeAIDiagnosis(d.AIDiagnosis),
		Logs:         logs,
	}
	if out.StartedAt == "" {
		out.StartedAt = formatTime(d.CreatedAt)
	}
	if d.DurationSec != nil {
		out.Duration = int64(*d.DurationSec)
	} else if d.StartedAt != nil {
		end := now()
		if d.FinishedAt != nil {
			end = *d.FinishedAt
		}
		out.Duration = int64(end.Sub(*d.StartedAt).Seconds())
	}
	return out
}

func rollbackDTO(r *model.RollbackRecord, envName, projectName string) RollbackDTO {
	out := RollbackDTO{
		ID:          httpx.IDString(r.ID),
		DeployID:    httpx.IDString(r.DeployID),
		EnvKey:      r.EnvKey,
		EnvName:     envName,
		ProjectKey:  r.ProjectKey,
		Service:     projectName,
		FromImage:   r.FromImage,
		ToImage:     r.ToImage,
		Reason:      r.Reason,
		Operator:    r.OperatorName,
		Status:      r.Status,
		StartedAt:   formatTime(r.CreatedAt),
		DeployNo:    r.DeployNo,
		TriggerType: r.TriggerType,
	}
	if r.DurationSec != nil {
		out.Duration = int64(*r.DurationSec)
	}
	if r.AIConfidence != nil {
		out.AIConfid = *r.AIConfidence
	}
	return out
}

// --------------------------------------------------------------------------- 系统

// AuditLogDTO 审计日志。
type AuditLogDTO struct {
	ID         string `json:"id"`
	User       string `json:"user"`
	UserName   string `json:"userName"`
	Role       string `json:"role"`
	Module     string `json:"module"`
	Action     string `json:"action"`
	TargetType string `json:"targetType"`
	Target     string `json:"target"`
	Detail     string `json:"detail"`
	IP         string `json:"ip"`
	Result     string `json:"result"`
	At         string `json:"at"`
}

func auditToDTO(a *model.AuditLog) AuditLogDTO {
	return AuditLogDTO{
		ID:         httpx.IDString(a.ID),
		User:       a.Username,
		UserName:   a.UserName,
		Role:       a.RoleCode,
		Module:     a.Module,
		Action:     a.Action,
		TargetType: a.TargetType,
		Target:     a.TargetName,
		Detail:     a.Detail,
		IP:         a.IP,
		Result:     a.Result,
		At:         formatTime(a.CreatedAt),
	}
}

// SysUserDTO 系统用户。
type SysUserDTO struct {
	ID          string   `json:"id"`
	Username    string   `json:"username"`
	Name        string   `json:"name"`
	Email       string   `json:"email"`
	Phone       string   `json:"phone"`
	Role        string   `json:"role"`
	Teams       []string `json:"teams"`
	Enabled     bool     `json:"enabled"`
	LastLoginAt string   `json:"lastLoginAt"`
	CreatedAt   string   `json:"createdAt"`
}

func userToDTO(u *model.User, roleCode, team string) SysUserDTO {
	teams := []string{}
	if team != "" {
		teams = append(teams, team)
	}
	return SysUserDTO{
		ID:          httpx.IDString(u.ID),
		Username:    u.Username,
		Name:        u.Name,
		Email:       u.Email,
		Phone:       u.Phone,
		Role:        roleCode,
		Teams:       teams,
		Enabled:     u.Enabled,
		LastLoginAt: formatTimePtr(u.LastLoginAt),
		CreatedAt:   formatTime(u.CreatedAt),
	}
}

// UserOptionDTO 「选人」下拉用的精简用户信息。
//
// 刻意**只回这三个字段**：调用方（环境的负责人 / 审批人选择器）只需要「展示名 + 唯一标识」。
// 邮箱、手机、角色、启停状态属于用户管理范畴，只有 `user:manage` 才该看到 ——
// 那是 `GET /users` 的既有语义，本接口不碰它。
//
// 存在的理由：这两个下拉原先直接调 `GET /users`（要 `user:manage`），
// 于是持有 `env:write` 的环境维护者调它返 `40300`、候选恒为空 ——
// 抽屉能打开，却一个人都选不到（负责人换不了、审批人加不了）。
type UserOptionDTO struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

// SysRoleDTO 角色。
type SysRoleDTO struct {
	Key   string   `json:"key"`
	Name  string   `json:"name"`
	Desc  string   `json:"desc"`
	Users int64    `json:"users"`
	Color string   `json:"color"`
	Perms []string `json:"perms"`
}

// NotifyChannelDTO 通知渠道。
type NotifyChannelDTO struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Target  string   `json:"target"`
	Enabled bool     `json:"enabled"`
	Events  []string `json:"events"`
	// 投递状态。这三个字段是"我配了渠道，可消息没收到"唯一的自查入口 ——
	// 没有它们，用户只能猜是自己没订阅、还是平台压根没发出去。
	// LastSentAt 为空 = 从未成功发过（不是"没发过"：失败会体现在 FailCount/LastError 上）。
	LastSentAt string `json:"lastSentAt,omitempty"`
	FailCount  uint32 `json:"failCount"`
	LastError  string `json:"lastError,omitempty"`
	UpdatedAt  string `json:"updatedAt"`
}

func notifyToDTO(n *model.NotifyChannel) NotifyChannelDTO {
	return NotifyChannelDTO{
		ID:         httpx.IDString(n.ID),
		Type:       n.Type,
		Name:       n.Name,
		Target:     n.Target,
		Enabled:    n.Enabled,
		Events:     decodeStringList(n.Events),
		LastSentAt: formatTimePtr(n.LastSentAt),
		FailCount:  n.FailCount,
		LastError:  n.LastError,
		UpdatedAt:  formatTime(n.UpdatedAt),
	}
}

// ModelProviderDTO 大模型通道。密钥只回掩码。
type ModelProviderDTO struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Vendor         string   `json:"vendor"`
	BaseURL        string   `json:"baseUrl"`
	APIKeyMasked   string   `json:"apiKeyMasked"`
	Models         []string `json:"models"`
	DefaultModel   string   `json:"defaultModel"`
	Enabled        bool     `json:"enabled"`
	IsDefault      bool     `json:"isDefault"`
	Timeout        int      `json:"timeout"`
	MaxConcurrency int      `json:"maxConcurrency"`
	Temperature    float64  `json:"temperature"`
	MaxTokens      int      `json:"maxTokens"`
	Status         string   `json:"status"`
	Latency        *int     `json:"latency,omitempty"`
	CheckedAt      string   `json:"checkedAt,omitempty"`
	CallCount      uint32   `json:"callCount"`
	FailCount      uint32   `json:"failCount"`
	Desc           string   `json:"desc"`
	UpdatedAt      string   `json:"updatedAt"`
}

func modelToDTO(m *model.AIModelConfig) ModelProviderDTO {
	out := ModelProviderDTO{
		ID:             httpx.IDString(m.ID),
		Name:           m.Name,
		Vendor:         m.Vendor,
		BaseURL:        m.BaseURL,
		APIKeyMasked:   m.APIKeyMasked,
		Models:         decodeStringList(m.Models),
		DefaultModel:   m.DefaultModel,
		Enabled:        m.Enabled,
		IsDefault:      m.IsDefault,
		Timeout:        m.TimeoutSec,
		MaxConcurrency: m.MaxConcurrency,
		Temperature:    m.Temperature,
		MaxTokens:      m.MaxTokens,
		Status:         m.Status,
		Latency:        m.LatencyMS,
		CheckedAt:      formatTimePtr(m.CheckedAt),
		CallCount:      m.CallCount,
		FailCount:      m.FailCount,
		Desc:           m.Description,
		UpdatedAt:      formatTime(m.UpdatedAt),
	}
	// 判断"有没有密钥"只看**密文**，不看掩码：掩码是从明文派生的展示值，
	// 明文不存在时掩码必然不可信。原型期的种子数据就踩过这个坑——
	// 库里存着一串 `sk-****************a3f9` 但密文为空，
	// 照抄掩码会让界面显示"密钥已配置"，而真实调用一定 401。
	if len(m.APIKeyCipher) == 0 || out.APIKeyMasked == "" {
		out.APIKeyMasked = "未配置"
	}
	return out
}

// SystemConfigDTO 系统配置项。
type SystemConfigDTO struct {
	Key         string `json:"configKey"`
	Value       string `json:"configValue"`
	ValueType   string `json:"valueType"`
	Group       string `json:"groupName"`
	Description string `json:"description"`
	UpdatedAt   string `json:"updatedAt"`
}

// StatsDTO 概览页统计。
type StatsDTO struct {
	Projects    int64 `json:"projects"`
	TodayBuilds int64 `json:"todayBuilds"`
	// TodaySuccess / TodayFailed 只统计今日**已结束**（success/failed/canceled）的构建。
	// 排除了进行中与排队中——把它们算进失败会让成功率在构建期间无故下跌。
	// 两者相加不一定等于 TodayBuilds，差值就是还没跑完的那部分，这是刻意的。
	TodaySuccess    int64   `json:"todaySuccess"`
	TodayFailed     int64   `json:"todayFailed"`
	SuccessRate     float64 `json:"successRate"`
	Environments    int64   `json:"environments"`
	RunningBuilds   int64   `json:"runningBuilds"`
	RunningDeploys  int64   `json:"runningDeploys"`
	PendingApproval int64   `json:"pendingApproval"`
	TotalImages     int64   `json:"totalImages"`
	TotalDeploys    int64   `json:"totalDeploys"`
	TotalRollbacks  int64   `json:"totalRollbacks"`
	ActiveServices  int64   `json:"activeServices"`
}

// IntegrationStatusDTO 第三方集成状态。
type IntegrationStatusDTO struct {
	Key     string `json:"key"`
	Name    string `json:"name"`
	Impl    string `json:"impl"`
	Enabled bool   `json:"enabled"`
	Ready   bool   `json:"ready"`
	Detail  string `json:"detail"`
}

// LoginResult 登录响应，形状对齐 docs/03 §1。
type LoginResult struct {
	AccessToken  string         `json:"accessToken"`
	RefreshToken string         `json:"refreshToken"`
	TokenType    string         `json:"tokenType"`
	ExpiresIn    int            `json:"expiresIn"`
	User         CurrentUserDTO `json:"user"`
}

// RoleBrief 角色简要信息。
type RoleBrief struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// CurrentUserDTO 当前用户。
//
// 角色同时下发**嵌套与扁平两种形态**，这是一个刻意的兼容决定：
// docs/03 自己就不一致——§1 的登录响应写的是 `role: {code,name}`，
// §9 的审计响应写的是扁平的 `role: "ops"`；而前端 SessionUser 用的是
// `role`（代码）+ `roleName`（中文名）两个扁平字段。
// 与其让前端在联调时为了这两处差异写适配层，不如后端把两种都给出：
// 嵌套形态给文档对齐用，扁平那对给前端直接用。字段冗余但语义明确，不产生歧义。
type CurrentUserDTO struct {
	ID       string    `json:"id"`
	Username string    `json:"username"`
	Name     string    `json:"name"`
	Email    string    `json:"email"`
	Role     RoleBrief `json:"role"`
	RoleCode string    `json:"roleCode"`
	RoleName string    `json:"roleName"`
	Teams    []string  `json:"teams"`
	// Permissions 是展开后的权限点（含 env:* / *:read 这类通配原样保留），
	// 前端用它控制入口显隐；服务端鉴权时由 auth.Match 负责通配展开。
	Permissions []string `json:"permissions"`
}

// --------------------------------------------------------------------------- K8s 集群接入

// K8sClusterDTO 集群对外的表示。
//
// **没有任何字段承载 kubeconfig**（明文或密文都不行），只有一个布尔 hasKubeconfig。
// 这不是"暂时没做"，而是设计：kubeconfig 含 client-key / token，属集群管理员级凭据，
// 一旦有读回明文的出口，它就会出现在浏览器缓存、前端日志、截图里。
type K8sClusterDTO struct {
	Key               string `json:"key"`
	Name              string `json:"name"`
	APIServer         string `json:"apiServer"`
	Context           string `json:"context"`
	KubeconfigCluster string `json:"kubeconfigCluster"`
	K8sVersion        string `json:"k8sVersion"`
	// 下面三个用指针：null = 没读到（无权 / 不可达），0 = 读到且确实是 0。
	// 用 0 表示"没读到"会让页面显示"0 个节点"，那是编的。
	Nodes          *int `json:"nodes"`
	NodesReady     *int `json:"nodesReady"`
	NamespaceCount *int `json:"namespaceCount"`

	Status           string `json:"status"`
	LastProbedAt     string `json:"lastProbedAt"`
	LastProbeMessage string `json:"lastProbeMessage"`
	LastProbeMS      int    `json:"lastProbeMs"`
	HasKubeconfig    bool   `json:"hasKubeconfig"`
	// UsedByEnvs 引用该集群的环境 key。删除按钮靠它预先禁用；
	// 非空时后端也会拒绝删除，两边口径一致。
	UsedByEnvs []string `json:"usedByEnvs"`
	Remark     string   `json:"remark"`
	CreatedAt  string   `json:"createdAt"`
	UpdatedAt  string   `json:"updatedAt"`
}

// ClusterContextDTO kubeconfig 里的一个 context。
type ClusterContextDTO struct {
	Context         string `json:"context"`
	Cluster         string `json:"cluster"`
	APIServer       string `json:"apiServer"`
	Namespace       string `json:"namespace"`
	User            string `json:"user"`
	CredentialKind  string `json:"credentialKind"`
	Problem         string `json:"problem"`
	AlreadyImported bool   `json:"alreadyImported"`
}

// ClusterParseResultDTO 解析结果。ok=false 时 errors 里是原因。
type ClusterParseResultDTO struct {
	OK             bool                `json:"ok"`
	Errors         []string            `json:"errors"`
	CurrentContext string              `json:"currentContext"`
	Contexts       []ClusterContextDTO `json:"contexts"`
}

// ClusterParseInput 解析入参。
type ClusterParseInput struct {
	Kubeconfig string `json:"kubeconfig" binding:"required"`
}

// ClusterInput 单个 context 的接入入参。
type ClusterInput struct {
	Key     string `json:"key" binding:"required,max=64"`
	Name    string `json:"name" binding:"max=128"`
	Context string `json:"context" binding:"max=191"`
	// Kubeconfig 只在接入/更换凭据时提供，且**整份原文**都要给——
	// 平台保存的是最小化后的单 context，无法凭它反推出别的 context。
	Kubeconfig string `json:"kubeconfig"`
	Remark     string `json:"remark" binding:"max=255"`
}

// ClusterCreateInput 批量接入入参。
type ClusterCreateInput struct {
	Clusters []ClusterInput `json:"clusters" binding:"required,min=1,dive"`
}

// ClusterCreateError 批量接入里失败的那一项。必须带 context 名，
// 否则用户不知道是哪一项没进来（见 ClusterCreateResultDTO 的注释）。
type ClusterCreateError struct {
	Context string `json:"context"`
	Message string `json:"message"`
}

// ClusterCreateResultDTO 批量接入结果：**部分成功语义**。
//
// 为什么不是"全成或全败"：一次导入常含多个 context，某个集群网络不通是常事。
// 整批回滚会让用户挨个重试；只返回成功项则用户不知道另外几个为什么没出现。
type ClusterCreateResultDTO struct {
	Clusters []K8sClusterDTO      `json:"clusters"`
	Errors   []ClusterCreateError `json:"errors"`
}

// ClusterUpdateInput 编辑入参。
//
// 三个字段的"空值含义"各不相同，是刻意的：
//   - Name 为空 → 不修改（名称不允许被清空）；
//   - Remark 用指针 → 传空串表示"清掉备注"，nil 表示不修改；
//   - Context / Kubeconfig 为空 → 不修改凭据（平台不存原文，无法"只换 context"）。
//
// 把"不修改"与"清空"混为一谈的代价很具体：前端只看得到"已配置"、
// 回传不了 kubeconfig 原文，若空串被当成清空，改一次备注就会顺手把凭据删掉。
type ClusterUpdateInput struct {
	Name       string  `json:"name" binding:"max=128"`
	Context    string  `json:"context" binding:"max=191"`
	Kubeconfig string  `json:"kubeconfig"`
	Remark     *string `json:"remark" binding:"omitempty,max=255"`
}

// ClusterTestResultDTO 单集群探测结果。
// ok=false 也是 HTTP 200（业务成功、结论为不通），与 GitLab / Jenkins 同约定。
type ClusterTestResultDTO struct {
	Key            string `json:"key"`
	OK             bool   `json:"ok"`
	Message        string `json:"message"`
	LatencyMS      int    `json:"latencyMs"`
	K8sVersion     string `json:"k8sVersion"`
	Nodes          *int   `json:"nodes"`
	NodesReady     *int   `json:"nodesReady"`
	NamespaceCount *int   `json:"namespaceCount"`
	// Warnings 「能连上但迟早出事」类提醒（如凭据有效但无权列节点）。
	// 与 ok 无关，必须原样展示——它是这次探测里唯一有价值的新信息。
	Warnings []string `json:"warnings"`
}
