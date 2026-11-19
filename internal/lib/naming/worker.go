package naming

// maxWorkerIDLength 限制 worker ID 长度，规则与 scripts/generate-dev-certs.sh 保持一致。
const maxWorkerIDLength = 128

// ValidWorkerID 校验 worker ID 格式：只允许字母、数字、点、下划线和连字符。
func ValidWorkerID(id string) bool {
	if id == "" || len(id) > maxWorkerIDLength {
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
