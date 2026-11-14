package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 根路径给管理后台入口页（HTML，含主题与脚本引用）。
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
	for _, want := range []string{"WorkPuppy", "/assets/tabler.min.css", "/assets/app.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("入口页缺少 %q", want)
		}
	}
	// <head> 里不能混入非空白文本：浏览器会提前结束 head，把那串字符渲染到页面顶部。
	// 真实事故：模板里误写成字面量 `n（不是换行），页面左上角多出一个 "`n"。
	if strings.Contains(body, "`n") {
		t.Fatalf("入口页含字面量 `n（会被浏览器渲染到页面顶部）：%q", body[:200])
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

// 内嵌资源可访问且类型正确（主题样式、Bootstrap 交互脚本与 app.js 都在 embed.FS 里）。
func TestAssetsServed(t *testing.T) {
	cases := map[string]string{
		"/assets/tabler.min.css":          "text/css",
		"/assets/bootstrap.bundle.min.js": "javascript",
		"/assets/echarts.min.js":          "javascript",
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
// 且 app.css 必须在主题样式表（tabler.min.css）之后加载，这条规则才生效。
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

	// 顺序：tabler.min.css 先、app.css 后，覆盖才成立。
	idx := httptest.NewRecorder()
	Handler().ServeHTTP(idx, httptest.NewRequest(http.MethodGet, "/", nil))
	html := idx.Body.String()
	theme := strings.Index(html, "/assets/tabler.min.css")
	app := strings.Index(html, "/assets/app.css")
	if theme < 0 || app < 0 || theme > app {
		t.Fatalf("样式表加载顺序不对（tabler=%d, app=%d）：[hidden] 规则会被主题样式覆盖", theme, app)
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

// 扫码加号必须能显示二维码（不只是链接）。
//
// 回归背景：前端曾只渲染 auth_url 文本，用户得自己复制链接；现在 app.js 通过
// fetch('/admin/qr?...') 取 PNG blob 挂到 <img id="qrImg">。这里断言"接线还在"：
// 入口页有该 img，脚本里确实调用了 /admin/qr，样式里给了白底+静区。
func TestAdminPageHasQRCodeWiring(t *testing.T) {
	idx := httptest.NewRecorder()
	Handler().ServeHTTP(idx, httptest.NewRequest(http.MethodGet, "/", nil))
	html := idx.Body.String()
	for _, want := range []string{`id="qrImg"`, `id="addUrl"`, `id="btnAuthStart"`} {
		if !strings.Contains(html, want) {
			t.Errorf("入口页缺少 %s", want)
		}
	}

	js := httptest.NewRecorder()
	Handler().ServeHTTP(js, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if !strings.Contains(js.Body.String(), "/admin/qr") {
		t.Fatal("app.js 没有调用 /admin/qr：扫码加号只会显示链接，不显示二维码")
	}

	css := httptest.NewRecorder()
	Handler().ServeHTTP(css, httptest.NewRequest(http.MethodGet, "/assets/app.css", nil))
	if !strings.Contains(css.Body.String(), ".qr-img") {
		t.Error("app.css 缺少 .qr-img（二维码需要白底/静区/像素化渲染）")
	}
}

// 缓存策略：入口页 no-cache（每次拿到最新的资源 URL）；/assets/* 的 URL 带内容哈希
// `?v=…`，所以可以 immutable 长缓存一年；/assets/version.json 永远 no-store，
// 长驻页面靠它发现"服务端换了新前端"。
func TestAssetCachePolicy(t *testing.T) {
	h := Handler()
	ver := Version()
	if len(ver) != 12 {
		t.Fatalf("Version()=%q，期望 12 位十六进制", ver)
	}

	// 入口页：no-cache + ETag（未变回 304），且资源 URL 带上当前版本。
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("/ code=%d", w.Code)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("/ Cache-Control=%q want no-cache", cc)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("/ 缺 ETag（无法做 304 校验）")
	}
	html := w.Body.String()
	if !strings.Contains(html, "/assets/app.js?v="+ver) {
		t.Error("入口页的资源 URL 未带 ?v=<Version()>")
	}
	if strings.Contains(html, "__WP_VERSION__") {
		t.Error("入口页仍残留 __WP_VERSION__ 占位符")
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("If-None-Match", etag)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req)
	if w2.Code != http.StatusNotModified || w2.Body.Len() != 0 {
		t.Errorf("入口页带 If-None-Match 应回 304 空体，实际 %d/%d", w2.Code, w2.Body.Len())
	}

	// 静态资源：一年 immutable（URL 版本化，内容变则 URL 变）。
	for _, path := range []string{"/assets/app.js", "/assets/app.css", "/assets/tabler.min.css", "/assets/bootstrap.bundle.min.js", "/assets/echarts.min.js"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s code=%d", path, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
			t.Errorf("%s Cache-Control=%q want immutable/max-age=31536000", path, cc)
		}
	}

	// 版本探针：no-store，值必须与 Version() 一致（JS 用它判断是否需要更新）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/version.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("version.json code=%d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("version.json Cache-Control=%q want no-store", cc)
	}
	if !strings.Contains(rec.Body.String(), `"version":"`+ver+`"`) {
		t.Errorf("version.json 内容=%s，want version=%s", rec.Body.String(), ver)
	}
}

// CI 冒烟里出现的 /assets/* 必须真实存在（防"改名/换框架后 CI 静默变红"）。
//
// 回归背景：UI 从 Bootstrap CSS 迁到 Tabler 后，docker.yml 的冒烟仍在 curl
// 已不存在的 /assets/bootstrap.min.css，拿到 19B 的 404 页面 → 冒烟失败 →
// 镜像连续几个版本没打成，部署侧一直拿不到新界面。这条测试把"CI 引用的资源"
// 与"embed 里实际有的文件"绑在一起：谁改名，go test 先红。
func TestCIAssetReferencesExist(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "docker.yml"))
	if err != nil {
		t.Skipf("读不到 docker.yml（非仓库布局？）：%v", err)
	}
	re := regexp.MustCompile(`/assets/([A-Za-z0-9._-]+)`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		if _, err := fs.Stat(assetsFS, "assets/"+name); err != nil {
			t.Errorf("docker.yml 引用了 /assets/%s，但 embed 里没有这个文件（冒烟会拿到 404）", name)
		}
	}
	if len(seen) == 0 {
		t.Fatal("docker.yml 里没找到任何 /assets 引用：冒烟断言可能已被删空")
	}
	// 反向：webui 自己的资源文件如果没被 CI 冒烟覆盖，也提示一下（不失败）。
	for _, name := range []string{"app.js", "app.css"} {
		if !seen[name] {
			t.Logf("提示：docker.yml 未校验 /assets/%s", name)
		}
	}
}

// 单账号积分曲线必须"点得到、画得出"。
//
// 回归背景：概览的积分日账本只有全局汇总，看不出某个号的钱是怎么走的；
// 现在账号页每行第一列的 UID 可点，弹窗里按 7/15/30 天画 4 条折线
// （日终总积分 / 当日获得 / 当日消耗 / 当日修正）。数据来自
// /admin/credit-daily 的 histories[]（已含每账号明细），所以这里断言
// "接线还在"：入口页有弹窗与区间按钮、脚本里有图表与铺日历的实现、
// 样式里有图表容器。
func TestAccountCreditChartWiring(t *testing.T) {
	idx := httptest.NewRecorder()
	Handler().ServeHTTP(idx, httptest.NewRequest(http.MethodGet, "/", nil))
	html := idx.Body.String()
	for _, want := range []string{
		`id="creditChartModal"`, `id="creditModalChart"`, `id="creditModalRange"`,
		`data-days="7"`, `data-days="15"`, `data-days="30"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("入口页缺少 %s（积分曲线弹窗/区间按钮）", want)
		}
	}
	// 概览：整池积分曲线（原日账本表格已改为折线图）+ 4 个总计卡片 + 网关状态默认折叠。
	for _, want := range []string{
		`id="kpiCredits"`, `id="kpiEarned"`, `id="kpiSpent"`, `id="kpiAdjust"`,
		`id="overviewChart"`, `id="overviewRange"`,
		`data-bs-target="#statusRawWrap"`, `id="statusRawWrap"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("入口页缺少 %s（积分总计卡片/概览曲线/网关状态折叠）", want)
		}
	}
	// 顺序：积分日账本在网关状态上方。
	creditAt := strings.Index(html, `id="creditCard"`)
	statusAt := strings.Index(html, `id="statusRawWrap"`)
	if creditAt < 0 || statusAt < 0 || creditAt > statusAt {
		t.Fatalf("积分日账本应在网关状态上方（credit=%d, status=%d）", creditAt, statusAt)
	}
	// ECharts 自托管（CSP 只允许 self，不能引 CDN），且必须在 app.js 之前加载。
	ecAt := strings.Index(html, "/assets/echarts.min.js")
	appAt := strings.Index(html, "/assets/app.js")
	if ecAt < 0 {
		t.Fatal("入口页缺少 /assets/echarts.min.js（积分曲线依赖）")
	}
	if appAt < 0 || ecAt > appAt {
		t.Fatalf("echarts 必须在 app.js 之前加载（echarts=%d, app=%d）", ecAt, appAt)
	}
	// 账号表：昵称在第一列、UID 在第二列（点昵称开曲线），且不再有右对齐列。
	nickAt := strings.Index(html, "<th>昵称</th>")
	uidAt := strings.Index(html, "<th>UID</th>")
	if nickAt < 0 || uidAt < 0 || nickAt > uidAt {
		t.Fatalf("账号表头顺序应为 昵称 → UID（nick=%d, uid=%d）", nickAt, uidAt)
	}
	// 账号 / 模型两表的表头与单元格一律左对齐（原来 积分 / 在途 / 操作、上下文 /
	// 最大输出 是 text-end 右对齐；积分日账本那张表仍保持数字右对齐，不在本次约束内）。
	for _, bad := range []string{
		`<th class="text-end">积分</th>`,
		`<th class="text-end" title=`,
		`<th class="text-end">操作</th>`,
		`<th class="text-end">上下文</th>`,
		`<th class="text-end">最大输出</th>`,
	} {
		if strings.Contains(html, bad) {
			t.Errorf("账号/模型表不应再有右对齐表头：%s", bad)
		}
	}
	// 折叠容器默认不带 show（= 收起）。
	if strings.Contains(html, `id="statusRawWrap" class="collapse show"`) {
		t.Error("网关状态默认应折叠（collapse 不能带 show）")
	}

	js := httptest.NewRecorder()
	Handler().ServeHTTP(js, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	body := js.Body.String()
	for _, want := range []string{
		"data-chart-uid", "openCreditChart", "chartRows", "dayLabels", "CREDIT_SERIES",
		"renderOverviewChart", "renderChart", "creditChartOption", "window.echarts",
		// 对数压缩：喂 log1p、轴刻度与 tooltip 用 invLog 还原原始积分；
		// 基准线走"常识刻度"（0/10/50/100/500/1k/5k/1万…）而不是等分
		"logData", "invLog", "chartBaselines", "markLine",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("app.js 缺少 %s（积分曲线/缺失日期补 0）", want)
		}
	}

	css := httptest.NewRecorder()
	Handler().ServeHTTP(css, httptest.NewRequest(http.MethodGet, "/assets/app.css", nil))
	// ECharts 画在 canvas 上：宿主必须有明确高度，弹窗里给更高一档。
	for _, want := range []string{".credit-chart-host", ".credit-chart-modal", "height: 320px", "height: 360px"} {
		if !strings.Contains(css.Body.String(), want) {
			t.Errorf("app.css 缺少 %s（图表容器高度）", want)
		}
	}
	// 表头不换行 + 模型表列宽比例：回归背景是「最大输/出」「积分倍/率」被挤成两行。
	for _, want := range []string{
		"#view-accounts table th", "#view-models table th", "white-space: nowrap",
		"table-layout: fixed", "min-width: 1020px", "nth-child(7)",
	} {
		if !strings.Contains(css.Body.String(), want) {
			t.Errorf("app.css 缺少 %s（表头不换行/模型表列宽比例）", want)
		}
	}
}

// 品牌字标：登录页与导航栏都只显示 WorkPuppy（旧的「WB」徽标 + 「WP」文字已移除）。
//
// 回归背景：品牌原来是 `<span class="badge">WB</span>WP` 的两段式，改字标时容易只改
// 一处（登录页或导航栏），或者漏掉后半段「WP」——这里两处一起锁。
func TestBrandWordmark(t *testing.T) {
	w := httptest.NewRecorder()
	Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	html := w.Body.String()

	if got := strings.Count(html, ">WorkPuppy<"); got != 2 {
		t.Errorf("登录页与导航栏应各有一处 WorkPuppy 字标，实际 %d 处", got)
	}
	if strings.Contains(html, ">WB</span>") {
		t.Error("旧「WB」徽标应已移除")
	}
	if strings.Contains(html, ">WP<") || strings.Contains(html, "</span>WP") {
		t.Error("字标旁不应再保留「WP」文字")
	}
	// 浏览器标签页标题同样用品牌全名（改字标时最容易漏的一处）。
	if !strings.Contains(html, "<title>WorkPuppy 管理后台</title>") {
		t.Error("标签页标题应为「WorkPuppy 管理后台」")
	}
	// favicon 是内联 SVG 的 data URI，里面的字是 URL 编码的 `>WB<` / `>WP<`。
	if !strings.Contains(html, "%3EWP%3C/text%3E") {
		t.Error("favicon 里的字应改为 WP")
	}
	if strings.Contains(html, "%3EWB%3C/text%3E") {
		t.Error("favicon 里不应再是旧的 WB")
	}
}

// 扫码授权的有效期必须"看得见、过期后明确失效"。
//
// 回归背景：原先只写"已开始轮询授权结果"，用户不知道还能扫多久；state 过期后
// 界面毫无变化，扫了才失败。现在要求：倒计时徽标 + 失效遮罩 + 链接删除线。
// 这里断言三块接线都在（行为本身由 scripts/ui-login-check.mjs 的真浏览器用例覆盖）。
func TestAuthExpiryWiring(t *testing.T) {
	idx := httptest.NewRecorder()
	Handler().ServeHTTP(idx, httptest.NewRequest(http.MethodGet, "/", nil))
	html := idx.Body.String()
	for _, want := range []string{`id="addCountdown"`, `id="qrOverlay"`, `id="addHint"`} {
		if !strings.Contains(html, want) {
			t.Errorf("入口页缺少 %s（倒计时/失效遮罩）", want)
		}
	}

	js := httptest.NewRecorder()
	Handler().ServeHTTP(js, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	body := js.Body.String()
	for _, want := range []string{"startAuthCountdown", "expireAuth", "renderAuthCountdown", "e.status === 410"} {
		if !strings.Contains(body, want) {
			t.Errorf("app.js 缺少 %s（倒计时/过期处理）", want)
		}
	}

	css := httptest.NewRecorder()
	Handler().ServeHTTP(css, httptest.NewRequest(http.MethodGet, "/assets/app.css", nil))
	cssBody := css.Body.String()
	for _, want := range []string{".qr-overlay", ".auth-expired", "text-decoration: line-through"} {
		if !strings.Contains(cssBody, want) {
			t.Errorf("app.css 缺少 %s（失效遮罩/删除线样式）", want)
		}
	}
}

// 设置页与首次初始化的"接线"：页面骨架、脚本调用、样式锚点都在。
//
// 这些 id/函数名是前后端的契约（app.js 直接按 id 取节点、按路径调接口），
// 改 HTML 结构时最容易漏掉其中一处，表现是"页面能开但按钮没反应"。
func TestSettingsAndSetupWiring(t *testing.T) {
	idx := httptest.NewRecorder()
	Handler().ServeHTTP(idx, httptest.NewRequest(http.MethodGet, "/", nil))
	html := idx.Body.String()
	for _, want := range []string{
		`id="setupForm"`, `id="setupPassword"`, `id="setupError"`,
		`data-view="settings"`, `id="view-settings"`, `id="cfgForm"`,
		`id="cfgGroups"`, `id="btnSaveConfig"`, `id="btnResetAll"`,
		`id="btnChangePassword"`, `id="restartBanner"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("入口页缺少 %s（设置页/初始化向导）", want)
		}
	}

	js := httptest.NewRecorder()
	Handler().ServeHTTP(js, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	body := js.Body.String()
	for _, want := range []string{
		"/admin/setup/status", "/admin/setup", "/admin/config", "/admin/password",
		"renderSettings", "validateField", "resetConfig", "updateRestartBanner",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("app.js 缺少 %s", want)
		}
	}

	css := httptest.NewRecorder()
	Handler().ServeHTTP(css, httptest.NewRequest(http.MethodGet, "/assets/app.css", nil))
	cssBody := css.Body.String()
	for _, want := range []string{".wp-cfg-row", ".wp-cfg-savebar", ".wp-restart-banner"} {
		if !strings.Contains(cssBody, want) {
			t.Errorf("app.css 缺少 %s", want)
		}
	}
}

// 账号表：**任何列都不换行**（含 36 字符 UID），状态列展示 状态 / 猫猫 / 已签。
//
// 回归背景：曾用 fixed + 百分比列宽（UID 8%），UID 被挤成 2~3 行；现在改成
// 自动布局 + 单元格 nowrap，窄屏交给 .table-responsive 横向滚动。
// 猫猫状态读池里的快照（pool.CatState，由旅行巡检写入），页面打开时不为每行
// 额外打上游；没巡检过显示「猫未巡检」而不是留空。
func TestAccountsNowrapAndCatColumn(t *testing.T) {
	js := httptest.NewRecorder()
	Handler().ServeHTTP(js, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	body := js.Body.String()
	for _, want := range []string{"catBadge", "badge-row", "a.cat", "fmtAgo", "猫未巡检", "text: '已签'", "text: '未签'", "已领奖", "未领养", "待派出", "查询失败"} {
		if !strings.Contains(body, want) {
			t.Errorf("app.js 缺少 %s（账号表猫猫/签到标签）", want)
		}
	}
	// 状态列顺序固定：在线状态 → 签到状态 → 领养（猫）状态。
	iStatus := strings.Index(body, "          statusBadge(a),")
	iCheckin := strings.Index(body, "            text: '已签',")
	iCat := strings.Index(body, "          catBadge(a),")
	if iStatus < 0 || iCheckin < 0 || iCat < 0 || !(iStatus < iCheckin && iCheckin < iCat) {
		t.Errorf("状态列顺序应为 在线(%d) → 已签(%d) → 猫猫(%d)", iStatus, iCheckin, iCat)
	}

	css := httptest.NewRecorder()
	Handler().ServeHTTP(css, httptest.NewRequest(http.MethodGet, "/assets/app.css", nil))
	cssBody := css.Body.String()
	for _, want := range []string{
		"#view-accounts table th",
		"white-space: nowrap",
		"flex-wrap: nowrap", "text-bg-secondary-subtle",
		".cat-badge",
		"#view-accounts .badge-row",
	} {
		if !strings.Contains(cssBody, want) {
			t.Errorf("app.css 缺少 %s（账号表不换行/猫猫标签）", want)
		}
	}
	// 百分比列宽是 UID 折行的根因，不允许再回来。
	if strings.Contains(cssBody, "#view-accounts table th:nth-child") {
		t.Error("账号表不应再用百分比列宽（会导致 UID 折行）")
	}
}

// 概览轮询 + 前端版本更新的接线：刷新不闪屏（没有启动 Logo），资源 URL 带版本号，
// 长驻页面能通过 version.json 发现服务端换了新前端。
func TestOverviewPollingAndVersionWiring(t *testing.T) {
	ver := Version()
	idx := httptest.NewRecorder()
	Handler().ServeHTTP(idx, httptest.NewRequest(http.MethodGet, "/", nil))
	html := idx.Body.String()
	for _, want := range []string{`id="overviewUpdated"`, `id="loginWrap" hidden`, `id="versionBanner"`, `id="btnApplyUpdate"`, `name="wp-frontend-version"`} {
		if !strings.Contains(html, want) {
			t.Errorf("入口页缺少 %s", want)
		}
	}
	if strings.Contains(html, "bootSplash") {
		t.Error("入口页不应再有启动遮罩（会闪 Logo）")
	}
	if !strings.Contains(html, "?v="+ver) {
		t.Error("资源 URL 应带 ?v=<Version()>（长缓存的前提）")
	}

	js := httptest.NewRecorder()
	Handler().ServeHTTP(js, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	body := js.Body.String()
	for _, want := range []string{
		"/admin/overview", "refreshOverview", "startOverviewPolling", "OVERVIEW_POLL_MS",
		"checkFrontendVersion", "pageVersion", "/assets/version.json", "applyFrontendUpdate", "wp.frontend.version",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("app.js 缺少 %s（概览轮询/前端版本）", want)
		}
	}

	css := httptest.NewRecorder()
	Handler().ServeHTTP(css, httptest.NewRequest(http.MethodGet, "/assets/app.css", nil))
	cssBody := css.Body.String()
	if !strings.Contains(cssBody, ".wp-version-banner") {
		t.Error("app.css 缺少 .wp-version-banner（版本更新提示）")
	}
	if strings.Contains(cssBody, ".wp-boot-text") {
		t.Error("app.css 不应再保留启动遮罩样式")
	}
}

// 弹窗里的积分曲线必须"每次打开都能画"。
//
// 回归背景（实测）：openCreditChart 用 replaceChildren 显示"加载中…"，会把 ECharts 的
// <canvas> 从宿主里摘掉；而缓存的实例 isDisposed() 仍是 false，setOption 就画到了脱离
// 文档的 canvas 上——表现为"第一次打开有图，之后全空白"。
// 这里钉住两件事：①renderChart 走 chartFor（检测 canvas 丢失后重建实例）；
// ②打开弹窗前会 dispose 旧实例。真浏览器验证见提交说明（Edge + CDP 连开两次均 PASS）。
func TestCreditChartRecreatesStaleInstance(t *testing.T) {
	js := httptest.NewRecorder()
	Handler().ServeHTTP(js, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	body := js.Body.String()
	for _, want := range []string{"function chartFor(", "host.querySelector('canvas')", "chartInstances.delete('creditModalChart')"} {
		if !strings.Contains(body, want) {
			t.Errorf("app.js 缺少 %q（弹窗图表第二次会画不出来）", want)
		}
	}
}

// 内嵌的第三方资源不应带 sourceMappingURL：仓库不提供 .map，devtools 会报 404 噪音。
func TestAssetsHaveNoDanglingSourceMaps(t *testing.T) {
	for _, path := range []string{"/assets/bootstrap.bundle.min.js", "/assets/tabler.min.css"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s code=%d", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "sourceMappingURL") {
			t.Errorf("%s 仍带 sourceMappingURL（对应的 .map 不存在，devtools 会报错）", path)
		}
	}
}

// 概览文案：KPI 卡与图表系列的措辞是对外契约，改动会直接影响运维读表。
func TestOverviewWording(t *testing.T) {
	idx := httptest.NewRecorder()
	Handler().ServeHTTP(idx, httptest.NewRequest(http.MethodGet, "/", nil))
	html := idx.Body.String()
	for _, want := range []string{
		"账号总数", "可用账号", "冷却中账号", "已禁用账号", "在途占满", "粘性会话",
		"总积分", "今日获得", "今日消耗", "今日调整",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("概览缺少文案 %q", want)
		}
	}

	js := httptest.NewRecorder()
	Handler().ServeHTTP(js, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	body := js.Body.String()
	for _, want := range []string{
		"name: '日终总计'", "name: '获得'", "name: '消耗'", "name: '调整'",
		"日终总计：", "获得：", "消耗：", "调整：",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("图表文案缺少 %q（概览/弹窗系列名）", want)
		}
	}
	// 回归：四条线不能再用 stack —— 堆叠会把"调整=0"画到获得/消耗的累计高度上，
	// 工具提示是 0、图上却不是 0（真实 bug）。也不应保留互相遮挡的面积填充。
	if strings.Contains(body, "stack: 'credit'") {
		t.Error("积分曲线不应再堆叠（0 值的序列会被抬高）")
	}
	if !strings.Contains(html, "四条线各自独立") {
		t.Error("概览图表说明应写明四条线各自独立")
	}
}
