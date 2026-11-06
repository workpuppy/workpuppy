package scheduler

import (
	"testing"
	"time"
)

// 时点是「自 00:00 起的分钟数」——09:30 必须真的排在 09:30，而不是被抹成整点。
func TestNextFireHonoursMinutePrecision(t *testing.T) {
	loc := time.Local
	now := time.Date(2026, 11, 6, 8, 0, 0, 0, loc)

	next := nextFire(now, []int{9*60 + 30})
	want := time.Date(2026, 11, 6, 9, 30, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("nextFire=%s want %s", next, want)
	}

	// 已过 09:30 → 顺延到次日同一时刻。
	after := time.Date(2026, 11, 6, 9, 31, 0, 0, loc)
	if got := nextFire(after, []int{9*60 + 30}); !got.Equal(want.AddDate(0, 0, 1)) {
		t.Fatalf("nextFire(after)=%s want 次日 09:30", got)
	}

	// 多时点取最近的那个（含分钟）。
	got := nextFire(now, []int{21*60 + 45, 9*60 + 5, 23*60 + 59})
	if want := time.Date(2026, 11, 6, 9, 5, 0, 0, loc); !got.Equal(want) {
		t.Fatalf("nextFire(random order)=%s want %s", got, want)
	}
}

// nextWake 走的是同一套时点：09:30 的签到槽位应被识别为「今天 09:30」。
func TestNextWakeSeesHalfPastSlot(t *testing.T) {
	s := New(Config{
		CheckinAt:          []int{9*60 + 30},
		TravelDisabled:     true,
		ActivityDisabled:   true,
		KeepaliveDisabled:  true,
		SchoolDisabled:     true,
		CatDisabled:        true,
		RandomDelayMinutes: 0, // 关掉时点抖动，直接看原始时点
	})
	now := time.Date(2026, 11, 6, 8, 0, 0, 0, time.Local)
	at, kinds := s.nextWake(now)
	want := time.Date(2026, 11, 6, 9, 30, 0, 0, time.Local)
	if !at.Equal(want) || len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Fatalf("nextWake=%s kinds=%v want %s [checkin]", at, kinds, want)
	}
}

// 时点抖动（random_delay_minutes）不得把 09:30 的槽位推出它所在的那一小时。
func TestHalfPastSlotJitterStaysInsideHour(t *testing.T) {
	s := New(Config{
		RandomDelayMinutes: 60,
		RandomDelaySeed:    "synthetic-deployment",
		CheckinAt:          []int{9*60 + 30},
		TravelDisabled:     true,
		ActivityDisabled:   true,
		KeepaliveDisabled:  true,
		SchoolDisabled:     true,
		CatDisabled:        true,
	})
	day := time.Date(2026, 11, 6, 0, 0, 0, 0, cstZone)
	for d := 0; d < 30; d++ {
		slot := s.scheduledSlot(day.AddDate(0, 0, d), 9*60+30, taskCheckin)
		if slot.Hour() != 9 {
			t.Fatalf("09:30 槽位抖动跨出小时：%s", slot)
		}
		if slot.Minute() < 30 {
			t.Fatalf("09:30 槽位抖动跑到时点之前：%s", slot)
		}
	}
}
