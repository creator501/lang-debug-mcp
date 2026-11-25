// Package backend 定义统一调试后端抽象与受支持语言清单。
//
// 十一种语言收敛为五类后端：JVM（JDWP）、native（lldb-dap / gdb）、Python
// （debugpy）、Node.js（V8 Inspector）、Go（dlv dap）。上层只依赖本包的抽象，
// 不出现语言分支。
package backend

import "fmt"

// Language 是受支持的语言标识。
type Language string

// 受支持的语言。
const (
	Java    Language = "java"
	Kotlin  Language = "kotlin"
	Scala   Language = "scala"
	Groovy  Language = "groovy"
	C       Language = "c"
	Cpp     Language = "cpp"
	Rust    Language = "rust"
	Cangjie Language = "cangjie"
	Python  Language = "python"
	NodeJS  Language = "nodejs"
	Go      Language = "go"
)

// Supported 按稳定顺序列出全部受支持语言。
var Supported = []Language{Java, Kotlin, Scala, Groovy, C, Cpp, Rust, Cangjie, Python, NodeJS, Go}

// Valid 报告语言标识是否受支持。
func (l Language) Valid() bool {
	for _, s := range Supported {
		if s == l {
			return true
		}
	}
	return false
}

// Family 返回语言所属的后端族，用于确定适配器类型。
func (l Language) Family() string {
	switch l {
	case Java, Kotlin, Scala, Groovy:
		return "jvm"
	case C, Cpp, Rust, Cangjie:
		// 仓颉编译成带 DWARF 的本地程序，与 C/C++/Rust 共用 lldb-dap 后端。
		return "native"
	case Python:
		return "python"
	case NodeJS:
		return "nodejs"
	case Go:
		return "go"
	}
	return ""
}

// Status 是某语言后端可用性的探测结果。
type Status struct {
	Language    Language `json:"language"`
	Backend     string   `json:"backend"`
	Available   bool     `json:"available"`
	Version     string   `json:"version,omitempty"`
	Missing     []string `json:"missing,omitempty"`
	InstallHint string   `json:"installHint,omitempty"`
	Notes       []string `json:"notes,omitempty"`
	// Alternatives 是按优先级排列的候选后端探测结果（含首选）。
	Alternatives []Status `json:"alternatives,omitempty"`
}

// Prober 探测单个语言后端的可用性。
type Prober interface {
	// Language 返回该探测器负责的语言。
	Language() Language
	// Backend 返回后端名称，如 lldb-dap、debugpy、cdp。
	Backend() string
	// Probe 执行探测，探测失败应体现在返回的 Status 中而非 error。
	Probe() Status
}

// Registry 保存已注册的后端探测器。
type Registry struct {
	order   []Language
	probers map[Language][]Prober
}

// NewRegistry 创建一个空注册表。
func NewRegistry() *Registry {
	return &Registry{probers: make(map[Language][]Prober)}
}

// Register 注册一个探测器，同一语言可注册多个，注册顺序即优先级顺序。
func (r *Registry) Register(p Prober) error {
	if p == nil {
		return fmt.Errorf("探测器为空")
	}
	lang := p.Language()
	if !lang.Valid() {
		return fmt.Errorf("不支持的语言: %s", lang)
	}
	if _, ok := r.probers[lang]; !ok {
		r.order = append(r.order, lang)
	}
	r.probers[lang] = append(r.probers[lang], p)
	return nil
}

// Languages 返回已注册探测器的语言，按注册顺序。
func (r *Registry) Languages() []Language {
	out := make([]Language, len(r.order))
	copy(out, r.order)
	return out
}

// Detect 探测所有已注册语言，按注册顺序返回结果。
//
// 同一语言注册多个探测器时，返回首选结果，并在 Alternatives 中给出完整
// 优先级链，便于调用方理解降级关系。
func (r *Registry) Detect() []Status {
	out := make([]Status, 0, len(r.order))
	for _, lang := range r.order {
		alts := make([]Status, 0, len(r.probers[lang]))
		for _, p := range r.probers[lang] {
			st := p.Probe()
			if st.Language == "" {
				st.Language = lang
			}
			if st.Backend == "" {
				st.Backend = p.Backend()
			}
			alts = append(alts, st)
		}
		if len(alts) == 0 {
			continue
		}
		primary := alts[0]
		for _, alt := range alts {
			if alt.Available {
				primary = alt
				break
			}
		}
		if len(alts) > 1 {
			primary.Alternatives = alts
		}
		out = append(out, primary)
	}
	return out
}

// ProbeLanguage 探测指定语言。
func (r *Registry) ProbeLanguage(l Language) (Status, bool) {
	for _, st := range r.Detect() {
		if st.Language == l {
			return st, true
		}
	}
	return Status{Language: l, Available: false, Missing: []string{"backend"}, InstallHint: "该语言尚无已注册的调试后端"}, false
}
