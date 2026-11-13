//go:build !unix

package enrollment

import (
	"io/fs"
	"os"
)

func openCAFile(_ string) (*os.File, error) { return nil, ErrCA }

// 首期只认证 Unix 的私钥文件权限与链接语义。
func singleLink(_ fs.FileInfo) bool { return false }
