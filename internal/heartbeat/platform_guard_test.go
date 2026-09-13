package heartbeat

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenHeartbeatImports 是心跳实现绝对禁止依赖的平台（闲鱼）包前缀。
// 心跳只写自有数据库，不得触达消息、mtop、cookie 刷新、登录或 WebSocket 等任何平台能力。
var forbiddenHeartbeatImports = []string{
	"xianyu-go/internal/xianyu",
	"xianyu-go/internal/browser",
	"xianyu-go/internal/engine",
	"xianyu-go/internal/adapter",
	"xianyu-go/internal/automation",
	"mtop",
	"websocket",
	"playwright",
}

// TestHeartbeatFilesImportNoPlatformPackages 静态断言心跳相关文件不 import 任何平台包。
// 覆盖 internal/heartbeat 包全部 .go 文件与 db 层心跳仓储文件，从构造上证明心跳零平台调用。
func TestHeartbeatFilesImportNoPlatformPackages(t *testing.T) {
	// files 是需要扫描的全部心跳相关源码路径。
	files := heartbeatSourceFiles(t)
	for // path 表示当前遍历过程中的源码路径。
	_, path := range files {
		// fset 是本次解析的位置集合。
		fset := token.NewFileSet()
		// file、err 分别是本次解析得到的 AST 与解析错误。
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("解析 %s 失败: %v", path, err)
		}
		// imp 表示当前遍历过程中的导入声明。
		for _, imp := range file.Imports {
			// pkg 是去除了引号与别名后的导入路径。
			pkg := strings.Trim(imp.Path.Value, `"`)
			for // forbidden 表示当前遍历过程中的禁用平台包前缀。
			_, forbidden := range forbiddenHeartbeatImports {
				if strings.Contains(pkg, forbidden) {
					t.Fatalf("%s 禁止 import 平台包 %q（心跳不得发起任何平台请求）", path, pkg)
				}
			}
		}
	}
}

// heartbeatSourceFiles 收集心跳包目录与 db 层心跳仓储的全部 .go 文件（含测试与非测试）。
func heartbeatSourceFiles(t *testing.T) []string {
	// roots 是待扫描的根目录：当前包目录与上层 db 包目录。
	roots := []string{".", "../db"}
	// collected 汇总所有待校验的源码路径。
	collected := make([]string, 0)
	for // root 表示当前遍历过程中的扫描根目录。
	_, root := range roots {
		// entries 是当前目录下的全部条目。
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatalf("读取目录 %s 失败: %v", root, err)
		}
		for // entry 表示当前遍历过程中的目录条目。
		_, entry := range entries {
			// name 是文件名；只保留心跳相关的 .go 源文件。
			name := entry.Name()
			if !strings.HasSuffix(name, ".go") {
				continue
			}
			if !strings.Contains(name, "heartbeat") {
				continue
			}
			// full 是归一化后的绝对源码路径。
			full, absErr := filepath.Abs(filepath.Join(root, name))
			if absErr != nil {
				t.Fatalf("归一化路径 %s 失败: %v", name, absErr)
			}
			collected = append(collected, full)
		}
	}
	if len(collected) == 0 {
		t.Fatal("未扫描到任何心跳相关源文件，断言失去意义")
	}
	return collected
}
