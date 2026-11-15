package balancerefresh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"wp/internal/auth"
	"wp/internal/pool"
	"wp/internal/upstream"
)

func stubBilling(t *testing.T, remain int64) *upstream.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		inner := `{"Response":{"Data":{"Accounts":[{"PackageName":"trial","CapacitySize":` +
			itoa(remain*2) + `,"CapacityRemain":` + itoa(remain) + `}]}}}`
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok","data":` + inner + `}`))
	}))
	t.Cleanup(srv.Close)
	return &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// 刷新余额：有 refreshToken 的号回写余额；无凭据/禁用的号跳过。
func TestRefreshUpdatesCredits(t *testing.T) {
	p := pool.New("")
	t.Cleanup(p.Close)
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at"}) // 无 refreshToken
	p.Add(&auth.Auth{UID: "u3", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Disable("u3", "manual")

	res, err := Refresh(context.Background(), p, stubBilling(t, 4321))
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.OK != 1 || res.Skipped != 2 || res.Fail != 0 || res.Total != 3 {
		t.Fatalf("汇总 = %+v", res)
	}
	st, ok := p.Status("u1")
	if !ok || st.Credits != 4321 {
		t.Fatalf("u1 credits = %+v, want 4321", st)
	}
}

// 空池也能安全返回（后台定时任务首次 tick 时常见）。
func TestRefreshEmptyPool(t *testing.T) {
	p := pool.New("")
	t.Cleanup(p.Close)
	res, err := Refresh(context.Background(), p, stubBilling(t, 1))
	if err != nil || res.Total != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// ctx 取消后在账号边界退出（不报错，返回已完成的部分）。
func TestRefreshStopsOnContextCancel(t *testing.T) {
	p := pool.New("")
	t.Cleanup(p.Close)
	for _, uid := range []string{"a", "b", "c"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 一进循环就退出
	res, err := Refresh(ctx, p, stubBilling(t, 10))
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.Total != 0 {
		t.Fatalf("取消后不应继续刷新：%+v", res)
	}
}
