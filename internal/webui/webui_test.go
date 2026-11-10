package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 根路径给管理后台入口页（HTML，含 Bootstrap 引用）。
func TestIndexServed(t *testing.T) {
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("GET / = %d，want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q，want text/html", ct)
	}
	body := w.Body.String()
	for _, want := range []string{"WP", "/assets/bootstrap.min.css", "/assets/app.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("入口页缺少 %q", want)
		}
	}
	// CSP 必须下发：管理后台能停号/发密钥，XSS 一次就全丢。
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("缺少 CSP：%q", csp)
	}
	// 入口页不做长缓存：升级后要立刻看到新界面。
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("入口页 Cache-Control = %q，want no-cache", cc)
	}
}

// 内嵌资源可访问且类型正确（Bootstrap 与 app.js 都在 embed.FS 里）。
func TestAssetsServed(t *testing.T) {
	cases := map[string]string{
		"/assets/bootstrap.min.css":       "text/css",
		"/assets/bootstrap.bundle.min.js": "javascript",
		"/assets/app.js":                  "javascript",
		"/assets/app.css":                 "text/css",
	}
	for path, wantType := range cases {
		w := httptest.NewRecorder()
		Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d，want 200", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, wantType) {
			t.Errorf("%s Content-Type = %q，want 含 %q", path, ct, wantType)
		}
		if w.Body.Len() == 0 {
			t.Errorf("%s 内容为空", path)
		}
	}
}

// 非界面路径回 JSON 404（别把 HTML 当成接口响应）。
func TestUnknownPathJSON404(t *testing.T) {
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET /nope = %d，want 404", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("Content-Type = %q，want application/json", ct)
	}
	if !strings.Contains(w.Body.String(), "/v1") {
		t.Errorf("404 体应提示接口位置：%s", w.Body.String())
	}
}

// 不存在的资源也是 404（FileServer 行为），不应泄漏目录结构。
func TestMissingAsset(t *testing.T) {
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/assets/nope.js", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET /assets/nope.js = %d，want 404", w.Code)
	}
}

// 路径穿越不能读到 assets 之外的文件（embed.FS 本身也不含其它内容，这里守行为）。
func TestNoTraversal(t *testing.T) {
	for _, p := range []string{"/assets/../../go.mod", "/assets/..%2fgo.mod"} {
		w := httptest.NewRecorder()
		Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, p, nil))
		if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "module wp") {
			t.Fatalf("%s 读到了仓库内文件", p)
		}
	}
}

// hidden 属性必须真的隐藏元素。
//
// 回归背景（实测 bug）：登录成功后 app.js 把 `#loginWrap.hidden = true`、`#app.hidden = false`，
// 但登录页容器带着 Bootstrap 的 `d-flex`，而 `.d-flex{display:flex!important}` 会盖掉
// UA 样式里的 `[hidden]{display:none}` —— 表现为"密码正确、/admin/status 也 200，
// 页面却还停在登录页"。app.css 里用 `[hidden]{display:none!important}` 钉死优先级，
// 且 app.css 必须在 bootstrap.min.css 之后加载，这条规则才生效。
func TestHiddenAttributeIsAuthoritative(t *testing.T) {
	css := httptest.NewRecorder()
	Handler().ServeHTTP(css, httptest.NewRequest(http.MethodGet, "/assets/app.css", nil))
	if css.Code != http.StatusOK {
		t.Fatalf("GET /assets/app.css = %d", css.Code)
	}
	body := css.Body.String()
	if !strings.Contains(body, "[hidden]") || !strings.Contains(body, "display: none !important") {
		t.Fatalf("app.css 缺少 `[hidden] { display: none !important }`：" +
			"登录后页面不会切到主界面（Bootstrap 的 d-flex 会盖掉 hidden）")
	}

	// 顺序：bootstrap.min.css 先、app.css 后，覆盖才成立。
	idx := httptest.NewRecorder()
	Handler().ServeHTTP(idx, httptest.NewRequest(http.MethodGet, "/", nil))
	html := idx.Body.String()
	bs := strings.Index(html, "/assets/bootstrap.min.css")
	app := strings.Index(html, "/assets/app.css")
	if bs < 0 || app < 0 || bs > app {
		t.Fatalf("样式表加载顺序不对（bootstrap=%d, app=%d）：[hidden] 规则会被 Bootstrap 覆盖", bs, app)
	}

	// 登录页与主界面都靠 hidden 切换，这两个 id 必须存在。
	for _, id := range []string{`id="loginWrap"`, `id="app"`} {
		if !strings.Contains(html, id) {
			t.Errorf("入口页缺少 %s", id)
		}
	}
	if !strings.Contains(html, `id="app" hidden`) {
		t.Errorf(`"#app 初始必须是 hidden（避免登录前露出空面板）`)
	}
}
