//go:build !unix

package source

import "os"

// 首期控制面仅认证 Unix 持久卷与文件身份语义，未认证平台失败关闭。
func openRegular(_ *os.Root, _ string) (*os.File, error) { return nil, ErrUnavailable }
