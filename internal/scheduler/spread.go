// spread.go 定时批量任务的「随机摊开」：把一批账号的执行时刻摊在一个窗口里。
//
// 为什么需要它：`config.json` 的 `schedule.*_hours` 只回答「哪一小时」（如 9 点签到），
// 原先的实现是到点立刻按固定间隔（签到 3s、旅行/上报 0.8s）顺序打——N 个账号在时间轴
// 上是一条**等距直线**：第 2 个永远在第 1 个之后 3 秒，整批几分钟内跑完。在服务端看来
// 这是机器节奏，真实用户不会在同一分钟里按固定间隔签到 N 次。
//
// 做法：**分层随机抽样（stratified sampling）+ 随机执行顺序**
//  1. 把窗口（默认 15 分钟）按账号数 N 等分成 N 段，每段里取一个均匀随机时刻；
//  2. 随机打乱「哪一段先执行」——账号与时刻的对应关系因此不可预测；
//  3. 结果：整批**必然**落在窗口内、相邻账号不会挤在一起（期望间隔 = 窗口/N），
//     但没有任何固定周期能被统计出来。
//
// 为什么不用「每个账号独立取 [0,窗口) 的均匀随机」：N 稍大就会出现成簇（生日问题），
// 而「成簇 + 空洞」本身也是特征。分层抽样把"分散"变成结构保证，随机性留在段内与顺序上。
//
// 配置项**保持不变**（`schedule.*_hours` 照旧只写小时，不新增必填项）；窗口可用环境变量
// 覆盖，用于排障或调整：
//
//	WB2A_TASK_SPREAD=20m   # 改窗口
//	WB2A_TASK_SPREAD=0     # 关闭摊开（回到旧的"立即 + 固定间隔"顺序执行）
//
// 只对**定时触发**生效：手动点「全部签到」时用户正等着结果，不能把它摊成 15 分钟。
package scheduler

import (
	"context"
	"math/rand"
	"os"
	"strings"
	"time"
)

const (
	// spreadWindowDefault 一批账号摊开的默认窗口（15 分钟）。
	//
	// 15 分钟是"够散但不至于跨出这一小时"的折中：窗口必须明显小于 1 小时，
	// 否则下一个整点的任务会与上一轮重叠；同时要远大于单账号请求耗时，
	// 这样即使几百个账号也不会把所有请求挤在前几秒。
	spreadWindowDefault = 15 * time.Minute
)

// spreadMinGap 相邻两个账号的最小期望间隔下限。
// 账号多到 N*spreadMinGap 超过窗口时窗口自动放宽——宁可超过 15 分钟，
// 也不要让几百个账号在几百秒里贴着发（那等于没摊开）。
// 变量而不是常量：测试会把它调小，才能用毫秒级窗口验证"确实摊开了"。
var spreadMinGap = 2 * time.Second

// spreadRandInt63n 随机源（[0,n)）。包级变量便于测试注入确定值。
var spreadRandInt63n = func(n int64) int64 {
	if n <= 1 {
		return 0
	}
	return rand.Int63n(n)
}

// spreadWindow 返回当前生效的摊开窗口；0 表示关闭（回到固定间隔）。
// 解析失败一律回落默认值：这个开关只影响节奏，不该让任务停摆。
func spreadWindow() time.Duration {
	v := strings.TrimSpace(os.Getenv("WB2A_TASK_SPREAD"))
	if v == "" {
		return spreadWindowDefault
	}
	switch strings.ToLower(v) {
	case "0", "off", "false", "no":
		return 0
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return d
	}
	return spreadWindowDefault
}

// accountPacer 一次批量任务的「账号投放节奏」。
//
// 两种模式：
//   - active（定时 + 窗口开启）：按预计算的分层随机时刻逐个等待；
//   - 非 active（手动触发 / 窗口关闭 / 单账号）：第 1 个立即执行，其余退回既有的
//     `accountGap(base)` 固定间隔（行为与改造前完全一致，便于排障对照）。
type accountPacer struct {
	ctx    context.Context
	s      *Scheduler
	base   time.Duration
	active bool
	window time.Duration
	at     []time.Time // active 时：第 k 个执行的绝对时刻（升序）
	order  []int       // 执行顺序：order[k] 指向调用方传入的列表下标
}

// newAccountPacer 生成节奏器。n 是**实际要处理**的账号数（调用方应先过滤掉跳过项，
// 否则计划里会混进不会执行的账号，窗口被稀释）。
func (s *Scheduler) newAccountPacer(ctx context.Context, kind taskKind, n int, base time.Duration) *accountPacer {
	p := &accountPacer{ctx: ctx, s: s, base: base}

	// 是否定时触发：只有定时链路才会塞这个键（dispatch 里设置）。
	scheduled, _ := ctx.Value(scheduledAccountKey{}).(bool)
	window := spreadWindow()
	if !scheduled || window <= 0 || n <= 1 {
		p.order = identityOrder(n)
		return p
	}

	// 账号太多时放宽窗口，保证"摊开"这件事真的成立。
	if need := time.Duration(n) * spreadMinGap; need > window {
		window = need
	}
	p.active = true
	p.window = window

	width := window / time.Duration(n)
	if width <= 0 {
		width = time.Millisecond
	}
	start := nowFunc()
	p.at = make([]time.Time, n)
	for i := 0; i < n; i++ {
		// 第 i 段内的均匀随机点：[i*width, (i+1)*width)。
		off := time.Duration(i)*width + time.Duration(spreadRandInt63n(int64(width)))
		p.at[i] = start.Add(off)
	}
	// 段已升序；打乱"哪一段归谁执行"。
	p.order = shuffledOrder(n)
	return p
}

// Active 是否处于「随机摊开」模式。
func (p *accountPacer) Active() bool { return p.active }

// Window 生效窗口（仅 active 时有意义），用于日志。
func (p *accountPacer) Window() time.Duration { return p.window }

// Order 返回执行顺序（下标指向调用方传入的账号列表）。
func (p *accountPacer) Order() []int { return p.order }

// Wait 等到第 k 个执行位（0-based）。false = ctx 取消，调用方应立即返回。
func (p *accountPacer) Wait(k int) bool {
	if p.ctx.Err() != nil {
		return false
	}
	if p.active {
		if k >= len(p.at) {
			return true
		}
		if d := p.at[k].Sub(nowFunc()); d > 0 {
			return sleepCtx(p.ctx, d)
		}
		return true
	}
	// 旧行为：第一个立即跑，之后按 accountGap（含 config 的账号间隔抖动）等待。
	if k == 0 {
		return true
	}
	return sleepCtx(p.ctx, p.s.accountGap(p.ctx, p.base))
}

// Describe 给日志用的一句话（"摊开：N 个账号 / 15m" 或 "顺序执行"）。
func (p *accountPacer) Describe(n int) string {
	if !p.active {
		return "sequential (no spread)"
	}
	next := ""
	if len(p.at) > 0 {
		next = p.at[0].Format("15:04:05")
	}
	return "spread " + p.window.Round(time.Second).String() + " for " + itoa(n) + " account(s), first at " + next
}

func identityOrder(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

// shuffledOrder Fisher–Yates 洗牌（用可注入的随机源）。
func shuffledOrder(n int) []int {
	out := identityOrder(n)
	for i := n - 1; i > 0; i-- {
		j := int(spreadRandInt63n(int64(i + 1)))
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// itoa 极小整数转字符串（避免为一行日志引入 fmt 依赖链）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
