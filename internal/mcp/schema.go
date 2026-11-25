package mcp

// Object 构造一个 JSON Schema 对象描述。
func Object(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	schema := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// Str 构造一个字符串属性描述。
func Str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// Int 构造一个整数属性描述。
func Int(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

// Bool 构造一个布尔属性描述。
func Bool(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

// Enum 构造一个枚举字符串属性描述。
func Enum(desc string, values ...string) map[string]any {
	return map[string]any{
		"type":        "string",
		"description": desc,
		"enum":        values,
	}
}

// Arr 构造一个数组属性描述。
func Arr(desc string, items map[string]any) map[string]any {
	return map[string]any{
		"type":        "array",
		"description": desc,
		"items":       items,
	}
}
