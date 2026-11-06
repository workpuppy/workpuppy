package upstream

import (
	"testing"
	"time"
)

// Configure 必须真正改掉包级阈值（cmd/server 启动时用配置调用它）。
func TestConfigureAppliesTunings(t *testing.T) {
	oldRetry, oldRefresh := retryAfterSanity, refreshIOTimeout
	oldGzip, oldTTL, oldMax := gzipBodyThreshold, deviceTokenFileTTL, deviceTokenFileMaxLen
	oldCtx := DefaultContextWindow
	oldCatTimeout, oldCatCool, oldCatTTL := modelsDevTimeout, modelsDevFetchCooldown, modelsDevNegativeTTL
	t.Cleanup(func() {
		retryAfterSanity, refreshIOTimeout = oldRetry, oldRefresh
		gzipBodyThreshold, deviceTokenFileTTL, deviceTokenFileMaxLen = oldGzip, oldTTL, oldMax
		DefaultContextWindow = oldCtx
		modelsDevTimeout, modelsDevFetchCooldown, modelsDevNegativeTTL = oldCatTimeout, oldCatCool, oldCatTTL
	})

	Configure(Options{
		RetryAfterMax:          30 * time.Minute,
		RefreshIOTimeout:       5 * time.Second,
		GzipThresholdBytes:     1024,
		DeviceTokenFileTTL:     time.Minute,
		DeviceTokenFileMaxLen:  2048,
		DefaultContextWindow:   128000,
		CatalogTimeout:         2 * time.Second,
		CatalogRefreshCooldown: 10 * time.Minute,
		CatalogNegativeTTL:     time.Hour,
	})
	if retryAfterSanity != 30*time.Minute || refreshIOTimeout != 5*time.Second {
		t.Errorf("retry/refresh=%v/%v want 30m/5s", retryAfterSanity, refreshIOTimeout)
	}
	if gzipBodyThreshold != 1024 || deviceTokenFileMaxLen != 2048 || deviceTokenFileTTL != time.Minute {
		t.Errorf("gzip/device=%v/%v/%v", gzipBodyThreshold, deviceTokenFileMaxLen, deviceTokenFileTTL)
	}
	if DefaultContextWindow != 128000 {
		t.Errorf("DefaultContextWindow=%d want 128000", DefaultContextWindow)
	}
	if modelsDevTimeout != 2*time.Second || modelsDevFetchCooldown != 10*time.Minute || modelsDevNegativeTTL != time.Hour {
		t.Errorf("catalog=%v/%v/%v want 2s/10m/1h", modelsDevTimeout, modelsDevFetchCooldown, modelsDevNegativeTTL)
	}

	// 0 = 不设置（保留已有值）；GzipThresholdBytes 特殊：0 是合法值（不压缩）
	Configure(Options{RetryAfterMax: 0, GzipThresholdBytes: 0})
	if retryAfterSanity != 30*time.Minute {
		t.Errorf("0 不应覆盖已有值：retryAfterSanity=%v", retryAfterSanity)
	}
	if gzipBodyThreshold != 0 {
		t.Errorf("gzip 阈值 0 应生效（不压缩）：%v", gzipBodyThreshold)
	}
}
