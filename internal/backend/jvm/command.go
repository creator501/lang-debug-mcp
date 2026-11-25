package jvm

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// javaCommand 是拆解后的 Java 启动命令。
type javaCommand struct {
	// Interpreter 是 java 可执行文件。
	Interpreter string
	// JVMArgs 是 JVM 选项（含 -cp 的值对），已剔除既有调试参数。
	JVMArgs []string
	// ClassPath 是类路径条目。
	ClassPath []string
	// MainClass 是主类名（点分）；使用 -jar 时为空。
	MainClass string
	// Jar 是 -jar 指定的包路径。
	Jar string
	// ProgramArgs 传给 main 的参数。
	ProgramArgs []string
}

// 调试代理相关参数，改写启动命令时需要剔除。
var jdwpArgPrefixes = []string{
	"-agentlib:jdwp",
	"-Xrunjdwp",
	"-Xdebug",
	"-Xnoagent",
	"-agentpath:",
}

// parseJavaCommand 拆解 java 启动命令。
//
// 只接受直接由 java 解释器启动的形式：Maven / Gradle / kotlin / scala 等包装
// 器会自己拉起 JVM，注入的调试参数不会生效，因此明确拒绝并给出替代方案。
func parseJavaCommand(command []string) (javaCommand, error) {
	if len(command) == 0 {
		return javaCommand{}, model.New(model.CodeInputInvalid, "启动命令为空").
			WithFix(`示例: ["java", "-cp", "out", "com.example.App"]`)
	}
	if !isJavaInterpreter(command[0]) {
		return javaCommand{}, model.New(model.CodeEnvDebugInfo, "无法为 %s 注入 JDWP 参数", command[0]).
			WithFix("JVM 后端只能直接启动 java（如 [\"java\", \"-cp\", \"out\", \"Main\"]）；" +
				"Maven/Gradle/kotlin/scala 请先构建出 class 或 jar，再改用 java 启动；" +
				"若目标已在运行，用 -agentlib:jdwp 启动后走 attach")
	}

	c := javaCommand{Interpreter: command[0]}
	seenDebug := false
	i := 1
	for i < len(command) {
		arg := command[i]
		switch {
		case arg == "-cp" || arg == "-classpath" || arg == "--class-path":
			if i+1 >= len(command) {
				return javaCommand{}, model.New(model.CodeInputInvalid, "%s 之后缺少类路径", arg)
			}
			c.ClassPath = append(c.ClassPath, splitPathList(command[i+1])...)
			c.JVMArgs = append(c.JVMArgs, arg, command[i+1])
			i += 2
		case arg == "-jar":
			if i+1 >= len(command) {
				return javaCommand{}, model.New(model.CodeInputInvalid, "-jar 之后缺少包路径")
			}
			c.Jar = command[i+1]
			c.ProgramArgs = append([]string{}, command[i+2:]...)
			return c, nil
		case strings.HasPrefix(arg, "-"):
			if isDebugArg(arg) {
				seenDebug = true
				i++
				continue
			}
			c.JVMArgs = append(c.JVMArgs, arg)
			i++
		default:
			c.MainClass = arg
			c.ProgramArgs = append([]string{}, command[i+1:]...)
			return c, nil
		}
	}
	_ = seenDebug
	if c.Jar == "" && c.MainClass == "" {
		return javaCommand{}, model.New(model.CodeInputInvalid, "启动命令中没有主类或 -jar").
			WithFix(`示例: ["java", "-cp", "out", "com.example.App"]`)
	}
	return c, nil
}

// isDebugArg 报告参数是否与调试代理相关。
func isDebugArg(arg string) bool {
	for _, prefix := range jdwpArgPrefixes {
		if strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return false
}

// isJavaInterpreter 报告命令首项是否为 java。
func isJavaInterpreter(arg string) bool {
	base := strings.ToLower(filepath.Base(arg))
	base = strings.TrimSuffix(base, ".exe")
	return base == "java" || base == "javaw"
}

// argv 生成注入调试参数后的完整命令行。
func (c javaCommand) argv(port int) []string {
	out := make([]string, 0, len(c.JVMArgs)+len(c.ProgramArgs)+4)
	out = append(out, c.Interpreter)
	out = append(out, AgentArgv(strconv.Itoa(port))...)
	out = append(out, c.JVMArgs...)
	if c.Jar != "" {
		out = append(out, "-jar", c.Jar)
	} else {
		out = append(out, c.MainClass)
	}
	out = append(out, c.ProgramArgs...)
	return out
}

// AgentArgv 返回 JDWP 代理参数。
//
// 注入启动命令与生成“可复制的重启命令”共用这一份格式，避免两处写法漂移。
func AgentArgv(address string) []string {
	return []string{"-agentlib:jdwp=transport=dt_socket,server=y,suspend=y,address=" + address}
}

// RestartHint 是“目标未开启调试代理”时给出的重启建议。
type RestartHint struct {
	// Argv 是带 JDWP 代理的完整命令行。
	Argv []string
	// Port 是建议的调试端口；执行 Argv 后按该端口 attach。
	Port int
	// Display 是可直接复制执行的命令行文本。
	Display string
}

// RestartHintFor 根据调用方给出的启动命令生成带 JDWP 代理的重启命令。
//
// 端口优先沿用调用方指定的调试端口；未指定时现取一个空闲端口，让“执行命令 →
// 按端口 attach”这条修复路径不需要调用方再猜参数。启动命令无法解析为 java
// 形式时返回 false，由调用方退回纯文本指引。
func RestartHintFor(spec backend.LaunchSpec, port int) (RestartHint, bool) {
	if len(spec.Command) == 0 {
		return RestartHint{}, false
	}
	cmdInfo, err := parseJavaCommand(spec.Command)
	if err != nil {
		return RestartHint{}, false
	}
	if port <= 0 {
		port, err = freePort()
		if err != nil {
			return RestartHint{}, false
		}
	}
	argv := cmdInfo.argv(port)
	return RestartHint{Argv: argv, Port: port, Display: joinArgs(argv)}, true
}

// joinArgs 把命令行拼成可复制执行的文本，含空格或引号的参数加双引号。
func joinArgs(argv []string) string {
	quoted := make([]string, 0, len(argv))
	for _, arg := range argv {
		if arg != "" && !strings.ContainsAny(arg, " \t\"'") {
			quoted = append(quoted, arg)
			continue
		}
		quoted = append(quoted, `"`+strings.ReplaceAll(arg, `"`, `\"`)+`"`)
	}
	return strings.Join(quoted, " ")
}

// effectiveClassPath 返回类路径条目，未指定时按当前目录处理。
func (c javaCommand) effectiveClassPath() []string {
	if len(c.ClassPath) > 0 {
		return c.ClassPath
	}
	return []string{"."}
}

// splitPathList 按平台分隔符拆分类路径。
func splitPathList(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, string(os.PathListSeparator))
}

// freePort 申请一个空闲端口。
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, model.New(model.CodeEnvPermission, "无法申请空闲调试端口").Wrap(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}
