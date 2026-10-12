// Package store 负责数据库结构（迁移）与初始数据（种子）。
//
// 除了 AutoMigrate 与 Seed，这里还提供 GORM 的**命名策略**：
// 表名/列名的约定必须全平台唯一，否则设计文档、迁移产物与手写 SQL 三者会互相打架。
package store

import "gorm.io/gorm/schema"

// NamingStrategy 返回全平台统一的表名/列名策略。
//
// 关键是 SingularTable=true —— docs/02 的表名全部是**单数**：
//
//	user / role / permission / user_role / role_permission / project /
//	build_record / build_log / image_record / environment / service_instance /
//	deploy_record / deploy_step / deploy_param / rollback_record / audit_log …
//
// 而 GORM 默认会把结构体名复数化（User → users、Role → roles、BuildRecord →
// build_records）。这两者一旦不一致就会出现两种后果，且都很隐蔽：
//
//  1. 迁移建出 users/roles，而代码里的原生 SQL 写的是 user_role / role
//     → 运行到那条查询才报 "Table 'yunnian.user_role' doesn't exist"，
//     编译期完全发现不了；
//  2. 文档里贴的 DDL 与线上实际表名对不上，DBA 排查时容易误判。
//
// 所以这里把策略固定下来，并作为**唯一**的 gorm.Config 来源，
// 让迁移、CRUD 与手写 JOIN 共用同一套表名。
//
// 列名保持 GORM 默认的 snake_case（project_key / created_at），与 docs/02 一致，
// 因此只需要覆盖表名这一个维度。
func NamingStrategy() schema.NamingStrategy {
	return schema.NamingStrategy{SingularTable: true}
}
