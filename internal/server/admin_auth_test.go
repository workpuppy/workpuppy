// admin_auth_test.go /admin/auth/start 与 /admin/auth/poll 的"有效期"契约。
//
// 覆盖两件管理后台依赖的事：
//  1. start 响应带 expires_in / expires_at，前端才能显示倒计时；
//  2. state 过期后 poll 返回 410（而不是笼统的 400），前端据此把二维码置为"已失效"。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wp/internal/loginsvc"
)

// stubLoginServer 假上游：state 申请固定成功，token 永远 pending。
func stubLoginServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v2/plugin/auth/state":
			_, _ = w.Write([]byte(`{"code":0,"data":{"state":"st-1","authUrl":"https://example.com/login?state=st-1"}}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":40001,"msg":"pending"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newAuthTestHandler(t *testing.T, ttl time.Duration) (*Handler, *loginsvc.Manager) {
	t.Helper()
	srv := stubLoginServer(t)
	m := loginsvc.New()
	m.SetBases(srv.URL, srv.URL)
	if ttl > 0 {
		m.SetTTL(ttl)
	}
	h := newAdminHandler(t, nil, "")
	h.cfg.Login = m
	return h, m
}

// start 响应必须带有效期（前端倒计时 + 到点置灰全靠它）。
func TestAdminAuthStartReturnsTTL(t *testing.T) {
	h, _ := newAuthTestHandler(t, 15*time.Minute)

	rec := adminReq(h, http.MethodPost, "/admin/auth/start", strings.NewReader(`{"realm":"cn"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		State     string `json:"state"`
		AuthURL   string `json:"auth_url"`
		ExpiresIn int    `json:"expires_in"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.State != "st-1" || out.AuthURL == "" {
		t.Fatalf("state/auth_url 异常: %+v", out)
	}
	if out.ExpiresIn != 900 {
		t.Errorf("expires_in=%d want 900（15 分钟）", out.ExpiresIn)
	}
	// expires_at 是绝对时刻，应该落在「现在 + 15 分钟」附近
	if d := time.Until(time.Unix(out.ExpiresAt, 0)); d < 14*time.Minute || d > 16*time.Minute {
		t.Errorf("expires_at 偏差过大：还剩 %v", d)
	}
}

// state 过期后 poll 回 410 + auth_state_expired（前端凭这个切"已失效"）。
func TestAdminAuthPollExpiredReturns410(t *testing.T) {
	h, m := newAuthTestHandler(t, 20*time.Millisecond)

	base := time.Now()
	m.SetClock(func() time.Time { return base })
	rec := adminReq(h, http.MethodPost, "/admin/auth/start", strings.NewReader(`{"realm":"cn"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("start code=%d", rec.Code)
	}
	var start struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &start)

	// 越过 TTL
	m.SetClock(func() time.Time { return base.Add(time.Second) })
	poll := adminReq(h, http.MethodGet, "/admin/auth/poll?state="+start.State+"&realm=cn", nil)
	if poll.Code != http.StatusGone {
		t.Fatalf("过期 poll code=%d want 410 body=%s", poll.Code, poll.Body.String())
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(poll.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errBody.Error.Code != "auth_state_expired" {
		t.Errorf("error.code=%q want auth_state_expired", errBody.Error.Code)
	}
}
