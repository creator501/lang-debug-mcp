package jvm

import (
	"math"
	"strconv"
	"strings"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// primitiveNames 是原始类型签名到可读名的映射。
var primitiveNames = map[byte]string{
	tagBoolean: "boolean",
	tagByte:    "byte",
	tagChar:    "char",
	tagShort:   "short",
	tagInt:     "int",
	tagLong:    "long",
	tagFloat:   "float",
	tagDouble:  "double",
	tagVoid:    "void",
}

// typeName 把 JVM 签名转换为可读的短类型名。
func typeName(sig string) string {
	if sig == "" {
		return ""
	}
	if strings.HasPrefix(sig, "[") {
		return typeName(sig[1:]) + "[]"
	}
	if name, ok := primitiveNames[sig[0]]; ok {
		return name
	}
	return shortClassName(className(sig))
}

// className 把 "Lcom/foo/Bar;" 转换为 "com.foo.Bar"。
func className(sig string) string {
	if len(sig) >= 2 && sig[0] == tagObject && sig[len(sig)-1] == ';' {
		return strings.ReplaceAll(sig[1:len(sig)-1], "/", ".")
	}
	return sig
}

// shortClassName 取类名的最后一段，保留内部类的 $ 分隔。
func shortClassName(name string) string {
	if idx := strings.LastIndexByte(name, '.'); idx >= 0 {
		return name[idx+1:]
	}
	return name
}

// sigTag 返回读取槽位值时需要的类型标签。
//
// 签名首字节就是 JVM 类型描述符标签：原始类型用自身字母，对象用 L，数组用 [。
func sigTag(sig string) byte {
	if sig == "" {
		return tagObject
	}
	switch sig[0] {
	case tagByte, tagChar, tagDouble, tagFloat, tagInt, tagLong, tagShort, tagBoolean, tagObject, tagArray:
		return sig[0]
	}
	return tagObject
}

// isPrimitive 报告签名是否为原始类型（含 void）。
func isPrimitive(sig string) bool {
	if sig == "" {
		return false
	}
	_, ok := primitiveNames[sig[0]]
	return ok
}

// isReferenceSig 报告签名是否为引用类型。
func isReferenceSig(sig string) bool {
	return sig != "" && (sig[0] == tagObject || sig[0] == '[')
}

// isStringSig 报告签名是否为 java.lang.String。
func isStringSig(sig string) bool {
	return sig == "Ljava/lang/String;"
}

// isArraySig 报告签名是否为数组。
func isArraySig(sig string) bool {
	return strings.HasPrefix(sig, "[")
}

// elementSig 返回数组元素签名。
func elementSig(sig string) string {
	if isArraySig(sig) {
		return sig[1:]
	}
	return sig
}

// countOfArray 返回数组维数。
func countOfArray(sig string) int {
	n := 0
	for n < len(sig) && sig[n] == '[' {
		n++
	}
	return n
}

// literalValue 把调用方给出的字面量转换为与目标签名匹配的 JDWP 值。
//
// 字符串需要先在目标堆上创建，因此由调用方传入创建回调。
func literalValue(sig, literal string, newString func(string) (uint64, error)) (value, string, error) {
	text := strings.TrimSpace(literal)
	switch sig {
	case "Z":
		switch text {
		case "true", "True":
			return value{tag: tagBoolean, num: 1}, "boolean", nil
		case "false", "False":
			return value{tag: tagBoolean}, "boolean", nil
		}
		return value{}, "", model.New(model.CodeInputInvalid, "目标类型为 boolean，无法写入 %q", literal).
			WithFix("使用 true 或 false")
	case "B", "S", "I", "J":
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return value{}, "", model.New(model.CodeInputInvalid, "目标类型为整数，无法写入 %q", literal).
				WithFix("给出十进制整数字面量，如 42")
		}
		return value{tag: sig[0], num: n}, typeName(sig), nil
	case "C":
		runes := []rune(text)
		if len(runes) != 1 {
			return value{}, "", model.New(model.CodeInputInvalid, "目标类型为 char，需要单个字符，实际 %q", literal)
		}
		return value{tag: tagChar, num: int64(runes[0])}, "char", nil
	case "F", "D":
		f, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return value{}, "", model.New(model.CodeInputInvalid, "目标类型为浮点数，无法写入 %q", literal).
				WithFix("给出十进制小数字面量，如 3.14")
		}
		if sig == "F" {
			return value{tag: tagFloat, num: int64(int32(math.Float32bits(float32(f))))}, "float", nil
		}
		return value{tag: tagDouble, num: int64(math.Float64bits(f))}, "double", nil
	}

	if text == "null" {
		if !isReferenceSig(sig) {
			return value{}, "", model.New(model.CodeInputInvalid, "目标类型 %s 不是引用类型，不能写入 null", typeName(sig))
		}
		return value{tag: referenceTagFor(sig)}, typeName(sig), nil
	}

	if isStringSig(sig) {
		ref, err := newString(literal)
		if err != nil {
			return value{}, "", err
		}
		return value{tag: tagString, ref: ref}, "String", nil
	}
	if isArraySig(sig) {
		return value{}, "", model.New(model.CodeCapUnsupported, "不支持整体替换数组 %s", typeName(sig)).
			WithFix("改为逐元素修改，路径用 item[0] 这类形式；或写入 null")
	}
	return value{}, "", model.New(model.CodeCapUnsupported, "不支持向类型 %s 写入字面量", typeName(sig)).
		WithFix("JDWP 只能直接写入原始类型、String 与 null；包装类型与自定义对象请改其字段")
}

// referenceTagFor 返回引用类型对应的值标签。
func referenceTagFor(sig string) byte {
	switch {
	case isArraySig(sig):
		return tagArray
	case isStringSig(sig):
		return tagString
	}
	return tagObject
}
