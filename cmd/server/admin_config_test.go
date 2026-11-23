package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 管理面鉴权只认管理员密码哈希，与下游接入 key（data/access-keys.json）完全分离，
// 所以 admin.enabled=true 时配置文件里**没有任何密钥字段**也必须能启动。
func TestAdminEnabledWithoutAnyKeyOK(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"admin":{"enabled":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("admin.enabled=true 且无任何密钥字段应允许启动: %v", err)
	}
	if !c.Admin.Enabled {
		t.Fatal("admin.enabled 应为 true")
	}
}

// admin 未开时同样没有密钥字段：合法。
func TestAdminDisabledOK(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"admin":{"enabled":false}}`), 0o600)
	if _, err := Load(fp); err != nil {
		t.Fatalf("admin 关应通过: %v", err)
	}
}

// 旧版的 api_key 键已退役：作为 JSON 未知键被忽略（启动日志会点名），不影响加载。
func TestLegacyAPIKeyIgnored(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"legacy","admin":{"enabled":true}}`), 0o600)
	if _, err := Load(fp); err != nil {
		t.Fatalf("退役的 api_key 应被忽略而不是报错: %v", err)
	}
}

// WP_ADMIN_ENABLED env 覆盖（ParseBool；非法值忽略不报错）。
func TestAdminEnvOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"admin":{"enabled":false}}`), 0o600)

	t.Setenv("WP_ADMIN_ENABLED", "true")
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Admin.Enabled {
		t.Error("WP_ADMIN_ENABLED=true 应开启 admin")
	}

	os.WriteFile(fp, []byte(`{"admin":{"enabled":true}}`), 0o600)
	t.Setenv("WP_ADMIN_ENABLED", "false")
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Admin.Enabled {
		t.Error("WP_ADMIN_ENABLED=false 应覆盖 config 的 true")
	}

	t.Setenv("WP_ADMIN_ENABLED", "yes-please")
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Admin.Enabled {
		t.Error("非法 env 值应被忽略（保持 config 值）")
	}
}
