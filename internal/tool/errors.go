package tool

import (
	"errors"

	"github.com/langdebug/lang-debug-mcp/internal/model"
)

// asModelError 提取统一错误，便于渲染带 fix 的失败结果。
func asModelError(err error, target **model.Error) bool {
	if err == nil {
		return false
	}
	var mErr *model.Error
	if errors.As(err, &mErr) {
		*target = mErr
		return true
	}
	return false
}
