// admin_account_actions.go 账号页顶部的「批量动作」端点：
//   - 一键刷新余额：把导入后从未打过上游的账号拉一次余额（导入的号 credits 常为 0）；
//   - 一键猫猫旅行：手动补一次自动任务（领养 / 派出 / 领奖闭环）。
package server

import (
	"context"
	"errors"
	"log"
	"net/http"

	"wp/internal/balancerefresh"
)

// adminRefreshBalances POST /admin/accounts/refresh —— 逐账号拉一次余额（只读）。
//
// 为什么需要：从旧平台导入的 auth 文件里**没有积分字段**，池里的 credits 只有在
// 签到 / 手动刷新 / 定时刷新后才有值；而积分日账本的「调整」也只有权威回写产生
// 负差额时才会出现。这里手动触发一轮，与定时任务共用 balancerefresh 的单飞锁：
//   - 串行 + 账号间 sleep，不打爆上游；
//   - 只调 billing 只读接口（UserResource），不碰签到；
//   - 结果回写池（credits / creditsExpiring），并顺手解冻"积分恢复"的冷却账号。
func (h *Handler) adminRefreshBalances(w http.ResponseWriter, r *http.Request) {
	res, err := balancerefresh.Refresh(context.Background(), h.cfg.Pool, h.cfg.Upstream)
	if errors.Is(err, balancerefresh.ErrBusy) {
		writeOpenAIError(w, http.StatusConflict, "busy", err.Error())
		return
	}
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "refresh_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": res.OK, "skipped": res.Skipped, "fail": res.Fail, "total": res.Total,
		"accounts": res.Rows,
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
