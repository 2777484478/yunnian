// 运行参数：可以在「系统管理」里在线调整的排程上限。
//
// 与 config.yaml 的关系遵循本仓库一贯口径：**库优先、逐键回退配置文件**。
//
// 为什么放到库里而不是只留 config.yaml：并发上限是"用着用着才发现要调"的东西 ——
// 2026-10-14 用户的反馈就是「批量发布为什么只排队发一个，我想自己设置」。
// 每次都要改文件 + 重启服务，等于这个旋钮不存在。
package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/config"
	"github.com/2777484478/yunnian/internal/model"
)

// 运行参数在 system_config 里的键名。
const (
	KeyRunnerBuildConcurrency  = "runner.buildConcurrency"
	KeyRunnerDeployConcurrency = "runner.deployConcurrency"
	runnerConfigGroup          = "runner"
)

// IsRuntimeKey 判断一个配置键是不是运行参数（并发上限）。
//
// 用途：`PUT /system/config` 保存后要据此决定"要不要立刻把新值应用到任务池"。
// 判断收敛在这里，避免上层各处写死字符串而漏掉一个。
func IsRuntimeKey(key string) bool {
	switch strings.TrimSpace(key) {
	case KeyRunnerBuildConcurrency, KeyRunnerDeployConcurrency:
		return true
	default:
		return false
	}
}

// RuntimeSettings 当前生效的运行参数。
type RuntimeSettings struct {
	BuildConcurrency  int
	DeployConcurrency int
	// FromDB 为 false 表示库里还没有这些键，用的全是 config.yaml 的值。
	// 界面据此告诉用户"改了 config.yaml 也不会立刻生效"，避免两处口径打架。
	FromDB    bool
	UpdatedAt time.Time
}

// LoadRuntimeSettings 读取当前生效的运行参数。
//
// 逐键回退：库里缺哪个键就用 config.yaml 的那个（而不是整体回退），
// 否则"只在界面上改了部署并发"会把构建并发一并顶回配置文件默认值。
func LoadRuntimeSettings(ctx context.Context, db *gorm.DB, fallback config.RunnerConfig) (*RuntimeSettings, error) {
	out := &RuntimeSettings{
		BuildConcurrency:  fallback.BuildConcurrency,
		DeployConcurrency: fallback.DeployConcurrency,
	}
	var rows []model.SystemConfig
	if err := db.WithContext(ctx).
		Where("config_key IN ?", []string{KeyRunnerBuildConcurrency, KeyRunnerDeployConcurrency}).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("查询运行参数失败: %w", err)
	}
	for _, row := range rows {
		out.FromDB = true
		if row.UpdatedAt.After(out.UpdatedAt) {
			out.UpdatedAt = row.UpdatedAt
		}
		n, err := strconv.Atoi(strings.TrimSpace(row.ConfigValue))
		if err != nil {
			// 坏值不静默变成 0（0 会让任务池彻底不再调度）：保留配置文件里的值，
			// 由调用方在日志里告警。这里不返回错误，是为了"一个键写坏了不至于让服务起不来"。
			continue
		}
		switch row.ConfigKey {
		case KeyRunnerBuildConcurrency:
			out.BuildConcurrency = n
		case KeyRunnerDeployConcurrency:
			out.DeployConcurrency = n
		}
	}
	return out, nil
}

// EnsureRuntimeSettings 把两个运行参数键补进 system_config（已存在就不动）。
//
// 为什么要有这一步：**通用参数编辑器只显示库里已有的键**。
// 不补的话，用户在界面上根本看不到这两个旋钮 —— 那正是本次要解决的问题。
// 只补新增的键，不覆盖用户改过的值（与 SyncPermissionCatalog 同一纪律）。
func EnsureRuntimeSettings(ctx context.Context, db *gorm.DB, fallback config.RunnerConfig) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		seed := []struct {
			key   string
			value int
			desc  string
		}{
			{KeyRunnerBuildConcurrency, fallback.BuildConcurrency,
				"同时执行的构建任务数上限（超出排队的等待）。构建很吃机器，不要盲目调大"},
			{KeyRunnerDeployConcurrency, fallback.DeployConcurrency,
				"同时执行的部署任务数上限（超出排队的等待）。1 = 逐服务串行发布，避免滚动更新互相抢资源与依赖顺序错乱"},
		}
		for _, s := range seed {
			var row model.SystemConfig
			err := tx.Where("config_key = ?", s.key).First(&row).Error
			if err == nil {
				continue // 已存在：用户的调整优先
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err := upsertSystemConfig(tx, s.key, strconv.Itoa(s.value), "number", runnerConfigGroup, s.desc, nil); err != nil {
				return err
			}
		}
		return nil
	})
}

// SaveRuntimeSettings 覆盖式写入两个运行参数（供专用接口用；通用编辑器走 upsertSystemConfig）。
func SaveRuntimeSettings(ctx context.Context, db *gorm.DB, build, deploy int, userID *uint64) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := upsertSystemConfig(tx, KeyRunnerBuildConcurrency, strconv.Itoa(build), "number",
			runnerConfigGroup, "同时执行的构建任务数上限（超出排队的等待）", userID); err != nil {
			return err
		}
		return upsertSystemConfig(tx, KeyRunnerDeployConcurrency, strconv.Itoa(deploy), "number",
			runnerConfigGroup, "同时执行的部署任务数上限（超出排队的等待）", userID)
	})
}
