//go:build unix

package buildrunner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// 固定入口及其子进程处于独立进程组，取消先SIGTERM，宽限后SIGKILL整个任务组。
// 该进程组仅在任务容器中，不能借此访问/控制宿主进程。
func runProcessGroup(ctx context.Context, cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 6 * time.Second
	if err := cmd.Start(); err != nil {
		return err
	}
	finished := make(chan struct{})
	killed := make(chan struct{})
	go func() {
		defer close(killed)
		select {
		case <-finished:
			return
		case <-ctx.Done():
		}
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-finished:
			return
		case <-timer.C:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
	err := cmd.Wait()
	close(finished)
	<-killed
	// Shell入口退出时不得遗留其后台buildkitd；固定任务组在当前进程结束前收敛。
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	return err
}
