// Package configdata 持有**编译进二进制的默认配置**（config.example.json）。
//
// 为什么把默认值放进这份文件，而不是散落在 Go 源码里：
//   - 默认值只应有**一处**权威来源。以前 listen / 超时 / 池阈值 / 排程时刻等几十个
//     默认值硬编码在 cmd/server.Default() 里，用户读 README 也拼不出"完整的默认配置"，
//     改一个默认值要翻代码。
//   - 现在：程序启动时用这份内嵌 JSON 反序列化出默认配置，再把 config.json 叠加在上面。
//     用户在 `config.example.json` 里看到的就是**真实生效的默认值**。
//   - 仓库根目录的 `config.example.json` 是这份文件的**对外副本**（给人查看/复制用），
//     由测试强制保持一致（见 configdata_test.go），如需同步执行：
//     go run ./cmd/config-default -write
package configdata

import (
	_ "embed"
	"log"
	"os"
)

// DefaultName 默认配置文件名（仓库根目录的对外副本同名）。
const DefaultName = "config.example.json"

//go:embed config.example.json
var defaultConfig []byte

// Bytes 返回内嵌默认配置的原始字节。调用方负责解析（可能带 BOM，由调用方裁剪）。
//
// 返回的是底层切片，调用方只读使用即可（json.Unmarshal 不会修改入参）。
func Bytes() []byte { return defaultConfig }

// EmbeddedSource 表示「用的是编译进二进制的默认配置」。
const EmbeddedSource = "builtin:" + DefaultName

// Resolve 读取用户配置文件（通常是 config.json）：
//
//   - 文件存在 → 返回其字节，source 为路径；
//   - 文件不存在，或该路径其实是个**目录**（docker compose 挂载不存在的宿主机文件时
//     Docker 会创建同名目录）→ 打 WARNING 并返回**内嵌默认配置**，source 为 EmbeddedSource。
//
// 「属性级」的兜底不在这里做：读取到的字节会被叠加在内嵌默认配置之上（见 cmd/server.Load），
// 因此 config.json 里没写的键、以及写成 null 的键，最终都取 config.example.json 里的值。
//
// 读取出「文件存在但没权限」这类真错误时返回错误，不做静默回退——那说明环境有问题，
// 换配置只会更难查。
func Resolve(path string) (raw []byte, source string, err error) {
	if path == "" {
		path = "config.json"
	}
	b, readErr := os.ReadFile(path)
	if readErr == nil {
		return b, path, nil
	}
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
		log.Printf("WARNING: 找不到配置文件 %s，改用内置默认配置（%s）", path, EmbeddedSource)
	}
	log.Printf("WARNING: 下面所有配置项都取 %s 的值；正式运行请复制一份 %s 并按需修改（尤其 api_key）",
		DefaultName, path)
	return defaultConfig, EmbeddedSource, nil
}
