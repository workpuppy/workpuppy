package session

import (
	"testing"
	"time"
)

type testScope map[string]bool

func (s testScope) Allows(uid string) bool { return s[uid] }

// 范围外的账号不得被分配：即使权重回调想选它，也会因候选集已过滤而回落哈希。
func TestResolveForModelScopedFiltersCandidates(t *testing.T) {
	r := New(Config{
		TTL:       time.Minute,
		Available: func() []string { return []string{"a1", "a2"} },
	})
	r.cfg.PickForModel = func(string, []string) string { return "a2" } // 越界意图

	got, ok := r.ResolveForModelScoped("c1", "m", testScope{"a1": true})
	if !ok || got != "a1" {
		t.Fatalf("scoped resolve = %q,%v want a1,true", got, ok)
	}
}

// 已绑定到范围外账号时必须重分配（旧绑定不能因为"账号健康"继续沿用）。
func TestResolveForModelScopedReassignsOutOfScopeBinding(t *testing.T) {
	r := New(Config{
		TTL:       time.Minute,
		Available: func() []string { return []string{"a1", "a2"} },
	})
	r.Bind("c1", "a2")
	got, ok := r.ResolveForModelScoped("c1", "m", testScope{"a1": true})
	if !ok || got != "a1" {
		t.Fatalf("out-of-scope binding should move to a1, got %q,%v", got, ok)
	}
	if cur, ok := r.ResolveForModel("c1", "m"); !ok || cur != "a1" {
		t.Fatalf("rebind not persisted: %q,%v", cur, ok)
	}
}

// 空范围 = 一把都不允许，不得回落成"不限"。
func TestResolveForModelScopedEmptyScope(t *testing.T) {
	r := New(Config{
		TTL:       time.Minute,
		Available: func() []string { return []string{"a1"} },
	})
	if got, ok := r.ResolveForModelScoped("c1", "m", testScope{}); ok || got != "" {
		t.Fatalf("空范围应无可分配，got %q,%v", got, ok)
	}
}

// 注入 scoped 回调时优先使用它，并且仍会做一次范围过滤（回调疏忽不变成越权）。
func TestResolveForModelScopedUsesScopedCallbacks(t *testing.T) {
	var sawScope Scope
	r := New(Config{
		TTL: time.Minute,
		AvailableForModelScoped: func(_ string, scope Scope) []string {
			sawScope = scope
			return []string{"a3", "a9"} // a9 越界，应被二次过滤
		},
		PickForModelScoped: func(_ string, candidates []string, _ Scope) string { return candidates[0] },
	})
	got, ok := r.ResolveForModelScoped("c1", "m", testScope{"a3": true})
	if sawScope == nil {
		t.Fatal("scoped 可用回调未收到 scope")
	}
	if !ok || got != "a3" {
		t.Fatalf("scoped callback resolve = %q,%v want a3,true", got, ok)
	}
}

// nil scope 与旧入口逐位一致（开闭原则：新层不改旧层）。
func TestResolveForModelNilScopeBackCompat(t *testing.T) {
	r := New(Config{
		TTL:       time.Minute,
		Available: func() []string { return []string{"a1", "a2"} },
	})
	r.cfg.PickForModel = func(_ string, candidates []string) string { return candidates[0] }
	if got, ok := r.ResolveForModelScoped("c1", "m", nil); !ok || got != "a1" {
		t.Fatalf("nil scope = %q,%v want a1,true", got, ok)
	}
	if got, ok := r.ResolveForModel("c2", "m"); !ok || (got != "a1" && got != "a2") {
		t.Fatalf("legacy resolve = %q,%v want one of a1/a2", got, ok)
	}
}
