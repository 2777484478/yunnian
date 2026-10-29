package model

import (
	"time"

	"gorm.io/gorm"
)

// ScheduleTask 定时任务：到点替用户"点一下按钮"。
//
// ⚠️ 它**不执行构建 / 部署本身**，只负责在时间点创建一条普通的构建单或部署单。
// 后面的排队、并发控制、失败通知、超时判定全都复用现有链路 ——
// 定时任务只是"另一个触发来源"，不是第二套执行器。
//
// 这条边界很重要：如果定时器自己去跑构建，它就成了第二套 CI，
// 日志、状态机、中断补偿、通知全要再实现一遍，且两条路的行为迟早分叉。
type ScheduleTask struct {
	ID   uint64 `gorm:"primaryKey;autoIncrement" json:"-"`
	Name string `gorm:"size:64;not null" json:"name"`

	// TaskType build / deploy。比"一个任务里既构建又部署"更好：
	// 两者失败面完全不同（构建失败是代码问题，部署失败是集群问题），
	// 混在一个任务里，用户看不出是哪一步卡住。
	TaskType string `gorm:"column:task_type;size:16;not null;index:idx_schedule_type" json:"taskType"`

	ProjectKey string `gorm:"column:project_key;size:64;not null;index:idx_schedule_project" json:"projectKey"`
	// EnvKey 只有 deploy 任务用。build 任务留空。
	EnvKey string `gorm:"column:env_key;size:64" json:"envKey"`

	// Cron 标准 5 字段表达式（分 时 日 月 周），例如 `0 2 * * *`。
	//
	// 存表达式而不是"每天几点"这类结构化字段：表达式能表达的更多，
	// 而结构化字段一加就收不住（每 N 小时 / 工作日 / 每月 1 号 …）。
	// 界面上给常用预设 + "下次几次执行时间"预览，用户不必会写 cron。
	Cron    string `gorm:"size:64;not null" json:"cron"`
	Enabled bool   `gorm:"not null;default:true;index:idx_schedule_enabled" json:"enabled"`

	// ---- build 专用 ----
	// Branch 空 = 用项目默认分支。
	Branch string `gorm:"size:96" json:"branch"`
	// BuildParams 构建参数（Jenkins 参数化构建等）。原样透传给 TriggerBuild。
	BuildParams JSONText `gorm:"column:build_params;type:json" json:"buildParams,omitempty"`

	// ---- deploy 专用 ----
	// ImageSource 到点发哪个版本，**在创建任务时就定下来**：
	//
	//	fixed   发 ImageTag / ImageRecordID 指定的那个版本（每次都是同一版）
	//	latest  每次执行时取该项目**此刻最新**的成功构建（"每天把最新版铺上去"）
	//
	// 为什么不支持"前一环境当前版本"：那要求环境间有链路关系且上一级版本可解析，
	// 配错了会静默发错版本。真有晋级需求时再单独做，不在这个任务里猜。
	ImageSource string `gorm:"column:image_source;size:16" json:"imageSource"`
	// ImageTag 平台构建记录里的 tag 或仓库标签（ImageSource=fixed 时必填）。
	ImageTag string `gorm:"column:image_tag;size:191" json:"imageTag"`
	// ImageRecordID fixed 且来源是平台构建时的记录 id（字符串，与 DTO 口径一致）。
	ImageRecordID string `gorm:"column:image_record_id;size:32" json:"imageRecordId"`
	// Replicas 空 = 保持服务当前副本数（不传，走平台默认口径）。
	Replicas *int `gorm:"column:replicas" json:"replicas"`

	Remark string `gorm:"size:255" json:"remark"`

	// ---- 运行时状态（由调度器维护，界面只读）----
	// NextRunAt 下次该执行的时间。停用或表达式非法时为 NULL ——
	// 用指针而不是零值，是为了让"还没算过"和"算出来是某个时刻"可区分。
	NextRunAt *time.Time `gorm:"column:next_run_at;index:idx_schedule_next" json:"nextRunAt"`
	LastRunAt *time.Time `gorm:"column:last_run_at" json:"lastRunAt"`
	// LastStatus success / failed / skipped / missed / running
	LastStatus string `gorm:"column:last_status;size:16" json:"lastStatus"`
	LastError  string `gorm:"column:last_error;size:512" json:"lastError"`
	// MissedCount 累计错过次数（服务停机期间到点但没跑）。
	//
	// 为什么单独计数而不是只写日志：用户需要知道"这个任务到底有没有在正常工作"。
	// 一个每周跑一次的任务，停了三个月没发现，界面上一句"错过 12 次"比什么都直接。
	MissedCount int `gorm:"column:missed_count;not null;default:0" json:"missedCount"`
	// LastBuildID / LastDeployID 上次建的记录，点进去能直接跳到那次日志。
	LastBuildID  uint64 `gorm:"column:last_build_id" json:"lastBuildId"`
	LastDeployID uint64 `gorm:"column:last_deploy_id" json:"lastDeployId"`

	CreatedBy *uint64 `gorm:"column:created_by" json:"-"`
	UpdatedBy *uint64 `gorm:"column:updated_by" json:"-"`
	CreatedAt time.Time
	UpdatedAt time.Time
	// 与项目其它表统一用 gorm.DeletedAt：列表查询自动过滤软删，
	// 用 *time.Time 就得每处手写 deleted_at IS NULL，漏一处就多显示一条。
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}
