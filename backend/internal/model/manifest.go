package model

import (
	"time"

	"gorm.io/gorm"
)

// ProjectManifest 一个项目在**某个环境**里的一份 k8s 资源定义。
//
// 为什么需要它：现在发布前必须手工把整个项目的资源（Deployment / Service …）
// `kubectl apply` 到集群，有了资源 CD 才能发。平台对这些资源一无所知 ——
// 不知道它长什么样、不知道某个环境到底有没有它。这份表把它们管起来。
//
// # 为什么一条记录只放一个资源
//
// 而不是"一份多文档 YAML（`---` 分隔）"：
//   - 查看时能按资源分类（一个项目往往 5~10 份），不用在一大坨文本里找；
//   - 后期 apply 时能逐条报告「哪个成功、哪个失败」，而不是整块失败看不出是哪段；
//   - 编辑粒度小，改一个 Service 不会碰到 Deployment。
//
// # 为什么按环境拆（而不是项目级一份）
//
// 同一个项目在不同环境的副本数、资源限制、环境变量常常不同。
// 项目级共用一份就得靠占位符表达差异，而占位符写错会**静默**发出错误配置。
//
// # Kind / Name 从 YAML 里解析，不由用户填
//
// 让用户选「这是 Deployment」等于给了他一次标错的机会（标了 Service、存的是
// Deployment），而且要到后期 apply 才炸。这里直接解析内容得出 Kind/Name，
// 单一事实来源，`kind` 不可能与内容不符。
//
// # 唯一键 (project_id, env_key, kind, name) 为什么不带 deleted_at
//
// 与 `uk_project_key` 同一口径：软删后重建同名资源时**复用同一行**并覆盖内容。
// 若把 deleted_at 放进唯一索引，MySQL 里多个 NULL 互不相等，约束等于失效。
type ProjectManifest struct {
	ID        uint64 `gorm:"primaryKey;autoIncrement" json:"-"`
	ProjectID uint64 `gorm:"not null;uniqueIndex:uk_manifest_scope,priority:1;index:idx_manifest_project" json:"-"`
	// EnvKey 归属环境。空串表示"项目级通用"，预留给以后可能的共用模板 ——
	// 当前版本一律要求有值（后端校验），不允许空。
	EnvKey string `gorm:"size:64;not null;uniqueIndex:uk_manifest_scope,priority:2;index:idx_manifest_env" json:"envKey"`
	// Kind 资源类型，从 YAML 的 kind 解析而来（Deployment / Service / ConfigMap …）。
	Kind string `gorm:"size:48;not null;uniqueIndex:uk_manifest_scope,priority:3" json:"kind"`
	// Name 资源名，从 YAML 的 metadata.name 解析而来。
	Name string `gorm:"size:191;not null;uniqueIndex:uk_manifest_scope,priority:4" json:"name"`
	// Content YAML 原文。**原样保存**，不做格式化、不做变量替换 ——
	// 用户看到的就是他写的，后期 apply 时改镜像也走结构化改字段而不是文本替换。
	Content string `gorm:"type:mediumtext;not null" json:"content"`
	// Note 备注，可空。用来记"这份为什么这么写"（比如为什么要 2Gi 内存）。
	Note string `gorm:"size:255" json:"note"`
	// Sort 列表排序，小的在前。同 kind 内按它排，避免每次进来顺序都变。
	Sort int `gorm:"not null;default:0" json:"sort"`

	CreatedBy *uint64        `json:"-"`
	UpdatedBy *uint64        `json:"-"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// TableName 固定表名。
//
// 虽然 SingularTable=true 也会得到 project_manifest，但这里显式写死：
// docs/02 的 DDL 与手写 SQL 都按这个名字来，多一层显式声明少一处对不上的可能。
func (ProjectManifest) TableName() string { return "project_manifest" }
