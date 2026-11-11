package scheduler

import (
	"testing"
	"time"
)

// 包级节奏的**初始值必须来自 config.default.json**（不是散落的字面量）。
func TestPacingDefaultsComeFromDefaultConfig(t *testing.T) {
	d := schedDefaults()
	if d.CheckinAccountDelayDur != 3*time.Second {
		t.Errorf("checkin_account_delay=%v want 3s（应来自 config.default.json）", d.CheckinAccountDelayDur)
	}
	if d.TravelAccountDelayDur != 800*time.Millisecond || d.ActivityAccountDelayDur != 800*time.Millisecond {
		t.Errorf("travel/activity account delay=%v/%v want 800ms",
			d.TravelAccountDelayDur, d.ActivityAccountDelayDur)
	}
	if d.CheckinRefreshSkewDur != 10*time.Minute || d.WakeupGraceDur != 5*time.Second {
		t.Errorf("refresh skew / wakeup grace=%v/%v want 10m/5s",
			d.CheckinRefreshSkewDur, d.WakeupGraceDur)
	}
	if d.CatchupWindowDur != 24*time.Hour {
		t.Errorf("catchup_window=%v want 24h", d.CatchupWindowDur)
	}
	if len(d.CheckinRetryDelaysDur) != 3 || d.CheckinRetryDelaysDur[0] != 5*time.Second {
		t.Errorf("checkin_retry_delays=%v want [5s 15s 60s]", d.CheckinRetryDelaysDur)
	}
}

// Configure 逐字段覆盖（含 0 = 关闭），负值忽略，切片为空覆盖为"不重试"。
func TestConfigureOverridesPacing(t *testing.T) {
	oldDelay, oldTravel, oldAct := checkinAccountDelay, travelAccountDelay, activityAccountDelay
	oldSkew, oldGrace, oldCatchUp := checkinRefreshSkew, wakeupGraceDelay, catchupWindow
	oldRetry := checkinRetryDelays
	t.Cleanup(func() {
		checkinAccountDelay, travelAccountDelay, activityAccountDelay = oldDelay, oldTravel, oldAct
		checkinRefreshSkew, wakeupGraceDelay, catchupWindow = oldSkew, oldGrace, oldCatchUp
		checkinRetryDelays = oldRetry
	})

	Configure(Options{
		CheckinAccountDelay:  250 * time.Millisecond,
		TravelAccountDelay:   0, // 显式关闭
		ActivityAccountDelay: -1 * time.Second,
		CheckinRefreshSkew:   3 * time.Minute,
		CheckinRetryDelays:   []time.Duration{time.Second, 2 * time.Second},
		WakeupGrace:          0,
		CatchupWindow:        2 * time.Hour,
	})
	if checkinAccountDelay != 250*time.Millisecond {
		t.Errorf("checkinAccountDelay=%v want 250ms", checkinAccountDelay)
	}
	if travelAccountDelay != 0 {
		t.Errorf("travelAccountDelay=%v want 0（显式关闭）", travelAccountDelay)
	}
	if activityAccountDelay != oldAct {
		t.Errorf("负值应被忽略：activityAccountDelay=%v want %v", activityAccountDelay, oldAct)
	}
	if checkinRefreshSkew != 3*time.Minute || wakeupGraceDelay != 0 {
		t.Errorf("refresh skew / grace=%v/%v want 3m/0", checkinRefreshSkew, wakeupGraceDelay)
	}
	if catchupWindow != 2*time.Hour {
		t.Errorf("catchupWindow=%v want 2h", catchupWindow)
	}
	if len(checkinRetryDelays) != 2 || checkinRetryDelays[1] != 2*time.Second {
		t.Errorf("retry delays=%v want [1s 2s]", checkinRetryDelays)
	}
}
