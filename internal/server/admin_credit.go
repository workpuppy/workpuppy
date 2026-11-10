// admin_credit.go 积分日账本与「休养」开关的管理面端点。
package server

import (
	"net/http"
	"strconv"
	"strings"
)

// adminCreditDaily GET /admin/credit-daily?days=30 —— 积分日账本。
//
// 口径：earned=赚（签到/活动）、spent=请求消耗、adjust=非请求的余额减少（过期/回收）。
// 响应：today=当日日期；accounts=当日每账号明细（**内存热值**，实时）；
//
//	histories=历史每日汇总（落盘，最多 30 天，含当日）。
func (h *Handler) adminCreditDaily(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Credits == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "not_implemented", "积分账本未接线")
		return
	}
	days := 30
	if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			days = n
		}
	}
	if days > 400 {
		days = 400
	}
	snap := h.cfg.Credits.Snap()
	writeJSON(w, http.StatusOK, map[string]any{
		"today":     snap.Today,
		"totals":    snap.Totals,
		"accounts":  snap.Accounts,
		"histories": h.cfg.Credits.History(days),
		"days":      days,
		"hint":      "totals/accounts 是当日内存热值；histories 是落盘的每日汇总（每账号 earned/spent/adjust + 日末余额）",
	})
}

// adminAccountRest POST /admin/accounts/{uid}/rest —— 让账号「休养」：
// 不参与下游选号（不接对话流量），但签到 / 活跃上报 / 猫猫旅行 / token 保活照跑。
func (h *Handler) adminAccountRest(w http.ResponseWriter, r *http.Request) {
	h.setResting(w, r, true)
}

// adminAccountUnrest 解除休养（恢复正常接流）。
func (h *Handler) adminAccountUnrest(w http.ResponseWriter, r *http.Request) {
	h.setResting(w, r, false)
}

func (h *Handler) setResting(w http.ResponseWriter, r *http.Request, on bool) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" || !uidPattern.MatchString(uid) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "uid 非法")
		return
	}
	found, changed := h.cfg.Pool.SetResting(uid, on)
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	resting, _ := h.cfg.Pool.RestingState(uid)
	hint := "休养中：不接下游流量，签到/上报/旅行/保活照常"
	if !resting {
		hint = "已恢复接流"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"uid":     uid,
		"resting": resting,
		"changed": changed,
		"hint":    hint,
	})
}
