package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDefaultScheduleActivityCount 缺省 ActivityReportCount=5（与 cmd/server Default() 对齐）。
// issue #49：cmd/activity 曾因复制结构体无默认值，缺省回落到 1，与 server 的 5 漂移。
func TestDefaultScheduleActivityCount(t *testing.T) {
	s := DefaultSchedule()
	if s.ActivityReportCount != 5 {
		t.Errorf("default ActivityReportCount=%d want 5", s.ActivityReportCount)
	}
	if !s.CheckinEnabled || !s.TravelEnabled || !s.ActivityEnabled || !s.KeepaliveEnabled {
		t.Errorf("all switches must default true: %+v", s)
	}
	// 时刻由 Normalize 统一填默认值 —— 这里先归一化再断言。
	if err := s.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	// 时刻是 "HH:MM" 字符串（可精确到分）。
	want := map[string][]string{
		"checkin": {"09:00", "21:00"}, "travel": {"09:00", "21:00"},
		"activity": {"10:00"}, "keepalive": {"22:00"},
		"school": {"12:00"}, "cat": {"01:00"},
	}
	got := map[string][]string{
		"checkin": s.CheckinTime, "travel": s.TravelTime,
		"activity": s.ActivityTime, "keepalive": s.KeepaliveTime,
		"school": s.SchoolTime, "cat": s.CatTime,
	}
	for key, exp := range want {
		if strings.Join(got[key], ",") != strings.Join(exp, ",") {
			t.Errorf("%s_time=%v want %v", key, got[key], exp)
		}
	}
	if !s.SchoolEnabled || !s.CatEnabled {
		t.Errorf("school/cat switches must default true: %+v", s)
	}
}

// TestNormalizeScheduleThreeStates 缺省/显式 0/显式 N 三态默认值：
//   - 缺省（空 schedule）→ ActivityReportCount 保留 DefaultSchedule 的 5，时刻回落默认。
//   - 显式 0  → 归一为 1（兼容旧行为）。
//   - 显式 N  → 保留 N。
func TestNormalizeScheduleThreeStates(t *testing.T) {
	cases := []struct {
		name string
		s    Schedule
		want int
	}{
		{"absent_keeps_default", DefaultSchedule(), 5},
		{"explicit_zero_to_one", Schedule{ActivityReportCount: 0}, 1},
		{"explicit_negative_to_one", Schedule{ActivityReportCount: -3}, 1},
		{"explicit_n", Schedule{ActivityReportCount: 8}, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s
			if err := s.Normalize(); err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if s.ActivityReportCount != tc.want {
				t.Errorf("ActivityReportCount=%d want %d", s.ActivityReportCount, tc.want)
			}
		})
	}
}

// TestParseClockAndFormat "HH:MM" 解析/渲染：接受 H:MM，拒绝越界与畸形。
func TestParseClockAndFormat(t *testing.T) {
	ok := map[string]int{
		"09:30": 570, "9:30": 570, "00:00": 0, "23:59": 1439, " 07:05 ": 425,
	}
	for in, want := range ok {
		got, err := ParseClock(in)
		if err != nil || got != want {
			t.Errorf("ParseClock(%q)=%d,%v want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "9", "24:00", "09:60", "09-30", "09:30:00", "aa:bb"} {
		if _, err := ParseClock(bad); err == nil {
			t.Errorf("ParseClock(%q) 应报错", bad)
		}
	}
	if got := FormatClock(570); got != "09:30" {
		t.Errorf("FormatClock(570)=%q want 09:30", got)
	}
	if got := FormatClock(0); got != "00:00" {
		t.Errorf("FormatClock(0)=%q want 00:00", got)
	}
}

// TestNormalizeScheduleEmptyTimesFallback 空数组/缺省时刻一律回落默认，并解析成分钟数。
func TestNormalizeScheduleEmptyTimesFallback(t *testing.T) {
	s := Schedule{ActivityReportCount: 5} // 时刻全零值（未配）
	if err := s.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if strings.Join(s.CheckinTime, ",") != "09:00,21:00" {
		t.Errorf("checkin_time=%v", s.CheckinTime)
	}
	// 分钟数（scheduler 直接用）：09:00=540、21:00=1260
	if len(s.CheckinAt) != 2 || s.CheckinAt[0] != 540 || s.CheckinAt[1] != 1260 {
		t.Errorf("checkin_at=%v want [540 1260]", s.CheckinAt)
	}
	if len(s.KeepaliveAt) != 1 || s.KeepaliveAt[0] != 22*60 {
		t.Errorf("keepalive_at=%v want [1320]", s.KeepaliveAt)
	}
	if len(s.CatAt) != 1 || s.CatAt[0] != 60 {
		t.Errorf("cat_at=%v want [60]", s.CatAt)
	}
}

// TestNormalizeScheduleMinutePrecision 分钟精度：09:30 必须被真正采纳（不是被抹成 09:00）。
func TestNormalizeScheduleMinutePrecision(t *testing.T) {
	s := Schedule{CheckinTime: []string{"09:30", "21:45"}}
	if err := s.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(s.CheckinAt) != 2 || s.CheckinAt[0] != 9*60+30 || s.CheckinAt[1] != 21*60+45 {
		t.Fatalf("checkin_at=%v want [570 1305]", s.CheckinAt)
	}
}

// TestNormalizeScheduleDedupAndSort 重复时刻去重、乱序输入排成升序。
func TestNormalizeScheduleDedupAndSort(t *testing.T) {
	s := Schedule{ActivityTime: []string{"21:00", "09:30", "09:30"}}
	if err := s.Normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(s.ActivityAt) != 2 || s.ActivityAt[0] != 570 || s.ActivityAt[1] != 1260 {
		t.Fatalf("activity_at=%v want [570 1260]", s.ActivityAt)
	}
}

// TestNormalizeScheduleInvalidTime 非法时刻快速失败，错误信息指向字段与正确开关。
func TestNormalizeScheduleInvalidTime(t *testing.T) {
	cases := []struct {
		s         Schedule
		wantInErr string
	}{
		{Schedule{CheckinTime: []string{"25:00"}}, "checkin_time"},
		{Schedule{CheckinTime: []string{"09:70"}}, "checkin_time"},
		{Schedule{CheckinTime: []string{"0930"}}, "checkin_time"},
		{Schedule{KeepaliveTime: []string{"24:00"}}, "keepalive_time"},
		{Schedule{TravelTime: []string{"x"}}, "travel_time"},
		{Schedule{ActivityTime: []string{"-1:00"}}, "activity_time"},
		{Schedule{SchoolTime: []string{"12:60"}}, "school_time"},
		{Schedule{CatTime: []string{""}}, "cat_time"},
	}
	for _, tc := range cases {
		err := tc.s.Normalize()
		if err == nil {
			t.Errorf("want error for %+v", tc.s)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantInErr) {
			t.Errorf("error %q should mention %s", err.Error(), tc.wantInErr)
		}
		if !strings.Contains(err.Error(), "_enabled") {
			t.Errorf("error %q should point at the _enabled switch", err.Error())
		}
	}
}

// TestScheduleDefaultsSurvivePartialUnmarshal 键缺席必须保留上一层的默认值
// （默认配置来自内嵌 config.default.json；用户文件只写一部分键是常态）。
func TestScheduleDefaultsSurvivePartialUnmarshal(t *testing.T) {
	s := DefaultSchedule()
	if err := json.Unmarshal([]byte(`{"checkin_time":["09:30"]}`), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := strings.Join(s.CheckinTime, ","); got != "09:30" {
		t.Errorf("checkin_time=%s want 09:30", got)
	}
	// 没写的键保持示例里的默认值。
	if got := strings.Join(s.KeepaliveTime, ","); got != "22:00" {
		t.Errorf("keepalive_time=%s want 22:00（未写的键应保留默认）", got)
	}
	if got := s.ActivityReportCount; got != 5 {
		t.Errorf("activity_report_count=%d want 5（未写的键应保留默认）", got)
	}
	if got := s.SpreadWindow; got != "15m" {
		t.Errorf("spread_window=%s want 15m（未写的键应保留默认）", got)
	}
	if !s.CheckinEnabled || !s.CatEnabled {
		t.Errorf("开关默认应保留 true：%+v", s)
	}
}
