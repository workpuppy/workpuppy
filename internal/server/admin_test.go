// admin_test.go 运维管理端点的契约测试（issue #138/#118）。
//
// 覆盖：开关默认关闭（404 且不暴露管理面）、鉴权与 /status 同源、禁用/启用的状态迁移、
// 幂等、未知 uid，以及「单一 disabled 位 + disabled_source 来源」的语义。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wp/internal/adminauth"
	"wp/internal/auth"
)

// adminResp 与 adminState 同构（测试侧独立声明，避免测试跟着实现改字段名）。
type adminResp struct {
	UID            string `json:"uid"`
	Disabled       bool   `json:"disabled"`
	DisabledSource string `json:"disabled_source"`
	DisabledReason string `json:"disabled_reason"`
	Changed        bool   `json:"changed"`
}

func doAdmin(t *testing.T, h *Handler, method, path string, body string) (*httptest.ResponseRecorder, adminResp) {
	t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, rdr)
	// 管理面现在只认管理密码（与 /v1 的 api_key 分离），helper 统一带上。
	req.Header.Set("Authorization", "Bearer "+testAdminPW)
	h.ServeHTTP(rec, req)
	var out adminResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode admin response: %v body=%s", err, rec.Body)
		}
	}
	return rec, out
}

// TestAdminDisabledByDefault 开关默认关闭：路由不注册，路径一律 404 且为
// mux 默认纯文本形态（"404 page not found"）——与真 404 不可区分，不向外
// 暴露"这里存在管理面"（设计 supplement §2.3：路由条件注册，而非 handler 内 404）。
func TestAdminDisabledByDefault(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, APIKey: "k"}) // AdminEnabled 零值 false

	for _, path := range []string{
		"/admin/accounts/u1/disable",
		"/admin/accounts/u1/enable",
		"/admin/accounts/u1/rest",
	} {
		// 即使带 key 也得 404：路由未注册，withAuth 根本不进场
		// （若路由注册了，正确 key 会命中 handler——这条同时锁住"关闭态不注册"本身）。
		rec, _ := doAdmin(t, h, "POST", path, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: code=%d want 404 when admin disabled", path, rec.Code)
		}
		// mux 默认 404 是纯文本，非 writeOpenAIError 的 JSON 信封——两者可区分
		// 就是存在性泄露面。
		if !strings.Contains(rec.Body.String(), "404 page not found") {
			t.Fatalf("%s: 关闭态 404 应为 mux 默认纯文本形态, body=%q", path, rec.Body.String())
		}

		// GET 探测 POST 路径：未注册的路由没有 405（已注册才回 405 + Allow 头）。
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s: code=%d want 404（未注册，不得出现 405）", path, rec.Code)
		}
	}
	// 状态位未被触碰
	if st, _ := p.Status("u1"); st.Disabled {
		t.Fatal("关闭状态下不应有任何状态变更")
	}
}

// TestAdminRequiresAdminPassword 管理端点只认管理密码（与 /v1 的 api_key 分离）。
func TestAdminRequiresAdminPassword(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{
		Pool:         p,
		APIKey:       "secret",
		AdminAuth:    adminauth.MustNew("admin-secret"),
		AdminEnabled: true,
	})

	// 不带任何凭据 → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/accounts/u1/disable", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401 without credential", rec.Code)
	}
	// 用 /v1 的 api_key → 401（这正是本次改动的目的：下游 key 不再能管后台）
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/accounts/u1/disable", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401 with api_key（管理面不应接受 /v1 的 key）", rec.Code)
	}
	// 错的管理密码 → 401
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/admin/accounts/u1/disable", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d want 401 with wrong admin password", rec.Code)
	}
	// 正确的管理密码 → 200
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/admin/accounts/u1/disable", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 with correct admin password, body=%s", rec.Code, rec.Body)
	}
}

// TestAdminDisableEnableRoundTrip 禁用 → 状态可读且不参与选号 → 启用 → 可选。
func TestAdminDisableEnableRoundTrip(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, AdminAuth: adminauth.MustNew(testAdminPW), AdminEnabled: true})

	rec, out := doAdmin(t, h, "POST", "/admin/accounts/u1/disable", `{"reason":"观察几天"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable code=%d body=%s", rec.Code, rec.Body)
	}
	if !out.Disabled || !out.Changed {
		t.Fatalf("disable 响应 = %+v", out)
	}
	if out.DisabledSource != "manual" {
		t.Errorf("disabled_source=%q want manual", out.DisabledSource)
	}
	if out.DisabledReason != "观察几天" {
		t.Errorf("disabled_reason=%q", out.DisabledReason)
	}

	// 生效：不再被选中
	if got := p.Pick(""); got != nil {
		t.Fatalf("禁用后仍被选中: %+v", got)
	}
	// 状态位可读（仍在池里）
	st, ok := p.Status("u1")
	if !ok || !st.Disabled {
		t.Fatalf("状态应可读且 disabled=true: %+v ok=%v", st, ok)
	}

	// 启用
	_, out = doAdmin(t, h, "POST", "/admin/accounts/u1/enable", "")
	if out.Disabled || !out.Changed {
		t.Fatalf("enable 响应 = %+v", out)
	}
	if got := p.Pick(""); got == nil || got.UID != "u1" {
		t.Fatalf("启用后应可选, got %+v", got)
	}
}

// TestAdminDisableWithoutBody 无请求体是最常见的调用形态（curl / CLI），必须可用。
func TestAdminDisableWithoutBody(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, AdminAuth: adminauth.MustNew(testAdminPW), AdminEnabled: true})

	rec, out := doAdmin(t, h, "POST", "/admin/accounts/u1/disable", "")
	if rec.Code != http.StatusOK || !out.Disabled {
		t.Fatalf("code=%d out=%+v", rec.Code, out)
	}
	// 默认原因文案
	if out.DisabledReason != "manual" {
		t.Errorf("无 reason 时 default=%q, want manual", out.DisabledReason)
	}
}

// TestAdminIdempotent 面板重试场景：重复调用不报错，第二次 changed=false。
func TestAdminIdempotent(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, AdminAuth: adminauth.MustNew(testAdminPW), AdminEnabled: true})

	_, first := doAdmin(t, h, "POST", "/admin/accounts/u1/disable", `{"reason":"r"}`)
	if !first.Changed {
		t.Fatal("首次应 changed=true")
	}
	rec, second := doAdmin(t, h, "POST", "/admin/accounts/u1/disable", `{"reason":"r"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("重复调用应 200（幂等）, got %d", rec.Code)
	}
	if second.Changed || !second.Disabled {
		t.Fatalf("重复禁用 = %+v, want changed=false disabled=true", second)
	}
}

// TestAdminEnableClearsBothSources 单一 disabled 位：无论手动还是系统置位，
// 一次「启用」全部清掉。
func TestAdminEnableClearsBothSources(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, AdminAuth: adminauth.MustNew(testAdminPW), AdminEnabled: true})

	// 制造系统自动禁用
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("precondition: 应被自动禁用")
	}

	// 「启用」= 让这个号重新可用
	_, out := doAdmin(t, h, "POST", "/admin/accounts/u1/enable", "")
	if out.Disabled {
		t.Error("enable 后不应再是 disabled")
	}
	if !out.Changed {
		t.Error("enable 应 changed=true")
	}
	if got := p.Pick(""); got == nil {
		t.Fatal("启用后应恢复可选")
	}

	// 手动禁用后同样一次 enable 清掉
	p.SetDisabled("u1", true, "运维禁用")
	_, out = doAdmin(t, h, "POST", "/admin/accounts/u1/enable", "")
	if out.Disabled || out.DisabledSource != "" {
		t.Fatalf("enable 应清禁用位与来源：%+v", out)
	}
	if got := p.Pick(""); got == nil {
		t.Fatal("启用后应可选")
	}
}

// TestAdminUnknownUID uid 不存在 → 404（三个端点一致）。
func TestAdminUnknownUID(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, AdminAuth: adminauth.MustNew(testAdminPW), AdminEnabled: true})

	for _, op := range []string{"disable", "enable", "rest"} {
		rec, _ := doAdmin(t, h, "POST", "/admin/accounts/nope/"+op, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s unknown uid: code=%d want 404", op, rec.Code)
		}
		assertJSONErrorCode(t, rec.Body.String(), "not_found")
	}
}

// TestAdminStatusExposesDisabledSource /status 透出 disabled + disabled_source。
func TestAdminStatusExposesDisabledSource(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, AdminAuth: adminauth.MustNew(testAdminPW), AdminEnabled: true})
	p.SetDisabled("u1", true, "面板禁用")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status code=%d", rec.Code)
	}
	var body struct {
		Accounts []adminResp `json:"accounts"`
		Disabled int         `json:"disabled"`
		Healthy  int         `json:"healthy"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts=%d", len(body.Accounts))
	}
	a := body.Accounts[0]
	if !a.Disabled {
		t.Error("status 应透出 disabled=true")
	}
	if a.DisabledSource != "manual" {
		t.Errorf("disabled_source=%q want manual", a.DisabledSource)
	}
	if a.DisabledReason != "面板禁用" {
		t.Errorf("disabled_reason=%q", a.DisabledReason)
	}
	// 计数口径：禁用归入 disabled
	if body.Disabled != 1 || body.Healthy != 0 {
		t.Errorf("counts disabled=%d healthy=%d, want 1/0", body.Disabled, body.Healthy)
	}
}
