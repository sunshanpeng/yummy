//go:build !unix

package sourcearchive

import (
	"io/fs"
	"os"
)

func openLocalFile(_ *os.Root, _ string) (*os.File, error) { return nil, ErrUnsupported }

// 首发认证的本地上传端为 macOS/Linux；其他系统未验证硬链接语义，拒绝打包。
func hasMultipleLinks(_ fs.FileInfo) bool { return true }
