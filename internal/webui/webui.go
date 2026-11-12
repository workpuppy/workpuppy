// Package webui 管理后台静态界面（Bootstrap + 原生 JS，全部 embed 进二进制）。
//
// 选型说明（与需求一致）：
//   - **不用 npm / 不用 Node 构建**：Tabler 1.7.4 的 dist 编译产物直接 vendored 进仓库
//     （internal/webui/assets/tabler.min.css，内含 Bootstrap 5 的完整样式），交互组件
//     仍用 Bootstrap 的 bundle（assets/bootstrap.bundle.min.js）；改样式就改 app.css，
//     `go build` 即生效。
//   - **容器里不要 nginx**：页面与 API 同源由本进程托管，`/` 是后台入口，
//     `/v1/...` 仍是 OpenAI 兼容入口，`/admin/...` 是管理面 REST —— 一个进程一个端口。
//   - 前端只做展示：所有判定都在服务端（见 internal/server/admin_panel.go），
//     页面拿到的错误原文原样显示，不在这里"翻译"。
//
// 安全边界：
//   - 页面本身可以匿名打开（不含任何密钥），但每个数据请求都要带 `Authorization:
//     Bearer <网关 api_key>`，由服务端 withAuth 判定；api_key 只存在浏览器 localStorage。
//   - 响应统一带 CSP（default-src 'self'，禁 inline script / 外链），因此本包内不允许
//     出现内联 <script> 或 onclick= 属性；所有交互走 assets/app.js 的事件委托。
//   - 不开 `Access-Control-Allow-Origin`：跨站页面读不到管理面数据。
//
// 缓存策略（升级后必须立刻看到新界面，因此按"谁写的"分两类）：
//   - 第三方框架的 vendored 编译产物（tabler.min.css / bootstrap.bundle.min.js）：
//     只在升级 vendored 版本时变，体积大，保留短 max-age 缓存；
//   - 其余资源（index.html / app.js / app.css / NOTICE.txt 等我们自己维护的文件）：
//     `no-cache` + 内容哈希 ETag —— 浏览器每条请求都回来校验，未变回 304（几十字节），
//     变了立刻拿新的。这样不会出现"HTML 是新版、app.js 还是旧版"的半新半旧错配
//     （曾经踩过：概览改折线图后，旧 app.js 找不到新 DOM，图表区一片空白）。
package webui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
	"sync"
)

//go:embed assets
var assetsFS embed.FS

// frameworkAssets 第三方框架的 vendored 编译产物（按 embed 里的文件名判断）。
// 它们随 vendored 版本固定、体积大且极少变，保留 max-age 缓存；名单之外一律
// no-cache + ETag —— 新增自己的资源文件时什么都不用改，天然走"升级即时生效"。
var frameworkAssets = map[string]bool{
	"tabler.min.css":          true,
	"bootstrap.bundle.min.js": true,
}

// etagCache 资源内容哈希缓存：embed.FS 编译期固定，每个文件算一次即可。
var etagCache sync.Map // name -> etag（含引号，形如 `"a1b2c3..."`）

// contentETag 由内容算强 ETag（取 sha256 前 8 字节，够防碰撞且短）。
func contentETag(raw []byte) string {
	sum := sha256.Sum256(raw)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// assetETag 取某个 embed 资源的 ETag（读不到文件返回空串，交给 FileServer 出 404）。
func assetETag(sub fs.FS, name string) string {
	if v, ok := etagCache.Load(name); ok {
		return v.(string)
	}
	raw, err := fs.ReadFile(sub, name)
	if err != nil {
		return ""
	}
	et := contentETag(raw)
	etagCache.Store(name, et)
	return et
}

// etagMatched 判断 If-None-Match 是否命中我们的 ETag（容错弱校验前缀与逗号列表）。
func etagMatched(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		part = strings.TrimPrefix(part, "W/")
		if part == etag {
			return true
		}
	}
	return false
}

// CSP 管理后台的内容安全策略。
//
// 为什么比"能跑就行"更严：管理面能停号、能发密钥、能看到全部账号，XSS 一次就等于
// 全丢。这里只允许同源脚本/样式/连接，图片限定 self+data:（favicon），并禁掉内联
// 脚本与 iframe 嵌入。style-src 保留 'unsafe-inline' 是因为 Bootstrap 的部分组件
// 会在元素上写内联 style（如 modal 的 padding-right），禁掉会破坏布局。
const csp = "default-src 'self'; " +
	"img-src 'self' data:; " +
	"style-src 'self' 'unsafe-inline'; " +
	"script-src 'self'; " +
	"connect-src 'self'; " +
	"font-src 'self'; " +
	"frame-ancestors 'none'; " +
	"base-uri 'self'; " +
	"form-action 'self'"

// Handler 返回管理后台的 http.Handler，挂到 `GET /`（catch-all）上。
//
// 路由规则（保持极简，避免引入前端路由框架）：
//
//	/            → index.html（单页；内部用 location.hash 切视图）
//	/assets/...  → 嵌入式静态资源（bootstrap / app.js / app.css）
//	其它         → 404（纯 JSON；不把 HTML 当成接口响应回给调用方）
func Handler() http.Handler {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		// 只可能发生在 embed 路径写错的编译期场景；运行时直接 panic 比静默 404 好。
		panic("webui: embed assets 不可用: " + err.Error())
	}
	files := http.FileServer(http.FS(sub))
	// 入口页直接读字节发出去：http.FileServer 对 "/index.html" 有特殊处理
	// （301 重定向到 "./"），会让 "/" 变成 301，测试与浏览器都多一跳。
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		panic("webui: 缺少 assets/index.html: " + err.Error())
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")

		path := r.URL.Path
		switch {
		case path == "/" || path == "/index.html":
			// 页面每次回来校验（no-cache + ETag）：升级后立刻看到新界面，未变只回 304。
			etag := contentETag(index)
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("ETag", etag)
			if etagMatched(r.Header.Get("If-None-Match"), etag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(index)

		case strings.HasPrefix(path, "/assets/"):
			name := strings.TrimPrefix(path, "/assets/")
			if frameworkAssets[name] {
				// 框架基础包：内容随 vendored 版本固定，给短缓存、不回源校验。
				w.Header().Set("Cache-Control", "public, max-age=300")
			} else {
				// 其余资源：no-cache + 内容哈希 ETag，浏览器每次回来问一句。
				w.Header().Set("Cache-Control", "no-cache")
				if etag := assetETag(sub, name); etag != "" {
					w.Header().Set("ETag", etag)
					if etagMatched(r.Header.Get("If-None-Match"), etag) {
						w.WriteHeader(http.StatusNotModified)
						return
					}
				}
			}
			r = cloneWithPath(r, "/"+name)
			files.ServeHTTP(w, r)

		default:
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found","hint":"管理后台在 /，OpenAI 兼容接口在 /v1"}`))
		}
	})
}

// cloneWithPath 复制一份请求并改写 URL.Path（http.Request 不应被就地改写：
// 上游中间件可能还在读它）。
func cloneWithPath(r *http.Request, path string) *http.Request {
	cp := r.Clone(r.Context())
	u := *r.URL
	u.Path = path
	u.RawPath = ""
	cp.URL = &u
	return cp
}
