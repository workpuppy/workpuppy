package loginsvc

import (
	"errors"
	"testing"
	"time"
)

// 过期 / 未知 state 必须是同一个哨兵错误：管理后台据此把二维码标成"已失效"。
func TestErrStateExpiredSentinel(t *testing.T) {
	m, stop := newTestManager(t, false)
	defer stop()
	m.SetTTL(20 * time.Millisecond)

	base := time.Now()
	m.SetClock(func() time.Time { return base })
	state, _, err := m.Start("cn")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	// TTL 内 → 不是过期错误
	if _, _, err := m.Poll("cn", state); err != nil && errors.Is(err, ErrStateExpired) {
		t.Fatalf("TTL 内不应判过期: %v", err)
	}
	// 越过 TTL → 过期哨兵
	m.SetClock(func() time.Time { return base.Add(time.Second) })
	if _, _, err := m.Poll("cn", state); !errors.Is(err, ErrStateExpired) {
		t.Fatalf("过期应返回 ErrStateExpired，实际 %v", err)
	}
	// 未知 state 同样按过期处理（前端表现一致：请重新获取）
	if _, _, err := New().Poll("cn", "no-such-state"); !errors.Is(err, ErrStateExpired) {
		t.Fatalf("未知 state 应返回 ErrStateExpired，实际 %v", err)
	}
}

// TTL 访问器：默认 15 分钟，SetTTL 覆盖，非法值忽略。
func TestTTLAccessor(t *testing.T) {
	m := New()
	if got := m.TTL(); got != defaultTTL {
		t.Errorf("默认 TTL=%v want %v", got, defaultTTL)
	}
	m.SetTTL(2 * time.Minute)
	if got := m.TTL(); got != 2*time.Minute {
		t.Errorf("SetTTL 后=%v want 2m", got)
	}
	m.SetTTL(0)
	if got := m.TTL(); got != 2*time.Minute {
		t.Errorf("SetTTL(0) 应忽略，实际 %v", got)
	}
	m.SetTTL(-time.Minute)
	if got := m.TTL(); got != 2*time.Minute {
		t.Errorf("负值应忽略，实际 %v", got)
	}
}
