// GitLab 接入配置的读写。
//
// 配置分两处存，这是刻意的：
//
//   - 地址 / 启用开关 / 超时 / 上次识别到的令牌身份 → system_config
//     （与平台其它参数同一张表，能被 SQL 直接排查，也能被审计记录覆盖）
//   - 访问令牌                                      → secret_credential 的加密列
//
// 为什么不把令牌一并塞进 system_config：那一列是明文 text，而「第三方集成」页上
// 明确写着"凭证加密存储"。明文入库会让那句话变成谎话；更实际的问题是，
// 此后任何一次库导出（备份、脱敏失败、只读副本、开发拉库）都等于一次令牌泄露。
//
// 读取遵循 **DB 优先、逐键回退**：库里没有的键才用 config.yaml 的值。
// 逐键而不是整体回退，是为了避免"只在界面上改了超时"却把地址一并顶回配置文件默认值。
// 也正因如此，这几个键**不写进种子数据**——否则种子里会固化一份默认地址，
// 与 config.yaml 里的地址形成两个真值来源，改哪个都不放心。
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
	"github.com/2777484478/yunnian/internal/crypto"
	"github.com/2777484478/yunnian/internal/model"
)

// GitLab 配置在 system_config 里的键名。
const (
	KeyGitLabEnabled  = "integration.gitlab.enabled"
	KeyGitLabBaseURL  = "integration.gitlab.baseUrl"
	KeyGitLabTimeout  = "integration.gitlab.timeout"
	KeyGitLabIdentity = "integration.gitlab.identity"
	// KeyGitLabAuthMode 认证方式（token / password）。空值按 token 处理（兼容老库）。
	KeyGitLabAuthMode = "integration.gitlab.authMode"
	// KeyGitLabUsername 账号密码模式的用户名。不是机密：界面上要显示它是谁在连。
	KeyGitLabUsername = "integration.gitlab.username"

	gitlabConfigGroup = "integration"

	// 令牌在 secret_credential 里的定位。一个平台只接一套 GitLab，
	// 所以 scope 固定，不留多套的余地——留了就要在 UI 上选，而目前没有这个需求。
	GitLabCredScope = "gitlab"
	GitLabCredName  = "access-token"
	GitLabCredType  = "git_token"
	// GitLabPwdName 账号密码模式下的密码。与令牌**分开两行**而不是共用一行：
	// 两种认证方式可能同时配着（切回来还能用），共用一行意味着切换即覆盖，
	// 而覆盖掉的密码没人能还原（库里只有密文）。
	GitLabPwdName = "password"
	GitLabPwdType = "git_password"
)

// GitLabSettings 从库里读回的 GitLab 配置，附带"这份值从哪来"。
type GitLabSettings struct {
	Cfg config.GitLabConfig
	// FromDB 为 false 表示库里还没有这些键，用的全是 config.yaml 的值。
	FromDB bool
	// Identity 是上次连通性测试识别到的令牌账号（可能为空：还没测过）。
	Identity string
	// TokenExpiresAt 是令牌自身声明的到期时间（来自 GitLab，可能为空）。
	TokenExpiresAt *time.Time
	// UpdatedAt 取最新一次改动时间，用于界面上显示"最后修改"。
	UpdatedAt time.Time
}

// LoadGitLabConfig 读取当前生效的 GitLab 连接配置。
//
// 令牌解密失败会**返回错误**而不是当作"没配令牌"：
// 后者会退化成 401，让人去查令牌内容，而真因其实是主密钥被换过。
func LoadGitLabConfig(ctx context.Context, db *gorm.DB, sealer *crypto.Sealer, fallback config.GitLabConfig) (*GitLabSettings, error) {
	out := &GitLabSettings{Cfg: fallback}

	keys := []string{KeyGitLabEnabled, KeyGitLabBaseURL, KeyGitLabTimeout, KeyGitLabIdentity,
		KeyGitLabAuthMode, KeyGitLabUsername}
	var rows []model.SystemConfig
	if err := db.WithContext(ctx).Where("config_key IN ?", keys).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("查询 GitLab 配置失败: %w", err)
	}
	byKey := make(map[string]model.SystemConfig, len(rows))
	for i := range rows {
		byKey[rows[i].ConfigKey] = rows[i]
	}

	if row, ok := byKey[KeyGitLabEnabled]; ok {
		out.FromDB = true
		out.Cfg.Enabled = strings.EqualFold(strings.TrimSpace(row.ConfigValue), "true")
	}
	if row, ok := byKey[KeyGitLabBaseURL]; ok {
		out.FromDB = true
		out.Cfg.BaseURL = strings.TrimSpace(row.ConfigValue)
	}
	if row, ok := byKey[KeyGitLabAuthMode]; ok {
		out.FromDB = true
		out.Cfg.AuthMode = strings.TrimSpace(row.ConfigValue)
	}
	if row, ok := byKey[KeyGitLabUsername]; ok {
		out.FromDB = true
		out.Cfg.Username = strings.TrimSpace(row.ConfigValue)
	}
	if row, ok := byKey[KeyGitLabTimeout]; ok {
		out.FromDB = true
		// 值非法时保留配置文件里的超时，而不是把 0 传下去（0 会被 net/http 解释成"永不超时"）。
		if secs, err := strconv.Atoi(strings.TrimSpace(row.ConfigValue)); err == nil && secs > 0 {
			out.Cfg.Timeout = config.Duration(time.Duration(secs) * time.Second)
		}
	}
	if row, ok := byKey[KeyGitLabIdentity]; ok {
		out.Identity = strings.TrimSpace(row.ConfigValue)
	}
	for _, row := range rows {
		if row.UpdatedAt.After(out.UpdatedAt) {
			out.UpdatedAt = row.UpdatedAt
		}
	}

	// 令牌
	var cred model.SecretCredential
	err := db.WithContext(ctx).Where("scope = ? AND name = ?", GitLabCredScope, GitLabCredName).First(&cred).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		// 库里没有令牌行。
		//
		// 分两种情况，不能混为一谈：
		//   - 界面从未接管过（FromDB=false）→ 沿用 config.yaml 里的 token，这是初始兜底；
		//   - 界面已经接管过（FromDB=true）→ **不回退**。
		//     否则"清除令牌"会被配置文件里那份悄悄顶回来：用户以为撤掉了凭据，
		//     平台实际还在用旧令牌访问仓库，这是最不该出错的场景。
		if out.FromDB {
			out.Cfg.Token = ""
		}
	case err != nil:
		return nil, fmt.Errorf("查询 GitLab 令牌失败: %w", err)
	default:
		plain, err := sealer.Open(crypto.Sealed{Cipher: cred.Cipher, IV: cred.IV, AuthTag: cred.AuthTag})
		if err != nil {
			return nil, fmt.Errorf("GitLab 令牌解密失败（security.masterKey 是否被更换过？）: %w", err)
		}
		out.Cfg.Token = plain
		if cred.ExpiresAt != nil {
			out.TokenExpiresAt = cred.ExpiresAt
		}
		if cred.UpdatedAt.After(out.UpdatedAt) {
			out.UpdatedAt = cred.UpdatedAt
		}
	}

	// 账号密码模式下的密码（独立一行，见 GitLabPwdName 的注释）。
	var pwd model.SecretCredential
	err = db.WithContext(ctx).Where("scope = ? AND name = ?", GitLabCredScope, GitLabPwdName).First(&pwd).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		// 与令牌同一口径：界面接管过（FromDB）就不回退到 config.yaml，
		// 否则"清除密码"会被配置文件里那份悄悄顶回来。
		if out.FromDB {
			out.Cfg.Password = ""
		}
	case err != nil:
		return nil, fmt.Errorf("查询 GitLab 密码失败: %w", err)
	default:
		plain, err := sealer.Open(crypto.Sealed{Cipher: pwd.Cipher, IV: pwd.IV, AuthTag: pwd.AuthTag})
		if err != nil {
			return nil, fmt.Errorf("GitLab 密码解密失败（security.masterKey 是否被更换过？）: %w", err)
		}
		out.Cfg.Password = plain
		if pwd.UpdatedAt.After(out.UpdatedAt) {
			out.UpdatedAt = pwd.UpdatedAt
		}
	}
	return out, nil
}

// GitLabSaveInput 一次保存的入参。
type GitLabSaveInput struct {
	Enabled bool
	BaseURL string
	Timeout int // 秒
	// Token 与 ClearToken 是三种互斥意图，必须能分清，否则会静默丢凭据：
	//   - Token 非空            → 覆盖为新令牌
	//   - Token 空 && ClearToken → 删除已存令牌
	//   - Token 空 && !ClearToken → 保持原令牌不变
	//     （前端只看得到掩码、回传不了明文；若把空串当"清空"，
	//      那么每次改地址都会顺手把令牌删掉）
	Token      string
	ClearToken bool
	// AuthMode / Username 走 system_config；Password / ClearPassword 与令牌同口径：
	//   - Password 非空            → 覆盖为新密码
	//   - Password 空 && ClearPassword → 删除已存密码
	//   - Password 空 && !ClearPassword → 保持原密码不变
	AuthMode      string
	Username      string
	Password      string
	ClearPassword bool
	// FallbackToken 是 configs/config.yaml 里的 token。
	// "保持原令牌不变"时若库里还没有令牌行，就把这份搬进加密存储——
	// 这样"留空=不修改"在首次保存时也字面成立，而不是静默把令牌弄丢。
	FallbackToken string
	UserID        *uint64
}

// SaveGitLabConfig 保存 GitLab 配置。整体一个事务：不能出现"地址存了、令牌没存"
// 这种半截状态——那会让平台拿新地址配旧令牌，报一个与真实原因无关的 401。
func SaveGitLabConfig(ctx context.Context, db *gorm.DB, sealer *crypto.Sealer, in GitLabSaveInput) error {
	enabled := "false"
	if in.Enabled {
		enabled = "true"
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = 10
	}

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, kv := range []struct{ key, val, typ, desc string }{
			{KeyGitLabEnabled, enabled, "bool", "GitLab 接入开关"},
			{KeyGitLabBaseURL, strings.TrimSpace(in.BaseURL), "string", "GitLab 服务地址（如 https://gitlab.example.com）"},
			{KeyGitLabTimeout, strconv.Itoa(timeout), "number", "GitLab API 超时（秒）"},
			{KeyGitLabAuthMode, normalizedAuthMode(in.AuthMode), "string", "GitLab 认证方式（token=访问令牌 / password=账号密码）"},
			{KeyGitLabUsername, strings.TrimSpace(in.Username), "string", "GitLab 账号密码模式下的用户名"},
		} {
			if err := upsertSystemConfig(tx, kv.key, kv.val, kv.typ, gitlabConfigGroup, kv.desc, in.UserID); err != nil {
				return err
			}
		}

		// 凭据有没有变，决定要不要清掉"上次识别到的身份"（它属于旧凭据）。
		credChanged := false

		// ---------------- 访问令牌
		if in.ClearToken {
			// 删行而不是写空密文——密文列 NOT NULL，
			// 而且留着一条"解不开也没用的密文"只会让后来人以为平台还持有凭据。
			if err := tx.Where("scope = ? AND name = ?", GitLabCredScope, GitLabCredName).
				Delete(&model.SecretCredential{}).Error; err != nil {
				return fmt.Errorf("清除 GitLab 令牌失败: %w", err)
			}
			credChanged = true
		} else if token := strings.TrimSpace(in.Token); token != "" || !hasGitLabCred(tx, GitLabCredName) {
			// 为空但有且仅有"库里还没有令牌行"这一种情况需要搬迁配置文件里的令牌，
			// 让"留空=不修改"在首次保存时也字面成立。
			if token == "" {
				token = strings.TrimSpace(in.FallbackToken)
			}
			if token != "" {
				if err := upsertGitLabCred(tx, sealer, GitLabCredName, GitLabCredType, token,
					"平台调用 GitLab REST API（由「第三方集成」页维护）", in.UserID); err != nil {
					return fmt.Errorf("保存 GitLab 令牌失败: %w", err)
				}
				credChanged = true
			}
		}

		// ---------------- 账号密码（独立一行，与令牌互不覆盖）
		if in.ClearPassword {
			if err := tx.Where("scope = ? AND name = ?", GitLabCredScope, GitLabPwdName).
				Delete(&model.SecretCredential{}).Error; err != nil {
				return fmt.Errorf("清除 GitLab 密码失败: %w", err)
			}
			credChanged = true
		} else if pwd := in.Password; strings.TrimSpace(pwd) != "" {
			// ⚠️ 密码**不做 Trim**：前后空格也是密码的一部分，
			// 裁掉会让人对着一个"我明明填对了"的账号查半天。只判空用 TrimSpace。
			if err := upsertGitLabCred(tx, sealer, GitLabPwdName, GitLabPwdType, pwd,
				"GitLab 账号密码模式下的密码（平台用它换 OAuth 令牌，由「第三方集成」页维护）", in.UserID); err != nil {
				return fmt.Errorf("保存 GitLab 密码失败: %w", err)
			}
			credChanged = true
		}

		if credChanged {
			// 凭据换了，之前识别到的身份与到期时间都属于旧凭据，清掉免得界面显示旧账号。
			return resetGitLabIdentity(tx, in.UserID)
		}
		return nil
	})
}

// normalizedAuthMode 归一化落库值：空按 token（兼容老库）。
func normalizedAuthMode(v string) string {
	if strings.EqualFold(strings.TrimSpace(v), config.GitLabAuthPassword) {
		return config.GitLabAuthPassword
	}
	return config.GitLabAuthToken
}

// hasGitLabCred 库里是否已有该名字的凭据行。
func hasGitLabCred(tx *gorm.DB, name string) bool {
	var n int64
	_ = tx.Model(&model.SecretCredential{}).
		Where("scope = ? AND name = ?", GitLabCredScope, name).Count(&n).Error
	return n > 0
}

// upsertGitLabCred 写入或更新一条 GitLab 凭据（加密）。
//
// 抽出来是因为令牌与密码走完全相同的加密/更新逻辑 —— 两处各写一遍，
// 早晚有一处忘了更新 type 或 key_version，而那种错只在解密时才暴露。
func upsertGitLabCred(tx *gorm.DB, sealer *crypto.Sealer, name, credType, plain, desc string, userID *uint64) error {
	enc, err := sealer.Seal(plain)
	if err != nil {
		return fmt.Errorf("加密凭据失败: %w", err)
	}
	var existing model.SecretCredential
	findErr := tx.Where("scope = ? AND name = ?", GitLabCredScope, name).First(&existing).Error
	switch {
	case errors.Is(findErr, gorm.ErrRecordNotFound):
		return tx.Create(&model.SecretCredential{
			Name: name, Type: credType, Scope: GitLabCredScope,
			Cipher: enc.Cipher, IV: enc.IV, AuthTag: enc.AuthTag, KeyVersion: 1,
			Description: desc, CreatedBy: userID,
		}).Error
	case findErr != nil:
		return findErr
	}
	return tx.Model(&model.SecretCredential{}).Where("id = ?", existing.ID).Updates(map[string]any{
		"cipher": enc.Cipher, "iv": enc.IV, "auth_tag": enc.AuthTag,
		"type": credType, "updated_at": time.Now(),
	}).Error
}

// resetGitLabIdentity 清空"上次测试识别到的令牌身份"与到期时间。
func resetGitLabIdentity(tx *gorm.DB, userID *uint64) error {
	if err := upsertSystemConfig(tx, KeyGitLabIdentity, "", "string", gitlabConfigGroup,
		"上次连通性测试识别到的令牌账号", userID); err != nil {
		return err
	}
	return tx.Model(&model.SecretCredential{}).
		Where("scope = ? AND name = ?", GitLabCredScope, GitLabCredName).
		Update("expires_at", nil).Error
}

// SaveGitLabProbeResult 记录一次连通性测试识别到的令牌身份与到期时间。
//
// 只在"测试的是已保存的令牌"时调用：用表单里还没保存的值测出来的身份，
// 不属于当前配置，记下来会让界面显示一个并不生效的账号。
func SaveGitLabProbeResult(ctx context.Context, db *gorm.DB, identity string, expiresAt *time.Time) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := upsertSystemConfig(tx, KeyGitLabIdentity, identity, "string", gitlabConfigGroup,
			"上次连通性测试识别到的令牌账号", nil); err != nil {
			return err
		}
		return tx.Model(&model.SecretCredential{}).
			Where("scope = ? AND name = ?", GitLabCredScope, GitLabCredName).
			Updates(map[string]any{"expires_at": expiresAt, "last_used_at": time.Now()}).Error
	})
}

// upsertSystemConfig 写入或更新一条系统配置。
//
// 不复用 service 的 UpdateConfig：那条路径**只允许改已存在的键**
// （为了防止"写进去了但没人读"的幽灵配置），而这里恰恰要按需创建这几个键。
func upsertSystemConfig(tx *gorm.DB, key, value, valueType, group, desc string, userID *uint64) error {
	var row model.SystemConfig
	err := tx.Where("config_key = ?", key).First(&row).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return tx.Create(&model.SystemConfig{
			ConfigKey: key, ConfigValue: value, ValueType: valueType,
			GroupName: group, Description: desc,
			UpdatedBy: userID, UpdatedAt: time.Now(),
		}).Error
	case err != nil:
		return err
	}
	if row.ConfigValue == value {
		return nil
	}
	return tx.Model(&model.SystemConfig{}).Where("id = ?", row.ID).Updates(map[string]any{
		"config_value": value, "updated_by": userID, "updated_at": time.Now(),
	}).Error
}
