package domain

import "time"

const (
	// MaxWorkerIDLength 限制 worker ID 长度。
	MaxWorkerIDLength = 128
	// MaxWorkerVersionLength 限制 worker 版本号的字节数，避免写入异常大的在线记录。
	MaxWorkerVersionLength = 128
)

type WorkerPresence struct {
	WorkerID   string
	InstanceID string
	Version    string
	LastSeen   time.Time
}

// ValidWorkerID 校验 worker ID 格式，规则与 scripts/generate-dev-certs.sh 一致：
// 只允许字母、数字、点、下划线和连字符。
func ValidWorkerID(id string) bool {
	if id == "" || len(id) > MaxWorkerIDLength {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
