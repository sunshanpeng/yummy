package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// NewID 使用操作系统随机源；失败必须向上传递，不能降级为时间戳。
func NewID(prefix string) (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(bytes), nil
}

func NewToken(prefix string) (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(bytes), nil
}

func Digest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

type SecretBox struct {
	aead cipher.AEAD
}

// NewSecretBox 接收独立配置的 32 字节主密钥，不从数据库口令或开发证书派生。
func NewSecretBox(encodedKey string) (*SecretBox, error) {
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil || len(key) != 32 {
		return nil, errors.New("主密钥必须为 Base64 编码的 32 字节随机值")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &SecretBox{aead: aead}, nil
}

// Encrypt 用资源身份和版本作为附加认证数据，防止密文跨应用或版本替换。
func (b *SecretBox) Encrypt(plaintext []byte, identity string) ([]byte, error) {
	if b == nil || b.aead == nil || identity == "" {
		return nil, errors.New("秘密加密配置或资源身份无效")
	}
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return b.aead.Seal(nonce, nonce, plaintext, []byte(identity)), nil
}

func (b *SecretBox) Decrypt(encrypted []byte, identity string) ([]byte, error) {
	if b == nil || b.aead == nil || identity == "" || len(encrypted) < b.aead.NonceSize() {
		return nil, errors.New("秘密密文或资源身份无效")
	}
	nonce, ciphertext := encrypted[:b.aead.NonceSize()], encrypted[b.aead.NonceSize():]
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, []byte(identity))
	if err != nil {
		return nil, fmt.Errorf("秘密解密失败: %w", err)
	}
	return plaintext, nil
}
