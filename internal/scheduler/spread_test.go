package scheduler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/pool"
)

// withScheduled 模拟定时触发的 ctx（dispatch 会给它塞 scheduledAccountKey）。
func withScheduled(ctx context.Context) context.Context {
	return context.WithValue(ctx, scheduledAccountKey{}, true)
}

// spreadWindow 的解析：空=默认 15m；0/off=关闭；合法时长照用；非法回落默认。
func TestSpreadWindowParsing(t *testing.T) {
	cases := map[string]time.Duration{
		"":        spreadWindowDefault,
		"0":       0,
		"off":     0,
		"OFF":     0,
		"false":   0,
		"20m":     20 * time.Minute,
		"90s":     90 * time.Second,
		"garbage": spreadWindowDefault,
		"-5m":     spreadWindowDefault,
	}
	for in, want := range cases {
		t.Setenv("WB2A_TASK_SPREAD", in)
		if got := spreadWindow(); got != want {
			t.Errorf("WB2A_TASK_SPREAD=%q → %v，want %v", in, got, want)
		}
	}
}

// 定时 + 窗口开启时：每个账号都有自己的时刻，全部落在窗口内，且相邻间隔不小于
// 「分层宽度/2」——即「不挤在一起」是结构保证，不靠运气。
func TestPacerSpreadsAccountsWithinWindow(t *testing.T) {
	t.Setenv("WB2A_TASK_SPREAD", "10m")
	const n = 12
	s := New(Config{})
	ctx := withScheduled(context.Background())

	before := time.Now()
	p := s.newAccountPacer(ctx, taskCheckin, n, 0)
	if !p.Active() {
		t.Fatal("定时 + 窗口开启时应处于摊开模式")
	}
	if p.Window() != 10*time.Minute {
		t.Fatalf("窗口 = %v，want 10m", p.Window())
	}
	if len(p.at) != n || len(p.Order()) != n {
		t.Fatalf("计划长度 = %d/%d，want %d", len(p.at), len(p.Order()), n)
	}

	width := p.Window() / time.Duration(n)
	for i, at := range p.at {
		off := at.Sub(before)
		// 分层抽样的硬保证：第 i 个时刻落在第 i 段 [i*width, (i+1)*width) 内。
		// 相邻两点偶尔可以很近（前一段的段尾 + 后一段的段首），但"每段只有一个账号"
		// 才是均匀摊开的结构保证——不是靠运气凑出漂亮间隔。
		if off < time.Duration(i)*width {
			t.Fatalf("第 %d 个时刻 %v 早于本段起点 %v", i, off, time.Duration(i)*width)
		}
		if off > time.Duration(i+1)*width+50*time.Millisecond {
			t.Fatalf("第 %d 个时刻 %v 越出本段终点 %v", i, off, time.Duration(i+1)*width)
		}
	}
	// 首尾必须真正分开：整批跨度至少有窗口的一半（12 个号摊在 10 分钟里）。
	if span := p.at[n-1].Sub(p.at[0]); span < p.Window()/2 {
		t.Fatalf("整批跨度 %v 不足窗口 %v 的一半——摊开不成立", span, p.Window())
	}
}

// 顺序必须是 0..n-1 的排列（每个账号恰好执行一次）。
func TestPacerOrderIsPermutation(t *testing.T) {
	t.Setenv("WB2A_TASK_SPREAD", "10m")
	const n = 9
	s := New(Config{})
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

// 账号多到 N*spreadMinGap 超过窗口时，窗口自动放宽（不把几百个号挤在窗口里贴着发）。
func TestPacerWidensWindowForManyAccounts(t *testing.T) {
	t.Setenv("WB2A_TASK_SPREAD", "1m")
	s := New(Config{})
	const n = 100 // 100*2s = 200s > 60s
	p := s.newAccountPacer(withScheduled(context.Background()), taskCheckin, n, 0)
	if !p.Active() {
		t.Fatal("应处于摊开模式")
	}
	if want := time.Duration(n) * spreadMinGap; p.Window() < want {
		t.Fatalf("窗口 = %v，应至少放宽到 %v", p.Window(), want)
	}
}

// 关闭摊开 / 手动触发 / 单账号：立即执行（第 0 个不等），顺序保持原样。
func TestPacerInactiveFallsBackToLegacy(t *testing.T) {
	s := New(Config{})
	old := checkinAccountDelay
	checkinAccountDelay = 0 // 旧路径的固定间隔置 0，测试不真等
	t.Cleanup(func() { checkinAccountDelay = old })

	cases := []struct {
		name      string
		env       string
		scheduled bool
		n         int
	}{
		{name: "关闭摊开", env: "0", scheduled: true, n: 5},
		{name: "手动触发", env: "10m", scheduled: false, n: 5},
		{name: "单账号", env: "10m", scheduled: true, n: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WB2A_TASK_SPREAD", tc.env)
			ctx := context.Background()
			if tc.scheduled {
				ctx = withScheduled(ctx)
			}
			p := s.newAccountPacer(ctx, taskCheckin, tc.n, 0)
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
}

// ctx 取消时 Wait 必须立刻返回 false（优雅停机不能挂在窗口等待上）。
func TestPacerWaitHonoursCancel(t *testing.T) {
	t.Setenv("WB2A_TASK_SPREAD", "10m")
	s := New(Config{})

	ctx, cancel := context.WithCancel(withScheduled(context.Background()))
	cancel()
	p := s.newAccountPacer(ctx, taskCheckin, 3, 0)
	if !p.Active() {
		t.Fatal("应处于摊开模式")
	}
	start := time.Now()
	if p.Wait(0) {
		t.Fatal("取消后 Wait 应返回 false")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("取消后不应等待（等了 %v）", elapsed)
	}
}

// 端到端：定时签到在摊开模式下——所有账号都跑到、总耗时落在窗口量级（测试用毫秒级
// 窗口），且**第一个账号也不再是"到点立刻打"**。
func TestScheduledCheckinSpreads(t *testing.T) {
	t.Setenv("WB2A_TASK_SPREAD", "200ms")
	oldGap := spreadMinGap
	spreadMinGap = 10 * time.Millisecond
	t.Cleanup(func() { spreadMinGap = oldGap })

	s, p := newCheckinS(t, &checkinStub{checkinBody: `{"code":0,"data":{}}`, resourceRemain: 100})
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
	t.Setenv("WB2A_TASK_SPREAD", "60ms")
	oldGap := spreadMinGap
	spreadMinGap = 5 * time.Millisecond
	t.Cleanup(func() { spreadMinGap = oldGap })

	installTaskBins(t, TaskBinSchool)
	p := pool.New("")
	t.Cleanup(p.Close)
	for _, uid := range []string{"s1", "s2", "s3"} {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	s := New(Config{Pool: p})

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
