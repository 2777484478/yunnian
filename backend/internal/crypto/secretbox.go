// Package crypto 提供凭证类字段的对称加密能力（AES-256-GCM）。
//
// 为什么单独一个包：加密与解密必须成对出现，散落在 service 里很容易出现
// 「写的时候加密了、读的时候忘了解密」这类静默错误。把成对逻辑收口在这里，
// 并在 Sealer 上只暴露三个方法（Seal / Open / Mask），使用面足够窄。
//
// 密钥来源：config.security.masterKey（环境变量 YUNNIAN_MASTER_KEY）。
// 主密钥本身不入库——库里只存密文、IV 与 AuthTag；密钥轮换由 keyVersion 字段配合。
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
)

// Sealer 加解密器。零值不可用，必须经 NewSealer 构造。
type Sealer struct {
	aead cipher.AEAD
	// fallback 标记当前用的是内置开发密钥，供启动日志告警。
	fallback bool
}

// NewSealer 由主密钥派生 AES-256 密钥。
//
// 用 SHA-256 派生而不是直接截断：主密钥是人工填的字符串，长度不定且熵分布不均，
// 哈希后固定 32 字节且把长度差异摊平。
func NewSealer(masterKey string, isFallback bool) (*Sealer, error) {
	if strings.TrimSpace(masterKey) == "" {
		return nil, fmt.Errorf("主密钥为空，无法初始化加密器")
	}
	if len(masterKey) < 16 {
		return nil, fmt.Errorf("主密钥至少 16 位（当前 %d 位）", len(masterKey))
	}
	key := sha256.Sum256([]byte("yunnian/secretbox/v1|" + masterKey))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, fmt.Errorf("初始化 AES 失败: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("初始化 GCM 失败: %w", err)
	}
	return &Sealer{aead: aead, fallback: isFallback}, nil
}

// UsingFallbackKey 表示当前用的是内置开发密钥。
func (s *Sealer) UsingFallbackKey() bool { return s != nil && s.fallback }

// Sealed 一段密文的三要素。与 secret_credential / ai_model_config 的列一一对应。
type Sealed struct {
	Cipher  []byte
	IV      []byte
	AuthTag []byte
}

// Empty 判断是否没有密文。
func (s Sealed) Empty() bool { return len(s.Cipher) == 0 && len(s.IV) == 0 }

// Seal 加密明文。
func (s *Sealer) Seal(plain string) (Sealed, error) {
	if s == nil || s.aead == nil {
		return Sealed{}, fmt.Errorf("加密器未初始化")
	}
	iv := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return Sealed{}, fmt.Errorf("生成随机 IV 失败: %w", err)
	}
	// Go 的 GCM Seal 把 tag 追加在密文尾部；这里拆开存，与 DDL 的三列对齐。
	out := s.aead.Seal(nil, iv, []byte(plain), nil)
	tagSize := s.aead.Overhead()
	return Sealed{
		Cipher:  out[:len(out)-tagSize],
		IV:      iv,
		AuthTag: out[len(out)-tagSize:],
	}, nil
}

// Open 解密。任一要素缺失都返回错误（而不是返回空串），
// 避免"解密失败被当成空密码"这种更难排查的问题。
func (s *Sealer) Open(in Sealed) (string, error) {
	if s == nil || s.aead == nil {
		return "", fmt.Errorf("加密器未初始化")
	}
	if in.Empty() {
		return "", fmt.Errorf("密文为空")
	}
	if len(in.IV) != s.aead.NonceSize() {
		return "", fmt.Errorf("IV 长度非法：期望 %d，实际 %d", s.aead.NonceSize(), len(in.IV))
	}
	combined := make([]byte, 0, len(in.Cipher)+len(in.AuthTag))
	combined = append(combined, in.Cipher...)
	combined = append(combined, in.AuthTag...)
	plain, err := s.aead.Open(nil, in.IV, combined, nil)
	if err != nil {
		return "", fmt.Errorf("解密失败（密钥不匹配或密文被篡改）: %w", err)
	}
	return string(plain), nil
}

// Mask 生成密钥掩码用于回显，形如 sk-********a3f9。
//
// 规则：长度 ≥ 12 时保留前 3 位与后 4 位；否则整体打码。
// 刻意不保留中间任何字符——掩码的唯一目的是让用户确认"填的是哪一把密钥"。
func Mask(plain string) string {
	v := strings.TrimSpace(plain)
	if v == "" {
		return "未配置"
	}
	if len(v) < 12 {
		return "********"
	}
	return v[:3] + "****************" + v[len(v)-4:]
}

// MaskBytes 对已存密文的展示用掩码（无法还原明文时使用）。
func MaskBytes(b []byte) string {
	if len(b) == 0 {
		return "未配置"
	}
	return "********（已加密存储）"
}

// Base64 编解码辅助：用于把密钥经配置文件/环境变量传递时避免特殊字符问题。
func Base64(in []byte) string { return base64.StdEncoding.EncodeToString(in) }
