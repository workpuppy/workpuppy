package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A 组阈值的默认值必须**来自 config.default.json**（不是源码里的字面量）。
func TestTuningsDefaultsComeFromDefaultConfig(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if err := c.wireTunings(); err != nil {
		t.Fatalf("wireTunings: %v", err)
	}
	checks := []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"cooldown.not_found", c.CooldownNotFoundDur, 60 * time.Second},
		{"cooldown.waf_base", c.CooldownWafBaseDur, 60 * time.Second},
		{"cooldown.upstream_restriction", c.CooldownUpstreamRestrictionDur, 45 * time.Minute},
		{"pool.min_pick_gap", c.MinPickGapDur, 100 * time.Millisecond},
		{"pool.rotate_backoff_base", c.RotateBackoffBaseDur, 500 * time.Millisecond},
		{"upstream.retry_after_max", c.RetryAfterMaxDur, 2 * time.Hour},
		{"upstream.refresh_io_timeout", c.RefreshIOTimeoutDur, 30 * time.Second},
		{"upstream.device_token_ttl", c.DeviceTokenTTLDur, 5 * time.Minute},
		{"auths.watch_interval", c.AuthsWatchIntervalDur, 5 * time.Second},
		{"state.flush_interval", c.StateFlushIntervalDur, 5 * time.Second},
		{"risk.waf_window", c.RiskWafWindowDur, 60 * time.Second},
		{"models.catalog_timeout", c.ModelsCatalogTimeoutDur, 5 * time.Second},
		{"models.catalog_refresh_cooldown", c.ModelsCatalogRefreshDur, 5 * time.Minute},
		{"models.catalog_negative_ttl", c.ModelsCatalogNegativeDur, 24 * time.Hour},
		{"upstash.key_ttl", c.UpstashKeyTTLDur, 168 * time.Hour},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s=%v want %v（应来自 config.default.json）", ck.name, ck.got, ck.want)
		}
	}
	// 非时长类阈值
	if c.Pool.SessionDeadThreshold != 3 || c.Pool.ExpiringWeight != 8 {
		t.Errorf("pool 阈值=%+v want session_dead_threshold=3 expiring_weight=8", c.Pool)
	}
	if c.Risk.WafThreshold != 2 || c.Metrics.Capacity != 512 || c.Debug.DumpMinBytes != 4194304 {
		t.Errorf("risk/metrics/debug=%+v/%+v/%+v", c.Risk, c.Metrics, c.Debug)
	}
	if c.Admin.BodyLimitBytes != 8192 || c.Admin.PasswordLength != 20 || c.Admin.MinPasswordLength != 8 {
		t.Errorf("admin=%+v want 8192/20/8", c.Admin)
	}
	if c.Models.DefaultContextWindow != 1000000 || c.Upstash.WriteConcurrency != 8 {
		t.Errorf("models/upstash=%+v/%+v", c.Models, c.Upstash)
	}
	if c.Upstream.GzipThresholdBytes != 65536 || c.Upstream.DeviceTokenMaxLen != 1024 {
		t.Errorf("upstream=%+v want gzip=65536 device_token_max_len=1024", c.Upstream)
	}
}

// 用户在 config.json 里写了的阈值必须生效（覆盖默认）。
func TestTuningsUserOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	body := `{
	  "pool": {"min_pick_gap": "250ms", "session_dead_threshold": 5, "expiring_weight": 12},
	  "cooldown": {"not_found": "5s"},
	  "risk": {"waf_window": "2m", "waf_threshold": 7},
	  "admin": {"password_length": 24, "min_password_length": 12},
	  "models": {"default_context_window": 200000, "catalog_timeout": "9s"},
	  "upstash": {"key_ttl": "24h", "write_concurrency": 4}
	}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.MinPickGapDur != 250*time.Millisecond || c.Pool.SessionDeadThreshold != 5 {
		t.Errorf("pool 覆盖未生效：min_pick_gap=%v session_dead_threshold=%d",
			c.MinPickGapDur, c.Pool.SessionDeadThreshold)
	}
	if c.Pool.ExpiringWeight != 12 || c.CooldownNotFoundDur != 5*time.Second {
		t.Errorf("pool/cooldown 覆盖未生效：expiring_weight=%v not_found=%v",
			c.Pool.ExpiringWeight, c.CooldownNotFoundDur)
	}
	if c.RiskWafWindowDur != 2*time.Minute || c.Risk.WafThreshold != 7 {
		t.Errorf("risk 覆盖未生效：window=%v threshold=%d", c.RiskWafWindowDur, c.Risk.WafThreshold)
	}
	if c.Admin.PasswordLength != 24 || c.Admin.MinPasswordLength != 12 {
		t.Errorf("admin 覆盖未生效：%+v", c.Admin)
	}
	if c.Models.DefaultContextWindow != 200000 || c.ModelsCatalogTimeoutDur != 9*time.Second {
		t.Errorf("models 覆盖未生效：%d/%v", c.Models.DefaultContextWindow, c.ModelsCatalogTimeoutDur)
	}
	if c.UpstashKeyTTLDur != 24*time.Hour || c.Upstash.WriteConcurrency != 4 {
		t.Errorf("upstash 覆盖未生效：%v/%d", c.UpstashKeyTTLDur, c.Upstash.WriteConcurrency)
	}
	// 没写的键仍取默认
	if c.AuthsWatchIntervalDur != 5*time.Second {
		t.Errorf("未写的键应保留默认：auths.watch_interval=%v", c.AuthsWatchIntervalDur)
	}
}

// 非法时长在加载期报错，并点名是哪一项。
func TestTuningsInvalidDurationRejected(t *testing.T) {
	cases := map[string]string{
		"pool.min_pick_gap":             `{"pool":{"min_pick_gap":"oops"}}`,
		"risk.waf_window":               `{"risk":{"waf_window":"-1s"}}`,
		"models.catalog_timeout":        `{"models":{"catalog_timeout":"abc"}}`,
		"upstream.retry_after_max":      `{"upstream":{"retry_after_max":"x"}}`,
		"cooldown.upstream_restriction": `{"cooldown":{"upstream_restriction":"??"}}`,
	}
	for wantKey, body := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(fp)
		if err == nil {
			t.Errorf("%s: 期望报错", wantKey)
			continue
		}
		if !contains(err.Error(), wantKey) {
			t.Errorf("错误信息应点名 %s：%v", wantKey, err)
		}
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// 改名后的键：qrcode_ttl / expiring_soon_window / default_context_window 生效，
// 旧的 admin.login_ttl 不再被读取（避免"看起来配了其实没生效"）。
func TestRenamedKeysEffective(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	body := `{
	  "qrcode_ttl": "7m",
	  "pool": {"expiring_soon_window": "48h"},
	  "models": {"default_context_window": 64000}
	}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.QRCodeTTLDur != 7*time.Minute {
		t.Errorf("qrcode_ttl=%v want 7m", c.QRCodeTTLDur)
	}
	if c.ExpiringSoonDur != 48*time.Hour {
		t.Errorf("expiring_soon_window=%v want 48h", c.ExpiringSoonDur)
	}
	if c.Models.DefaultContextWindow != 64000 {
		t.Errorf("default_context_window=%d want 64000", c.Models.DefaultContextWindow)
	}
	if len(c.LegacyKeys()) != 0 {
		t.Errorf("用了新键不应记 legacy：%v", c.LegacyKeys())
	}

	// 旧的 admin.login_ttl 已被删除：写了也不生效（回落到默认 15m）。
	fp2 := filepath.Join(dir, "old.json")
	if err := os.WriteFile(fp2, []byte(`{"admin":{"login_ttl":"1m"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(fp2)
	if err != nil {
		t.Fatalf("load old: %v", err)
	}
	if c2.QRCodeTTLDur != 15*time.Minute {
		t.Errorf("旧的 admin.login_ttl 不应再生效：qrcode_ttl=%v want 默认 15m", c2.QRCodeTTLDur)
	}
}

// 旧键仍被接受（打 WARN 提示迁移），且新键优先。
func TestLegacyKeysStillAcceptedWithWarning(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	body := `{
	  "pool": {"expiring_soon": "24h"},
	  "models": {"default_context": 32000}
	}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.ExpiringSoonDur != 24*time.Hour {
		t.Errorf("旧键 expiring_soon 应被接受：%v", c.ExpiringSoonDur)
	}
	if c.Models.DefaultContextWindow != 32000 {
		t.Errorf("旧键 default_context 应被接受：%d", c.Models.DefaultContextWindow)
	}
	keys := c.LegacyKeys()
	if len(keys) != 2 {
		t.Fatalf("应记录 2 个旧键，实际 %v", keys)
	}

	// 新旧并存 → 新键优先，且不记 legacy
	fp2 := filepath.Join(dir, "both.json")
	body2 := `{"pool":{"expiring_soon":"24h","expiring_soon_window":"12h"}}`
	if err := os.WriteFile(fp2, []byte(body2), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(fp2)
	if err != nil {
		t.Fatalf("load both: %v", err)
	}
	if c2.ExpiringSoonDur != 12*time.Hour {
		t.Errorf("新键应优先：%v want 12h", c2.ExpiringSoonDur)
	}
	if len(c2.LegacyKeys()) != 0 {
		t.Errorf("新旧并存时不该记 legacy：%v", c2.LegacyKeys())
	}
}
