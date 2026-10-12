package model

import (
	"time"

	"gorm.io/gorm"
)

// --------------------------------------------------------------------------- K8s 集群（对 docs/02 的补充表）

// K8sCluster 平台纳管的 K8s 集群。
//
// 为什么需要这张表：在此之前平台**没有"集群"这个实体**。
// `environment.cluster` 只是一段自由文本（`EnvironmentInput.Cluster` 的校验是
// `binding:"required,max=128"`，不校验存在性、不校验可达性），而执行侧是
// LocalCluster 模拟器（`Ping` 恒返回 nil、`Nodes` 按集群名哈希编数字），
// 于是「测试连接」永远全绿，填一个拼错的集群名也绿。
//
// 后果还有一条很直观：同一个集群在库里能有两个名字——种子里 canary 存的是
// `k8s-prod-01`、prod 存的是 `k8s-prod-01（双可用区）`，前端只好靠正则裁括号抹平
// （见 frontend `config/topology.ts` 的 clusterKeyOf）。集群一旦实体化，
// 名字由这张表统一定义，环境只能引用它，同一个集群不会再有两个名字。
//
// 与 environment 的关系刻意是**软引用**（按 Key 关联，不是外键）：
//   - 存量环境的 cluster 值不在本表里是常态（历史手输值），软引用让它们照旧可读，
//     页面如实标"未接入"即可，不必为了加外键而回填一堆编出来的集群；
//   - 删除集群时由服务层显式检查引用（usedByEnvs），而不是靠数据库级联——
//     这样拒绝删除时能一并把"是谁在引用"告诉用户。
type K8sCluster struct {
	ID uint64 `gorm:"primaryKey;autoIncrement" json:"-"`

	// Key 集群标识，`environment.cluster` 存的就是它（如 k8s-prod-01）。
	// 唯一索引**不含 deleted_at**，与 project 的 uk_project_key 是同一形态——
	// 于是删掉再想用同一个标识重建会撞唯一键（见 README 已知项）。
	// 集群场景下这反而是想要的：标识是运维共识（脚本、工单里都在用），
	// 静默换一个主体比报错危险。删除接口因此不做物理删，只软删。
	Key string `gorm:"column:cluster_key;size:64;uniqueIndex:uk_cluster_key" json:"key"`

	Name string `gorm:"size:128" json:"name"`
	// APIServer 该 context 指向的 API Server 地址，解析 kubeconfig 得到。
	APIServer string `gorm:"column:api_server;size:255" json:"apiServer"`
	// ContextName 用户选定的 kubeconfig context 名。
	ContextName string `gorm:"column:context_name;size:191" json:"context"`
	// KubeconfigCluster kubeconfig 内部引用的集群名。它与 context 名未必相同
	// （`kubectl config get-contexts` 的第一列与第三列经常不一样），
	// 排障时要对得上，所以单独存。
	KubeconfigCluster string `gorm:"column:kubeconfig_cluster;size:191" json:"kubeconfigCluster"`

	// CredID 指向 secret_credential，存的是**该 context 的最小化 kubeconfig**（密文）。
	// 不是整份原文件：一份 kubeconfig 常含 dev/staging/prod 多个 context，
	// 把整份存进每个集群行，等于让"只该访问测试集群的凭据"同时带着生产集群的证书。
	CredID *uint64 `gorm:"column:kubeconfig_credential_id" json:"-"`

	// ---- 以下是探测结果。**读不到就留空/留 NULL**，不要用推测填。----

	// Status connected/unreachable/unauthorized/unprobed。
	// unprobed 是真实的初始状态：接入时探测失败就不会落库，所以"未探测"
	// 只可能出现在「先落库、后探测」的路径上，页面必须能表达它。
	Status string `gorm:"size:16;not null;default:unprobed" json:"status"`
	// K8sVersion 集群 gitVersion，如 v1.29.6。
	K8sVersion string `gorm:"column:k8s_version;size:32" json:"k8sVersion"`
	// Nodes 节点总数。用指针是因为 NULL 与 0 是两件事：
	// NULL = 没读到（无权列节点 / 集群不可达），0 = 读到且确实是 0 个。
	// 用 0 表示"没读到"会让页面显示"0 个节点"，那是编的。
	Nodes *int `json:"nodes"`
	// NodesReady Ready 节点数，同上。
	NodesReady *int `gorm:"column:nodes_ready" json:"nodesReady"`
	// NamespaceCount 命名空间数量，同上。
	NamespaceCount *int `gorm:"column:namespace_count" json:"namespaceCount"`

	// LastProbeMessage 上次探测的**原始结论**：成功是「集群连通」，
	// 失败是真实错误原文（含 "connection refused" 这类底层信息）。
	// 不加工成"探测失败"四个字——排障时要的就是原文。
	LastProbeMessage string     `gorm:"column:last_probe_message;size:512" json:"lastProbeMessage"`
	LastProbeMS      *int       `gorm:"column:last_probe_ms" json:"lastProbeMs"`
	LastProbedAt     *time.Time `gorm:"column:last_probed_at" json:"lastProbedAt"`

	Remark string `gorm:"size:255" json:"remark"`

	CreatedBy *uint64        `json:"-"`
	UpdatedBy *uint64        `json:"-"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// TableName 显式指定表名。
// 模型名里的 K8s 大小写交替，GORM 的复数化+s蛇形推导易出意外（k8_s_clusters 之类），
// 显式声明避免"迁移建的表和查询用的表不是同一张"。
func (K8sCluster) TableName() string { return "k8s_cluster" }

// --------------------------------------------------------------------------- 状态常量

// 集群连通状态。与前端 types/index.ts 的 ClusterStatus 逐字对应。
const (
	ClusterStatusConnected    = "connected"    // 探测通过
	ClusterStatusUnreachable  = "unreachable"  // 网络不通 / TLS 失败 / 超时
	ClusterStatusUnauthorized = "unauthorized" // 拨通了但凭据无效或被拒（401/403）
	ClusterStatusUnprobed     = "unprobed"     // 尚未探测
)
