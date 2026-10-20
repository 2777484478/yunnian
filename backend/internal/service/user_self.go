package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"

	"github.com/2777484478/yunnian/internal/errs"
	"github.com/2777484478/yunnian/internal/model"
)

// --------------------------------------------------------------------------- 个人设置

// UpdateSelfInput 用户**改自己**能动的字段。
//
// ⚠️ 只有这四项。角色、启用状态、密码哈希都不在这里 ——
// 那些是 `user:manage` 的权限，不能因为"这是自己的账号"就把口子放宽
//（否则任何人都能给自己提权）。
type UpdateSelfInput struct {
	Name   *string `json:"name" binding:"omitempty,max=64"`
	Email  *string `json:"email" binding:"omitempty,max=128,email"`
	Phone  *string `json:"phone" binding:"omitempty,max=20"`
	Avatar *string `json:"avatar" binding:"omitempty,max=255"`
}

// UpdateSelf 修改当前登录用户的基本信息。
func (s *AuthService) UpdateSelf(ctx context.Context, in UpdateSelfInput, c Caller) (*model.User, error) {
	uid := c.UserID()
	if uid == nil || *uid == 0 {
		return nil, errs.Unauthorized("未登录")
	}
	var u model.User
	if err := s.db(ctx).First(&u, *uid).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errs.NotFoundf("当前用户不存在")
		}
		return nil, errs.Internalf(err, "查询用户失败")
	}

	updates := map[string]any{}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" {
			return nil, errs.Invalid("姓名不能为空")
		}
		updates["name"] = name
	}
	if in.Email != nil {
		// 邮箱有唯一索引。空串在 MySQL 唯一索引里会互相冲突（NULL 才不会），
		// 而这里"没有邮箱"就是没有值 —— 存 NULL 而不是空串。
		email := strings.TrimSpace(*in.Email)
		if email == "" {
			updates["email"] = nil
		} else {
			var n int64
			if err := s.db(ctx).Model(&model.User{}).
				Where("email = ? AND id <> ?", email, u.ID).Count(&n).Error; err != nil {
				return nil, errs.Internalf(err, "检查邮箱占用失败")
			}
			if n > 0 {
				return nil, errs.StateConflict(fmt.Sprintf("邮箱 %s 已被其他账号使用", email))
			}
			updates["email"] = email
		}
	}
	if in.Phone != nil {
		updates["phone"] = strings.TrimSpace(*in.Phone)
	}
	if in.Avatar != nil {
		updates["avatar"] = strings.TrimSpace(*in.Avatar)
	}
	if len(updates) == 0 {
		return &u, nil
	}
	updates["updated_at"] = now()
	if err := s.db(ctx).Model(&model.User{}).Where("id = ?", u.ID).Updates(updates).Error; err != nil {
		// 唯一索引兜底：并发下上面那次 Count 可能都通过，冲突由这里暴露。
		if isDupKeyOn(err, "email") || isDupKeyOn(err, "uk_user_email") {
			return nil, errs.StateConflict("该邮箱已被其他账号使用")
		}
		return nil, errs.Internalf(err, "更新用户信息失败")
	}
	if err := s.db(ctx).First(&u, u.ID).Error; err != nil {
		return nil, errs.Internalf(err, "读取更新后的用户失败")
	}
	return &u, nil
}

// ChangeSelfPassword 改自己的密码。
//
// 必须验旧密码：会话是可能被别人拿到的（共享机器、忘记退出），
// 只凭"已登录"就允许改密码，等于把一次会话泄露升级成永久接管。
func (s *AuthService) ChangeSelfPassword(ctx context.Context, oldPwd, newPwd string, c Caller) error {
	uid := c.UserID()
	if uid == nil || *uid == 0 {
		return errs.Unauthorized("未登录")
	}
	if len(strings.TrimSpace(newPwd)) < 8 {
		return errs.Invalid("新密码至少 8 位")
	}
	if newPwd == oldPwd {
		return errs.Invalid("新密码不能与旧密码相同")
	}
	var u model.User
	if err := s.db(ctx).First(&u, *uid).Error; err != nil {
		return errs.Internalf(err, "查询用户失败")
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(oldPwd)) != nil {
		return errs.StateConflict("旧密码不正确")
	}
	hash, err := s.HashPassword(newPwd)
	if err != nil {
		return errs.Internalf(err, "生成密码哈希失败")
	}
	if err := s.db(ctx).Model(&model.User{}).Where("id = ?", u.ID).
		Updates(map[string]any{"password_hash": hash, "updated_at": now()}).Error; err != nil {
		return errs.Internalf(err, "更新密码失败")
	}
	return nil
}

// AvatarDir 头像落盘目录。
//
// 用相对运行目录的 `data/uploads/avatars`：后端的工作目录固定是 `backend/`，
// 所以实际位置是 `backend/data/uploads/avatars`。不放前端产物目录（那里每次构建被清空）。
func AvatarDir() string { return filepath.Join("data", "uploads", "avatars") }

// SaveSelfAvatar 保存当前用户的头像文件，返回可访问的相对 URL。
//
// 约束：只收 png/jpeg/webp，单文件 ≤ 1MB。理由不是"省空间"，而是头像这个位置
// **没有任何理由**接受大文件 —— 允许 10MB 的结果是列表页每次加载都拖几十 MB。
func (s *AuthService) SaveSelfAvatar(ctx context.Context, fileName string, data []byte, c Caller) (string, error) {
	uid := c.UserID()
	if uid == nil || *uid == 0 {
		return "", errs.Unauthorized("未登录")
	}
	if len(data) == 0 {
		return "", errs.Invalid("文件为空")
	}
	if len(data) > 1<<20 {
		return "", errs.Invalid("头像不能超过 1MB")
	}
	ext := strings.ToLower(filepath.Ext(fileName))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp":
	default:
		return "", errs.Invalid("只支持 png / jpg / webp 格式的头像")
	}
	// 用**魔数**再确认一次：扩展名是客户端说了算的，不能只信它。
	if !looksLikeImage(data, ext) {
		return "", errs.Invalid("文件内容不是有效图片（扩展名与实际格式不符）")
	}

	dir := AvatarDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", errs.Internalf(err, "创建头像目录失败")
	}
	// 文件名带时间戳：同一用户换头像后，浏览器不会因为 URL 相同而继续用旧缓存。
	name := fmt.Sprintf("%d-%d%s", *uid, time.Now().Unix(), ext)
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		return "", errs.Internalf(err, "保存头像失败")
	}
	url := "/uploads/avatars/" + name
	if err := s.db(ctx).Model(&model.User{}).Where("id = ?", *uid).
		Updates(map[string]any{"avatar": url, "updated_at": now()}).Error; err != nil {
		return "", errs.Internalf(err, "更新头像字段失败")
	}
	return url, nil
}

// looksLikeImage 按文件头判断格式，不信扩展名。
func looksLikeImage(data []byte, ext string) bool {
	if len(data) < 12 {
		return false
	}
	switch ext {
	case ".png":
		return data[0] == 0x89 && data[1] == 'P' && data[2] == 'N' && data[3] == 'G'
	case ".jpg", ".jpeg":
		return data[0] == 0xFF && data[1] == 0xD8
	case ".webp":
		return string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP"
	}
	return false
}
