package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/rand"
	"time"
)

type scheduledAccountKey struct{}

// accountGap adds jitter to the original minimum pacing, only on scheduled
// traversals. Manual calls and a zero range keep their existing behavior.
func (s *Scheduler) accountGap(ctx context.Context, base time.Duration) time.Duration {
	if ctx.Value(scheduledAccountKey{}) != true || s.cfg.AccountDelayMaxSeconds <= 0 {
		return base
	}
	lo, hi := s.cfg.AccountDelayMinSeconds, s.cfg.AccountDelayMaxSeconds
	if lo < 0 || hi < lo || hi > 600 {
		return base
	}
	seconds := lo + rand.Intn(hi-lo+1)
	return base + time.Duration(seconds)*time.Second
}

func (s *Scheduler) accountScriptArgs(ctx context.Context, args []string) []string {
	if ctx.Value(scheduledAccountKey{}) != true || s.cfg.AccountDelayMaxSeconds <= 0 {
		return args
	}
	return append(args, "--account-delay-min", fmt.Sprint(s.cfg.AccountDelayMinSeconds),
		"--account-delay-max", fmt.Sprint(s.cfg.AccountDelayMaxSeconds))
}

// scheduledSlot 由稳定私钥派生出「这个时点的随机偏移」：同一时点在重启后仍是同一
// 偏移（不会因为重启就变），而每个日期/时点/任务类型/部署各自独立抽签，无常驻随机状态。
//
// minuteOfDay 是**自 00:00 起的分钟数**（0..1439，如 570 = 09:30）。
// 偏移窗口被钳在**该时点所在小时的剩余部分**内：09:30 的抖动不会越过 10:00 去撞另一个
// 时点；整点（xx:00）时上限仍是 59:59，与既有行为一致。
func (s *Scheduler) scheduledSlot(day time.Time, minuteOfDay int, kind taskKind) time.Time {
	at := time.Date(day.Year(), day.Month(), day.Day(), minuteOfDay/60, minuteOfDay%60, 0, 0, day.Location())
	minutes := s.cfg.RandomDelayMinutes
	if minutes <= 0 {
		return at
	}
	if minutes > 60 {
		minutes = 60
	}
	seconds := minutes * 60
	// 距离本小时结束还有多少秒（59:59 收口）：保证抖动不跨小时。
	remain := 59*60 + 59 - (minuteOfDay%60)*60
	if remain < 0 {
		remain = 0
	}
	if seconds > remain {
		seconds = remain
	}
	identity := fmt.Sprintf("%s\x00%s\x00%d\x00%d", s.cfg.RandomDelaySeed,
		at.Format("2006-01-02"), minuteOfDay, kind)
	digest := sha256.Sum256([]byte(identity))
	offset := binary.BigEndian.Uint64(digest[:8]) % uint64(seconds+1)
	return at.Add(time.Duration(offset) * time.Second)
}

func (s *Scheduler) nextScheduledFire(now time.Time, sch schedule) time.Time {
	if s.cfg.RandomDelayMinutes <= 0 {
		return nextFire(now, sch.at)
	}
	var earliest time.Time
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for i := 0; i < 2; i++ {
		for _, at := range sch.at {
			fire := s.scheduledSlot(day.AddDate(0, 0, i), at, sch.kind)
			if fire.After(now) && (earliest.IsZero() || fire.Before(earliest)) {
				earliest = fire
			}
		}
	}
	return earliest
}
