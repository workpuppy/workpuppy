package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.Listen != ":7863" {
		t.Errorf("listen=%s", c.Listen)
	}
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.SoftRateDur.Seconds() != 600 {
		t.Errorf("soft=%v want 600s", c.SoftRateDur)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9999" || c.APIKey != "k" {
		t.Errorf("c=%+v", c)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("WB2A_LISTEN", ":7777")
	t.Setenv("WB2A_API_KEY", "envkey")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":7777" || c.APIKey != "envkey" {
		t.Errorf("c=%+v", c)
	}
}

func TestBadDuration(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"not-a-duration"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad duration")
	}
}

func TestHardCreditKeyIgnored(t *testing.T) {
	// 退役的 hard_credit 键作为 JSON 未知字段被自然忽略，不报错。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"hard_credit":"not-a-duration","soft_rate":"30s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("hard_credit must be ignored (not validated): %v", err)
	}
	if c.SoftRateDur.Seconds() != 30 {
		t.Errorf("soft_rate=%v want 30s", c.SoftRateDur)
	}
}

func TestNewPoolConfigDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Pool.MaxInFlight != 3 {
		t.Errorf("max_in_flight=%d want 3", c.Pool.MaxInFlight)
	}
	if c.Pool.MaxInFlightGlobal != 2 {
		t.Errorf("max_in_flight_global=%d want 2 (WAF P1-1 global 档默认)", c.Pool.MaxInFlightGlobal)
	}
	if c.Pool.BreakerThreshold != 3 {
		t.Errorf("breaker_threshold=%d want 3", c.Pool.BreakerThreshold)
	}
	if c.BreakerCooldownDur.Minutes() != 30 {
		t.Errorf("breaker_cooldown=%v want 30m", c.BreakerCooldownDur)
	}
	if c.BreakerCooldownMaxD.Hours() != 6 {
		t.Errorf("breaker_cooldown_max=%v want 6h", c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.5 || c.Pool.IdleWeightMax != 5.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if c.SoftRateMaxDur.Hours() != 2 {
		t.Errorf("soft_rate_max=%v want 2h", c.SoftRateMaxDur)
	}
	if !c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want true")
	}
	if c.SessionTTL.Minutes() != 30 || c.SessionGCInterval.Minutes() != 5 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		t.Errorf("upstash default should be empty: %+v", c.Upstash)
	}
}

func TestPoolConfigParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{
		"upstash":{"url":"https://foo.upstash.io","token":"tok"},
		"pool":{
			"max_in_flight":5,
			"max_in_flight_global":4,
			"breaker_threshold":4,
			"breaker_cooldown":"10m",
			"breaker_cooldown_max":"2h",
			"idle_weight_per_hour":0.7,
			"idle_weight_max":8.0
		},
		"session_sticky":{"enabled":false,"ttl":"1h","gc_interval":"2m"}
	}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstash.URL != "https://foo.upstash.io" || c.Upstash.Token != "tok" {
		t.Errorf("upstash=%+v", c.Upstash)
	}
	if c.Pool.MaxInFlight != 5 || c.Pool.BreakerThreshold != 4 {
		t.Errorf("pool=%+v", c.Pool)
	}
	if c.Pool.MaxInFlightGlobal != 4 {
		t.Errorf("max_in_flight_global=%d want 4 (config 覆盖默认)", c.Pool.MaxInFlightGlobal)
	}
	if c.BreakerCooldownDur.Minutes() != 10 || c.BreakerCooldownMaxD.Hours() != 2 {
		t.Errorf("breaker durations=%v/%v", c.BreakerCooldownDur, c.BreakerCooldownMaxD)
	}
	if c.Pool.IdleWeightPerHour != 0.7 || c.Pool.IdleWeightMax != 8.0 {
		t.Errorf("idle weights=%v/%v", c.Pool.IdleWeightPerHour, c.Pool.IdleWeightMax)
	}
	if c.SessionSticky.Enabled {
		t.Error("session_sticky.enabled want false from file")
	}
	if c.SessionTTL.Hours() != 1 || c.SessionGCInterval.Minutes() != 2 {
		t.Errorf("session durations=%v/%v", c.SessionTTL, c.SessionGCInterval)
	}
}

// 定时任务的节奏设置：spread_window（账号之间）与 intra_request_jitter_*（账号内连打）。
func TestSpreadWindowAndJitterParsed(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"spread_window":"45s","intra_request_jitter_min":"2s","intra_request_jitter_max":"5s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.SpreadWindowDur != 45*time.Second {
		t.Errorf("spread_window=%v want 45s", c.SpreadWindowDur)
	}
	if c.IntraJitterMinD != 2*time.Second || c.IntraJitterMaxD != 5*time.Second {
		t.Errorf("jitter=%v~%v want 2s~5s", c.IntraJitterMinD, c.IntraJitterMaxD)
	}
}

// 缺省值：spread_window=15m、jitter=1s~3s（用户要求的默认区间）。
func TestSpreadAndJitterDefaults(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.SpreadWindowDur != 15*time.Minute {
		t.Errorf("spread_window 默认 = %v want 15m", c.SpreadWindowDur)
	}
	if c.IntraJitterMinD != time.Second || c.IntraJitterMaxD != 3*time.Second {
		t.Errorf("jitter 默认 = %v~%v want 1s~3s", c.IntraJitterMinD, c.IntraJitterMaxD)
	}
}

// "0" 是合法值（关闭），不是"回落默认"。
func TestSpreadAndJitterExplicitZero(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"spread_window":"0","intra_request_jitter_min":"0","intra_request_jitter_max":"0"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.SpreadWindowDur != 0 || c.IntraJitterMinD != 0 || c.IntraJitterMaxD != 0 {
		t.Errorf("显式 0 应关闭：spread=%v jitter=%v~%v", c.SpreadWindowDur, c.IntraJitterMinD, c.IntraJitterMaxD)
	}
}

// min > max 属配置错误：加载期就报错，而不是运行时静默按某个值跑。
func TestJitterMinGreaterThanMaxRejected(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"intra_request_jitter_min":"5s","intra_request_jitter_max":"2s"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("min>max 应报错")
	} else if !strings.Contains(err.Error(), "intra_request_jitter_max") {
		t.Fatalf("错误信息应点名配置项：%v", err)
	}
}

// 非法时长同样在加载期报错。
func TestJitterBadDurationRejected(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"intra_request_jitter_min":"abc"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("非法时长应报错")
	}
}

func TestSoftRateMaxParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"5m","soft_rate_max":"45m"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.SoftRateDur.Minutes() != 5 {
		t.Errorf("soft_rate=%v want 5m", c.SoftRateDur)
	}
	if c.SoftRateMaxDur.Minutes() != 45 {
		t.Errorf("soft_rate_max=%v want 45m", c.SoftRateMaxDur)
	}
}

func TestSoftRateMaxEmptyFallsBackToDefault(t *testing.T) {
	// 键缺席 → Default() 的 2h 保留（空串无法 ParseDuration）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate":"90s"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.SoftRateMaxDur.Hours() != 2 {
		t.Errorf("soft_rate_max=%v want 2h fallback", c.SoftRateMaxDur)
	}
}

func TestBadSoftRateMax(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"cooldown":{"soft_rate_max":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad soft_rate_max")
	}
}

func TestBadBreakerCooldown(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"pool":{"breaker_cooldown":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad breaker_cooldown")
	}
}

func TestUpstreamTimeoutDefaults(t *testing.T) {
	// 默认：header 回落 timeout，idle 回落 300。
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if c.Upstream.TimeoutSeconds != 120 {
		t.Errorf("timeout_seconds=%d want 120", c.Upstream.TimeoutSeconds)
	}
	if c.Upstream.HeaderTimeoutSeconds != 120 {
		t.Errorf("header_timeout_seconds=%d want fallback 120", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamHeaderFallsBackToTimeout(t *testing.T) {
	// 只设 timeout_seconds：header 回落同值，idle 回落 300。
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":60}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 60 {
		t.Errorf("header_timeout_seconds=%d want fallback 60", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 300 {
		t.Errorf("idle_timeout_seconds=%d want fallback 300", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamExplicitHeaderIdle(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"timeout_seconds":120,"header_timeout_seconds":30,"idle_timeout_seconds":600}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 30 {
		t.Errorf("header_timeout_seconds=%d want 30", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 600 {
		t.Errorf("idle_timeout_seconds=%d want 600", c.Upstream.IdleTimeoutSeconds)
	}
}

func TestUpstreamEnvOverride(t *testing.T) {
	t.Setenv("WB2A_HEADER_TIMEOUT_SECONDS", "45")
	t.Setenv("WB2A_IDLE_TIMEOUT_SECONDS", "900")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.HeaderTimeoutSeconds != 45 {
		t.Errorf("header_timeout_seconds=%d want env 45", c.Upstream.HeaderTimeoutSeconds)
	}
	if c.Upstream.IdleTimeoutSeconds != 900 {
		t.Errorf("idle_timeout_seconds=%d want env 900", c.Upstream.IdleTimeoutSeconds)
	}
}

// TestRetiredTravelIntervalKeyIgnored 退役的 travel_interval_minutes 键按未知字段忽略，不报错。
func TestRetiredTravelIntervalKeyIgnored(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	// 旧写法 checkin_hours 仍可用（折算成 09:00），退役键不报错。
	os.WriteFile(fp, []byte(`{"schedule":{"travel_interval_minutes":15,"checkin_hours":[9]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatalf("retired key should not fail load: %v", err)
	}
	if strings.Join(c.Schedule.CheckinTime, ",") != "09:00" {
		t.Errorf("checkin_time=%v want [09:00]（旧 checkin_hours 折算）", c.Schedule.CheckinTime)
	}
}

// TestScheduleEnabledByDefault 四个任务的 enabled 开关默认均为 true，
// 且时刻以 "HH:MM" 形式回落默认。
func TestScheduleEnabledByDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("enabled defaults want true/true, got %v/%v",
			c.Schedule.CheckinEnabled, c.Schedule.KeepaliveEnabled)
	}
	if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
		t.Errorf("travel/activity enabled defaults want true/true, got %v/%v",
			c.Schedule.TravelEnabled, c.Schedule.ActivityEnabled)
	}
	if got := strings.Join(c.Schedule.TravelTime, ","); got != "09:00,21:00" {
		t.Errorf("travel_time=%s want 09:00,21:00", got)
	}
	if got := strings.Join(c.Schedule.ActivityTime, ","); got != "10:00" {
		t.Errorf("activity_time=%s want 10:00", got)
	}
	if got := strings.Join(c.Schedule.SchoolTime, ","); got != "12:00" {
		t.Errorf("school_time=%s want 12:00", got)
	}
	if got := strings.Join(c.Schedule.CatTime, ","); got != "01:00" {
		t.Errorf("cat_time=%s want 01:00", got)
	}
	// 解析出的分钟数（scheduler 直接用这些）。
	if len(c.Schedule.TravelAt) != 2 || c.Schedule.TravelAt[0] != 9*60 || c.Schedule.TravelAt[1] != 21*60 {
		t.Errorf("travel_at=%v want [540 1260]", c.Schedule.TravelAt)
	}
	if !c.Schedule.SchoolEnabled || !c.Schedule.CatEnabled {
		t.Errorf("school/cat enabled defaults want true/true, got %v/%v",
			c.Schedule.SchoolEnabled, c.Schedule.CatEnabled)
	}
}

// TestScheduleLegacyConfigKeepsRunning 老 config（只写 *_hours 数组）加载后：
// 开关仍为启用、旧小时折算成 "HH:00"、未写的时刻回落默认。
func TestScheduleLegacyConfigKeepsRunning(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_hours":[9,21],"keepalive_hours":[22]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("legacy config must stay enabled: %+v", c.Schedule)
	}
	if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
		t.Errorf("new switches must default true on legacy config: %+v", c.Schedule)
	}
	if got := strings.Join(c.Schedule.CheckinTime, ","); got != "09:00,21:00" {
		t.Errorf("checkin_time=%s want 09:00,21:00（旧数组折算）", got)
	}
	if got := strings.Join(c.Schedule.KeepaliveTime, ","); got != "22:00" {
		t.Errorf("keepalive_time=%s want 22:00", got)
	}
	// 未配置的时刻回落默认。
	if got := strings.Join(c.Schedule.TravelTime, ","); got != "09:00,21:00" {
		t.Errorf("travel_time=%s want default 09:00,21:00", got)
	}
	if got := strings.Join(c.Schedule.ActivityTime, ","); got != "10:00" {
		t.Errorf("activity_time=%s want default 10:00", got)
	}
	// 迁移提示：旧键被记录，供启动日志 WARN。
	if len(c.Schedule.MigratedFields()) == 0 {
		t.Error("旧 *_hours 应被记入迁移列表")
	}
}

// TestScheduleExplicitDisable 显式 checkin_enabled=false 即可真正关掉签到
// （issue #27 边界：此前无论怎么配小时都关不掉）。
func TestScheduleExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"keepalive_enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CheckinEnabled || c.Schedule.KeepaliveEnabled {
		t.Errorf("want both disabled: %+v", c.Schedule)
	}
	// 时刻仍回落默认值（禁用与默认值互不干扰：重新启用无需补配时刻）。
	if got := strings.Join(c.Schedule.CheckinTime, ","); got != "09:00,21:00" {
		t.Errorf("checkin_time=%s want default 09:00,21:00 even when disabled", got)
	}
	if got := strings.Join(c.Schedule.KeepaliveTime, ","); got != "22:00" {
		t.Errorf("keepalive_time=%s want default 22:00 even when disabled", got)
	}
}

// TestScheduleTravelActivityExplicitDisable 显式关闭旅行/活跃上报开关。
func TestScheduleTravelActivityExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_enabled":false,"activity_enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.TravelEnabled || c.Schedule.ActivityEnabled {
		t.Errorf("want travel/activity disabled: %+v", c.Schedule)
	}
	if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
		t.Errorf("checkin/keepalive should stay enabled: %+v", c.Schedule)
	}
	// 时刻仍回落默认。
	if got := strings.Join(c.Schedule.TravelTime, ","); got != "09:00,21:00" {
		t.Errorf("travel_time=%s want default 09:00,21:00 even when disabled", got)
	}
	if got := strings.Join(c.Schedule.ActivityTime, ","); got != "10:00" {
		t.Errorf("activity_time=%s want default 10:00 even when disabled", got)
	}
}

// TestScheduleInvalidTimesRejected 非法时刻报错并指向正确开关：
// 新旧两种写法都要拦（旧的小时越界、新的 HH:MM 畸形）。
func TestScheduleInvalidTimesRejected(t *testing.T) {
	cases := []struct{ body, wantSwitch string }{
		{`{"schedule":{"travel_hours":[25]}}`, "travel_enabled"},
		{`{"schedule":{"travel_hours":[-1]}}`, "travel_enabled"},
		{`{"schedule":{"activity_hours":[24]}}`, "activity_enabled"},
		{`{"schedule":{"checkin_time":["25:00"]}}`, "checkin_enabled"},
		{`{"schedule":{"keepalive_time":["22:60"]}}`, "keepalive_enabled"},
		{`{"schedule":{"school_time":["12"]}}`, "school_enabled"},
		{`{"schedule":{"cat_time":[""]}}`, "cat_enabled"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(tc.body), 0o600)
		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for %s", tc.body)
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error for %s should point at schedule.%s: %v", tc.body, tc.wantSwitch, err)
		}
	}
}

// TestScheduleExplicitTimes 显式配置时刻：**分钟精度必须被真正采纳**。
func TestScheduleExplicitTimes(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"travel_time":["09:15","21:45"],"activity_time":["11:05"]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.Schedule.TravelTime, ","); got != "09:15,21:45" {
		t.Errorf("travel_time=%s want 09:15,21:45", got)
	}
	if len(c.Schedule.TravelAt) != 2 || c.Schedule.TravelAt[0] != 9*60+15 || c.Schedule.TravelAt[1] != 21*60+45 {
		t.Errorf("travel_at=%v want [555 1305]", c.Schedule.TravelAt)
	}
	if len(c.Schedule.ActivityAt) != 1 || c.Schedule.ActivityAt[0] != 11*60+5 {
		t.Errorf("activity_at=%v want [665]", c.Schedule.ActivityAt)
	}
}

// TestScheduleDisableKeepsExplicitTimes 禁用不擦除用户配置的时刻（便于原样恢复）。
func TestScheduleDisableKeepsExplicitTimes(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"schedule":{"checkin_enabled":false,"checkin_time":["10:30","14:00"]}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Schedule.CheckinEnabled {
		t.Error("checkin should be disabled")
	}
	if got := strings.Join(c.Schedule.CheckinTime, ","); got != "10:30,14:00" {
		t.Errorf("explicit time must be preserved: %s", got)
	}
	if len(c.Schedule.CheckinAt) != 2 || c.Schedule.CheckinAt[0] != 10*60+30 || c.Schedule.CheckinAt[1] != 14*60 {
		t.Errorf("checkin_at=%v want [630 840]", c.Schedule.CheckinAt)
	}
}

// TestScheduleEmptyTimesFallsBackToDefault 空数组 / null / 缺省都视同「未配置」→ 回落默认。
func TestScheduleEmptyTimesFallsBackToDefault(t *testing.T) {
	cases := map[string]string{
		"absent":   `{}`,
		"empty":    `{"schedule":{}}`,
		"null":     `{"schedule":{"checkin_time":null,"keepalive_time":null,"travel_time":null,"activity_time":null}}`,
		"emptyarr": `{"schedule":{"checkin_time":[],"keepalive_time":[],"travel_time":[],"activity_time":[]}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			fp := filepath.Join(dir, "c.json")
			os.WriteFile(fp, []byte(body), 0o600)
			c, err := Load(fp)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(c.Schedule.CheckinTime, ","); got != "09:00,21:00" {
				t.Errorf("checkin_time=%s want default 09:00,21:00", got)
			}
			if got := strings.Join(c.Schedule.KeepaliveTime, ","); got != "22:00" {
				t.Errorf("keepalive_time=%s want default 22:00", got)
			}
			if got := strings.Join(c.Schedule.TravelTime, ","); got != "09:00,21:00" {
				t.Errorf("travel_time=%s want default 09:00,21:00", got)
			}
			if got := strings.Join(c.Schedule.ActivityTime, ","); got != "10:00" {
				t.Errorf("activity_time=%s want default 10:00", got)
			}
			if !c.Schedule.CheckinEnabled || !c.Schedule.KeepaliveEnabled {
				t.Errorf("empty times must not imply disabled: %+v", c.Schedule)
			}
			if !c.Schedule.TravelEnabled || !c.Schedule.ActivityEnabled {
				t.Errorf("empty times must not imply disabled: %+v", c.Schedule)
			}
		})
	}
}

// TestScheduleInvalidHourRejected 非法小时快速失败：指向正确的禁用开关，避免用户
// 猜测哨兵值（[-1] 之类）被静默当成"改到别的整点"。
func TestScheduleInvalidHourRejected(t *testing.T) {
	cases := []struct{ body, wantSwitch string }{
		{`{"schedule":{"checkin_hours":[25]}}`, "checkin_enabled"},
		{`{"schedule":{"checkin_hours":[-1]}}`, "checkin_enabled"},
		{`{"schedule":{"keepalive_hours":[-1]}}`, "keepalive_enabled"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(tc.body), 0o600)
		_, err := Load(fp)
		if err == nil {
			t.Fatalf("want error for %s", tc.body)
		}
		if !strings.Contains(err.Error(), tc.wantSwitch) {
			t.Errorf("error for %s should point at schedule.%s: %v", tc.body, tc.wantSwitch, err)
		}
	}
}

func TestBadSessionTTL(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"session_sticky":{"ttl":"oops"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for bad session_sticky.ttl")
	}
}

// TestMaxBodyLegacyKeyIgnored max_body_mb 已移除（BREAKING）：旧配置文件里仍带该键
// （含非法值形态 0/-1 与 server 段整体存在）必须解析成功、启动不报错——字段已删，
// JSON 未知键天然忽略（非 DisallowUnknownFields 严格模式），无 deprecation 噪音。
func TestMaxBodyLegacyKeyIgnored(t *testing.T) {
	for _, v := range []string{"8", "16", "0", "-1"} {
		dir := t.TempDir()
		fp := filepath.Join(dir, "c.json")
		os.WriteFile(fp, []byte(`{"server":{"max_body_mb":`+v+`}}`), 0o600)
		if _, err := Load(fp); err != nil {
			t.Fatalf("legacy max_body_mb=%s must not fail startup: %v", v, err)
		}
	}
}

// TestPromptDefaultMode 默认 prompt.mode=passthrough 且不加载 PromptText（透传客户端原始 system）。
func TestPromptDefaultMode(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("prompt.mode=%q want passthrough", c.Prompt.Mode)
	}
	if c.PromptText != "" {
		t.Errorf("default passthrough should not load PromptText, got len=%d", len(c.PromptText))
	}
}

// TestPromptExplicitPassthrough passthrough 模式不加载文本（透传客户端原始 system）。
func TestPromptExplicitPassthrough(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"passthrough"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("mode=%q want passthrough", c.Prompt.Mode)
	}
	if c.PromptText != "" {
		t.Errorf("passthrough should not load PromptText, got len=%d", len(c.PromptText))
	}
}

// TestPromptInvalidMode 非法 mode 启动报错。
func TestPromptInvalidMode(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"bogus"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for invalid prompt.mode")
	}
}

// TestPromptFileMissing 文件路径非空但不存在 → 启动报错（fail fast）。
func TestPromptFileMissing(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"custom","file":"/nonexistent/p.md"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for missing prompt file")
	}
}

// TestPromptFileOverride 自定义 file 覆盖内置默认。
func TestPromptFileOverride(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "my.md")
	want := "我的自定义人格入口"
	os.WriteFile(pf, []byte(want), 0o600)
	cf := filepath.Join(dir, "c.json")
	// 路径写进 JSON 字符串需转义反斜杠：Windows 下 filepath.Join 生成 C:\Users\...，
	// 原样拼接会让 \U 成为非法 JSON 转义。ToSlash 统一为正斜杠（跨平台可解析）。
	os.WriteFile(cf, []byte(`{"prompt":{"mode":"custom","file":"`+filepath.ToSlash(pf)+`"}}`), 0o600)
	c, err := Load(cf)
	if err != nil {
		t.Fatal(err)
	}
	if c.PromptText != want {
		t.Errorf("PromptText=%q want %q", c.PromptText, want)
	}
}

// TestPromptEnvOverride env 覆盖 prompt.mode 与 prompt.file。
func TestPromptEnvOverride(t *testing.T) {
	t.Setenv("WB2A_PROMPT_MODE", "passthrough")
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("mode=%q want passthrough", c.Prompt.Mode)
	}
}

// TestPromptLegacyConfigNoImpact 旧 config（无 prompt 段）零影响：mode 走缺省 passthrough。
func TestPromptLegacyConfigNoImpact(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999","api_key":"k"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "passthrough" {
		t.Errorf("legacy config should default to passthrough, got %q", c.Prompt.Mode)
	}
	if c.Listen != ":9999" {
		t.Errorf("listen=%q", c.Listen)
	}
}

// TestUpstreamVersionConfig 配置 upstream.client_version / cli_version 与 env
// WB2A_CLIENT_VERSION / WB2A_CLI_VERSION 均生效；缺省空串 = headers 层回落内置默认。
func TestUpstreamVersionConfig(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"client_version":"6.0.0","cli_version":"3.0.0"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.ClientVersion != "6.0.0" || c.Upstream.CliVersion != "3.0.0" {
		t.Errorf("client_version=%q cli_version=%q want 6.0.0/3.0.0", c.Upstream.ClientVersion, c.Upstream.CliVersion)
	}
	// 缺省为空（headers 层回落内置默认）。
	if c2, err := Load(""); err != nil || c2.Upstream.ClientVersion != "" || c2.Upstream.CliVersion != "" {
		t.Errorf("default versions=%q/%q want empty (err=%v)", c2.Upstream.ClientVersion, c2.Upstream.CliVersion, err)
	}
	// env 覆盖。
	t.Setenv("WB2A_CLIENT_VERSION", "7.0.0")
	t.Setenv("WB2A_CLI_VERSION", "4.0.0")
	c3, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c3.Upstream.ClientVersion != "7.0.0" || c3.Upstream.CliVersion != "4.0.0" {
		t.Errorf("env versions=%q/%q want 7.0.0/4.0.0", c3.Upstream.ClientVersion, c3.Upstream.CliVersion)
	}
}

// TestUpstreamUserAgentConfig 配置 upstream.user_agent 与 env WB2A_USER_AGENT 均生效，
// 缺省空串保持现状（headers 层回落到 clientUA）。
func TestUpstreamUserAgentConfig(t *testing.T) {
	// JSON 配置
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"upstream":{"user_agent":"WorkBuddy/1.2.3"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream.UserAgent != "WorkBuddy/1.2.3" {
		t.Errorf("user_agent=%q want WorkBuddy/1.2.3", c.Upstream.UserAgent)
	}
	// 缺省为空
	if c2, err := Load(""); err != nil || c2.Upstream.UserAgent != "" {
		t.Errorf("default user_agent=%q want empty (err=%v)", c2.Upstream.UserAgent, err)
	}
	// env 覆盖
	t.Setenv("WB2A_USER_AGENT", "EnvAgent/9")
	c3, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c3.Upstream.UserAgent != "EnvAgent/9" {
		t.Errorf("env user_agent=%q want EnvAgent/9", c3.Upstream.UserAgent)
	}
}

// ---- prompt.mode=append（issue #129 三模式） ----

// TestPromptAppendModeAccepted B1：append 新值合法，PromptText 非空（内置默认，
// 与 custom 同加载路径）。
func TestPromptAppendModeAccepted(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"append"}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prompt.Mode != "append" {
		t.Errorf("mode=%q want append", c.Prompt.Mode)
	}
	if c.PromptText == "" {
		t.Error("append should load PromptText (built-in default)")
	}
}

// TestPromptAppendFileOverride B2：append + file → PromptText 为文件内容。
func TestPromptAppendFileOverride(t *testing.T) {
	dir := t.TempDir()
	pf := filepath.Join(dir, "my.md")
	want := "我的 append 模式人格"
	os.WriteFile(pf, []byte(want), 0o600)
	cf := filepath.Join(dir, "c.json")
	os.WriteFile(cf, []byte(`{"prompt":{"mode":"append","file":"`+filepath.ToSlash(pf)+`"}}`), 0o600)
	c, err := Load(cf)
	if err != nil {
		t.Fatal(err)
	}
	if c.PromptText != want {
		t.Errorf("PromptText=%q want %q", c.PromptText, want)
	}
}

// TestPromptAppendFileMissingFailsFast B3：append + 不可读 file → 启动报错（同 custom）。
func TestPromptAppendFileMissingFailsFast(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"append","file":"/nonexistent/p.md"}}`), 0o600)
	if _, err := Load(fp); err == nil {
		t.Fatal("want error for missing prompt file in append mode")
	}
}

// TestPromptInvalidModeStillErrors B4：非法值报错文案含三值说明。
func TestPromptInvalidModeStillErrors(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"prompt":{"mode":"replace"}}`), 0o600)
	_, err := Load(fp)
	if err == nil {
		t.Fatal("want error for invalid prompt.mode")
	}
	if !strings.Contains(err.Error(), "custom / append / passthrough") {
		t.Errorf("error should mention (custom / append / passthrough): %v", err)
	}
}

// 思考通道兜底（issue #135）：默认开；JSON 显式 false 与 env 都能关。
func TestReasoningAsContentDefaultAndOverride(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.ReasoningAsContent == nil || !*c.ReasoningAsContent {
		t.Fatalf("默认应为 true，实际 %v", c.ReasoningAsContent)
	}

	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"reasoning_as_content":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReasoningAsContent == nil || *c.ReasoningAsContent {
		t.Fatalf("JSON 显式 false 应生效，实际 %v", c.ReasoningAsContent)
	}

	t.Setenv("WB2A_REASONING_AS_CONTENT", "1")
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.ReasoningAsContent == nil || !*c.ReasoningAsContent {
		t.Fatalf("环境变量应覆盖 JSON，实际 %v", c.ReasoningAsContent)
	}

	t.Setenv("WB2A_REASONING_AS_CONTENT", "0")
	c, err = Load("")
	if err != nil {
		t.Fatal(err)
	}
	if c.ReasoningAsContent == nil || *c.ReasoningAsContent {
		t.Fatalf("环境变量 0 应关闭，实际 %v", c.ReasoningAsContent)
	}
}
