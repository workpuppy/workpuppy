// admin_qr.go 管理后台的二维码渲染端点。
//
// 为什么放在服务端而不是前端引一个 JS 库：
//   - 管理后台的 CSP 是 `default-src 'self'`，不允许外链脚本，前端要画二维码就得
//     vendored 一整个编码库进仓库（又多一份要维护的第三方代码）；
//   - 服务端只用 40 行 + 一个纯 Go 的 MIT 库（rsc.io/qr，无传递依赖），
//     二维码永远能画出来，不依赖浏览器 canvas / 字体 / 缩放。
//
// 用途：扫码加号（/admin/auth/start 返回的 auth_url）渲成二维码给手机扫。
// 也可以拿来渲任意短文本（排障时贴一个 URL 试试）。
package server

import (
	"net/http"
	"strconv"
	"strings"

	"rsc.io/qr"
)

// qrMaxTextLen 二维码承载的文本上限（auth_url 通常 ~200 字节；留足余量）。
// 超过直接拒绝：既不值得编码，也能避免有人拿它当压缩服务刷 CPU。
const qrMaxTextLen = 1024

// adminQR GET /admin/qr?text=<url>&size=<px> —— 把文本渲成 PNG 二维码。
//
// 需要管理凭据（与其它 /admin/* 一致）：浏览器侧用 fetch 带 Authorization 取 blob，
// 再挂到 <img> 上——`<img src>` 没法带请求头，所以不能直接把本端点写进 src。
// size 默认 280px，钳在 [120, 720]；输出 PNG（无缓存，避免换了 auth_url 还扫到旧的）。
func (h *Handler) adminQR(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	text := strings.TrimSpace(q.Get("text"))
	if text == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少 text 参数（要编码的文本/URL）")
		return
	}
	if len(text) > qrMaxTextLen {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"text 过长（"+strconv.Itoa(len(text))+" 字节 > 上限 "+strconv.Itoa(qrMaxTextLen)+"）")
		return
	}

	size := 280
	if v := strings.TrimSpace(q.Get("size")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			size = n
		}
	}
	if size < 120 {
		size = 120
	}
	if size > 720 {
		size = 720
	}

	// M 级容错：手机对着屏幕扫，反光/摩尔纹常见，L 级偶尔扫不出来。
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "无法编码为二维码："+err.Error())
		return
	}
	// 每个 QR 模块放大成多少像素：按目标边长反推（整数倍，保证模块边缘不糊）。
	// rsc.io/qr 的位图自带 4 模块静区，实际边长 = (Size+8)*Scale，所以这里按 Size+8 反推，
	// 保证输出边长恒 <= size（扫码用的静区由库提供，前端 padding 只是视觉留白）。
	scale := size / (code.Size + 8)
	if scale < 1 {
		scale = 1
	}
	code.Scale = scale

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(code.PNG())
}
