package accesskey

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func newTempStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	st := New(path)
	if err := st.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return st, path
}

// 新建 → 落盘 → 重新 Load → 按密钥查回；落盘权限必须是 0600（文件里是明文 key）。
func TestCreateLookupPersist(t *testing.T) {
	st, path := newTempStore(t)
	k, err := st.Create(Input{Name: "team-a", AllAccounts: true, AllModels: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if k.ID == "" || k.Key == "" || !k.Enabled || !k.AllAccounts || !k.AllModels {
		t.Fatalf("unexpected key: %+v", k)
	}
	if len(k.Key) < 16 || k.Key[:3] != "wp-" {
		t.Errorf("生成的 key 形态异常: %q", k.Key)
	}

	st2 := New(path)
	if err := st2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := st2.Lookup(k.Key)
	if !ok || got.ID != k.ID || got.Name != "team-a" {
		t.Fatalf("Lookup after reload = %+v, %v", got, ok)
	}
	if _, ok := st2.Lookup("wp-nope"); ok {
		t.Error("不存在的 key 不应命中")
	}

	// Windows 的 os.Chmod 只映射只读位，不呈现 POSIX 权限；只在类 Unix 上断言。
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("文件权限 = %v，want 0600（明文密钥）", perm)
		}
	}
}

// 空列表 + 未勾「全部」= 显式拒绝：避免漏配把整池放出去。
func TestValidationRequiresExplicitScopes(t *testing.T) {
	st, _ := newTempStore(t)
	cases := []struct {
		name string
		in   Input
	}{
		{"both empty", Input{Name: "x"}},
		{"accounts only", Input{Name: "x", Accounts: []string{"u1"}}},
		{"models only", Input{Name: "x", Models: []string{"cn:m"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := st.Create(c.in); err == nil {
				t.Fatal("want validation error")
			}
		})
	}
	// 勾「全部」可以没有列表。
	if _, err := st.Create(Input{Name: "all", AllAccounts: true, AllModels: true}); err != nil {
		t.Fatalf("all flags should pass: %v", err)
	}
	// 两个列表都非空也可以。
	if _, err := st.Create(Input{Name: "list", Accounts: []string{"u1"}, Models: []string{"cn:m"}}); err != nil {
		t.Fatalf("explicit lists should pass: %v", err)
	}
}

// AllowsModel：完整 id 精确匹配；裸名等价 cn:；跨域必须单独勾选。
func TestAllowsModel(t *testing.T) {
	k := Key{Models: []string{"cn:deepseek-v4-flash", "global:gpt-5.4"}}
	cases := []struct {
		model string
		want  bool
	}{
		{"cn:deepseek-v4-flash", true},
		{"deepseek-v4-flash", true}, // 裸名 = cn 域
		{"global:deepseek-v4-flash", false},
		{"global:gpt-5.4", true},
		{"gpt-5.4", false}, // 裸名是 cn，不等于 global 那份
		{"cn:gpt-5.4", false},
		{"", false},
	}
	for _, c := range cases {
		if got := k.AllowsModel(c.model); got != c.want {
			t.Errorf("AllowsModel(%q) = %v, want %v", c.model, got, c.want)
		}
	}
	// 勾「全部模型」后不受白名单限制。
	all := Key{AllModels: true}
	if !all.AllowsModel("global:whatever") {
		t.Error("AllModels 应允许任意模型")
	}
}

func TestAllowsAccount(t *testing.T) {
	k := Key{Accounts: []string{"u1", "u2"}}
	if !k.AllowsAccount("u1") || k.AllowsAccount("u3") {
		t.Errorf("Accounts 白名单判定错误: %+v", k)
	}
	all := Key{AllAccounts: true}
	if !all.AllowsAccount("anyone") {
		t.Error("AllAccounts 应允许任意账号")
	}
}

// 空白名单（非 All）拒绝一切——与「空 = 不允许」的显式授权语义一致。
func TestEmptyScopeDeniesAll(t *testing.T) {
	k := Key{}
	if k.AllowsAccount("u1") || k.AllowsModel("cn:m") {
		t.Error("空白名单必须全部拒绝")
	}
}

func TestUpdateRegenerateDelete(t *testing.T) {
	st, _ := newTempStore(t)
	k, err := st.Create(Input{Name: "a", Accounts: []string{"u1"}, Models: []string{"cn:m"}})
	if err != nil {
		t.Fatal(err)
	}
	oldSecret := k.Key

	enabled := false
	upd, err := st.Update(k.ID, Input{
		Name: "b", Enabled: &enabled,
		AllAccounts: true, AllModels: true,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.Name != "b" || upd.Enabled || !upd.AllAccounts || !upd.AllModels {
		t.Errorf("Update = %+v", upd)
	}
	if upd.Key != oldSecret {
		t.Error("Update 不应改密钥")
	}

	re, err := st.Regenerate(k.ID)
	if err != nil {
		t.Fatalf("Regenerate: %v", err)
	}
	if re.Key == oldSecret || len(re.Key) < 16 {
		t.Errorf("Regenerate 未换密钥: %q → %q", oldSecret, re.Key)
	}
	if _, ok := st.Lookup(oldSecret); ok {
		t.Error("旧密钥应失效")
	}
	if _, ok := st.Lookup(re.Key); !ok {
		t.Error("新密钥应可查")
	}

	if err := st.Delete(k.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := st.Get(k.ID); ok {
		t.Error("删除后仍能 Get")
	}
	if err := st.Delete(k.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("重复删除 err=%v，want ErrNotFound", err)
	}
}

func TestDuplicateSecretRejected(t *testing.T) {
	st, _ := newTempStore(t)
	if _, err := st.Create(Input{Name: "a", Secret: "same-secret", AllAccounts: true, AllModels: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(Input{Name: "b", Secret: "same-secret", AllAccounts: true, AllModels: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复密钥 err=%v，want ErrConflict", err)
	}
	// 自定义密钥同样要过形态校验。
	if _, err := st.Create(Input{Name: "c", Secret: "bad secret", AllAccounts: true, AllModels: true}); err == nil {
		t.Fatal("含空格的密钥应被拒")
	}
}

// 编辑时「填了非空 key 就视为编辑」：旧 key 立即失效、新 key 生效；
// 留空/纯空白保持原 key；与其他 key 冲突回 ErrConflict。
func TestUpdateSecret(t *testing.T) {
	st, _ := newTempStore(t)
	k, err := st.Create(Input{Name: "a", Secret: "old-secret-1", AllAccounts: true, AllModels: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(Input{Name: "b", Secret: "taken-secret-2", AllAccounts: true, AllModels: true}); err != nil {
		t.Fatal(err)
	}

	// 纯空白 = 不改 key。
	upd, err := st.Update(k.ID, Input{Name: "a", Secret: "   ", AllAccounts: true, AllModels: true})
	if err != nil {
		t.Fatalf("blank secret should keep: %v", err)
	}
	if upd.Key != "old-secret-1" {
		t.Fatalf("空白密钥不应改 key，得到 %q", upd.Key)
	}

	// 非空 = 改 key；旧 key 立即失效。
	upd, err = st.Update(k.ID, Input{Name: "a", Secret: "new-secret-3", AllAccounts: true, AllModels: true})
	if err != nil {
		t.Fatalf("update secret: %v", err)
	}
	if upd.Key != "new-secret-3" {
		t.Fatalf("key=%q want new-secret-3", upd.Key)
	}
	if _, ok := st.Lookup("old-secret-1"); ok {
		t.Error("旧 key 应立即失效")
	}
	if got, ok := st.Lookup("new-secret-3"); !ok || got.ID != k.ID {
		t.Error("新 key 应可命中同一把 key")
	}

	// 与其他 key 冲突。
	if _, err := st.Update(k.ID, Input{Name: "a", Secret: "taken-secret-2", AllAccounts: true, AllModels: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("冲突密钥 err=%v want ErrConflict", err)
	}
	// 非法形态。
	if _, err := st.Update(k.ID, Input{Name: "a", Secret: "bad secret", AllAccounts: true, AllModels: true}); err == nil {
		t.Fatal("含空格的 key 应被拒")
	}
}

// 停用的 key 仍然能被 Lookup 找到，由调用方按 Enabled 拒绝（便于管理面回显）。
func TestDisabledKeyStillLookupable(t *testing.T) {
	st, _ := newTempStore(t)
	off := false
	k, err := st.Create(Input{Name: "off", Enabled: &off, AllAccounts: true, AllModels: true})
	if err != nil {
		t.Fatal(err)
	}
	got, ok := st.Lookup(k.Key)
	if !ok || got.Enabled {
		t.Errorf("Lookup = %+v, %v", got, ok)
	}
	if cur, err := st.SetEnabled(k.ID, true); err != nil || !cur.Enabled {
		t.Errorf("SetEnabled = %+v, %v", cur, err)
	}
}

// 损坏的密钥文件必须报错而不是静默当空（否则所有下游突然 401 且无线索）。
func TestLoadCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := New(path).Load(); err == nil {
		t.Fatal("损坏文件应报错")
	}
}

// 落盘格式可读、带 version；List 顺序稳定（创建时间 → ID）。
func TestFileShapeAndStableOrder(t *testing.T) {
	st, path := newTempStore(t)
	for _, name := range []string{"a", "b", "c"} {
		if _, err := st.Create(Input{Name: name, AllAccounts: true, AllModels: true}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Version int   `json:"version"`
		Keys    []Key `json:"keys"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Version != fileVersion || len(doc.Keys) != 3 {
		t.Errorf("doc=%+v", doc)
	}
	list := st.List()
	for i := 1; i < len(list); i++ {
		if list[i].CreatedAt.Before(list[i-1].CreatedAt) {
			t.Fatalf("List 未按创建时间排序: %v", list)
		}
	}
}

func TestSplitModel(t *testing.T) {
	cases := []struct {
		in, realm, bare string
	}{
		{"cn:m", "cn", "m"},
		{"global:m", "global", "m"},
		{"m", "cn", "m"},
		{"other:m", "cn", "other:m"},
		{"", "cn", ""},
	}
	for _, c := range cases {
		r, b := SplitModel(c.in)
		if r != c.realm || b != c.bare {
			t.Errorf("SplitModel(%q) = (%q,%q), want (%q,%q)", c.in, r, b, c.realm, c.bare)
		}
	}
}
