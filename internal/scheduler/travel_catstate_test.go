package scheduler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 巡检结果要写进池的 CatState——账号列表「状态」列里的猫猫标签就读这份快照，
// 页面打开时不再为每行额外打上游。
func TestRunTravelRecordsCatState(t *testing.T) {
	cases := []struct {
		name       string
		buddy      string
		state      string
		wantState  string
		wantBuddy  string
		wantLimit  bool
		wantAwait  int64
		wantDetail string
	}{
		{
			name: "旅行中", buddy: `{"id":7,"name":"档案喵"}`,
			state:     `{"state":"traveling","record_id":9}`,
			wantState: "traveling", wantBuddy: "档案喵",
		},
		{
			name: "到站领奖成功后回到空闲", buddy: `{"id":7,"name":"档案喵"}`,
			state: `{"state":"arrived","record_id":9,"reward_credit":20}`,
			// 领奖成功（stub 返回 reward_credit=9）后猫回到空闲，reward 清零，详情记到账数。
			wantState: "idle", wantBuddy: "档案喵", wantAwait: 0, wantDetail: "已领奖 +9",
		},
		{
			name: "空闲且今日已派出", buddy: `{"id":7,"name":"档案喵"}`,
			state:     `{"state":"idle","daily_limit_reached":true}`,
			wantState: "idle", wantBuddy: "档案喵", wantLimit: true, wantDetail: "今日已派出",
		},
		{
			name: "空闲未达上限-派出后旅行中", buddy: `{"id":7,"name":"档案喵"}`,
			state:     `{"state":"idle","daily_limit_reached":false}`,
			wantState: "traveling", wantBuddy: "档案喵", wantLimit: true, wantDetail: "已派出",
		},
		{
			name: "未知状态归到查询失败", buddy: `{"id":7,"name":"档案喵"}`,
			state:     `{"state":"teleporting"}`,
			wantState: "error", wantBuddy: "档案喵", wantDetail: "未知状态 teleporting",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fastTravel(t)
			stub := &travelStub{buddy: c.buddy, state: c.state}
			srv := stub.server()
			defer srv.Close()

			s, p := newTravelScheduler(t, srv, "u1")
			s.RunTravelNow()

			cat, ok := p.CatOf("u1")
			if !ok || cat == nil {
				t.Fatal("巡检后应记录猫猫状态")
			}
			if cat.State != c.wantState || cat.Buddy != c.wantBuddy || cat.DailyLimitReached != c.wantLimit || cat.Reward != c.wantAwait {
				t.Fatalf("cat=%+v want state=%s buddy=%s limit=%v reward=%d",
					cat, c.wantState, c.wantBuddy, c.wantLimit, c.wantAwait)
			}
			if c.wantDetail != "" && !strings.Contains(cat.Detail, c.wantDetail) {
				t.Errorf("cat.Detail=%q want 含 %q", cat.Detail, c.wantDetail)
			}
			if cat.UpdatedAt.IsZero() {
				t.Error("UpdatedAt 应被填上（账号页显示「更新于…」）")
			}
		})
	}
}

// 无猫（还没领养）也要留痕：账号页显示「未领猫」而不是空白。
func TestRunTravelRecordsNoBuddy(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: "null"}
	srv := stub.server()
	defer srv.Close()

	s, p := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	cat, ok := p.CatOf("u1")
	if !ok || cat == nil || cat.State != "none" {
		t.Fatalf("无猫时应记 none: %+v ok=%v", cat, ok)
	}
}

// 上游查询失败记为 error（不阻断巡检，其它账号照跑）。
func TestRunTravelRecordsErrorState(t *testing.T) {
	fastTravel(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	s, p := newTravelScheduler(t, srv, "u1")
	s.RunTravelNow()

	cat, ok := p.CatOf("u1")
	if !ok || cat == nil || cat.State != "error" {
		t.Fatalf("查询失败应记 error: %+v ok=%v", cat, ok)
	}
	if cat.Detail == "" {
		t.Error("error 状态应带 detail（排障用）")
	}
}
