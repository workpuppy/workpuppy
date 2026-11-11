package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A 组阈值的默认值必须**来自 config.example.json**（不是源码里的字面量）。
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
		{"pool.rotation_backoff_base", c.RotationBackoffBaseDur, 500 * time.Millisecond},
		{"upstream.retry_after_max", c.RetryAfterMaxDur, 2 * time.Hour},
		{"upstream.refresh_io_timeout", c.RefreshIOTimeoutDur, 30 * time.Second},
		{"upstream.device_token_ttl", c.DeviceTokenTTLDur, 5 * time.Minute},
		{"auths.watch_interval", c.AuthsWatchIntervalDur, 5 * time.Second},
		{"state.flush_interval", c.StateFlushIntervalDur, 5 * time.Second},
		{"state.soft_flush_interval", c.StateSoftFlushIntervalDur, 30 * time.Second},
		{"state.credit_flush_interval", c.StateCreditFlushIntervalDur, 30 * time.Second},
		{"risk.waf_window", c.RiskWafWindowDur, 60 * time.Second},
		{"models.catalog_timeout", c.ModelsCatalogTimeoutDur, 5 * time.Second},
		{"models.catalog_refresh_cooldown", c.ModelsCatalogRefreshDur, 5 * time.Minute},
		{"models.catalog_negative_ttl", c.ModelsCatalogNegativeDur, 24 * time.Hour},
		{"upstash.key_ttl", c.UpstashKeyTTLDur, 168 * time.Hour},
	}
	for _, ck := range checks {
		if ck.got != ck.want {
			t.Errorf("%s=%v want %v（应来自 config.example.json）", ck.name, ck.got, ck.want)
		}
	}
	// 非时长类阈值
	if c.Pool.SessionDeadThreshold != 3 || c.Pool.ExpiringWeight != 8 {
		t.Errorf("pool 阈值=%+v want session_dead_threshold=3 expiring_weight=8", c.Pool)
	}
	if c.Risk.WafThreshold != 2 || c.Metrics.Capacity != 512 || c.Debug.DumpMinBytes != 4194304 {
		t.Errorf("risk/metrics/debug=%+v/%+v/%+v", c.Risk, c.Metrics, c.Debug)
	}
	if c.Admin.BodyLimitBytes != 8192 || c.Admin.MinPasswordLength != 8 {
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
	  "admin": {"min_password_length": 12},
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
	if c.Admin.MinPasswordLength != 12 {
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

// 落盘节奏（state.*）的覆盖与钳制：用户可调，但必须落在 [1s, 5m]。
//
// 背景：默认 5s/30s 的分工是「关键状态 5s 上界、软计数与积分日账 30s 合批」；
// 配成 0.1s 只会把磁盘写爆，配成 1h 则让崩溃丢的数据超出可接受范围。
func TestFlushIntervalsClamped(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	body := `{"state":{"flush_interval":"100ms","soft_flush_interval":"500ms","credit_flush_interval":"10m"}}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.StateFlushIntervalDur != time.Second {
		t.Errorf("flush_interval=100ms 应钳到 1s，实际 %v", c.StateFlushIntervalDur)
	}
	if c.StateSoftFlushIntervalDur != time.Second {
		t.Errorf("soft_flush_interval=500ms 应钳到 1s，实际 %v", c.StateSoftFlushIntervalDur)
	}
	if c.StateCreditFlushIntervalDur != 5*time.Minute {
		t.Errorf("credit_flush_interval=10m 应钳到 5m，实际 %v", c.StateCreditFlushIntervalDur)
	}

	// 区间内原样生效；未写的键保留默认值（默认层来自 config.example.json）。
	fp2 := filepath.Join(dir, "c2.json")
	if err := os.WriteFile(fp2, []byte(`{"state":{"soft_flush_interval":"45s"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(fp2)
	if err != nil {
		t.Fatalf("load2: %v", err)
	}
	if c2.StateSoftFlushIntervalDur != 45*time.Second {
		t.Errorf("soft_flush_interval=45s 应原样生效，实际 %v", c2.StateSoftFlushIntervalDur)
	}
	if c2.StateFlushIntervalDur != 5*time.Second || c2.StateCreditFlushIntervalDur != 30*time.Second {
		t.Errorf("未写的落盘键应保留默认 5s/30s，实际 %v/%v",
			c2.StateFlushIntervalDur, c2.StateCreditFlushIntervalDur)
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

// 未知键（拼错 / 旧版键名）要被列出来，而不是静默忽略。
func TestUnknownKeysReported(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	body := `{
	  "listen": ":7863",
	  "schedule": {"checkin_hours": [10, 14], "checkin_time": ["09:30"]},
	  "pool": {"expiring_soon": "24h", "max_in_flight": 4},
	  "admin": {"password_length": 20},
	  "proxies": {"hk": "http://127.0.0.1:7890"},
	  "typo_key": 1
	}`
	if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("未知键不应导致加载失败: %v", err)
	}
	got := c.UnknownKeys()
	want := map[string]bool{
		"schedule.checkin_hours": true,
		"pool.expiring_soon":     true,
		// 拼错/退役的键必须被点名（不做兼容垫片）：admin.password_length 是
		// "随机密码长度"时代的旧名，现已废弃（密码改为页面初始化 + 哈希存储）。
		"admin.password_length": true,
		"typo_key":              true,
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("不应把 %q 判为未知键", k)
		}
		delete(want, k)
	}
	if len(want) > 0 {
		t.Fatalf("漏报未知键：%v（实际 %v）", want, got)
	}
	// proxies 是自由结构（线路名由用户定义），不能误报
	for _, k := range got {
		if strings.HasPrefix(k, "proxies") {
			t.Errorf("proxies 不应参与未知键检查：%q", k)
		}
	}
	// 正常键不误报
	if w := unknownKeysWarning(nil); w != "" {
		t.Errorf("无未知键时不应有告警：%q", w)
	}
}
