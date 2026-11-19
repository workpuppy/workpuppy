package scheduler

import (
	"testing"
	"time"

	// 测试二进制也内嵌时区库：CI 镜像（distroless/scratch 或精简 ubuntu）可能没有
	// /usr/share/zoneinfo，否则 LoadLocation("Asia/Shanghai") 会失败。
	_ "time/tzdata"
)

// shanghai 排程时区（生产默认值）。中国无夏令时，固定 +8。
func shanghai(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	return loc
}

// onlyCheckin 只保留签到一类任务（New 会给其余五类补默认时刻，会干扰断言）。
func onlyCheckin(loc *time.Location, at []int, randomDelayMinutes int) *Scheduler {
	return New(Config{
		Location:           loc,
		CheckinAt:          at,
		RandomDelayMinutes: randomDelayMinutes,
		RandomDelaySeed:    "seed",
		TravelDisabled:     true,
		ActivityDisabled:   true,
		KeepaliveDisabled:  true,
		SchoolDisabled:     true,
		CatDisabled:        true,
	})
}

// TestNextFireHonorsConfiguredTimezone 时点按**配置时区**解释，不受 now 自带时区影响：
// 容器里 now 常是 UTC，而 "09:00" 必须指北京时间的 09:00。
func TestNextFireHonorsConfiguredTimezone(t *testing.T) {
	loc := shanghai(t)
	// 北京时间 2026-09-11 20:00 = UTC 12:00
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

	s := onlyCheckin(loc, []int{9 * 60, 21 * 60}, 0)
	at, kinds := s.nextWake(now)
	// 21:00 CST = 13:00 UTC（当天）
	want := time.Date(2026, 9, 11, 13, 0, 0, 0, time.UTC)
	if !at.Equal(want) {
		t.Fatalf("nextWake = %s (CST %s) want %s（北京 21:00）", at, at.In(loc), want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Fatalf("kinds = %v want [checkin]", kinds)
	}
	if h := at.In(loc).Hour(); h != 21 {
		t.Fatalf("槽位在配置时区里是 %02d 点，want 21（时区被忽略了？）", h)
	}

	// 北京时间 22:00（UTC 14:00）→ 下一次是次日 09:00 CST = 次日 01:00 UTC
	now2 := time.Date(2026, 9, 11, 14, 0, 0, 0, time.UTC)
	at2, _ := s.nextWake(now2)
	if want2 := time.Date(2026, 9, 12, 1, 0, 0, 0, time.UTC); !at2.Equal(want2) {
		t.Fatalf("nextWake(22:00 CST) = %s want %s（次日 09:00 CST）", at2, want2)
	}
}

// TestNextFireTimezoneDiffersFromContainerLocal 同一份配置，换个时区就跑在另一个钟点：
// 这正是「容器 TZ=UTC 时 09:00 变成北京时间 17:00」的根因。
func TestNextFireTimezoneDiffersFromContainerLocal(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) // 北京时间 20:00
	at := []int{9 * 60}                                  // 只有 09:00 一档

	sh := onlyCheckin(shanghai(t), at, 0)
	utc := onlyCheckin(time.UTC, at, 0)

	gotCST, _ := sh.nextWake(now)  // 次日 09:00 CST = 次日 01:00 UTC
	gotUTC, _ := utc.nextWake(now) // 次日 09:00 UTC = 次日 09:00 UTC
	if want := time.Date(2026, 9, 12, 1, 0, 0, 0, time.UTC); !gotCST.Equal(want) {
		t.Errorf("CST 排程 = %s want %s", gotCST, want)
	}
	if want := time.Date(2026, 9, 12, 9, 0, 0, 0, time.UTC); !gotUTC.Equal(want) {
		t.Errorf("UTC 排程 = %s want %s", gotUTC, want)
	}
	if diff := gotUTC.Sub(gotCST); diff != 8*time.Hour {
		t.Errorf("两种时区应相差 8 小时，实际 %s", diff)
	}
}

// TestDueSlotsCrossMidnightInConfiguredTimezone 补跑枚举也要按配置时区切日：
// 北京时间 21:00 的槽位落在 UTC 当天的 13:00，用 UTC 取年月日会错一天。
func TestDueSlotsCrossMidnightInConfiguredTimezone(t *testing.T) {
	loc := shanghai(t)
	s := onlyCheckin(loc, []int{9 * 60, 21 * 60}, 0)

	// 北京时间 20:30 → 21:10（跨过 21:00 那个槽）
	cursor := time.Date(2026, 9, 11, 12, 30, 0, 0, time.UTC) // 20:30 CST
	now := time.Date(2026, 9, 11, 13, 10, 0, 0, time.UTC)    // 21:10 CST
	due := s.dueSlots(cursor, now)
	if len(due) != 1 {
		t.Fatalf("dueSlots = %d 个 want 1（21:00 CST）", len(due))
	}
	if want := time.Date(2026, 9, 11, 13, 0, 0, 0, time.UTC); !due[0].planned.Equal(want) {
		t.Fatalf("planned = %s want %s", due[0].planned, want)
	}

	// UTC 侧已跨日、但北京还在同一天早上的那个窗口：
	// cursor = 08:59 CST（UTC 00:59），now = 09:01 CST（UTC 01:01）
	cursor2 := time.Date(2026, 9, 12, 0, 59, 0, 0, time.UTC)
	now2 := time.Date(2026, 9, 12, 1, 1, 0, 0, time.UTC)
	due2 := s.dueSlots(cursor2, now2)
	if len(due2) != 1 {
		t.Fatalf("dueSlots(09:00 CST) = %d 个 want 1", len(due2))
	}
	if want := time.Date(2026, 9, 12, 1, 0, 0, 0, time.UTC); !due2[0].planned.Equal(want) {
		t.Fatalf("planned = %s want %s（09:00 CST）", due2[0].planned, want)
	}
	// 同样的时间窗，UTC 排程不会命中（那里的 09:00 在 8 小时之后）。
	utcS := onlyCheckin(time.UTC, []int{9 * 60, 21 * 60}, 0)
	if got := utcS.dueSlots(cursor2, now2); len(got) != 0 {
		t.Fatalf("UTC 排程不该在这个窗口命中槽位：%v", got)
	}
}

// TestNilLocationUsesClockZone 零值 Config（未接时区）保持老行为：时点按 now 自带
// 时区解释。刻意不用 time.Local——那会让同一份测试在 UTC 容器与 +8 开发机上
// 结果不同（CI 曾经因此失败）。
func TestNilLocationUsesClockZone(t *testing.T) {
	s := New(Config{CheckinAt: []int{9 * 60}})
	now := time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC)
	if got := s.locFor(now); got != time.UTC {
		t.Fatalf("locFor(nil 配置) = %v want now 的时区 UTC", got)
	}
	at, _ := s.nextWake(now)
	if want := time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC); !at.Equal(want) {
		t.Fatalf("nextWake = %s want %s", at, want)
	}
	// 换一个时区喂同一个调度器：时刻跟着 now 走，不需要改配置。
	nowCST := time.Date(2026, 9, 11, 8, 0, 0, 0, shanghai(t))
	atCST, _ := s.nextWake(nowCST)
	if want := time.Date(2026, 9, 11, 9, 0, 0, 0, shanghai(t)); !atCST.Equal(want) {
		t.Fatalf("nextWake(CST now) = %s want %s", atCST, want)
	}
}

// TestRandomDelaySlotStaysInConfiguredTimezone 随机偏移（schedule.random_delay_minutes）
// 也在配置时区的同一天同一小时内抖动，不因时区换算漂到别的小时。
func TestRandomDelaySlotStaysInConfiguredTimezone(t *testing.T) {
	loc := shanghai(t)
	s := onlyCheckin(loc, []int{9 * 60}, 60)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) // 北京 20:00
	at, _ := s.nextWake(now)
	bj := at.In(loc)
	if bj.Hour() != 9 || bj.Day() != 12 || bj.Month() != time.September {
		t.Fatalf("随机槽位 = %s（北京 %s），want 9 月 12 日 09:xx（北京时间）", at, bj)
	}
}
