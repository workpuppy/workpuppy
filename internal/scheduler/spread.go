// spread.go 定时批量任务的「随机摊开」：把一批账号的执行时刻散在一个延迟区间里。
//
// 为什么需要它：`config.json` 的 `schedule.*_hours` 只回答「哪一小时」（如 9 点签到），
// 原先的实现是到点立刻按固定间隔（签到 3s、旅行/上报 0.8s）顺序打——N 个账号在时间轴
// 上是一条**等距直线**：第 2 个永远在第 1 个之后 3 秒，整批几分钟内跑完。在服务端看来
// 这是机器节奏，真实用户不会在同一分钟里按固定间隔签到 N 次。
//
// 做法：**给定区间 [0, 窗口)，每个账号独立取一个均匀随机位置**
//   - 每个账号的延迟 = uniform(0, 窗口)，全部样本落在同一区间内、位置互不相关；
//   - 按延迟升序执行（睡眠不能往前跳），但"哪个账号占哪一份延迟"另外洗一次牌，
//     使「哪个号先跑」也不可预测；
//   - 结果：整批必然落在窗口内，间隔没有固定周期，也没有「每段一个」的刻意均匀。
//
// 窗口由**设置项**给定：`config.json` 的 `schedule.spread_window`（时长字符串，
// 如 "15m"；"0" 表示关闭）。排障时也可用环境变量临时覆盖：
//
//	WP_TASK_SPREAD=20m   # 临时改窗口
//	WP_TASK_SPREAD=0     # 临时关闭
//
// 只对**定时触发**生效：手动点「全部签到」时用户正等着结果，不能把它摊成十几分钟。
package scheduler

import (
	"context"
	"math/rand"
	"os"
	"strings"
	"time"
)

// spreadWindowDefault 未配置 `schedule.spread_window` 时使用的窗口。
//
// 15 分钟是「够散但不至于跨出这一小时」的折中：窗口要明显小于 1 小时，否则下一个
// 整点的任务会与上一轮重叠；同时远大于单账号请求耗时，几百个账号也不会挤在前几秒。
const spreadWindowDefault = 15 * time.Minute

// spreadRandInt63n 随机源（[0,n)）。包级变量便于测试注入确定值。
var spreadRandInt63n = func(n int64) int64 {
	if n <= 1 {
		return 0
	}
	return rand.Int63n(n)
}

// spreadWindow 返回当前生效的窗口；0 表示关闭（回到「到点立刻 + 固定间隔」）。
// 优先级：WP_TASK_SPREAD（临时覆盖/排障）> schedule.spread_window（设置项）。
func (s *Scheduler) spreadWindow() time.Duration {
	if v := strings.TrimSpace(os.Getenv("WP_TASK_SPREAD")); v != "" {
		switch strings.ToLower(v) {
		case "0", "off", "false", "no":
			return 0
		}
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
		// 解析失败回落设置项：环境变量只是临时开关，不该让任务停摆。
	}
	return s.cfg.SpreadWindow
}

// accountPacer 一次批量任务的「账号投放节奏」。
//
// 两种模式：
//   - active（定时 + 窗口>0 + 至少 2 个账号）：每个账号等自己那份随机延迟；
//   - 非 active（手动触发 / 窗口关闭 / 单账号）：第 1 个立即执行，其余退回既有的
//     `accountGap(base)` 固定间隔（与改造前完全一致，便于排障对照）。
type accountPacer struct {
	ctx    context.Context
	s      *Scheduler
	base   time.Duration
	active bool
	window time.Duration
	at     []time.Time // active 时：按执行顺序排列的绝对时刻（升序）
	order  []int       // 执行顺序：order[k] 指向调用方传入的列表下标
}

// newAccountPacer 生成节奏器。n 是**实际要处理**的账号数（调用方应先过滤掉跳过项，
// 否则计划里会混进不会执行的账号，把随机分布稀释掉）。
func (s *Scheduler) newAccountPacer(ctx context.Context, kind taskKind, n int, base time.Duration) *accountPacer {
	p := &accountPacer{ctx: ctx, s: s, base: base}

	// 是否定时触发：只有定时链路才会塞这个键（dispatch 里设置）。
	scheduled, _ := ctx.Value(scheduledAccountKey{}).(bool)
	window := s.spreadWindow()
	if !scheduled || window <= 0 || n <= 1 {
		p.order = identityOrder(n)
		return p
	}

	p.active = true
	p.window = window

	start := nowFunc()
	offsets := make([]time.Duration, n)
	for i := 0; i < n; i++ {
		// 每个账号独立取一个 [0, 窗口) 内的均匀随机延迟。
		offsets[i] = time.Duration(spreadRandInt63n(int64(window)))
	}
	// 升序执行（睡眠不能往前跳）；「哪个账号占哪份延迟」由下方洗牌决定。
	sortDurations(offsets)
	p.at = make([]time.Time, n)
	for i, off := range offsets {
		p.at[i] = start.Add(off)
	}
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

// sortDurations 升序排序（插入排序，N 是账号数、几十到几百，足够）。
func sortDurations(d []time.Duration) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}

// ── 同一账号内的连续请求间隔 ────────────────────────────────────────
//
// 为什么单独做一件事：一个账号在一次任务里往往要连打好几个请求（签到链路是
// 「刷新 token → 签到 → 查余额」；旅行是「查有无猫 → 查旅行状态 → 派出/领奖」）。
// 这些请求同一账号、同一域名、同一秒内背靠背发出——比"整批账号挤在一起"更像机器。
//
// 间隔区间由 config.json 的 `schedule.intra_request_jitter_min/max` 决定
// （默认 1s~3s；两者都设 0 = 关闭）。取值语义：
//   - 每次等待 = uniform[min, max]；
//   - max <= min 时退化为固定间隔 min（配置里 min>max 会在加载期报错）；
//   - 两个都为 0 → 不等待（测试与排障用）。
func (s *Scheduler) intraJitter(ctx context.Context) bool {
	min := s.cfg.IntraJitterMin
	max := s.cfg.IntraJitterMax
	if max < min {
		max = min
	}
	d := min
	if span := int64(max - min); span > 0 {
		d += time.Duration(spreadRandInt63n(span))
	}
	return sleepCtx(ctx, d)
}
