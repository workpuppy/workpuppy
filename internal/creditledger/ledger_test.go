package creditledger

import (
	"path/filepath"
	"testing"
	"time"
)

func cst(y int, m time.Month, d, h int) time.Time {
	return time.Date(y, m, d, h, 0, 0, 0, cstZone)
}

// 当日热值：赚/花/调整分别累计，全局由账号加总。
func TestRecordAndTotals(t *testing.T) {
	l := New("", 30)
	l.SetClock(func() time.Time { return cst(2026, 11, 10, 12) })

	l.RecordEarned("u1", "cn", 300)
	l.RecordSpent("u1", "cn", 120)
	l.RecordAdjust("u1", "cn", 30)
	l.RecordEarned("g1", "global", 50)
	l.RecordSpent("g1", "global", 10)

	snap := l.Snap()
	if snap.Today != "2026-11-10" {
		t.Fatalf("today=%s", snap.Today)
	}
	if snap.Totals.Earned != 350 || snap.Totals.Spent != 130 || snap.Totals.Adjust != 30 {
		t.Fatalf("totals=%+v", snap.Totals)
	}
	if snap.Totals.Net != 350-130-30 {
		t.Errorf("net=%d want %d", snap.Totals.Net, 350-130-30)
	}
	if a := snap.Accounts["u1"]; a == nil || a.Earned != 300 || a.Spent != 120 || a.Adjust != 30 || a.Realm != "cn" {
		t.Fatalf("u1=%+v", a)
	}
}

// 跨天滚动：昨天的热态冻结进历史，新的一天从零开始；历史总量仍可查。
func TestDayRollover(t *testing.T) {
	l := New("", 30)
	now := cst(2026, 11, 10, 23)
	l.SetClock(func() time.Time { return now })
	l.RecordEarned("u1", "cn", 100)

	now = cst(2026, 11, 11, 1) // 跨天
	l.RecordSpent("u1", "cn", 40)

	snap := l.Snap()
	if snap.Today != "2026-11-11" {
		t.Fatalf("today=%s", snap.Today)
	}
	if snap.Totals.Earned != 0 || snap.Totals.Spent != 40 {
		t.Errorf("当日应只剩新一天：%+v", snap.Totals)
	}
	if len(snap.Histories) != 1 || snap.Histories[0].Date != "2026-11-10" {
		t.Fatalf("历史=%+v", snap.Histories)
	}
	if h := snap.Histories[0].Totals; h.Earned != 100 || h.Spent != 0 {
		t.Errorf("历史总量=%+v", h)
	}
}

// 落盘 + 重启恢复：当日热态从盘上继续累加，历史保留，超过保留天数被裁剪。
func TestFlushAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credit-daily.json")

	l := New(path, 30)
	now := cst(2026, 11, 10, 12)
	l.SetClock(func() time.Time { return now })
	l.RecordEarned("u1", "cn", 300)
	l.RecordSpent("u1", "cn", 50)
	if err := l.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	// 重启（同一时钟）：当日热值应恢复
	l2 := New(path, 30)
	l2.SetClock(func() time.Time { return now })
	if err := l2.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	snap := l2.Snap()
	if a := snap.Accounts["u1"]; a == nil || a.Earned != 300 || a.Spent != 50 {
		t.Fatalf("重启后当日热值丢失：%+v", snap.Accounts)
	}
	// 继续累加不覆盖
	l2.RecordEarned("u1", "cn", 20)
	if got := l2.Snap().Totals.Earned; got != 320 {
		t.Errorf("累加后 earned=%d want 320", got)
	}
}

// 保留天数：超过 retain 的旧记录被裁掉。
func TestRetention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credit-daily.json")
	l := New(path, 2)

	day := cst(2026, 11, 1, 12)
	l.SetClock(func() time.Time { return day })
	for i := 0; i < 5; i++ {
		day = day.AddDate(0, 0, 1)
		l.SetClock(func() time.Time { return day })
		l.RecordEarned("u1", "cn", int64(i+1))
		if err := l.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
	}
	snap := l.Snap()
	// retain=2 → 历史最多 2 天（不含今天）
	if len(snap.Histories) > 2 {
		t.Fatalf("历史天数=%d 超过保留 2 天：%+v", len(snap.Histories), snap.Histories)
	}
	if snap.Histories[len(snap.Histories)-1].Date != "2026-11-05" {
		t.Errorf("最后一天历史=%s", snap.Histories[len(snap.Histories)-1].Date)
	}
}

// 余额变化拆成"赚"与"调整"（>0 赚、<0 调整、0 忽略）。
func TestBalanceDelta(t *testing.T) {
	l := New("", 30)
	l.SetClock(func() time.Time { return cst(2026, 11, 10, 9) })
	l.RecordBalanceDelta("u1", "cn", 200)
	l.RecordBalanceDelta("u1", "cn", -80)
	l.RecordBalanceDelta("u1", "cn", 0)
	a := l.Snap().Accounts["u1"]
	if a.Earned != 200 || a.Adjust != 80 || a.Spent != 0 {
		t.Fatalf("delta 拆分错：%+v", a)
	}
}

// History(n) 只返回最近 n 天（含今天，按日期升序）。
func TestHistoryWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credit-daily.json")
	l := New(path, 30)
	day := cst(2026, 11, 1, 12)
	for i := 0; i < 5; i++ {
		day = day.AddDate(0, 0, 1)
		l.SetClock(func() time.Time { return day })
		l.RecordEarned("u1", "cn", 10)
		_ = l.Flush()
	}
	h := l.History(3)
	if len(h) != 3 {
		t.Fatalf("history(3) 长度=%d", len(h))
	}
	if h[len(h)-1].Date != "2026-11-06" {
		t.Errorf("最后一天应是今天 2026-11-06，实际 %s", h[len(h)-1].Date)
	}
	for i := 1; i < len(h); i++ {
		if h[i-1].Date >= h[i].Date {
			t.Fatalf("日期未升序：%v", h)
		}
	}
}
