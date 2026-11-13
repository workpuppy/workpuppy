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
)

// adminOverview GET /admin/overview —— 概览页一次性拿到 status + credits。
func (h *Handler) adminOverview(w http.ResponseWriter, _ *http.Request) {
	out := map[string]any{
		"generated_at": time.Now().Format(time.RFC3339),
		"status":       h.statusPayload(),
	}
	if h.cfg.Credits != nil {
		snap := h.cfg.Credits.Snap()
		out["credits"] = map[string]any{
			"today":     snap.Today,
			"totals":    snap.Totals,
			"accounts":  snap.Accounts,
			"histories": h.cfg.Credits.History(30),
			"days":      30,
			"hint":      "totals/accounts 是当日内存热值；histories 是落盘的每日汇总",
		}
	} else {
		out["credits"] = nil
	}
	writeJSON(w, http.StatusOK, out)
}
