package model

import (
	"time"

	"gorm.io/gorm"
)

// ScheduleTarget 一条任务里的一个目标。
//
// 为什么要有它：定时构建 / 定时部署在真实使用中**从来不是单个服务** ——
// 用户原话："部署发布的时候怎么可能只有 1 个服务？我肯定是批量的。"
// 一晚要打 8 个包、一次要把 6 个服务铺到预生产，逐个建任务既啰嗦又容易漏。
//
// 于是"任务"和"目标"分开：任务管时间与动作，目标管"对谁做、用哪个分支 / 版本"。
// 执行时**逐条独立建单**（与批量发布同一口径：一次建出全部单交给后端队列），
// 一条失败不影响其余，最终结果按"全部成功 / 部分成功 / 全部失败"三档汇报。
type ScheduleTarget struct {
	ProjectKey string `json:"projectKey"`
	// Branch 平台自建构建用的分支。Jenkins 项目里它是**要传给 Jenkins 的值**，
	// 真正落到哪个参数名由 BranchParam 决定（见下）。
	Branch string `json:"branch,omitempty"`
	// BranchParam Jenkins 项目里承载分支的参数名（如 git_branch）。
	//
	// 为什么不执行时再去问 Jenkins：Job 参数是**创建任务时**就能读到的配置，
	// 到点再去问会多一次外部依赖 —— Jenkins 不通时连"该往哪个参数传值"都定不下来，
	// 而这个决定本该在配置阶段就固定住。
	BranchParam string `json:"branchParam,omitempty"`
	// ImageTag / ImageRecordID 定时部署"指定固定版本"时每个目标各自的版本。
	ImageTag      string `json:"imageTag,omitempty"`
	ImageRecordID string `json:"imageRecordId,omitempty"`
}

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

	// Targets 这条任务要处理的目标列表（项目 + 各自的分支 / 版本），JSON 数组。
	//
	// ⚠️ 下面的 ProjectKey / Branch / ImageTag / ImageRecordID 是它的**镜像**，
	// 只为了让"按项目筛任务"和旧数据读得出来，不作为真相：
	// 写的时候一律从 Targets 派生（取第一个目标），读的时候**先看 Targets**。
	// 两处口径分家会让界面显示 A 项目、实际去建 B 项目的单。
	Targets JSONText `gorm:"column:targets;type:json" json:"targets,omitempty"`

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
	// Branch 第一个目标的构建分支（镜像列，见 Targets）。空 = 用项目默认分支。
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
	// LastBuildID / LastDeployID 上次建的**第一条**记录，点进去能直接跳到那次日志。
	// 多目标时它只是"其中一条"，完整清单在 LastIDs 里。
	LastBuildID  uint64 `gorm:"column:last_build_id" json:"lastBuildId"`
	LastDeployID uint64 `gorm:"column:last_deploy_id" json:"lastDeployId"`

	// LastIDs 上次执行建出的全部记录（`{"k":"build","id":12}` 形态的 JSON 数组）。
	//
	// 为什么不能只留 LastBuildID：多目标任务的"上次还没跑完就跳过"必须**逐条**判断，
	// 只看第一条会出现"第一条早结束了、后面那条还在跑，于是又发一轮"，
	// 而同一个服务的部署单会撞锁、构建会白跑一遍。
	LastIDs JSONText `gorm:"column:last_ids;type:json" json:"lastIds,omitempty"`

	CreatedBy *uint64 `gorm:"column:created_by" json:"-"`
	UpdatedBy *uint64 `gorm:"column:updated_by" json:"-"`
	CreatedAt time.Time
	UpdatedAt time.Time
	// 与项目其它表统一用 gorm.DeletedAt：列表查询自动过滤软删，
	// 用 *time.Time 就得每处手写 deleted_at IS NULL，漏一处就多显示一条。
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}
