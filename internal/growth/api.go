// Package growth 成长中心（growth）任务域：任务列表/接取/领奖、连登、Buddy 领养、
// 活跃上报、真实对话，以及「开学季」「夜猫子」等一次性任务的公共底座。
//
// 移植自上游 scripts/task_common.py（该文件与同目录其它 .py 已删除）。为什么整体
// 搬到 Go：目标形态是单镜像 + Go 二进制、镜像不装 python3；而这些脚本原来是
// 「面板 exec python」的另一套实现，与 internal/upstream 的同类调用来回漂移。
//
// 端点权威口径（与 Go 侧 report.go / travel.go 一致）：
//
//	chat 域  https://copilot.tencent.com
//	  GET  /v2/activity/growth/tasks                任务列表
//	  POST /v2/activity/growth/tasks/accept         接取 {"task_codes":[code]}
//	  POST /activity/growth/tasks/{code}/claim      领奖（路径含 code、无 body）
//	        chat 域 400 时降级 web 域 https://www.workbuddy.cn 同路径 + x-client-platform: web
//	  POST /activity/growth/buddy/agreement|first   领养（幂等）
//	  GET  /activity/growth/streak                  连登天数
//	  POST /v2/chat/completions                     真实对话（强制流式）
//	billing 域  https://www.codebuddy.cn
//	  POST /v2/report                               活跃上报（chat_request_send 事件数组）
package growth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// ChatBase chat 域（growth / tasks / buddy / streak / chat）。
	ChatBase = "https://copilot.tencent.com"
	// BillBase billing 域（report / billing）。
	BillBase = "https://www.codebuddy.cn"

	PathListTasks      = "/v2/activity/growth/tasks"
	PathAcceptTasks    = "/v2/activity/growth/tasks/accept"
	PathClaimReward    = "/activity/growth/tasks" // + "/{code}/claim"
	PathBuddyFirst     = "/activity/growth/buddy/first"
	PathBuddyAgreement = "/activity/growth/buddy/agreement"
	PathStreak         = "/activity/growth/streak"
	PathReport         = "/v2/report"
	PathChat           = "/v2/chat/completions"

	// ClientUA 出站 UA：与官方 CLI 一致，非官方形态会被判成非授权渠道。
	ClientUA = "CLI/2.63.2 CodeBuddy/2.63.2"
)

// Auth 一个账号的调用上下文（脚本里的 auth dict）。
type Auth struct {
	Token  string
	UID    string
	Domain string
	Nick   string
	File   string
	Realm  string

	// ChatBaseOverride / BillBaseOverride 预留覆盖（脚本支持 auth.chat_base）。
	// 正常路径恒空 = 用包级常量；保留是为了让测试/灰度能指到别的基址。
	ChatBaseOverride string
	BillBaseOverride string
}

// UIDPrefixFromFile 从 auths 文件名推导 uid 前缀（脚本里是 basename[10:18]）。
//
// 与脚本的差别：脚本按**字节切片**取 8 位，uid 不足 8 位时会切到 ".json" 的尾巴，
// 于是 `load_auth` 找不到任何文件、报「no auth for 900001.j」——真实 uid 都是长数字
// 所以这个缺陷一直没暴露。这里先剥掉 "workbuddy-" 与 ".json" 再取前 8 个字符，
// 短 uid 也能正确命中；长 uid 的结果与脚本逐字相同（前缀匹配语义不变）。
func UIDPrefixFromFile(base string) string {
	name := strings.TrimSuffix(base, ".json")
	trimmed := strings.TrimPrefix(name, "workbuddy-")
	if trimmed == name {
		// 不认识的命名：不猜，原样返回（调用方会按 uid 前缀去 glob）。
		return base
	}
	r := []rune(trimmed)
	if len(r) > 8 {
		r = r[:8]
	}
	return string(r)
}

// AuthsDir 账号目录：WP_AUTHS > 可执行文件旁的 auths/ > ./auths > /root/wp/auths。
//
// 与脚本的三级回退保持同一顺序；多出来的 ./auths 只是让「在工作目录直接跑」也能用。
func AuthsDir() string {
	if v := strings.TrimSpace(os.Getenv("WP_AUTHS")); v != "" {
		return v
	}
	if exe, err := os.Executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "auths")
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand
		}
	}
	if fi, err := os.Stat("./auths"); err == nil && fi.IsDir() {
		return "./auths"
	}
	return "/root/wp/auths"
}

// LoadAuth 加载账号：uidOrFile 可以是 auths 文件名/绝对路径，也可以是 uid **前缀**
// （与脚本一致：`workbuddy-<前缀>*.json` 的第一个命中）。
func LoadAuth(uidOrFile string) (*Auth, error) {
	var path string
	switch {
	case strings.ContainsAny(uidOrFile, `/\`) || strings.HasSuffix(uidOrFile, ".json"):
		path = uidOrFile
		if !filepath.IsAbs(path) {
			path = filepath.Join(AuthsDir(), path)
		}
	default:
		hits, _ := filepath.Glob(filepath.Join(AuthsDir(), "workbuddy-"+uidOrFile+"*.json"))
		if len(hits) == 0 {
			// 兜底：调用方给的「前缀」可能带着文件名残余（历史脚本用固定切片
			// basename[10:18] 取前缀，uid 短于 8 位时会切到 `.json` 的尾巴）。
			// 这里退一步按「只保留合法文件名字符」的前缀再找一次，避免把
			// 「账号文件名命名特殊」报成「没有这个账号」。
			trimmed := strings.TrimRightFunc(uidOrFile, func(r rune) bool {
				return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r == '-')
			})
			if trimmed != "" && trimmed != uidOrFile {
				hits, _ = filepath.Glob(filepath.Join(AuthsDir(), "workbuddy-"+trimmed+"*.json"))
			}
		}
		if len(hits) == 0 {
			return nil, fmt.Errorf("no auth for %s", uidOrFile)
		}
		sort.Strings(hits)
		path = hits[0]
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Auth struct {
			AccessToken string `json:"accessToken"`
			Domain      string `json:"domain"`
			Realm       string `json:"realm"`
		} `json:"auth"`
		Account struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
		} `json:"account"`
		Realm string `json:"realm"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("解析账号文件 %s: %w", filepath.Base(path), err)
	}
	if strings.TrimSpace(doc.Auth.AccessToken) == "" {
		return nil, fmt.Errorf("账号文件 %s 缺 accessToken", filepath.Base(path))
	}
	realm := doc.Auth.Realm
	if realm == "" {
		realm = doc.Realm
	}
	return &Auth{
		Token:  doc.Auth.AccessToken,
		UID:    doc.Account.UID,
		Domain: doc.Auth.Domain,
		Nick:   doc.Account.Nickname,
		File:   filepath.Base(path),
		Realm:  realm,
	}, nil
}

// IsGlobal 判定 global realm：显式 realm=global 或 domain 后缀 .workbuddy.ai
// （与 Go auth.Realm() 同口径：显式优先、domain 回落）。
func (a *Auth) IsGlobal() bool {
	if strings.EqualFold(strings.TrimSpace(a.Realm), "global") {
		return true
	}
	d := strings.ToLower(strings.TrimSpace(a.Domain))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// Realm 归一化后的域标签（"cn" / "global"）。
func (a *Auth) RealmLabel() string {
	if a.IsGlobal() {
		return "global"
	}
	return "cn"
}

// ChatBaseOf 该账号应使用的 chat 域基址。
func (a *Auth) ChatBaseOf() string {
	if a.ChatBaseOverride != "" {
		return a.ChatBaseOverride
	}
	return ChatBase
}

// BillBaseOf 该账号应使用的 billing 域基址。
func (a *Auth) BillBaseOf() string {
	if a.BillBaseOverride != "" {
		return a.BillBaseOverride
	}
	return BillBase
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// SetHTTPClient 替换底层 client（测试注入假 transport）。
func SetHTTPClient(c *http.Client) {
	if c != nil {
		httpClient = c
	}
}

func (a *Auth) headers() map[string]string {
	h := map[string]string{
		"Authorization": "Bearer " + a.Token,
		"Accept":        "application/json",
		"Content-Type":  "application/json",
		"User-Agent":    ClientUA,
		"Origin":        "https://www.codebuddy.cn",
		"Referer":       "https://www.codebuddy.cn/",
	}
	if a.UID != "" {
		h["X-User-Id"] = a.UID
	}
	if a.Domain != "" {
		h["X-Domain"] = a.Domain
	}
	return h
}

// Request 发一次请求，返回 (HTTP 状态码, 解析后的 JSON)。网络层错误返回 (-1, {"err": ...})。
//
// 刻意**不**把非 2xx 当错误：这些接口的业务语义大量藏在 4xx 的 code/msg 里
// （10001 已签到、14051 已领取、400 走 web 降级），调用方要能读到。
func (a *Auth) Request(method, base, path string, body any, extraHeaders map[string]string) (int, map[string]any) {
	url := path
	if !strings.HasPrefix(path, "http") {
		url = base + path
	}
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return -1, map[string]any{"err": err.Error()}
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return -1, map[string]any{"err": err.Error()}
	}
	for k, v := range a.headers() {
		req.Header.Set(k, v)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return -1, map[string]any{"err": err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return -1, map[string]any{"err": err.Error()}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return resp.StatusCode, map[string]any{"raw": truncate(string(raw), 300)}
	}
	return resp.StatusCode, out
}

// Get 读请求（JSON）。
func (a *Auth) Get(base, path string, extraHeaders map[string]string) (int, map[string]any) {
	return a.Request(http.MethodGet, base, path, nil, extraHeaders)
}

// Post 写请求（body 为 map；nil 表示不带 body）。
func (a *Auth) Post(base, path string, body any, extraHeaders map[string]string) (int, map[string]any) {
	return a.Request(http.MethodPost, base, path, body, extraHeaders)
}

// ListTasks 任务列表（原始 map，字段由上游决定）。
func (a *Auth) ListTasks() ([]map[string]any, error) {
	st, d := a.Get(a.ChatBaseOf(), PathListTasks, nil)
	if st != http.StatusOK {
		return nil, fmt.Errorf("list_tasks http=%d", st)
	}
	data, _ := d["data"].(map[string]any)
	raw, _ := data["tasks"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// TaskStatus 查单个任务；不存在返回 (nil, nil)。
func (a *Auth) TaskStatus(taskCode string) (map[string]any, error) {
	tasks, err := a.ListTasks()
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if code, _ := t["task_code"].(string); code == taskCode {
			return t, nil
		}
	}
	return nil, nil
}

// AcceptTasks 接取任务（not_accepted → accepted），返回 (status, body)。
func (a *Auth) AcceptTasks(codes []string) (int, map[string]any) {
	return a.Post(a.ChatBaseOf(), PathAcceptTasks, map[string]any{"task_codes": codes}, nil)
}

// Streak 连登天数；失败返回 -1（调用方据此记日志，不当作 0 天）。
func (a *Auth) Streak() int {
	st, d := a.Get(a.ChatBaseOf(), PathStreak, nil)
	if st != http.StatusOK {
		return -1
	}
	data, _ := d["data"].(map[string]any)
	streak, _ := data["streak"].(map[string]any)
	switch v := streak["days"].(type) {
	case float64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	}
	return 0
}

// ClaimReward 领奖。chat 域 400 时降级 web 域同路径（带 Origin/Referer/
// x-client-platform: web，与 web 成长中心同款）。重复领是业务错误码，幂等安全。
func (a *Auth) ClaimReward(taskCode string, webFallback bool) (int, map[string]any) {
	path := PathClaimReward + "/" + taskCode + "/claim"
	st, r := a.Post(a.ChatBaseOf(), path, nil, nil)
	if webFallback && st == http.StatusBadRequest {
		webHdr := map[string]string{
			"Authorization":     "Bearer " + a.Token,
			"Accept":            "application/json, text/plain, */*",
			"Content-Type":      "application/json",
			"Origin":            "https://www.workbuddy.cn",
			"Referer":           "https://www.workbuddy.cn/profile/growth-center",
			"x-client-platform": "web",
			"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
				"(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36",
			"X-User-Id": a.UID,
			"X-Domain":  "https://www.workbuddy.cn",
		}
		st, r = a.Post("https://www.workbuddy.cn", path, nil, webHdr)
	}
	return st, r
}

// BuddyAdopt 领养第一只 Buddy：同意协议 → buddy/first（幂等）。
//
// 前置是当日活跃上报；缺口门槛会返回 400 "first_buddy task not completed yet"，
// 调用方按预期跳过即可。返回 buddy/first 的 (status, body)。
func (a *Auth) BuddyAdopt(gap time.Duration) (int, map[string]any) {
	a.Post(a.ChatBaseOf(), PathBuddyAgreement, map[string]any{"agree": true}, nil)
	if gap > 0 {
		time.Sleep(gap)
	}
	return a.Post(a.ChatBaseOf(), PathBuddyFirst, map[string]any{}, nil)
}

// ChatEvent 构造 chat_request_send 事件（形状照抄 report.go）。
//
// 必须带 userId（=账号 uid）：缺了服务端仍回 200，但会**静默丢弃**，
// 于是「上报成功」与「压根没记上」在日志里长得一模一样。
func ChatEvent(a *Auth, conversationID, modelID, modelName, mode string) map[string]any {
	now := time.Now().UnixMilli()
	cid := conversationID
	if cid == "" {
		cid = fmt.Sprintf("task-%d", now)
	}
	if modelID == "" {
		modelID = "deepseek-v4-flash"
	}
	if modelName == "" {
		modelName = "DeepSeek V4 Flash"
	}
	if mode == "" {
		mode = "craft"
	}
	return map[string]any{
		"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
		"mode": mode, "conversationId": cid, "requestId": cid,
		"inputLength": 12, "requestModelId": modelID,
		"requestModelName": modelName, "isPlan": false,
		"isAutoExecuteTerminal": false, "isAutoModify": false,
		"codebaseEnable": false, "maxToken": 0, "maxSteps": 0, "temperature": 0,
		"maxRetries": 0, "mentionContexts": []any{}, "knowledgeId": []any{},
		"knowledgeName": []any{}, "codebaseId": "", "mentionContextCount": 0,
		"command": "", "expertId": "", "recommendId": "", "skillId": "",
		"skillCount": 0, "totalCount": 0, "fileUri": "", "presentAt": now,
		"traceId": "", "rootRequestId": cid, "parentConversationId": cid,
		"agentName": "default", "agentType": "conversation", "userId": a.UID,
	}
}

// ReportActivity 向 {billing}/v2/report 上报 count 条 chat_request_send，间隔 >= gap。
// 返回每条上报的 (status, code)。
func (a *Auth) ReportActivity(count int, gap time.Duration, modelID, modelName, mode string) []ReportResult {
	if count <= 0 {
		return nil
	}
	out := make([]ReportResult, 0, count)
	for i := 0; i < count; i++ {
		ev := ChatEvent(a, "", modelID, modelName, mode)
		st, r := a.Post(a.BillBaseOf(), PathReport, []any{ev}, nil)
		res := ReportResult{Status: st}
		if c, ok := r["code"]; ok {
			res.Code = fmt.Sprint(c)
		}
		out = append(out, res)
		if i < count-1 && gap > 0 {
			time.Sleep(gap)
		}
	}
	return out
}

// ReportResult 单条上报结果。
type ReportResult struct {
	Status int
	Code   string
}

// ChatCompletion 真实对话一次（**强制流式**：服务端只认 stream:true），
// 返回 (status, 首个正文片段)。extraVar 会并进顶层 extra_vars。
func (a *Auth) ChatCompletion(modelID, prompt string, maxTokens int, timeout time.Duration, extraVar map[string]any) (int, string) {
	if modelID == "" {
		modelID = "glm-5.2"
	}
	if prompt == "" {
		prompt = "hi"
	}
	if maxTokens <= 0 {
		maxTokens = 32
	}
	body := map[string]any{
		"model":      modelID,
		"messages":   []any{map[string]any{"role": "user", "content": prompt}},
		"stream":     true,
		"max_tokens": maxTokens,
	}
	if len(extraVar) > 0 {
		merged := map[string]any{}
		body["extra_vars"] = merged
		for k, v := range extraVar {
			merged[k] = v
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return -1, err.Error()
	}
	req, err := http.NewRequest(http.MethodPost, a.ChatBaseOf()+PathChat, bytes.NewReader(raw))
	if err != nil {
		return -1, err.Error()
	}
	for k, v := range a.headers() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "text/event-stream")

	cli := httpClient
	if timeout > 0 {
		cli = &http.Client{Timeout: timeout}
	}
	resp, err := cli.Do(req)
	if err != nil {
		return -1, truncate(err.Error(), 200)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		rawErr, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return resp.StatusCode, truncate(string(rawErr), 200)
	}
	first := ""
	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				idx := bytes.IndexByte(buf, '\n')
				if idx < 0 {
					break
				}
				line := strings.TrimRight(string(buf[:idx]), "\r")
				buf = buf[idx+1:]
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data: "))
				if payload == "" || payload == "[DONE]" {
					continue
				}
				var obj struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if err := json.Unmarshal([]byte(payload), &obj); err != nil {
					continue
				}
				if len(obj.Choices) > 0 && obj.Choices[0].Delta.Content != "" && first == "" {
					first = obj.Choices[0].Delta.Content
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	return resp.StatusCode, first
}

// AccountSpacing 账号之间的随机间隔（第一个账号立即执行）。
//
// 为什么随机而不是固定：固定间隔在风控眼里就是机器节奏，随机化与「人工操作」
// 更接近；上限 600s 是刻意的（再长就是排队事故而不是节流）。
type AccountSpacing struct {
	Min, Max int
	started  bool
}

// NewAccountSpacing 校验参数并构造。非法区间直接报错（0 <= min <= max <= 600）。
func NewAccountSpacing(min, max int) (*AccountSpacing, error) {
	if min < 0 || max < min || max > 600 {
		return nil, fmt.Errorf("account delays require 0 <= min <= max <= 600 seconds")
	}
	return &AccountSpacing{Min: min, Max: max}, nil
}

// Wait 在第二个及以后的账号前等待（打印一行说明，便于日志对齐）。
func (s *AccountSpacing) Wait() {
	if s.started && s.Max > 0 {
		delay := float64(s.Min)
		if s.Max > s.Min {
			delay += rand.Float64() * float64(s.Max-s.Min) //nolint:gosec // 节流用，不需要密码学随机
		}
		fmt.Printf("account spacing: waiting %.1fs\n", delay)
		time.Sleep(time.Duration(delay * float64(time.Second)))
	}
	s.started = true
}

// UID8 账号短标签（日志里与上游日志的 uid8 口径一致）。
func (a *Auth) UID8() string {
	if len(a.UID) >= 8 {
		return a.UID[:8]
	}
	return a.UID
}

// Label 日志标签：昵称(uid8)，与上游日志形态一致。
func (a *Auth) Label() string {
	if a.Nick == "" {
		return a.UID8()
	}
	return a.Nick + "(" + a.UID8() + ")"
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
