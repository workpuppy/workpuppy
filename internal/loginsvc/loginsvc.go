// Package loginsvc 扫码登录（设备授权）流程的服务化版本。
//
// 为什么需要它：`cmd/login` 已经把这条流程跑通了，但它是**进程级 CLI** ——
// state 落在 tmp 文件、结果打印到 stdout、一次只能有一个 pending 登录。
// 管理面（REST/CLI）需要的是：多个并发 pending、state 带 TTL、结果直接拿到
// 结构化凭据。协议细节（端点、请求头、{code,msg,data} 信封）与 `cmd/login`
// **完全一致**，那边是这条链路的参照实现，改一处必须核对另一处：
//
//	POST {base}/v2/plugin/auth/state?platform=CLI   → {state, authUrl}
//	GET  {base}/v2/plugin/auth/token?state=<state>  → {accessToken, refreshToken, expiresIn, domain}
//	GET  {base}/v2/plugin/login/account?state=<state>（带 Bearer）→ {uid, enterpriseId, nickname}
//
// realm 决定 base 与 Origin/Referer：
//
//	cn     → https://copilot.tencent.com（Origin: https://www.codebuddy.cn）
//	global → https://www.workbuddy.ai（Origin: https://www.workbuddy.ai）
//
// state 在服务端只保留「realm + 创建时刻」，**不落盘**：它是短命的一次性凭据，
// 落盘只会制造「重启后还能继续上一次扫码」的错觉，实际腾讯侧早已过期。
package loginsvc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"sync"
	"time"

	"wp/internal/auth"
)

const (
	realmCN     = "cn"
	realmGlobal = "global"

	baseCN     = "https://copilot.tencent.com"
	baseGlobal = "https://www.workbuddy.ai"
	originCN   = "https://www.codebuddy.cn"
	originURL  = "https://www.workbuddy.ai"

	clientUA = "CLI/2.63.2 CodeBuddy/2.63.2"

	// defaultTTL state 的有效期。与面板侧此前 300s 的做法不同，这里给足 15 分钟：
	// 手机号 + 短信验证码登录经常超过 5 分钟，过期就白白让用户重扫一次二维码。
	defaultTTL = 15 * time.Minute
	// maxPending 同时挂起的登录数上限（防滥用把内存当存储用）。
	maxPending = 32
)

// ErrStateExpired 扫码 state 不存在或已过期（超过 TTL）。
//
// 单独做成哨兵错误而不是普通字符串：管理后台要据此把二维码标成"已失效"并停止轮询，
// 而其它错误（上游 5xx、realm 不匹配）应该如实报成失败、提示用户重试。
var ErrStateExpired = errors.New("授权二维码已过期或不存在，请重新获取")

// Credentials 是登录成功后的结构化结果。
type Credentials struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterprise_id"`
	Nickname     string `json:"nickname"`
	AccessToken  string `json:"-"`
	RefreshToken string `json:"-"`
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
	ExpiresIn    int64  `json:"expires_in"`
}

// ExpiresAt 把上游给的 expiresIn（秒）换算成绝对时刻。
//
// expiresIn 缺失（0）时回落 1 小时：这个值只用于「还剩多久」的展示与提前刷新
// 判断，宁可报得保守一些，也不要写 0 让下游以为 token 一出生就过期。
func (c *Credentials) ExpiresAt(now time.Time) int64 {
	ttl := c.ExpiresIn
	if ttl <= 0 {
		ttl = 3600
	}
	return now.Add(time.Duration(ttl) * time.Second).Unix()
}

type entry struct {
	realm   string
	created time.Time
}

// Manager 管理挂起中的扫码登录。零值不可用，用 New 构造。
type Manager struct {
	mu     sync.Mutex
	states map[string]entry
	client *http.Client
	ttl    time.Duration
	now    func() time.Time // 测试注入时钟

	// baseCN / baseGlobal 上游地址。默认取包内常量；测试用 SetBases 指到 httptest。
	baseCN     string
	baseGlobal string
}

// New 构造一个 Manager。每个 Manager 自带 cookie jar（多账号登录互不串会话）。
func New() *Manager {
	jar, _ := cookiejar.New(nil)
	return &Manager{
		states:     make(map[string]entry),
		client:     &http.Client{Timeout: 30 * time.Second, Jar: jar},
		ttl:        defaultTTL,
		now:        time.Now,
		baseCN:     baseCN,
		baseGlobal: baseGlobal,
	}
}

// SetBases 覆盖两个 realm 的上游基址（仅测试用；空串忽略）。
// 生产路径不接受外部注入地址：登录端点由腾讯域名决定，可配置化只会制造
// 「指向了错误域名却看不出来」的故障面。
func (m *Manager) SetBases(cn, global string) {
	if cn != "" {
		m.baseCN = cn
	}
	if global != "" {
		m.baseGlobal = global
	}
}

// TTL 返回当前 state 有效期（管理后台据此显示"有效期剩余"倒计时）。
func (m *Manager) TTL() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ttl
}

// SetTTL 覆盖 state 有效期（配置项 admin.login_ttl；<=0 忽略）。
func (m *Manager) SetTTL(d time.Duration) {
	if d > 0 {
		m.ttl = d
	}
}

// SetClock 替换时钟（测试用；nil 恢复 time.Now）。
func (m *Manager) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	m.now = now
}

// realmOrigin 归一化 realm 并返回它的 Origin（请求头用）。非法/空 → cn（与 cmd/login 同意）。
func realmOrigin(realm string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(realm)) {
	case realmGlobal:
		return realmGlobal, originURL
	default:
		return realmCN, originCN
	}
}

// baseFor 取该 realm 的上游基址（可被 SetBases 覆盖）。
func (m *Manager) baseFor(realm string) string {
	if realm == realmGlobal {
		return m.baseGlobal
	}
	return m.baseCN
}

// headers 按 origin 构造通用请求头（与 cmd/login.commonHeaders 逐项一致）。
func headers(origin string) func(*http.Request) {
	return func(req *http.Request) {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		req.Header.Set("Origin", origin)
		req.Header.Set("Referer", origin+"/")
		req.Header.Set("User-Agent", clientUA)
	}
}

// envelope 上游 {code,msg,data} 业务信封。
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doJSON 发一次请求并解信封；code!=0 视为业务错误（pending 也走这条路径）。
func (m *Manager) doJSON(method, url string, hdr func(*http.Request), body []byte) (json.RawMessage, int, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, 0, err
	}
	hdr(req)
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, fmt.Errorf("http_error: upstream %d", resp.StatusCode)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse failed: %w", err)
	}
	if env.Code != 0 {
		return nil, resp.StatusCode, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return env.Data, resp.StatusCode, nil
}

// Start 申请一次扫码登录，返回 (state, authURL)。调用方把 authURL 变成二维码。
func (m *Manager) Start(realm string) (string, string, error) {
	realm, origin := realmOrigin(realm)
	base := m.baseFor(realm)
	m.mu.Lock()
	m.pruneLocked()
	if len(m.states) >= maxPending {
		m.mu.Unlock()
		return "", "", fmt.Errorf("待登录的二维码过多（%d 个），请先完成或稍后再试", maxPending)
	}
	m.mu.Unlock()

	data, _, err := m.doJSON(http.MethodPost, base+"/v2/plugin/auth/state?platform=CLI",
		headers(origin), []byte("{}"))
	if err != nil {
		return "", "", fmt.Errorf("auth state failed: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return "", "", fmt.Errorf("auth state: 上游未返回 state/authUrl")
	}
	m.mu.Lock()
	m.states[st.State] = entry{realm: realm, created: m.now()}
	m.mu.Unlock()
	return st.State, st.AuthURL, nil
}

// Poll 查询一次登录结果。
//
// 三态返回：
//   - pending=true, err=nil：还没扫/还没确认，调用方继续轮询；
//   - cred!=nil：登录成功（state 已消费，不能重复 poll）；
//   - err!=nil：真错误（state 未知/过期、上游故障）。
func (m *Manager) Poll(realm, state string) (cred *Credentials, pending bool, err error) {
	state = strings.TrimSpace(state)
	if state == "" {
		return nil, false, fmt.Errorf("state 必填")
	}
	wantRealm, _ := realmOrigin(realm)
	m.mu.Lock()
	e, ok := m.states[state]
	m.mu.Unlock()
	if !ok {
		return nil, false, ErrStateExpired
	}
	if e.realm != wantRealm {
		// 防混域：cn 的 state 拿去 global 轮询只会拿到一句看不懂的上游错误。
		return nil, false, fmt.Errorf("realm 不匹配：该二维码属于 %s", e.realm)
	}
	if m.now().Sub(e.created) > m.ttl {
		m.forget(state)
		return nil, false, ErrStateExpired
	}

	_, origin := realmOrigin(wantRealm)
	base := m.baseFor(wantRealm)
	data, status, err := m.doJSON(http.MethodGet, base+"/v2/plugin/auth/token?state="+state,
		headers(origin), nil)
	if err != nil {
		// 4xx（含 pending 的 code!=0）是「还没登录完成」，不是故障；
		// 5xx/传输层错误才是真错误 —— 与 cmd/login 的判据一致。
		if status == 0 || status >= 500 {
			return nil, false, fmt.Errorf("token endpoint error: %w", err)
		}
		return nil, true, nil
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return nil, true, nil
	}

	// 账号信息是**尽力而为**：拿不到 uid 时这条凭据没法落盘成账号文件，
	// 所以这里失败即视为登录未完成（让调用方重试，而不是写一个无主凭证）。
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	acctHdr := func(r *http.Request) {
		headers(origin)(r)
		r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	}
	acctRaw, _, errAcct := m.doJSON(http.MethodGet, base+"/v2/plugin/login/account?state="+state, acctHdr, nil)
	if errAcct != nil {
		return nil, true, nil
	}
	_ = json.Unmarshal(acctRaw, &acct)
	if strings.TrimSpace(acct.UID) == "" {
		return nil, true, nil
	}

	m.forget(state) // 一次性：拿到凭据后 state 立即作废，防重放
	return &Credentials{
		UID:          acct.UID,
		EnterpriseID: acct.EnterpriseID,
		Nickname:     acct.Nickname,
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		Domain:       tok.Domain,
		Realm:        auth.ResolveRealm(wantRealm, tok.Domain),
		ExpiresIn:    tok.ExpiresIn,
	}, false, nil
}

// PendingCount 当前挂起的 state 数（供 /admin/auth/start 的响应与测试观察）。
func (m *Manager) PendingCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked()
	return len(m.states)
}

func (m *Manager) forget(state string) {
	m.mu.Lock()
	delete(m.states, state)
	m.mu.Unlock()
}

// pruneLocked 清掉过期 state。调用方需持有 m.mu。
//
// 为什么要主动清：state 只在 Poll 时被判过期，而「扫了不确认就关页面」是最常见的
// 用法 —— 不清就会一直占着 maxPending 的名额。
func (m *Manager) pruneLocked() {
	now := m.now()
	for k, v := range m.states {
		if now.Sub(v.created) > m.ttl {
			delete(m.states, k)
		}
	}
}
