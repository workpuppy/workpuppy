package configdata

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 内嵌示例必须是一份**能解析的完整配置**：它是所有默认值的权威来源，坏了等于程序起不来。
func TestEmbeddedExampleIsValidJSON(t *testing.T) {
	raw := Example()
	if len(raw) == 0 {
		t.Fatal("内嵌示例为空")
	}
	if bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("内嵌示例不应带 BOM")
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("内嵌示例不是合法 JSON: %v", err)
	}
	// 关键段必须存在：默认值就靠它们。
	for _, key := range []string{"listen", "auth_dir", "state_file", "schedule", "upstream", "pool", "session_sticky", "cooldown"} {
		if _, ok := m[key]; !ok {
			t.Errorf("内嵌示例缺少 %q 段", key)
		}
	}
}

// 仓库根目录的 config.example.json 是内嵌文件的对外副本：两者必须逐字节一致，
// 否则用户照着根目录那份复制，跑起来却不是他看到的默认值。
//
// 不一致时执行：go run ./cmd/example-config -write
func TestRootExampleMatchesEmbedded(t *testing.T) {
	// 本文件在 internal/configdata/，仓库根是上两级。
	root := filepath.Join("..", "..", ExampleName)
	got, err := os.ReadFile(root)
	if err != nil {
		t.Fatalf("读取根目录 %s: %v（执行 go run ./cmd/example-config -write 生成）", ExampleName, err)
	}
	if !bytes.Equal(got, Example()) {
		t.Fatalf("根目录 %s 与内嵌副本不一致：执行 `go run ./cmd/example-config -write` 同步", ExampleName)
	}
}

// 缺文件时回退到示例（存在则用磁盘上的示例，不存在则用内嵌），且不返回错误。
func TestResolveFallsBackToExample(t *testing.T) {
	dir := t.TempDir()
	ex := filepath.Join(dir, ExampleName)
	if err := os.WriteFile(ex, []byte(`{"listen":":9999"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, src, err := Resolve(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if src != ex {
		t.Errorf("source=%q want %q", src, ex)
	}
	if !bytes.Contains(raw, []byte(":9999")) {
		t.Errorf("应读到磁盘上的示例：%s", raw)
	}
}

// 目录里连示例都没有 → 用内嵌副本（仍然可用，不报错）。
func TestResolveFallsBackToEmbedded(t *testing.T) {
	dir := t.TempDir() // 空目录
	old := exampleSearchDirs
	exampleSearchDirs = func(string) []string { return []string{dir} } // 只在空目录找
	t.Cleanup(func() { exampleSearchDirs = old })

	raw, src, err := Resolve(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if src != EmbeddedSource {
		t.Errorf("source=%q want %q", src, EmbeddedSource)
	}
	if !bytes.Equal(raw, Example()) {
		t.Error("应返回内嵌示例字节")
	}
}

// 真实存在的配置文件优先，不回退。
func TestResolvePrefersExistingFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(`{"listen":":1234"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, src, err := Resolve(p)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if src != p || !bytes.Contains(raw, []byte(":1234")) {
		t.Fatalf("source=%q raw=%s", src, raw)
	}
}
