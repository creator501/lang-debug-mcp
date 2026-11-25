package nodejs

import (
	"path/filepath"
	"runtime"
	"time"

	"github.com/langdebug/lang-debug-mcp/internal/backend"
)

// launchSpecForTest 构造指向测试夹具的启动参数。
func launchSpecForTest(port int) backend.LaunchSpec {
	_, file, _, _ := runtime.Caller(0)
	// internal/backend/nodejs -> 仓库根 -> testdata/fixture/app.js
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	fixture := filepath.Join(root, "testdata", "fixture", "app.js")
	return backend.LaunchSpec{
		Language: backend.NodeJS,
		Mode:     "launch",
		Command:  []string{"node", fixture},
		Timeout:  25 * time.Second,
	}
}
