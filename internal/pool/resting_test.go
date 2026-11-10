package pool

import (
	"testing"

	"wp/internal/auth"
)

// 休养：不参与选号（Pick / AvailableUIDs），但账号本身仍然"活着"
// （签到/上报等自动任务走 Pool.List() + Status，不受影响）。
func TestRestingExcludedFromSelection(t *testing.T) {
	p := New("")
	defer p.Close()
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})

	if got := p.Pick("m"); got == nil {
		t.Fatal("正常账号应可被选中")
	}
	found, changed := p.SetResting("u1", true)
	if !found || !changed {
		t.Fatalf("SetResting = (%v,%v) want (true,true)", found, changed)
	}
	if resting, ok := p.RestingState("u1"); !ok || !resting {
		t.Fatalf("RestingState = (%v,%v)", resting, ok)
	}
	// 不再被选中
	if got := p.Pick("m"); got != nil {
		t.Fatalf("休养中不应被选中，实际 %s", got.UID)
	}
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("AvailableUIDs=%v 应为空", uids)
	}
	// 但仍在池里（List 可见，供自动任务遍历）
	var seen bool
	for _, st := range p.List() {
		if st.UID == "u1" {
			seen = true
			if !st.Resting {
				t.Error("Status.Resting 应为 true")
			}
			if st.Disabled || st.ManualDisabled {
				t.Error("休养不应被当成停用")
			}
		}
	}
	if !seen {
		t.Fatal("休养账号应从 List() 里可见（否则自动任务会漏掉它）")
	}

	// 幂等：再开一次 changed=false
	if _, changed := p.SetResting("u1", true); changed {
		t.Error("重复开休养不该 changed")
	}
	// 恢复后可再次被选中
	if _, changed := p.SetResting("u1", false); !changed {
		t.Error("解除休养应 changed")
	}
	if got := p.Pick("m"); got == nil {
		t.Fatal("解除休养后应恢复可选")
	}
	// 未知 uid
	if found, _ := p.SetResting("nope", true); found {
		t.Error("未知 uid 应 found=false")
	}
}

// 休养状态要持久化（重启保留运维意图）。
func TestRestingPersisted(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1", RefreshToken: "rt", ExpiresAt: 9999999999})
	if found, _ := p.SetResting("u1", true); !found {
		t.Fatal("SetResting 失败")
	}
	p.Flush()
	p.Close()

	p2 := New(fp)
	defer p2.Close()
	p2.Add(&auth.Auth{UID: "u1", RefreshToken: "rt", ExpiresAt: 9999999999})
	if resting, ok := p2.RestingState("u1"); !ok || !resting {
		t.Fatalf("重启后休养状态丢失：resting=%v ok=%v", resting, ok)
	}
	// 休养账号仍不可选
	if got := p2.Pick("m"); got != nil {
		t.Fatalf("重启后休养账号不应可被选中，实际 %s", got.UID)
	}
}
