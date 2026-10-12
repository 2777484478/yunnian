// Package store 负责数据库连接、表结构迁移与种子数据。
package store

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/gorm/schema"

	"github.com/yunnian/yunnian/internal/config"
)

// Open 按配置建立数据库连接。
//
// 支持两种驱动：
//   - mysql  —— 生产环境，连接串由配置或环境变量提供
//   - sqlite —— 本地开发/单机部署，纯 Go 实现（modernc），不依赖 CGO 与数据库服务
//
// 两者共用同一套模型与查询代码，切换只改配置里的 driver。
func Open(cfg *config.Config) (*gorm.DB, error) {
	gormCfg := &gorm.Config{
		// 表名保持单数，与 docs/02 的 DDL 完全一致（user / build_record / …）。
		// 不设这一项 GORM 会建出 users / build_records，与既有 DDL 冲突。
		NamingStrategy: schema.NamingStrategy{SingularTable: true},
		Logger:         newGormLogger(cfg),
		// 关闭隐式事务：单条写入不必包事务，多步写入由 service 层显式 Begin。
		SkipDefaultTransaction: true,
		// 建表时不自动创建外键约束——DDL 里没有外键，靠应用层保证一致性，
		// 避免迁移时因约束顺序问题失败。
		DisableForeignKeyConstraintWhenMigrating: true,
	}

	var dialector gorm.Dialector
	switch cfg.Database.Driver {
	case "mysql":
		dialector = mysql.Open(cfg.Database.DSN)

	case "sqlite":
		if dir := filepath.Dir(cfg.Database.SQLitePath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("创建 SQLite 目录 %s 失败: %w", dir, err)
			}
		}
		// WAL 提升读写并发；busy_timeout 让并发写入排队而不是直接报 "database is locked"
		dsn := fmt.Sprintf("%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)",
			cfg.Database.SQLitePath)
		dialector = sqlite.Open(dsn)

	default:
		return nil, fmt.Errorf("不支持的数据库驱动 %q（可选 mysql / sqlite）", cfg.Database.Driver)
	}

	db, err := gorm.Open(dialector, gormCfg)
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败（driver=%s）: %w", cfg.Database.Driver, err)
	}
	if err := applyPool(db, cfg); err != nil {
		return nil, err
	}
	return db, nil
}

// applyPool 设置连接池参数。
func applyPool(db *gorm.DB, cfg *config.Config) error {
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("获取底层连接池失败: %w", err)
	}

	maxOpen := cfg.Database.MaxOpenConns
	if cfg.Database.Driver == "sqlite" && maxOpen > 4 {
		// SQLite 是单写者模型，连接数放开只会把锁竞争推到数据库侧，
		// 本地开发 4 个连接足够，多余并发由 busy_timeout 兜住。
		maxOpen = 4
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(cfg.Database.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.Database.ConnMaxLifetime.D())

	return nil
}

// newGormLogger 把 GORM 的日志接到 slog 上，避免出现两套日志格式。
func newGormLogger(cfg *config.Config) gormlogger.Interface {
	level := gormlogger.Warn
	if cfg.Log.Level == "debug" {
		level = gormlogger.Info
	}
	return gormlogger.New(&slogWriter{log: slog.Default()}, gormlogger.Config{
		SlowThreshold:             cfg.Database.SlowThreshold.D(),
		LogLevel:                  level,
		IgnoreRecordNotFoundError: true, // 记录不存在是业务常态，不必刷日志
		Colorful:                  false,
	})
}

// slogWriter 适配 gormlogger.Writer 接口。
type slogWriter struct{ log *slog.Logger }

// Printf 实现 gormlogger.Writer。消息里已含 [sql]/[slow] 等标记，这里只做转发。
func (w *slogWriter) Printf(format string, args ...any) {
	w.log.Debug("gorm: " + fmt.Sprintf(format, args...))
}

// Ping 探测数据库连通性，启动时用于快速失败。
func Ping(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Ping()
}
