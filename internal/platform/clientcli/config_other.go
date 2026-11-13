//go:build !unix

package clientcli

import (
	"errors"
	"os"
)

// 未验证原子锁和安全打开语义的平台默认拒绝保存或读取凭据。
func (s configStore) withLock(func(*os.Root) error) error {
	return errors.New("当前平台不支持安全凭据存储")
}
func openSecureFile(*os.Root, string, int, bool) (*os.File, error) {
	return nil, errors.New("当前平台不支持安全凭据存储")
}
func atomicSecureWrite(*os.Root, string, []byte) error {
	return errors.New("当前平台不支持安全凭据存储")
}
