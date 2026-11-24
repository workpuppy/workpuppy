package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wp/internal/accesskey"
	"wp/internal/adminauth"
	"wp/internal/auth"
	"wp/internal/session"
	"wp/internal/upstream"
)

// newAccessKeyStore 用显式密钥创建 key（测试需要同时知道明文）。
func newAccessKeyStore(t *testing.T, inputs ...accesskey.Input) *accesskey.Store {
	t.Helper()
	st := accesskey.New("")
	for _, in := range inputs {
		if _, err := st.Create(in); err != nil {
			t.Fatalf("Create %q: %v", in.Name, err)
		}
	}
	return st
}

func chatReq(h *Handler, secret, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// 未配置任何 key（空存储）时数据面一律 401：不再有「api_key 为空 = 不鉴权」。
func TestAccessKeyEmptyStoreRejects(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), AccessKeys: accesskey.New("")})

	for _, secret := range []string{"", "whatever"} {
		req := httptest.NewRequest(http.MethodGet, "/status", nil)
		if secret != "" {
			req.Header.Set("Authorization", "Bearer "+secret)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("secret=%q code=%d want 401", secret, rec.Code)
		}
	}
}

// 停用的 key 立即失效（与删除的区别只是保留配置便于再启用）。
func TestAccessKeyDisabledRejected(t *testing.T) {
	off := false
	secret := testSecret("disabled")
	st := newAccessKeyStore(t, accesskey.Input{
		Name: "off", Secret: secret, Enabled: &off, AllAccounts: true, AllModels: true,
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), AccessKeys: st})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled key code=%d want 401", rec.Code)
	}
}

// 模型越权在进入上游之前回 404 model_not_found；裸名与 cn: 等价。
func TestAccessKeyModelNotAllowed(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	secret := testSecret("model")
	st := newAccessKeyStore(t, accesskey.Input{
		Name: "a", Secret: secret,
		Accounts: []string{"u1"}, Models: []string{"cn:glm-5.2"},
	})
	h := NewHandler(Config{Pool: p, Upstream: up, AccessKeys: st})

	rec := chatReq(h, secret, `{"model":"cn:other","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("越权模型 code=%d want 404（body=%s）", rec.Code, rec.Body)
	}
	if !assertJSONErrorCode(t, rec.Body.String(), "model_not_found") {
		return
	}
	if calls != 0 {
		t.Errorf("越权模型不应打到上游，calls=%d", calls)
	}

	// 裸名等价 cn: —— 老客户端不带前缀不能被拒。
	rec = chatReq(h, secret, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("裸名（cn 等价）code=%d want 200（body=%s）", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("calls=%d want 1", calls)
	}
}

// 选号只能落在 key 的账号白名单内：即使池里另有更高权重账号也绝不越界。
func TestAccessKeyAccountScopeRouting(t *testing.T) {
	var auths []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		auths = append(auths, authz)
		return 200, sseOK, true
	})
	// testPoolWith 的确定性随机源恒选候选集第一个；u1 权重更高，但不在范围内。
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	secret := testSecret("scope")
	st := newAccessKeyStore(t, accesskey.Input{
		Name: "b", Secret: secret,
		Accounts: []string{"u2"}, AllModels: true,
	})
	h := NewHandler(Config{Pool: p, Upstream: up, AccessKeys: st})

	for i := 0; i < 5; i++ {
		rec := chatReq(h, secret, `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"stream":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	for _, a := range auths {
		if a != "Bearer at2" {
			t.Fatalf("越界调用：%v（只允许 u2/at2）", auths)
		}
	}
}

// /status 按 key 收缩账号列表与汇总数（同一个 scope），管理面 /admin/status 仍看全池。
func TestAccessKeyStatusScoped(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	secret := testSecret("status")
	st := newAccessKeyStore(t, accesskey.Input{
		Name: "a", Secret: secret,
		Accounts: []string{"u1"}, AllModels: true,
	})
	h := NewHandler(Config{
		Pool: p, Upstream: upstream.New(), AccessKeys: st,
		AdminAuth: adminauth.MustNew(testAdminPW), AdminEnabled: true,
	})

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var data struct {
		Accounts []struct {
			UID string `json:"uid"`
		} `json:"accounts"`
		Total   int `json:"total"`
		Healthy int `json:"healthy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data.Total != 1 || data.Healthy != 1 || len(data.Accounts) != 1 || data.Accounts[0].UID != "u1" {
		t.Errorf("scoped /status = %+v，want 只有 u1（total=1 healthy=1）", data)
	}

	// 管理密码看全池。
	req = httptest.NewRequest(http.MethodGet, "/admin/status", nil)
	req.Header.Set("Authorization", "Bearer "+testAdminPW)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin status code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"total":2`) {
		t.Errorf("/admin/status 应看全池：%s", rec.Body.String())
	}
}

// /v1/models 按 key 的模型白名单过滤；管理面 /admin/models 保持全量。
func TestAccessKeyModelsListingFiltered(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "alpha"}, {ID: "beta"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	secret := testSecret("models")
	st := newAccessKeyStore(t, accesskey.Input{
		Name: "a", Secret: secret,
		Accounts: []string{"u1"}, Models: []string{"cn:alpha"},
	})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), AccessKeys: st, GlobalEnabled: false})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+secret)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data) != 1 || resp.Data[0].ID != "cn:alpha" {
		t.Errorf("/v1/models = %+v，want 仅 cn:alpha", resp.Data)
	}
}

// 会话粘性按 key 命名空间隔离：同一段对话在两把 key 下各绑各的账号，互不串号。
func TestAccessKeyStickyIsolation(t *testing.T) {
	var auths []string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		auths = append(auths, authz)
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	secretA := testSecret("sticky-a")
	secretB := testSecret("sticky-b")
	st := newAccessKeyStore(t,
		accesskey.Input{Name: "a", Secret: secretA, Accounts: []string{"u1"}, AllModels: true},
		accesskey.Input{Name: "b", Secret: secretB, Accounts: []string{"u2"}, AllModels: true},
	)
	binds := newBindStore()
	sess := session.New(session.Config{
		TTL:   time.Minute,
		Store: binds,
		AvailableForModel: func(string) []string {
			return []string{"u1", "u2"} // 全池可用；scope 在 Router 内再过滤
		},
	})
	h := NewHandler(Config{Pool: p, Upstream: up, AccessKeys: st, Session: sess})

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"同一个会话"}],"metadata":{"conversation_id":"c1"},"stream":true}`
	for _, pair := range [][2]string{{secretA, "Bearer at1"}, {secretB, "Bearer at2"}} {
		for i := 0; i < 2; i++ {
			rec := chatReq(h, pair[0], body)
			if rec.Code != http.StatusOK {
				t.Fatalf("key %s req %d code=%d body=%s", pair[0], i, rec.Code, rec.Body)
			}
			if last := auths[len(auths)-1]; last != pair[1] {
				t.Fatalf("key %s 走了 %s，want %s（粘性跨 key 泄漏）", pair[0], last, pair[1])
			}
		}
	}
	// 两把 key 各自的绑定都在（键带 ak:<id>: 前缀）。
	keys := binds.LoadBinds()
	if len(keys) != 2 {
		t.Fatalf("bindings=%v，want 两把 key 各自一条", keys)
	}
	for k := range keys {
		if !strings.HasPrefix(k, "ak:") || !strings.HasSuffix(k, ":c1") {
			t.Errorf("粘性键未按 key 命名空间隔离: %q", k)
		}
	}
}

// 管理面接入 key 的完整 CRUD + 校验错误映射。
func TestAdminAccessKeysCRUD(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	st := accesskey.New("")
	h := NewHandler(Config{
		Pool: p, Upstream: upstream.New(), AccessKeys: st,
		AdminAuth: adminauth.MustNew(testAdminPW), AdminEnabled: true,
	})
	call := func(method, path, body string) *httptest.ResponseRecorder {
		var rdr *strings.Reader
		if body == "" {
			rdr = strings.NewReader("")
		} else {
			rdr = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rdr)
		req.Header.Set("Authorization", "Bearer "+testAdminPW)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	var created struct {
		Key accesskey.Key `json:"key"`
	}

	// 校验：空白名单必须 400（不能静默放全池）。
	if rec := call(http.MethodPost, "/admin/access-keys", `{"name":"bad"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty scopes code=%d want 400（body=%s）", rec.Code, rec.Body)
	}

	rec := call(http.MethodPost, "/admin/access-keys",
		`{"name":"team-a","accounts":["u1"],"models":["cn:glm-5.2"]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create code=%d body=%s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Key.ID == "" || created.Key.Key == "" || !created.Key.Enabled {
		t.Fatalf("created=%+v", created.Key)
	}

	// 列表
	rec = call(http.MethodGet, "/admin/access-keys", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), created.Key.ID) {
		t.Fatalf("list code=%d body=%s", rec.Code, rec.Body)
	}

	// 停用 → 数据面立即 401
	if rec := call(http.MethodPost, "/admin/access-keys/"+created.Key.ID+"/disable", ""); rec.Code != http.StatusOK {
		t.Fatalf("disable code=%d body=%s", rec.Code, rec.Body)
	}
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer "+created.Key.Key)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled key /status code=%d want 401", rec.Code)
	}

	// 更新为全部账号/模型、重新启用，并直接编辑 key（非空 = 改密钥）
	editedSecret := testSecret("edited")
	rec = call(http.MethodPut, "/admin/access-keys/"+created.Key.ID,
		`{"name":"team-a2","enabled":true,"all_accounts":true,"all_models":true,"secret":"`+editedSecret+`"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "team-a2") {
		t.Fatalf("update code=%d body=%s", rec.Code, rec.Body)
	}
	var edited struct {
		Key accesskey.Key `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &edited); err != nil {
		t.Fatal(err)
	}
	if edited.Key.Key != editedSecret {
		t.Fatalf("编辑后的 key=%q want %q", edited.Key.Key, editedSecret)
	}
	// 旧 key 立即失效，新 key 可访问数据面。
	req = httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer "+created.Key.Key)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("旧密钥 code=%d want 401", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer "+editedSecret)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("新密钥 code=%d want 200", rec.Code)
	}
	created.Key.Key = edited.Key.Key // 后续 regenerate 断言用新 key 作基线

	// 换密钥：旧 key 失效、新 key 可查
	var re struct {
		Key accesskey.Key `json:"key"`
	}
	rec = call(http.MethodPost, "/admin/access-keys/"+created.Key.ID+"/regenerate", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("regenerate code=%d body=%s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &re); err != nil {
		t.Fatal(err)
	}
	if re.Key.Key == created.Key.Key {
		t.Error("regenerate 没有换密钥")
	}
	req = httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer "+created.Key.Key)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("旧密钥 code=%d want 401", rec.Code)
	}

	// 删除
	if rec := call(http.MethodDelete, "/admin/access-keys/"+created.Key.ID, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete code=%d body=%s", rec.Code, rec.Body)
	}
	if st.Count() != 0 {
		t.Errorf("删除后 count=%d", st.Count())
	}
	if rec := call(http.MethodDelete, "/admin/access-keys/"+created.Key.ID, ""); rec.Code != http.StatusNotFound {
		t.Errorf("重复删除 code=%d want 404", rec.Code)
	}
}

// scopeOfKey：nil/AllAccounts = 不限；空白名单 = 非 nil 的空集合（全部拒绝）。
func TestScopeOfKey(t *testing.T) {
	if scopeOfKey(nil) != nil {
		t.Error("nil key 应表示不限")
	}
	if scopeOfKey(&accesskey.Key{AllAccounts: true}) != nil {
		t.Error("AllAccounts 应表示不限")
	}
	scope := scopeOfKey(&accesskey.Key{})
	if scope == nil {
		t.Fatal("空白名单必须是「全部拒绝」而不是 nil")
	}
	if scope.Allows("u1") {
		t.Error("空白名单不应允许任何账号")
	}
}
