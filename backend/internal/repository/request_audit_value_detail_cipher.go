package repository

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// requestAuditValueDetailKeyBytes 是 AES-256 密钥长度。
const requestAuditValueDetailKeyBytes = 32

// requestAuditValueDetailKeyInfo 是派生专用子密钥的 HKDF info 标签。
//
// 用途隔离在这里是硬要求：值明细与 429 错误诊断正文（error-diagnostic-body）、
// 诊断头值**不能**共用同一把密钥，否则一处密文的泄露或误用会波及另一处。
const requestAuditValueDetailKeyInfo = "sub2api:request-audit-value-detail:v1"

// requestAuditValueDetailKeyVersion 是当前密文格式所属的密钥代。
const requestAuditValueDetailKeyVersion = 1

// requestAuditValueDetailAESCipher 用 AES-256-GCM 加密值明细载荷。
//
// 密文格式为 nonce || ciphertext || tag，作为 BYTEA 直接落库，不做 base64。
// 没有任何明文回退路径：解密失败即视为不可用，调用方只能看到「值不可用」。
type requestAuditValueDetailAESCipher struct {
	aead    cipher.AEAD
	version int
}

// NewRequestAuditValueDetailCipher 用给定密钥构造值明细加密器。
func NewRequestAuditValueDetailCipher(key []byte, version int) (service.RequestAuditValueDetailCipher, error) {
	if len(key) != requestAuditValueDetailKeyBytes {
		return nil, fmt.Errorf("request audit value detail key must be %d bytes, got %d", requestAuditValueDetailKeyBytes, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create request audit value detail cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create request audit value detail gcm: %w", err)
	}
	return &requestAuditValueDetailAESCipher{aead: aead, version: version}, nil
}

// NewRequestAuditValueDetailCipherFromConfig 从配置中已存在的加密主密钥派生专用子密钥。
//
// 使用 HKDF-SHA256 做用途隔离，而不是直接复用 TOTP／凭据密钥或诊断正文密钥。
// 主密钥缺失或格式不对时返回错误；调用方可以据此不提供 cipher，
// 此时值一律不留存（绝不回退为明文）。
func NewRequestAuditValueDetailCipherFromConfig(cfg *config.Config) (service.RequestAuditValueDetailCipher, error) {
	if cfg == nil {
		return nil, errors.New("request audit value detail cipher needs configuration")
	}
	master, err := hex.DecodeString(strings.TrimSpace(cfg.Totp.EncryptionKey))
	if err != nil {
		return nil, fmt.Errorf("request audit value detail cipher master key is not valid hex: %w", err)
	}
	if len(master) != requestAuditValueDetailKeyBytes {
		return nil, fmt.Errorf("request audit value detail cipher master key must be %d bytes, got %d", requestAuditValueDetailKeyBytes, len(master))
	}
	derived, err := hkdfSHA256(master, []byte(requestAuditValueDetailKeyInfo), requestAuditValueDetailKeyBytes)
	if err != nil {
		return nil, err
	}
	return NewRequestAuditValueDetailCipher(derived, requestAuditValueDetailKeyVersion)
}

// ProvideRequestAuditValueDetailCipher 为依赖注入提供值明细加密器。
//
// 返回 nil 而不是合成明文、使用不安全的默认密钥，或让整个服务启动失败：
//   - 主密钥缺失、不是 32 字节或不是合法 hex 时不可用；
//   - 主密钥不是**手动配置**的（EncryptionKeyConfigured=false）时同样不可用：
//     未配置时配置层会自动生成一把进程级密钥，换进程即变，密文会在重启后永久无法解密，
//     而接口仍然承诺「7 天内可查看」——那是静默的数据损失，不是外观问题；
//   - 整个服务不能因为一个默认关闭的能力而起不来。
//
// nil 加密器的语义是「值一律不留存」：门控照常判定，值读取一律拒绝，不存在明文回退。
// 只记录一次不含密钥材料的告警，便于运维发现「开了采集但密钥不可用于留存」。
func ProvideRequestAuditValueDetailCipher(cfg *config.Config) service.RequestAuditValueDetailCipher {
	if !isRequestAuditValueDetailKeyRestartStable(cfg) {
		logger.LegacyPrintf("repository.request_audit_value_detail",
			"request audit value detail cipher is unavailable; encrypted values will not be retained")
		return nil
	}
	valueCipher, err := NewRequestAuditValueDetailCipherFromConfig(cfg)
	if err != nil {
		logger.LegacyPrintf("repository.request_audit_value_detail",
			"request audit value detail cipher is unavailable; encrypted values will not be retained")
		return nil
	}
	return valueCipher
}

// isRequestAuditValueDetailKeyRestartStable 报告派生密钥的主密钥是否来自显式配置。
//
// 只有手动配置的密钥才是跨重启稳定的；自动生成的密钥换个进程就变，
// 用它留存值等于承诺了无法兑现的到期读取。
func isRequestAuditValueDetailKeyRestartStable(cfg *config.Config) bool {
	return cfg != nil && cfg.Totp.EncryptionKeyConfigured
}

// Encrypt 加密一段正文字节；每次调用使用独立随机 nonce。
func (c *requestAuditValueDetailAESCipher) Encrypt(plaintext []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("request audit value detail cipher is unavailable")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("request audit value detail cipher nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt 解密并验证认证标签；任何篡改或密钥不符都以错误结束，不返回部分明文。
func (c *requestAuditValueDetailAESCipher) Decrypt(ciphertext []byte) ([]byte, error) {
	if c == nil || c.aead == nil {
		return nil, errors.New("request audit value detail cipher is unavailable")
	}
	nonceSize := c.aead.NonceSize()
	if len(ciphertext) <= nonceSize {
		return nil, errors.New("request audit value detail cipher: ciphertext too short")
	}
	plaintext, err := c.aead.Open(nil, ciphertext[:nonceSize], ciphertext[nonceSize:], nil)
	if err != nil {
		return nil, fmt.Errorf("request audit value detail cipher: decrypt failed: %w", err)
	}
	return plaintext, nil
}

// KeyVersion 返回密文所属的密钥代，供轮换与审计使用。
func (c *requestAuditValueDetailAESCipher) KeyVersion() int {
	if c == nil {
		return 0
	}
	return c.version
}
