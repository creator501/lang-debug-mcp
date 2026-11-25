//go:build !windows

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
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
	// 回收直接子进程：Unix 上若不 Wait，被 SIGKILL 的进程会一直停在僵尸态，
	// kill(pid, 0) 仍返回成功，上层会误以为整棵树没杀干净。
	_, _ = cmd.Process.Wait()
	return nil
}

func (t *pgroupTree) close() error { return nil }

// IsAlive 报告进程是否仍然存在；僵尸态视为已结束。
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// /proc/<pid>/stat 形如 "123 (name) S ..."，进程名可能含空格与括号，
	// 因此从最后一个 ')' 之后取状态字段。
	if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		if i := strings.LastIndexByte(string(data), ')'); i >= 0 {
			fields := strings.Fields(string(data[i+1:]))
			if len(fields) > 0 && fields[0] == "Z" {
				return false
			}
		}
	}
	return syscall.Kill(pid, 0) == nil
}
