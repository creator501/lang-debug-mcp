package platform

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	envHelper     = "LANGDEBUG_HELPER"
	envHelperLeaf = "LANGDEBUG_HELPER_LEAF"
)

// TestHelperProcess 不是真实用例，而是被托管子进程的入口。
func TestHelperProcess(t *testing.T) {
	if os.Getenv(envHelper) == "" {
		return
	}
	leaf := os.Getenv(envHelperLeaf) == "1"
	if !leaf {
		// 等待父进程完成 Job Object 归属，再派生孙进程，避免测试竞态。
		time.Sleep(300 * time.Millisecond)
		gc := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
		gc.Env = append(os.Environ(), envHelper+"=1", envHelperLeaf+"=1")
		if err := gc.Start(); err != nil {
			fmt.Printf("SPAWN_FAILED %v\n", err)
			os.Exit(3)
		}
		fmt.Printf("GRANDCHILD %d\n", gc.Process.Pid)
	}
	fmt.Printf("SELF %d\n", os.Getpid())
	os.Stdout.Sync()
	time.Sleep(2 * time.Minute)
	os.Exit(0)
}

// TestKillTerminatesTree 验证终止托管进程后其子孙进程不残留。
func TestKillTerminatesTree(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(), envHelper+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("创建管道失败: %v", err)
	}
	p, err := StartProc(cmd)
	if err != nil {
		t.Fatalf("启动托管进程失败: %v", err)
	}

	pids := readHelperPids(t, stdout)
	if len(pids) < 2 {
		_ = p.Close()
		t.Fatalf("未取到父子进程标识，实际得到 %v", pids)
	}
	for _, pid := range pids {
		if !IsAlive(pid) {
			_ = p.Close()
			t.Fatalf("进程 %d 应在启动后存活", pid)
		}
	}

	if err := p.Close(); err != nil {
		t.Fatalf("关闭托管进程失败: %v", err)
	}

	for _, pid := range pids {
		if !waitDead(pid, 10*time.Second) {
			t.Fatalf("进程 %d 在终止整棵树后仍存活", pid)
		}
	}
}

func readHelperPids(t *testing.T, r interface{ Read([]byte) (int, error) }) []int {
	t.Helper()
	sc := bufio.NewScanner(r)
	var pids []int
	deadline := time.Now().Add(30 * time.Second)
	for sc.Scan() {
		line := sc.Text()
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		switch fields[0] {
		case "SELF", "GRANDCHILD":
			if pid, err := strconv.Atoi(fields[1]); err == nil {
				pids = append(pids, pid)
			}
		}
		if len(pids) >= 2 {
			return pids
		}
		if time.Now().After(deadline) {
			t.Fatalf("读取子进程输出超时，已取到 %v", pids)
		}
	}
	return pids
}

func waitDead(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !IsAlive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
