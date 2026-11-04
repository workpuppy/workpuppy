package loginsvc

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeUpstream 模拟三条登录端点。tokenStatus 控制 token 端点回什么（0 = 成功）。
func fakeUpstream(t *testing.T, pending bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/state"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"state":"s-1","authUrl":"https://qr.test/s-1"}}`))
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/auth/token"):
			if pending {
				_, _ = w.Write([]byte(`{"code":10001,"msg":"login ing"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"AT","refreshToken":"RT","expiresIn":1800,"domain":"copilot.tencent.com"}}`))
		case strings.HasPrefix(r.URL.Path, "/v2/plugin/login/account"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"uid":"42","enterpriseId":"e","nickname":"号"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func newTestManager(t *testing.T, pending bool) (*Manager, func()) {
	t.Helper()
	srv := fakeUpstream(t, pending)
	m := New()
	m.SetBases(srv.URL, srv.URL)
	return m, srv.Close
}

// 完整链路：start 拿到 state 与 URL；poll 成功时返回结构化凭据并推算过期时间。
func TestStartAndPollSuccess(t *testing.T) {
	m, stop := newTestManager(t, false)
	defer stop()

	state, authURL, err := m.Start("cn")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if state != "s-1" || authURL != "https://qr.test/s-1" {
		t.Fatalf("start 返回不符：state=%q url=%q", state, authURL)
	}

	cred, pending, err := m.Poll("cn", state)
	if err != nil || pending {
		t.Fatalf("poll: err=%v pending=%v", err, pending)
	}
	if cred.UID != "42" || cred.AccessToken != "AT" || cred.RefreshToken != "RT" {
		t.Fatalf("凭据不符：%+v", cred)
	}
	if cred.Realm != "cn" {
		t.Fatalf("realm 应归一为 cn，实际 %q", cred.Realm)
	}
	// expiresIn 必须换算成绝对值：写 0 会让 token 一出生就被判过期。
	if got := cred.ExpiresAt(time.Unix(1000, 0)); got != 1000+1800 {
		t.Fatalf("ExpiresAt 换算错误：%d", got)
	}
	// 一次性：拿到凭据后 state 立即作废。
	if _, _, err := m.Poll("cn", state); err == nil {
		t.Fatal("state 应一次性消费，重放必须报错")
	}
}

// 还没扫码时是 pending（不是错误）：调用方据此继续轮询。
func TestPollPending(t *testing.T) {
	m, stop := newTestManager(t, true)
	defer stop()

	state, _, err := m.Start("cn")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	cred, pending, err := m.Poll("cn", state)
	if err != nil || !pending || cred != nil {
		t.Fatalf("应 pending：cred=%v pending=%v err=%v", cred, pending, err)
	}
}

// 防混域：cn 的 state 拿去 global 轮询必须明确报错，而不是打到一个错误的上游。
func TestPollRealmMismatch(t *testing.T) {
	m, stop := newTestManager(t, false)
	defer stop()

	state, _, err := m.Start("cn")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, _, err := m.Poll("global", state); err == nil ||
		!strings.Contains(err.Error(), "realm") {
		t.Fatalf("realm 不一致应报错，实际 %v", err)
	}
}

// state 过期要如实说「已过期」并释放名额（否则扫了不确认的人会一直占坑）。
func TestPollExpired(t *testing.T) {
	m, stop := newTestManager(t, false)
	defer stop()
	m.SetTTL(50 * time.Millisecond)

	base := time.Now()
	m.SetClock(func() time.Time { return base })
	state, _, err := m.Start("cn")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	m.SetClock(func() time.Time { return base.Add(time.Second) })

	if _, _, err := m.Poll("cn", state); err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("过期应明确报错，实际 %v", err)
	}
	if n := m.PendingCount(); n != 0 {
		t.Fatalf("过期 state 应被清理，剩余 %d", n)
	}
}

// 未知 state 不能静默 pending（会让调用方无限轮询）。
func TestPollUnknownState(t *testing.T) {
	m := New()
	if _, _, err := m.Poll("cn", "nope"); err == nil {
		t.Fatal("未知 state 必须报错")
	}
	if _, _, err := m.Poll("cn", "  "); err == nil {
		t.Fatal("空 state 必须报错")
	}
}
