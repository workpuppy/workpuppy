package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"wp/internal/auth"
)

// SetCat 记录猫猫状态快照：自动填 UpdatedAt、对外只返回副本（调用方改不到池内）。
func TestSetCatClonesAndExposesStatus(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	if !p.SetCat("u1", CatState{State: "traveling", Buddy: "喵喵", Reward: 12}) {
		t.Fatal("SetCat 对已入池账号应返回 true")
	}
	cat, ok := p.CatOf("u1")
	if !ok || cat.State != "traveling" || cat.Buddy != "喵喵" || cat.Reward != 12 {
		t.Fatalf("CatOf=%+v ok=%v", cat, ok)
	}
	if cat.UpdatedAt.IsZero() {
		t.Error("UpdatedAt 应自动填上（账号页要用它显示「更新于 x 分钟前」）")
	}

	// 返回值是副本：改它不该影响池内状态。
	cat.State = "arrived"
	if again, _ := p.CatOf("u1"); again.State != "traveling" {
		t.Error("CatOf 应返回副本，调用方修改不应写回池内")
	}

	// Status（/status 与 /admin/accounts 共用）里也要带出来，且同样是副本。
	st, ok := p.Status("u1")
	if !ok || st.Cat == nil || st.Cat.State != "traveling" {
		t.Fatalf("Status.Cat 缺失: %+v ok=%v", st.Cat, ok)
	}
	st.Cat.State = "hacked"
	if again, _ := p.CatOf("u1"); again.State != "traveling" {
		t.Error("Status.Cat 应是副本，调用方修改不应写回池内")
	}

	// 未知 uid 不 panic、返回 false。
	if p.SetCat("nope", CatState{State: "idle"}) {
		t.Error("未知 uid 的 SetCat 应返回 false")
	}
}

// 猫猫状态随 state.json 持久化：重启后账号页的猫猫列不会变空。
func TestCatStatePersistsAcrossRestart(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.SetCat("u1", CatState{State: "arrived", Buddy: "档案喵", Reward: 30, DailyLimitReached: true})
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("读 state.json: %v", err)
	}
	var sf stateFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		t.Fatalf("解析 state.json: %v", err)
	}
	if got := sf.Accounts["u1"].Cat; got == nil || got.State != "arrived" || got.Reward != 30 || got.Buddy != "档案喵" {
		t.Fatalf("落盘的 cat 不对: %+v", got)
	}

	// 重新加载（无 Redis 快照 → 读本地 state.json）。
	p2 := New(fp)
	p2.SetStore(&memStore{loadOK: false})
	p2.RestoreFromSnapshot()
	cat, ok := p2.CatOf("u1")
	if !ok || cat.State != "arrived" || cat.Buddy != "档案喵" || !cat.DailyLimitReached || cat.Reward != 30 {
		t.Fatalf("重启后猫状态丢失: %+v ok=%v", cat, ok)
	}
}
