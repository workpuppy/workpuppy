package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 新模型：管理面鉴权独立于 api_key（改用管理员密码哈希），所以
// admin.enabled=true 且 api_key 为空**不再**拒绝启动 —— 否则首次初始化
// （用户还没填 api_key）会直接起不来。
func TestAdminEnabledWithoutAPIKeyAllowed(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"api_key":"","admin":{"enabled":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("admin.enabled=true + 空 api_key 应允许启动: %v", err)
	}
	if !c.Admin.Enabled {
		t.Fatal("admin.enabled 应为 true")
	}
}

// admin.enabled=true 且 api_key 非空：同样正常（对照组）。
func TestAdminEnabledWithKeyOK(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"k","admin":{"enabled":true}}`), 0o600)
	if _, err := Load(fp); err != nil {
		t.Fatalf("admin 开 + key 非空应通过: %v", err)
	}
}

// admin 未开时空 api_key 仍合法。
func TestAdminDisabledWithoutKeyOK(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"","admin":{"enabled":false}}`), 0o600)
	if _, err := Load(fp); err != nil {
		t.Fatalf("admin 关 + 空 key 应通过: %v", err)
	}
}

// WP_ADMIN_ENABLED env 覆盖（ParseBool；非法值忽略不报错）。
func TestAdminEnvOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"k","admin":{"enabled":false}}`), 0o600)

	t.Setenv("WP_ADMIN_ENABLED", "true")
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Admin.Enabled {
		t.Error("WP_ADMIN_ENABLED=true 应开启 admin")
	}

	os.WriteFile(fp, []byte(`{"api_key":"k","admin":{"enabled":true}}`), 0o600)
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
