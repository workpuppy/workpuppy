// Package region 国际版（global realm）注册地区完善与 trial 领取。
//
// 移植自上游 scripts/global_region.py（该脚本已删除），协议逆向自注册完善页面的
// web chunk `RegisterRegion-*.js`（codebuddy.ai/login）：
//
//	POST /billing/area/get-country-code     {filterForbidden:1} → 国家列表
//	     （响应 data 是**JSON 字符串**，内层才是 {code,data:{list:[…]}}）
//	POST /billing/area/get-user-area-info   {action:"getUserAreaInfo"} → 当前地区
//	POST /console/login/account             {"attributes":{countryCode:[Code],
//	                                          countryFullName:[EnName],countryName:[IOS2]}}
//	GET  /auth/realms/copilot/overseas/user/register?userId=<uid>
//	     → 激活查询（code:200 成功；code:500 / "region required" 表示缺地区）
//	POST /billing/ide/trial                 → 一次性 trial 加油包（幂等码 14051）
//
// 展示集沿用 web 国际版白名单（HK/MO/SG/TH/PH/MY/ID），拉取失败时调用方回退全量。
//
// 为什么要 Go 化而不是继续调 python：Docker 镜像不再安装 python3（目标形态是
// 单镜像 + Go 二进制），而 `login.sh` 的 global 分支依赖这条链路。
package region

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Base 国际版 web 站点基址（billing 与 register 都在同一域）。
const Base = "https://www.workbuddy.ai"

// IntlCodes 国际版 web 端可见的地区白名单，顺序即展示顺序。
var IntlCodes = []string{"HK", "MO", "SG", "TH", "PH", "MY", "ID"}

// UA 与 web 端一致：这条链路的服务端会看客户端形态，用 CLI UA 会被判成非官方渠道。
const UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// Country 单个可选地区。Code 在接口里是数字，但不同地区可能给字符串，
// 这里保留原始 JSON 由 CodeString 统一取值。
type Country struct {
	EnName string          `json:"EnName"`
	Name   string          `json:"Name"`
	IOS2   string          `json:"IOS2"`
	IOS3   string          `json:"IOS3"`
	Code   json.RawMessage `json:"Code"`
}

// CodeString 返回提交接口要的 Code 文本（去引号）。
func (c Country) CodeString() string {
	s := strings.TrimSpace(string(c.Code))
	s = strings.Trim(s, `"`)
	if s == "" || s == "null" {
		return ""
	}
	return s
}

// envelope 上游 {code,msg,data} 信封（data 可能是对象，也可能是 JSON 字符串）。
type envelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// inner 把 data（可能是 JSON 字符串）解成目标结构。
func (e envelope) inner(v any) error {
	raw := bytes.TrimSpace(e.Data)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		raw = []byte(s)
	}
	return json.Unmarshal(raw, v)
}

// client 统一超时与头（与脚本一致：20s、同域 Origin/Referer）。
type client struct {
	http  *http.Client
	token string
	uid   string
	base  string
}

func newClient(base, token, uid string) *client {
	if strings.TrimSpace(base) == "" {
		base = Base
	}
	return &client{
		http:  &http.Client{Timeout: 20 * time.Second},
		token: token,
		uid:   uid,
		base:  strings.TrimRight(base, "/"),
	}
}

func (c *client) do(method, path string, body any) (*envelope, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", UA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", Base)
	req.Header.Set("Referer", Base+"/")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.uid != "" && strings.HasPrefix(path, "/auth/realms/") {
		// register 端点要 X-User-Id（web 端同形）；其余端点带上也无害。
		req.Header.Set("X-User-Id", c.uid)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, err
	}
	var env envelope
	unmarshalErr := json.Unmarshal(raw, &env)
	if resp.StatusCode >= 400 {
		// 4xx 也常带业务信封（如 trial 的 14051、register 的 500 region required）：
		// 能解就往上带，解不出才当 HTTP 错误。
		if unmarshalErr == nil {
			return &env, raw, &HTTPError{Status: resp.StatusCode, Env: &env, Raw: raw}
		}
		return nil, raw, &HTTPError{Status: resp.StatusCode, Raw: raw}
	}
	if unmarshalErr != nil {
		return nil, raw, fmt.Errorf("响应不是 JSON（HTTP %d）：%s", resp.StatusCode, truncate(string(raw), 120))
	}
	return &env, raw, nil
}

// HTTPError 保留状态码与已解析信封，供 trial/register 做幂等判定。
type HTTPError struct {
	Status int
	Env    *envelope
	Raw    []byte
}

func (e *HTTPError) Error() string {
	if e.Env != nil && e.Env.Msg != "" {
		return fmt.Sprintf("http %d: %s", e.Status, e.Env.Msg)
	}
	return fmt.Sprintf("http %d: %s", e.Status, truncate(string(e.Raw), 120))
}

// FetchCountries 拉取可选地区。intlOnly=true 时按国际版白名单过滤并按白名单顺序排列。
func FetchCountries(base string, intlOnly bool) ([]Country, error) {
	c := newClient(base, "", "")
	env, _, err := c.do(http.MethodPost, "/billing/area/get-country-code", map[string]any{"filterForbidden": 1})
	if err != nil {
		return nil, err
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	var payload struct {
		Data struct {
			List []Country `json:"list"`
		} `json:"data"`
	}
	if err := env.inner(&payload); err != nil {
		return nil, fmt.Errorf("解析地区列表失败：%w", err)
	}
	all := payload.Data.List
	if !intlOnly {
		return all, nil
	}
	byIOS2 := make(map[string]Country, len(all))
	for _, c := range all {
		if c.IOS2 != "" {
			byIOS2[c.IOS2] = c
		}
	}
	out := make([]Country, 0, len(IntlCodes))
	for _, code := range IntlCodes {
		if c, ok := byIOS2[code]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// DetectRegion 检测当前注册地区，返回 (IOS2, EnName)。未设置时返回空串。
func DetectRegion(base, token string) (string, string, error) {
	c := newClient(base, token, "")
	env, _, err := c.do(http.MethodPost, "/billing/area/get-user-area-info",
		map[string]any{"action": "getUserAreaInfo"})
	if err != nil {
		return "", "", err
	}
	if env.Code != 0 {
		return "", "", fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	var payload struct {
		Data struct {
			IOS2   string `json:"IOS2"`
			EnName string `json:"enName"`
		} `json:"data"`
	}
	if err := env.inner(&payload); err != nil {
		return "", "", err
	}
	return payload.Data.IOS2, payload.Data.EnName, nil
}

// SubmitRegion 提交注册地区（实测幂等）。
func SubmitRegion(base, token string, pick Country) error {
	c := newClient(base, token, "")
	code := pick.CodeString()
	if code == "" {
		return fmt.Errorf("地区 %s 缺少 Code 字段", pick.IOS2)
	}
	body := map[string]any{"attributes": map[string]any{
		"countryCode":     []string{code},
		"countryFullName": []string{pick.EnName},
		"countryName":     []string{pick.IOS2},
	}}
	env, _, err := c.do(http.MethodPost, "/console/login/account", body)
	if err != nil {
		return err
	}
	if env.Code != 0 {
		return fmt.Errorf("code=%d msg=%s", env.Code, env.Msg)
	}
	return nil
}

// registerResult 注册激活查询的归一化结果。
type registerResult struct {
	OK          bool
	NeedsRegion bool
	Msg         string
}

// Activate 查询/激活国际版账号的 register 状态。
func Activate(base, token, uid string) registerResult {
	c := newClient(base, token, uid)
	env, raw, err := c.do(http.MethodGet,
		"/auth/realms/copilot/overseas/user/register?userId="+urlQueryEscape(uid), nil)
	if err != nil {
		// 4xx/5xx 带信封时按信封判定（region required 就藏在 500 里）。
		var he *HTTPError
		if asHTTPError(err, &he) && he.Env != nil {
			env = he.Env
		} else {
			return registerResult{Msg: err.Error()}
		}
	}
	if env == nil {
		return registerResult{Msg: truncate(string(raw), 120)}
	}
	if env.Code == 200 {
		return registerResult{OK: true, Msg: "register success"}
	}
	msg := env.Msg
	if msg == "" {
		msg = fmt.Sprintf("code=%d", env.Code)
	}
	if env.Code == 500 || strings.Contains(strings.ToLower(msg), "region required") {
		return registerResult{NeedsRegion: true, Msg: msg}
	}
	return registerResult{Msg: msg}
}

// TrialResult trial 领取结果。
type TrialResult struct {
	OK      bool
	Already bool
	Msg     string
}

// ClaimTrial 领取一次性 trial 加油包（幂等码 14051 → Already=true）。
func ClaimTrial(base, token string) TrialResult {
	c := newClient(base, token, "")
	env, _, err := c.do(http.MethodPost, "/billing/ide/trial", map[string]any{})
	if err != nil {
		var he *HTTPError
		if asHTTPError(err, &he) {
			if he.Env != nil && strings.Contains(he.Env.Msg, "14051") {
				return TrialResult{OK: true, Already: true, Msg: "已领取"}
			}
			if strings.Contains(string(he.Raw), "14051") {
				return TrialResult{OK: true, Already: true, Msg: "已领取"}
			}
			return TrialResult{Msg: err.Error()}
		}
		return TrialResult{Msg: err.Error()}
	}
	if env.Code == 0 {
		return TrialResult{OK: true, Msg: "ok"}
	}
	// 幂等码 14051 可能在 code 也可能在 msg 里（上游两种形态都出现过）：
	// 与脚本的 `"14051" in json.dumps(body)` 等价，两处都查。
	if env.Code == 14051 || strings.Contains(env.Msg, "14051") {
		return TrialResult{OK: true, Already: true, Msg: "已领取"}
	}
	return TrialResult{Msg: fmt.Sprintf("code=%d msg=%s", env.Code, env.Msg)}
}

// CompleteFlow 整体完善流程：提交地区 → 重新 register 验证。
// callRegister=true 时先查一次 register（缺地区才继续）。
func CompleteFlow(base, token, uid string, pick *Country, callRegister bool) error {
	if callRegister {
		r := Activate(base, token, uid)
		if r.OK {
			return nil
		}
		if !r.NeedsRegion {
			return fmt.Errorf("register 失败：%s", r.Msg)
		}
	}
	if pick == nil {
		return fmt.Errorf("需完善注册地区，但未提供选择")
	}
	if err := SubmitRegion(base, token, *pick); err != nil {
		return fmt.Errorf("提交地区失败：%w", err)
	}
	if r := Activate(base, token, uid); !r.OK {
		return fmt.Errorf("提交地区后 register 仍失败：%s（needs_region=%v）", r.Msg, r.NeedsRegion)
	}
	return nil
}

// PickByIOS2 按 IOS2 找地区；找不到返回 nil（供 --region 直选与自动判定）。
func PickByIOS2(countries []Country, ios2 string) *Country {
	want := strings.ToUpper(strings.TrimSpace(ios2))
	for i := range countries {
		if strings.ToUpper(countries[i].IOS2) == want {
			return &countries[i]
		}
	}
	return nil
}

// InteractiveMenu 终端编号菜单，返回选中的地区；回车/非交互环境返回 nil。
//
// 为什么要读 /dev/tty：login.sh 用 heredoc 驱动时 stdin 是脚本本身，直接读 stdin
// 会立刻 EOF。这里优先看 stdin 是不是终端，不是就回退 /dev/tty（无 tty 则视为取消）。
func InteractiveMenu(countries []Country, detectedIOS2, title string) *Country {
	if len(countries) == 0 {
		fmt.Println("（无可选地区）")
		return nil
	}
	if title == "" {
		title = "国际版账号需完善注册地区"
	}
	fmt.Printf("\n%s（请输入编号选择注册地区）：\n", title)
	for i, c := range countries {
		mark := ""
		if strings.EqualFold(c.IOS2, detectedIOS2) {
			mark = "  ◀ 检测到当前地区"
		}
		fmt.Printf("%3d) %-4s %-8s %s%s\n", i+1, c.IOS2, c.CodeString(), c.EnName, mark)
	}
	line, ok := readLine("\n请选择注册地区编号（回车取消）: ")
	if !ok {
		return nil
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}
	idx, err := strconv.Atoi(line)
	if err != nil || idx < 1 || idx > len(countries) {
		fmt.Printf("无效编号 %s\n", line)
		return nil
	}
	return &countries[idx-1]
}

// asHTTPError 判定并取出 *HTTPError（避免调用方到处 errors.As 样板）。
func asHTTPError(err error, dst **HTTPError) bool {
	for err != nil {
		if he, ok := err.(*HTTPError); ok {
			*dst = he
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// urlQueryEscape 只用于 uid（字母数字/下划线/连字符），保持与脚本的朴素拼接一致。
func urlQueryEscape(s string) string {
	if s == "" {
		return ""
	}
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[ch>>4])
			b.WriteByte(hex[ch&0x0f])
		}
	}
	return b.String()
}

// ReadLineFromTTY 供 CLI 复用：优先 /dev/tty，其次 stdin（有终端时）。
func readLine(prompt string) (string, bool) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err == nil {
		defer tty.Close()
		fmt.Fprint(tty, prompt)
		buf := make([]byte, 0, 64)
		one := make([]byte, 1)
		for {
			n, rerr := tty.Read(one)
			if n == 0 || rerr != nil || one[0] == '\n' {
				break
			}
			if one[0] != '\r' {
				buf = append(buf, one[0])
			}
		}
		return strings.TrimRight(string(buf), "\r\n"), true
	}
	// 没有 /dev/tty（非 Linux 或容器内无 tty）：退回 stdin，非交互时读到 EOF 视为取消。
	fmt.Print(prompt)
	buf := make([]byte, 0, 64)
	one := make([]byte, 1)
	for {
		n, rerr := os.Stdin.Read(one)
		if n == 0 || rerr != nil {
			if len(buf) == 0 {
				return "", false
			}
			break
		}
		if one[0] == '\n' {
			break
		}
		if one[0] != '\r' {
			buf = append(buf, one[0])
		}
	}
	return string(buf), true
}
