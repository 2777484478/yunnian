// Package model 定义平台的持久化模型。
//
// 表结构与 docs/02-数据库设计.md 一一对应（22 张表），另外新增 ai_model_config
// —— 对应前端「系统管理 › 模型设置」页，属后期新增需求，DDL 文档需同步补一节。
//
// 命名策略：GORM 使用 SingularTable，User 映射到 user 表、BuildRecord 映射到
// build_record 表，与 DDL 完全一致。
//
// 关于关联表：user_role / role_permission 采用**显式模型 + 手写查询**，不用 GORM 的
// many2many。原因是 many2many 的写入语义是"先清空再插入"，会丢掉 created_at 审计信息；
// 而权限变更属于必须留痕的操作。
package model

import (
	"database/sql/driver"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// JSONText 把任意 JSON 存进 TEXT/JSON 列，原样存、原样取。
//
// 不引 gorm.io/datatypes：那个包会带进一批额外的类型与间接依赖，
// 而平台只需要「原样存原样取」这一种语义。
type JSONText []byte

// Value 实现 driver.Valuer。
func (j JSONText) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	return string(j), nil
}

// Scan 实现 sql.Scanner。
func (j *JSONText) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*j = nil
	case []byte:
		*j = append((*j)[:0], v...)
	case string:
		*j = []byte(v)
	default:
		return fmt.Errorf("JSONText 不支持从 %T 扫描", src)
	}
	return nil
}

// MarshalJSON 原样输出，避免 JSON 被二次转义成字符串字面量。
func (j JSONText) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	return j, nil
}

// UnmarshalJSON 原样保存。
func (j *JSONText) UnmarshalJSON(b []byte) error {
	*j = append((*j)[:0], b...)
	return nil
}

// Empty 判断是否没有内容。
func (j JSONText) Empty() bool { return len(j) == 0 }

// --------------------------------------------------------------------------- 1. 用户 / 角色 / 权限

// User 平台用户。
type User struct {
	ID           uint64 `gorm:"primaryKey;autoIncrement" json:"-"`
	Username     string `gorm:"size:64;uniqueIndex:uk_user_username" json:"username"`
	PasswordHash string `gorm:"size:128" json:"-"`
	Name         string `gorm:"size:64" json:"name"`
	Email        string `gorm:"size:128;uniqueIndex:uk_user_email" json:"email"`
	Phone        string `gorm:"size:20" json:"phone"`
	Avatar       string `gorm:"size:255" json:"avatar"`
	// SSOSubject 用指针而不是 string：该列有唯一索引，而未接入 SSO 时它是"没有值"。
	// Go 的零值是空串，多个空串在 MySQL 唯一索引里会互相冲突（NULL 才不会），
	// 所以"可空唯一列"必须用指针表达。
	SSOSubject  *string    `gorm:"size:128;uniqueIndex:uk_user_sso" json:"-"`
	Enabled     bool       `gorm:"not null;default:true" json:"enabled"`
	LastLoginAt *time.Time `json:"lastLoginAt"`
	LastLoginIP string     `gorm:"size:45" json:"lastLoginIp"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// Team 团队主数据。
//
// 为什么需要它：`project.team` 一直是个**自由字符串**（用户的数据权限按它反查），
// 而候选值是前端写死的 6 个中文名 —— 后端没有任何约束，手填一个不存在的团队也能存进去。
//
// ⚠️ 唯一的标识就是 `name` 本身（不是另立 code）：
// 存量数据 `project.team` 存的是中文名，改名时必须**级联更新**所有项目，
// 否则项目会指向一个已经不存在的团队。这条在 `UpdateTeam` 里做（同一事务）。
type Team struct {
	ID          uint64    `gorm:"primaryKey;autoIncrement" json:"-"`
	Name        string    `gorm:"size:64;uniqueIndex:uk_team_name" json:"name"`
	Description string    `gorm:"size:255" json:"description"`
	Sort        int       `gorm:"not null;default:0" json:"sort"`
	Enabled     bool      `gorm:"not null;default:true" json:"enabled"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Role 角色。code 为 admin/ops/dev/qa/readonly。
type Role struct {
	ID          uint64    `gorm:"primaryKey;autoIncrement" json:"-"`
	Code        string    `gorm:"size:32;uniqueIndex:uk_role_code" json:"code"`
	Name        string    `gorm:"size:64" json:"name"`
	Description string    `gorm:"size:255" json:"description"`
	Builtin     bool      `gorm:"not null;default:true" json:"builtin"`
	Sort        int       `gorm:"not null;default:0" json:"sort"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Permission 权限点。code 形如 deploy:test,staging / env:* / project:read。
type Permission struct {
	ID          uint64 `gorm:"primaryKey;autoIncrement" json:"-"`
	Code        string `gorm:"size:96;uniqueIndex:uk_perm_code" json:"code"`
	Name        string `gorm:"size:64" json:"name"`
	Module      string `gorm:"size:32;index:idx_perm_module" json:"module"`
	Description string `gorm:"size:255" json:"description"`
}

// UserRole 用户-角色关联。显式建模以保留 created_at 审计信息。
type UserRole struct {
	ID        uint64    `gorm:"primaryKey;autoIncrement" json:"-"`
	UserID    uint64    `gorm:"not null;uniqueIndex:uk_user_role,priority:1;index:idx_ur_role" json:"userId"`
	RoleID    uint64    `gorm:"not null;uniqueIndex:uk_user_role,priority:2" json:"roleId"`
	CreatedAt time.Time `json:"createdAt"`
}

// RolePermission 角色-权限关联。
type RolePermission struct {
	ID           uint64 `gorm:"primaryKey;autoIncrement" json:"-"`
	RoleID       uint64 `gorm:"not null;uniqueIndex:uk_role_perm,priority:1" json:"roleId"`
	PermissionID uint64 `gorm:"not null;uniqueIndex:uk_role_perm,priority:2" json:"permissionId"`
}

// --------------------------------------------------------------------------- 2. 项目

// Project CI 项目（一个仓库 = 一个可构建单元）。
type Project struct {
	ID          uint64 `gorm:"primaryKey;autoIncrement" json:"-"`
	ProjectKey  string `gorm:"column:project_key;size:64;uniqueIndex:uk_project_key" json:"key"`
	Name        string `gorm:"size:128;index:idx_project_name" json:"name"`
	Type        string `gorm:"size:16;index:idx_project_type_enabled,priority:1" json:"type"` // frontend/backend
	GitProvider string `gorm:"size:16;not null;default:gitlab" json:"gitProvider"`
	GitURL      string `gorm:"size:255" json:"gitUrl"`

	// GitUsername 拉代码用的账号，**明文存**：它不是秘密 —— 它会出现在 git remote
	// 地址里、CI 日志里、GitLab 的提交记录里，把它加密只会让"这个项目用哪个账号拉的"
	// 变成一个查不到的问题。真正的秘密是下面的 GitCredential 指向的密码。
	//
	// 空 = 匿名检出（公开仓库是合法的，不能当错误）。
	GitUsername string `gorm:"column:git_username;size:128" json:"gitUsername"`
	// GitCredential 密码/令牌，指向 secret_credential（AES-256-GCM 密文）。
	//
	// 按项目存而不是只有一份"平台级 GitLab 通道"：不同仓库可能在不同的 GitLab 上、
	// 用不同的机器人账号。平台级一份配置会把它们绑死在同一套凭据上 ——
	// 而那个通道一旦关掉，所有项目都会静默退化成匿名检出。
	GitCredential  *uint64 `gorm:"column:git_credential_id" json:"-"`
	DefaultBranch  string  `gorm:"size:96;not null;default:master" json:"defaultBranch"`
	BuildMethod    string  `gorm:"size:16;not null;default:dockerfile" json:"buildMethod"`
	JenkinsJob     string  `gorm:"size:128" json:"jenkinsJob"`
	DockerfilePath string  `gorm:"size:255;not null;default:Dockerfile" json:"dockerfilePath"`
	BuildContext   string  `gorm:"size:255;not null;default:." json:"buildContext"`

	// ---------------------------------------------------------- 自建构建（buildMethod=script）
	//
	// 两种自建构建方式的区别只在于**谁来描述打包过程**：
	//
	//   dockerfile（自带 Dockerfile）：用户提供 Dockerfile，平台做
	//     `docker build -f <DockerfilePath> -t <Image> <BuildContext>`。
	//     编译环境（`FROM maven:3.9-eclipse-temurin-21 AS builder`）写在文件里。
	//
	//   script（自定义脚本）：构建流程**整个由这段脚本定义** —— 拉代码、编译、打包、
	//     推送都写在里面。平台给一份默认脚本（按项目类型挑），用户可随意改。
	//
	// 为什么 script 用"一列脚本"而不是一组描述性字段：字段与脚本是同一件事的两个
	// 真相来源，改了一处忘了另一处就是一次查不明白的构建失败。曾短暂存在过一组
	// 「模板构建」字段（build_image / build_artifact / runtime_image / run_command …
	// 由平台替用户拼多阶段 Dockerfile），已随本模式改为"给默认脚本、用户自己改"而退役：
	// 默认脚本本身就是那个模板，而且它可读、可改、可版本化，比藏在表单后面的隐式生成更好。
	//
	// 平台只负责三件事：把仓库凭据配好、注入 YUNNIAN_* 环境变量、把输出收进构建日志。
	BuildScript string `gorm:"column:build_script;type:text" json:"buildScript"`

	Registry string `gorm:"size:128" json:"registry"`
	// RegistryCredID 推送凭据，指向 secret_credential（密文，与 kubeconfig 同一套）。
	// 空 = 用构建机上 `docker login` 已有的登录态（本机实测：~/.docker/config.json 里
	// 已登录 es-repo.example.com:5000，走的就是这条路）。
	// 之所以留这个可空列而不是强制填：本机开发场景下多一份凭据落库是净增暴露面。
	RegistryCredID *uint64        `gorm:"column:registry_credential_id" json:"-"`
	ImageName      string         `gorm:"size:128" json:"imageName"`
	TagPattern     string         `gorm:"size:128;not null;default:'{branch}-{commitId}-{timestamp}'" json:"tagPattern"`
	BuildParams    JSONText       `gorm:"type:json" json:"buildParams"`
	TimeoutSec     int            `gorm:"not null;default:1800" json:"timeoutSec"`
	OwnerID        uint64         `gorm:"index:idx_project_owner" json:"-"`
	Team           string         `gorm:"size:64" json:"team"`
	Description    string         `gorm:"size:512" json:"description"`
	Enabled        bool           `gorm:"not null;default:true;index:idx_project_type_enabled,priority:2" json:"enabled"`
	AutoBuild      bool           `gorm:"not null;default:false" json:"autoBuild"`
	AutoBuildRefs  JSONText       `gorm:"column:auto_build_branches;type:json" json:"autoBuildBranches"`
	CreatedBy      *uint64        `json:"-"`
	UpdatedBy      *uint64        `json:"-"`
	CreatedAt      time.Time      `json:"createdAt"`
	UpdatedAt      time.Time      `json:"updatedAt"`
	DeletedAt      gorm.DeletedAt `gorm:"index" json:"-"`
}

// ProjectMember 项目成员授权。
type ProjectMember struct {
	ID        uint64    `gorm:"primaryKey;autoIncrement" json:"-"`
	ProjectID uint64    `gorm:"not null;uniqueIndex:uk_pm,priority:1" json:"projectId"`
	UserID    uint64    `gorm:"not null;uniqueIndex:uk_pm,priority:2;index:idx_pm_user" json:"userId"`
	Role      string    `gorm:"size:16;not null;default:developer" json:"role"`
	CreatedAt time.Time `json:"createdAt"`
}

// --------------------------------------------------------------------------- 3. 构建

// BuildRecord 构建记录。
//
// 状态机：queued → running → success / failed / canceled / interrupted / orphaned（见下）。
// 后两个都表示"没跑完就没了"，但**判据与排查方向相反**，不可混用：
//
//	interrupted 平台自己重启，执行随进程中消失 → 去查平台为什么重启
//	orphaned    构建机心跳超时，执行下落不明   → 去查那台构建机
//
// ⚠️ `orphaned` 曾经是**预留值**（2026-10-10 用户拍板 Runner Agent 暂不做，
// 平台是单体单机形态，不存在"构建机心跳"这回事）。**2026-10-12 已启用**：
// Agent 领单后由 `AgentService.ReconcileLeases` 按租约过期产出，见 model/runner.go 顶部。
// 前端 `BuildStatus` 还没有这个值 —— 那是 #179 遗留的前端缺口，不是"后端没实现"。
//
// 另外两个容易踩的：`pending_approval` 是**合法长期态**不是中断（把它当中断等于替用户撤单）；
// `CrashLoopBackOff` 那种"进程还在、但业务没起来"属于 failed，不属于这两个。
type BuildRecord struct {
	ID             uint64   `gorm:"primaryKey;autoIncrement" json:"-"`
	BuildNo        uint32   `gorm:"not null;uniqueIndex:uk_build_no,priority:2" json:"buildNo"`
	ProjectID      uint64   `gorm:"not null;uniqueIndex:uk_build_no,priority:1;index:idx_build_project_time,priority:1" json:"-"`
	ProjectKey     string   `gorm:"size:64;index:idx_build_commit,priority:1" json:"projectKey"`
	Branch         string   `gorm:"size:96" json:"branch"`
	CommitID       string   `gorm:"size:40;index:idx_build_commit,priority:2" json:"commitId"`
	CommitMsg      string   `gorm:"size:512" json:"commitMsg"`
	TriggerType    string   `gorm:"size:16;not null;default:manual" json:"triggerType"` // manual/webhook/schedule/retry
	TriggerBy      *uint64  `json:"-"`
	TriggerName    string   `gorm:"size:64" json:"trigger"`
	ParamsSnapshot JSONText `gorm:"type:json" json:"-"`

	Status      string   `gorm:"size:16;not null;default:queued;index:idx_build_status_time,priority:1" json:"status"`
	Stage       string   `gorm:"size:32" json:"stage"`
	Progress    uint8    `gorm:"not null;default:0" json:"progress"`
	DurationSec *uint32  `gorm:"column:duration_sec" json:"-"`
	ImageRecordID *uint64 `json:"-"`
	LogSize       uint32  `gorm:"not null;default:0" json:"logSize"`
	LogURL        string  `gorm:"size:255" json:"-"`
	ErrorCode     string  `gorm:"size:64" json:"errorCode"`
	// ErrorMessage 是失败那一刻的**人类可读真因**（如「拉取依赖失败：EACCES …」）。
	// 它以前只活在日志流里，而日志流是进程内 + TTL 的 —— 平台一重启，这条构建就
	// 只剩一个机器码，没人说得清当时为什么失败。error_code 用来筛选/统计，
	// 这一列用来**解释**，两者都不可少。
	ErrorMessage string   `gorm:"column:error_message;size:512" json:"errorMessage"`
	AIDiagnosis  JSONText `gorm:"column:ai_diagnosis;type:json" json:"aiDiagnosis,omitempty"`
	// ---------------------------------------------------------------- Jenkins 构建
	//
	// 这三个字段只为 buildMethod=jenkins 的构建存在。为什么不复用别的列：
	//   - JenkinsJob 无法从别处反推：项目上的 jenkinsJob 之后可能被改绑，
	//     而"这一次构建到底跑的是哪个 Job"必须永远可查——拉日志、查状态都靠它；
	//   - JenkinsBuildNo 是 **Jenkins 侧**的编号（不是平台的 BuildNo）。
	//     触发后 job 要先排队，编号是异步才拿到的，所以必须可空；
	//   - JenkinsQueueID 存队列项 id：构建还没开跑时只有它，
	//     进程重启后要靠它把构建号找回来，丢了就永远停在"排队中"。
	JenkinsJob     string  `gorm:"size:128;index:idx_build_jenkins" json:"jenkinsJob,omitempty"`
	JenkinsBuildNo *uint32 `gorm:"column:jenkins_build_no" json:"jenkinsBuildNo,omitempty"`
	JenkinsQueueID *uint64 `gorm:"column:jenkins_queue_id" json:"-"`
	// IdempotencyKey 用指针：同 SSOSubject，可空唯一列不能用空串表达。
	IdempotencyKey *string    `gorm:"size:128;uniqueIndex:uk_build_idem" json:"-"`
	StartedAt      *time.Time `json:"startedAt"`
	FinishedAt     *time.Time `json:"finishedAt"`
	CreatedAt      time.Time  `gorm:"index:idx_build_status_time,priority:2;index:idx_build_project_time,priority:2" json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

// BuildLog 构建日志分片。seq 与 SSE 的 Last-Event-ID 对齐，用于断线续传。
type BuildLog struct {
	ID       uint64    `gorm:"primaryKey;autoIncrement" json:"-"`
	BuildID  uint64    `gorm:"not null;uniqueIndex:uk_log_seq,priority:1;index:idx_log_build" json:"-"`
	Seq      uint32    `gorm:"not null;uniqueIndex:uk_log_seq,priority:2" json:"seq"`
	Level    string    `gorm:"size:8;not null;default:info" json:"level"` // info/warn/error/cmd
	Content  string    `gorm:"size:2000" json:"line"`
	LoggedAt time.Time `json:"ts"`
}

// ImageRecord 镜像版本记录，对应 Harbor 里的一次 push。
type ImageRecord struct {
	ID          uint64         `gorm:"primaryKey;autoIncrement" json:"-"`
	ProjectID   uint64         `gorm:"not null;index:idx_image_project_time,priority:1" json:"-"`
	ProjectKey  string         `gorm:"size:64;index:idx_image_commit,priority:1" json:"projectKey"`
	Registry    string         `gorm:"size:128;uniqueIndex:uk_image,priority:1" json:"registry"`
	Repo        string         `gorm:"size:255;uniqueIndex:uk_image,priority:2" json:"-"`
	Tag         string         `gorm:"size:191;uniqueIndex:uk_image,priority:3" json:"tag"`
	Image       string         `gorm:"size:512" json:"image"`
	Digest      string         `gorm:"size:80" json:"digest"`
	SizeBytes   *uint64        `gorm:"column:size_bytes" json:"-"`
	Arch        string         `gorm:"size:32" json:"arch"`
	Branch      string         `gorm:"size:96" json:"branch"`
	CommitID    string         `gorm:"size:40;index:idx_image_commit,priority:2" json:"commitId"`
	BuildID     uint64         `gorm:"not null;index:idx_image_build" json:"-"`
	BuildNo     uint32         `gorm:"not null" json:"buildNo"`
	PushedAt    time.Time      `gorm:"index:idx_image_project_time,priority:2" json:"pushedAt"`
	PullCount   uint32         `gorm:"not null;default:0" json:"pullCount"`
	DeployedEnv JSONText       `gorm:"column:deployed_envs;type:json" json:"deployedEnvs"`
	CreatedAt   time.Time      `json:"createdAt"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
}
