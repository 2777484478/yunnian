package service

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/2777484478/yunnian/internal/store"
)

// newTestDB 开一个临时 SQLite 库并建好全部表，供"需要真库但不需要真集群"的用例。
//
// 为什么值得搭这层桩：本轮修的几条都是**失败路径**，而它们的成功分支会真去动东西 ——
// 回滚会往集群写一次 Deployment、重建项目会写库。拿真环境做负向测试，
// 万一守卫没生效，代价是集群里多一个跑错镜像的负载。用桩库把"该拒的必须拒"钉住，
// 既不碰真集群，也能长期复跑（比一次性的 curl 断言可靠）。
//
// 用临时**文件**而不是 `:memory:`：`:memory:` 在 GORM 连接池下每条连接各是一个库，
// 事务里建的表事务外看不见（`database is locked` / `no such table` 交替出现，很难查）。
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{
		// 与生产同一套单数表名策略，否则测试里查的表跟真库对不上，等于白测。
		NamingStrategy: store.NamingStrategy(),
		Logger:         gormlogger.Default.LogMode(gormlogger.Silent),
		// DDL 里本就没有外键（靠应用层保证一致性），迁移时别让 sqlite 卡在约束顺序上。
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	if err := store.AutoMigrate(db); err != nil {
		t.Fatalf("迁移测试库失败: %v", err)
	}
	return db
}
