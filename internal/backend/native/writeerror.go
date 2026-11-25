package native

import (
	"strings"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// 写值失败的特征片段：lldb 只给文本原因，这里按措辞归类。
var (
	// immutableMarkers 表示编译期常量：写入不会影响已生成的代码。
	immutableMarkers = []string{"const-qualified", "is const", "constant", "immutable"}
	// readOnlyMarkers 表示只读内存段：字符串字面量、只读数据。
	readOnlyMarkers = []string{"read-only", "readonly", "read only", "not writable",
		"cannot write", "write protected"}
	// optimizedMarkers 表示变量没有存储位置：已被优化掉或缺少调试信息。
	optimizedMarkers = []string{"optimized out", "no location", "no debug information"}
	// unsupportedMarkers 表示适配器本身没有写能力。
	unsupportedMarkers = []string{"not supported", "unsupported", "not implemented", "unknown request"}
)

// MapWriteError 实现 dap 的写值失败归类。
//
// 判据是 llvm 的措辞而非目标内存状态，因此顺序上先认“编译期常量”（消息里常
// 同时出现 cannot write），再认只读段、优化掉与能力缺失；都不匹配时返回 nil，
// 由通用层原样上报，避免臆造原因。
func (d Dialect) MapWriteError(typ string, err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case matchMarker(msg, immutableMarkers):
		return model.New(model.CodeTargetImmutable,
			"目标 %s 是编译期常量，写入不会影响已生成的代码: %v", typ, err).
			WithFix("改调用它的代码路径或换成运行期变量，不要指望改常量生效")
	case matchMarker(msg, readOnlyMarkers):
		return model.New(model.CodeTargetReadOnly,
			"写入目标 %s 位于只读内存（字符串字面量或只读全局），直接写会破坏进程: %v", typ, err).
			WithFix("改写到堆上或运行期可写的变量，再重新读值确认")
	case matchMarker(msg, optimizedMarkers):
		return model.New(model.CodeTargetOptimized,
			"变量 %s 在当前优化级别下没有存储位置（已被优化掉或缺少调试信息）: %v", typ, err).
			WithFix("用 -g -O0 重新编译后复现，再尝试写入")
	case matchMarker(msg, unsupportedMarkers):
		return model.New(model.CodeCapUnsupported, "该调试器不支持写入 %s: %v", typ, err).
			WithFix("用 debug_eval 在栈帧内执行赋值表达式")
	}
	return nil
}

// matchMarker 报告消息里是否出现任一特征片段。
func matchMarker(msg string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
