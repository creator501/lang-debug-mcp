package native

import (
	"github.com/langdebug/lang-debug-mcp/internal/backend"
)

// Prober 探测 native 后端（lldb-dap，缺失时降级 gdb MI）的可用性。
type Prober struct {
	// Lang 是该探测器负责的语言（C/C++/Rust）。
	Lang backend.Language
}

// Language 实现 backend.Prober。
func (p Prober) Language() backend.Language {
	if p.Lang == "" {
		return backend.C
	}
	return p.Lang
}

// Backend 实现 backend.Prober：优先报告 lldb-dap。
func (Prober) Backend() string { return backendName }

// Probe 实现 backend.Prober。
//
// 探测失败通过 Status 表达：调用方据此给出安装指引，而不是让建会话直接失败。
// 只有 gdb 时仍报告可用，但把后端名与能力子集写进 Status，避免调用方按
// lldb-dap 的完整能力预期去用改值与 logpoint。
func (p Prober) Probe() backend.Status {
	lang := p.Language()
	st := backend.Status{Language: lang, Backend: backendName}
	if adapter, err := Discover(); err == nil {
		st.Available = true
		st.Version = adapter.Version
		st.Notes = describe(string(lang))
		return st
	}
	if info, err := discoverGDB(); err == nil {
		st.Available = true
		st.Backend = gdbBackendName
		st.Version = info.version
		st.Notes = append(describe(string(lang)),
			"未找到 lldb-dap，降级到 gdb MI：仅保证断点、栈与变量读取",
			"gdb: "+info.path+"（改值与 logpoint 不可用，改用 debug_eval）",
		)
		return st
	}
	st.Missing = []string{"lldb-dap", "gdb"}
	st.InstallHint = "安装 LLVM（含 lldb-dap，能力完整）或 gdb（降级：仅断点/栈/变量），" +
		"MSYS2 可用 pacman -S mingw-w64-ucrt-x86_64-lldb；装好后仍找不到时，" +
		"用 MSYS2_ROOT 指向 MSYS2 根目录，或用 LANGDEBUG_LLDB_DAP 指定 lldb-dap 路径"
	return st
}

// 确保实现契约。
var _ backend.Prober = Prober{}
