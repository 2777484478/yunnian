// Jenkins 接入配置的读写。
//
// 与 GitLab 同一套口径（同类事用同一套实现，不该让后来人学两遍），
// 多出来的一个维度是**用户名**：
//
//   - 启用开关 / 地址 / 用户名 / 超时 / 上次识别到的账号 / CSRF 状态 → system_config
//   - 密钥（API Token 或密码）                                    → secret_credential 的加密列
//
// 用户名刻意**不加密**：它不是机密（界面上本来就要显示它），把它一起塞进密文列
// 会让"到底是谁在连"变成一个要解密才能回答的问题，排查时反而更麻烦。
// 真正需要保护的是密钥本身——那一列是明文 text，一旦入库，
// 此后任何一次库导出（备份、只读副本、开发拉库）都等于一次凭据泄露。
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

	"github.com/yunnian/yunnian/internal/config"
	"github.com/yunnian/yunnian/internal/crypto"
	"github.com/yunnian/yunnian/internal/model"
)

// Jenkins 配置在 system_config 里的键名。
const (
	KeyJenkinsEnabled  = "integration.jenkins.enabled"
	KeyJenkinsBaseURL  = "integration.jenkins.baseUrl"
	KeyJenkinsUsername = "integration.jenkins.username"
	KeyJenkinsTimeout  = "integration.jenkins.timeout"
	KeyJenkinsIdentity = "integration.jenkins.identity"
	KeyJenkinsCrumb    = "integration.jenkins.crumb"

	jenkinsConfigGroup = "integration"

	// 密钥在 secret_credential 里的定位。一个平台只接一套 Jenkins，
	// 所以 scope 固定——留了多套的余地就要在 UI 上选，而目前没有这个需求。
	JenkinsCredScope = "jenkins"
	JenkinsCredName  = "basic-credential"
	JenkinsCredType  = "jenkins_basic"
)

// JenkinsSettings 从库里读回的 Jenkins 配置，附带"这份值从哪来"。
type JenkinsSettings struct {
	Cfg config.JenkinsConfig
	// FromDB 为 false 表示库里还没有这些键，用的全是 config.yaml 的值。
	FromDB bool
	// Identity 上次连通性测试识别到的账号名（可能为空：还没测过）。
	Identity string
	// CrumbEnabled 上次探测到的是否启用了 CSRF 保护。存下来是为了让配置卡片
	// 在没点过"测试连接"之前也能显示一个已知值，而不是空白。
	CrumbEnabled bool
	// UpdatedAt 取最新一次改动时间，用于界面上显示"最后修改"。
	UpdatedAt time.Time
}

// LoadJenkinsConfig 读取当前生效的 Jenkins 连接配置。
//
// 密钥解密失败会**返回错误**而不是当作"没配密钥"：
// 后者会退化成 401，让人去查密码内容，而真因其实是主密钥被换过。
func LoadJenkinsConfig(ctx context.Context, db *gorm.DB, sealer *crypto.Sealer, fallback config.JenkinsConfig) (*JenkinsSettings, error) {
	out := &JenkinsSettings{Cfg: fallback}

	keys := []string{
		KeyJenkinsEnabled, KeyJenkinsBaseURL, KeyJenkinsUsername,
		KeyJenkinsTimeout, KeyJenkinsIdentity, KeyJenkinsCrumb,
	}
	var rows []model.SystemConfig
	if err := db.WithContext(ctx).Where("config_key IN ?", keys).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("查询 Jenkins 配置失败: %w", err)
	}
	byKey := make(map[string]model.SystemConfig, len(rows))
	for i := range rows {
		byKey[rows[i].ConfigKey] = rows[i]
	}

	if row, ok := byKey[KeyJenkinsEnabled]; ok {
		out.FromDB = true
		out.Cfg.Enabled = strings.EqualFold(strings.TrimSpace(row.ConfigValue), "true")
	}
	if row, ok := byKey[KeyJenkinsBaseURL]; ok {
		out.FromDB = true
		out.Cfg.BaseURL = strings.TrimSpace(row.ConfigValue)
	}
	if row, ok := byKey[KeyJenkinsUsername]; ok {
		out.FromDB = true
		out.Cfg.Username = strings.TrimSpace(row.ConfigValue)
	}
	if row, ok := byKey[KeyJenkinsTimeout]; ok {
		out.FromDB = true
		// 值非法时保留配置文件里的超时，而不是把 0 传下去
		// （0 会被 net/http 解释成"永不超时"，表现为请求永远挂着）。
		if secs, err := strconv.Atoi(strings.TrimSpace(row.ConfigValue)); err == nil && secs > 0 {
			out.Cfg.Timeout = config.Duration(time.Duration(secs) * time.Second)
		}
	}
	if row, ok := byKey[KeyJenkinsIdentity]; ok {
		out.Identity = strings.TrimSpace(row.ConfigValue)
	}
	if row, ok := byKey[KeyJenkinsCrumb]; ok {
		out.CrumbEnabled = strings.EqualFold(strings.TrimSpace(row.ConfigValue), "true")
	}
	for _, row := range rows {
		if row.UpdatedAt.After(out.UpdatedAt) {
			out.UpdatedAt = row.UpdatedAt
		}
	}

	// 密钥
	var cred model.SecretCredential
	err := db.WithContext(ctx).Where("scope = ? AND name = ?", JenkinsCredScope, JenkinsCredName).First(&cred).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		// 库里没有密钥行。分两种情况，不能混为一谈：
		//   - 界面从未接管过（FromDB=false）→ 沿用 config.yaml 里的，这是初始兜底；
		//   - 界面已经接管过（FromDB=true）→ **不回退**。
		//     否则"清除密钥"会被配置文件里那份悄悄顶回来：用户以为撤掉了凭据，
		//     平台实际还在用旧密码连他的 Jenkins，这是最不该出错的场景。
		if out.FromDB {
			out.Cfg.Token = ""
		}
	case err != nil:
		return nil, fmt.Errorf("查询 Jenkins 密钥失败: %w", err)
	default:
		plain, err := sealer.Open(crypto.Sealed{Cipher: cred.Cipher, IV: cred.IV, AuthTag: cred.AuthTag})
		if err != nil {
			return nil, fmt.Errorf("Jenkins 密钥解密失败（security.masterKey 是否被更换过？）: %w", err)
		}
		out.Cfg.Token = plain
		if cred.UpdatedAt.After(out.UpdatedAt) {
			out.UpdatedAt = cred.UpdatedAt
		}
	}
	return out, nil
}

// JenkinsSaveInput 一次保存的入参。
type JenkinsSaveInput struct {
	Enabled  bool
	BaseURL  string
	Username string
	Timeout  int // 秒
	// Token 与 ClearToken 是三种互斥意图，必须能分清，否则会静默丢凭据：
	//   - Token 非空             → 覆盖为新密钥
	//   - Token 空 && ClearToken → 删除已存密钥
	//   - Token 空 && !ClearToken → 保持原密钥不变
	//     （前端只看得到掩码、回传不了明文；若把空串当"清空"，
	//      那么每次改地址都会顺手把密钥删掉）
	Token      string
	ClearToken bool
	// FallbackToken 是 configs/config.yaml 里的密钥。
	// "保持原密钥不变"时若库里还没有密钥行，就把这份搬进加密存储——
	// 这样"留空=不修改"在首次保存时也字面成立，而不是静默把密钥弄丢。
	FallbackToken string
	UserID        *uint64
}

// SaveJenkinsConfig 保存 Jenkins 配置。整体一个事务：不能出现"地址存了、密钥没存"
// 这种半截状态——那会让平台拿新地址配旧密码，报一个与真实原因无关的 401。
func SaveJenkinsConfig(ctx context.Context, db *gorm.DB, sealer *crypto.Sealer, in JenkinsSaveInput) error {
	enabled := "false"
	if in.Enabled {
		enabled = "true"
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = 15
	}

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, kv := range []struct{ key, val, typ, desc string }{
			{KeyJenkinsEnabled, enabled, "bool", "Jenkins 接入开关"},
			{KeyJenkinsBaseURL, strings.TrimSpace(in.BaseURL), "string", "Jenkins 服务地址（含 context path，如 http://host:1143/jenkins）"},
			{KeyJenkinsUsername, strings.TrimSpace(in.Username), "string", "Jenkins Basic 认证用户名"},
			{KeyJenkinsTimeout, strconv.Itoa(timeout), "number", "Jenkins API 超时（秒）"},
		} {
			if err := upsertSystemConfig(tx, kv.key, kv.val, kv.typ, jenkinsConfigGroup, kv.desc, in.UserID); err != nil {
				return err
			}
		}

		// 清除密钥：直接删掉那行。删行而不是写空密文——密文列 NOT NULL，
		// 而且留着一条"解不开也没用的密文"只会让后来人以为平台还持有凭据。
		if in.ClearToken {
			if err := tx.Where("scope = ? AND name = ?", JenkinsCredScope, JenkinsCredName).
				Delete(&model.SecretCredential{}).Error; err != nil {
				return fmt.Errorf("清除 Jenkins 密钥失败: %w", err)
			}
			return resetJenkinsProbe(tx, in.UserID)
		}

		token := strings.TrimSpace(in.Token)
		var existing model.SecretCredential
		findErr := tx.Where("scope = ? AND name = ?", JenkinsCredScope, JenkinsCredName).First(&existing).Error
		hasRow := findErr == nil

		if token == "" && !hasRow && strings.TrimSpace(in.FallbackToken) != "" {
			// 首次保存且只填了地址：把配置文件里的密钥搬进加密存储，
			// 避免"界面接管后凭据凭空消失"。这是一次性的搬迁，不是持续回退。
			token = strings.TrimSpace(in.FallbackToken)
		}
		if token == "" {
			// 保持原密钥不变。
			return nil
		}

		sealed, err := sealer.Seal(token)
		if err != nil {
			return fmt.Errorf("加密 Jenkins 密钥失败: %w", err)
		}
		if !hasRow {
			cred := model.SecretCredential{
				Name: JenkinsCredName, Type: JenkinsCredType, Scope: JenkinsCredScope,
				Cipher: sealed.Cipher, IV: sealed.IV, AuthTag: sealed.AuthTag, KeyVersion: 1,
				Description: "平台调用 Jenkins REST API（由「第三方集成」页维护）",
				CreatedBy:   in.UserID,
			}
			if err := tx.Create(&cred).Error; err != nil {
				return fmt.Errorf("写入 Jenkins 密钥失败: %w", err)
			}
		} else if err := tx.Model(&model.SecretCredential{}).Where("id = ?", existing.ID).Updates(map[string]any{
			"cipher": sealed.Cipher, "iv": sealed.IV, "auth_tag": sealed.AuthTag,
			"type": JenkinsCredType, "updated_at": time.Now(),
		}).Error; err != nil {
			return fmt.Errorf("更新 Jenkins 密钥失败: %w", err)
		}

		// 密钥换了，之前识别到的账号与 CSRF 状态都属于旧凭据，清掉免得界面继续显示旧账号。
		return resetJenkinsProbe(tx, in.UserID)
	})
}

// resetJenkinsProbe 清空"上次测试识别到的账号"与 CSRF 状态。
func resetJenkinsProbe(tx *gorm.DB, userID *uint64) error {
	for _, kv := range []struct{ key, desc string }{
		{KeyJenkinsIdentity, "上次连通性测试识别到的账号"},
		{KeyJenkinsCrumb, "上次探测到的 CSRF 保护状态"},
	} {
		if err := upsertSystemConfig(tx, kv.key, "", "string", jenkinsConfigGroup, kv.desc, userID); err != nil {
			return err
		}
	}
	return tx.Model(&model.SecretCredential{}).
		Where("scope = ? AND name = ?", JenkinsCredScope, JenkinsCredName).
		Update("expires_at", nil).Error
}

// SaveJenkinsProbeResult 记录一次连通性测试识别到的账号与 CSRF 状态。
//
// 只在"测试的是已保存的凭据"时调用：用表单里还没保存的值测出来的账号，
// 不属于当前配置，记下来会让界面显示一个并不生效的账号。
func SaveJenkinsProbeResult(ctx context.Context, db *gorm.DB, identity string, crumbEnabled bool) error {
	crumb := "false"
	if crumbEnabled {
		crumb = "true"
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := upsertSystemConfig(tx, KeyJenkinsIdentity, identity, "string", jenkinsConfigGroup,
			"上次连通性测试识别到的账号", nil); err != nil {
			return err
		}
		if err := upsertSystemConfig(tx, KeyJenkinsCrumb, crumb, "string", jenkinsConfigGroup,
			"上次探测到的 CSRF 保护状态", nil); err != nil {
			return err
		}
		return tx.Model(&model.SecretCredential{}).
			Where("scope = ? AND name = ?", JenkinsCredScope, JenkinsCredName).
			Updates(map[string]any{"last_used_at": time.Now()}).Error
	})
}
