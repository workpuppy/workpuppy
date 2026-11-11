// admin_config.go 设置页的后端：配置读写、恢复默认、初始化/修改管理密码。
//
// 边界：
//   - 除 /admin/setup、/admin/setup/status、/admin/login 外，全部要求管理密码；
//   - 配置写入是**整份校验通过才落盘**（字段级错误原样回给前端），不会写半份；
//   - 保存不改运行期行为（除管理面开关与密码强度外），页面用"重启生效"横幅提示。
package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wp/internal/adminauth"
)

// configBodyLimit 设置页保存/恢复默认的请求体上限：整份配置含数组与映射，
// 管理面默认的 8KB 上限不够用（只放宽这两个端点，登录/改密仍走 adminBodyLimit）。
const configBodyLimit = 1 << 20

// ConfigFieldError 字段级校验错误（path 为空 = 整份配置的错误）。
type ConfigFieldError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

// ConfigSnapshot 是 /admin/config 的核心载荷：
// values 是"文件 + 默认值"合并后的生效值（不含管理密码哈希），defaults 供前端做
// "已恢复默认"对比与单字段复位。
type ConfigSnapshot struct {
	Values   map[string]any `json:"values"`
	Defaults map[string]any `json:"defaults"`
	Schema   any            `json:"schema"`
	Path     string         `json:"path"`
	Writable bool           `json:"writable"`
	// MinPasswordLength 由实现方从生效配置解析（schema 里也有，这里给后端校验用）。
	MinPasswordLength int `json:"min_password_length"`
}

// ConfigResetRequest 恢复默认：单字段 / 整组 / 全部。
type ConfigResetRequest struct {
	Scope   string   `json:"scope"`   // "fields" | "section" | "all"
	Section string   `json:"section"` // scope=section 时的分组 key
	Paths   []string `json:"paths"`   // scope=fields 时的字段路径
}

// ConfigStore 由 cmd/server 实现：负责文件读写、校验与热生效的最小集合。
type ConfigStore interface {
	Snapshot() (*ConfigSnapshot, error)
	Save(values map[string]any) (*ConfigSnapshot, []ConfigFieldError, error)
	Reset(req ConfigResetRequest) (*ConfigSnapshot, []ConfigFieldError, error)
	// SetPassword 写入新的密码哈希；enable=true 时同时把 admin.enabled 置 true
	// （首次初始化用；改密时传 false，保持现有开关）。
	SetPassword(hash string, enable bool) error
}

// configMeta 是 GET/PUT 响应里的运行期元信息。
type configMeta struct {
	Path                string `json:"path"`
	Writable            bool   `json:"writable"`
	RestartPending      bool   `json:"restart_pending"`
	AdminEnabled        bool   `json:"admin_enabled"`
	PasswordInitialized bool   `json:"password_initialized"`
	MinPasswordLength   int    `json:"min_password_length"`
}

// configView 是 GET /admin/config 的完整响应。
type configView struct {
	Values   map[string]any `json:"values"`
	Defaults map[string]any `json:"defaults"`
	Schema   any            `json:"schema"`
	Meta     configMeta     `json:"meta"`
}

func (h *Handler) configMeta(snap *ConfigSnapshot) configMeta {
	return configMeta{
		Path:                snap.Path,
		Writable:            snap.Writable,
		RestartPending:      h.configPending.Load(),
		AdminEnabled:        h.adminEnabled.Load(),
		PasswordInitialized: h.cfg.AdminAuth.Configured(),
		MinPasswordLength:   snap.MinPasswordLength,
	}
}

// adminSetupStatus GET /admin/setup/status —— 匿名，用来决定首屏显示"初始化"还是"登录"。
func (h *Handler) adminSetupStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"initialized":         h.cfg.AdminAuth.Configured(),
		"admin_enabled":       h.adminEnabled.Load(),
		"min_password_length": adminauth.MinLength(),
	})
}

// adminSetup POST /admin/setup —— 首次初始化管理员密码。
//
// 只在"还没初始化"时可成功；成功后把哈希写进 config 文件（同时开启管理面），
// 并热更新内存里的 Store —— 不需要重启。
func (h *Handler) adminSetup(w http.ResponseWriter, r *http.Request) {
	h.setupMu.Lock()
	defer h.setupMu.Unlock()

	if h.cfg.AdminAuth.Configured() {
		writeOpenAIError(w, http.StatusConflict, "admin_already_initialized", "管理员密码已初始化，请直接登录")
		return
	}
	ip := adminClientIP(r)
	if h.adminLoginLimit.blocked(ip, time.Now()) {
		writeOpenAIError(w, http.StatusTooManyRequests, "too_many_attempts", "尝试次数过多，请 5 分钟后再试")
		return
	}
	if c := h.cfg.ConfigStore; c == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "config_unavailable", "配置接口未接线（config store 为空）")
		return
	}
	var body struct {
		Password string `json:"password"`
		Confirm  string `json:"confirm"`
	}
	if err := decodeJSONBody(r, &body, adminBodyLimit); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return
	}
	if strings.TrimSpace(body.Password) != strings.TrimSpace(body.Confirm) {
		h.adminLoginLimit.noteFail(ip, time.Now())
		writeOpenAIError(w, http.StatusBadRequest, "password_mismatch", "两次输入的密码不一致")
		return
	}
	if err := adminauth.CheckPlain(body.Password); err != nil {
		h.adminLoginLimit.noteFail(ip, time.Now())
		writeOpenAIError(w, http.StatusBadRequest, "weak_password", err.Error())
		return
	}
	hash, err := adminauth.HashPassword(body.Password)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "hash_failed", "密码哈希失败："+err.Error())
		return
	}
	if err := h.cfg.ConfigStore.SetPassword(hash, true); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "config_write_failed",
			"写入配置失败（容器内请确认 data 目录可写）："+err.Error())
		return
	}
	if err := h.cfg.AdminAuth.SetStored(hash); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "hash_failed", "保存密码哈希失败："+err.Error())
		return
	}
	h.SetAdminEnabled(true)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminChangePassword POST /admin/password —— 改密（Bearer 就是当前密码）。
func (h *Handler) adminChangePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
		Confirm  string `json:"confirm"`
	}
	if err := decodeJSONBody(r, &body, adminBodyLimit); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return
	}
	if strings.TrimSpace(body.Password) != strings.TrimSpace(body.Confirm) {
		writeOpenAIError(w, http.StatusBadRequest, "password_mismatch", "两次输入的密码不一致")
		return
	}
	if err := adminauth.CheckPlain(body.Password); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "weak_password", err.Error())
		return
	}
	if h.cfg.ConfigStore == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "config_unavailable", "配置接口未接线")
		return
	}
	hash, err := adminauth.HashPassword(body.Password)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "hash_failed", "密码哈希失败："+err.Error())
		return
	}
	if err := h.cfg.ConfigStore.SetPassword(hash, false); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "config_write_failed",
			"写入配置失败："+err.Error())
		return
	}
	if err := h.cfg.AdminAuth.SetStored(hash); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "hash_failed", "保存密码哈希失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminConfigGet GET /admin/config —— 生效值 + 默认值 + schema + 元信息。
func (h *Handler) adminConfigGet(w http.ResponseWriter, _ *http.Request) {
	if h.cfg.ConfigStore == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "config_unavailable", "配置接口未接线")
		return
	}
	snap, err := h.cfg.ConfigStore.Snapshot()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "config_read_failed", "读取配置失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, configView{
		Values:   snap.Values,
		Defaults: snap.Defaults,
		Schema:   snap.Schema,
		Meta:     h.configMeta(snap),
	})
}

// adminConfigPut PUT /admin/config —— 深合并保存；校验失败返回字段级错误。
func (h *Handler) adminConfigPut(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ConfigStore == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "config_unavailable", "配置接口未接线")
		return
	}
	var body struct {
		Values map[string]any `json:"values"`
	}
	if err := decodeJSONBody(r, &body, configBodyLimit); err != nil || len(body.Values) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "请求体应包含 values 对象")
		return
	}
	snap, fieldErrs, err := h.cfg.ConfigStore.Save(body.Values)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "config_write_failed", "保存失败："+err.Error())
		return
	}
	if len(fieldErrs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"code":    "config_invalid",
				"message": "配置有 " + strconv.Itoa(len(fieldErrs)) + " 处不合法",
				"fields":  fieldErrs,
			},
		})
		return
	}
	h.applyHotConfig(snap)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"restart_pending": h.configPending.Load(),
		"values":          snap.Values,
	})
}

// adminConfigReset POST /admin/config/reset —— 单字段 / 整组 / 全部恢复默认。
func (h *Handler) adminConfigReset(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ConfigStore == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "config_unavailable", "配置接口未接线")
		return
	}
	var req ConfigResetRequest
	if err := decodeJSONBody(r, &req, configBodyLimit); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "请求体不是合法 JSON")
		return
	}
	switch req.Scope {
	case "fields":
		if len(req.Paths) == 0 {
			writeOpenAIError(w, http.StatusBadRequest, "bad_request", "paths 不能为空")
			return
		}
	case "section":
		if strings.TrimSpace(req.Section) == "" {
			writeOpenAIError(w, http.StatusBadRequest, "bad_request", "section 不能为空")
			return
		}
	case "all":
	default:
		writeOpenAIError(w, http.StatusBadRequest, "bad_request", "scope 只能是 fields / section / all")
		return
	}
	snap, fieldErrs, err := h.cfg.ConfigStore.Reset(req)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "config_write_failed", "恢复默认失败："+err.Error())
		return
	}
	if len(fieldErrs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]any{
				"code":    "config_invalid",
				"message": "恢复默认后的配置不合法",
				"fields":  fieldErrs,
			},
		})
		return
	}
	h.applyHotConfig(snap)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"restart_pending": h.configPending.Load(),
		"values":          snap.Values,
	})
}

// applyHotConfig 保存成功后同步"可热生效"的两项：管理面开关、密码最小长度。
//
// 其余配置一律重启生效（避免在请求路径上重建 pool/upstream 等全局状态）。
func (h *Handler) applyHotConfig(snap *ConfigSnapshot) {
	h.configPending.Store(true)
	if snap == nil {
		return
	}
	if raw, ok := snap.Values["admin"]; ok {
		if m, ok := raw.(map[string]any); ok {
			if v, ok := m["enabled"].(bool); ok {
				h.SetAdminEnabled(v)
			}
			if n, ok := m["min_password_length"].(float64); ok && n > 0 {
				adminauth.Configure(adminauth.Options{MinLength: int(n)})
			}
		}
	}
}

// decodeJSONBody 限制请求体大小并解码 JSON。
func decodeJSONBody(r *http.Request, dst any, limit int64) error {
	if limit <= 0 {
		limit = adminBodyLimit
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return errors.New("empty body")
	}
	return json.Unmarshal(raw, dst)
}
