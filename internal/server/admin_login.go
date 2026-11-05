// admin_login.go 管理后台的登录与凭据校验。
//
// 与 /v1 彻底分离：`/v1`、`/status` 继续用 config.json 的 `api_key`（发给下游客户端
// 的那把），管理面用**另一套凭据**——进程启动时随机生成、只存内存、打印到终端。
// 这样「下游手里的 key」不再附带停号/加号/看全量账号的权限。
//
// 三条硬约束（都有测试钉住）：
//  1. 凭据未初始化时，管理面**一律拒绝**（503），绝不沿用「api_key 为空 = 不鉴权」；
//  2. 管理端点**只认** adminauth.Store，拿 api_key 打 /admin/* 必须 401；
//  3. 登录失败要有限速（内存窗口），避免把终端日志里那串密码跑成在线爆破靶子。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// adminLoginBody 登录请求体：只需要密码（不要用户名）。
type adminLoginBody struct {
	Password string `json:"password"`
}

// adminLoginWindow / adminLoginMaxFails 登录失败的限速窗口与阈值（按来源 IP）。
//
// 为什么按 IP 而不是全局：全局锁会让一个人试错就把所有运维锁在门外；按 IP
// 则各自计数。密码是 20 字符随机串，在线爆破本就不现实，这里只是关掉脚本化
// 撞库这类噪音。
const (
	adminLoginWindow   = 5 * time.Minute
	adminLoginMaxFails = 10
)

// adminLoginLimiter 记录每个 IP 在窗口内的失败次数（进程内存，重启即清）。
type adminLoginLimiter struct {
	mu   sync.Mutex
	fail map[string][]time.Time
}

func (l *adminLoginLimiter) blocked(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	kept := l.prune(ip, now)
	return len(kept) >= adminLoginMaxFails
}

func (l *adminLoginLimiter) noteFail(ip string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fail[ip] = append(l.prune(ip, now), now)
	// 表本身也要有上限：被扫时 IP 是无限的，不清理会把内存吃满。
	if len(l.fail) > 4096 {
		for k, v := range l.fail {
			if len(v) == 0 || now.Sub(v[len(v)-1]) > adminLoginWindow {
				delete(l.fail, k)
			}
		}
	}
}

func (l *adminLoginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fail, ip)
}

// prune 丢掉窗口外的失败记录（调用方需持锁）。
func (l *adminLoginLimiter) prune(ip string, now time.Time) []time.Time {
	old := l.fail[ip]
	kept := old[:0]
	for _, t := range old {
		if now.Sub(t) <= adminLoginWindow {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fail, ip)
		return nil
	}
	l.fail[ip] = kept
	return kept
}

// adminClientIP 取来源 IP。这里**不信任任何转发头**：管理面登录是爆破面，
// 宁可把反代后面的所有请求都算作同一个 IP（反代自己会传真实 IP 到日志里），
// 也不接受客户端自报地址。
func adminClientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return "unknown"
	}
	return host
}

// adminLogin POST /admin/login —— 用密码换"可以进后台"的确认。
//
// 说明：这里**不发 cookie、不发 token**。前端把密码存本地，后续每个请求用它做
// Bearer 校验（无会话 = 无 CSRF 面，也就不用处理 Cookie 的 SameSite/CSRF 那一套）。
// 登录端点存在的意义有两个：① 让前端在进入页面前先验一次密码，错密码不落到
// 空白界面上；② 给失败加限速与明确文案。
func (h *Handler) adminLogin(w http.ResponseWriter, r *http.Request) {
	if !h.cfg.AdminAuth.Configured() {
		writeOpenAIError(w, http.StatusServiceUnavailable, "admin_auth_unconfigured",
			"管理密码未初始化：服务启动异常，请查看容器日志并重启")
		return
	}
	ip := adminClientIP(r)
	now := time.Now()
	if h.adminLoginLimit.blocked(ip, now) {
		writeOpenAIError(w, http.StatusTooManyRequests, "too_many_attempts",
			"密码错误次数过多，请 5 分钟后再试")
		return
	}

	var body adminLoginBody
	raw, err := io.ReadAll(io.LimitReader(r.Body, adminBodyLimit))
	if err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	if !h.cfg.AdminAuth.Verify(body.Password) {
		h.adminLoginLimit.noteFail(ip, now)
		// 文案刻意不区分"空/错"：不给探测者任何"密码存在但不对"的额外信息。
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_admin_password",
			"密码错误（密码在容器启动日志里，重启会更换）")
		return
	}
	h.adminLoginLimit.reset(ip)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"issued": h.cfg.AdminAuth.IssuedAt().Format(time.RFC3339),
	})
}

// withAdminAuth 管理端点的鉴权：只认 adminauth.Store 里的密码。
//
// 注意与 withAuth（数据面）的区别：那个在 api_key 为空时**放行**（上游既有语义：
// 不设 key 就是不鉴权）；这里恰恰相反 —— 未配置凭据一律拒绝。管理面能停号、
// 能加号、能看全量账号，任何"漏配即裸奔"的默认值都不可接受。
func (h *Handler) withAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.cfg.AdminAuth.Configured() {
			writeOpenAIError(w, http.StatusServiceUnavailable, "admin_auth_unconfigured",
				"管理密码未初始化：服务启动异常，请查看容器日志并重启")
			return
		}
		authz := r.Header.Get("Authorization")
		provided := strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
		if !strings.HasPrefix(authz, "Bearer ") || !h.cfg.AdminAuth.Verify(provided) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_admin_password",
				"管理密码错误或未提供（密码在容器启动日志里）")
			return
		}
		next(w, r)
	}
}

// adminStatus GET /admin/status —— 与 /status 同形，但走管理凭据。
//
// 为什么单独开一个：/status 是**数据面**接口（下游用 api_key 调，做自己的监控），
// 管理后台不该为了看池子而去拿下游密钥。两者共用同一份 payload 构造，口径不会漂。
func (h *Handler) adminStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.statusPayload())
}
