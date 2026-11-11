package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wp/internal/adminauth"
)

// fakeConfigStore 只测 HTTP 语义：校验逻辑在 cmd/server 的实现里有独立单测。
type fakeConfigStore struct {
	values   map[string]any
	errs     []ConfigFieldError
	saved    map[string]any
	password string
	enabled  bool
	snapPath string
}

func newFakeConfigStore() *fakeConfigStore {
	return &fakeConfigStore{
		values: map[string]any{
			"admin":  map[string]any{"enabled": true, "min_password_length": 8.0},
			"listen": ":7863",
		},
		snapPath: "/tmp/config.json",
	}
}

func (f *fakeConfigStore) Snapshot() (*ConfigSnapshot, error) {
	return &ConfigSnapshot{
		Values:            f.values,
		Defaults:          map[string]any{"listen": ":7863"},
		Schema:            map[string]any{"groups": []any{}, "fields": []any{}},
		Path:              f.snapPath,
		Writable:          true,
		MinPasswordLength: 8,
	}, nil
}

func (f *fakeConfigStore) Save(values map[string]any) (*ConfigSnapshot, []ConfigFieldError, error) {
	f.saved = values
	if len(f.errs) > 0 {
		return nil, f.errs, nil
	}
	snap, err := f.Snapshot()
	return snap, nil, err
}

func (f *fakeConfigStore) Reset(req ConfigResetRequest) (*ConfigSnapshot, []ConfigFieldError, error) {
	snap, err := f.Snapshot()
	return snap, nil, err
}

func (f *fakeConfigStore) SetPassword(hash string, enable bool) error {
	f.password = hash
	if enable {
		f.enabled = true
	}
	return nil
}

func newConfigTestHandler(t *testing.T, store ConfigStore) *Handler {
	t.Helper()
	store0, err := adminauth.NewFromStored("")
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(Config{
		APIKey:       "api-key",
		AdminAuth:    store0,
		AdminEnabled: false, // 首次安装：管理面还没打开
		ConfigStore:  store,
	})
}

func cfgReq(t *testing.T, h *Handler, method, path, token string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = strings.NewReader(string(raw))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// 首次初始化：status → setup → 自动开启管理面 → 登录，全程无需重启。
func TestSetupFlow(t *testing.T) {
	store := newFakeConfigStore()
	h := newConfigTestHandler(t, store)

	rec, out := cfgReq(t, h, http.MethodGet, "/admin/setup/status", "", nil)
	if rec.Code != http.StatusOK || out["initialized"] != false {
		t.Fatalf("初始化前 status = %d %v", rec.Code, out)
	}

	rec, _ = cfgReq(t, h, http.MethodPost, "/admin/setup", "", map[string]any{
		"password": "init-password-789", "confirm": "different",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("两次密码不一致应 400，实际 %d", rec.Code)
	}
	rec, _ = cfgReq(t, h, http.MethodPost, "/admin/setup", "", map[string]any{
		"password": "short", "confirm": "short",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("弱密码应 400，实际 %d", rec.Code)
	}

	rec, _ = cfgReq(t, h, http.MethodPost, "/admin/setup", "", map[string]any{
		"password": "init-password-789", "confirm": "init-password-789",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("初始化应 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if store.password == "" {
		t.Fatal("密码哈希未写入 ConfigStore")
	}
	if !store.enabled {
		t.Fatal("初始化应把 admin.enabled 置 true")
	}
	if !h.adminEnabled.Load() {
		t.Fatal("初始化后管理面应即时可用（不需要重启）")
	}

	// 已初始化后重复 setup 必须 409。
	rec, _ = cfgReq(t, h, http.MethodPost, "/admin/setup", "", map[string]any{
		"password": "another-password", "confirm": "another-password",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("重复初始化应 409，实际 %d", rec.Code)
	}

	rec, _ = cfgReq(t, h, http.MethodPost, "/admin/login", "", map[string]any{"password": "init-password-789"})
	if rec.Code != http.StatusOK {
		t.Fatalf("初始化后的密码应能登录，实际 %d", rec.Code)
	}
}

// 改密：Bearer 就是旧密码；改完旧密码立刻失效。
func TestChangePassword(t *testing.T) {
	store := newFakeConfigStore()
	h := newConfigTestHandler(t, store)
	if _, err := setupPassword(t, h, "old-password-123"); err != nil {
		t.Fatal(err)
	}

	rec, _ := cfgReq(t, h, http.MethodPost, "/admin/password", "old-password-123", map[string]any{
		"password": "new-password-456", "confirm": "new-password-456",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("改密应 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	rec, _ = cfgReq(t, h, http.MethodPost, "/admin/login", "", map[string]any{"password": "old-password-123"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("旧密码应失效，实际 %d", rec.Code)
	}
	rec, _ = cfgReq(t, h, http.MethodPost, "/admin/login", "", map[string]any{"password": "new-password-456"})
	if rec.Code != http.StatusOK {
		t.Fatalf("新密码应能登录，实际 %d", rec.Code)
	}
	if store.password == "" {
		t.Fatal("改密后哈希应写回配置")
	}
}

// setupPassword 走一次真实的 setup 接口，返回初始化用的明文密码。
func setupPassword(t *testing.T, h *Handler, pw string) (string, error) {
	rec, _ := cfgReq(t, h, http.MethodPost, "/admin/setup", "", map[string]any{
		"password": pw, "confirm": pw,
	})
	if rec.Code != http.StatusOK {
		return "", errString("setup failed: " + rec.Body.String())
	}
	return pw, nil
}

type errString string

func (e errString) Error() string { return string(e) }

// 配置接口：必须带管理密码；字段错误 400 且带 path；保存后置"待重启"。
func TestConfigAPI(t *testing.T) {
	store := newFakeConfigStore()
	h := newConfigTestHandler(t, store)
	if _, err := setupPassword(t, h, "admin-password-1"); err != nil {
		t.Fatal(err)
	}

	if rec, _ := cfgReq(t, h, http.MethodGet, "/admin/config", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据读配置应 401，实际 %d", rec.Code)
	}
	rec, out := cfgReq(t, h, http.MethodGet, "/admin/config", "admin-password-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("配置读取应 200，实际 %d", rec.Code)
	}
	meta, _ := out["meta"].(map[string]any)
	if meta == nil || meta["password_initialized"] != true {
		t.Fatalf("meta 不正确：%v", out["meta"])
	}
	if h.configPending.Load() {
		t.Fatal("刚启动不应有待重启标记")
	}

	store.errs = []ConfigFieldError{{Path: "pool.max_in_flight", Message: "应为整数"}}
	rec, out = cfgReq(t, h, http.MethodPut, "/admin/config", "admin-password-1", map[string]any{
		"values": map[string]any{"pool": map[string]any{"max_in_flight": "abc"}},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("字段错误应 400，实际 %d", rec.Code)
	}
	errObj, _ := out["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "config_invalid" {
		t.Fatalf("错误信封不正确：%v", out)
	}

	store.errs = nil
	rec, out = cfgReq(t, h, http.MethodPut, "/admin/config", "admin-password-1", map[string]any{
		"values": map[string]any{"listen": ":9000"},
	})
	if rec.Code != http.StatusOK || out["restart_pending"] != true {
		t.Fatalf("保存应成功并标记待重启：%d %v", rec.Code, out)
	}
	if !h.configPending.Load() {
		t.Fatal("保存后应有待重启标记")
	}

	rec, _ = cfgReq(t, h, http.MethodPost, "/admin/config/reset", "admin-password-1", map[string]any{
		"scope": "fields", "paths": []string{"listen"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("恢复默认应 200，实际 %d（%s）", rec.Code, rec.Body.String())
	}
}

// 管理面关闭时：初始化/登录仍可用，其余 /admin/* 与"未注册路由"同形（纯文本 404）。
func TestAdminGate(t *testing.T) {
	h := newConfigTestHandler(t, newFakeConfigStore())

	rec, _ := cfgReq(t, h, http.MethodGet, "/admin/setup/status", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("关闭态 setup/status 应可用，实际 %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/accounts/u1/disable", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "404 page not found") {
		t.Fatalf("关闭态应回纯文本 404，实际 %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/accounts", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET 探测也应是 404（不能出现 405），实际 %d", rec.Code)
	}
}
