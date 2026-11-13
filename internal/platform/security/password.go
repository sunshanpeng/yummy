package security

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// 密码参数按 OWASP 的 Argon2id 最低建议设置，正式部署仍需在目标硬件测量。
const (
	passwordMemory  = 19 * 1024
	passwordTime    = 2
	passwordThreads = 1
)

var ErrPassword = errors.New("密码必须为 12～128 个字节")

func HashPassword(password string) (string, error) {
	if len(password) < 12 || len(password) > 128 {
		return "", ErrPassword
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成密码盐失败: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, passwordTime, passwordMemory, passwordThreads, 32)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", passwordMemory, passwordTime, passwordThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func VerifyPassword(encoded, password string) bool {
	if len(password) > 128 {
		return false
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	params := strings.Split(parts[3], ",")
	if len(params) != 3 {
		return false
	}
	values := make([]uint64, 3)
	for i, prefix := range []string{"m=", "t=", "p="} {
		if !strings.HasPrefix(params[i], prefix) {
			return false
		}
		value, err := strconv.ParseUint(strings.TrimPrefix(params[i], prefix), 10, 32)
		if err != nil {
			return false
		}
		values[i] = value
	}
	// 对持久化格式也设置上界，损坏的数据不能诱导无限制的内存分配。
	if values[0] < 8*1024 || values[0] > 128*1024 || values[1] < 1 || values[1] > 10 || values[2] < 1 || values[2] > 8 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 16 || len(salt) > 64 {
		return false
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(expected) != 32 {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, uint32(values[1]), uint32(values[0]), uint8(values[2]), uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1
}
