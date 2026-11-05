package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/adminauth"
	"workbuddy2api/internal/pool"
	"workbuddy2api/internal/upstream"
	"workbuddy2api/internal/webui"
)

// 管理后台挂在 `GET /` 之后，具体路径必须仍然走各自的 handler。
//
// 这条测试守的是一个很容易出事的点：注册 catch-all（`GET /`）后，如果 ServeMux
// 的优先级判断写错（或将来换路由器），`/status`、`/v1/*`、`/admin/*` 会被页面抢走，
// 表现是「浏览器能打开后台，但 SDK 报 HTML 解析错误」——这类故障在浏览器里完全看不出来。
func TestUIMountedDoesNotShadowAPI(t *testing.T) {
	p := pool.New("")
	t.Cleanup(p.Close)
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     upstream.New(),
		APIKey:       testAPIKey,
		AdminAuth:    adminauth.MustNew(testAdminPW),
		AdminEnabled: true,
		UI:           webui.Handler(),
	})

	cases := []struct {
		name   string
		method string
		path   string
		// auth 三种取值："" 不带头、"api" 数据面 key、"admin" 管理密码 ——
		// 两套凭据的边界正是本文件要钉住的东西。
		auth string
		want int
		// bodyHas 非空时断言响应体包含该片段（用来确认「命中的是 API 而不是页面」）。
		bodyHas string
	}{
		{name: "后台入口", method: http.MethodGet, path: "/", want: http.StatusOK, bodyHas: "/assets/app.js"},
		{name: "登录页在入口里", method: http.MethodGet, path: "/", want: http.StatusOK, bodyHas: `id="loginForm"`},
		{name: "内嵌样式", method: http.MethodGet, path: "/assets/app.css", want: http.StatusOK},
		// /healthz 在「没有任何健康账号」时按设计回 503（活着但不可服务），
		// 这里只断言它仍然由 healthz handler 应答（JSON + service 字段），
		// 不被后台页面接管。
		{name: "存活探测仍无鉴权", method: http.MethodGet, path: "/healthz", want: http.StatusServiceUnavailable, bodyHas: `"service"`},
		{name: "状态接口仍要鉴权", method: http.MethodGet, path: "/status", want: http.StatusUnauthorized},
		{name: "状态接口带 api_key 走 JSON", method: http.MethodGet, path: "/status", auth: "api", want: http.StatusOK, bodyHas: `"accounts"`},
		{name: "网关入口不被页面吞掉", method: http.MethodPost, path: "/v1/chat/completions", want: http.StatusUnauthorized},
		{name: "模型列表走 API", method: http.MethodGet, path: "/v1/models", auth: "api", want: http.StatusOK, bodyHas: `"object"`},
		{name: "管理面要管理密码", method: http.MethodGet, path: "/admin/accounts", want: http.StatusUnauthorized},
		{name: "管理面不接受 api_key", method: http.MethodGet, path: "/admin/accounts", auth: "api", want: http.StatusUnauthorized},
		{name: "管理面接受管理密码", method: http.MethodGet, path: "/admin/accounts", auth: "admin", want: http.StatusOK, bodyHas: `"accounts"`},
		{name: "管理状态只认管理密码", method: http.MethodGet, path: "/admin/status", auth: "api", want: http.StatusUnauthorized},
		{name: "管理状态走管理密码", method: http.MethodGet, path: "/admin/status", auth: "admin", want: http.StatusOK, bodyHas: `"sticky_sessions"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			switch tc.auth {
			case "api":
				req.Header.Set("Authorization", "Bearer "+testAPIKey)
			case "admin":
				req.Header.Set("Authorization", "Bearer "+testAdminPW)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("%s %s = %d，want %d（body=%s）", tc.method, tc.path, w.Code, tc.want, w.Body.String())
			}
			if tc.bodyHas != "" && !strings.Contains(w.Body.String(), tc.bodyHas) {
				t.Fatalf("%s %s 响应不含 %q：%s", tc.method, tc.path, tc.bodyHas, w.Body.String())
			}
			// 任何 API 路径都不应该返回入口页的 HTML（那会让 SDK 解析失败）。
			if tc.path != "/" && strings.Contains(w.Body.String(), "/assets/app.js") {
				t.Fatalf("%s %s 被管理后台页面接管了", tc.method, tc.path)
			}
		})
	}
}

// 不设 UI 时（纯 API 部署/测试）根路径应当 404，而不是 panic。
func TestNoUIKeepsRoot404(t *testing.T) {
	p := pool.New("")
	t.Cleanup(p.Close)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "k"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("未挂 UI 时 GET / = %d，want 404", w.Code)
	}
}
