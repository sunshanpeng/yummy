// Package workerprotocol 收口 worker 双向流的共享错误与协议限制。
package workerprotocol

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
)

var (
	ErrQualification = errors.New("缺少可验证的节点资格证据")
	ErrIdentity      = errors.New("worker连接身份无效或已被替代")
	ErrSpoolFull     = errors.New("worker持久发送日志已达到容量上限")
	ErrProtocol      = errors.New("worker协议无效")
)

// RuntimeLogMaxChunkBytes 是运行日志单块字节上限，与数据库约束保持一致。
const RuntimeLogMaxChunkBytes = 16 << 10

// RuntimeLogStreamID 是运行日志流的 journal 键，worker 持久化与控制面 ack 两侧共用同一实现，
// 因此不存在构造分歧；对 (pod, container, restart) 取 SHA-256 保证长度有界且无歧义。
func RuntimeLogStreamID(value *platformv1.WorkerRuntimeLog) (string, error) {
	if value == nil {
		return "", ErrProtocol
	}
	sum := sha256.Sum256([]byte(value.PodUid + "\x00" + value.Container + "\x00" + strconv.FormatInt(int64(value.RestartCount), 10)))
	return hex.EncodeToString(sum[:]), nil
}
