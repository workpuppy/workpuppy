package pool

import (
	"testing"
	"time"

	"wp/internal/auth"
)

// 范围过滤只允许白名单内的账号参与正常选号（多轮抽样）。
func TestScopedPickOnlyAllowed(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 10)
	p.SetCredits("u2", 1000) // 权重更高，但不在范围内

	scope := NewUIDSet([]string{"u1"})
	for i := 0; i < 200; i++ {
		got := p.PickExcludingForRealmScoped(nil, "", "", scope)
		if got == nil || got.UID != "u1" {
			t.Fatalf("pick = %+v, want u1（scope 外账号不得参与）", got)
		}
	}
}

// 空集合表示「一个都不允许」，与 nil（不限）必须区分。
func TestScopedPickEmptySetDeniesAll(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)

	empty := NewUIDSet(nil)
	if got := p.PickExcludingForRealmScoped(nil, "", "", empty); got != nil {
		t.Fatalf("空 scope 应拒绝全部，实际选到 %+v", got)
	}
	if got := p.PickExcludingForRealmScoped(nil, "", "", nil); got == nil {
		t.Fatal("nil scope 应保持不限（现状语义）")
	}
}

// 全冷却兜底同样受 scope 约束：范围内唯一账号在冷却时选它，绝不越界到范围外。
func TestScopedFallbackRespectsScope(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 10)
	p.SetCredits("u2", 1000)
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit")

	got := p.PickExcludingForRealmScoped(nil, "", "", NewUIDSet([]string{"u1"}))
	if got == nil || got.UID != "u1" {
		t.Fatalf("fallback = %+v, want u1（范围外健康号 u2 不得兜底）", got)
	}

	// 范围内唯一账号被禁用：兜底也不得参与 → nil，而不是打到 u2。
	p.Disable("u1", "manual")
	if got := p.PickExcludingForRealmScoped(nil, "", "", NewUIDSet([]string{"u1"})); got != nil {
		t.Fatalf("范围内账号禁用后应返回 nil，实际 %+v", got)
	}
}

func TestScopedAvailableAndCounts(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.Disable("u2", "manual")

	scope := NewUIDSet([]string{"u1", "u2"})
	uids := p.AvailableUIDsForModelRealmScoped("", "", scope)
	if len(uids) != 1 || uids[0] != "u1" {
		t.Errorf("AvailableUIDs scoped = %v, want [u1]", uids)
	}

	total, healthy, cooling, disabled, inFlightFull := p.CountsDetailedScoped(scope)
	if total != 2 || healthy != 1 || cooling != 0 || disabled != 1 || inFlightFull != 0 {
		t.Errorf("counts scoped = total=%d healthy=%d cooling=%d disabled=%d full=%d, want 2/1/0/1/0",
			total, healthy, cooling, disabled, inFlightFull)
	}
	list := p.ListScoped(scope)
	if len(list) != 2 {
		t.Fatalf("ListScoped = %d 条, want 2", len(list))
	}
	for _, st := range list {
		if st.UID == "u3" {
			t.Error("ListScoped 泄漏了范围外账号 u3")
		}
	}
}

func TestScopedPickByUID(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})

	if got := p.PickByUIDForModelScoped("u1", "", NewUIDSet([]string{"u1"})); got == nil {
		t.Fatal("范围内账号应可取")
	}
	if got := p.PickByUIDForModelScoped("u2", "", NewUIDSet([]string{"u1"})); got != nil {
		t.Fatalf("范围外账号不应可取，实际 %+v", got)
	}
	// 无 Scoped 后缀的旧入口保持不限。
	if got := p.PickByUIDForModel("u2", ""); got == nil {
		t.Fatal("旧入口应保持不限")
	}
}
