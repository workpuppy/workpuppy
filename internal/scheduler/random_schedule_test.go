package scheduler

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
	"wp/internal/auth"
)

func TestScheduledAccountGapBoundsAndManualBypass(t *testing.T) {
	s := New(Config{AccountDelayMinSeconds: 30, AccountDelayMaxSeconds: 120})
	ctx := context.WithValue(context.Background(), scheduledAccountKey{}, true)
	base := 800 * time.Millisecond
	for i := 0; i < 100; i++ {
		gap := s.accountGap(ctx, base)
		if gap < base+30*time.Second || gap > base+120*time.Second {
			t.Fatalf("gap out of range: %s", gap)
		}
	}
	if got := s.accountGap(context.Background(), base); got != base {
		t.Fatalf("manual pacing changed: %s", got)
	}
	s.cfg.AccountDelayMinSeconds, s.cfg.AccountDelayMaxSeconds = 0, 0
	if got := s.accountGap(ctx, base); got != base {
		t.Fatalf("zero range changed old pacing: %s", got)
	}
}

func TestScheduledCheckinAccountWaitCanBeCancelled(t *testing.T) {
	s, p := newCheckinS(t, &checkinStub{checkinBody: `{"code":0,"data":{}}`, resourceRemain: 100})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	s.cfg.AccountDelayMinSeconds, s.cfg.AccountDelayMaxSeconds = 30, 120
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), scheduledAccountKey{}, true), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	out, err := s.checkinAll(ctx)
	if err != context.DeadlineExceeded || len(out) != 1 {
		t.Fatalf("cancelled batch: out=%v err=%v", out, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("cancellation waited for random delay")
	}
	// Manual entry still processes both accounts immediately with the same config.
	out, err = s.CheckinAll()
	if err != nil || len(out) != 2 {
		t.Fatalf("manual batch: out=%v err=%v", out, err)
	}
}

func TestScheduledScriptsReceiveAccountRangeOnlyOnAutomaticEntry(t *testing.T) {
	// 任务二进制必须先存在：Go 化之后 runSchool/runCat 找不到二进制会直接 WARN 返回，
	// 不会构造任何命令。
	installTaskBins(t, TaskBinSchool, TaskBinRunner)
	f := installFakeExec(t)
	s := New(Config{AccountDelayMinSeconds: 30, AccountDelayMaxSeconds: 120})
	s.RunSchoolNow()
	// 手工入口：ALL --run --yes —— 二进制路径是 Program 参数，不在 args 里。
	if len(f.lastArgs) != 3 {
		t.Fatalf("manual school flags changed: %v", f.lastArgs)
	}
	s.dispatch(context.Background(), taskSchool)
	if len(f.lastArgs) != 7 || f.lastArgs[3] != "--account-delay-min" || f.lastArgs[4] != "30" || f.lastArgs[6] != "120" {
		t.Fatalf("scheduled school flags missing: %v", f.lastArgs)
	}
	s.RunCatNow()
	if len(f.lastArgs) != 4 {
		t.Fatalf("manual cat flags changed: %v", f.lastArgs)
	}
	s.dispatch(context.Background(), taskCat)
	if len(f.lastArgs) != 8 || f.lastArgs[4] != "--account-delay-min" {
		t.Fatalf("scheduled cat flags missing: %v", f.lastArgs)
	}
}

type spacingBatchCommand struct{ active, maximum, calls *atomic.Int32 }

func (c *spacingBatchCommand) SetDir(string) {}
func (c *spacingBatchCommand) Run() error {
	active := c.active.Add(1)
	for old := c.maximum.Load(); active > old; old = c.maximum.Load() {
		if c.maximum.CompareAndSwap(old, active) {
			break
		}
	}
	time.Sleep(30 * time.Millisecond)
	c.calls.Add(1)
	c.active.Add(-1)
	return nil
}

func TestStaggeredCoincidentTaskTypesDoNotOverlap(t *testing.T) {
	installTaskBins(t, TaskBinSchool, TaskBinRunner)
	var active, maximum, calls atomic.Int32
	old := newScriptCmd
	newScriptCmd = func(string, ...string) scriptRunner { return &spacingBatchCommand{&active, &maximum, &calls} }
	t.Cleanup(func() { newScriptCmd = old })
	s := New(Config{AccountDelayMinSeconds: 30, AccountDelayMaxSeconds: 120})
	s.runBatch(context.Background(), []taskKind{taskSchool, taskCat})
	if maximum.Load() != 1 || calls.Load() != 2 {
		t.Fatalf("batches overlapped: max=%d calls=%d", maximum.Load(), calls.Load())
	}
}

func jitterScheduler(minutes int) *Scheduler {
	return New(Config{
		RandomDelayMinutes: minutes, RandomDelaySeed: "synthetic-deployment",
		CheckinAt: []int{9 * 60}, TravelDisabled: true, ActivityDisabled: true,
		KeepaliveDisabled: true, SchoolDisabled: true, CatDisabled: true,
	})
}

func TestRandomScheduleZeroPreservesExactHour(t *testing.T) {
	s := jitterScheduler(0)
	now := time.Date(2026, 10, 14, 8, 30, 0, 0, cstZone)
	at, kinds := s.nextWake(now)
	want := time.Date(2026, 10, 14, 9, 0, 0, 0, cstZone)
	if !at.Equal(want) || len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Fatalf("zero jitter: at=%s kinds=%v", at, kinds)
	}
}

func TestRandomScheduleStableAcrossRestartAndCatchup(t *testing.T) {
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, cstZone)
	s := jitterScheduler(60)
	at := s.scheduledSlot(day, 9*60, taskCheckin)
	if !at.Equal(jitterScheduler(60).scheduledSlot(day, 9*60, taskCheckin)) {
		t.Fatal("restart redrew the planned slot")
	}
	next, _ := s.nextWake(day)
	due := s.dueSlots(day, at)
	if !next.Equal(at) || len(due) != 1 || !due[0].planned.Equal(at) {
		t.Fatalf("nextWake/dueSlots disagree: next=%s due=%v want=%s", next, due, at)
	}
	if due := s.dueSlots(at, at.Add(time.Minute)); len(due) != 0 {
		t.Fatalf("already dispatched slot replayed: %v", due)
	}
}

func TestRandomScheduleBoundsStayInOriginalHourAndDay(t *testing.T) {
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, cstZone)
	// 时点是「自 00:00 起的分钟数」：整点、半点与一天最后一分钟都必须
	//   ① 落在时点所在的那一小时内、同一自然日内；
	//   ② 偏移不超过 random_delay_minutes，且被钳到「本小时结束前」。
	for _, delayMin := range []int{1, 30, 60, 1000000} {
		s := jitterScheduler(delayMin)
		for d := 0; d < 30; d++ {
			date := day.AddDate(0, 0, d)
			for _, mod := range []int{0, 30, 7*60 + 30, 23*60 + 59} {
				for kind := taskCheckin; kind <= taskCat; kind++ {
					at := s.scheduledSlot(date, mod, kind)
					base := time.Date(date.Year(), date.Month(), date.Day(), mod/60, mod%60, 0, 0, cstZone)
					if at.Hour() != base.Hour() || at.Format("2006-01-02") != date.Format("2006-01-02") {
						t.Fatalf("crossed hour/day: delay=%dm mod=%d at=%s", delayMin, mod, at)
					}
					limit := time.Duration(delayMin) * time.Minute
					if delayMin >= 60 {
						limit = time.Hour - time.Second
					}
					if remain := time.Duration((59*60+59)-(mod%60)*60) * time.Second; limit > remain {
						limit = remain
					}
					if off := at.Sub(base); off < 0 || off > limit {
						t.Fatalf("out of jitter bounds: at=%s base=%s limit=%s", at, base, limit)
					}
				}
			}
		}
	}
}

func TestRandomScheduleNewDateTaskAndDeploymentDrawIndependently(t *testing.T) {
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, cstZone)
	s := jitterScheduler(60)
	at := s.scheduledSlot(day, 9*60, taskCheckin)
	other := jitterScheduler(60)
	other.cfg.RandomDelaySeed = "other-synthetic-deployment"
	for name, actual := range map[string]time.Duration{
		"date":       s.scheduledSlot(day.AddDate(0, 0, 1), 9, taskCheckin).Sub(day.AddDate(0, 0, 1).Add(9 * time.Hour)),
		"task":       s.scheduledSlot(day, 9, taskTravel).Sub(day.Add(9 * time.Hour)),
		"deployment": other.scheduledSlot(day, 9, taskCheckin).Sub(day.Add(9 * time.Hour)),
	} {
		if actual == at.Sub(day.Add(9*time.Hour)) {
			t.Fatalf("fixture %s draw was unchanged", name)
		}
	}
}

func TestRandomScheduleRestartWithinHourStillFindsPendingSlot(t *testing.T) {
	s := jitterScheduler(60)
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, cstZone)
	at := s.scheduledSlot(day, 9*60, taskCheckin)
	if !at.After(day.Add(9 * time.Hour)) {
		t.Fatal("fixture must have a positive delay")
	}
	next, kinds := s.nextWake(at.Add(-time.Second))
	if !next.Equal(at) || len(kinds) != 1 {
		t.Fatalf("lost pending randomized slot: %s %v", next, kinds)
	}
	next, _ = s.nextWake(at)
	if next.Format("2006-01-02") != "2026-10-15" {
		t.Fatalf("slot replayed instead of tomorrow: %s", next)
	}
}

func TestRandomScheduleDisabledTaskAndMultipleTimes(t *testing.T) {
	s := jitterScheduler(60)
	s.cfg.CheckinDisabled = true
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, cstZone)
	if at, kinds := s.nextWake(day); !at.IsZero() || len(kinds) != 0 {
		t.Fatal("disabled task was scheduled")
	}
	if due := s.dueSlots(day, day.Add(24*time.Hour)); len(due) != 0 {
		t.Fatal("disabled task caught up")
	}
	s.cfg.CheckinDisabled = false
	s.cfg.CheckinAt = []int{9 * 60, 21 * 60}
	due := s.dueSlots(day, day.Add(24*time.Hour))
	if len(due) != 2 || due[0].planned.Hour() != 9 || due[1].planned.Hour() != 21 {
		t.Fatalf("multiple-hour slots changed: %v", due)
	}
}
