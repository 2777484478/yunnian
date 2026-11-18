package service

import (
	"context"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/model"
)

// 角色管理（2026-11-18）：内置的 5 个角色只能改权限点，不能改名/删；
// 这里补的是"用户自建角色"—— 他想要一个「产品经理」角色，勾自己那部分权限。

// RoleInput 新建 / 修改角色的入参。
//
// 注意**没有 code**：自定义角色的 code 由服务端生成（`custom_N`）。
// 让用户填 code 只会多一个出错的地方，而他真正关心的是中文名。
type RoleInput struct {
	Name  string `json:"name" binding:"required,max=64"`
	Desc  string `json:"desc" binding:"omitempty,max=255"`
	Perms []string `json:"perms"`
}

// CreateRole 新建自定义角色，并立刻写入它勾选的权限点。
//
// 为什么权限点要在这里一起写：分两步（先建角色、再单独调权限接口）会出现
// "角色建好了但没有任何权限"的中间态，用户以为建成功了，用起来什么都点不了。
func (s *SystemService) CreateRole(ctx context.Context, in RoleInput, c Caller) (*SysRoleDTO, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, errs.Invalid("角色名称不能为空")
	}
	var cnt int64
	if err := s.db(ctx).Model(&model.Role{}).Where("name = ?", name).Count(&cnt).Error; err != nil {
		return nil, errs.Internalf(err, "检查同名角色失败")
	}
	if cnt > 0 {
		return nil, errs.StateConflict(fmt.Sprintf("角色「%s」已存在", name))
	}

	code, err := s.nextRoleCode(ctx)
	if err != nil {
		return nil, err
	}
	role := model.Role{
		Code:        code,
		Name:        name,
		Description: strings.TrimSpace(in.Desc),
		// 自建角色一律非内置 —— 内置标记决定"能不能删"，跟来源无关。
		Builtin: false,
		Sort:    50,
	}
	// ⚠️ 必须用 Select 列出字段：`Builtin` 在模型上是 `default:true`，
	// 而 Go 的零值是 false —— GORM 会把零值当成"没设置"，转而用数据库默认值，
	// 于是自建角色被建成 builtin=true（删不掉、界面上还标成内置）。
	// 这类"默认值 + 零值"的坑在布尔字段上只要遇到一次就会静默出错。
	if err := s.db(ctx).Select("code", "name", "description", "builtin", "sort").Create(&role).Error; err != nil {
		if isDupKeyOn(err, "uk_role_code") || isDupKeyOn(err, "code") {
			return nil, errs.StateConflict("角色编码已被占用，请重试")
		}
		return nil, errs.Internalf(err, "创建角色失败")
	}
	// GORM 对 `default:true` 的布尔列会把零值（false）当成"没设置"，
	// 转而用数据库默认值 —— 结果是自建角色被建成 builtin=true（删不掉、
	// 界面上还标成内置）。Select 指定字段也挡不住这一层，必须显式回写一次。
	// 不去改模型的 default：内置角色的种子数据靠它兜底，去掉会让内置角色变成可删。
	if err := s.db(ctx).Model(&model.Role{}).Where("id = ?", role.ID).
		UpdateColumn("builtin", false).Error; err != nil {
		return nil, errs.Internalf(err, "标记自建角色失败")
	}

	if len(in.Perms) > 0 {
		if _, err := s.UpdateRolePermissions(ctx, code,
			UpdateRolePermissionsInput{Perms: in.Perms}, true, c); err != nil {
			// 权限写入失败就把角色删掉：留一个没有任何权限的空角色，
			// 用户会以为"建好了但不好使"，而真正的原因在一条已经被吞掉的错误里。
			if derr := s.db(ctx).Delete(&model.Role{}, role.ID).Error; derr != nil {
				s.log("system").Warn("回滚新建角色失败", "code", code, "err", derr)
			}
			return nil, err
		}
	}

	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "role:create", TargetType: "role",
		TargetID: &role.ID, TargetName: name,
		Detail: fmt.Sprintf("新建角色 %s（%s），权限点 %d 个", name, code, len(in.Perms)),
	})
	return s.roleDTOByCode(ctx, code)
}

// UpdateRole 改角色的名称与描述。**code 与权限点不在这里改**。
//
// 权限点单独走 UpdateRolePermissions，是因为那里有一套动态权限点
// （按环境生成的 env:view:xxx / deploy:xxx）的补齐逻辑，重复一份必然漏改。
func (s *SystemService) UpdateRole(ctx context.Context, code string, in RoleInput, c Caller) (*SysRoleDTO, error) {
	role, err := s.roleByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	updates := map[string]any{}
	if name := strings.TrimSpace(in.Name); name != "" && name != role.Name {
		var cnt int64
		if err := s.db(ctx).Model(&model.Role{}).
			Where("name = ? AND id <> ?", name, role.ID).Count(&cnt).Error; err != nil {
			return nil, errs.Internalf(err, "检查同名角色失败")
		}
		if cnt > 0 {
			return nil, errs.StateConflict(fmt.Sprintf("角色「%s」已存在", name))
		}
		updates["name"] = name
	}
	if in.Desc != "" || role.Description != "" {
		updates["description"] = strings.TrimSpace(in.Desc)
	}
	if len(updates) == 0 {
		return s.roleDTOByCode(ctx, code)
	}
	updates["updated_at"] = now()
	if err := s.db(ctx).Model(&model.Role{}).Where("id = ?", role.ID).Updates(updates).Error; err != nil {
		return nil, errs.Internalf(err, "更新角色失败")
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "role:update", TargetType: "role",
		TargetID: &role.ID, TargetName: role.Name,
		Detail: fmt.Sprintf("修改角色 %s（%s）", role.Code, role.Name),
	})
	return s.roleDTOByCode(ctx, code)
}

// DeleteRole 删自定义角色。
//
// 两条硬约束：
//   - 内置角色不可删（admin/ops/dev/qa/readonly 是平台的骨架，删掉会让
//     已存在的用户失去角色，登录后什么都点不了）；
//   - 还有用户在用的角色不可删 —— 删了那些用户的权限会凭空消失，
//     而界面上只能看到"我没有权限"，看不出原因。
func (s *SystemService) DeleteRole(ctx context.Context, code string, c Caller) error {
	role, err := s.roleByCode(ctx, code)
	if err != nil {
		return err
	}
	if role.Builtin {
		return errs.StateConflict(fmt.Sprintf("「%s」是内置角色，不能删除；如需限制权限，请改它的权限点", role.Name))
	}
	var used int64
	if err := s.db(ctx).Model(&model.UserRole{}).Where("role_id = ?", role.ID).Count(&used).Error; err != nil {
		return errs.Internalf(err, "统计角色用户数失败")
	}
	if used > 0 {
		return errs.StateConflict(fmt.Sprintf(
			"还有 %d 个用户使用角色「%s」，请先把他们改到其它角色", used, role.Name))
	}
	if err := s.db(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("role_id = ?", role.ID).Delete(&model.RolePermission{}).Error; err != nil {
			return errs.Internalf(err, "清理角色权限失败")
		}
		if err := tx.Delete(&model.Role{}, role.ID).Error; err != nil {
			return errs.Internalf(err, "删除角色失败")
		}
		return nil
	}); err != nil {
		return err
	}
	s.audit(ctx, c, auditInput{
		Module: "系统", Action: "role:delete", TargetType: "role",
		TargetID: &role.ID, TargetName: role.Name,
		Detail: fmt.Sprintf("删除角色 %s（%s）", role.Name, role.Code),
	})
	return nil
}

// nextRoleCode 生成 `custom_N` 形式的角色编码。
//
// 不用 UUID：审计日志和通知里会出现这个 code，`custom_7` 比
// `a3f9-...` 可读得多。冲突时顺延，最坏情况也只是多查几次。
func (s *SystemService) nextRoleCode(ctx context.Context) (string, error) {
	for i := 1; i <= 999; i++ {
		code := fmt.Sprintf("custom_%d", i)
		var cnt int64
		if err := s.db(ctx).Model(&model.Role{}).Where("code = ?", code).Count(&cnt).Error; err != nil {
			return "", errs.Internalf(err, "生成角色编码失败")
		}
		if cnt == 0 {
			return code, nil
		}
	}
	return "", errs.StateConflict("自建角色数量已达上限（999），请联系管理员清理不用的角色")
}

// roleDTOByCode 单个角色的完整视图（含权限点与用户数）。
func (s *SystemService) roleDTOByCode(ctx context.Context, code string) (*SysRoleDTO, error) {
	var r model.Role
	if err := s.db(ctx).Where("code = ?", code).First(&r).Error; err != nil {
		return nil, errs.Internalf(err, "读取角色失败")
	}
	perms, err := s.permsOfRoles(ctx, []uint64{r.ID})
	if err != nil {
		return nil, err
	}
	counts, err := s.userCountOfRoles(ctx, []uint64{r.ID})
	if err != nil {
		return nil, err
	}
	return &SysRoleDTO{
		Key:     r.Code,
		Name:    r.Name,
		Desc:    r.Description,
		Users:   counts[r.ID],
		Color:   roleColor(r.Code),
		Perms:   perms[r.ID],
		Builtin: r.Builtin,
	}, nil
}
