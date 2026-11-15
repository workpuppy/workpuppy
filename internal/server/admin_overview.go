// admin_overview.go 概览页轮询用的聚合快照。
//
// 为什么单独开一个接口，而不是让前端每 5 秒分别打 /admin/status + /admin/credit-daily：
//   - 概览页刷新频率高（5s），一次请求比两次省一半连接与鉴权开销；
//   - 两个接口的口径必须来自**同一时刻**（否则 KPI 与曲线会互相打架），聚合后天然一致。
//
// 硬约束：**不发起任何上游请求**。这里读的全部是进程内状态与内存/落盘缓存：
//   - statusPayload：账号池运行态（内存）；
//   - Credits.Snap()/History：当日内存热值 + 启动时载入的账本文件（最多 30 天）。
//
// 因此轮询不会给上游账号增加任何负载，也不会触发 refresh/checkin 之类动作。
package server

import (
	"net/http"
	"time"

	"wp/internal/creditledger"
	"wp/internal/pool"
)

// adminOverview GET /admin/overview —— 概览页一次性拿到 status + credits。
func (h *Handler) adminOverview(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{
		"generated_at": time.Now().Format(time.RFC3339),
		"status":       h.statusPayload(),
	}
	if h.cfg.Credits != nil {
		snap := h.cfg.Credits.Snap()
		credits := map[string]any{
			"today":     snap.Today,
			"totals":    snap.Totals,
			"accounts":  snap.Accounts,
			"histories": h.cfg.Credits.History(30),
			"days":      30,
			"hint":      "totals/accounts 是当日内存热值；histories 是落盘的每日汇总",
		}
		// 卡片「今日调整(过期+校正)」用的派生值：昨日日终 + 今日获得 − 今日消耗 − 当前总分。
		// 图表里的「调整」仍用账本自己记录的正值，两者互不影响。
		if adj, ok := derivedTodayAdjust(snap, h.cfg.Pool.List()); ok {
			credits["today_adjust"] = adj
		}
		out["credits"] = credits
	} else {
		out["credits"] = nil
	}
	writeJSON(w, http.StatusOK, out)
}

// derivedTodayAdjust 计算「今日调整(过期+校正)」的**有符号**值：
//
//	Σ(每个账号：昨日日终余额 + 今日获得 − 今日记录消耗 − 当前余额)
//
// 正数 = 实际余额低于预期（过期/上游回收/本地估算误差），负数 = 高于预期。
// 逐个账号取"今天之前最近一条 credits_end"作为日终基准：某天没有记录的账号沿用
// 更早的基准；从未有历史基准的新号不参与（否则首次导入的余额会被算成调整）。
//
// 只用于概览 KPI；图表里的「调整」仍是账本记录的正值流水，不受影响。
func derivedTodayAdjust(snap creditledger.Snapshot, statuses []pool.Status) (int64, bool) {
	type basis struct {
		date    string
		credits int64
	}
	prev := make(map[string]basis)
	for _, d := range snap.Histories {
		if d.Date >= snap.Today {
			continue
		}
		for _, a := range d.Accounts {
			if old, ok := prev[a.UID]; !ok || d.Date > old.date {
				prev[a.UID] = basis{date: d.Date, credits: a.Credits}
			}
		}
	}
	current := make(map[string]int64, len(statuses))
	for _, st := range statuses {
		current[st.UID] = st.Credits
	}
	var sum int64
	counted := 0
	for uid, before := range prev {
		cur, ok := current[uid]
		if !ok {
			continue // 已不在池内的账号不参与
		}
		var earned, spent int64
		if a, ok := snap.Accounts[uid]; ok {
			earned, spent = a.Earned, a.Spent
		}
		sum += before.credits + earned - spent - cur
		counted++
	}
	return sum, counted > 0
}
