package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wp/internal/auth"
	"wp/internal/pool"
	"wp/internal/upstream"
)

// 余额定期刷新：循环启动后首轮就会把权威余额回写池（不必等一个完整 interval）。
func TestBalanceRefreshLoopUpdatesCredits(t *testing.T) {
	old := balanceRefreshFirstDelay
	balanceRefreshFirstDelay = 10 * time.Millisecond
	t.Cleanup(func() { balanceRefreshFirstDelay = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":{"Response":{"Data":{"Accounts":[` +
			`{"PackageName":"trial","CapacitySize":8642,"CapacityRemain":4321}]}}}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	t.Cleanup(p.Close)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}

	s := New(Config{Pool: p, Upstream: up, BalanceRefreshInterval: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st, ok := p.Status("u1"); ok && st.Credits == 4321 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st, ok := p.Status("u1"); !ok || st.Credits != 4321 {
		t.Fatalf("定期刷新未回写余额：%+v", st)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("scheduler 未在 ctx 取消后退出")
	}
	_ = fmt.Sprintf // keep fmt for future debugging helpers
}
