package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/loginsvc"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
)

// newAdminHandler 构造一个开启管理面的 handler（无状态文件、纯内存池）。
func newAdminHandler(t *testing.T, poolAccs []*auth.Auth, authDir string) *Handler {
	t.Helper()
	p := pool.New("")
	for _, a := range poolAccs {
		p.Add(a)
	}
	t.Cleanup(p.Close)
	return NewHandler(Config{
		Pool:         p,
		Upstream:     upstream.New(),
		APIKey:       "test-key",
		AdminEnabled: true,
		AuthDir:      authDir,
	})
}

// adminReq 发一次带鉴权的管理面请求。
func adminReq(h *Handler, method, target string, body io.Reader) *httptest.ResponseRecorder {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, body)
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	r.Header.Set("Authorization", "Bearer test-key")
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// writeAuthFile 在 dir 下写一个账号文件（走生产同一条 SaveAtomic 路径）。
func writeAuthFile(t *testing.T, dir, uid, nickname, realm string) string {
	t.Helper()
	a := auth.NewFromLogin(uid, "", nickname, "access-"+uid, "refresh-"+uid, "copilot.tencent.com", realm, 4102444800)
	a.FilePath = filepath.Join(dir, "workbuddy-"+uid+".json")
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	return a.FilePath
}

// 管理面整体受 admin.enabled 闸门：未开启时这些路径必须**根本不存在**（404），
// 而不是 401/403 —— 后者等于告诉扫描者「这里有管理面，只是没给钥匙」。
func TestAdminRoutesAbsentWhenDisabled(t *testing.T) {
	p := pool.New("")
	defer p.Close()
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "test-key"})

	for _, target := range []string{"/admin/accounts", "/admin/models"} {
		w := adminReq(h, http.MethodGet, target, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s 未开启管理面时应 404，实际 %d（body=%s）", target, w.Code, w.Body.String())
		}
	}
}

// 管理面与 /status 同源鉴权：没有 api_key 一律 401。
func TestAdminRequiresAPIKey(t *testing.T) {
	h := newAdminHandler(t, nil, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/admin/accounts", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("缺 key 应 401，实际 %d", w.Code)
	}
}

// 账号清单要同时覆盖「池里的号」与「只在文件里、没进池的号」：缺后者的表现是
// 「我加了这个号但列表里没有」，属于最难查的一类。
func TestAdminAccountsMergesPoolAndFiles(t *testing.T) {
	dir := t.TempDir()

	inPool := auth.NewFromLogin("111", "", "池里的号", "a1", "r1", "copilot.tencent.com", "cn", 4102444800)
	fileOnly := writeAuthFile(t, dir, "222", "只有文件的号", "global")

	h := newAdminHandler(t, []*auth.Auth{inPool}, dir)

	w := adminReq(h, http.MethodGet, "/admin/accounts", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Accounts []map[string]any `json:"accounts"`
		Total    int              `json:"total"`
		AuthDir  string           `json:"auth_dir"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if resp.AuthDir != dir {
		t.Fatalf("auth_dir 应回显 %q，实际 %q", dir, resp.AuthDir)
	}
	byUID := map[string]map[string]any{}
	for _, a := range resp.Accounts {
		uid, _ := a["uid"].(string)
		byUID[uid] = a
	}
	if _, ok := byUID["111"]; !ok {
		t.Fatalf("池里的账号 111 应出现在清单里：%v", byUID)
	}
	row, ok := byUID["222"]
	if !ok {
		t.Fatalf("只在文件里的账号 222 也必须列出（否则加号后看不见）：%v", byUID)
	}
	if inPool, _ := row["in_pool"].(bool); inPool {
		t.Fatalf("222 不在池里，in_pool 应为 false：%v", row)
	}
	if row["realm"] != "global" {
		t.Fatalf("222 的 realm 应来自文件（global），实际 %v", row["realm"])
	}
	if fileOnly == "" {
		t.Fatal("测试自身写文件失败")
	}
}

// 单账号详情：不存在 → 404；存在 → 带上凭证侧元信息。
func TestAdminAccountDetail(t *testing.T) {
	acc := auth.NewFromLogin("333", "ent-1", "明细号", "a3", "r3", "copilot.tencent.com", "cn", 4102444800)
	h := newAdminHandler(t, []*auth.Auth{acc}, "")

	w := adminReq(h, http.MethodGet, "/admin/accounts/333", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["realm"] != "cn" || resp["can_refresh"] != true {
		t.Fatalf("详情字段不符：%v", resp)
	}

	missing := adminReq(h, http.MethodGet, "/admin/accounts/nope", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("不存在的账号应 404，实际 %d", missing.Code)
	}
}

// 聊天测试台：`?uid=` 把选号钉到指定账号，且沿用 /v1 的响应形状。
func TestAdminPlaygroundForcedAccount(t *testing.T) {
	target := auth.NewFromLogin("444", "", "首选号", "a4", "r4", "copilot.tencent.com", "cn", 4102444800)
	other := auth.NewFromLogin("555", "", "备选号", "a5", "r5", "copilot.tencent.com", "cn", 4102444800)

	p := pool.New("")
	t.Cleanup(p.Close)
	p.Add(target)
	p.Add(other)

	up := upstream.New()
	var seenAuth string
	// 上游对聊天一律按 SSE 回（非流式请求也走同一条链路，由 handler 聚合），
	// 所以 fake 必须给 event-stream：一段普通 JSON 会被判成「流里没有有效事件」。
	up.ChatHTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seenAuth = r.Header.Get("Authorization")
		body := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\"," +
			"\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"pong\"}}]}\n\n" +
			"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\"," +
			"\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]," +
			"\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n" +
			"data: [DONE]\n\n"
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}

	h := NewHandler(Config{Pool: p, Upstream: up, APIKey: "test-key", AdminEnabled: true})

	payload := `{"model":"glm-5.2","messages":[{"role":"user","content":"ping"}]}`
	w := adminReq(h, http.MethodPost, "/admin/playground/chat?uid=444", strings.NewReader(payload))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("响应应透传上游内容：%s", w.Body.String())
	}
	if seenAuth != "Bearer a4" {
		t.Fatalf("应使用指定账号 444 的 accessToken，实际 %q", seenAuth)
	}
	if got := p.List()[0].InFlight; got != 0 {
		t.Fatalf("请求结束应释放在途租约，实际 in_flight=%d", got)
	}
}

// 扫码加号：start → poll(pending) → poll(saved) 全链路，落盘到 auth_dir，
// 且文件能被池正常解析回来。
func TestAdminAuthStartPollWritesFile(t *testing.T) {
	dir := t.TempDir()
	var pollCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/state"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"state":"st-1","authUrl":"https://example.test/qr/st-1"}}`))
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/token"):
			pollCalls++
			if pollCalls == 1 {
				// 第一次：还没扫码（上游用 code!=0 表示 pending）
				_, _ = w.Write([]byte(`{"code":10001,"msg":"login ing"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"AT","refreshToken":"RT","expiresIn":7200,"domain":"copilot.tencent.com"}}`))
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/login/account"):
			if r.Header.Get("Authorization") != "Bearer AT" {
				t.Errorf("账号端点必须带 Bearer token，实际 %q", r.Header.Get("Authorization"))
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"uid":"777","enterpriseId":"e1","nickname":"新号"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	login := loginsvc.New()
	login.SetBases(srv.URL, srv.URL)
	p := pool.New("")
	t.Cleanup(p.Close)
	h := NewHandler(Config{
		Pool: p, Upstream: upstream.New(), APIKey: "test-key",
		AdminEnabled: true, AuthDir: dir, Login: login,
	})

	w := adminReq(h, http.MethodPost, "/admin/auth/start", strings.NewReader(`{"realm":"cn"}`))
	if w.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", w.Code, w.Body.String())
	}
	var start struct {
		State   string `json:"state"`
		AuthURL string `json:"auth_url"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &start)
	if start.State != "st-1" || start.AuthURL == "" {
		t.Fatalf("start 响应不符：%s", w.Body.String())
	}

	pending := adminReq(h, http.MethodGet, "/admin/auth/poll?state="+start.State+"&realm=cn", nil)
	if pending.Code != http.StatusOK || !strings.Contains(pending.Body.String(), "pending") {
		t.Fatalf("首次 poll 应为 pending，实际 %d %s", pending.Code, pending.Body.String())
	}

	saved := adminReq(h, http.MethodGet, "/admin/auth/poll?state="+start.State+"&realm=cn", nil)
	if saved.Code != http.StatusOK || !strings.Contains(saved.Body.String(), "saved") {
		t.Fatalf("二次 poll 应 saved，实际 %d %s", saved.Code, saved.Body.String())
	}

	auths, err := auth.LoadDir(dir)
	if err != nil || len(auths) != 1 {
		t.Fatalf("落盘后应能解析出 1 个账号：err=%v n=%d", err, len(auths))
	}
	got := auths[0]
	if got.UID != "777" || got.Nickname != "新号" || got.Realm() != "cn" {
		t.Fatalf("落盘内容不符：uid=%s nickname=%s realm=%s", got.UID, got.Nickname, got.Realm())
	}
	if got.AccessTokenValue() != "AT" || got.RefreshTokenValue() != "RT" {
		t.Fatalf("token 未按预期写入")
	}
	if got.ExpiresAt == 0 {
		t.Fatal("expiresAt 必须由 expiresIn 换算后写入，否则 token 会被判成永久有效")
	}

	// state 是一次性的：同一个 state 不能再换一次账号。
	again := adminReq(h, http.MethodGet, "/admin/auth/poll?state="+start.State+"&realm=cn", nil)
	if again.Code == http.StatusOK && strings.Contains(again.Body.String(), "saved") {
		t.Fatalf("state 应一次性消费，重放仍成功：%s", again.Body.String())
	}
}

// uid 会拼进文件名：非法形态必须拒绝落盘（防目录穿越）。
func TestAdminAuthPollRejectsBadUID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/state"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"state":"s","authUrl":"u"}}`))
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/token"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"AT","refreshToken":"RT","expiresIn":1}}`))
		default:
			_, _ = w.Write([]byte(`{"code":0,"data":{"uid":"../../evil","nickname":"x"}}`))
		}
	}))
	defer srv.Close()

	login := loginsvc.New()
	login.SetBases(srv.URL, srv.URL)
	dir := t.TempDir()
	p := pool.New("")
	t.Cleanup(p.Close)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "test-key",
		AdminEnabled: true, AuthDir: dir, Login: login})

	w := adminReq(h, http.MethodGet, "/admin/auth/poll?state=s&realm=cn", nil)
	if w.Code == http.StatusOK {
		t.Fatalf("非法 uid 不应落盘成功：%s", w.Body.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("非法 uid 不应写出任何文件：%v", entries)
	}
}

// 批量签到在「调度器未接线」时回 501，而不是静默成功。
func TestAdminCheckinAllWithoutScheduler(t *testing.T) {
	h := newAdminHandler(t, nil, "")
	w := adminReq(h, http.MethodPost, "/admin/accounts/checkin", strings.NewReader("{}"))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("未接线应 501，实际 %d body=%s", w.Code, w.Body.String())
	}
}

// 单号签到：缺 refreshToken 的账号要明确拒绝（否则用户看到的是上游一句看不懂的报错）。
func TestAdminCheckinOneWithoutRefreshToken(t *testing.T) {
	acc := auth.NewFromLogin("888", "", "无刷新令牌", "a8", "", "copilot.tencent.com", "cn", 4102444800)
	h := newAdminHandler(t, []*auth.Auth{acc}, "")
	w := adminReq(h, http.MethodPost, "/admin/accounts/888/checkin", strings.NewReader("{}"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺 refreshToken 应 400，实际 %d body=%s", w.Code, w.Body.String())
	}
}
