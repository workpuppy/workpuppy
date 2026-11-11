package configdata

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 内嵌默认配置必须是一份**能解析的完整配置**：它是所有默认值的权威来源，坏了等于程序起不来。
func TestEmbeddedDefaultIsValidJSON(t *testing.T) {
	raw := Bytes()
	if len(raw) == 0 {
		t.Fatal("内嵌默认配置为空")
	}
	if bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("内嵌默认配置不应带 BOM")
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("内嵌默认配置不是合法 JSON: %v", err)
	}
	// 关键段必须存在：默认值就靠它们。
	for _, key := range []string{"listen", "auth_dir", "state_file", "schedule", "upstream", "pool", "session_sticky", "cooldown"} {
		if _, ok := m[key]; !ok {
			t.Errorf("内嵌默认配置缺少 %q 段", key)
		}
	}
}

// 仓库根目录的 config.default.json 是内嵌文件的对外副本：两者必须逐字节一致，
// 否则用户照着根目录那份复制，跑起来却不是他看到的默认值。
//
// 不一致时执行：go run ./cmd/config-default -write
func TestRootDefaultMatchesEmbedded(t *testing.T) {
	// 本文件在 internal/configdata/，仓库根是上两级。
	root := filepath.Join("..", "..", DefaultName)
	got, err := os.ReadFile(root)
	if err != nil {
		t.Fatalf("读取根目录 %s: %v（执行 go run ./cmd/config-default -write 生成）", DefaultName, err)
	}
	if !bytes.Equal(got, Bytes()) {
		t.Fatalf("根目录 %s 与内嵌副本不一致：执行 `go run ./cmd/config-default -write` 同步", DefaultName)
	}
}

// 真实存在的配置文件优先，原样返回。
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

// 文件缺失 → 用内置默认配置（不再是「找磁盘上的 config.default.json」）。
func TestResolveMissingFallsBackToBuiltin(t *testing.T) {
	dir := t.TempDir()
	raw, src, err := Resolve(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if src != EmbeddedSource {
		t.Errorf("source=%q want %q", src, EmbeddedSource)
	}
	if !bytes.Equal(raw, Bytes()) {
		t.Error("应返回内嵌默认配置字节")
	}
}

// 路径是目录（docker compose 挂载不存在的宿主机文件）也按缺失处理。
func TestResolveDirectoryPathFallsBackToBuiltin(t *testing.T) {
	dir := t.TempDir()
	asDir := filepath.Join(dir, "config.json")
	if err := os.Mkdir(asDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, src, err := Resolve(asDir)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if src != EmbeddedSource || !bytes.Equal(raw, Bytes()) {
		t.Fatalf("目录路径应回退到内置默认：source=%q", src)
	}
}
