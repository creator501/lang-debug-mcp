//go:build windows

package platform

import (
	"fmt"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// jobTree 用 Job Object 承载进程树。
//
// 置 KILL_ON_JOB_CLOSE 后，本进程无论正常退出还是崩溃，系统都会回收整棵树，
// 因此不会留下孤儿调试器或目标进程。
type jobTree struct {
	job windows.Handle
}

func newProcTree() (procTree, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("创建 Job Object 失败: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("配置 Job Object 失败: %w", err)
	}
	return &jobTree{job: job}, nil
}

func (t *jobTree) start(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	// 子进程在纳入 Job 之前若已再派生进程，那些进程不在树内。调试器后端
	// 启动时的派生窗口极小，且会话关闭时另有逐进程回收兜底。
	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("打开目标进程失败: %w", err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(t.job, h); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("把目标进程纳入 Job Object 失败: %w", err)
	}
	return nil
}

func (t *jobTree) kill(cmd *exec.Cmd) error {
	if err := windows.TerminateJobObject(t.job, 1); err != nil {
		// 目标已自然退出时终止整棵树会返回错误，此时退回单进程终止。
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}
	return nil
}

func (t *jobTree) close() error {
	if t.job == 0 {
		return nil
	}
	err := windows.CloseHandle(t.job)
	t.job = 0
	return err
}

// aliveCode 是 GetExitCodeProcess 表示“仍在运行”的取值。
const aliveCode = 259

// IsAlive 报告进程是否仍然存在。
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == aliveCode
}
