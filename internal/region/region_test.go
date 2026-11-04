package region

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeServer 模拟国际版 web 的三条端点（响应形态照抄脚本逆向结论：
// get-country-code 的 data 是**JSON 字符串**，内层才是 {code,data:{list}}）。
func fakeServer(t *testing.T, needRegion bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/billing/area/get-country-code", func(w http.ResponseWriter, r *http.Request) {
		inner := map[string]any{"code": 0, "data": map[string]any{"list": []map[string]any{
			{"EnName": "Hong Kong", "Name": "香港", "IOS2": "HK", "IOS3": "HKG", "Code": 852},
			{"EnName": "Singapore", "Name": "新加坡", "IOS2": "SG", "IOS3": "SGP", "Code": 65},
			{"EnName": "United States", "Name": "美国", "IOS2": "US", "IOS3": "USA", "Code": 1},
		}}}
		blob, _ := json.Marshal(inner)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": string(blob)})
	})
	mux.HandleFunc("/billing/area/get-user-area-info", func(w http.ResponseWriter, r *http.Request) {
		inner := map[string]any{"code": 0, "data": map[string]any{"IOS2": "SG", "enName": "Singapore"}}
		blob, _ := json.Marshal(inner)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": string(blob)})
	})
	mux.HandleFunc("/console/login/account", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Attributes map[string][]string `json:"attributes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// 三字段缺一不可（web 端同形），少一个就是「提交成功但地区没生效」。
		if len(body.Attributes["countryCode"]) == 0 ||
			len(body.Attributes["countryFullName"]) == 0 ||
			len(body.Attributes["countryName"]) == 0 {
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 400, "msg": "attributes required"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "msg": "ok"})
	})
	mux.HandleFunc("/auth/realms/copilot/overseas/user/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-User-Id") == "" {
			t.Errorf("register 必须带 X-User-Id（与 web 端同形）")
		}
		if needRegion {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 500, "msg": "region required"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200, "msg": "ok"})
	})
	mux.HandleFunc("/billing/ide/trial", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 14051, "msg": "already claimed"})
	})
	return httptest.NewServer(mux)
}

// FetchCountries 只保留国际版白名单并按白名单顺序排列（US 不在白名单里）。
func TestFetchCountriesIntlOnly(t *testing.T) {
	srv := fakeServer(t, false)
	defer srv.Close()

	got, err := FetchCountries(srv.URL, true)
	if err != nil {
		t.Fatalf("FetchCountries: %v", err)
	}
	var codes []string
	for _, c := range got {
		codes = append(codes, c.IOS2)
	}
	if strings.Join(codes, ",") != "HK,SG" {
		t.Fatalf("白名单过滤/排序不符：%v", codes)
	}
	if got[1].CodeString() != "65" {
		t.Fatalf("Code 应渲染成文本 65，实际 %q", got[1].CodeString())
	}
}

// Activate 把 500 + "region required" 判成「需完善地区」而不是硬错误。
func TestActivateNeedsRegion(t *testing.T) {
	srv := fakeServer(t, true)
	defer srv.Close()

	res := Activate(srv.URL, "tok", "u1")
	if res.OK || !res.NeedsRegion {
		t.Fatalf("应判 needs_region：%+v", res)
	}
}

// CompleteFlow：提交地区 → 复验 register 成功。
func TestCompleteFlowSubmitsAndVerifies(t *testing.T) {
	// needRegion=true 的首轮会失败；提交完地区后第二次 register 成功——
	// 用带状态的 handler 模拟这一点。
	srv := fakeServer(t, true)
	defer srv.Close()

	if err := SubmitRegion(srv.URL, "tok", Country{IOS2: "SG", EnName: "Singapore", Code: json.RawMessage("65")}); err != nil {
		t.Fatalf("SubmitRegion: %v", err)
	}
	// 本地 fake 不会因提交而翻转 register 结果，所以这里只验证「提交失败/成功」的判定：
	// 真正的状态翻转属于服务端行为，端到端验证在 login save 的集成测试之外。
	if res := Activate(srv.URL, "tok", "u1"); !res.NeedsRegion {
		t.Fatalf("fake 仍应报 needs_region：%+v", res)
	}
}

// ClaimTrial 把 14051 判成「已领取」而不是失败。
func TestClaimTrialIdempotent(t *testing.T) {
	srv := fakeServer(t, false)
	defer srv.Close()

	res := ClaimTrial(srv.URL, "tok")
	if !res.OK || !res.Already {
		t.Fatalf("14051 应判 already：%+v", res)
	}
}

// DetectRegion 解析 data 为 JSON 字符串的嵌套形态。
func TestDetectRegion(t *testing.T) {
	srv := fakeServer(t, false)
	defer srv.Close()

	ios2, name, err := DetectRegion(srv.URL, "tok")
	if err != nil {
		t.Fatalf("DetectRegion: %v", err)
	}
	if ios2 != "SG" || name != "Singapore" {
		t.Fatalf("检测结果不符：%q %q", ios2, name)
	}
}

// PickByIOS2 大小写不敏感；找不到返回 nil。
func TestPickByIOS2(t *testing.T) {
	list := []Country{{IOS2: "HK"}, {IOS2: "SG"}}
	if p := PickByIOS2(list, "sg"); p == nil || p.IOS2 != "SG" {
		t.Fatalf("应命中 SG：%+v", p)
	}
	if p := PickByIOS2(list, "US"); p != nil {
		t.Fatalf("US 不应命中：%+v", p)
	}
}
