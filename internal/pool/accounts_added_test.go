// accounts_added_test.go 「新账号入池」回调：运行期新纳管的账号（扫码加号 / 手工放
// 凭证文件，由 auths 目录监听捡起）要靠这个回调让调度器补当天的任务。
package pool

import (
	"testing"
	"time"

	"wp/internal/auth"
)

// TestSyncToDirNotifiesOnlyNewAccounts 只有**本次新增**的 UID 会被回调上报：
// 已在池里的账号重复同步不重复报（否则每次目录变化都会把整池重补一遍）。
func TestSyncToDirNotifiesOnlyNewAccounts(t *testing.T) {
	p := New("")
	var calls [][]string
	p.SetAccountsAddedHook(func(uids []string) { calls = append(calls, uids) })

	p.SyncToDir([]*auth.Auth{{UID: "u1"}, {UID: "u2"}})
	p.SyncToDir([]*auth.Auth{{UID: "u1"}, {UID: "u2"}}) // 无新增
	p.SyncToDir([]*auth.Auth{{UID: "u1"}, {UID: "u2"}, {UID: "u3"}})
	p.SyncToDir([]*auth.Auth{{UID: "u2"}, {UID: "u3"}}) // 删掉 u1：不算新增

	if len(calls) != 2 {
		t.Fatalf("回调次数=%d want 2（两批新增）: %v", len(calls), calls)
	}
	if got := calls[0]; len(got) != 2 || got[0] != "u1" || got[1] != "u2" {
		t.Errorf("首批新增=%v want [u1 u2]", got)
	}
	if got := calls[1]; len(got) != 1 || got[0] != "u3" {
		t.Errorf("第二批新增=%v want [u3]", got)
	}
}

// TestAddNotifiesNewAccount 单账号 Add 同样上报；已存在的账号不算新增。
func TestAddNotifiesNewAccount(t *testing.T) {
	p := New("")
	var got []string
	p.SetAccountsAddedHook(func(uids []string) { got = append(got, uids...) })

	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u1"}) // 已存在（仅换凭证）
	p.Add(&auth.Auth{UID: "u2"})

	if len(got) != 2 || got[0] != "u1" || got[1] != "u2" {
		t.Fatalf("新增上报=%v want [u1 u2]", got)
	}
}

// TestAccountsAddedHookRunsOutsideLock 回调必须在**池锁之外**触发：
// 调度器收到通知后要 List/CheckedInToday，若回调在持锁下执行就自死锁。
// 这里让回调读池，并把 SyncToDir 放 goroutine 里跑（真死锁表现为超时而不是挂住整包）。
func TestAccountsAddedHookRunsOutsideLock(t *testing.T) {
	p := New("")
	var n int
	p.SetAccountsAddedHook(func(uids []string) {
		n = len(p.List())     // 持锁回调会卡死在这里
		_ = p.AvailableUIDs() // 再读写锁一次
	})

	done := make(chan struct{})
	go func() {
		p.SyncToDir([]*auth.Auth{{UID: "u1"}, {UID: "u2"}})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SyncToDir 卡住：新账号回调可能在池锁内被触发（会死锁）")
	}
	if n != 2 {
		t.Errorf("回调里读到的账号数=%d want 2", n)
	}
}

// TestNilHookIsNoop 未注册回调时增删账号照常工作（不 panic）。
func TestNilHookIsNoop(t *testing.T) {
	p := New("")
	p.SyncToDir([]*auth.Auth{{UID: "u1"}})
	p.Add(&auth.Auth{UID: "u2"})
	if got := len(p.AvailableUIDs()); got != 2 {
		t.Fatalf("账号数=%d want 2", got)
	}
}
