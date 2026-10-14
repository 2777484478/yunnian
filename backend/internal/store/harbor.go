// Harbor 接入配置的读写。
//
// 与 Jenkins 同一套口径（同类事用同一套实现，不该让后来人学两遍）：
//
//   - 启用开关 / 两个地址 / 镜像地址前缀 / 用户名 / 跳过证书校验 / 超时 → system_config
//   - 密码（Harbor 账号口令）                                        → secret_credential 的加密列
//
// ⚠️ **两个地址必须分开填**，因为平台要读的是两套互不相同的 HTTP 接口：
//
//	registry 地址（baseUrl）  如 https://host:5000 —— 走 /v2/（OCI Distribution），
//	                          用来读 manifest / digest，判断"这个 tag 到底存不存在"；
//	Core API 地址（coreApiUrl）如 https://host     —— 走 /api/v2.0/，
//	                          用来读**推送时间与推送人**（/v2/ 给不出这两个字段）。
//
// 实测目标 Harbor 只对外暴露了 registry 组件：`/api/v2.0/systeminfo` 在 :5000 / :443 / :80
// 上全是 nginx 的 404 页。所以填错其中一个的表现不是"接不上"，而是
// **某一类信息永远读不到** —— 界面上必须分别探活、分别报错，不能合成一句"连接失败"。
//
// 读取遵循 DB 优先、逐键回退：库里没有的键才用 config.yaml 的值（逐键而不是整体回退，
// 避免"只在界面上改了超时"却把地址一并顶回配置文件默认值）。
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

// Harbor 配置在 system_config 里的键名。
const (
	KeyHarborEnabled  = "integration.harbor.enabled"
	KeyHarborBaseURL  = "integration.harbor.baseUrl"
	KeyHarborCoreURL  = "integration.harbor.coreApiUrl"
	KeyHarborRegistry = "integration.harbor.registry"
	KeyHarborUsername = "integration.harbor.username"
	KeyHarborInsecure = "integration.harbor.insecure"
	KeyHarborTimeout  = "integration.harbor.timeout"
	KeyHarborIdentity = "integration.harbor.identity"
	harborConfigGroup = "integration"
	HarborCredScope   = "harbor"
	HarborCredName    = "basic-credential"
	HarborCredType    = "harbor_basic"
	harborDefaultSecs = 15
)

// HarborSettings 从库里读回的 Harbor 配置，附带"这份值从哪来"。
type HarborSettings struct {
	Cfg config.HarborConfig
	// FromDB 为 false 表示库里还没有这些键，用的全是 config.yaml 的值。
	FromDB bool
	// Identity 上次探测到能用的账号（未认证访问时为空）。
	Identity string
	// UpdatedAt 取最新一次改动时间，用于界面显示"最后修改"。
	UpdatedAt time.Time
}

// LoadHarborConfig 读取当前生效的 Harbor 连接配置。
//
// 密码解密失败会**返回错误**而不是当作"没配密码"：后者会退化成 401，
// 让人去怀疑密码填错了，而真因其实是主密钥被换过。
func LoadHarborConfig(ctx context.Context, db *gorm.DB, sealer *crypto.Sealer, fallback config.HarborConfig) (*HarborSettings, error) {
	out := &HarborSettings{Cfg: fallback}

	keys := []string{
		KeyHarborEnabled, KeyHarborBaseURL, KeyHarborCoreURL, KeyHarborRegistry,
		KeyHarborUsername, KeyHarborInsecure, KeyHarborTimeout, KeyHarborIdentity,
	}
	var rows []model.SystemConfig
	if err := db.WithContext(ctx).Where("config_key IN ?", keys).Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("查询 Harbor 配置失败: %w", err)
	}
	byKey := make(map[string]model.SystemConfig, len(rows))
	for i := range rows {
		byKey[rows[i].ConfigKey] = rows[i]
	}

	if row, ok := byKey[KeyHarborEnabled]; ok {
		out.FromDB = true
		out.Cfg.Enabled = strings.EqualFold(strings.TrimSpace(row.ConfigValue), "true")
	}
	if row, ok := byKey[KeyHarborBaseURL]; ok {
		out.FromDB = true
		out.Cfg.BaseURL = strings.TrimSpace(row.ConfigValue)
	}
	if row, ok := byKey[KeyHarborCoreURL]; ok {
		out.FromDB = true
		out.Cfg.CoreAPIURL = strings.TrimSpace(row.ConfigValue)
	}
	if row, ok := byKey[KeyHarborRegistry]; ok {
		out.FromDB = true
		out.Cfg.Registry = strings.TrimSpace(row.ConfigValue)
	}
	if row, ok := byKey[KeyHarborUsername]; ok {
		out.FromDB = true
		out.Cfg.Username = strings.TrimSpace(row.ConfigValue)
	}
	if row, ok := byKey[KeyHarborInsecure]; ok {
		out.FromDB = true
		out.Cfg.Insecure = strings.EqualFold(strings.TrimSpace(row.ConfigValue), "true")
	}
	if row, ok := byKey[KeyHarborTimeout]; ok {
		out.FromDB = true
		// 非法值保留配置文件里的超时，而不是把 0 传下去
		//（0 会被 net/http 解释成"永不超时"，表现为请求永远挂着）。
		if secs, err := strconv.Atoi(strings.TrimSpace(row.ConfigValue)); err == nil && secs > 0 {
			out.Cfg.Timeout = config.Duration(time.Duration(secs) * time.Second)
		}
	}
	if row, ok := byKey[KeyHarborIdentity]; ok {
		out.Identity = strings.TrimSpace(row.ConfigValue)
	}
	for _, row := range rows {
		if row.UpdatedAt.After(out.UpdatedAt) {
			out.UpdatedAt = row.UpdatedAt
		}
	}

	var cred model.SecretCredential
	err := db.WithContext(ctx).Where("scope = ? AND name = ?", HarborCredScope, HarborCredName).First(&cred).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		// 库里没有密码行。分两种情况，不能混为一谈：
		//   - 界面从未接管过（FromDB=false）→ 沿用 config.yaml 里的，这是初始兜底；
		//   - 界面已经接管过（FromDB=true）→ **不回退**，否则"清除密码"会被配置文件里
		//     那份悄悄顶回来：用户以为撤掉了凭据，平台实际还在用旧口令连他的 Harbor。
		if out.FromDB {
			out.Cfg.Password = ""
		}
	case err != nil:
		return nil, fmt.Errorf("查询 Harbor 密码失败: %w", err)
	default:
		plain, err := sealer.Open(crypto.Sealed{Cipher: cred.Cipher, IV: cred.IV, AuthTag: cred.AuthTag})
		if err != nil {
			return nil, fmt.Errorf("Harbor 密码解密失败（security.masterKey 是否被更换过？）: %w", err)
		}
		out.Cfg.Password = plain
		if cred.UpdatedAt.After(out.UpdatedAt) {
			out.UpdatedAt = cred.UpdatedAt
		}
	}
	return out, nil
}

// HarborSaveInput 一次保存的入参。
type HarborSaveInput struct {
	Enabled  bool
	BaseURL  string
	CoreURL  string
	Registry string
	Username string
	Insecure bool
	Timeout  int
	// Password 与 ClearPassword 是三种互斥意图，必须能分清，否则会静默丢凭据：
	//   - Password 非空                → 覆盖为新口令
	//   - Password 空 && ClearPassword → 删除已存口令
	//   - Password 空 && !ClearPassword → 保持原口令不变
	//     （前端只看得到掩码、回传不了明文；若把空串当"清空"，
	//      那么每次改地址都会顺手把口令删掉）
	Password      string
	ClearPassword bool
	// FallbackPassword 是 configs/config.yaml 里的口令。"保持原口令不变"时若库里还没有
	// 密钥行，就把这份搬进加密存储 —— 让"留空=不修改"在首次保存时也字面成立。
	FallbackPassword string
	UserID           *uint64
}

// SaveHarborConfig 保存 Harbor 配置。整体一个事务：不能出现"地址存了、口令没存"
// 这种半截状态 —— 那会拿新地址配旧口令，报一个与真实原因无关的 401。
func SaveHarborConfig(ctx context.Context, db *gorm.DB, sealer *crypto.Sealer, in HarborSaveInput) error {
	enabled := "false"
	if in.Enabled {
		enabled = "true"
	}
	insecure := "false"
	if in.Insecure {
		insecure = "true"
	}
	timeout := in.Timeout
	if timeout <= 0 {
		timeout = harborDefaultSecs
	}

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, kv := range []struct{ key, val, typ, desc string }{
			{KeyHarborEnabled, enabled, "bool", "Harbor 接入开关"},
			{KeyHarborBaseURL, strings.TrimSpace(in.BaseURL), "string", "Harbor registry 组件地址（走 /v2/，如 https://host:5000）"},
			{KeyHarborCoreURL, strings.TrimSpace(in.CoreURL), "string", "Harbor Core API 地址（走 /api/v2.0/，如 https://host）"},
			{KeyHarborRegistry, strings.TrimSpace(in.Registry), "string", "镜像地址里出现的仓库部分（如 host:5000）"},
			{KeyHarborUsername, strings.TrimSpace(in.Username), "string", "Harbor 账号（建议用只读 robot 账号）"},
			{KeyHarborInsecure, insecure, "bool", "跳过 Harbor 证书校验（自签证书时打开）"},
			{KeyHarborTimeout, strconv.Itoa(timeout), "number", "Harbor API 超时（秒）"},
		} {
			if err := upsertSystemConfig(tx, kv.key, kv.val, kv.typ, harborConfigGroup, kv.desc, in.UserID); err != nil {
				return err
			}
		}

		// 清除口令：直接删掉那行。密文列 NOT NULL，且留着一条"解不开也没用的密文"
		// 只会让后来人以为平台还持有凭据。
		if in.ClearPassword {
			if err := tx.Where("scope = ? AND name = ?", HarborCredScope, HarborCredName).
				Delete(&model.SecretCredential{}).Error; err != nil {
				return fmt.Errorf("清除 Harbor 密码失败: %w", err)
			}
			return resetHarborProbe(tx, in.UserID)
		}

		pwd := strings.TrimSpace(in.Password)
		var existing model.SecretCredential
		findErr := tx.Where("scope = ? AND name = ?", HarborCredScope, HarborCredName).First(&existing).Error
		hasRow := findErr == nil

		if pwd == "" && !hasRow && strings.TrimSpace(in.FallbackPassword) != "" {
			// 首次保存且只填了地址：把配置文件里的口令搬进加密存储，
			// 避免"界面接管后凭据凭空消失"。一次性搬迁，不是持续回退。
			pwd = strings.TrimSpace(in.FallbackPassword)
		}
		if pwd == "" {
			return nil // 保持原口令不变
		}

		sealed, err := sealer.Seal(pwd)
		if err != nil {
			return fmt.Errorf("加密 Harbor 密码失败: %w", err)
		}
		if !hasRow {
			cred := model.SecretCredential{
				Name: HarborCredName, Type: HarborCredType, Scope: HarborCredScope,
				Cipher: sealed.Cipher, IV: sealed.IV, AuthTag: sealed.AuthTag, KeyVersion: 1,
				Description: "平台读取 Harbor 仓库（由「第三方集成」页维护）",
				CreatedBy:   in.UserID,
			}
			if err := tx.Create(&cred).Error; err != nil {
				return fmt.Errorf("写入 Harbor 密码失败: %w", err)
			}
		} else if err := tx.Model(&model.SecretCredential{}).Where("id = ?", existing.ID).Updates(map[string]any{
			"cipher": sealed.Cipher, "iv": sealed.IV, "auth_tag": sealed.AuthTag,
			"type": HarborCredType, "updated_at": time.Now(),
		}).Error; err != nil {
			return fmt.Errorf("更新 Harbor 密码失败: %w", err)
		}

		// 口令换了，之前识别到的账号属于旧凭据，清掉免得界面继续显示旧账号。
		return resetHarborProbe(tx, in.UserID)
	})
}

// resetHarborProbe 清空"上次测试识别到的账号"。
func resetHarborProbe(tx *gorm.DB, userID *uint64) error {
	if err := upsertSystemConfig(tx, KeyHarborIdentity, "", "string", harborConfigGroup,
		"上次连通性测试识别到的 Harbor 账号", userID); err != nil {
		return err
	}
	return tx.Model(&model.SecretCredential{}).
		Where("scope = ? AND name = ?", HarborCredScope, HarborCredName).
		Update("expires_at", nil).Error
}

// SaveHarborProbeResult 记录一次连通性测试识别到的账号。
//
// 只在"测的是已保存的凭据"时调用：用表单里还没保存的值测出来的账号，
// 不属于当前配置，记下来会让界面显示一个并不生效的账号。
func SaveHarborProbeResult(ctx context.Context, db *gorm.DB, identity string) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := upsertSystemConfig(tx, KeyHarborIdentity, identity, "string", harborConfigGroup,
			"上次连通性测试识别到的 Harbor 账号", nil); err != nil {
			return err
		}
		return tx.Model(&model.SecretCredential{}).
			Where("scope = ? AND name = ?", HarborCredScope, HarborCredName).
			Updates(map[string]any{"last_used_at": time.Now()}).Error
	})
}
