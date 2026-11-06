// Package configdata 持有**编译进二进制的示例配置**（config.example.json）。
//
// 为什么把默认值放进这份文件，而不是散落在 Go 源码里：
//   - 默认值只应有**一处**权威来源。以前 listen / 超时 / 池阈值 / 排程时刻等几十个
//     默认值硬编码在 cmd/server.Default() 里，用户读 README 也拼不出"完整的默认配置"，
//     改一个默认值要翻代码。
//   - 现在：程序启动时用这份内嵌 JSON 反序列化出默认配置；`config.json` 缺失时也回退到它
//     （见 Resolve）。用户在 `config.example.json` 里看到的就是**真实生效的默认值**。
//   - 仓库根目录的 `config.example.json` 是这份文件的**对外副本**（给人复制用），
//     由测试强制保持一致（见 configdata_test.go），如需同步执行：
//     go run ./cmd/example-config -write
package configdata

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
)

// ExampleName 对外副本的文件名（用户复制成 config.json 用）。
const ExampleName = "config.example.json"

//go:embed config.example.json
var example []byte

// Example 返回内嵌示例配置的原始字节。调用方负责解析（可能带 BOM，由调用方裁剪）。
//
// 返回的是底层切片，调用方只读使用即可（json.Unmarshal 不会修改入参）。
func Example() []byte { return example }

// EmbeddedSource 表示「用的是编译进二进制的示例配置」。
const EmbeddedSource = "builtin:" + ExampleName

// Resolve 读取配置文件，失败时逐级回退，并且**一定会**返回可用的字节：
//
//  1. path（通常 ./config.json）：存在则直接用；
//  2. 不存在 → WARNING，尝试同目录 / 工作目录 / 可执行文件目录里的 config.example.json；
//  3. 仍然没有 → WARNING，用编译期内嵌的示例配置。
//
// 第二个返回值是来源说明（日志与排障用）。读取出「文件存在但没权限」这类真错误时返回错误，
// 不做回退——那说明用户配了东西但环境有问题，静默换配置只会更难查。
func Resolve(path string) (raw []byte, source string, err error) {
	if path == "" {
		path = "config.json"
	}
	b, readErr := os.ReadFile(path)
	if readErr == nil {
		return b, path, nil
	}
	// 目录也算「没有配置文件」：docker compose 的 `- ./config.json:/app/config.json:ro`
	// 在宿主机文件不存在时会**创建一个同名目录**，os.ReadFile 报 EISDIR。
	// 这对用户极难自查，按缺失处理并明确提示，比抛一句 "is a directory" 有用得多。
	isDir := false
	if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
		isDir = true
	}
	if !os.IsNotExist(readErr) && !isDir {
		return nil, "", readErr
	}

	if isDir {
		log.Printf("WARNING: %s 是个**目录**而不是文件（常见原因：docker compose 挂载 "+
			"`./config.json:/app/config.json:ro` 时宿主机文件不存在），按缺失处理", path)
	} else {
		log.Printf("WARNING: 找不到配置文件 %s，回退到示例配置作为默认值", path)
	}
	if alt := findExample(path); alt != "" {
		if b, err := os.ReadFile(alt); err == nil {
			log.Printf("WARNING: 已改用 %s —— 正式运行前请把它复制成 %s 并按需修改（尤其 api_key）", alt, path)
			return b, alt, nil
		}
	}
	log.Printf("WARNING: 未找到 %s，使用**编译进二进制的内置默认配置**（%s）", ExampleName, EmbeddedSource)
	return example, EmbeddedSource, nil
}

// exampleSearchDirs 返回「去哪儿找 config.example.json」的候选目录（测试可替换）。
//
// 覆盖三种常见部署：容器（cwd=/app，示例随镜像）、本地 go run（cwd=仓库根）、
// 单文件部署（示例与二进制同目录）。
var exampleSearchDirs = func(forPath string) []string {
	dirs := make([]string, 0, 3)
	if d := filepath.Dir(forPath); d != "" {
		dirs = append(dirs, d)
	}
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	return dirs
}

// findExample 在候选目录里找 config.example.json；没有返回空串。
func findExample(forPath string) string {
	for _, d := range exampleSearchDirs(forPath) {
		p := filepath.Join(d, ExampleName)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}
