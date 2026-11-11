// flush_cadence_test.go 两级落盘节奏（关键状态 5s 上界 / 软计数合批）的契约测试。
//
// 背景：纯接流的部署里每个成功请求都会改「成功计数 / 成本台账 / 余额内插估算」，
// 这些软计数若跟随关键状态每 5s 全量重写 state.json，写放大没有任何收益
// （100 号实测：105 KB × 12 次/分）。所以拆成两级脏位：
//   - dirty     关键状态（禁用/休养/签到日/冷却/熔断/刷新风控）→ flushInterval 内落盘；
//   - softDirty 软计数（NoteModelCost/NoteSuccess 的计数与余额估算）→ softFlushInterval 合批。
package pool

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"wp/internal/auth"
)

// withFlushCadence 临时把落盘节奏调到毫秒级（默认 5s/30s 在测试里等不起）。
func withFlushCadence(t *testing.T, flush, soft time.Duration) {
	t.Helper()
	oldFlush, oldSoft := flushInterval, softFlushInterval
	flushInterval, softFlushInterval = flush, soft
	t.Cleanup(func() { flushInterval, softFlushInterval = oldFlush, oldSoft })
}

// waitSaves 等到落盘次数达到 want（或超时）。
func waitSaves(t *testing.T, p *Pool, base int64, want int64, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p.saves.Load()-base >= want {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return p.saves.Load()-base >= want
}

// TestSoftCountersCoalesced 软计数在 softFlushInterval 内不落盘，到点后合并成一次写入。
func TestSoftCountersCoalesced(t *testing.T) {
	withFlushCadence(t, 10*time.Millisecond, 250*time.Millisecond)
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"})
	p.Flush() // 基线（Add 后的初始状态）
	base := p.saves.Load()

	// 20 次软变更，跨过多个 flush tick（每个 tick 10ms），但仍在 soft 间隔内。
	for i := 0; i < 20; i++ {
		p.NoteModelCost("u1", "m", 1.5, 1000)
		time.Sleep(5 * time.Millisecond)
	}
	if n := p.saves.Load() - base; n != 0 {
		t.Fatalf("软计数在 soft 间隔内不应落盘，实际写了 %d 次", n)
	}

	// 到点后应落盘，且是合批（不是 20 次）。
	if !waitSaves(t, p, base, 1, 500*time.Millisecond) {
		t.Fatal("软计数到 softFlushInterval 后应落盘")
	}
	if n := p.saves.Load() - base; n > 2 {
		t.Fatalf("软计数应合批：20 次变更写了 %d 次盘", n)
	}
}

// TestSoftFlushWriteCountAt100Accounts 100 号持续软变更时的写盘次数：验证"合批"
// 在池规模下依然成立（这是写放大的主场景——纯接流时每个成功请求都改软计数）。
//
// 时间按比例缩放（真实 5s/30s → 测试 20ms/200ms，比例同为 1:6）：
//   - 旧行为（软计数跟随 flushInterval）：300ms 内约 15 次写盘；
//   - 新行为：只在 softFlushInterval 边界写 1～2 次。
func TestSoftFlushWriteCountAt100Accounts(t *testing.T) {
	withFlushCadence(t, 20*time.Millisecond, 200*time.Millisecond)
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	uids := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		uid := fmt.Sprintf("u%03d", i)
		uids = append(uids, uid)
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt"})
	}
	p.Flush()
	base := p.saves.Load()

	// 300ms 内持续制造软变更（约 60 轮 × 100 号 × 2 类 = 上万次）。
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		for _, uid := range uids {
			p.NoteSuccess(uid)
			p.NoteModelCost(uid, "glm-5.2", 1.0, 100)
		}
		time.Sleep(5 * time.Millisecond)
	}
	n := p.saves.Load() - base
	if n == 0 {
		t.Fatal("软变更应在 softFlushInterval 到点时落盘")
	}
	if n > 3 {
		t.Fatalf("100 号软变更应合批（约 1～2 次），实际写了 %d 次", n)
	}
}

// TestCriticalStateFlushesWithinInterval 关键状态（这里是冷却）必须在 flushInterval 内落盘。
func TestCriticalStateFlushesWithinInterval(t *testing.T) {
	withFlushCadence(t, 10*time.Millisecond, 10*time.Second)
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"})
	p.Flush()
	base := p.saves.Load()

	p.Cooldown("u1", CoolSoft, time.Minute, "429 rate limit")
	if !waitSaves(t, p, base, 1, 300*time.Millisecond) {
		t.Fatal("关键状态应在 flushInterval 内落盘")
	}
}

// TestNoChangeNoWrite 无变更时不写盘（ticker 空转）。
func TestNoChangeNoWrite(t *testing.T) {
	withFlushCadence(t, 10*time.Millisecond, 250*time.Millisecond)
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"})
	p.Flush()
	base := p.saves.Load()

	time.Sleep(150 * time.Millisecond) // 15 个 tick
	if n := p.saves.Load() - base; n != 0 {
		t.Fatalf("无变更不应写盘，实际写了 %d 次", n)
	}
}

// TestFlushWritesSoftDirty Flush（优雅停机）必须把软计数一起落盘，不能等 soft 间隔。
func TestFlushWritesSoftDirty(t *testing.T) {
	withFlushCadence(t, time.Hour, time.Hour) // 关掉后台写，只验证 Flush
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt"})
	p.Flush()
	base := p.saves.Load()

	p.NoteModelCost("u1", "glm-5.2", 2.0, 500)
	if !p.softDirty.Load() {
		t.Fatal("NoteModelCost 应置软脏位")
	}
	p.Flush()
	if n := p.saves.Load() - base; n != 1 {
		t.Fatalf("Flush 应落盘一次（含软计数），实际 %d", n)
	}
	if p.softDirty.Load() {
		t.Fatal("Flush 后软脏位应清零")
	}
}

// TestNoteSuccessDirtyClassification 成功请求默认属软计数；但若它清掉了会被持久化的
// 失败/熔断/降权状态，则必须按关键状态落盘（否则重启后可能带着已解除的熔断复活）。
func TestNoteSuccessDirtyClassification(t *testing.T) {
	// 无 state 文件 → 不起后台 flusher，脏位保持可观测。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.NoteSuccess("u1")
	if p.dirty.Load() {
		t.Error("纯累加成功计数不应置关键脏位")
	}
	if !p.softDirty.Load() {
		t.Error("纯累加成功计数应置软脏位")
	}

	// 制造熔断，再让一次成功把它清掉。
	p.dirty.Store(false)
	p.softDirty.Store(false)
	p.SetBreaker(1, time.Minute, time.Hour)
	p.NoteError("u1")
	if st, _ := p.Status("u1"); st.BreakerUntil.IsZero() {
		t.Fatal("precondition: 应已熔断")
	}
	p.dirty.Store(false)
	p.softDirty.Store(false)

	p.NoteSuccess("u1")
	if !p.dirty.Load() {
		t.Error("清掉熔断成功应置关键脏位（5s 内落盘）")
	}
}

// TestNoteErrorDirtyClassification 只累加错误计数是软计数；触发熔断则升为关键状态。
func TestNoteErrorDirtyClassification(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(3, time.Minute, time.Hour)

	p.NoteError("u1") // 1 < 阈值 3 → 未熔断
	if p.dirty.Load() {
		t.Error("未达阈的错误计数不应置关键脏位")
	}
	if !p.softDirty.Load() {
		t.Error("未达阈的错误计数应置软脏位")
	}

	p.dirty.Store(false)
	p.softDirty.Store(false)
	p.NoteError("u1")
	p.NoteError("u1") // 第 3 次 → 熔断
	if !p.dirty.Load() {
		t.Error("触发熔断应置关键脏位")
	}
}
