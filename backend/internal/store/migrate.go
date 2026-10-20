package store

import (
	"github.com/2777484478/yunnian/internal/model"
	"gorm.io/gorm"
)

// AutoMigrate 按模型定义建表并补齐缺失的列。
//
// 说明：这是开发与演示阶段的便捷方式。生产环境建议用 docs/02 的 DDL 建表
// （那里带上了分区、注释、字符集等 AutoMigrate 表达不了的信息），
// 之后把 config 里的 database.autoMigrate 关掉即可。
func AutoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(Models()...)
}

// Models 返回全部需要迁移的模型，共 27 张表
// （docs/02 的 22 张 + ai_model_config、k8s_cluster、runner，以及 2026-10-15
// 为「AI 会话历史」新增的 ai_conversation、ai_message）。
func Models() []any {
	return []any{
		// 1. 用户 / 角色 / 权限
		&model.User{},
		&model.Role{},
		&model.Team{},
		&model.Permission{},
		&model.UserRole{},
		&model.RolePermission{},

		// 2. 项目
		&model.Project{},
		&model.ProjectMember{},

		// 3. 构建 / 镜像
		&model.BuildRecord{},
		&model.BuildLog{},
		&model.ImageRecord{},

		// 4. 环境 / 服务实例
		&model.Environment{},
		&model.EnvironmentVar{},
		&model.EnvironmentProject{},
		&model.ServiceInstance{},

		// 5. 部署 / 步骤 / 参数 / 回滚
		&model.DeployRecord{},
		&model.DeployStep{},
		&model.DeployParam{},
		&model.RollbackRecord{},

		// 6. 审计 / 通知 / 凭证 / 配置 / AI 通道 / K8s 集群
		&model.AuditLog{},
		&model.NotifyChannel{},
		&model.SecretCredential{},
		&model.SystemConfig{},
		&model.AIModelConfig{},
		&model.K8sCluster{},

		// 7. AI 会话（2026-10-15 新增：历史要留下来，见 model/ai.go 的文件头）
		&model.AIConversation{},
		&model.AIMessage{},
	}
}
