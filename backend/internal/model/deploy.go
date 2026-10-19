package model

import (
	"time"

	"gorm.io/gorm"
)

// --------------------------------------------------------------------------- 4. 环境

// Environment 部署环境（一个 K8s 集群 + Namespace 的组合）。
type Environment struct {
	ID         uint64  `gorm:"primaryKey;autoIncrement" json:"-"`
	EnvKey     string  `gorm:"column:env_key;size:64;uniqueIndex:uk_env_key" json:"key"`
	Name       string  `gorm:"size:64" json:"name"`
	Cluster    string  `gorm:"size:128" json:"cluster"`
	ClusterVer string  `gorm:"column:cluster_version;size:32" json:"clusterVersion"`
	Namespace  string  `gorm:"size:128" json:"namespace"`
	KubeCredID *uint64 `gorm:"column:kubeconfig_credential_id" json:"-"`
	// DeployMethod 是**原型期留下的列**，平台改集群只有一条路：写 AppRelease CR，
	// 由 operator 下发到 Deployment —— 全仓没有任何代码读这一列。
	// 保留列是为了不改表结构（可能残留 helm/compose 的历史值），但**不再对外提供选择**，
	// 前端也不消费（见 service.EnvironmentInput.DeployMethod 的说明）。2026-10-15 收敛。
	DeployMethod string `gorm:"size:16;not null;default:k8s" json:"deployMethod"`
	// HelmChart 更彻底：连写入代码都没有过，是原型建表时留的空列。
	HelmChart     string `gorm:"size:255" json:"-"`
	Registry      string `gorm:"size:128" json:"registry"`
	IngressDomain string `gorm:"size:191" json:"ingressDomain"`
	// StageLabel / StageOrder：发布阶段名与链路序号，由环境自带（而不是前端按 key 硬映射），
	// 这样自定义环境（如「华东压测区」）也能排进链路，而不是被扔到最后。
	// StageOrder 用指针：nil = 没设（排到所有设了序号的之后），0 与"没设"必须能区分开。
	StageLabel   string         `gorm:"column:stage_label;size:32" json:"stageLabel"`
	StageOrder   *int           `gorm:"column:stage_order" json:"stageOrder"`
	PullSecret   string         `gorm:"column:image_pull_secret;size:128" json:"-"`
	NeedApproval bool           `gorm:"not null;default:false" json:"needApproval"`
	ApproverIDs  JSONText       `gorm:"column:approver_ids;type:json" json:"-"`
	OwnerID      uint64         `gorm:"not null" json:"-"`
	Description  string         `gorm:"size:512" json:"description"`
	Enabled      bool           `gorm:"not null;default:true;index:idx_env_enabled" json:"enabled"`
	HealthStatus string         `gorm:"column:health_status;size:16;not null;default:unknown" json:"status"`
	CPUUsage     uint8          `gorm:"column:cpu_usage;not null;default:0" json:"cpuUsage"`
	MemUsage     uint8          `gorm:"column:mem_usage;not null;default:0" json:"memUsage"`
	NodeCount    uint32         `gorm:"column:node_count;not null;default:0" json:"nodes"`
	LastSyncAt   *time.Time     `json:"lastSyncAt"`
	CreatedBy    *uint64        `json:"-"`
	UpdatedBy    *uint64        `json:"-"`
	CreatedAt    time.Time      `json:"createdAt"`
	UpdatedAt    time.Time      `json:"updatedAt"`
	DeletedAt    gorm.DeletedAt `gorm:"index" json:"-"`
}

// EnvironmentVar 环境级变量（可标记为密文）。
type EnvironmentVar struct {
	ID          uint64    `gorm:"primaryKey;autoIncrement" json:"-"`
	EnvID       uint64    `gorm:"not null;uniqueIndex:uk_env_var,priority:1" json:"-"`
	VarKey      string    `gorm:"size:128;not null;uniqueIndex:uk_env_var,priority:2" json:"key"`
	VarValue    string    `gorm:"size:2000" json:"value"`
	IsSecret    bool      `gorm:"not null;default:false" json:"secret"`
	Description string    `gorm:"size:255" json:"description"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// EnvironmentProject 环境关联的项目。
type EnvironmentProject struct {
	ID        uint64    `gorm:"primaryKey;autoIncrement" json:"-"`
	EnvID     uint64    `gorm:"not null;uniqueIndex:uk_env_project,priority:1" json:"-"`
	ProjectID uint64    `gorm:"not null;uniqueIndex:uk_env_project,priority:2;index:idx_ep_project" json:"-"`
	CreatedAt time.Time `json:"createdAt"`
}

// ServiceInstance 服务实例：环境 × 项目 = 一个可操作的 K8s 工作负载。
//
// 它是 CD 拓扑页上「一个圆圈」的数据来源，也是部署/回滚/扩缩容的作用对象。
type ServiceInstance struct {
	ID            uint64         `gorm:"primaryKey;autoIncrement" json:"-"`
	EnvID         uint64         `gorm:"not null;uniqueIndex:uk_svc_env_project,priority:1" json:"-"`
	ProjectID     uint64         `gorm:"not null;uniqueIndex:uk_svc_env_project,priority:2;index:idx_svc_project" json:"-"`
	EnvKey        string         `gorm:"size:64;index:idx_svc_env_status,priority:1" json:"envKey"`
	ProjectKey    string         `gorm:"size:64;index:idx_svc_project" json:"projectKey"`
	WorkloadKind  string         `gorm:"size:24;not null;default:Deployment" json:"kind"`
	WorkloadName  string         `gorm:"size:191" json:"-"`
	CurrentImgID  *uint64        `gorm:"column:current_image_id" json:"-"`
	CurrentImage  string         `gorm:"size:512" json:"image"`
	CurrentTag    string         `gorm:"size:191" json:"tag"`
	Replicas      int            `gorm:"not null;default:1" json:"replicas"`
	ReadyReplicas int            `gorm:"not null;default:0" json:"readyReplicas"`
	Status        string         `gorm:"size:16;not null;default:pending;index:idx_svc_env_status,priority:2" json:"status"`
	CPURequest    string         `gorm:"size:16" json:"cpuRequest"`
	CPULimit      string         `gorm:"size:16" json:"cpuLimit"`
	MemRequest    string         `gorm:"size:16" json:"memRequest"`
	MemLimit      string         `gorm:"size:16" json:"memLimit"`
	EnvVars       JSONText       `gorm:"type:json" json:"envVars"`
	ConfigMaps    JSONText       `gorm:"type:json" json:"configMaps"`
	Secrets       JSONText       `gorm:"type:json" json:"secrets"`
	Entrypoint    string         `gorm:"size:255" json:"entrypoint"`
	HPAEnabled    bool           `gorm:"not null;default:false" json:"hpaEnabled"`
	LivenessOK    bool           `gorm:"not null;default:true" json:"-"`
	ReadinessOK   bool           `gorm:"not null;default:true" json:"-"`
	AppliedRev    *int           `gorm:"column:applied_revision" json:"-"`
	LastDeployID  *uint64        `json:"-"`
	UpdatedBy     *uint64        `json:"-"`
	CreatedAt     time.Time      `json:"-"`
	UpdatedAt     time.Time      `json:"updatedAt"`
	DeletedAt     gorm.DeletedAt `gorm:"index" json:"-"`
}

// --------------------------------------------------------------------------- 5. 部署

// DeployRecord 部署记录。
// 状态机：pending_approval → queued → progressing → success/failed/canceled；
// 回滚产生的部署单终态为 rolled_back。
type DeployRecord struct {
	ID         uint64 `gorm:"primaryKey;autoIncrement" json:"-"`
	DeployNo   string `gorm:"size:32;uniqueIndex:uk_deploy_no" json:"deployNo"`
	EnvID      uint64 `gorm:"not null;index:idx_deploy_env_time,priority:1" json:"-"`
	EnvKey     string `gorm:"size:64" json:"envKey"`
	ProjectID  uint64 `gorm:"not null;index:idx_deploy_project" json:"-"`
	ProjectKey string `gorm:"size:64" json:"projectKey"`
	ServiceID  uint64 `gorm:"not null;index:idx_deploy_service,priority:1" json:"-"`
	ImageRecID uint64 `gorm:"column:image_record_id;not null" json:"-"`
	Image      string `gorm:"size:512" json:"image"`
	// ImageDigest 这次部署实际用的镜像摘要（sha256:…）。
	//
	// image_record 里那份 digest 是构建成功时回填的；而**从仓库标签部署**时没有记录可查
	// （别人推的 tag 平台从没构建过），就在发单那一步从仓库读回来。
	// 少了它，"当时发的到底是哪一份"事后答不上来 —— 同一个 tag 被重新推过就指向别的内容了。
	//
	// 仓库读不到时留空，**不拿 tag 或时间去凑**。
	ImageDigest  string     `gorm:"column:image_digest;size:80" json:"imageDigest"`
	Replicas     int        `gorm:"not null;default:1" json:"replicas"`
	Strategy     string     `gorm:"size:24;not null;default:RollingUpdate" json:"strategy"`
	EnvOverrides JSONText   `gorm:"type:json" json:"-"`
	Remark       string     `gorm:"size:512" json:"-"`
	Status       string     `gorm:"size:24;not null;default:queued;index:idx_deploy_status,priority:1" json:"status"`
	Progress     uint8      `gorm:"not null;default:0" json:"progress"`
	OperatorID   uint64     `gorm:"not null" json:"-"`
	OperatorName string     `gorm:"size:64" json:"operator"`
	ApproverID   *uint64    `json:"-"`
	ApproverName string     `gorm:"size:64" json:"approver"`
	ApprovedAt   *time.Time `json:"-"`
	RejectReason string     `gorm:"size:512" json:"-"`
	ParentDeploy *uint64    `gorm:"column:parent_deploy_id" json:"-"`
	// RecoveredAt 判失败**之后**集群自己就绪了的时间（只有 ROLLOUT_STUCK 会填）。
	//
	// 为什么要有它：K8s 在 progressDeadlineSeconds 到点就宣布 rollout 卡死，
	// 而"镜像拉得慢"的场景里 Pod 常常在宣布**之后**才拉完并起来 ——
	// 于是出现"集群已经好了、平台记录说失败"（2026-10-19 用户报的）。
	//
	// 刻意**不改 Status**：失败是当时的事实，改了会让审计失真，
	// 也没法回答"这次到底算不算成功"。只把"事后自愈"记下来，页面标「失败后已就绪」。
	RecoveredAt  *time.Time `gorm:"column:recovered_at" json:"recoveredAt"`
	ErrorCode    string     `gorm:"size:64" json:"errorCode"`
	// ErrorMessage 是失败那一刻的**人类可读真因**（如「滚动更新卡死：新 Pod 在进度
	// 截止时间内始终没有就绪（工作负载 default/demobasic，progressDeadlineSeconds=600s）」）。
	// 「部署失败不回滚」之后这条尤其要紧：现场只剩"集群停在新镜像"这一个事实，
	// 解释它的线索以前只在易失的日志流里，平台一重启就没了。
	ErrorMessage string   `gorm:"column:error_message;size:512" json:"errorMessage"`
	AIDiagnosis  JSONText `gorm:"column:ai_diagnosis;type:json" json:"aiDiagnosis,omitempty"`
	// IdempotencyKey 可空唯一列，用指针表达（空串会撞唯一索引，NULL 不会）。
	// 前端每次发起部署请求都带同一个 key，连点两次只会产生一个部署单。
	IdempotencyKey *string    `gorm:"size:128;uniqueIndex:uk_deploy_idem" json:"-"`
	StartedAt      *time.Time `json:"startedAt"`
	FinishedAt     *time.Time `json:"finishedAt"`
	DurationSec    *uint32    `gorm:"column:duration_sec" json:"-"`
	CreatedAt      time.Time  `gorm:"index:idx_deploy_env_time,priority:2;index:idx_deploy_status,priority:2;index:idx_deploy_service,priority:2" json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

// DeployStep 部署步骤明细（拉取镜像 / 创建 Pod / 健康检查 / 就绪确认…）。
type DeployStep struct {
	ID         uint64     `gorm:"primaryKey;autoIncrement" json:"-"`
	DeployID   uint64     `gorm:"not null;uniqueIndex:uk_step_seq,priority:1" json:"-"`
	Seq        uint8      `gorm:"not null;uniqueIndex:uk_step_seq,priority:2" json:"-"`
	Name       string     `gorm:"size:64" json:"name"`
	Status     string     `gorm:"size:16;not null;default:pending" json:"status"`
	Detail     string     `gorm:"size:512" json:"detail"`
	StartedAt  *time.Time `json:"at"`
	FinishedAt *time.Time `json:"-"`
}

// DeployParam 服务参数版本历史。每次变更留一版，支持 diff 与回退。
type DeployParam struct {
	ID         uint64    `gorm:"primaryKey;autoIncrement" json:"-"`
	ServiceID  uint64    `gorm:"not null;uniqueIndex:uk_param_ver,priority:1;index:idx_param_env_proj,priority:1" json:"-"`
	EnvKey     string    `gorm:"size:64;index:idx_param_env_proj,priority:2" json:"envKey"`
	ProjectKey string    `gorm:"size:64" json:"projectKey"`
	Version    uint32    `gorm:"not null;uniqueIndex:uk_param_ver,priority:2" json:"version"`
	Replicas   *int      `json:"replicas"`
	CPURequest string    `gorm:"size:16" json:"cpuRequest"`
	CPULimit   string    `gorm:"size:16" json:"cpuLimit"`
	MemRequest string    `gorm:"size:16" json:"memRequest"`
	MemLimit   string    `gorm:"size:16" json:"memLimit"`
	HPAEnabled *bool     `json:"hpaEnabled"`
	EnvVars    JSONText  `gorm:"type:json" json:"envVars"`
	ConfigMaps JSONText  `gorm:"type:json" json:"configMaps"`
	Secrets    JSONText  `gorm:"type:json" json:"secrets"`
	Diff       JSONText  `gorm:"type:json" json:"diff"`
	ChangedBy  *uint64   `json:"-"`
	ChangedAt  time.Time `json:"changedAt"`
	Note       string    `gorm:"size:255" json:"note"`
}

// RollbackRecord 回滚记录。回滚复用部署状态机，这里只记录回滚语义。
type RollbackRecord struct {
	ID          uint64 `gorm:"primaryKey;autoIncrement" json:"-"`
	DeployID    uint64 `gorm:"not null" json:"-"`
	DeployNo    string `gorm:"size:32" json:"deployNo"`
	EnvID       uint64 `gorm:"not null;index:idx_rbk_env_time,priority:1" json:"-"`
	EnvKey      string `gorm:"size:64" json:"envKey"`
	ProjectID   uint64 `gorm:"not null" json:"-"`
	ProjectKey  string `gorm:"size:64" json:"projectKey"`
	ServiceID   uint64 `gorm:"not null;index:idx_rbk_service,priority:1" json:"-"`
	FromImageID uint64 `gorm:"not null" json:"-"`
	FromImage   string `gorm:"size:512" json:"fromImage"`
	ToImageID   uint64 `gorm:"not null" json:"-"`
	ToImage     string `gorm:"size:512" json:"toImage"`
	Reason      string `gorm:"size:512" json:"reason"`
	// manual / ai_suggested 是**现在可写入**的取值（接口 binding 见 service/cd.go 的 oneof）。
	// `auto` 只剩历史：自动回滚能力 2026-10-08 下线（现状是失败不回滚），但下线前的记录仍带它，
	// 前端为此保留了一个展示分支 —— 改这里的枚举时别把 `auto` 当成"可以删掉的遗留值"。
	TriggerType  string    `gorm:"size:16;not null;default:manual" json:"triggerType"`
	AIConfidence *float64  `gorm:"column:ai_confidence;type:decimal(4,3)" json:"-"`
	OperatorID   uint64    `gorm:"not null" json:"-"`
	OperatorName string    `gorm:"size:64" json:"operator"`
	Status       string    `gorm:"size:16;not null;default:progressing" json:"status"`
	DurationSec  *uint32   `gorm:"column:duration_sec" json:"-"`
	CreatedAt    time.Time `gorm:"index:idx_rbk_env_time,priority:2;index:idx_rbk_service,priority:2" json:"startedAt"`
	UpdatedAt    time.Time `json:"-"`
}
