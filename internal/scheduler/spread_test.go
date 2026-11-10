package scheduler

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"wp/internal/auth"
	"wp/internal/pool"
)

// withScheduled 模拟定时触发的 ctx（dispatch 会给它塞 scheduledAccountKey）。
func withScheduled(ctx context.Context) context.Context {
	return context.WithValue(ctx, scheduledAccountKey{}, true)
}

// 窗口解析优先级：环境变量（临时覆盖）> 设置项；环境变量写 0/off 表示临时关闭；
// 环境变量写坏则回落设置项（不能因为一个临时开关写错就让任务停摆）。
func TestSpreadWindowResolution(t *testing.T) {
	s := New(Config{SpreadWindow: 10 * time.Minute})

	os.Unsetenv("WP_TASK_SPREAD")
	if got := s.spreadWindow(); got != 10*time.Minute {
		t.Fatalf("未设环境变量时应取设置项 10m，实际 %v", got)
	}

	t.Setenv("WP_TASK_SPREAD", "3m")
	if got := s.spreadWindow(); got != 3*time.Minute {
		t.Fatalf("环境变量应覆盖设置项，实际 %v", got)
	}

	for _, off := range []string{"0", "off", "OFF", "false"} {
		t.Setenv("WP_TASK_SPREAD", off)
		if got := s.spreadWindow(); got != 0 {
			t.Fatalf("环境变量 %q 应关闭摊开，实际 %v", off, got)
		}
	}

	t.Setenv("WP_TASK_SPREAD", "not-a-duration")
	if got := s.spreadWindow(); got != 10*time.Minute {
		t.Fatalf("环境变量非法应回落设置项，实际 %v", got)
	}

	// 设置项为 0（未配置/显式关闭）时就是关闭。
	os.Unsetenv("WP_TASK_SPREAD")
	if got := New(Config{}).spreadWindow(); got != 0 {
		t.Fatalf("设置项为空应视为关闭，实际 %v", got)
	}
}

// 核心语义：**每个账号的延迟都是 [0, 窗口) 内的均匀随机位置**——
// 全部落在区间内、互不相关，而且要能在整个区间里铺开（不是只落在前半段）。
func TestPacerUniformWithinWindow(t *testing.T) {
	const window = 10 * time.Minute
	const n = 12
	s := New(Config{SpreadWindow: window})
	ctx := withScheduled(context.Background())

	before := time.Now()
	p := s.newAccountPacer(ctx, taskCheckin, n, 0)
	if !p.Active() {
		t.Fatal("定时 + 窗口>0 时应处于摊开模式")
	}
	if p.Window() != window {
		t.Fatalf("窗口 = %v，want %v", p.Window(), window)
	}
	if len(p.at) != n || len(p.Order()) != n {
		t.Fatalf("计划长度 = %d/%d，want %d", len(p.at), len(p.Order()), n)
	}

	var minOff, maxOff time.Duration
	for i, at := range p.at {
		off := at.Sub(before)
		if off < 0 || off >= window+50*time.Millisecond {
			t.Fatalf("第 %d 个延迟 %v 越出 [0,%v)", i, off, window)
		}
		if i > 0 && at.Before(p.at[i-1]) {
			t.Fatalf("执行时刻必须升序（第 %d 个 %v 早于前一个 %v）", i, at, p.at[i-1])
		}
		if i == 0 || off < minOff {
			minOff = off
		}
		if off > maxOff {
			maxOff = off
		}
	}
	// 12 个均匀样本全都落在窗口前 1/4 的概率是 (1/4)^12 ≈ 6e-8，可以放心断言。
	if maxOff < window/4 {
		t.Fatalf("最大延迟 %v 仍在前 1/4 窗口内——不像均匀分布（window=%v）", maxOff, window)
	}
}

// 顺序必须是 0..n-1 的排列（每个账号恰好执行一次）。
func TestPacerOrderIsPermutation(t *testing.T) {
	const n = 9
	s := New(Config{SpreadWindow: 10 * time.Minute})
	p := s.newAccountPacer(withScheduled(context.Background()), taskTravel, n, 0)

	seen := make(map[int]bool, n)
	for _, idx := range p.Order() {
		if idx < 0 || idx >= n {
			t.Fatalf("下标越界：%d", idx)
		}
		if seen[idx] {
			t.Fatalf("下标 %d 重复", idx)
		}
		seen[idx] = true
	}
	if len(seen) != n {
		t.Fatalf("只覆盖 %d/%d 个账号", len(seen), n)
	}
}

// 关闭摊开 / 手动触发 / 单账号：立即执行（第 0 个不等），顺序保持原样。
func TestPacerInactiveFallsBackToLegacy(t *testing.T) {
	s := New(Config{SpreadWindow: 10 * time.Minute})

	cases := []struct {
		name      string
		cfg       Config
		scheduled bool
		n         int
	}{
		{name: "窗口关闭", cfg: Config{SpreadWindow: 0}, scheduled: true, n: 5},
		{name: "手动触发", cfg: Config{SpreadWindow: 10 * time.Minute}, scheduled: false, n: 5},
		{name: "单账号", cfg: Config{SpreadWindow: 10 * time.Minute}, scheduled: true, n: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := New(tc.cfg)
			ctx := context.Background()
			if tc.scheduled {
				ctx = withScheduled(ctx)
			}
			p := sc.newAccountPacer(ctx, taskCheckin, tc.n, 0)
			if p.Active() {
				t.Fatal("不应进入摊开模式")
			}
			for i, idx := range p.Order() {
				if idx != i {
					t.Fatalf("非摊开模式应保持原顺序：order[%d]=%d", i, idx)
				}
			}
			start := time.Now()
			if !p.Wait(0) {
				t.Fatal("第 0 个应立即放行")
			}
			if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
				t.Fatalf("第 0 个不应等待（等了 %v）", elapsed)
			}
		})
	}
	_ = s
}

// ctx 取消时 Wait 必须立刻返回 false（优雅停机不能挂在随机等待上）。
func TestPacerWaitHonoursCancel(t *testing.T) {
	s := New(Config{SpreadWindow: 10 * time.Minute})

	ctx, cancel := context.WithCancel(withScheduled(context.Background()))
	cancel()
	p := s.newAccountPacer(ctx, taskCheckin, 3, 0)
	start := time.Now()
	if p.Wait(0) {
		t.Fatal("取消后 Wait 应返回 false")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("取消后不应等待（等了 %v）", elapsed)
	}
}

// 端到端：定时签到在摊开模式下——所有账号都跑到、总耗时落在窗口量级。
func TestScheduledCheckinSpreads(t *testing.T) {
	s, p := newCheckinS(t, &checkinStub{checkinBody: `{"code":0,"data":{}}`, resourceRemain: 100})
	s.cfg.SpreadWindow = 200 * time.Millisecond
	for _, uid := range []string{"u2", "u3", "u4"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}

	start := time.Now()
	out, err := s.checkinAll(withScheduled(context.Background()))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("checkinAll: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("应有 4 个结果，实际 %d", len(out))
	}
	if elapsed < 20*time.Millisecond {
		t.Fatalf("整批在 %v 内跑完——看起来没有摊开", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("整批耗时 %v，远超窗口 200ms", elapsed)
	}
}

// 端到端：脚本类任务在摊开模式下按**单账号**逐个起子进程（而不是一次 ALL）。
func TestScheduledScriptTaskRunsPerAccount(t *testing.T) {
	installTaskBins(t, TaskBinSchool)
	p := pool.New("")
	t.Cleanup(p.Close)
	for _, uid := range []string{"s1", "s2", "s3"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	s := New(Config{Pool: p, SpreadWindow: 60 * time.Millisecond})

	var mu sync.Mutex
	var calls [][]string
	orig := newScriptCmd
	newScriptCmd = func(name string, args ...string) scriptRunner {
		mu.Lock()
		calls = append(calls, append([]string{name}, args...))
		mu.Unlock()
		return &fakeScriptExec{}
	}
	t.Cleanup(func() { newScriptCmd = orig })

	s.runSchool(withScheduled(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("应逐账号起 3 个子进程，实际 %d：%v", len(calls), calls)
	}
	seen := map[string]bool{}
	for _, c := range calls {
		if len(c) < 3 {
			t.Fatalf("参数过短：%v", c)
		}
		if !strings.HasSuffix(c[0], TaskBinSchool) {
			t.Errorf("应执行 %s，实际 %s", TaskBinSchool, c[0])
		}
		if c[1] == "ALL" {
			t.Errorf("摊开模式不应再用 ALL 一次跑完：%v", c)
		}
		seen[c[1]] = true
	}
	for _, uid := range []string{"s1", "s2", "s3"} {
		if !seen[uid] {
			t.Errorf("账号 %s 未被调度（seen=%v）", uid, seen)
		}
	}
}

// 同一账号内的连续请求之间要有随机间隔：单号签到时是「签到 → 查余额」两次背靠背
// 调用，把区间设为 30ms~30ms（固定 30ms）后总耗时应 ≥ 55ms。
// 区间来自 config.json 的 schedule.intra_request_jitter_min/max，这里直接设 Config。
func TestIntraAccountJitterApplied(t *testing.T) {
	s, _ := newCheckinS(t, &checkinStub{checkinBody: `{"code":0,"data":{}}`, resourceRemain: 100})
	s.cfg.IntraJitterMin = 30 * time.Millisecond
	s.cfg.IntraJitterMax = 30 * time.Millisecond
	start := time.Now()
	if _, err := s.CheckinAll(); err != nil { // 手动触发：不摊开，只有账号内抖动
		t.Fatalf("CheckinAll: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 55*time.Millisecond {
		t.Fatalf("单号签到耗时 %v：账号内两次请求之间没有加抖动", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("单号签到耗时 %v，抖动没有上限约束？", elapsed)
	}
}
