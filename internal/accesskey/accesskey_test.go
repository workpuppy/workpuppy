package accesskey

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

// testSecret 派生一个符合生产规则（sk- + 32 位字母/数字）的确定性测试密钥。
func testSecret(label string) string {
	sum := sha256.Sum256([]byte(label))
	return "sk-" + hex.EncodeToString(sum[:16])
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
	if k.Realm != "cn" {
		t.Errorf("默认版本应为 cn，实际 %q", k.Realm)
	}
	if len(k.Key) != len(secretPrefix)+secretBodyLen || !secretPattern.MatchString(k.Key) {
		t.Errorf("生成的 key 形态异常（want sk- + 32 位字母数字）: %q", k.Key)
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
	same := testSecret("same")
	if _, err := st.Create(Input{Name: "a", Secret: same, AllAccounts: true, AllModels: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(Input{Name: "b", Secret: same, AllAccounts: true, AllModels: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("重复密钥 err=%v，want ErrConflict", err)
	}
	// 自定义密钥同样要过格式校验。
	if _, err := st.Create(Input{Name: "c", Secret: "bad secret", AllAccounts: true, AllModels: true}); err == nil {
		t.Fatal("含空格的密钥应被拒")
	}
	if _, err := st.Create(Input{Name: "d", Secret: "sk-tooShort", AllAccounts: true, AllModels: true}); err == nil {
		t.Fatal("sk- 后不足 32 位的密钥应被拒")
	}
}

// 编辑时「填了非空 key 就视为编辑」：旧 key 立即失效、新 key 生效；
// 留空/纯空白保持原 key；与其他 key 冲突回 ErrConflict。
func TestUpdateSecret(t *testing.T) {
	st, _ := newTempStore(t)
	oldSecret := testSecret("old")
	takenSecret := testSecret("taken")
	newSecret := testSecret("new")
	k, err := st.Create(Input{Name: "a", Secret: oldSecret, AllAccounts: true, AllModels: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(Input{Name: "b", Secret: takenSecret, AllAccounts: true, AllModels: true}); err != nil {
		t.Fatal(err)
	}

	// 纯空白 = 不改 key。
	upd, err := st.Update(k.ID, Input{Name: "a", Secret: "   ", AllAccounts: true, AllModels: true})
	if err != nil {
		t.Fatalf("blank secret should keep: %v", err)
	}
	if upd.Key != oldSecret {
		t.Fatalf("空白密钥不应改 key，得到 %q", upd.Key)
	}

	// 非空 = 改 key；旧 key 立即失效。
	upd, err = st.Update(k.ID, Input{Name: "a", Secret: newSecret, AllAccounts: true, AllModels: true})
	if err != nil {
		t.Fatalf("update secret: %v", err)
	}
	if upd.Key != newSecret {
		t.Fatalf("key=%q want %q", upd.Key, newSecret)
	}
	if _, ok := st.Lookup(oldSecret); ok {
		t.Error("旧 key 应立即失效")
	}
	if got, ok := st.Lookup(newSecret); !ok || got.ID != k.ID {
		t.Error("新 key 应可命中同一把 key")
	}

	// 与其他 key 冲突。
	if _, err := st.Update(k.ID, Input{Name: "a", Secret: takenSecret, AllAccounts: true, AllModels: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("冲突密钥 err=%v want ErrConflict", err)
	}
	// 非法形态、超长、非字母数字。
	if _, err := st.Update(k.ID, Input{Name: "a", Secret: "bad secret", AllAccounts: true, AllModels: true}); err == nil {
		t.Fatal("含空格的 key 应被拒")
	}
	tooLong := "sk-" + strings.Repeat("a", 62) // 65 > 64
	if _, err := st.Update(k.ID, Input{Name: "a", Secret: tooLong, AllAccounts: true, AllModels: true}); err == nil {
		t.Fatal("超过 64 字符的 key 应被拒")
	}
	if _, err := st.Update(k.ID, Input{Name: "a", Secret: "sk-" + strings.Repeat("a", 32) + "-", AllAccounts: true, AllModels: true}); err == nil {
		t.Fatal("含连字符的 key 应被拒")
	}
	// 边界：64 字符恰好允许。
	maxSecret := "sk-" + strings.Repeat("a", 61)
	if _, err := st.Update(k.ID, Input{Name: "a", Secret: maxSecret, AllAccounts: true, AllModels: true}); err != nil {
		t.Fatalf("64 字符 key 应允许: %v", err)
	}
	// 后端必须 trim：前后空格不应进入落盘值。
	trimmed := testSecret("trim")
	upd, err = st.Update(k.ID, Input{Name: "a", Secret: "  " + trimmed + "  ", AllAccounts: true, AllModels: true})
	if err != nil {
		t.Fatalf("trim update: %v", err)
	}
	if upd.Key != trimmed {
		t.Fatalf("后端未 trim：key=%q want %q", upd.Key, trimmed)
	}
}

// 旧版本生成的 wp-xxx key 必须仍可编辑：只改名称/授权范围时不校验旧格式，
// 只有真正改成新值时才按 sk- 规则校验。避免升级后所有存量 key 都改不了。
func TestUpdateLegacyKeyWithoutChangingSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	raw := `{"version":1,"keys":[{"id":"ak_legacy","name":"legacy","key":"wp-legacy-key-0123456789","enabled":true,"all_accounts":true,"all_models":true,"accounts":[],"models":[]}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	st := New(path)
	if err := st.Load(); err != nil {
		t.Fatal(err)
	}
	if got, ok := st.Lookup("wp-legacy-key-0123456789"); !ok || got.ID != "ak_legacy" {
		t.Fatalf("legacy key 未加载: %+v %v", got, ok)
	}
	// 不传 secret（前端未改动时就是这条路径）→ 保留 wp- 旧值。
	upd, err := st.Update("ak_legacy", Input{Name: "renamed", AllAccounts: true, AllModels: true})
	if err != nil {
		t.Fatalf("给旧 key 改名不应被新规则拦住: %v", err)
	}
	if upd.Key != "wp-legacy-key-0123456789" {
		t.Fatalf("旧 key 被意外改写: %q", upd.Key)
	}
	if upd.Realm != "cn" {
		t.Fatalf("旧 key 缺 realm 应归一为 cn，实际 %q", upd.Realm)
	}
	// 原样回传旧 secret（某些客户端会全量提交）也视为未改动。
	if _, err := st.Update("ak_legacy", Input{Name: "renamed", Secret: "wp-legacy-key-0123456789", AllAccounts: true, AllModels: true}); err != nil {
		t.Fatalf("原样回传旧 secret 应视为未改: %v", err)
	}
	// 真要换新值时才严格校验。
	if _, err := st.Update("ak_legacy", Input{Name: "renamed", Secret: "sk-short", AllAccounts: true, AllModels: true}); err == nil {
		t.Fatal("旧 key 改成非法新值仍应被拒")
	}
}

// 版本约束：key 只能调同区域模型；AllModels 也不能跨区；模型白名单必须同区。
func TestRealmValidationAndModelFilter(t *testing.T) {
	st, _ := newTempStore(t)
	if _, err := st.Create(Input{Name: "bad-realm", Realm: "mars", AllAccounts: true, AllModels: true}); err == nil {
		t.Fatal("非法 realm 应被拒")
	}
	if _, err := st.Create(Input{Name: "cross", Realm: "cn", AllAccounts: true, Models: []string{"global:gpt-5.4"}}); err == nil {
		t.Fatal("cn key 不应接受 global: 模型白名单")
	}

	globalKey, err := st.Create(Input{
		Name: "g", Realm: "global", Accounts: []string{"g1"}, Models: []string{"global:gpt-5.4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if globalKey.Realm != "global" {
		t.Fatalf("realm=%q", globalKey.Realm)
	}
	if !globalKey.AllowsModel("global:gpt-5.4") {
		t.Error("global key 应允许 global 模型")
	}
	if globalKey.AllowsModel("cn:glm-5.2") || globalKey.AllowsModel("glm-5.2") {
		t.Error("global key 不应允许 cn 模型（裸名按 cn 解释）")
	}

	allGlobal := Key{Realm: "global", AllModels: true}
	if allGlobal.AllowsModel("cn:glm-5.2") {
		t.Error("AllModels 仍不能跨区域放行")
	}
	if !allGlobal.AllowsModel("global:any") {
		t.Error("global AllModels 应允许 global 模型")
	}

	// 旧文件/零值 realm 的模型匹配保持宽松（真实加载会归一成 cn）。
	legacy := Key{Models: []string{"cn:m", "global:g"}}
	if !legacy.AllowsModel("global:g") {
		t.Error("零值 realm 不应额外约束（测试/旧结构兼容）")
	}
}

// 外部改文件 → Reload 覆盖内存；接口写入后 Reload 不会回退。
func TestReloadFromFile(t *testing.T) {
	st, path := newTempStore(t)
	a, err := st.Create(Input{Name: "a", Secret: testSecret("reload-a"), AllAccounts: true, AllModels: true})
	if err != nil {
		t.Fatal(err)
	}
	// 外部编辑：整份替换为另一把 key。
	b := a
	b.ID = "ak_external"
	b.Name = "external"
	b.Key = testSecret("reload-b")
	writeKeyFile(t, path, b)
	if err := st.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if _, ok := st.Lookup(a.Key); ok {
		t.Error("旧 key 应被文件内容覆盖")
	}
	if got, ok := st.Lookup(b.Key); !ok || got.Name != "external" {
		t.Fatalf("文件里的 key 未生效: %+v %v", got, ok)
	}

	// 接口写入（改名）后立刻 Reload：不能回退成文件旧内容。
	if _, err := st.Update("ak_external", Input{Name: "renamed", Secret: b.Key, AllAccounts: true, AllModels: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.Reload(); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Get("ak_external"); got.Name != "renamed" {
		t.Fatalf("Reload 把接口修改回退了: %q", got.Name)
	}
}

// 坏文件保留内存配置（不因一次半截写入让全站 401）；文件修好后下次能恢复。
func TestReloadInvalidKeepsCache(t *testing.T) {
	st, path := newTempStore(t)
	a, err := st.Create(Input{Name: "a", Secret: testSecret("keep"), AllAccounts: true, AllModels: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := st.Reload(); err == nil {
		t.Fatal("坏文件应返回错误")
	}
	if got, ok := st.Lookup(a.Key); !ok || got.Name != "a" {
		t.Fatalf("坏文件不应清空内存配置: %+v %v", got, ok)
	}

	// 同一份坏文件不重复解析；改回合法内容后能恢复。
	b := a
	b.Name = "fixed"
	writeKeyFile(t, path, b)
	if err := st.Reload(); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Get(a.ID); got.Name != "fixed" {
		t.Fatalf("修复后未重新加载: %+v", got)
	}
}

// 文件被删除 = 配置真相为空：内存清空（下游 401），而不是继续用缓存。
func TestReloadMissingClears(t *testing.T) {
	st, path := newTempStore(t)
	if _, err := st.Create(Input{Name: "a", Secret: testSecret("gone"), AllAccounts: true, AllModels: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := st.Reload(); err != nil {
		t.Fatal(err)
	}
	if st.Count() != 0 {
		t.Fatalf("文件删除后 count=%d want 0", st.Count())
	}
}

func TestNextAligned(t *testing.T) {
	base := time.Date(2026, 11, 25, 10, 0, 7, 0, time.UTC)
	cases := []struct {
		now  time.Time
		want time.Time
	}{
		{base, time.Date(2026, 11, 25, 10, 0, 30, 0, time.UTC)},
		{time.Date(2026, 11, 25, 10, 0, 30, 0, time.UTC), time.Date(2026, 11, 25, 10, 1, 0, 0, time.UTC)},
		{time.Date(2026, 11, 25, 10, 0, 29, 999_000_000, time.UTC), time.Date(2026, 11, 25, 10, 0, 30, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := nextAligned(c.now, DefaultReloadInterval); !got.Equal(c.want) {
			t.Errorf("nextAligned(%s)=%s want %s", c.now, got, c.want)
		}
	}
}

func writeKeyFile(t *testing.T, path string, keys ...Key) {
	t.Helper()
	raw, err := json.Marshal(fileDoc{Version: fileVersion, Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
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
