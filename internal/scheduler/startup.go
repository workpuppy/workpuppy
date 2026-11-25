// startup.go 启动补跑（startup catch-up）：把「重启期间整段错过的定时槽位」补上。
//
// 要解决的问题：Run 主循环的补跑（dueSlots）只能补**进程运行期间**跨过的槽位
// （机器睡眠 / 休眠 / 卡顿）。而运维动作——容器升级、删了重建、镜像换代、崩溃重启——
// 是"进程不在了"，重启后 Run 的 cursor 从 nowFunc() 起算，之前那些时点直接被跳过：
// 09:00 该签到，正好 08:58 开始重建、09:05 起来，当天签到就没了。
// 上游按自然日结算（签到 / 猫猫旅行都是），错过当天无法追溯，只能等明天。
//
// 做法：进程启动后先做一次**只看本地状态**的体检，只补"今天已经过点、但还没做完"的任务：
//
//	签到：今天已有到点时点 && 存在"今天还没签到成功"的账号 → 补跑一轮签到；
//	旅行：今天已有到点时点 && 存在"旅行流程可能没走完"的账号 → 补跑一轮旅行巡检。
//
// 此外还有第二种入口 CatchUpAccounts：**运行期新纳管的账号**（扫码加号 / 手工放凭证
// 文件）在当天时点已过时同样需要补——否则这个号要白等到明天。规则与启动补跑完全
// 相同，只是范围限定在那批新 UID 上。
//
// 三条边界（都很重要）：
//
//  1. **不越权**：只补今天的。昨天的签到/旅行已经过期（上游自然日已翻篇），
//     重放只会是无效请求，还可能被判异常客户端，因此绝不回溯。
//  2. **不抢跑**：今天一个时点都还没到（例如 08:00 重启、配置 09:00 签到）时什么都不做，
//     否则等于把任务提前到用户没配置的时间点。
//  3. **不破节奏**：补跑复用定时链路的账号投放节奏（spread_window 随机摊开 + 账号间隔 +
//     同号请求抖动），只是把起点从"时点"换成了"启动时刻"；补跑同样只跑一轮，
//     因为任务本身按自然日幂等（签到看 lastCheckinDay，旅行看上游状态机）。
package scheduler

import (
	"context"
	"log"
	"time"

	"wp/internal/auth"
	"wp/internal/config"
	"wp/internal/pool"
)

// StartupCatchUp 启动补跑：进程启动时调用一次（cmd/server 在起 Run 之后并发调用）。
// ctx 取消（优雅停机）时立即放弃剩余账号。
func (s *Scheduler) StartupCatchUp(ctx context.Context) { s.catchUpAt(ctx, nowFunc()) }

// CatchUpAccounts 新账号纳管补跑：账号是**运行期**加进来的（扫码加号 / 手工放凭证
// 文件，由 auths 目录监听在 5s 内捡起），当天的时点很可能已经过去了——不补的话
// 这个号今天就白等（签到按自然日结算，过了 CST 零点就追不回来）。
//
// 规则与启动补跑完全一致（今天已有到点时点 + 该号还没做完 + 沿用定时节奏），
// 只是范围限定在 uids 这批新号上，不会把老账号再打一遍。
func (s *Scheduler) CatchUpAccounts(ctx context.Context, uids []string) {
	s.catchUpAccountsAt(ctx, uids, nowFunc())
}

// catchUpAccountsAt 可注入时刻的新账号补跑实现（测试直接调它）。
func (s *Scheduler) catchUpAccountsAt(ctx context.Context, uids []string, now time.Time) {
	if s == nil || s.cfg.Pool == nil || s.cfg.Upstream == nil || len(uids) == 0 {
		return
	}
	only := make(map[string]bool, len(uids))
	for _, uid := range uids {
		only[uid] = true
	}
	loc := s.locFor(now)
	checkinSlot, checkinDue := passedSlot(now, s.cfg.CheckinAt, loc)
	travelSlot, travelDue := passedSlot(now, s.cfg.TravelAt, loc)

	if !s.cfg.CheckinDisabled && checkinDue {
		if pending, total := s.pendingCheckin(now, only); pending > 0 {
			log.Printf("accounts catch-up: 新纳管 %d 个账号，签到时点 %s 已过且其中 %d 个还没签到 → 立刻补跑（沿用定时节奏）",
				total, config.FormatClock(checkinSlot), pending)
			if _, err := s.checkinAllScoped(scheduledCtx(ctx), only); err != nil {
				// 撞上手动/定时的签到（ErrBusy）：新号交给下一个时点，不重复打上游。
				log.Printf("accounts catch-up: 签到补跑跳过: %v", err)
			}
		}
	}
	if !s.cfg.TravelDisabled && travelDue {
		if targets := s.travelCatchUpTargets(now, only); len(targets) > 0 {
			log.Printf("accounts catch-up: 新纳管账号中 %d 个的旅行进度可能没走完（旅行时点 %s 已过）→ 立刻补跑（沿用定时节奏）",
				len(targets), config.FormatClock(travelSlot))
			s.runTravelSubset(scheduledCtx(ctx), targets, true)
		}
	}
}

// catchUpAt 可注入时刻的补跑实现（测试直接调它，避免依赖机器当前时间）。
func (s *Scheduler) catchUpAt(ctx context.Context, now time.Time) {
	if s == nil || s.cfg.Pool == nil || s.cfg.Upstream == nil {
		return
	}
	loc := s.locFor(now)

	checkinSlot, checkinDue := passedSlot(now, s.cfg.CheckinAt, loc)
	travelSlot, travelDue := passedSlot(now, s.cfg.TravelAt, loc)

	didSomething := false
	if !s.cfg.CheckinDisabled && checkinDue {
		pending, total := s.pendingCheckin(now, nil)
		if pending > 0 {
			log.Printf("startup catch-up: 签到时点 %s 已过，%d/%d 个账号今天还没签到 → 立刻补跑（沿用定时节奏）",
				config.FormatClock(checkinSlot), pending, total)
			s.dispatch(ctx, taskCheckin)
			didSomething = true
		}
	}
	if !s.cfg.TravelDisabled && travelDue {
		targets := s.travelCatchUpTargets(now, nil)
		if len(targets) > 0 {
			log.Printf("startup catch-up: 旅行时点 %s 已过，%d 个账号的旅行进度可能没走完 → 立刻补跑（沿用定时节奏）",
				config.FormatClock(travelSlot), len(targets))
			s.runTravelSubset(scheduledCtx(ctx), targets, true)
			didSomething = true
		}
	}
	if !didSomething {
		log.Printf("startup catch-up: 无需补跑（今天还没有到点的时点，或该做的都已完成）")
	}
}

// passedSlot 返回今天 at 里**已经到点**的时点中最大的那个（分钟数，时点按 loc 解释）。
// ok=false 表示今天还没有任何时点到点——这时不该补跑（见文件头「不抢跑」）。
func passedSlot(now time.Time, at []int, loc *time.Location) (minute int, ok bool) {
	n := now.In(loc)
	for _, m := range at {
		t := time.Date(n.Year(), n.Month(), n.Day(), m/60, m%60, 0, 0, loc)
		if t.After(now) {
			continue
		}
		if !ok || m > minute {
			minute, ok = m, true
		}
	}
	return minute, ok
}

// pendingCheckin 今天还没签到成功的账号数（只看池里的本地状态，不打上游）。
//
// 过滤条件与 checkinAll 真正要跑的账号一致（禁用 / 风控暂停 / 无凭证 / global 跳过），
// 所以日志里的 "pending/total" 对得上随后那一轮的实际规模。
// 判定用传入的 now（而不是 time.Now()）：补跑与上层体检必须看同一个时刻。
// only 非空时只统计这些 UID（新账号纳管补跑）。
func (s *Scheduler) pendingCheckin(now time.Time, only map[string]bool) (pending, total int) {
	for _, st := range s.cfg.Pool.List() {
		if only != nil && !only[st.UID] {
			continue
		}
		if st.Disabled || s.cfg.Pool.RiskPaused(st.UID) {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" || a.IsGlobal() {
			continue
		}
		total++
		if done, _ := s.cfg.Pool.CheckedInToday(st.UID, now); !done {
			pending++
		}
	}
	return pending, total
}

// travelCatchUpTargets 挑出"今天旅行流程可能还没走完"的账号（判定只用本地快照）。
//
// 为什么需要挑而不是整批重跑：旅行巡检每号要打 2~3 个上游请求（buddy-info +
// travel/status + 动作），重启后把所有账号无条件重查一遍纯属浪费；而快照能明确
// 证明"今天已经派出且已领奖"的那部分账号，直接跳过即可。
// only 非空时只考虑这些 UID（新账号纳管补跑）。
func (s *Scheduler) travelCatchUpTargets(now time.Time, only map[string]bool) []*auth.Auth {
	today := travelDay(now)
	var out []*auth.Auth
	for _, st := range s.cfg.Pool.List() {
		if only != nil && !only[st.UID] {
			continue
		}
		if st.Disabled || s.cfg.Pool.RiskPaused(st.UID) {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if a.IsGlobal() {
			continue // D4 门控：global 没有猫猫旅行体系
		}
		cat, ok := s.cfg.Pool.CatOf(st.UID)
		if !travelMaybePending(cat, ok, today) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// travelMaybePending 该账号今天的旅行是否可能还有事要做（保守判定：拿不准就算要）。
//
// 只有「今天已派出且已领完奖」（快照 state=idle + daily_limit_reached，且快照是今天的）
// 能证明今日已收工，其余一律重新查一次状态：
//   - 无快照 / 快照是昨天的 → 不知道今天进度；
//   - arrived  → 有奖没领；
//   - traveling → 可能已经到站（快照早于到站时刻），查一次状态才能定；
//   - none     → 没领养，走领养链路（门槛未达时巡检自己会跳过）；
//   - error/未知状态 → 上次没查到，重试。
func travelMaybePending(cat *pool.CatState, ok bool, today string) bool {
	if !ok || cat == nil {
		return true
	}
	if travelDay(cat.UpdatedAt) != today {
		return true // 隔天的快照：daily_limit_reached 已按自然日重置，不代表今天
	}
	if cat.State == travelStateIdle {
		return !cat.DailyLimitReached
	}
	return true
}

// scheduledCtx 给 ctx 打上"定时链路"标记：节奏器（newAccountPacer）据此启用
// spread_window 随机摊开，而不是固定间隔顺序打。
func scheduledCtx(ctx context.Context) context.Context {
	return context.WithValue(ctx, scheduledAccountKey{}, true)
}
