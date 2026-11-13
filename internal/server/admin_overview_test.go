package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wp/internal/creditledger"
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
