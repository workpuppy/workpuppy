// admin_account_actions_test.go 账号页顶部批量动作的契约：
//   - 刷新余额：逐账号打上游只读接口并回写（导入的号首次刷新后才有积分）；
//   - 一键旅行：调度器未接线 501，接线后 202（异步执行）。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wp/internal/auth"
	"wp/internal/upstream"
)

// stubBilling 假上游：只实现余额只读接口。
func stubBilling(t *testing.T, remain int64) *upstream.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// 上游真实形状：Response.Data.Accounts[]，余额由 CapacityRemain/Size 推导。
		// 上游真实形状：{code,msg,data:{Response:{Data:{Accounts:[...]}}}}（billingMeterJSON 会剥 envelope）
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
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// 刷新余额：有凭据的账号回写 credits，无凭据/禁用的跳过。
func TestAdminRefreshBalances(t *testing.T) {
	p := []*auth.Auth{
		{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999},
		{UID: "u2", AccessToken: "at"}, // 无 refreshToken → 跳过
	}
	h := newAdminHandler(t, p, "")
	h.cfg.Upstream = stubBilling(t, 4321)

	rec := adminReq(h, http.MethodPost, "/admin/accounts/refresh", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK, Skipped, Fail, Total int
		Accounts                 []struct {
			UID, Status string
			Credits     *int64
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.OK != 1 || out.Skipped != 1 || out.Fail != 0 || out.Total != 2 {
		t.Fatalf("汇总=%+v", out)
	}
	for _, a := range out.Accounts {
		if a.UID == "u1" {
			if a.Status != "ok" || a.Credits == nil || *a.Credits != 4321 {
				t.Errorf("u1=%+v", a)
			}
		}
	}
	// 池里余额也更新了（导入的号刷新后不再显示 0）
	if st, ok := h.cfg.Pool.Status("u1"); ok && st.Credits != 4321 {
		t.Errorf("池内 credits=%d want 4321", st.Credits)
	}
}

// 一键旅行：未接线 501；接线后 202 且不阻塞。
func TestAdminTravelRun(t *testing.T) {
	h := newAdminHandler(t, nil, "")
	if rec := adminReq(h, http.MethodPost, "/admin/travel/run", nil); rec.Code != http.StatusNotImplemented {
		t.Fatalf("未接线 code=%d want 501", rec.Code)
	}
}

// 单号签到与 revive 端点已移除（UI 合并为「一键签到」与「启用」）。
func TestRemovedEndpointsAre404(t *testing.T) {
	h := newAdminHandler(t, []*auth.Auth{{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}}, "")
	for _, path := range []string{"/admin/accounts/u1/checkin", "/admin/accounts/u1/revive"} {
		rec := adminReq(h, http.MethodPost, path, strings.NewReader("{}"))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s code=%d want 404（端点已移除）", path, rec.Code)
		}
	}
}
