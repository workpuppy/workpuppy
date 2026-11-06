package pool

import (
	"testing"
	"time"
)

// Configure 必须真正改掉包级阈值（cmd/server 启动时用配置调用它）。
func TestConfigureAppliesTunings(t *testing.T) {
	oldGap, oldThresh, oldWeight := minPickGap, sessionDeadThreshold, expiringWeight
	oldWatch, oldFlush := watchInterval, flushInterval
	t.Cleanup(func() {
		minPickGap, sessionDeadThreshold, expiringWeight = oldGap, oldThresh, oldWeight
		watchInterval, flushInterval = oldWatch, oldFlush
	})

	Configure(Options{
		MinPickGap:           250 * time.Millisecond,
		SessionDeadThreshold: 5,
		ExpiringWeight:       12,
		WatchInterval:        2 * time.Second,
		FlushInterval:        3 * time.Second,
	})
	if minPickGap != 250*time.Millisecond {
		t.Errorf("minPickGap=%v want 250ms", minPickGap)
	}
	if sessionDeadThreshold != 5 || SessionDeadThreshold() != 5 {
		t.Errorf("sessionDeadThreshold=%d want 5", sessionDeadThreshold)
	}
	if expiringWeight != 12 {
		t.Errorf("expiringWeight=%v want 12", expiringWeight)
	}
	if watchInterval != 2*time.Second || flushInterval != 3*time.Second {
		t.Errorf("watch/flush=%v/%v want 2s/3s", watchInterval, flushInterval)
	}

	// 零值/负值 = "不设置"：不把已有值覆盖成 0（避免误把节奏关掉）。
	Configure(Options{MinPickGap: 0, SessionDeadThreshold: -1})
	if minPickGap != 250*time.Millisecond || sessionDeadThreshold != 5 {
		t.Errorf("无效值不应覆盖：minPickGap=%v threshold=%d", minPickGap, sessionDeadThreshold)
	}
}
