package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wp/internal/adminauth"
	"wp/internal/auth"
)

func TestSuccessfulProbeClearsOnlyMissingModel(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	// 先禁用再看模型级冷却：禁用会清冷却域（比冷却更强的不可用终态），
	// 所以这里按「禁用后再出现新冷却」的顺序构造，锚点仍是「探测不得清真实配额与运维状态」。
	p.SetDisabled("u1", true, "operator")
	p.BlockModelBackoff("u1", "deepseek-v4.1-flash", "11102 model not available")
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "glm-5.3", "6004 model rate limit")
	h := NewHandler(Config{Pool: p, APIKey: "test", AdminAuth: adminauth.MustNew(testAdminPW), AdminEnabled: true})
	url := "/admin/accounts/u1/models/deepseek-v4.1-flash/available"
	request := httptest.NewRequest("POST", url, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, request)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("recovery requires gateway key: %d", rec.Code)
	}
	request = httptest.NewRequest("POST", url, nil)
	request.Header.Set("Authorization", "Bearer "+testAdminPW)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, request)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	st, _ := p.Status("u1")
	if !st.Disabled || len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("must preserve real quota and operator state: %+v", st)
	}
}

func TestProbeRecoveryDoesNotClearSameModelQuota(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(time.Hour), "deepseek-v4.1-flash", "6004 model rate limit")
	h := NewHandler(Config{Pool: p, AdminAuth: adminauth.MustNew(testAdminPW), AdminEnabled: true})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/accounts/u1/models/deepseek-v4.1-flash/available", nil)
	// 管理端点只认管理密码（"api_key 为空 = 不鉴权" 已不再适用于管理面）。
	req.Header.Set("Authorization", "Bearer "+testAdminPW)
	h.ServeHTTP(rec, req)
	st, _ := p.Status("u1")
	if rec.Code != http.StatusOK || len(st.RateLimitedModels) != 1 {
		t.Fatalf("successful small probe must not erase token quota: %d %+v", rec.Code, st)
	}
}
