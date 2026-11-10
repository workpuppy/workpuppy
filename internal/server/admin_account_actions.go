// admin_account_actions.go 账号页顶部的「批量动作」端点：
//   - 一键刷新余额：把导入后从未打过上游的账号拉一次余额（导入的号 credits 常为 0）；
//   - 一键猫猫旅行：手动补一次自动任务（领养 / 派出 / 领奖闭环）。
package server

import (
	"log"
	"net/http"
	"sync"
	"time"

	"wp/internal/logfmt"
)

// adminRefreshBalances POST /admin/accounts/refresh —— 逐账号拉一次余额（只读）。
//
// 为什么需要：从旧平台导入的 auth 文件里**没有积分字段**，池里的 credits 只有在
// 签到 / 单号签到 / 首次请求后才有值，所以导入后一律显示 0（顶部刷新以前只重画页面，
// 并不会去上游取数）。这里做成显式的一次批量只读刷新：
//   - 串行 + 账号间 sleep（复用 schedule.intra_request_jitter 量级），不打爆上游；
//   - 只调 billing/meter 只读接口（UserResource），不碰签到；
//   - 结果回写池（credits / creditsExpiring），并顺手解冻"积分恢复"的冷却账号。
type balanceRefresh struct {
	mu      sync.Mutex
	running bool
}

var balanceRefreshState balanceRefresh // 单飞：同一时刻只允许一轮刷新

func (h *Handler) adminRefreshBalances(w http.ResponseWriter, r *http.Request) {
	if !balanceRefreshState.mu.TryLock() {
		writeOpenAIError(w, http.StatusConflict, "busy", "余额刷新正在进行中")
		return
	}
	defer balanceRefreshState.mu.Unlock()

	statuses := h.cfg.Pool.List()
	type row struct {
		UID      string `json:"uid"`
		Nickname string `json:"nickname,omitempty"`
		Status   string `json:"status"` // ok / skipped / fail
		Credits  *int64 `json:"credits,omitempty"`
		Detail   string `json:"detail,omitempty"`
	}
	rows := make([]row, 0, len(statuses))
	var okN, skipN, failN int

	for _, st := range statuses {
		if st.Disabled || h.cfg.Pool.RiskPaused(st.UID) {
			rows = append(rows, row{UID: st.UID, Nickname: st.Nickname, Status: "skipped", Detail: "disabled"})
			skipN++
			continue
		}
		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			rows = append(rows, row{UID: st.UID, Nickname: st.Nickname, Status: "skipped", Detail: "no credentials"})
			skipN++
			continue
		}
		remain, buckets, err := h.cfg.Upstream.UserResourceDetailed(a, 0) // 0 = 不分桶，只要总量
		if err != nil {
			rows = append(rows, row{UID: st.UID, Nickname: st.Nickname, Status: "fail", Detail: err.Error()})
			failN++
			log.Printf("refresh-balance %s: %v", logfmt.Label(st.UID, st.Nickname), err)
		} else {
			h.cfg.Pool.ReenableIfCredits(st.UID, remain)
			h.cfg.Pool.SetCreditsDetailed(st.UID, remain, buckets.Expiring)
			v := remain
			rows = append(rows, row{UID: st.UID, Nickname: st.Nickname, Status: "ok", Credits: &v})
			okN++
		}
		// 账号之间留一点间隔，避免一轮刷新把上游打热（与签到同一量级）。
		time.Sleep(200 * time.Millisecond)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": okN, "skipped": skipN, "fail": failN, "total": len(rows),
		"accounts": rows,
		"hint":     "余额来自上游 billing 只读接口；导入的账号首次刷新后才有值",
	})
}

// adminTravelRun POST /admin/travel/run —— 手动补一次「猫猫旅行」巡检
// （领养 / 派出 / 领奖闭环），作为自动计划任务错过时的补偿。
//
// 同步调用会按账号节奏跑很久（每号几百毫秒 + 抖动），所以异步执行、立刻回 202：
// 前端只需要"已开始"，进度看账号页/日志。
func (h *Handler) adminTravelRun(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Scheduler == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "not_implemented", "调度器未接线")
		return
	}
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("WARN: 手动猫猫旅行 panic: %v", rec)
			}
		}()
		h.cfg.Scheduler.RunTravelNow()
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{
		"started": true,
		"hint":    "已在后台执行一轮猫猫旅行（领养/派出/领奖）；结果看日志与账号页",
	})
}
