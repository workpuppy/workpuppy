// admin_qr_test.go /admin/qr 的契约测试。
//
// 覆盖：未鉴权必须 401（二维码里可能带授权链接，不能被随便拉取）、
// 鉴权后返回可用的 PNG、缺参/超长 400、size 参数被钳制。
package server

import (
	"bytes"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func qrURL(text string, extra string) string {
	u := "/admin/qr?text=" + url.QueryEscape(text)
	if extra != "" {
		u += "&" + extra
	}
	return u
}

// 没有管理凭据 → 401（与其它 /admin/* 一致；二维码本身就是敏感内容）。
func TestAdminQRRequiresAuth(t *testing.T) {
	h := newAdminHandler(t, nil, "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, qrURL("https://example.com/login?state=abc", ""), nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据 code=%d want 401", rec.Code)
	}
}

// 有凭据 → PNG，且能被标准库解出尺寸（证明不是空壳/坏图）。
func TestAdminQRRendersPNG(t *testing.T) {
	h := newAdminHandler(t, nil, "")
	text := "https://copilot.tencent.com/login?platform=CLI&state=a0ff26e4-03fd-4587-82cf-b90ba674cbcb"
	rec := adminReq(h, http.MethodGet, qrURL(text, "size=280"), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type=%q want image/png", ct)
	}
	// 二维码不缓存：换了 auth_url 必须立刻拿到新图。
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control=%q want no-store", cc)
	}
	body := rec.Body.Bytes()
	if !bytes.HasPrefix(body, []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatalf("不是 PNG（前 8 字节 % x）", body[:8])
	}
	img, err := png.Decode(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("PNG 解码失败: %v", err)
	}
	b := img.Bounds()
	if b.Dx() < 120 || b.Dy() < 120 {
		t.Fatalf("二维码尺寸 %dx%d 过小（应 ≥120）", b.Dx(), b.Dy())
	}
	if b.Dx() != b.Dy() {
		t.Errorf("二维码应为正方形，实际 %dx%d", b.Dx(), b.Dy())
	}
}

// 缺 text 或超长 → 400（不浪费 CPU，也不会被当成任意内容中转）。
func TestAdminQRRejectsBadInput(t *testing.T) {
	h := newAdminHandler(t, nil, "")
	if rec := adminReq(h, http.MethodGet, "/admin/qr", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("缺 text code=%d want 400", rec.Code)
	}
	if rec := adminReq(h, http.MethodGet, qrURL("   ", ""), nil); rec.Code != http.StatusBadRequest {
		t.Errorf("空白 text code=%d want 400", rec.Code)
	}
	long := string(bytes.Repeat([]byte("a"), qrMaxTextLen+1))
	if rec := adminReq(h, http.MethodGet, qrURL(long, ""), nil); rec.Code != http.StatusBadRequest {
		t.Errorf("超长 text code=%d want 400", rec.Code)
	}
}

// size 被钳制在 [120,720]：给小值也给得出一张能用的图（不会 0 像素）。
func TestAdminQRClampsSize(t *testing.T) {
	h := newAdminHandler(t, nil, "")
	// QR 的模块数固定，输出边长只能取"模块数 × 整数倍"，所以断言是：
	//   不超过钳制后的目标边长（120/720），且不小于目标的一半（不会退化成缩略图）。
	cases := []struct {
		raw   string
		limit int
	}{
		{"1", 120}, {"0", 120}, {"-5", 120}, {"99999", 720}, {"abc", 280},
	}
	for _, c := range cases {
		rec := adminReq(h, http.MethodGet, qrURL("https://example.com/x", "size="+c.raw), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("size=%s code=%d want 200", c.raw, rec.Code)
		}
		img, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
		if err != nil {
			t.Fatalf("size=%s PNG 解码失败: %v", c.raw, err)
		}
		n := img.Bounds().Dx()
		if n > c.limit {
			t.Errorf("size=%s 输出 %dpx 超过目标上限 %d", c.raw, n, c.limit)
		}
		if n < c.limit/2 {
			t.Errorf("size=%s 输出 %dpx 过小（下限 %d）", c.raw, n, c.limit/2)
		}
	}
}
