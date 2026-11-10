// admin_panel.go 管理面（REST）：账号管理、扫码加号、模型中心、聊天测试台。
//
// 定位：这是「原 Python 面板那些必须联网/必须看状态的能力」在 Go 侧的最小实现，
// 刻意只做 REST + CLI（`cmd/wpctl`），不带 Web UI。
//
// 设计约束（与本服务既有约定一致）：
//   - 全部端点挂在 `admin.enabled` 开关下，未开启时**根本不注册**（不是 403）——
//     不向未授权者暴露「这里存在管理面」；鉴权与 /status 同源（withAuth + 同一把 api_key）。
//   - 只读端点绝不触发上游写操作：`/admin/accounts` 直接从池状态与 auth 文件取，
//     不主动刷新 token、不查余额；要查余额就走签到（它顺手回填积分）。
//   - 上游错误原样透出 code/msg（排障时那串中文文案就是唯一线索），不包装成泛化错误。
//   - 账号文件写入一律走 auth.SaveAtomic（tmp + rename）：auths 目录有目录 watcher，
//     半截文件会被判成损坏账号。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"wp/internal/auth"
	"wp/internal/pool"
	"wp/internal/upstream"
)

// uidPattern auth 文件名里的 uid 白名单：uid 会直接拼进文件名，不校验等于开放
// 任意路径写入（`../../config.json` 之类）。
var uidPattern = regexp.MustCompile(`^[0-9A-Za-z_-]{1,80}$`)

// adminBodyLimit 管理端小接口的请求体上限（只需一个短 JSON）。
var adminBodyLimit int64 = 8 << 10

// adminAccount 管理面账号视图 = 池运行态（内嵌 pool.Status，字段全量透出）+ 凭证侧元信息。
//
// 为什么不直接复用 /status：那是**给下游客户端看的**稳定契约（字段一改就影响第三方
// SDK），管理面需要的是排障口径（token 何时过期、能不能刷新、绑了哪条线路、手写权重
// 多少）。两者用途不同，字段就该分开演进。
type adminAccount struct {
	pool.Status
	Nickname      string  `json:"nickname,omitempty"` // 池里可能没有（未入池/新文件），用文件里的补
	ExpiresAt     int64   `json:"expires_at,omitempty"`
	RemainingSec  int64   `json:"remaining_sec"`
	HasCredential bool    `json:"has_credential"`
	CanRefresh    bool    `json:"can_refresh"`
	ProxyName     string  `json:"proxy,omitempty"`
	Weight        float64 `json:"weight,omitempty"`
	InPool        bool    `json:"in_pool"`
	Checkinable   bool    `json:"checkinable"`
}

// adminAccounts GET /admin/accounts —— 账号全量清单（含未入池的凭证文件）。
//
// 数据来源两条：池（运行态）+ auths 目录（凭证真相）。两者**必须都看**：
// 池里没有的账号不代表文件坏了（可能是空 accessToken 被上游跳过），
// 文件里没有的 uid 也不该出现（幽灵行比缺行更难查）。
func (h *Handler) adminAccounts(w http.ResponseWriter, r *http.Request) {
	statuses := h.cfg.Pool.List()
	now := time.Now()
	seen := make(map[string]bool, len(statuses))
	out := make([]adminAccount, 0, len(statuses))

	add := func(a *auth.Auth, st *pool.Status) {
		var row adminAccount
		if st != nil {
			row.Status = *st
		}
		if a != nil {
			row.Nickname = a.Nickname
			row.HasCredential = strings.TrimSpace(a.AccessTokenValue()) != ""
			row.CanRefresh = strings.TrimSpace(a.RefreshTokenValue()) != ""
			row.ProxyName = a.ProxyName
			// PickWeight 把「未设置」也归一到 1；这里只在用户真写过权重的展示，
			// 免得每行都挂一个恒为 1 的数字（omitempty 对 1 不生效，故显式清零）。
			if w := a.PickWeight(); w != 1 {
				row.Weight = w
			}
			if a.ExpiresAt > 0 {
				row.ExpiresAt = a.ExpiresAt
				row.RemainingSec = a.ExpiresAt - now.Unix()
			}
		}
		if row.UID == "" && a != nil {
			row.UID = a.UID
		}
		if row.Status.Realm == "" && a != nil {
			row.Status.Realm = a.Realm()
		}
		if row.Nickname == "" && a != nil {
			row.Nickname = a.Nickname
		}
		// InPool 必须**由「是否真的进了池」决定**，不能只看三个禁用位：
		// 只在文件里、没进池的账号（例如缺 refreshToken）三个位都是 false，
		// 按位推断会把它显示成「在线」，而它其实一次请求都接不到。
		row.InPool = st != nil && !row.Disabled && !row.ManualDisabled && !row.Cooling
		// Checkinable：签到只看凭证是否可用（冷却中的号照常签到，这是既有语义）。
		row.Checkinable = row.HasCredential && row.CanRefresh && !row.Disabled
		out = append(out, row)
	}

	for _, st := range statuses {
		stCopy := st
		seen[st.UID] = true
		add(h.cfg.Pool.AuthByUID(st.UID), &stCopy)
	}
	// 只存在于文件、不在池里的账号（例如缺 refreshToken 被池剔除）：如实列出，
	// 否则「我加了这个号但界面上看不到」会变成无从下手的悬案。
	if h.cfg.AuthDir != "" {
		if auths, err := auth.LoadDir(h.cfg.AuthDir); err == nil {
			for _, a := range auths {
				if seen[a.UID] {
					continue
				}
				add(a, nil)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].UID < out[j].UID })

	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": out,
		"total":    total,
		"counts": map[string]int{
			"healthy": healthy, "cooling": cooling, "disabled": disabled, "in_flight_full": inFlightFull,
		},
		"auth_dir": h.cfg.AuthDir,
	})
}

// adminAccountDetail GET /admin/accounts/{uid} —— 单账号（含凭证侧元信息）。
func (h *Handler) adminAccountDetail(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "uid is required")
		return
	}
	a := h.cfg.Pool.AuthByUID(uid)
	st, ok := h.cfg.Pool.Status(uid)
	if !ok && a == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	resp := map[string]any{"uid": uid, "in_pool": ok}
	if ok {
		resp["status"] = st
	}
	if a != nil {
		resp["realm"] = a.Realm()
		resp["nickname"] = a.Nickname
		resp["enterprise_id"] = a.EnterpriseID
		resp["expires_at"] = a.ExpiresAt
		resp["remaining_sec"] = a.ExpiresAt - time.Now().Unix()
		resp["has_credential"] = strings.TrimSpace(a.AccessTokenValue()) != ""
		resp["can_refresh"] = strings.TrimSpace(a.RefreshTokenValue()) != ""
		resp["proxy"] = a.ProxyName
		resp["weight"] = a.PickWeight()
	}
	writeJSON(w, http.StatusOK, resp)
}

// adminCheckinAll POST /admin/accounts/checkin —— 全部账号签到。
//
// 复用调度器同一个入口（scheduler.CheckinAll）：它自带「相邻账号间隔」节流与单飞锁，
// 手动与定时撞车时**返回忙碌而不是并发打上游** —— 并发签到正是风控最敏感的特征。
func (h *Handler) adminCheckinAll(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Scheduler == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "not_implemented", "调度器未接线")
		return
	}
	outcomes, err := h.cfg.Scheduler.CheckinAll()
	if err != nil {
		// ErrBusy：回 409，让调用方知道「是有人在跑」而不是「签到失败」。
		writeOpenAIError(w, http.StatusConflict, "busy", err.Error())
		return
	}
	var okN, alreadyN, failN, skipN int
	for _, o := range outcomes {
		switch string(o.Status) {
		case "ok":
			okN++
		case "already":
			alreadyN++
		case "skipped":
			skipN++
		default:
			failN++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": okN, "already": alreadyN, "fail": failN, "skipped": skipN,
		"total": len(outcomes), "outcomes": outcomes,
	})
}

// adminCheckinOne POST /admin/accounts/{uid}/checkin —— 单账号签到（顺带回填积分）。
//
// 与批量签到的差别只在「只打一个号」，用的是同一个上游端点（upstream.DailyCheckin）
// 与同一套幂等判定（IsAlreadyCheckin → already，不是失败）。
func (h *Handler) adminCheckinOne(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "uid is required")
		return
	}
	a := h.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	if strings.TrimSpace(a.RefreshTokenValue()) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"该账号没有 refreshToken，请重新扫码登录")
		return
	}
	resp := map[string]any{"uid": uid, "nickname": a.Nickname}
	if err := h.cfg.Upstream.DailyCheckin(a); err != nil {
		if upstream.IsAlreadyCheckin(err) {
			resp["status"] = "already"
		} else {
			resp["status"] = "fail"
			resp["detail"] = err.Error()
			writeJSON(w, http.StatusBadGateway, resp)
			return
		}
	} else {
		resp["status"] = "ok"
	}
	// 签到顺带查余额并回填池：CLI/GUI 都靠这一步「签完立刻看到积分」。
	if remain, err := h.cfg.Upstream.UserResource(a); err == nil {
		h.cfg.Pool.SetCredits(uid, remain)
		h.cfg.Pool.ReenableIfCredits(uid, remain)
		resp["credits"] = remain
	} else {
		resp["credits_error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// adminAuthStart POST /admin/auth/start —— 申请一张扫码登录二维码。
//
// 请求体 {"realm":"cn|global"}（可省，默认 cn）。响应里的 auth_url 直接转二维码即可；
// state 是不透明凭据，交给 /admin/auth/poll 换结果。
func (h *Handler) adminAuthStart(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Login == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "not_implemented", "登录服务未接线")
		return
	}
	var body struct {
		Realm string `json:"realm"`
	}
	if raw, err := io.ReadAll(io.LimitReader(r.Body, adminBodyLimit)); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	state, authURL, err := h.cfg.Login.Start(body.Realm)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state":    state,
		"auth_url": authURL,
		"realm":    normalizeRealmLabel(body.Realm),
		"hint":     "用微信/QQ 扫码或打开 auth_url 完成授权，然后轮询 GET /admin/auth/poll?state=…",
	})
}

// adminAuthPoll GET /admin/auth/poll?state=…&realm=… —— 轮询扫码结果并落盘账号。
//
// 成功即写入 {auth_dir}/workbuddy-<uid>.json（auth.SaveAtomic，原子替换）。
// 池的 auths 目录 watcher 会把新文件自动收进池 —— 这里**不需要**也不应该重启上游。
func (h *Handler) adminAuthPoll(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Login == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "not_implemented", "登录服务未接线")
		return
	}
	q := r.URL.Query()
	cred, pending, err := h.cfg.Login.Poll(q.Get("realm"), q.Get("state"))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "login_error", err.Error())
		return
	}
	if pending {
		writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
		return
	}
	if h.cfg.AuthDir == "" {
		writeOpenAIError(w, http.StatusNotImplemented, "not_implemented",
			"未配置 auth_dir，无法落盘账号")
		return
	}
	if !uidPattern.MatchString(cred.UID) {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"上游返回的 uid 形态非法，已拒绝落盘")
		return
	}
	a := auth.NewFromLogin(cred.UID, cred.EnterpriseID, cred.Nickname,
		cred.AccessToken, cred.RefreshToken, cred.Domain, cred.Realm, cred.ExpiresAt(time.Now()))
	a.FilePath = filepath.Join(h.cfg.AuthDir, "workbuddy-"+cred.UID+".json")
	if err := a.SaveAtomic(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "save_failed",
			"账号文件写入失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "saved",
		"uid":        a.UID,
		"nickname":   a.Nickname,
		"realm":      a.Realm(),
		"expires_at": a.ExpiresAt,
		"file":       a.FilePath,
		"hint":       "账号文件已写入，池会在几秒内热加载（GET /admin/accounts 可见）",
	})
}

// adminModels GET /admin/models —— 模型中心。
//
// 与 /v1/models 的区别：那个是**给下游 SDK 的 OpenAI 兼容清单**（字段形状受协议约束），
// 这个是管理口径 —— 名称、中文描述、上下文、最大输出、思考档位、积分倍率、能力标记、
// 版本归属，供 CLI 选模型/比价用。
func (h *Handler) adminModels(w http.ResponseWriter, r *http.Request) {
	items := h.modelList()
	source := "dynamic"
	if len(items) == 0 {
		source = "empty"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source": source,
		"count":  len(items),
		"data":   items,
	})
}

// adminPlaygroundChat POST /admin/playground/chat —— 聊天测试台。
//
// 直接复用 /v1/chat/completions 的整条链路（payload 净化、指纹改写、轮转、SSE、
// 错误分类全部一致），只多一件事：可用 `?uid=` 指定账号，用于「这个号到底行不行」的
// 定点验证。指定账号仍然要过池可用性校验（冷却/模型限额），不可用会正常 503。
func (h *Handler) adminPlaygroundChat(w http.ResponseWriter, r *http.Request) {
	if uid := strings.TrimSpace(r.URL.Query().Get("uid")); uid != "" {
		r.Header.Set(forceAccountHeader, uid)
	}
	h.chatCompletions(w, r)
}

// normalizeRealmLabel 把入参 realm 归一化成对外展示用的标签（cn/global）。
func normalizeRealmLabel(realm string) string {
	if strings.EqualFold(strings.TrimSpace(realm), "global") {
		return "global"
	}
	return "cn"
}
