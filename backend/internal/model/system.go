package model

import (
	"time"
)

// --------------------------------------------------------------------------- 6. 审计 / 通知 / 凭证 / 配置

// AuditLog 操作审计日志。
//
// 注意：DDL 里 audit_log 按 created_at 做了 RANGE 分区（MySQL 特性），
// AutoMigrate 建出来的表**不带分区**。生产环境请用 docs/02 的 DDL 建表，
// 或后续补一个 migration 显式加分区；写入逻辑两者兼容。
type AuditLog struct {
	ID         uint64    `gorm:"primaryKey;autoIncrement" json:"-"`
	UserID     *uint64   `gorm:"index:idx_audit_user_time,priority:1" json:"-"`
	Username   string    `gorm:"size:64" json:"user"`
	UserName   string    `gorm:"size:64" json:"userName"`
	RoleCode   string    `gorm:"size:32" json:"role"`
	Module     string    `gorm:"size:16;index:idx_audit_module_action,priority:1" json:"module"` // CI/CD/系统/认证
	Action     string    `gorm:"size:64;index:idx_audit_module_action,priority:2" json:"action"`
	TargetType string    `gorm:"size:32;index:idx_audit_target,priority:1" json:"targetType"`
	TargetID   *uint64   `gorm:"index:idx_audit_target,priority:2" json:"-"`
	TargetName string    `gorm:"size:191" json:"target"`
	Detail     string    `gorm:"size:2000" json:"detail"`
	Diff       JSONText  `gorm:"type:json" json:"-"`
	Result     string    `gorm:"size:16;not null;default:success" json:"result"`
	ErrorMsg   string    `gorm:"size:512" json:"-"`
	IP         string    `gorm:"size:45" json:"ip"`
	UserAgent  string    `gorm:"size:255" json:"-"`
	TraceID    string    `gorm:"size:64" json:"-"`
	DurationMS *uint32   `gorm:"column:duration_ms" json:"-"`
	CreatedAt  time.Time `gorm:"index:idx_audit_time" json:"at"`
}

// NotifyChannel 通知渠道。
type NotifyChannel struct {
	ID         uint64     `gorm:"primaryKey;autoIncrement" json:"-"`
	Name       string     `gorm:"size:128" json:"name"`
	Type       string     `gorm:"size:16" json:"type"` // email/dingtalk/wecom/feishu/webhook
	Target     string     `gorm:"size:512" json:"target"`
	Events     JSONText   `gorm:"type:json" json:"events"`
	Enabled    bool       `gorm:"not null;default:true;index:idx_notify_enabled" json:"enabled"`
	LastSentAt *time.Time `json:"-"`
	FailCount  uint32     `gorm:"not null;default:0" json:"-"`
	// LastError 最近一次投递失败的原因。
	//
	// 这一列是"配了渠道却没收到消息"唯一的排查线索，所以每条失败路径都必须写它 ——
	// 包括"渠道类型还没接发送器"这种根本没发起投递的情况。
	// 投递成功时会被清空（它表达的是"当前是否正常"，不是"历史上失败过几次"）。
	LastError string    `gorm:"size:512" json:"-"`
	CreatedBy *uint64   `json:"-"`
	CreatedAt time.Time `json:"-"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// SecretCredential 加密凭证库（AES-256-GCM）。
//
// 密钥本体（主密钥）来自环境变量，不入库；库里只存密文、IV 与 AuthTag。
type SecretCredential struct {
	ID          uint64     `gorm:"primaryKey;autoIncrement" json:"-"`
	Name        string     `gorm:"size:128;uniqueIndex:uk_cred_scope_name,priority:2" json:"name"`
	Type        string     `gorm:"size:32" json:"type"` // git_token/harbor_robot/kubeconfig/ssh_key/webhook_secret
	Scope       string     `gorm:"size:128;uniqueIndex:uk_cred_scope_name,priority:1" json:"scope"`
	Cipher      []byte     `gorm:"type:varbinary(8192);not null" json:"-"`
	IV          []byte     `gorm:"type:varbinary(32);not null" json:"-"`
	AuthTag     []byte     `gorm:"column:auth_tag;type:varbinary(32);not null" json:"-"`
	KeyVersion  int        `gorm:"not null;default:1" json:"-"`
	ExpiresAt   *time.Time `gorm:"index:idx_cred_expires" json:"expiresAt"`
	LastUsedAt  *time.Time `json:"-"`
	Description string     `gorm:"size:255" json:"description"`
	CreatedBy   *uint64    `json:"-"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"-"`
}

// SystemConfig 系统配置（键值对，带类型）。
type SystemConfig struct {
	ID          uint64    `gorm:"primaryKey;autoIncrement" json:"-"`
	ConfigKey   string    `gorm:"size:128;uniqueIndex:uk_config_key" json:"configKey"`
	ConfigValue string    `gorm:"type:text" json:"configValue"`
	ValueType   string    `gorm:"size:16;not null;default:string" json:"valueType"` // string/number/bool/json
	GroupName   string    `gorm:"size:64;not null;default:common" json:"groupName"`
	Description string    `gorm:"size:255" json:"description"`
	UpdatedBy   *uint64   `json:"-"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// --------------------------------------------------------------------------- 7. AI 模型通道（新增表）

// AIModelConfig 大模型接入通道，对应前端「系统管理 › 模型设置」页。
//
// 这是相对 docs/02 的**新增表**：模型设置是前端原型后期补充的需求，
// 原 DDL 未覆盖，docs/02 需要同步补一节。
//
// 密钥策略：APIKey 明文只在写入时出现一次，落库用 AES-GCM 加密存 APIKeyCipher；
// 对外查询一律返回掩码 APIKeyMasked，明文永不回传。
type AIModelConfig struct {
	ID             uint64     `gorm:"primaryKey;autoIncrement" json:"-"`
	Name           string     `gorm:"size:64;not null" json:"name"`
	Vendor         string     `gorm:"size:32;not null" json:"vendor"` // dashscope/qwen/deepseek/openai/custom
	BaseURL        string     `gorm:"column:base_url;size:255;not null" json:"baseUrl"`
	APIKeyMasked   string     `gorm:"column:api_key_masked;size:128" json:"apiKeyMasked"`
	APIKeyCipher   []byte     `gorm:"column:api_key_cipher;type:varbinary(4096)" json:"-"`
	APIKeyIV       []byte     `gorm:"column:api_key_iv;type:varbinary(32)" json:"-"`
	APIKeyAuthTag  []byte     `gorm:"column:api_key_auth_tag;type:varbinary(32)" json:"-"`
	Models         JSONText   `gorm:"type:json" json:"models"`
	DefaultModel   string     `gorm:"column:default_model;size:96" json:"defaultModel"`
	Enabled        bool       `gorm:"not null;default:true" json:"enabled"`
	IsDefault      bool       `gorm:"column:is_default;not null;default:false" json:"isDefault"`
	TimeoutSec     int        `gorm:"column:timeout_sec;not null;default:60" json:"timeout"`
	MaxConcurrency int        `gorm:"column:max_concurrency;not null;default:3" json:"maxConcurrency"`
	Temperature    float64    `gorm:"type:decimal(3,2);not null;default:0.70" json:"temperature"`
	MaxTokens      int        `gorm:"column:max_tokens;not null;default:2048" json:"maxTokens"`
	Status         string     `gorm:"size:16;not null;default:unchecked" json:"status"` // connected/failed/unchecked/testing
	LatencyMS      *int       `gorm:"column:latency_ms" json:"latency"`
	CheckedAt      *time.Time `gorm:"column:checked_at" json:"checkedAt"`
	CallCount      uint32     `gorm:"column:call_count;not null;default:0" json:"callCount"`
	FailCount      uint32     `gorm:"column:fail_count;not null;default:0" json:"failCount"`
	Description    string     `gorm:"size:512" json:"desc"`
	CreatedAt      time.Time  `json:"-"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

// TableName 显式指定表名。
// GORM 的 snake_case 转换对连续大写（AI…）结果虽然正确，但显式声明更稳妥、可读。
func (AIModelConfig) TableName() string { return "ai_model_config" }
