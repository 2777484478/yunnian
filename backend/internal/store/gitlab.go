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

	gitlabConfigGroup = "integration"

	// 令牌在 secret_credential 里的定位。一个平台只接一套 GitLab，
	// 所以 scope 固定，不留多套的余地——留了就要在 UI 上选，而目前没有这个需求。
	GitLabCredScope = "gitlab"
	GitLabCredName  = "access-token"
	GitLabCredType  = "git_token"
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

	keys := []string{KeyGitLabEnabled, KeyGitLabBaseURL, KeyGitLabTimeout, KeyGitLabIdentity}
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
		} {
			if err := upsertSystemConfig(tx, kv.key, kv.val, kv.typ, gitlabConfigGroup, kv.desc, in.UserID); err != nil {
				return err
			}
		}

		// 清除令牌：直接删掉那行。删行而不是写空密文——密文列 NOT NULL，
		// 而且留着一条"解不开也没用的密文"只会让后来人以为平台还持有凭据。
		if in.ClearToken {
			if err := tx.Where("scope = ? AND name = ?", GitLabCredScope, GitLabCredName).
				Delete(&model.SecretCredential{}).Error; err != nil {
				return fmt.Errorf("清除 GitLab 令牌失败: %w", err)
			}
			return resetGitLabIdentity(tx, in.UserID)
		}

		token := strings.TrimSpace(in.Token)
		var existing model.SecretCredential
		findErr := tx.Where("scope = ? AND name = ?", GitLabCredScope, GitLabCredName).First(&existing).Error
		hasRow := findErr == nil

		if token == "" && !hasRow && strings.TrimSpace(in.FallbackToken) != "" {
			// 首次保存且只填了地址：把配置文件里的令牌搬进加密存储，
			// 避免"界面接管后令牌凭空消失"。这是一次性的搬迁，不是持续回退。
			token = strings.TrimSpace(in.FallbackToken)
		}
		if token == "" {
			// 保持原令牌不变。
			return nil
		}

		sealed, err := sealer.Seal(token)
		if err != nil {
			return fmt.Errorf("加密 GitLab 令牌失败: %w", err)
		}
		if !hasRow {
			cred := model.SecretCredential{
				Name: GitLabCredName, Type: GitLabCredType, Scope: GitLabCredScope,
				Cipher: sealed.Cipher, IV: sealed.IV, AuthTag: sealed.AuthTag, KeyVersion: 1,
				Description: "平台调用 GitLab REST API（由「第三方集成」页维护）",
				CreatedBy:   in.UserID,
			}
			if err := tx.Create(&cred).Error; err != nil {
				return fmt.Errorf("写入 GitLab 令牌失败: %w", err)
			}
		} else if err := tx.Model(&model.SecretCredential{}).Where("id = ?", existing.ID).Updates(map[string]any{
			"cipher": sealed.Cipher, "iv": sealed.IV, "auth_tag": sealed.AuthTag,
			"type": GitLabCredType, "updated_at": time.Now(),
		}).Error; err != nil {
			return fmt.Errorf("更新 GitLab 令牌失败: %w", err)
		}

		// 令牌换了，之前识别到的身份与到期时间都属于旧令牌，清掉免得界面继续显示旧账号。
		return resetGitLabIdentity(tx, in.UserID)
	})
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
