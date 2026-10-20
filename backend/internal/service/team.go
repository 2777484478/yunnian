package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/httpx"
	"github.com/2777484478/yunnian/internal/model"
)

// TeamDTO 团队主数据 + 有多少项目挂在它下面。
type TeamDTO struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Sort         int    `json:"sort"`
	Enabled      bool   `json:"enabled"`
	ProjectCount int64  `json:"projectCount"`
}

// --------------------------------------------------------------------------- 团队

// ListTeams 团队列表（按 sort / id）。
func (s *SystemService) ListTeams(ctx context.Context) ([]TeamDTO, error) {
	var teams []model.Team
	if err := s.db(ctx).Order("sort, id").Find(&teams).Error; err != nil {
		return nil, errs.Internalf(err, "查询团队失败")
	}
	// 一次分组统计，避免 N+1（团队多起来时那是肉眼可见的慢）
	type cnt struct {
		Team  string
		Total int64
	}
	var counts []cnt
	if err := s.db(ctx).Model(&model.Project{}).
		Select("team, COUNT(*) AS total").
		Where("deleted_at IS NULL").
		Group("team").Scan(&counts).Error; err != nil {
		return nil, errs.Internalf(err, "统计项目数失败")
	}
	byName := make(map[string]int64, len(counts))
	for _, c := range counts {
		byName[c.Team] = c.Total
	}

	out := make([]TeamDTO, 0, len(teams))
	for _, t := range teams {
		out = append(out, TeamDTO{
			ID:           httpx.IDString((t.ID)),
			Name:         t.Name,
			Description:  t.Description,
			Sort:         t.Sort,
			Enabled:      t.Enabled,
			ProjectCount: byName[t.Name],
		})
	}
	return out, nil
}

// CreateTeam 新建团队。
func (s *SystemService) CreateTeam(ctx context.Context, name, desc string, sort int) (*TeamDTO, error) {
	n := strings.TrimSpace(name)
	if n == "" {
		return nil, errs.Invalid("团队名称不能为空")
	}
	var exist int64
	if err := s.db(ctx).Model(&model.Team{}).Where("name = ?", n).Count(&exist).Error; err != nil {
		return nil, errs.Internalf(err, "检查同名团队失败")
	}
	if exist > 0 {
		return nil, errs.StateConflict(fmt.Sprintf("团队 %s 已存在", n))
	}
	t := model.Team{Name: n, Description: strings.TrimSpace(desc), Sort: sort, Enabled: true}
	if err := s.db(ctx).Create(&t).Error; err != nil {
		if isDupKeyOn(err, "uk_team_name") || isDupKeyOn(err, "name") {
			return nil, errs.StateConflict(fmt.Sprintf("团队 %s 已存在", n))
		}
		return nil, errs.Internalf(err, "创建团队失败")
	}
	return &TeamDTO{ID: httpx.IDString((t.ID)), Name: t.Name, Description: t.Description, Sort: t.Sort, Enabled: t.Enabled}, nil
}

// UpdateTeam 改团队。**改名字必须级联更新 project.team**。
//
// 为什么不能只改自己这行：`project.team` 存的是**名字本身**（不是 id）。
// 改了名不级联，那些项目就指向一个不存在的团队 —— 列表里那个团队的项目数会变成 0，
// 而项目自己还挂着旧名字，界面上看不出谁错。
func (s *SystemService) UpdateTeam(ctx context.Context, id uint64, in UpdateTeamInput) (*TeamDTO, error) {
	var t model.Team
	if err := s.db(ctx).First(&t, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("团队不存在")
		}
		return nil, errs.Internalf(err, "查询团队失败")
	}
	oldName := t.Name
	updates := map[string]any{}
	newName := oldName
	if in.Name != nil {
		n := strings.TrimSpace(*in.Name)
		if n == "" {
			return nil, errs.Invalid("团队名称不能为空")
		}
		if n != oldName {
			var exist int64
			if err := s.db(ctx).Model(&model.Team{}).Where("name = ? AND id <> ?", n, id).Count(&exist).Error; err != nil {
				return nil, errs.Internalf(err, "检查同名团队失败")
			}
			if exist > 0 {
				return nil, errs.StateConflict(fmt.Sprintf("团队 %s 已存在", n))
			}
			updates["name"] = n
			newName = n
		}
	}
	if in.Description != nil {
		updates["description"] = strings.TrimSpace(*in.Description)
	}
	if in.Sort != nil {
		updates["sort"] = *in.Sort
	}
	if in.Enabled != nil {
		updates["enabled"] = *in.Enabled
	}
	if len(updates) == 0 {
		return &TeamDTO{ID: httpx.IDString((t.ID)), Name: t.Name, Description: t.Description, Sort: t.Sort, Enabled: t.Enabled}, nil
	}
	updates["updated_at"] = now()

	err := s.db(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&model.Team{}).Where("id = ?", id).Updates(updates).Error; err != nil {
			return errs.Internalf(err, "更新团队失败")
		}
		if newName != oldName {
			// 项目那边的 team 是名字，必须跟着改；软删的也一起改，否则复活后又悬空。
			if err := tx.Model(&model.Project{}).Where("team = ?", oldName).
				Updates(map[string]any{"team": newName, "updated_at": now()}).Error; err != nil {
				return errs.Internalf(err, "同步项目的团队名失败")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.teamByID(ctx, id)
}

// DeleteTeam 删团队。**有项目挂着就拒绝**。
//
// 理由：删掉之后那些项目的 `team` 就成了悬空值，而用户数据权限正是按它反查的 ——
// 那些人会突然看不到自己的项目，且界面上没有任何地方能看出原因。
func (s *SystemService) DeleteTeam(ctx context.Context, id uint64) error {
	var t model.Team
	if err := s.db(ctx).First(&t, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errs.NotFoundf("团队不存在")
		}
		return errs.Internalf(err, "查询团队失败")
	}
	var n int64
	if err := s.db(ctx).Model(&model.Project{}).
		Where("deleted_at IS NULL AND team = ?", t.Name).Count(&n).Error; err != nil {
		return errs.Internalf(err, "统计团队下项目失败")
	}
	if n > 0 {
		return errs.StateConflict(fmt.Sprintf("团队 %s 下还有 %d 个项目，请先把它们改到别的团队", t.Name, n))
	}
	if err := s.db(ctx).Delete(&model.Team{}, id).Error; err != nil {
		return errs.Internalf(err, "删除团队失败")
	}
	return nil
}

func (s *SystemService) teamByID(ctx context.Context, id uint64) (*TeamDTO, error) {
	var t model.Team
	if err := s.db(ctx).First(&t, id).Error; err != nil {
		return nil, errs.Internalf(err, "读取团队失败")
	}
	var n int64
	_ = s.db(ctx).Model(&model.Project{}).Where("deleted_at IS NULL AND team = ?", t.Name).Count(&n).Error
	return &TeamDTO{ID: httpx.IDString((t.ID)), Name: t.Name, Description: t.Description, Sort: t.Sort, Enabled: t.Enabled, ProjectCount: n}, nil
}

// UpdateTeamInput 只改传了的字段（指针区分"没传"与"传了空"）。
type UpdateTeamInput struct {
	Name        *string `json:"name" binding:"omitempty,max=64"`
	Description *string `json:"description" binding:"omitempty,max=255"`
	Sort        *int    `json:"sort"`
	Enabled     *bool   `json:"enabled"`
}
