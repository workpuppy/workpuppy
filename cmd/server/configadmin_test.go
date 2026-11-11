package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"wp/internal/adminauth"
	"wp/internal/configdata"
	"wp/internal/server"
)

func TestEnsureConfigFileCreatesFromExample(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "config.json")

	created, err := ensureConfigFile(path)
	if err != nil {
		t.Fatalf("ensureConfigFile: %v", err)
	}
	if !created {
		t.Fatal("首次调用应创建文件")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, configdata.Bytes()) {
		t.Fatal("生成内容应等于内嵌 config.default.json")
	}
	if st, _ := os.Stat(path); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("权限 = %v，want 0600（文件含 api_key 与密码哈希）", st.Mode().Perm())
	}

	// 第二次不得覆盖用户文件。
	if err := os.WriteFile(path, []byte(`{"listen":":9999"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	created, err = ensureConfigFile(path)
	if err != nil || created {
		t.Fatalf("已存在时不应重建：created=%v err=%v", created, err)
	}
	got, _ = os.ReadFile(path)
	if !bytes.Contains(got, []byte(":9999")) {
		t.Fatal("已有配置被覆盖了")
	}
}

func TestEnsureConfigFileRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureConfigFile(path); err == nil {
		t.Fatal("路径是目录时应报错（常见于挂载了同名目录）")
	}
}

func TestWriteFileAtomicReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "new" {
		t.Fatalf("内容 = %q", got)
	}
	// 不应残留临时文件
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "x.json" {
			t.Errorf("残留临时文件：%s", e.Name())
		}
	}
}

func newTestConfigStore(t *testing.T) (*configStore, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "config.json")
	if _, err := ensureConfigFile(path); err != nil {
		t.Fatal(err)
	}
	return newConfigStore(path), path
}

func TestConfigStoreSnapshot(t *testing.T) {
	s, path := newTestConfigStore(t)
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Path != path || !snap.Writable {
		t.Fatalf("meta = %+v", snap)
	}
	if _, ok := snap.Values["admin"]; !ok {
		t.Fatal("values 缺 admin 段")
	}
	if admin, _ := snap.Values["admin"].(map[string]any); admin != nil {
		if _, ok := admin["password"]; ok {
			t.Fatal("密码哈希绝不能下发给前端")
		}
	}
	if snap.Schema == nil {
		t.Fatal("schema 未下发")
	}
}

func TestConfigStoreSaveAndReset(t *testing.T) {
	s, path := newTestConfigStore(t)

	snap, fieldErrs, err := s.Save(map[string]any{
		"schedule": map[string]any{"checkin_time": []any{"07:10", "19:20"}},
		"admin":    map[string]any{"body_limit_bytes": 16384.0},
	})
	if err != nil || len(fieldErrs) != 0 {
		t.Fatalf("保存应成功：err=%v fieldErrs=%v", err, fieldErrs)
	}
	if got := snap.Values["schedule"].(map[string]any)["checkin_time"].([]any); len(got) != 2 || got[0] != "07:10" {
		t.Fatalf("未写回：%v", got)
	}

	raw, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("落盘文件不是合法 JSON：%v", err)
	}
	if doc["admin"].(map[string]any)["body_limit_bytes"].(float64) != 16384 {
		t.Fatal("文件里没有新值")
	}

	// 单字段恢复默认
	snap, _, err = s.Reset(server.ConfigResetRequest{Scope: "fields", Paths: []string{"schedule.checkin_time"}})
	if err != nil {
		t.Fatal(err)
	}
	got := snap.Values["schedule"].(map[string]any)["checkin_time"].([]any)
	if len(got) != 2 || got[0] != "09:00" {
		t.Fatalf("恢复默认后 = %v（want 09:00,21:00）", got)
	}

	// 单字段非法：不写盘、返回字段错误
	before, _ := os.ReadFile(path)
	_, fieldErrs, err = s.Save(map[string]any{"pool": map[string]any{"max_in_flight": "abc"}})
	if err != nil {
		t.Fatalf("校验失败不应是 500：%v", err)
	}
	if len(fieldErrs) == 0 || fieldErrs[0].Path != "pool.max_in_flight" {
		t.Fatalf("应返回字段级错误：%v", fieldErrs)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("校验失败时不应写盘")
	}
}

func TestConfigStoreSaveRejectsUnknownKey(t *testing.T) {
	s, _ := newTestConfigStore(t)
	_, fieldErrs, err := s.Save(map[string]any{"pool": map[string]any{"no_such": 1.0}})
	if err != nil {
		t.Fatal(err)
	}
	if len(fieldErrs) == 0 || fieldErrs[0].Path != "pool.no_such" {
		t.Fatalf("未知键应报错：%v", fieldErrs)
	}
}

func TestConfigStoreResetAllKeepsAdmin(t *testing.T) {
	s, path := newTestConfigStore(t)
	hash, err := adminauth.HashPassword("init-password-789")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPassword(hash, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Reset(server.ConfigResetRequest{Scope: "all"}); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	admin := snap.Values["admin"].(map[string]any)
	if enabled, _ := admin["enabled"].(bool); !enabled {
		t.Fatal("全部恢复默认不应关闭管理面（会把自己锁在门外）")
	}
	raw, _ := os.ReadFile(path)
	if !bytes.Contains(raw, []byte("sha256x10$")) {
		t.Fatal("全部恢复默认不应清掉管理密码哈希")
	}
}
