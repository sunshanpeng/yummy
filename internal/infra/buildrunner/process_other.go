//go:build !unix

package buildrunner

import (
	"context"
	"os/exec"
)

// 首期固定runner只认证Linux；其他系统不降级为缺少进程组收敛的执行器。
func runProcessGroup(context.Context, *exec.Cmd) error { return ErrInvalid }
