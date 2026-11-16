package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wp/internal/creditledger"
	"wp/internal/pool"
)

// 概览聚合接口：一次返回 status + credits，全部来自本地状态/缓存，且必须带管理密码。
func TestAdminOverview(t *testing.T) {
	h := newAdminHandler(t, nil, "")

	// 未带凭据 → 401（管理面只认管理密码）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据 code=%d want 401", rec.Code)
	}

	l := creditledger.New("", creditledger.DefaultRetain)
	l.SetClock(func() time.Time { return time.Date(2026, 11, 13, 10, 0, 0, 0, time.FixedZone("CST", 8*3600)) })
	l.RecordEarned("u1", "cn", 120)
	l.RecordSpent("u1", "cn", 30)
	h.cfg.Credits = l

	rec = adminReq(h, http.MethodGet, "/admin/overview", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		GeneratedAt string         `json:"generated_at"`
		Status      map[string]any `json:"status"`
		Credits     *struct {
			Today     string           `json:"today"`
			Totals    map[string]any   `json:"totals"`
			Histories []map[string]any `json:"histories"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.GeneratedAt == "" {
		t.Error("generated_at 为空")
	}
	if out.Status == nil || out.Status["accounts"] == nil {
		t.Errorf("status 缺字段：%+v", out.Status)
	}
	if out.Credits == nil {
		t.Fatal("credits 为空（已接线账本）")
	}
	if out.Credits.Today != "2026-11-13" {
		t.Errorf("today=%s", out.Credits.Today)
	}
	if earned, _ := out.Credits.Totals["earned"].(float64); earned != 120 {
		t.Errorf("totals.earned=%v want 120", out.Credits.Totals["earned"])
	}
	if len(out.Credits.Histories) == 0 {
		t.Error("histories 为空（应含今天）")
	}
}

// 未接线账本时 credits 为 null，但接口仍可用（前端只显示池状态）。
func TestAdminOverviewWithoutLedger(t *testing.T) {
	h := newAdminHandler(t, nil, "")
	rec := adminReq(h, http.MethodGet, "/admin/overview", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if v, ok := out["credits"]; !ok || v != nil {
		t.Fatalf("credits=%v want null", v)
	}
}

// 今日调整：当前总分 + 今日消耗 − 昨日日终 − 今日获得（按账号逐个求和）。
func TestDerivedTodayAdjust(t *testing.T) {
	snap := creditledger.Snapshot{
		Today: "2026-11-15",
		Histories: []creditledger.DaySummary{
			{Date: "2026-11-13", Accounts: []creditledger.AccountDayView{
				{UID: "u1", AccountDay: creditledger.AccountDay{Credits: 900}},
				{UID: "u2", AccountDay: creditledger.AccountDay{Credits: 500}},
			}},
			{Date: "2026-11-14", Accounts: []creditledger.AccountDayView{
				{UID: "u1", AccountDay: creditledger.AccountDay{Credits: 1000}},
				{UID: "u2", AccountDay: creditledger.AccountDay{Credits: 500}},
			}},
		},
		Accounts: map[string]*creditledger.AccountDay{
			"u1": {Earned: 100, Spent: 30},
		},
	}
	statuses := []pool.Status{
		{UID: "u1", Credits: 1050}, // 1050 + 30 − 1000 − 100 = -20
		{UID: "u2", Credits: 500},  // 500 + 0 − 500 − 0 = 0
		{UID: "u3", Credits: 777},  // 无历史基准：不参与
	}
	if got, ok := derivedTodayAdjust(snap, statuses); !ok || got != -20 {
		t.Fatalf("adjust=%d ok=%v want -20/true", got, ok)
	}

	// 当前余额高于预期 → 正数（界面用 ▲ 表示）。
	statuses[0].Credits = 1150
	if got, ok := derivedTodayAdjust(snap, statuses); !ok || got != 80 {
		t.Fatalf("adjust=%d ok=%v want 80/true", got, ok)
	}

	// 完全没有历史 → 无法计算。
	if _, ok := derivedTodayAdjust(creditledger.Snapshot{Today: "2026-11-15"}, statuses); ok {
		t.Fatal("没有历史基准时不应给出调整值")
	}
}
