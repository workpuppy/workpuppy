// admin_access.go 管理面「接入映射」：下游接入 key 的增删改查。
//
// 数据流：
//
//	管理密码 → /admin/access-keys → accesskey.Store（data/access-keys.json）
//	下游 Bearer <key> → 数据面 withAccessKey → 请求上下文里的账号/模型白名单
//
// 设计约束：
//   - 端点只认管理密码（withAdminAuth），绝不受理下游 key——key 不能自己给自己加权限；
//   - 响应里的 key 是明文（文件本身就是 0600），面板需要能随时复制；
//   - 校验失败回 400/409 并带可读文案，前端直接展示（不做二次翻译）。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"wp/internal/accesskey"
)

// accessKeyBodyLimit 接入 key 请求体上限：账号/模型白名单最多各 1000 项，
// 8KB 不够（旧管理面小接口的默认值）。1 MiB 对两列表足够，也拦住畸形大包。
const accessKeyBodyLimit = 1 << 20

// adminAccessKeys GET /admin/access-keys —— 列出全部接入 key。
func (h *Handler) adminAccessKeys(w http.ResponseWriter, r *http.Request) {
	st := h.cfg.AccessKeys
	if st == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "not_implemented", "接入 key 存储未接线")
		return
	}
	keys := st.List()
	writeJSON(w, http.StatusOK, map[string]any{
		"keys":  keys,
		"total": len(keys),
		"path":  st.Path(),
	})
}

// adminAccessKeyCreate POST /admin/access-keys —— 新建 key。
func (h *Handler) adminAccessKeyCreate(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AccessKeys == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "not_implemented", "接入 key 存储未接线")
		return
	}
	in, ok := decodeAccessKeyInput(w, r)
	if !ok {
		return
	}
	k, err := h.cfg.AccessKeys.Create(in)
	if err != nil {
		writeAccessKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": k})
}

// adminAccessKeyUpdate PUT /admin/access-keys/{id} —— 更新名称/启用态/授权范围。
// 密钥本身不变（换密钥走 /regenerate），避免表单误提交把下游正在用的 key 换掉。
func (h *Handler) adminAccessKeyUpdate(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AccessKeys == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "not_implemented", "接入 key 存储未接线")
		return
	}
	id, ok := accessKeyID(w, r)
	if !ok {
		return
	}
	in, ok := decodeAccessKeyInput(w, r)
	if !ok {
		return
	}
	k, err := h.cfg.AccessKeys.Update(id, in)
	if err != nil {
		writeAccessKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": k})
}

// adminAccessKeyDelete DELETE /admin/access-keys/{id} —— 删除（立即失效）。
func (h *Handler) adminAccessKeyDelete(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AccessKeys == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "not_implemented", "接入 key 存储未接线")
		return
	}
	id, ok := accessKeyID(w, r)
	if !ok {
		return
	}
	if err := h.cfg.AccessKeys.Delete(id); err != nil {
		writeAccessKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": id})
}

// adminAccessKeyEnable / Disable 只切换启用态（保留配置，便于临时停用）。
func (h *Handler) adminAccessKeyEnable(w http.ResponseWriter, r *http.Request) {
	h.setAccessKeyEnabled(w, r, true)
}

func (h *Handler) adminAccessKeyDisable(w http.ResponseWriter, r *http.Request) {
	h.setAccessKeyEnabled(w, r, false)
}

func (h *Handler) setAccessKeyEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	if h.cfg.AccessKeys == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "not_implemented", "接入 key 存储未接线")
		return
	}
	id, ok := accessKeyID(w, r)
	if !ok {
		return
	}
	k, err := h.cfg.AccessKeys.SetEnabled(id, enabled)
	if err != nil {
		writeAccessKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": k})
}

// adminAccessKeyRegenerate POST /admin/access-keys/{id}/regenerate —— 换密钥。
// 旧 key 立即失效，其它字段（账号/模型白名单）保持不变。
func (h *Handler) adminAccessKeyRegenerate(w http.ResponseWriter, r *http.Request) {
	if h.cfg.AccessKeys == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "not_implemented", "接入 key 存储未接线")
		return
	}
	id, ok := accessKeyID(w, r)
	if !ok {
		return
	}
	k, err := h.cfg.AccessKeys.Regenerate(id)
	if err != nil {
		writeAccessKeyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"key": k})
}

// accessKeyID 提取并校验路径段 id。
func accessKeyID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 80 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "key id is required")
		return "", false
	}
	return id, true
}

// decodeAccessKeyInput 读取并解析创建/更新请求体。
func decodeAccessKeyInput(w http.ResponseWriter, r *http.Request) (accesskey.Input, bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, accessKeyBodyLimit))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return accesskey.Input{}, false
	}
	if len(raw) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不能为空")
		return accesskey.Input{}, false
	}
	var in accesskey.Input
	if err := json.Unmarshal(raw, &in); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不是合法 JSON: "+err.Error())
		return accesskey.Input{}, false
	}
	return in, true
}

// writeAccessKeyError 把存储层错误映射成 HTTP 状态码。
func writeAccessKeyError(w http.ResponseWriter, err error) {
	var ve *accesskey.ValidationError
	switch {
	case errors.As(err, &ve):
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", ve.Msg)
	case errors.Is(err, accesskey.ErrNotFound):
		writeOpenAIError(w, http.StatusNotFound, "not_found", "接入 key 不存在")
	case errors.Is(err, accesskey.ErrConflict):
		writeOpenAIError(w, http.StatusConflict, "conflict", "该密钥文本已存在")
	default:
		writeOpenAIError(w, http.StatusInternalServerError, "save_failed", "保存接入 key 失败: "+err.Error())
	}
}
