// credit_sink_test.go 权威回写的记账链路：积分日账本里「获得 / 调整」的唯一来源。
//
// 回归背景（生产问题：2026-11-11 / 11-12 两天 earned 与 adjust 始终为 0，只有请求扣费
// 有数）：签到与「刷新余额」的权威回写是「ReenableIfCredits（静默改写 credits）→
// SetCreditsDetailed（写同一个值，补快过期分桶）」两步。后者计算 delta = 新值 − 当前值
// 恒为 0，于是 creditSink 永远拿不到差额 —— 签到赚的积分、过期/回收造成的减少都没记账。
// 修复后由 ReenableIfCredits 负责上报差额；setCredits 只在差额非 0 时上报，因此上面
// 这条生产顺序不会重复记账。
package pool

import (
	"testing"

	"wp/internal/auth"
)

// creditEvent 捕获到的一条积分流水（与 creditSink 签名一一对应）。
type creditEvent struct {
	uid     string
	realm   string
	delta   int64
	spent   int64
	balance int64
}

// captureCredit 接管 creditSink（包级回调），返回收集切片；测试结束自动还原。
// 包内测试都不并行（无 t.Parallel），所以临时改写包级变量是安全的。
func captureCredit(t *testing.T) *[]creditEvent {
	t.Helper()
	old := creditSink
	events := &[]creditEvent{}
	creditSink = func(uid, realm string, delta, spent, balance int64) {
		*events = append(*events, creditEvent{uid: uid, realm: realm, delta: delta, spent: spent, balance: balance})
	}
	t.Cleanup(func() { creditSink = old })
	return events
}

// TestAuthoritativeWritebackRecordsEarned 签到让余额增加 → 恰好一条 delta>0 的流水。
func TestAuthoritativeWritebackRecordsEarned(t *testing.T) {
	events := captureCredit(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Domain: "www.codebuddy.cn"})

	p.SetCreditsDetailed("u1", 1000, 0) // 首笔余额（自身也是一次入账）
	*events = (*events)[:0]             // 只关心结算那一步

	// 生产顺序：先解冻+上报差额，再补写快过期分桶。
	p.ReenableIfCredits("u1", 1600)
	p.SetCreditsDetailed("u1", 1600, 0)

	ev := *events
	if len(ev) != 1 {
		t.Fatalf("应恰好上报一条流水（不得重复记账），实际 %d 条：%+v", len(ev), ev)
	}
	if ev[0].delta != 600 || ev[0].balance != 1600 || ev[0].spent != 0 {
		t.Errorf("流水 = %+v want delta=600 balance=1600 spent=0", ev[0])
	}
	if ev[0].uid != "u1" || ev[0].realm != "cn" {
		t.Errorf("uid/realm = %q/%q want u1/cn", ev[0].uid, ev[0].realm)
	}
}

// TestAuthoritativeWritebackRecordsAdjust 权威余额低于池内记录 → 一条 delta<0 的流水。
func TestAuthoritativeWritebackRecordsAdjust(t *testing.T) {
	events := captureCredit(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 1000, 0)
	*events = (*events)[:0]

	p.ReenableIfCredits("u1", 850) // 模拟过期/上游回收 150
	p.SetCreditsDetailed("u1", 850, 0)

	ev := *events
	if len(ev) != 1 {
		t.Fatalf("应恰好上报一条流水，实际 %d 条：%+v", len(ev), ev)
	}
	if ev[0].delta != -150 || ev[0].balance != 850 {
		t.Errorf("流水 = %+v want delta=-150 balance=850", ev[0])
	}
}

// TestAuthoritativeWritebackNoChangeNoEvent 余额没变化 → 不产生流水（避免重复喂日账本）。
func TestAuthoritativeWritebackNoChangeNoEvent(t *testing.T) {
	events := captureCredit(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 1000, 0)
	*events = (*events)[:0]

	p.ReenableIfCredits("u1", 1000)
	p.SetCreditsDetailed("u1", 1000, 0)

	if n := len(*events); n != 0 {
		t.Fatalf("余额未变不应上报，实际 %d 条：%+v", n, *events)
	}
}

// TestRequestSpendStillRecordsSpent 请求扣费仍按 spent 记账（delta 恒 0，与本次修复无关）。
func TestRequestSpendStillRecordsSpent(t *testing.T) {
	events := captureCredit(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 1000, 0)
	*events = (*events)[:0]

	p.NoteModelCost("u1", "glm-5.2", 25, 1000) // 扣 25 积分

	ev := *events
	if len(ev) != 1 {
		t.Fatalf("应上报一条 spent 流水，实际 %d 条：%+v", len(ev), ev)
	}
	if ev[0].spent != 25 || ev[0].delta != 0 || ev[0].balance != 975 {
		t.Errorf("流水 = %+v want spent=25 delta=0 balance=975", ev[0])
	}
}
