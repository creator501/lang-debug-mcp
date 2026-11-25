//go:build !windows

package platform

import (
	"fmt"
	"os/exec"
	"syscall"
)

// pgroupTree 用进程组承载进程树。
type pgroupTree struct{}

func newProcTree() (procTree, error) {
	return &pgroupTree{}, nil
}

func (t *pgroupTree) start(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// 让子进程成为新进程组组长，从而使整棵树落到同一个进程组内。
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return err
	}
	// 显式设置进程组，避免 Setpgid 在某些组合下被忽略导致的偏差。
	if err := syscall.Setpgid(cmd.Process.Pid, cmd.Process.Pid); err != nil {
		// 进程可能已退出，退化为单进程场景，不视为启动失败。
		_ = err
	}
	return nil
}

func (t *pgroupTree) kill(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pid := cmd.Process.Pid
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		// 进程组已不存在时退回单进程终止。
		if perr := cmd.Process.Kill(); perr != nil {
			return fmt.Errorf("终止进程树失败: %w", err)
		}
	}
	return nil
}

func (t *pgroupTree) close() error { return nil }

// IsAlive 报告进程是否仍然存在。
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
