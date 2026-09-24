package repository

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// errorDiagnosticKeyBytes 是 AES-256 密钥长度。
const errorDiagnosticKeyBytes = 32

// errorDiagnosticKeyInfo 是派生专用子密钥的 HKDF info 标签，
// 保证错误诊断正文与 TOTP／凭据加密不共用同一把密钥。
const errorDiagnosticKeyInfo = "sub2api:error-diagnostic-body:v1"

// errorDiagnosticKeyVersion 是当前密文格式所属的密钥代。
const errorDiagnosticKeyVersion = 1

// errorDiagnosticAESCipher 用 AES-256-GCM 加密诊断正文。
//
// 密文格式为 nonce || ciphertext || tag，作为 BYTEA 直接落库，不做 base64。
// 没有任何明文回退路径：解密失败即视为不可用，调用方只能看到「正文不可用」。
type errorDiagnosticAESCipher struct {
	aead    cipher.AEAD
	version int
}

// NewErrorDiagnosticBodyCipher 用给定密钥构造诊断正文加密器。
func NewErrorDiagnosticBodyCipher(key []byte, version int) (service.ErrorDiagnosticBodyCipher, error) {
	if len(key) != errorDiagnosticKeyBytes {
		return nil, fmt.Errorf("error diagnostic body key must be %d bytes, got %d", errorDiagnosticKeyBytes, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create error diagnostic cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create error diagnostic gcm: %w", err)
	}
	return &errorDiagnosticAESCipher{aead: aead, version: version}, nil
}

// NewErrorDiagnosticBodyCipherFromConfig 从配置中已存在的加密主密钥派生专用的诊断正文密钥。
//
// 使用 HKDF-SHA256 做用途隔离，而不是直接复用 TOTP／凭据密钥。
// 主密钥缺失或格式不对时返回错误；调用方可以据此不提供 cipher，
// 此时正文一律不留存（绝不回退为明文）。
func NewErrorDiagnosticBodyCipherFromConfig(cfg *config.Config) (service.ErrorDiagnosticBodyCipher, error) {
	if cfg == nil {
		return nil, errors.New("error diagnostic cipher needs configuration")
	}
	master, err := hex.DecodeString(cfg.Totp.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("error diagnostic cipher master key is not valid hex: %w", err)
	}
	if len(master) != errorDiagnosticKeyBytes {
		return nil, fmt.Errorf("error diagnostic cipher master key must be %d bytes, got %d", errorDiagnosticKeyBytes, len(master))
	}
	derived, err := hkdfSHA256(master, []byte(errorDiagnosticKeyInfo), errorDiagnosticKeyBytes)
	if err != nil {
		return nil, err
	}
	return NewErrorDiagnosticBodyCipher(derived, errorDiagnosticKeyVersion)
}

// ProvideErrorDiagnosticBodyCipher 为依赖注入提供诊断正文加密器。
//
// 返回 nil 而不是合成明文、使用不安全的默认密钥，或让整个服务启动失败：
//   - 主密钥缺失、不是 32 字节或不是合法 hex 时不可用；
//   - 主密钥不是**手动配置**的（EncryptionKeyConfigured=false）时同样不可用：
//     未配置时配置层会自动生成一把进程级密钥，换进程即变，密文会在重启后永久无法解密，
//     而接口仍然承诺「7 天内可查看」——那是静默的数据损失，不是外观问题；
//   - 整个服务不能因为一个默认关闭的诊断功能而起不来。
//
// nil 加密器在诊断服务里的语义是「正文一律不留存」：元数据照常以净化形式落库，
// 正文读取一律拒绝，不存在明文回退。门控只由管理端写入，但设置可以被其他路径
// （通用设置接口、直接改库）写成开启，因此这条不变量必须在加密层强制，而不是只靠界面。
//
// 只记录一次不含密钥材料的告警，便于运维发现「开了采集但密钥不可用于留存」。
func ProvideErrorDiagnosticBodyCipher(cfg *config.Config) service.ErrorDiagnosticBodyCipher {
	if !isErrorDiagnosticBodyKeyRestartStable(cfg) {
		logger.LegacyPrintf("repository.error_diagnostic",
			"error diagnostic body cipher is unavailable; encrypted bodies will not be retained")
		return nil
	}
	cipher, err := NewErrorDiagnosticBodyCipherFromConfig(cfg)
	if err != nil {
		logger.LegacyPrintf("repository.error_diagnostic",
			"error diagnostic body cipher is unavailable; encrypted bodies will not be retained")
		return nil
	}
	return cipher
}

// isErrorDiagnosticBodyKeyRestartStable 报告派生正文密钥的主密钥是否来自显式配置。
//
// 只有手动配置的密钥才是跨重启稳定的；自动生成的密钥换个进程就变，
// 用它留存正文等于承诺了无法兑现的到期读取。
func isErrorDiagnosticBodyKeyRestartStable(cfg *config.Config) bool {
	return cfg != nil && cfg.Totp.EncryptionKeyConfigured
}

// hkdfSHA256 实现 RFC 5869 的 HKDF-Extract + HKDF-Expand（salt 为空）。
func hkdfSHA256(secret, info []byte, length int) ([]byte, error) {
	if length <= 0 || length > 255*sha256.Size {
		return nil, errors.New("error diagnostic cipher: invalid derived key length")
	}
	salt := make([]byte, sha256.Size)
	extract := hmac.New(sha256.New, salt)
	// hmac.Hash.Write 永不返回错误（文档保证 err 恒为 nil），沿用仓库既有写法显式丢弃。
	_, _ = extract.Write(secret)
	prk := extract.Sum(nil)

	out := make([]byte, 0, length)
	var block []byte
	for counter := byte(1); len(out) < length; counter++ {
		expand := hmac.New(sha256.New, prk)
		_, _ = expand.Write(block)
		_, _ = expand.Write(info)
		_, _ = expand.Write([]byte{counter})
		block = expand.Sum(nil)
		out = append(out, block...)
	}
	return out[:length], nil
}

// Encrypt 加密一段文本正文字节；每次调用使用独立随机 nonce。
func (c *errorDiagnosticAESCipher) Encrypt(plaintext []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("error diagnostic cipher is unavailable")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("error diagnostic cipher nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt 解密并验证认证标签；任何篡改或密钥不符都以错误结束，不返回部分明文。
func (c *errorDiagnosticAESCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("error diagnostic cipher is unavailable")
	}
	nonceSize := c.aead.NonceSize()
	if len(ciphertext) <= nonceSize {
		return nil, errors.New("error diagnostic cipher: ciphertext too short")
	}
	plaintext, err := c.aead.Open(nil, ciphertext[:nonceSize], ciphertext[nonceSize:], nil)
	if err != nil {
		return nil, fmt.Errorf("error diagnostic cipher: decrypt failed: %w", err)
	}
	return plaintext, nil
}

// KeyVersion 返回密文所属的密钥代，供轮换与审计使用。
func (c *errorDiagnosticAESCipher) KeyVersion() int {
	if c == nil {
		return 0
	}
	return c.version
}
