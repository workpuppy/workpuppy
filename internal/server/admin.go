// admin.go 运维管理端点（issue #138 / #118）：账号的禁用 / 启用。
//
// 设计要点：
//   - 账号状态只有**一个**禁用位 disabled：既能由运维手动置位（POST /disable），
//     也能由系统自动置位（连续 12153 session dead 等）。disabled_source 只记录
//     来源（manual/system）用于展示与定责，不影响判定。
//   - 禁用 = 完全停：不接下游流量，且**所有自动任务都不跑**（签到 / 上报 / 旅行 /
//     保活 / 开学季 / 夜猫子）。想「只摘流量、养号照旧」请用休养（/rest），
//     两者语义不同，面板上并列展示。
//   - 「启用」是唯一解除入口：手动禁用与系统禁用一并解除（不再有 revive）。
//   - 默认关闭（config admin.enabled），开启后与 /status 共用同一把 api_key 鉴权。
//   - 幂等：面板重试不会报错；重复禁用只更新原因文案。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// adminState 管理端点的统一响应体：回显操作后的禁用态，面板据此直接更新 UI，
// 不必再打一次 /status。
type adminState struct {
	UID      string `json:"uid"`
	Disabled bool   `json:"disabled"`
	// DisabledSource manual（运维禁用）/ system（系统判定）。面板据此显示
	// 「手动禁用」还是「系统禁用」，以及原禁用原因。
	DisabledSource string `json:"disabled_source,omitempty"`
	DisabledReason string `json:"disabled_reason,omitempty"`
	Changed        bool   `json:"changed"`
}

// adminUID 提取并校验路径段 uid。返回 false 表示已写出响应（uid 为空 → 400），
// 调用方应直接 return。
// 开关判断不在这里：路由按 cfg.AdminEnabled 条件注册（见 NewHandler），
// 未开启时这些 handler 根本不可达——handler 内再判开关是多余的存在性泄露面。
func adminUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "uid is required")
		return "", false
	}
	return uid, true
}

// adminReasonFromBody 读可选 JSON 体里的 reason 字段。
// 空体/非 JSON/无该字段都返回空串（端点不因体格式拒绝——无体是最常见调用形态）。
func adminReasonFromBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	// 限制读取量：reason 是短文本，避免畸形大请求占用内存。
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	return strings.TrimSpace(body.Reason)
}

// adminAccountDisable 禁用：把账号停用（不接下游流量，自动任务也全部不跑）。
// 与系统判定置位的 disabled 是同一个位，这里把来源记为 manual。
func (h *Handler) adminAccountDisable(w http.ResponseWriter, r *http.Request) {
	uid, ok := adminUID(w, r)
	if !ok {
		return
	}
	reason := adminReasonFromBody(r)
	if reason == "" {
		reason = "manual"
	}
	found, changed := h.cfg.Pool.SetDisabled(uid, true, reason)
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	writeJSON(w, http.StatusOK, h.adminStateOf(uid, changed))
}

// adminAccountEnable 启用：解除禁用（唯一解除入口，手动与系统禁用一并解除），
// 连续 12153 计数一并清零。
func (h *Handler) adminAccountEnable(w http.ResponseWriter, r *http.Request) {
	uid, ok := adminUID(w, r)
	if !ok {
		return
	}
	found, changed, _, _ := h.cfg.Pool.ClearDisabled(uid)
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	writeJSON(w, http.StatusOK, h.adminStateOf(uid, changed))
}

// adminModelAvailable 接受管理端的成功探测，只解除该账号该模型的 11102 负缓存。
// 小请求成功不能证明真实 6004 token 配额已恢复，因此不清除 6004 或账号级状态。
func (h *Handler) adminModelAvailable(w http.ResponseWriter, r *http.Request) {
	uid, ok := adminUID(w, r)
	if !ok {
		return
	}
	if _, found := h.cfg.Pool.Status(uid); !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found")
		return
	}
	model := strings.TrimSpace(r.PathValue("model"))
	if model == "" || len(model) > 120 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	h.cfg.Pool.BlockModelClear(uid, model)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model": model})
}

// adminStateOf 读回操作后的禁用态（含来源与原因），供端点响应体使用。
func (h *Handler) adminStateOf(uid string, changed bool) adminState {
	st := adminState{UID: uid, Changed: changed}
	if cur, ok := h.cfg.Pool.Status(uid); ok {
		st.Disabled = cur.Disabled
		st.DisabledSource = cur.DisabledSource
		st.DisabledReason = cur.DisabledReason
	}
	return st
}
