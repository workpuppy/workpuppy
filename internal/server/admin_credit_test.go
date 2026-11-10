// admin_credit_test.go 「休养」开关与积分日账本端点的契约测试。
package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"wp/internal/auth"
	"wp/internal/creditledger"
)

// 休养：开启后不再被下游选中，但账号仍在池里（自动任务照跑）；解除后恢复。
func TestAdminRestToggle(t *testing.T) {
	h := newAdminHandler(t, []*auth.Auth{{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999}}, "")

	rec := adminReq(h, http.MethodPost, "/admin/accounts/u1/rest", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rest code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Resting bool `json:"resting"`
		Changed bool `json:"changed"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.Resting || !out.Changed {
		t.Fatalf("rest resp=%+v", out)
	}
	// 休养中：下游选不到
	if got := h.cfg.Pool.Pick("m"); got != nil {
		t.Fatalf("休养中不应被选中，实际 %s", got.UID)
	}
	// 但 /admin/accounts 里看得到，且 static 显示 resting=true
	list := adminReq(h, http.MethodGet, "/admin/accounts", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("accounts code=%d", list.Code)
	}

	// 解除
	rec = adminReq(h, http.MethodPost, "/admin/accounts/u1/unrest", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("unrest code=%d", rec.Code)
	}
	if resting, _ := h.cfg.Pool.RestingState("u1"); resting {
		t.Fatal("解除后应不再休养")
	}
	if got := h.cfg.Pool.Pick("m"); got == nil {
		t.Fatal("解除休养后应恢复可选")
	}

	// 未知账号 → 404
	if rec := adminReq(h, http.MethodPost, "/admin/accounts/nope/rest", nil); rec.Code != http.StatusNotFound {
		t.Errorf("未知账号 code=%d want 404", rec.Code)
	}
	// 非法 uid → 400
	if rec := adminReq(h, http.MethodPost, "/admin/accounts/..%2fetc/rest", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("非法 uid code=%d want 400", rec.Code)
	}
}

// 积分日账本：未接线 → 501；接线后返回当日热值 + 历史汇总。
func TestAdminCreditDaily(t *testing.T) {
	h := newAdminHandler(t, nil, "")
	// 未接线
	if rec := adminReq(h, http.MethodGet, "/admin/credit-daily", nil); rec.Code != http.StatusNotImplemented {
		t.Fatalf("未接线 code=%d want 501", rec.Code)
	}

	l := creditledger.New("", creditledger.DefaultRetain)
	l.SetClock(func() time.Time { return time.Date(2026, 11, 10, 12, 0, 0, 0, time.FixedZone("CST", 8*3600)) })
	l.RecordEarned("u1", "cn", 300)
	l.RecordSpent("u1", "cn", 80)
	h.cfg.Credits = l

	rec := adminReq(h, http.MethodGet, "/admin/credit-daily?days=7", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("credit-daily code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Today    string                                     `json:"today"`
		Totals   struct{ Earned, Spent, Adjust, Net int64 } `json:"totals"`
		Accounts map[string]struct {
			Realm  string `json:"realm"`
			Earned int64  `json:"earned"`
			Spent  int64  `json:"spent"`
		} `json:"accounts"`
		Histories []struct {
			Date string `json:"date"`
		} `json:"histories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Today != "2026-11-10" {
		t.Errorf("today=%s", out.Today)
	}
	if out.Totals.Earned != 300 || out.Totals.Spent != 80 || out.Totals.Net != 220 {
		t.Errorf("totals=%+v", out.Totals)
	}
	if a, ok := out.Accounts["u1"]; !ok || a.Earned != 300 || a.Spent != 80 || a.Realm != "cn" {
		t.Errorf("accounts=%+v", out.Accounts)
	}
	// 历史含今天
	if len(out.Histories) != 1 || out.Histories[0].Date != "2026-11-10" {
		t.Errorf("histories=%+v", out.Histories)
	}
}
