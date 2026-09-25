package service

// RequestAuditValueDetailCipher 加密值明细载荷。
//
// 接口刻意只有一个职责：把已经过白名单与有界校验的载荷变成密文，以及把密文变回载荷。
// 它**不**提供任何明文回退路径，也不暴露密钥：加密器缺失时调用方只能看到「值不可用」。
// 密钥派生（专用 HKDF 子密钥）与密钥代由 repository 层实现。
type RequestAuditValueDetailCipher interface {
	Encrypt(plaintext []byte) ([]byte, error)
	Decrypt(ciphertext []byte) ([]byte, error)
	KeyVersion() int
}
