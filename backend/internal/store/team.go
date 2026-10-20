package store

import (
	"context"
	"fmt"
	"sort"

	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/model"
)

// SyncPermissionCatalog 把 permSeed 里**新增**的权限点补进已有库。
//
// 为什么需要它：`Seed` 以"user 表为空"为判据，已有数据的库整段跳过 ——
// 于是后加的权限点（比如 `team:read` / `team:manage`）在跑了一段时间的库里**根本不存在**，
// 表现为「角色权限页勾不到它」+「接口一律 403」，而代码看起来完全正常。
//
// 两条边界（与项目既有纪律一致）：
//  ① 只补**本次新建**的 code。已存在的一律不动 —— 管理员可能改过名字或模块。
//  ② 也只在**本次新建**时按 roleSeed 授予内置角色。否则管理员特意去掉的授权会被还原，
//     那种"改完过两天又自己回来了"比不生效更难查。
func SyncPermissionCatalog(ctx context.Context, db *gorm.DB) error {
	var existing []model.Permission
	if err := db.WithContext(ctx).Find(&existing).Error; err != nil {
		return fmt.Errorf("查询已有权限点失败: %w", err)
	}
	have := make(map[string]struct{}, len(existing))
	for _, p := range existing {
		have[p.Code] = struct{}{}
	}

	var added []model.Permission
	for _, p := range permSeed {
		if _, ok := have[p.Code]; ok {
			continue
		}
		added = append(added, model.Permission{Code: p.Code, Name: p.Name, Module: p.Module})
	}
	if len(added) == 0 {
		return nil
	}

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&added).Error; err != nil {
			return fmt.Errorf("补齐权限点失败: %w", err)
		}
		// 只给**内置角色**补，而且只针对这次新增的 code。
		for _, rs := range roleSeed {
			var role model.Role
			if err := tx.Where("code = ?", rs.Code).First(&role).Error; err != nil {
				continue // 角色不存在（被删过）就跳过，不硬造
			}
			var want []string
			for _, code := range rs.Perms {
				if _, isNew := findAdded(added, code); isNew {
					want = append(want, code)
				}
			}
			if len(want) == 0 {
				continue
			}
			for _, code := range want {
				var pc model.Permission
				if err := tx.Where("code = ?", code).First(&pc).Error; err != nil {
					continue
				}
				var n int64
				if err := tx.Model(&model.RolePermission{}).
					Where("role_id = ? AND permission_id = ?", role.ID, pc.ID).Count(&n).Error; err != nil {
					return err
				}
				if n > 0 {
					continue
				}
				if err := tx.Create(&model.RolePermission{RoleID: role.ID, PermissionID: pc.ID}).Error; err != nil {
					return fmt.Errorf("给角色 %s 授予 %s 失败: %w", role.Code, code, err)
				}
			}
		}
		return nil
	})
}

// findAdded 返回该 code 是否在这次新增里。
func findAdded(added []model.Permission, code string) (string, bool) {
	for _, p := range added {
		if p.Code == code {
			return code, true
		}
	}
	return "", false
}

// EnsureTeams 确保 team 表里有**当前项目实际用到**的团队。
//
// 为什么不写死一份名单：现网 `project.team` 里就是权威值（26 个项目挂"基础架构组"、
// 13 个挂"前端体验组"…）。从存量数据反推，新库和已有库走同一套逻辑，
// 也不会出现"界面里有 6 个团队、项目的 team 却是第 7 个"这种悬空。
//
// 幂等：已存在的名字跳过，不覆盖描述与排序。
func EnsureTeams(ctx context.Context, db *gorm.DB) error {
	var names []string
	if err := db.WithContext(ctx).Model(&model.Project{}).
		Where("deleted_at IS NULL AND team <> ?", "").
		Distinct().Pluck("team", &names).Error; err != nil {
		return fmt.Errorf("读取项目团队失败: %w", err)
	}
	sort.Strings(names)

	var existing []string
	if err := db.WithContext(ctx).Model(&model.Team{}).Pluck("name", &existing).Error; err != nil {
		return fmt.Errorf("读取已有团队失败: %w", err)
	}
	have := make(map[string]struct{}, len(existing))
	for _, n := range existing {
		have[n] = struct{}{}
	}

	for i, n := range names {
		if _, ok := have[n]; ok {
			continue
		}
		t := model.Team{Name: n, Sort: (i + 1) * 10, Enabled: true}
		if err := db.WithContext(ctx).Create(&t).Error; err != nil {
			// 唯一索引兜底：并发启动时另一个进程刚插进去，跳过即可。
			continue
		}
	}
	return nil
}
