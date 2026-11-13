//go:build unix

package sourcearchive

import (
	"io/fs"
	"os"
	"syscall"
)

// 避免 lstat/open 之间被换成 FIFO 时阻塞，且最终文件不得是符号链接。
func openLocalFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

func hasMultipleLinks(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	// 无法确认类型时拒绝，不把未知文件系统身份当作安全普通文件。
	return !ok || stat.Nlink > 1
}
