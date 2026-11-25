package scheduler

// startup_test.go 启动补跑（startup catch-up）：容器升级/重建把当天时点整段吞掉后，
// 重启要按本地状态把"今天已过点但没做完"的签到 / 旅行补上（见 startup.go）。
//
// 所有用例都固定墙钟（nowFunc）并显式给排程时区，不依赖跑测试的机器时区与当前时间。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wp/internal/auth"
	"wp/internal/pool"
	"wp/internal/upstream"
)

// fixedNow 把墙钟固定到 tt（测试后恢复）。
func fixedNow(t *testing.T, tt time.Time) {
	t.Helper()
	old := nowFunc
	nowFunc = func() time.Time { return tt }
	t.Cleanup(func() { nowFunc = old })
}

// TestPassedSlot 只认"今天已到点"的时点：
//   - 今天还没到点（08:00 之于 09:00）→ 不补跑（不抢跑）；
//   - 整点已到 → 算到点；
//   - 跨到次日凌晨 → 昨天的时点不算今天的（不回溯）。
func TestPassedSlot(t *testing.T) {
	at := []int{9 * 60, 21 * 60}
	cases := []struct {
		name string
		now  time.Time
		min  int
		ok   bool
	}{
		{"07:59 还没到第一个时点", time.Date(2026, 9, 11, 7, 59, 0, 0, cstZone), 0, false},
		{"09:00 整点即算到点", time.Date(2026, 9, 11, 9, 0, 0, 0, cstZone), 540, true},
		{"09:05 第一个时点已过", time.Date(2026, 9, 11, 9, 5, 0, 0, cstZone), 540, true},
		{"20:00 只算第一个", time.Date(2026, 9, 11, 20, 0, 0, 0, cstZone), 540, true},
		{"21:30 取最晚的已过时点", time.Date(2026, 9, 11, 21, 30, 0, 0, cstZone), 1260, true},
		{"次日 00:10 不认昨天的时点", time.Date(2026, 9, 12, 0, 10, 0, 0, cstZone), 0, false},
	}
	for _, c := range cases {
		min, ok := passedSlot(c.now, at, cstZone)
		if ok != c.ok || min != c.min {
			t.Errorf("%s: passedSlot=%d,%v want %d,%v", c.name, min, ok, c.min, c.ok)
		}
	}
}

// TestTravelMaybePending 旅行进度判定：只有"今天已派出且已领完奖"能证明今日收工。
func TestTravelMaybePending(t *testing.T) {
	today := "2026-09-11"
	today9 := time.Date(2026, 9, 11, 9, 0, 0, 0, cstZone)
	yesterday21 := time.Date(2026, 9, 10, 21, 0, 0, 0, cstZone)
	cases := []struct {
		name string
		cat  *pool.CatState
		ok   bool
		want bool
	}{
		{"没有快照 → 不知道进度", nil, false, true},
		{"今天 idle 已派出 → 今日收工", &pool.CatState{State: travelStateIdle, DailyLimitReached: true, UpdatedAt: today9}, true, false},
		{"今天 idle 未派出 → 待派出", &pool.CatState{State: travelStateIdle, UpdatedAt: today9}, true, true},
		{"今天 arrived → 有奖没领", &pool.CatState{State: travelStateArrived, UpdatedAt: today9}, true, true},
		{"今天 traveling → 可能已到站", &pool.CatState{State: travelStateTraveling, UpdatedAt: today9}, true, true},
		{"昨天 idle 已派出 → 快照过期要重查", &pool.CatState{State: travelStateIdle, DailyLimitReached: true, UpdatedAt: yesterday21}, true, true},
		{"今天 error → 重试", &pool.CatState{State: "error", UpdatedAt: today9}, true, true},
		{"今天 none → 走领养", &pool.CatState{State: "none", UpdatedAt: today9}, true, true},
	}
	for _, c := range cases {
		if got := travelMaybePending(c.cat, c.ok, today); got != c.want {
			t.Errorf("%s: travelMaybePending=%v want %v", c.name, got, c.want)
		}
	}
}

// newCheckinCatchUpScheduler 只启用签到（旅行显式关掉，避免干扰签到断言）。
func newCheckinCatchUpScheduler(t *testing.T, srv *httptest.Server, now time.Time, uids ...string) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{
		Pool: p, Upstream: up, Location: cstZone,
		CheckinAt: []int{9 * 60, 21 * 60}, TravelDisabled: true,
	}), p
}

// TestStartupCatchUpSkipsBeforeFirstSlot 08:00 启动、签到配在 09:00：今天还没到点，
// 什么都不该做（否则等于把签到提前到用户没配置的时间）。
func TestStartupCatchUpSkipsBeforeFirstSlot(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	now := time.Date(2026, 9, 11, 8, 0, 0, 0, cstZone)
	fixedNow(t, now)
	s, _ := newCheckinCatchUpScheduler(t, srv, now, "u1")

	s.catchUpAt(context.Background(), now)

	if n := f.checkinCalls.Load(); n != 0 {
		t.Errorf("时点未到却签到了 %d 次", n)
	}
}

// TestStartupCatchUpCheckinWhenMissed 09:00 的时点被重启吞掉（09:05 才起来）→ 立刻补签。
func TestStartupCatchUpCheckinWhenMissed(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	now := time.Date(2026, 9, 11, 9, 5, 0, 0, cstZone)
	fixedNow(t, now)
	s, _ := newCheckinCatchUpScheduler(t, srv, now, "u1", "u2")

	s.catchUpAt(context.Background(), now)

	if n := f.checkinCalls.Load(); n != 2 {
		t.Errorf("checkin calls=%d want 2（两个号今天都没签）", n)
	}
}

// TestStartupCatchUpCheckinSkipsWhenDone 今天已经签到成功的账号不重复打上游。
func TestStartupCatchUpCheckinSkipsWhenDone(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	now := time.Date(2026, 9, 11, 9, 5, 0, 0, cstZone)
	fixedNow(t, now)
	s, p := newCheckinCatchUpScheduler(t, srv, now, "u1")
	p.MarkCheckin("u1", "ok", now) // 今天已签到

	s.catchUpAt(context.Background(), now)

	if n := f.checkinCalls.Load(); n != 0 {
		t.Errorf("今天已签到却补跑了 %d 次", n)
	}
}

// newTravelCatchUpScheduler 只启用旅行（签到显式关掉），依赖本地猫快照决定补不补。
func newTravelCatchUpScheduler(t *testing.T, srv *httptest.Server, uids ...string) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{
		Pool: p, Upstream: up, Location: cstZone,
		TravelAt: []int{9 * 60, 21 * 60}, CheckinDisabled: true,
	}), p
}

// TestStartupCatchUpTravelClaimsWhenArrived 快照显示"已到站可领奖"→ 补跑并真的领奖。
func TestStartupCatchUpTravelClaimsWhenArrived(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{
		buddy: `{"id":1,"name":"档案喵"}`,
		state: `{"state":"arrived","record_id":7,"reward_credit":50}`,
	}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	now := time.Date(2026, 9, 11, 21, 5, 0, 0, cstZone)
	fixedNow(t, now)
	s, p := newTravelCatchUpScheduler(t, srv, "u1")
	p.SetCat("u1", pool.CatState{State: travelStateArrived, Reward: 50, UpdatedAt: now})

	s.catchUpAt(context.Background(), now)

	if n := stub.statusCalls.Load(); n != 1 {
		t.Errorf("travel/status calls=%d want 1", n)
	}
	if n := stub.claimCalls.Load(); n != 1 {
		t.Errorf("travel/claim calls=%d want 1（到站必须补领）", n)
	}
	if got := stub.record.Load(); got != 7 {
		t.Errorf("claim record_id=%d want 7", got)
	}
}

// TestStartupCatchUpTravelSkipsWhenDoneToday 快照证明"今天已派出且已领完"→ 一个上游请求都不发。
func TestStartupCatchUpTravelSkipsWhenDoneToday(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":1,"name":"档案喵"}`, state: `{"state":"idle","daily_limit_reached":true}`}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	now := time.Date(2026, 9, 11, 21, 5, 0, 0, cstZone)
	fixedNow(t, now)
	s, p := newTravelCatchUpScheduler(t, srv, "u1")
	p.SetCat("u1", pool.CatState{State: travelStateIdle, DailyLimitReached: true, UpdatedAt: now})

	s.catchUpAt(context.Background(), now)

	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("今日已收工仍查了 buddy/info %d 次", n)
	}
	if n := stub.statusCalls.Load(); n != 0 {
		t.Errorf("今日已收工仍查了 travel/status %d 次", n)
	}
}

// TestStartupCatchUpTravelRechecksStaleSnapshot 快照是昨天的（daily_limit_reached 已按
// 自然日重置，不代表今天）→ 必须重查，但不能误派出（上游今日已达上限）。
func TestStartupCatchUpTravelRechecksStaleSnapshot(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":1,"name":"档案喵"}`, state: `{"state":"idle","daily_limit_reached":true}`}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	now := time.Date(2026, 9, 11, 9, 5, 0, 0, cstZone)
	fixedNow(t, now)
	s, p := newTravelCatchUpScheduler(t, srv, "u1")
	p.SetCat("u1", pool.CatState{State: travelStateIdle, DailyLimitReached: true, UpdatedAt: now.AddDate(0, 0, -1)})

	s.catchUpAt(context.Background(), now)

	if n := stub.statusCalls.Load(); n != 1 {
		t.Errorf("travel/status calls=%d want 1（隔天快照要重查）", n)
	}
	if n := stub.departCalls.Load(); n != 0 {
		t.Errorf("上游今日已达派出上限，却 depart 了 %d 次", n)
	}
}

// TestStartupCatchUpTravelSkipsWhenBusy 已有一趟巡检在跑时，补跑直接放弃（不重复 depart）。
func TestStartupCatchUpTravelSkipsWhenBusy(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{buddy: `{"id":1,"name":"档案喵"}`, state: `{"state":"idle","daily_limit_reached":false}`}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	now := time.Date(2026, 9, 11, 9, 5, 0, 0, cstZone)
	fixedNow(t, now)
	s, _ := newTravelCatchUpScheduler(t, srv, "u1")

	targets := s.travelCatchUpTargets(now, nil)
	if len(targets) != 1 {
		t.Fatalf("targets=%d want 1（无快照 → 保守重查）", len(targets))
	}
	s.travelMu.Lock() // 模拟另一趟（定时时点/手动）正在跑
	defer s.travelMu.Unlock()
	s.runTravelSubset(scheduledCtx(context.Background()), targets, true)

	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("撞车时仍发起了 %d 次 buddy/info", n)
	}
}

// TestStartupCatchUpRespectsDisabledSwitches 两类任务都被显式关掉时什么都不做。
func TestStartupCatchUpRespectsDisabledSwitches(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	now := time.Date(2026, 9, 11, 21, 5, 0, 0, cstZone)
	fixedNow(t, now)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{
		Pool: p, Upstream: up, Location: cstZone,
		CheckinAt: []int{9 * 60}, TravelAt: []int{9 * 60},
		CheckinDisabled: true, TravelDisabled: true,
	})

	s.catchUpAt(context.Background(), now)

	if n := f.checkinCalls.Load(); n != 0 {
		t.Errorf("签到已禁用却补跑了 %d 次", n)
	}
}

// TestStartupCatchUpTravelClaimsThenDeparts 重启跨过「领奖」与「当日派出」两个步骤时，
// 一趟补跑要把两步都追平：领完奖（今日还没派出）顺手把今日派出也补上。
func TestStartupCatchUpTravelClaimsThenDeparts(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{
		buddy: `{"id":1,"name":"档案喵"}`,
		state: `{"state":"arrived","record_id":7,"reward_credit":50,"daily_limit_reached":false}`,
	}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	now := time.Date(2026, 9, 11, 21, 5, 0, 0, cstZone)
	fixedNow(t, now)
	s, p := newTravelCatchUpScheduler(t, srv, "u1")
	p.SetCat("u1", pool.CatState{State: travelStateArrived, Reward: 50, UpdatedAt: now})

	s.catchUpAt(context.Background(), now)

	if n := stub.claimCalls.Load(); n != 1 {
		t.Errorf("claim calls=%d want 1", n)
	}
	if n := stub.departCalls.Load(); n != 1 {
		t.Errorf("depart calls=%d want 1（今日未派出，补跑应连着补）", n)
	}
	cat, ok := p.CatOf("u1")
	if !ok || cat.State != travelStateTraveling {
		t.Errorf("cat state=%v want traveling（补派出后在路上）", cat)
	}
	if ok && !cat.DailyLimitReached {
		t.Errorf("补派出后快照应标记 DailyLimitReached: %+v", cat)
	}
}

// TestRunTravelDoesNotDrainSteps 定时/手动链路保持原语义：领奖后不在同一趟里再派出
// （后面还有时点兜着，一次巡检只做一个动作）。
func TestRunTravelDoesNotDrainSteps(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{
		buddy: `{"id":1,"name":"档案喵"}`,
		state: `{"state":"arrived","record_id":7,"reward_credit":50,"daily_limit_reached":false}`,
	}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	now := time.Date(2026, 9, 11, 21, 5, 0, 0, cstZone)
	fixedNow(t, now)
	s, _ := newTravelCatchUpScheduler(t, srv, "u1")

	s.runTravel(context.Background()) // 定时链路（catchUp=false）

	if n := stub.claimCalls.Load(); n != 1 {
		t.Errorf("claim calls=%d want 1", n)
	}
	if n := stub.departCalls.Load(); n != 0 {
		t.Errorf("depart calls=%d want 0（定时链路不连做两个动作）", n)
	}
}

// TestCatchUpAccountsOnlyTouchesNewOnes 新账号纳管补跑只动这一批 UID：
// 老号即使今天还没签到，也不会因为别人入池被顺带重打一遍。
func TestCatchUpAccountsOnlyTouchesNewOnes(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	now := time.Date(2026, 9, 11, 9, 5, 0, 0, cstZone)
	fixedNow(t, now)
	s, p := newCheckinCatchUpScheduler(t, srv, now, "old", "new")
	p.MarkCheckin("old", "ok", now) // 老号今天已签到

	s.catchUpAccountsAt(context.Background(), []string{"new"}, now)
	if n := f.checkinCalls.Load(); n != 1 {
		t.Errorf("checkin calls=%d want 1（只补新号）", n)
	}

	// 即便把老号递进来：它今天已签到，仍然一次都不打。
	s.catchUpAccountsAt(context.Background(), []string{"old"}, now)
	if n := f.checkinCalls.Load(); n != 1 {
		t.Errorf("已签到的老号被重打了：calls=%d", n)
	}
}

// TestCatchUpAccountsSkipsBeforeSlot 今天时点还没到 → 新号也不补（不抢跑）。
func TestCatchUpAccountsSkipsBeforeSlot(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	now := time.Date(2026, 9, 11, 8, 0, 0, 0, cstZone)
	fixedNow(t, now)
	s, _ := newCheckinCatchUpScheduler(t, srv, now, "new")

	s.catchUpAccountsAt(context.Background(), []string{"new"}, now)

	if n := f.checkinCalls.Load(); n != 0 {
		t.Errorf("时点未到却给新号签到了 %d 次", n)
	}
}

// TestCatchUpAccountsEmptyAndUnknownUID 空列表 / 池里不存在的 UID：什么都不做、不 panic。
func TestCatchUpAccountsEmptyAndUnknownUID(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	now := time.Date(2026, 9, 11, 9, 5, 0, 0, cstZone)
	fixedNow(t, now)
	s, _ := newCheckinCatchUpScheduler(t, srv, now, "new")

	s.catchUpAccountsAt(context.Background(), nil, now)
	s.catchUpAccountsAt(context.Background(), []string{"ghost"}, now)

	if n := f.checkinCalls.Load(); n != 0 {
		t.Errorf("空/未知 UID 不该产生请求：calls=%d", n)
	}
}

// TestCatchUpAccountsTravelOnlyNewOne 旅行同理：只补新号的旅行，不碰老号（即便老号也在待领奖）。
func TestCatchUpAccountsTravelOnlyNewOne(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{
		buddy: `{"id":1,"name":"档案喵"}`,
		state: `{"state":"arrived","record_id":5,"reward_credit":20}`,
	}
	srv := billingAndGrowthServer(stub)
	defer srv.Close()

	now := time.Date(2026, 9, 11, 21, 5, 0, 0, cstZone)
	fixedNow(t, now)
	s, p := newTravelCatchUpScheduler(t, srv, "old", "new")
	p.SetCat("old", pool.CatState{State: travelStateArrived, Reward: 20, UpdatedAt: now})
	p.SetCat("new", pool.CatState{State: travelStateArrived, Reward: 20, UpdatedAt: now})

	s.catchUpAccountsAt(context.Background(), []string{"new"}, now)

	if n := stub.statusCalls.Load(); n != 1 {
		t.Errorf("travel/status calls=%d want 1（只查新号）", n)
	}
	if n := stub.claimCalls.Load(); n != 1 {
		t.Errorf("travel/claim calls=%d want 1（只领新号的奖）", n)
	}
}

// TestCatchUpAccountsRespectsDisabledSwitches 两类任务都关掉时新号也不补。
func TestCatchUpAccountsRespectsDisabledSwitches(t *testing.T) {
	f := &fakeUpstream{resourceRemain: 500}
	srv := f.server()
	defer srv.Close()

	now := time.Date(2026, 9, 11, 21, 5, 0, 0, cstZone)
	fixedNow(t, now)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "new", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{
		Pool: p, Upstream: up, Location: cstZone,
		CheckinAt: []int{9 * 60}, TravelAt: []int{9 * 60},
		CheckinDisabled: true, TravelDisabled: true,
	})

	s.catchUpAccountsAt(context.Background(), []string{"new"}, now)

	if n := f.checkinCalls.Load(); n != 0 {
		t.Errorf("签到已禁用却补跑了 %d 次", n)
	}
}

// TestStartupCatchUpBothTasks 重启后签到与旅行都缺：两者都要补（先签到、后旅行）。
func TestStartupCatchUpBothTasks(t *testing.T) {
	fastTravel(t)
	stub := &travelStub{
		buddy: `{"id":1,"name":"档案喵"}`,
		state: `{"state":"arrived","record_id":9,"reward_credit":30}`,
	}
	var checkinCalls atomic.Int32
	growth := stub.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			checkinCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/token/refresh"):
			w.Write([]byte(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))
		default:
			growth.ServeHTTP(w, r)
		}
	}))
	defer srv.Close()

	now := time.Date(2026, 9, 11, 9, 5, 0, 0, cstZone)
	fixedNow(t, now)
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{
		Pool: p, Upstream: up, Location: cstZone,
		CheckinAt: []int{9 * 60, 21 * 60}, TravelAt: []int{9 * 60, 21 * 60},
	})
	p.SetCat("u1", pool.CatState{State: travelStateArrived, Reward: 30, UpdatedAt: now})

	s.catchUpAt(context.Background(), now)

	if n := checkinCalls.Load(); n != 1 {
		t.Errorf("checkin calls=%d want 1", n)
	}
	if n := stub.claimCalls.Load(); n != 1 {
		t.Errorf("claim calls=%d want 1", n)
	}
}
