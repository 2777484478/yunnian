package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
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
	// MemberCount 属于这个团队的用户数。
	//
	// 它决定**谁能看到这个团队的项目**（见 CIService.visibleTeamNames）。
	// 0 个成员 + N 个项目 = 那些项目只有超级管理员看得到，界面上要提示出来。
	MemberCount int64 `json:"memberCount"`
}

// TeamMemberDTO 团队里的一个人。
type TeamMemberDTO struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
	Role     string `json:"role"`
}

// TeamProjectOptionDTO 团队编辑弹窗里"项目归属"的一行。
//
// 它列出**全部**项目（不只是本团队的），每个带上当前归属，
// 让用户能看清"勾上这个会把它从谁那里拿走"。
type TeamProjectOptionDTO struct {
	Key      string `json:"key"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Team     string `json:"team"`
	InTeam   bool   `json:"inTeam"`
	Disabled bool   `json:"disabled"`
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

	// 成员数同样一次分组统计。团队没有成员时这里是 0，
	// 界面上要提示"这个团队没人，它下面的项目只有超级管理员看得到"。
	memberIDs, err := s.teamMemberIDs(ctx)
	if err != nil {
		return nil, err
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
			MemberCount:  int64(len(memberIDs[t.ID])),
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
	// 有成员也要拒绝：user_team 是这条记录的唯一归属，删团队会把关系一起带走，
	// 那些人下次登录就"看不到任何项目"，而界面上找不到原因。
	var m int64
	if err := s.db(ctx).Model(&model.UserTeam{}).Where("team_id = ?", id).Count(&m).Error; err != nil {
		return errs.Internalf(err, "统计团队成员失败")
	}
	if m > 0 {
		return errs.StateConflict(fmt.Sprintf("团队 %s 下还有 %d 名成员，请先把他们移出该团队", t.Name, m))
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
	var m int64
	_ = s.db(ctx).Model(&model.UserTeam{}).Where("team_id = ?", t.ID).Count(&m).Error
	return &TeamDTO{ID: httpx.IDString((t.ID)), Name: t.Name, Description: t.Description, Sort: t.Sort, Enabled: t.Enabled, ProjectCount: n, MemberCount: m}, nil
}

// UpdateTeamInput 只改传了的字段（指针区分"没传"与"传了空"）。
type UpdateTeamInput struct {
	Name        *string `json:"name" binding:"omitempty,max=64"`
	Description *string `json:"description" binding:"omitempty,max=255"`
	Sort        *int    `json:"sort"`
	Enabled     *bool   `json:"enabled"`
}

// --------------------------------------------------------------------------- 团队成员

// teamMemberIDs teamID → 用户 ID 列表。一次分组查询，避免 N+1。
func (s *SystemService) teamMemberIDs(ctx context.Context) (map[uint64][]uint64, error) {
	type row struct {
		TeamID uint64 `gorm:"column:team_id"`
		UserID uint64 `gorm:"column:user_id"`
	}
	var rows []row
	if err := s.db(ctx).Table("user_team").
		Select("team_id, user_id").Order("user_id").Scan(&rows).Error; err != nil {
		return nil, errs.Internalf(err, "统计团队成员失败")
	}
	out := make(map[uint64][]uint64, len(rows))
	for _, r := range rows {
		out[r.TeamID] = append(out[r.TeamID], r.UserID)
	}
	return out, nil
}

// TeamMembers 团队成员列表（含角色，便于看清"这个团队里有没有能审批的人"）。
func (s *SystemService) TeamMembers(ctx context.Context, teamID uint64) ([]TeamMemberDTO, error) {
	var t model.Team
	if err := s.db(ctx).First(&t, teamID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("团队不存在")
		}
		return nil, errs.Internalf(err, "查询团队失败")
	}
	var users []model.User
	if err := s.db(ctx).
		Joins("JOIN user_team ON user_team.user_id = user.id").
		Where("user_team.team_id = ?", teamID).
		Order("user.id").Find(&users).Error; err != nil {
		return nil, errs.Internalf(err, "查询团队成员失败")
	}
	if len(users) == 0 {
		return []TeamMemberDTO{}, nil
	}
	roleByUser, err := s.roleCodesOf(ctx, userIDs(users))
	if err != nil {
		return nil, err
	}
	out := make([]TeamMemberDTO, 0, len(users))
	for i := range users {
		out = append(out, TeamMemberDTO{
			ID:       httpx.IDString(users[i].ID),
			Username: users[i].Username,
			Name:     users[i].Name,
			Role:     roleByUser[users[i].ID],
		})
	}
	return out, nil
}

// SetTeamMembers 覆盖式设置团队成员（先删后插）。
//
// 与 `User.Teams` 写的是**同一张表**，两个入口并不冲突：
// 从用户侧改是"这个人属于哪些团队"，从团队侧改是"这个团队有谁"，
// 最终都是 user_team 的增删。
func (s *SystemService) SetTeamMembers(ctx context.Context, teamID uint64, userIDsRaw []string, c Caller) error {
	var t model.Team
	if err := s.db(ctx).First(&t, teamID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errs.NotFoundf("团队不存在")
		}
		return errs.Internalf(err, "查询团队失败")
	}
	ids, err := parseIDList(userIDsRaw)
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		var n int64
		if err := s.db(ctx).Model(&model.User{}).Where("id IN ?", ids).Count(&n).Error; err != nil {
			return errs.Internalf(err, "校验用户失败")
		}
		if int(n) != len(ids) {
			return errs.Invalid("选中的用户里有已不存在的账号，请刷新后重选")
		}
	}
	if err := s.db(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("team_id = ?", teamID).Delete(&model.UserTeam{}).Error; err != nil {
			return errs.Internalf(err, "清理团队成员失败")
		}
		if len(ids) == 0 {
			return nil
		}
		rows := make([]model.UserTeam, 0, len(ids))
		for _, uid := range ids {
			rows = append(rows, model.UserTeam{UserID: uid, TeamID: teamID})
		}
		if err := tx.Create(&rows).Error; err != nil {
			return errs.Internalf(err, "写入团队成员失败")
		}
		return nil
	}); err != nil {
		return err
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "team:members", TargetType: "team",
		TargetID: &teamID, TargetName: t.Name,
		Detail: fmt.Sprintf("设置团队 %s 的成员：共 %d 人", t.Name, len(ids)),
	})
	return nil
}

// parseIDList 把字符串 ID 列表解析成去重后的 uint64 列表。
func parseIDList(raw []string) ([]uint64, error) {
	out := make([]uint64, 0, len(raw))
	seen := map[uint64]bool{}
	for _, v := range raw {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		id, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, errs.Invalidf("ID %q 格式不对", v)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

// --------------------------------------------------------------------------- 团队的项目

// TeamProjectOptions 团队编辑弹窗的「项目归属」列表：**全部项目** + 各自当前归属。
//
// 列全部而不是只列本团队的：这块的语义是"把项目迁到本团队"，
// 只列本团队的项目就没法迁入了。
func (s *SystemService) TeamProjectOptions(ctx context.Context, teamID uint64) ([]TeamProjectOptionDTO, error) {
	var t model.Team
	if err := s.db(ctx).First(&t, teamID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("团队不存在")
		}
		return nil, errs.Internalf(err, "查询团队失败")
	}
	var projects []model.Project
	if err := s.db(ctx).Order("team, project_key").Find(&projects).Error; err != nil {
		return nil, errs.Internalf(err, "查询项目失败")
	}
	out := make([]TeamProjectOptionDTO, 0, len(projects))
	for i := range projects {
		p := &projects[i]
		out = append(out, TeamProjectOptionDTO{
			Key:    p.ProjectKey,
			Name:   p.Name,
			Type:   p.Type,
			Team:   p.Team,
			InTeam: p.Team == t.Name,
			// 已停用的项目也一并列出来但置灰：迁一个停用项目没有任何意义，
			// 而"列表里突然少了一个项目"会让人以为数据丢了。
			Disabled: !p.Enabled,
		})
	}
	return out, nil
}

// MoveProjectsToTeam 把选中的项目**迁入**本团队（单向）。
//
// 为什么叫"迁入"而不是"勾选绑定"：`project.team` 是单值，
// 一个项目只能属于一个团队。如果做成"在团队 A 勾上、团队 B 也勾上"，
// 两边会各说各话（项目页显示 A、团队页显示 B）。
//
// 所以这里只做一件事：把 `project.team` 改成这个团队的名字。
// 原团队不需要任何清理动作 —— 项目本来就不在它名下了。
func (s *SystemService) MoveProjectsToTeam(ctx context.Context, teamID uint64, keys []string, c Caller) (int, error) {
	var t model.Team
	if err := s.db(ctx).First(&t, teamID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, errs.NotFoundf("团队不存在")
		}
		return 0, errs.Internalf(err, "查询团队失败")
	}
	if !t.Enabled {
		return 0, errs.StateConflict(fmt.Sprintf("团队 %s 已停用，不能把项目迁入", t.Name))
	}
	uniq := make([]string, 0, len(keys))
	seen := map[string]bool{}
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		uniq = append(uniq, k)
	}
	if len(uniq) == 0 {
		return 0, nil
	}
	var exist int64
	if err := s.db(ctx).Model(&model.Project{}).
		Where("project_key IN ?", uniq).Count(&exist).Error; err != nil {
		return 0, errs.Internalf(err, "校验项目失败")
	}
	if int(exist) != len(uniq) {
		return 0, errs.Invalid("选中的项目里有已不存在的，请刷新后重选")
	}
	res := s.db(ctx).Model(&model.Project{}).
		Where("project_key IN ?", uniq).
		Updates(map[string]any{"team": t.Name, "updated_at": now()})
	if res.Error != nil {
		return 0, errs.Internalf(res.Error, "迁移项目失败")
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "team:move-projects", TargetType: "team",
		TargetID: &teamID, TargetName: t.Name,
		Detail: fmt.Sprintf("把 %d 个项目迁入团队 %s：%s", len(uniq), t.Name, strings.Join(uniq, "、")),
	})
	return int(res.RowsAffected), nil
}
