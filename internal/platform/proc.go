// Package platform 收敛所有操作系统差异：进程树控制、路径归一、输出编码。
//
// 上层（会话、快照、工具面）不允许出现 GOOS 分支，一切平台差异都必须收敛在本包内。
package platform

import (
	"os/exec"
)

// Proc 持有被托管的子进程及其进程树控制句柄。
//
// 一个 Proc 对应一棵进程树：终止时整棵树一并清理，避免调试器后端退出后
// 目标进程残留。Windows 上由 Job Object 承载，Unix 上由进程组承载。
type Proc struct {
	cmd  *exec.Cmd
	tree procTree
}

// StartProc 启动命令并把其纳入进程树管理。
func StartProc(cmd *exec.Cmd) (*Proc, error) {
	tree, err := newProcTree()
	if err != nil {
		return nil, err
	}
	p := &Proc{cmd: cmd, tree: tree}
	if err := tree.start(cmd); err != nil {
		_ = tree.close()
		return nil, err
	}
	return p, nil
}

// Cmd 返回被托管的命令。
func (p *Proc) Cmd() *exec.Cmd { return p.cmd }

// Pid 返回目标进程标识，未启动时返回零。
func (p *Proc) Pid() int {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Kill 终止整棵进程树。
func (p *Proc) Kill() error {
	if p == nil || p.tree == nil {
		return nil
	}
	return p.tree.kill(p.cmd)
}

// Close 终止整棵进程树并释放平台资源。
func (p *Proc) Close() error {
	if p == nil || p.tree == nil {
		return nil
	}
	err := p.tree.kill(p.cmd)
	if cerr := p.tree.close(); err == nil {
		err = cerr
	}
	p.tree = nil
	return err
}

// procTree 是各平台的进程树实现。
type procTree interface {
	// start 启动命令并把其纳入树管理。
	start(cmd *exec.Cmd) error
	// kill 终止整棵树，对已退出进程应视为成功。
	kill(cmd *exec.Cmd) error
	// close 释放平台句柄。
	close() error
}
