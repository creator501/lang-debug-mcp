// Package cangjie 是仓颉（Cangjie）语言的调试后端。
//
// 仓颉用 cjc 编译成带 DWARF 调试信息的本地程序，调试链路上与 C/C++/Rust 相同：
// 复用 native 后端（lldb-dap，缺失时降级 gdb MI）。本包只补两件事：
// 把 cjc 纳入可用性探测，以及如实说明仓颉类型在调试器里的显示边界。
package cangjie

import (
	"context"
	"os/exec"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/backend/native"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// cjcEnvOverride 允许指定 cjc 可执行文件路径。
const cjcEnvOverride = "LANGDEBUG_CJC"

// Launcher 创建仓颉调试后端。
type Launcher struct{}

// Open 实现 backend.Launcher：复用 native 后端的 lldb-dap 链路。
func (Launcher) Open(ctx context.Context, spec backend.LaunchSpec) (backend.Debugger, error) {
	if spec.Language == "" {
		spec.Language = backend.Cangjie
	}
	dbg, err := native.Launcher{}.Open(ctx, spec)
	if err != nil {
		return nil, err
	}
	return withLanguage{Debugger: dbg}, nil
}

// withLanguage 把后端上报的语言改成仓颉，避免能力清单与错误信息里出现 c。
type withLanguage struct {
	backend.Debugger
}

// Language 实现 backend.Debugger。
func (w withLanguage) Language() backend.Language { return backend.Cangjie }

// Capability 实现 backend.Debugger：在 native 能力基础上追加仓颉侧边界。
func (w withLanguage) Capability() model.Capability {
	cap := w.Debugger.Capability()
	cap.Limits = append(cap.Limits, describe()...)
	return cap
}

// Prober 探测仓颉后端的可用性。
//
// 判定条件有两个：cjc 存在（否则根本没有可调试的产物）与 native 适配器
// 可用（lldb-dap，或降级的 gdb）。
type Prober struct{}

// Language 实现 backend.Prober。
func (Prober) Language() backend.Language { return backend.Cangjie }

// Backend 实现 backend.Prober：与 native 一致，优先 lldb-dap。
func (Prober) Backend() string { return "lldb-dap" }

// Probe 实现 backend.Prober。
func (Prober) Probe() backend.Status {
	st := native.Prober{Lang: backend.Cangjie}.Probe()
	st.Language = backend.Cangjie
	st.Notes = append(st.Notes, describe()...)
	path, err := resolveCJC()
	if err != nil {
		// 没有编译器就没有可调试的产物，即使适配器在也判为不可用。
		st.Available = false
		st.Missing = append(st.Missing, "cjc")
		st.InstallHint = "安装仓颉 SDK 并把 cjc 加入 PATH（或用 LANGDEBUG_CJC 指定），" +
			"编译时加 -g 才会带调试信息；" + st.InstallHint
		return st
	}
	return appendNote(st, "cjc: "+path)
}

// appendNote 追加一条说明并返回状态，保持 Probe 单出口的可读性。
func appendNote(st backend.Status, note string) backend.Status {
	st.Notes = append(st.Notes, note)
	return st
}

// resolveCJC 定位仓颉编译器。
func resolveCJC() (string, error) {
	path, err := exec.LookPath("cjc")
	if err == nil {
		return path, nil
	}
	return "", model.New(model.CodeEnvAdapterMissing,
		"未找到仓颉编译器 cjc").WithFix("安装仓颉 SDK 后把 cjc 加入 PATH")
}

// Limits 返回仓颉语言的调试边界，供能力清单与探测结果复用。
func Limits() []string { return describe() }

// describe 返回仓颉语言的调试边界。
func describe() []string {
	return []string{
		"仓颉编译产物是带 DWARF 的本地程序，调试链路与 C/C++ 相同（lldb-dap）",
		"必须用 cjc -g 编译：不带调试信息时只能看到地址与符号名",
		"class/struct 与泛型实例需要展开读取，字符串是对象而非裸指针",
		"Option<T> 等枚举显示为底层布局，需展开判断是 Some 还是 None",
		"采集点与栈采样可用；改值受 lldb-dap 能力限制（gdb 降级时不可用）",
	}
}

// 确保实现契约。
var (
	_ backend.Launcher = Launcher{}
	_ backend.Prober   = Prober{}
	_ backend.Debugger = withLanguage{}
)
