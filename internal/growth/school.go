// 开学季活动（2026「开学季」）任务自动化 —— 逐功能移植自
// scripts/school_open_day_2026.py（腾讯 CodeBuddy 小程序活动任务脚本）。
//
// 为什么单独一个文件而不并进 api.go：api.go 是 growth 域的**公共底座**
// （任务列表/接取/领奖/连登/Buddy/活跃上报/真实对话），被多个二进制共用；
// 开学季的端点、请求头、完成判据全是「一次活动的抓包口径」，塞进底座会让
// 公共调用形状被活动噪声污染。底座能直接用的（Auth / AuthsDir / LoadAuth /
// httpClient / PathReport / AccountSpacing）一律复用；缺的底层能力（微信小程序
// UA 头、/v2/report 的数组体、v4 UUID、与 python 取值语义对齐的工具）写在本文件，
// 且全部以 school 前缀命名，避免与同一 package 下的其它文件（如 tasks.go，
// 另一路 task_runner 移植）撞名。
//
// 关键「为什么」（抓包结论，别简化）：
//   - 请求头只发 4 个：Authorization/Accept/Content-Type/User-Agent(=微信 UA)。
//     api.go 的 headers() 是 CLI/web 口径（带 Origin/Referer/X-User-Id/X-Domain），
//     混进小程序域会被判成非小程序来源，所以这里另建 schoolHeaders。
//   - 上报事件必须带 activityId=school_open_day_2026，否则服务端不关联任务
//     （HTTP 200 但静默丢弃，日志里和成功长得一模一样）。
//   - 桌面对话 6 连必须**整体**上报且发 copilot.tencent.com 域；发 codebuddy.cn
//     不点亮桌面任务（抓包对比过，不是同一个聚合入口）。
//   - 写动作（viewed/share-complete/report/claim/draw）默认 dry-run，
//     只有 --yes 放行；写动作间隔 ≥1s（gap 下限 1.0）。
//   - student-verify（学生认证）是人工环节，脚本不碰、不伪造。
//
// 函数映射（python -> go）：
//
//	derive_id                     -> schoolDeriveID
//	mask                          -> 未移植（python 侧定义后从未调用）
//	_build_headers                -> schoolHeaders
//	_parse_response               -> schoolDo（内联）
//	_request / request_retry      -> schoolDo / schoolRetry
//	_interpret_failure            -> schoolInterpretFailure
//	fetch_school_tasks            -> (*Auth).SchoolTasks
//	fetch_profile                 -> (*Auth).SchoolProfile
//	list_account                  -> ListAccount
//	post_viewed                   -> (*Auth).schoolViewed
//	post_share_complete           -> (*Auth).schoolShareComplete
//	post_claim                    -> (*Auth).schoolClaim
//	fetch_lottery_config          -> (*Auth).SchoolLotteryConfig
//	post_lottery_draw             -> (*Auth).SchoolLotteryDraw
//	lottery_prize_text            -> schoolPrizeText
//	report_events                 -> schoolSendReportEvents
//	fetch_school_expert           -> schoolFetchExpert
//	expert_actual_use_event       -> schoolExpertUseEvent
//	make_expert_event             -> schoolMakeExpertEvent（并入 schoolMakeReportEvents）
//	mini_chat_event               -> schoolMiniChatEvent
//	desktop_fingerprint           -> schoolDesktopFingerprint
//	desktop_chat_sequence         -> schoolDesktopChatSequence
//	make_report_events            -> schoolMakeReportEvents
//	run_account                   -> RunAccount
//	lottery_account               -> LotteryAccount
//	print_notes                   -> PrintNotes
//	main                          -> cmd/school/main.go
package growth

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// SchoolCloudAgent 小程序域（脚本 CLOUD_AGENT = getCloudAgentEndpoint() 的观测值）。
	SchoolCloudAgent = "https://www.codebuddy.cn"
	// SchoolCopilot 桌面上报域（growth/chat 域）。桌面 6 连发这里才点亮。
	SchoolCopilot = "https://copilot.tencent.com"

	// SchoolActivityID 活动事件里 activityId 字段的值：服务端靠它把事件
	// 关联到 school 活动；缺了就是「200 但静默丢弃」。
	SchoolActivityID = "school_open_day_2026"

	// SchoolMPUA 小程序 UA：微信注入的真实形态（MiniProgramEnv + xweb）。
	SchoolMPUA = "Mozilla/5.0 (Linux; Android 14; MicroMessenger/8.0.49 WeChat/0.8.0 " +
		"MiniProgramEnv/android; wkbrowser xweb)"
	// SchoolDesktopUA 桌面上报 UA：桌面事件必须用桌面 UA，小程序 UA 会被判来源不符。
	SchoolDesktopUA = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"

	// schoolPath 开学季活动根路径；freshman/teacher 是同一批活动下的两个 profile 子活动。
	schoolPath   = "/portal/activity/school"
	freshmanPath = "/portal/activity/freshman"
	teacherPath  = "/portal/activity/teacher"

	// schoolExpertCategoryID 开学季专家分类 id。带数字前缀，list API 不做归一化，
	// 写成 "BackToSchool" 会拉到空列表。
	schoolExpertCategoryID = "16-BackToSchool"

	// schoolRetries 网络/5xx 重试次数（脚本 request_retry 的 retries=3）。
	schoolRetries = 3
	// schoolRetryGap 退避基数：第 i 次重试前睡 gap*i（与脚本一致）。
	schoolRetryGap = time.Second
)

// ---------------------------------------------------------------------------
// 错误类型（脚本 AuthError / TransientError；两者在 python 都是 RuntimeError 子类，
// 因此上层统一按「记 fail 并继续下一个账号」处理）
// ---------------------------------------------------------------------------

// SchoolAuthError 鉴权/未绑定微信类硬错误：重试无意义（token 只有小程序能静默刷新）。
type SchoolAuthError struct{ Msg string }

func (e *SchoolAuthError) Error() string { return e.Msg }

// SchoolTransientError 网络/5xx 可重试错误，重试耗尽后作为最终错误抛出。
type SchoolTransientError struct{ Msg string }

func (e *SchoolTransientError) Error() string { return e.Msg }

// SchoolError 普通运行时错误（接口返回业务失败码、专家列表不可用等）。
type SchoolError struct{ Msg string }

func (e *SchoolError) Error() string { return e.Msg }

// ---------------------------------------------------------------------------
// 与 python 取值语义对齐的小工具
// ---------------------------------------------------------------------------

// schoolStr 把 JSON 值渲染成 python str() 的样子（None/True/False/整数值浮点）。
// 为什么要对齐：进度行是给运维看的中文日志，python 侧会打印 None/True，
// 若这里打成 <nil>/true 会让新旧日志无法直接 diff。
func schoolStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		// JSON 数字统一解成 float64；整数值按 python 的 str(401) 形态输出。
		return strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		return x
	}
	return fmt.Sprintf("%v", v)
}

// schoolTruthy python 真值判定（仅覆盖 JSON 反序列化可能出现的类型）。
// 对应脚本里的 `x or default` 与 `bool(x)` 写法。
func schoolTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// schoolInt 数值取值（python 的 int 语义；None/缺失/非数字都退化成 0）。
func schoolInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return int(n)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return n
		}
	}
	return 0
}

// schoolOrZero python 的 `v or 0`：falsy 一律当 0。
func schoolOrZero(v any) int {
	if !schoolTruthy(v) {
		return 0
	}
	return schoolInt(v)
}

// schoolOrOne python 的 `v or 1`：falsy 一律当 1。
func schoolOrOne(v any) int {
	if !schoolTruthy(v) {
		return 1
	}
	return schoolInt(v)
}

// schoolCut 按 rune 截前 n 个字符（对应 python 的 str(msg)[:120]，不加省略号）。
func schoolCut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// schoolCodeIs 业务码匹配。python 侧同一处同时比对字符串与整数（"401" 与 401），
// JSON 反序列化把数字变成 float64，所以统一走 str 形态比较。
func schoolCodeIs(code any, wants ...string) bool {
	if code == nil {
		return false
	}
	got := schoolStr(code)
	for _, w := range wants {
		if got == w {
			return true
		}
	}
	return false
}

// schoolCodeIsZero 成功判据：脚本 OK_CODES=(0,)。注意刻意与 python 同口径——
// code 为字符串 "0" 或缺失都**不算**成功（python 里 `"0" not in (0,)` 为真）。
func schoolCodeIsZero(code any) bool {
	switch x := code.(type) {
	case float64:
		return x == 0
	case int:
		return x == 0
	case int64:
		return x == 0
	}
	return false
}

// schoolUUID4 生成 v4 UUID（抽奖每轮一次性，抽后即弃）。
// 随机源失败时退化成时间派生：服务端只要求「每轮不同」，不校验来源。
func schoolUUID4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		n := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(n >> (uint(i%8) * 8))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant RFC 4122
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// schoolSleep 写动作间隔（0 表示不等）。
func schoolSleep(d time.Duration) {
	if d > 0 {
		time.Sleep(d)
	}
}

// schoolDeriveID 由 uid 稳定派生设备标识（桌面指纹 machineId/sessionId）。
// 稳定派生是幂等的关键：同一账号反复上报必须是同一台「设备」，否则风控看得见漂移。
// md5 hex 只有 32 位，python 侧 [:36] 实际不截断。
func schoolDeriveID(a *Auth, salt string) string {
	sum := md5.Sum([]byte(salt + ":" + a.UID))
	return hex.EncodeToString(sum[:])
}

// schoolShortUID 日志短标签（脚本 uid8：uid 前 8 位，空则 "?"）。
func schoolShortUID(a *Auth) string {
	if a.UID == "" {
		return "?"
	}
	if len(a.UID) > 8 {
		return a.UID[:8]
	}
	return a.UID
}

// ---------------------------------------------------------------------------
// 响应 / 请求层
// ---------------------------------------------------------------------------

// schoolResp 一次响应：HTTP 状态 + 解析结果。
//
// Body 可能是 map（正常 JSON 对象）、[]any（/v2/report 之类的数组）或
// map{"raw": ...}（非 JSON 响应，对应脚本 _parse_response 的兜底）。
// 刻意不在这一层判成败：这些接口的业务语义大量藏在 4xx 的 code/msg 里。
type schoolResp struct {
	Status int
	Body   any
}

// dict 以 python 的 isinstance(r, dict) 视角看响应体。
func (r schoolResp) dict() map[string]any {
	m, _ := r.Body.(map[string]any)
	return m
}

// field 取顶层字段（非 dict 响应体一律 nil）。
func (r schoolResp) field(k string) any {
	return r.dict()[k]
}

// schoolHeaders 小程序域请求头（脚本 _build_headers）。
// 只有 4 个头，别加 Origin/Referer/X-User-Id：那是 web/CLI 口径。
func schoolHeaders(a *Auth, extra map[string]string) map[string]string {
	h := map[string]string{
		"Authorization": "Bearer " + a.Token,
		"Accept":        "application/json",
		"Content-Type":  "application/json",
		"User-Agent":    SchoolMPUA,
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// schoolDo 发一次请求（不分成败，不重试）。网络层错误返回 SchoolTransientError。
func schoolDo(a *Auth, method, url string, body any, extra map[string]string) (schoolResp, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return schoolResp{Status: -1}, &SchoolTransientError{Msg: err.Error()}
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return schoolResp{Status: -1}, &SchoolTransientError{Msg: err.Error()}
	}
	for k, v := range schoolHeaders(a, extra) {
		req.Header.Set(k, v)
	}
	// 复用 api.go 的包级 client：SetHTTPClient 注入的假 transport 对这里同样生效。
	resp, err := httpClient.Do(req)
	if err != nil {
		return schoolResp{Status: -1}, &SchoolTransientError{Msg: err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return schoolResp{Status: -1}, &SchoolTransientError{Msg: err.Error()}
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		// 非 JSON：连原文一起带出去，_interpret_failure 会打印 raw 前 120 字。
		return schoolResp{Status: resp.StatusCode, Body: map[string]any{"raw": schoolCut(string(raw), 300)}}, nil
	}
	return schoolResp{Status: resp.StatusCode, Body: out}, nil
}

// schoolRetry 带重试的请求（脚本 request_retry）：网络错误与 5xx 都重试，
// 第 i 次重试前睡 gap*i；重试耗尽后抛最后一次错误。
func schoolRetry(a *Auth, method, url string, body any, extra map[string]string) (schoolResp, error) {
	var last error
	var res schoolResp
	for i := 1; i <= schoolRetries; i++ {
		out, err := schoolDo(a, method, url, body, extra)
		if err != nil {
			last = err
			fmt.Printf("  [warn] 网络错误重试 %d/%d: %v\n", i, schoolRetries, err)
			if i < schoolRetries {
				time.Sleep(schoolRetryGap * time.Duration(i))
			}
			continue
		}
		if out.Status >= 500 && out.Status < 600 && i < schoolRetries {
			last = &SchoolTransientError{Msg: fmt.Sprintf("http %d", out.Status)}
			fmt.Printf("  [warn] 5xx=%d 重试 %d/%d\n", out.Status, i, schoolRetries)
			time.Sleep(schoolRetryGap * time.Duration(i))
			continue
		}
		return out, nil
	}
	if last != nil {
		return res, last
	}
	return res, nil
}

// schoolOK 成功判据（脚本 `st != 200 or (isinstance(r, dict) and r.get("code") not in OK_CODES)`）。
func schoolOK(r schoolResp) bool {
	if r.Status != http.StatusOK {
		return false
	}
	if m := r.dict(); m != nil {
		code, ok := m["code"]
		if !ok {
			return false // 缺 code 视为失败（与 python 的 None not in (0,) 一致）
		}
		return schoolCodeIsZero(code)
	}
	return true // 非 dict 响应体（如数组）只校验 HTTP 状态
}

// schoolInterpretFailure 把失败响应翻译成中文原因 + 是否属于鉴权类硬错误
// （脚本 _interpret_failure）。
func schoolInterpretFailure(r schoolResp) (string, bool) {
	code := r.field("code")
	var detail string
	if m := r.dict(); m != nil {
		// python: r.get("message") or r.get("msg") or r.get("raw") or ""
		for _, k := range []string{"message", "msg", "raw"} {
			if v, ok := m[k]; ok && schoolTruthy(v) {
				detail = schoolStr(v)
				break
			}
		}
	} else {
		detail = schoolStr(r.Body)
	}
	switch {
	case r.Status == 401 || schoolCodeIs(code, "401", "40100"):
		return "401/40100 未绑微信或 token 失效（小程序会静默刷新，本脚本不刷新）", true
	case r.Status == 403:
		return "403 forbidden", true
	case r.Status == 404:
		return "404 路由不存在", false
	case schoolCodeIs(code, "41000"):
		return "41000 活动未开始或已结束", false
	case schoolCodeIs(code, "40901"):
		return "40901 任务暂不可领取（未完成或已领）", false
	case schoolCodeIs(code, "83400"):
		return "83400 验证码已过期", false
	case schoolCodeIs(code, "50300"):
		return "50300 学生认证服务暂不可用", false
	}
	return fmt.Sprintf("http=%d code=%s %s", r.Status, schoolStr(code), schoolCut(detail, 120)), false
}

// schoolFail 由失败响应构造错误（脚本里 `raise (AuthError if is_auth else RuntimeError)(...)`）。
func schoolFail(prefix string, r schoolResp) error {
	msg, isAuth := schoolInterpretFailure(r)
	full := prefix + " " + msg
	if isAuth {
		return &SchoolAuthError{Msg: full}
	}
	return &SchoolError{Msg: full}
}

// schoolStatusOf 任务状态（小写），缺失退化成 "?"（脚本 `(t.get("status") or "?").lower()`）。
func schoolStatusOf(t map[string]any) string {
	v := t["status"]
	if !schoolTruthy(v) {
		return "?"
	}
	return strings.ToLower(schoolStr(v))
}

// schoolCodeText 日志里的业务码：dict 响应取 code，否则打印整个响应体
// （脚本 `r.get('code') if isinstance(r, dict) else r`）。
func schoolCodeText(r schoolResp) string {
	if m := r.dict(); m != nil {
		return schoolStr(m["code"])
	}
	return schoolStr(r.Body)
}

// schoolAsMap 把 JSON 值取成 map（缺失/类型不符返回 nil）。
func schoolAsMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// ---------------------------------------------------------------------------
// 任务分类表（脚本 KNOWN_TASKS）
// ---------------------------------------------------------------------------

// schoolTaskSpec 一类任务的处理方式。mode 语义：
//
//	manual  人工环节（学生认证/验证码/审核），--run 一律跳过
//	share   share-complete HTTP 端点点亮
//	report  /v2/report 事件上报点亮（mini chat / 桌面指纹 6 连 / 专家事件）
//
// 未在表内（服务端新增任务）按 unknown 保守跳过：没有分类证据就猜端点，
// 只会把请求打到不该打的地方。
type schoolTaskSpec struct {
	Mode       string
	Note       string
	ReportKind string
}

var schoolKnownTasks = map[string]schoolTaskSpec{
	"task_student_verify": {
		Mode: "manual",
		Note: "微信学生认证（人工，不碰）",
	},
	"share_invite": {
		Mode: "share",
		Note: "分享活动给好友；share-complete 点亮",
	},
	"chat_3_times": {
		Mode:       "report",
		Note:       "与 AI 对话 3 次；mini chat_request_send+activityId 点亮",
		ReportKind: "mini_chat",
	},
	"desktop_chat_1_time": {
		Mode:       "report",
		Note:       "桌面端对话 1 次；桌面指纹 6 连+activityId 点亮",
		ReportKind: "desktop_seq",
	},
	"expert_use": {
		Mode:       "report",
		Note:       "召唤开学季专家并对话；BackToSchool 分类专家 + expert_actual_use 点亮",
		ReportKind: "expert",
	},
}

// schoolOnlyMatches --only 过滤（Go 侧扩展：python 脚本没有这个参数）。
// 空串 = 不过滤，行为与 python 完全一致；非空时按 task_code / mode /
// report_kind 匹配，逗号分隔可多选。
func schoolOnlyMatches(only, code, mode, reportKind string) bool {
	only = strings.TrimSpace(only)
	if only == "" {
		return true
	}
	for _, want := range strings.Split(only, ",") {
		want = strings.ToLower(strings.TrimSpace(want))
		if want == "" {
			continue
		}
		if want == "all" {
			return true
		}
		if want == strings.ToLower(code) || want == mode || want == reportKind {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 只读盘点
// ---------------------------------------------------------------------------

// SchoolTasks GET /portal/activity/school/tasks -> (tasks, inPeriod)。
// 只读：dry-run / 盘点 / 每轮回读都走它。
func (a *Auth) SchoolTasks() ([]map[string]any, bool, error) {
	r, err := schoolRetry(a, http.MethodGet, SchoolCloudAgent+schoolPath+"/tasks", nil, nil)
	if err != nil {
		return nil, false, err
	}
	if !schoolOK(r) {
		return nil, false, schoolFail("school/tasks", r)
	}
	data := schoolAsMap(r.field("data"))
	if data == nil {
		data = map[string]any{}
	}
	raw, _ := data["tasks"].([]any)
	tasks := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			tasks = append(tasks, m)
		}
	}
	return tasks, schoolTruthy(data["in_period"]), nil
}

// SchoolProfile GET /portal/activity/{freshman|teacher}/profile -> data（可为 nil）。
// which 只接受 "freshman"，其它值按 teacher 处理（与脚本写法一致）。
func (a *Auth) SchoolProfile(which string) (map[string]any, error) {
	base := teacherPath
	if which == "freshman" {
		base = freshmanPath
	}
	r, err := schoolRetry(a, http.MethodGet, SchoolCloudAgent+base+"/profile", nil, nil)
	if err != nil {
		return nil, err
	}
	if !schoolOK(r) {
		return nil, schoolFail(which+"/profile", r)
	}
	return schoolAsMap(r.field("data")), nil
}

// ListAccount 只读盘点单个账号（脚本 list_account）：任务中心 + 两个子活动 profile。
func ListAccount(a *Auth, st *SchoolStats) error {
	uid8 := schoolShortUID(a)
	nick := a.Nick
	if nick == "" {
		nick = "-"
	}
	fmt.Printf("== %s (%s) ==\n", uid8, nick)

	tasks, inPeriod, err := a.SchoolTasks()
	if err != nil {
		fmt.Printf("[school2026] %s school/tasks 拉取失败: %v\n", uid8, err)
		st.Fail++
		tasks, inPeriod = nil, false
	}
	fmt.Printf("[school2026] %s school/tasks in_period=%s tasks=%d\n", uid8, schoolStr(inPeriod), len(tasks))

	for _, t := range tasks {
		code := "?"
		if v := t["task_code"]; schoolTruthy(v) {
			code = schoolStr(v)
		}
		status := "?"
		if v := t["status"]; schoolTruthy(v) {
			status = schoolStr(v)
		}
		prog := schoolOrZero(t["progress"])
		target := schoolOrOne(t["target_count"])
		kinds := schoolStr(t["task_type"])
		switch t["task_type"] {
		case "single":
			kinds = "单次"
		case "recurring":
			kinds = "每日"
		}
		spec := schoolKnownTasks[code]
		mode := spec.Mode
		if mode == "" {
			mode = "unknown"
		}
		title := ""
		if v := t["title"]; schoolTruthy(v) {
			title = schoolStr(v)
		}
		fmt.Printf("  - %-20s status=%-10s %d/%d reward=%3d [%s/%s] %s\n",
			code, status, prog, target, schoolOrZero(t["reward_credit"]), kinds, mode, title)
		desc := ""
		if v := t["description"]; schoolTruthy(v) {
			desc = strings.TrimSpace(schoolStr(v))
		}
		if desc != "" {
			fmt.Printf("      desc: %s\n", desc)
		}
		if spec.Note != "" {
			fmt.Printf("      note: %s\n", spec.Note)
		}
	}
	if len(tasks) == 0 {
		fmt.Println("  (无任务 / 未登录或活动未开启)")
	}

	for _, which := range []string{"freshman", "teacher"} {
		d, err := a.SchoolProfile(which)
		if err != nil {
			fmt.Printf("[school2026] %s %s/profile 拉取失败: %v\n", uid8, which, err)
			st.Fail++
			continue
		}
		if d == nil {
			fmt.Printf("[school2026] %s %s/profile -> 空\n", uid8, which)
			continue
		}
		fmt.Printf("[school2026] %s %s/profile claimed=%s enabled=%s\n",
			uid8, which, schoolStr(d["claimed"]), schoolStr(d["enabled"]))
	}
	st.Accounts++
	return nil
}

// ---------------------------------------------------------------------------
// school 专属写端点（点亮动作）
// ---------------------------------------------------------------------------

// schoolViewed POST /tasks/{code}/viewed —— 「接任务」动作（pending -> in_progress）。
// 为什么要先 viewed：pending 的任务直接上报判据服务端不认，H5 上也是先点「去完成」。
func (a *Auth) schoolViewed(code string) (schoolResp, error) {
	r, err := schoolRetry(a, http.MethodPost, SchoolCloudAgent+schoolPath+"/tasks/"+code+"/viewed", nil, nil)
	if err != nil {
		return r, err
	}
	if !schoolOK(r) {
		return r, schoolFail("viewed "+code, r)
	}
	return r, nil
}

// schoolShareComplete POST /tasks/share-complete {channel:"wechat"} —— share_invite 完成判据。
func (a *Auth) schoolShareComplete() (schoolResp, error) {
	body := map[string]any{"channel": "wechat"}
	r, err := schoolRetry(a, http.MethodPost, SchoolCloudAgent+schoolPath+"/tasks/share-complete", body, nil)
	if err != nil {
		return r, err
	}
	if !schoolOK(r) {
		return r, schoolFail("share-complete", r)
	}
	return r, nil
}

// schoolClaim POST /tasks/{code}/claim —— 领奖（completed -> claimed），发抽奖机会。
func (a *Auth) schoolClaim(code string) (schoolResp, error) {
	r, err := schoolRetry(a, http.MethodPost, SchoolCloudAgent+schoolPath+"/tasks/"+code+"/claim", nil, nil)
	if err != nil {
		return r, err
	}
	if !schoolOK(r) {
		return r, schoolFail("claim "+code, r)
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// 抽奖（幸运大转盘）
// ---------------------------------------------------------------------------

// SchoolLotteryConfig GET /portal/activity/school/config —— 盘面 + 余额（只读）。
//
// 返回 (config, chance)：
//
//	config = data（含 start_at/end_at/in_period/prizes[]）
//	chance = data.chance = {balance,total_earned,voucher_won,lottery_limit}
//	chance.balance 即抽奖次数余额。
func (a *Auth) SchoolLotteryConfig() (map[string]any, map[string]any, error) {
	r, err := schoolRetry(a, http.MethodGet, SchoolCloudAgent+schoolPath+"/config", nil, nil)
	if err != nil {
		return nil, nil, err
	}
	if !schoolOK(r) {
		return nil, nil, schoolFail("config", r)
	}
	data := schoolAsMap(r.field("data"))
	if data == nil {
		data = map[string]any{}
	}
	chance := schoolAsMap(data["chance"])
	if chance == nil {
		chance = map[string]any{}
	}
	return data, chance, nil
}

// SchoolLotteryDraw POST /portal/activity/school/wheel/draw {draw_uuid} —— 抽一次转盘。
//
// draw_uuid 每轮一次性（随机 UUID），抽后即弃。
// resp.data={prize_code,credit_amount,chance_balance}。
// 边界：余额 0 时返回 HTTP 409 code=40900 "no chance"（不再扣，安全）——
// 所以这里刻意不判成功，把 40900 交给调用方优雅收尾。
func (a *Auth) SchoolLotteryDraw(drawUUID string) (schoolResp, error) {
	body := map[string]any{"draw_uuid": drawUUID}
	return schoolRetry(a, http.MethodPost, SchoolCloudAgent+schoolPath+"/wheel/draw", body, nil)
}

// schoolPrizeLabels 转盘奖品映射（/config prizes 的 prize_code -> 标签）。
// 未知 prize_code 直接显示原码，不编造内容。
var schoolPrizeLabels = map[string]struct{ label, typ string }{
	"school_credit_6":        {"6积分", "credit"},
	"school_credit_66":       {"66积分", "credit"},
	"school_voucher_luckin":  {"瑞幸咖啡15元券", "voucher"},
	"school_voucher_kfc_ok":  {"肯德基OK餐券", "voucher"},
	"school_voucher_kfc_ice": {"肯德基冰淇淋券", "voucher"},
	"school_voucher_kugou":   {"酷狗会员月卡券", "voucher"},
}

// schoolPrizeText 把 prize_code 映射成语义化文本；积分类附带实际增量。
func schoolPrizeText(prizeCode string, creditAmount int) string {
	info, ok := schoolPrizeLabels[prizeCode]
	if !ok {
		return prizeCode
	}
	if info.typ == "credit" {
		return fmt.Sprintf("%s（+%d Credit）", info.label, creditAmount)
	}
	return info.label
}

// ---------------------------------------------------------------------------
// /v2/report 事件上报
// ---------------------------------------------------------------------------

// schoolSendReportEvents POST {host}/v2/report，body=[event,...]（脚本 report_events）。
// 事件是数组体而不是单对象，这是抓包口径；包成 {"events":[...]} 会被静默丢弃。
func schoolSendReportEvents(a *Auth, host string, events []any, extra map[string]string) (schoolResp, error) {
	return schoolRetry(a, http.MethodPost, host+PathReport, events, extra)
}

// ---------------------------------------------------------------------------
// 开学季专家（BackToSchool 分类）—— expert_use 判据载体
// ---------------------------------------------------------------------------

// schoolExpertFallback 分类专家列表拉不到时的回落名单。
// 用切片而不是 map：python 侧 dict 的插入顺序是确定的（总是先 ex_jB0dyFIQJEWa），
// Go map 迭代随机，必须保序才能复现同一选择。
var schoolExpertFallback = []struct{ id, name, title string }{
	{"ex_jB0dyFIQJEWa", "论小舟", "论文写作导师"},
	{"ex_lQjkerakvIex", "英语学习教练", "大学英语学习教练"},
}

// schoolFetchExpert 拉一个 BackToSchool 分类的真实开学季专家。
//
// 端点：POST {cloudAgent}/v2/operation-platform/market/expert/list
// body {page:1,page_size:20,sort_by:"use_count",sort_order:"desc",
// categories:["16-BackToSchool"],expert_type:"agent",edition_mode:"all,domestic"}
//
// 为什么优先拉真实专家而不是写死 id：专家名随活动运营更新，写死会让上报的
// expertTitle 与线上不一致；列表拉取失败才回落已知专家。
func schoolFetchExpert(a *Auth) (string, string, string, error) {
	body := map[string]any{
		"edition_mode": "all,domestic", "page": 1, "page_size": 20,
		"sort_by": "use_count", "sort_order": "desc",
		"categories": []any{schoolExpertCategoryID}, "expert_type": "agent",
	}
	if r, err := schoolRetry(a, http.MethodPost,
		SchoolCloudAgent+"/v2/operation-platform/market/expert/list", body, nil); err != nil {
		fmt.Printf("  [warn] 拉取 BackToSchool 专家失败，回落已知专家: %v\n", err)
	} else if !schoolOK(r) {
		fmt.Printf("  [warn] 拉取 BackToSchool 专家失败，回落已知专家: code=%s\n", schoolCodeText(r))
	} else if data := schoolAsMap(r.field("data")); data != nil {
		exps, _ := data["experts"].([]any)
		for _, item := range exps {
			e := schoolAsMap(item)
			if e == nil {
				continue
			}
			eid := ""
			if v := e["expert_id"]; schoolTruthy(v) {
				eid = schoolStr(v)
			}
			if eid == "" {
				continue
			}
			dn := schoolZh(e["display_name_zh"], eid)
			pf := schoolZh(e["profession_zh"], "")
			return eid, dn, pf, nil
		}
	}
	if len(schoolExpertFallback) > 0 {
		f := schoolExpertFallback[0]
		return f.id, f.name, f.title, nil
	}
	return "", "", "", &SchoolError{Msg: "无可用开学季专家"}
}

// schoolZh 多语言字段取值：既可能是 {"zh": "..."} 也可能是裸字符串（抓包两种都见过）；
// 都取不到时用 fallback。
func schoolZh(v any, fallback string) string {
	if m := schoolAsMap(v); m != nil {
		if schoolTruthy(m["zh"]) {
			return schoolStr(m["zh"])
		}
		return fallback
	}
	if schoolTruthy(v) {
		return schoolStr(v)
	}
	return fallback
}

// schoolExpertUseEvent expert_actual_use 事件（点亮 expert_use）。
//
// 事件字段：id/name/expertTitle/type/characterCount/expertType + 公共字段 +
// 必须 activityId + conversationId（服务端不校验真实性，但缺 activityId 不关联任务）。
func schoolExpertUseEvent(a *Auth, expertID, expertName, conversationID string) map[string]any {
	now := time.Now().UnixMilli()
	return map[string]any{
		"eventCode": "expert_actual_use", "timestamp": now, "reportDelay": 0,
		"source": "mini_program", "ideName": "wx_app_cloud", "ideType": "WorkBuddy_MP",
		"extName": "workbuddy-mp", "extVersion": "SaaS",
		"machineId": schoolDeriveID(a, "machine"), "os": "android", "osVersion": "14",
		"arch": "arm64", "timezone": "Asia/Shanghai",
		"userId": a.UID, "userNickname": a.Nick,
		// name 与 id 同值：线上小程序就是这么发的（不是笔误）。
		"id": expertID, "name": expertID, "expertTitle": expertName,
		"type": "send_message", "characterCount": 12, "expertType": "agent",
		"conversationId": conversationID,
		"activityId":     SchoolActivityID,
	}
}

// schoolMiniChatEvent 小程序域 chat_request_send 事件（点亮 chat_3_times）。
// 必须带 activityId=SchoolActivityID，否则服务端不关联任务。
func schoolMiniChatEvent(a *Auth, conversationID string) map[string]any {
	now := time.Now().UnixMilli()
	return map[string]any{
		"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
		"source": "mini_program", "ideName": "wx_app_cloud", "ideType": "WorkBuddy_MP",
		"extName": "workbuddy-mp", "extVersion": "SaaS", "mode": "chat",
		"conversationId": conversationID, "requestId": conversationID,
		"inputLength": 12,
		"activityId":  SchoolActivityID, "mentionContexts": []any{}, "mentionContextCount": 0,
		"userId": a.UID,
	}
}

// schoolDesktopFingerprint 桌面指纹：注入每个桌面事件。
// 与 mini 形状的差异：ideName=WorkBuddy / extName=workbuddy-desktop / os=win32 /
// arch=x64 / osVersion=10.0.26220 / machineId 与 sessionId 由 uid 稳定派生。
func schoolDesktopFingerprint(a *Auth) map[string]any {
	now := time.Now().UnixMilli()
	return map[string]any{
		"timezone": "Asia/Shanghai", "reportDelay": 2000,
		"userId": a.UID, "username": a.Nick, "userNickname": a.Nick,
		"product": "SaaS", "releaseDate": int64(1789036585355),
		"commit":  "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName": "WorkBuddy", "ideType": "WorkBuddy", "ideVersion": "5.5.6",
		"machineId": schoolDeriveID(a, "machine"), "sessionId": schoolDeriveID(a, "session"),
		"extName": "workbuddy-desktop", "extVersion": "5.5.6",
		"os": "win32", "arch": "x64", "osVersion": "10.0.26220",
		"cpuCores": 20, "memorySize": 24,
		"timestamp": now, "presentAt": now,
	}
}

// schoolDesktopChatSequence 桌面端成功对话 6 连事件链：
// agent_task_created -> chat_message_send -> chat_request_send ->
// chat_message_response -> chat_message_status -> chat_request_response。
// 必须**整体**上报（一次 6 连 = 一次桌面对话）+ activityId；拆开或缺环不点亮。
func schoolDesktopChatSequence(conversationID, requestID, messageID string) []map[string]any {
	now := time.Now().UnixMilli()
	ev := make([]map[string]any, 0, 6)
	mk := func(code string, extra map[string]any) {
		e := map[string]any{"eventCode": code}
		for k, v := range extra {
			e[k] = v
		}
		ev = append(ev, e)
	}

	mk("agent_task_created", map[string]any{
		"source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"has_repo": false, "repo_type": "none", "workspace_type": "empty",
		"has_connector": false, "connector_types": []any{},
		"has_mention": false, "mention_types": []any{},
		"has_template": false, "action": "", "template_name": "",
		"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
		"has_skill": false, "skill_names": []any{},
		"conversationId": conversationID, "messageId": messageID,
		"buddyId": "", "buddyName": "",
	})
	mk("chat_message_send", map[string]any{
		"messageId": messageID + "-assistant", "historyCount": 0,
		"isContextTruncated": false, "currentStepCount": 1,
		"traceId": requestID, "rootRequestId": requestID,
		"parentConversationId": conversationID,
		"agentName":            "cli", "agentType": "main",
	})
	mk("chat_request_send", map[string]any{
		"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
		"isAutoModify": false, "codebaseEnable": false, "maxToken": 0,
		"maxSteps": 500, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": []any{}, "knowledgeName": []any{},
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"recommendId": "", "skillId": "", "skillCount": 0, "totalCount": 0,
		"traceId": requestID, "rootRequestId": requestID,
		"parentConversationId": conversationID,
		"agentName":            "cli", "agentType": "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": requestID,
	})
	mk("chat_message_response", map[string]any{
		"messageId": messageID + "-assistant", "responseModelId": "fast-model",
		"inputToken": 120, "outputToken": 80, "totalToken": 200,
		"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
		"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
		"firstTokenAt": now, "traceId": requestID,
		"conversationId": conversationID,
		"rootRequestId":  requestID, "parentConversationId": conversationID,
		"agentName": "cli", "agentType": "main",
		"codebuddy.session_id":              conversationID,
		"codebuddy.conversation_request_id": requestID,
	})
	mk("chat_message_status", map[string]any{
		"messageId": messageID + "-assistant", "messageErrorCode": "0",
		"traceId": requestID, "rootRequestId": requestID,
		"parentConversationId": conversationID,
		"agentName":            "cli", "agentType": "main",
	})
	mk("chat_request_response", map[string]any{
		"mode": "craft", "toolCallCount": 0,
		"inputToken": 120, "outputToken": 80, "totalToken": 200,
		"cachedTokens": 0, "cachedWriteTokens": 0, "cachedMissTokens": 0,
		"isSuccessful": true, "messageErrorCode": "", "finishReason": "stop",
		"rootRequestId": requestID, "parentConversationId": conversationID,
	})
	return ev
}

// schoolMakeReportEvents 按任务 kind 构造上报事件数组（含 activityId）。
// 返回 (events, host, extraHeaders, error)。
func schoolMakeReportEvents(a *Auth, kind string) ([]any, string, map[string]string, error) {
	now := time.Now().UnixMilli()
	switch kind {
	case "mini_chat":
		return []any{schoolMiniChatEvent(a, fmt.Sprintf("wbmp-%d", now))}, SchoolCloudAgent, nil, nil
	case "expert":
		eid, name, _, err := schoolFetchExpert(a)
		if err != nil {
			return nil, "", nil, err
		}
		conv := fmt.Sprintf("wbexp-%d", now)
		return []any{schoolExpertUseEvent(a, eid, name, conv)}, SchoolCloudAgent, nil, nil
	case "desktop_seq":
		conv := fmt.Sprintf("wbdesk-%d", now)
		seq := schoolDesktopChatSequence(conv, conv, conv)
		fp := schoolDesktopFingerprint(a)
		events := make([]any, 0, len(seq))
		for _, e := range seq {
			// 先拷事件再覆盖指纹（python 的 dict(e); m.update(fp) 同序）：
			// 指纹里的 timestamp/presentAt/reportDelay 会盖掉事件自带值。
			m := make(map[string]any, len(e)+len(fp)+1)
			for k, v := range e {
				m[k] = v
			}
			for k, v := range fp {
				m[k] = v
			}
			m["activityId"] = SchoolActivityID
			events = append(events, m)
		}
		// 桌面上报走 copilot 域；发 codebuddy.cn 域不点亮桌面任务。
		return events, SchoolCopilot, map[string]string{
			"X-Product":  "SaaS",
			"User-Agent": SchoolDesktopUA,
		}, nil
	}
	return nil, "", nil, &SchoolError{Msg: "未知 report_kind: " + kind}
}

// ---------------------------------------------------------------------------
// 执行模式
// ---------------------------------------------------------------------------

// SchoolStats 汇总计数（脚本 stats dict）。
type SchoolStats struct {
	Accounts int
	OK       int
	Already  int
	Skip     int
	Pending  int
	Fail     int
}

// SchoolOptions 运行参数（脚本 argparse 的 a）。
type SchoolOptions struct {
	Run         bool
	LotteryOnly bool
	Yes         bool
	// Gap 写动作间隔（脚本 --gap，下限 1.0s）。
	Gap time.Duration
	// Only --only 过滤（Go 侧扩展，空 = 与 python 行为一致）。
	Only string
}

// RunAccount --run：viewed 激活 -> 执行判据（share-complete / report 事件）
// -> 回读轮询 -> claim -> 收尾抽奖。
//
// 顺序约束（抓包口径，别调换）：
//  1. pending 必须先 viewed，否则上报的判据不计数；
//  2. 每轮上报后 sleep(gap) 再回读，否则回读到的是上一轮快照，会多报一轮；
//  3. 进度已到 target / status 已 completed|claimed 就提前 break，不过度上报；
//  4. claim 只在回读确认 completed 后调（claim 会给账号加抽奖机会）；
//  5. 最后统一走 LotteryAccount（--yes 下才真抽）。
func RunAccount(a *Auth, opt SchoolOptions, st *SchoolStats) error {
	uid8 := schoolShortUID(a)
	st.Accounts++
	tasks, inPeriod, err := a.SchoolTasks()
	if err != nil {
		fmt.Printf("[school2026] %s run 拉取任务失败: %v\n", uid8, err)
		st.Fail++
		return nil
	}
	if !inPeriod {
		fmt.Printf("[school2026] %s 活动非进行期（in_period=false），跳过\n", uid8)
		return nil
	}

	// refetch 回读单个任务的最新状态；失败返回 nil（调用方按「回读失败」处理，
	// 不把网络抖动当成任务未完成）。
	refetch := func(code string) map[string]any {
		ts, _, e := a.SchoolTasks()
		if e != nil {
			return nil
		}
		for _, x := range ts {
			if c, _ := x["task_code"].(string); c == code {
				return x
			}
		}
		return nil
	}

	for _, t := range tasks {
		codeRaw, _ := t["task_code"].(string)
		code := "?"
		if schoolTruthy(t["task_code"]) {
			code = schoolStr(t["task_code"])
		}
		status := schoolStatusOf(t)
		spec := schoolKnownTasks[code]
		mode := spec.Mode
		if mode == "" {
			mode = "unknown"
		}

		if codeRaw == "" {
			continue
		}
		if status == "completed" || status == "claimed" {
			fmt.Printf("[school2026] %s %s: %s 已完成/已领，跳过\n", uid8, code, status)
			st.Already++
			continue
		}
		if !schoolOnlyMatches(opt.Only, code, mode, spec.ReportKind) {
			fmt.Printf("[school2026] %s %s: 未被 --only %s 选中，跳过\n", uid8, code, opt.Only)
			st.Skip++
			continue
		}

		switch mode {
		case "manual":
			fmt.Printf("[school2026] %s %s: %s -> 人工环节（%s），跳过\n", uid8, code, status, spec.Note)
			st.Skip++
		case "share", "report":
			if !opt.Yes {
				fmt.Printf("[school2026] %s %s: %s -> %s 动作（dry-run 不写）\n", uid8, code, status, mode)
				st.Pending++
				continue
			}
			// 1) pending -> viewed 激活（接单，H5 行为）
			if status == "pending" {
				if _, err := a.schoolViewed(code); err != nil {
					fmt.Printf("[school2026] %s %s: viewed 激活失败: %v\n", uid8, code, err)
					st.Fail++
					continue
				}
				fmt.Printf("[school2026] %s %s: viewed 激活 pending->in_progress\n", uid8, code)
				schoolSleep(opt.Gap)
			}

			// 2) 触发完成判据。share 一次即完成；report 按 target-当前进度 循环触发
			//    （每次事件独立 +1）。桌面 6 连一次 = 一次桌面对话，只报一轮。
			curProg := schoolOrZero(t["progress"])
			target := schoolOrOne(t["target_count"])
			rounds := 1
			if mode == "report" && spec.ReportKind != "desktop_seq" {
				rounds = target - curProg
				if rounds < 1 {
					rounds = 1
				}
			}
			progressChanged := false
			for ri := 0; ri < rounds; ri++ {
				if mode == "share" {
					r, err := a.schoolShareComplete()
					if err != nil {
						fmt.Printf("[school2026] %s %s: %s 触发 #%d 失败: %v\n", uid8, code, mode, ri+1, err)
						break
					}
					fmt.Printf("[school2026] %s %s: share-complete %d code=%s\n",
						uid8, code, r.Status, schoolCodeText(r))
				} else {
					events, host, extra, err := schoolMakeReportEvents(a, spec.ReportKind)
					if err != nil {
						fmt.Printf("[school2026] %s %s: %s 触发 #%d 失败: %v\n", uid8, code, mode, ri+1, err)
						break
					}
					r, err := schoolSendReportEvents(a, host, events, extra)
					if err != nil {
						fmt.Printf("[school2026] %s %s: %s 触发 #%d 失败: %v\n", uid8, code, mode, ri+1, err)
						break
					}
					fmt.Printf("[school2026] %s %s: report #%d/%d %d 事件 -> http=%d code=%s\n",
						uid8, code, ri+1, rounds, len(events), r.Status, schoolCodeText(r))
				}
				schoolSleep(opt.Gap)
				// 每轮回读一次，判断是否已到 target/completed，提早终止
				t2 := refetch(code)
				if t2 == nil {
					continue
				}
				afterProg := schoolOrZero(t2["progress"])
				after := schoolStatusOf(t2)
				if afterProg > curProg {
					curProg = afterProg
					progressChanged = true
				}
				if after == "completed" || after == "claimed" || afterProg >= target {
					break
				}
			}

			// 3) 最终回读确认
			t2 := refetch(code)
			if t2 == nil {
				fmt.Printf("[school2026] %s %s: 回读任务失败，跳过\n", uid8, code)
				st.Fail++
				continue
			}
			after := schoolStatusOf(t2)
			afterProg := schoolOrZero(t2["progress"])
			if after == "completed" || after == "claimed" || progressChanged {
				fmt.Printf("[school2026] %s %s: %s/%s -> %s/%d（点亮）\n",
					uid8, code, status, schoolStr(t["progress"]), after, afterProg)
				st.OK++
			} else {
				fmt.Printf("[school2026] %s %s: %s -> %s（未变化）\n", uid8, code, status, after)
				st.Pending++
			}

			// 4) 若 completed，claim 领奖（chance_granted）
			if after == "completed" {
				rc, err := a.schoolClaim(code)
				if err != nil {
					fmt.Printf("[school2026] %s %s: claim 失败: %v（可稍后手动补领）\n", uid8, code, err)
					st.Fail++
				} else {
					fmt.Printf("[school2026] %s %s: claim %d code=%s\n",
						uid8, code, rc.Status, schoolCodeText(rc))
					schoolSleep(opt.Gap)
				}
			}
		default: // unknown
			title := ""
			if v := t["title"]; schoolTruthy(v) {
				title = schoolStr(v)
			}
			fmt.Printf("[school2026] %s %s: %s -> 未知任务类型（%s），无分类证据，保守跳过\n",
				uid8, code, status, title)
			st.Skip++
		}
	}

	// 5) 任务循环收尾：抽奖段（claim 会发多次机会）。--yes 才真正 draw；
	//    dry-run 下 LotteryAccount 也只读查余额并打印将执行的调用。
	return LotteryAccount(a, opt, st, false)
}

// LotteryAccount 抽奖段：查余额 -> 循环抽到空 -> 汇总打印。
//
//	GET  /portal/activity/school/config       -> data.chance.balance（只读，抽前查余额）
//	POST /portal/activity/school/wheel/draw   {draw_uuid} -> data.prize_code/credit_amount/
//	                                                          chance_balance（抽奖 + 扣次数）
//
// 余额 0 时再抽返回 HTTP 409 code=40900 "no chance"（安全边界，脚本借此优雅停）。
// 余额未递减连续 3 次即视为服务端异常提前停（防死循环）。
// dry-run 只打印将执行的调用不发送。结果如实记录（含积分/实物券，不编造）。
// countAccount=false：供 --run 任务循环收尾复用（账号数已由 RunAccount 计入）。
func LotteryAccount(a *Auth, opt SchoolOptions, st *SchoolStats, countAccount bool) error {
	uid8 := schoolShortUID(a)
	if countAccount {
		st.Accounts++
	}

	// dry-run：只读查余额可做；写（draw）只打印不发送
	if !opt.Yes {
		_, chance, err := a.SchoolLotteryConfig()
		if err != nil {
			fmt.Printf("[school2026] %s lottery config 失败: %v\n", uid8, err)
			st.Fail++
			return nil
		}
		bal := schoolOrZero(chance["balance"])
		if bal <= 0 {
			fmt.Printf("[school2026] %s lottery balance=0，无需抽奖\n", uid8)
			return nil
		}
		fmt.Printf("[school2026] %s lottery balance=%d（dry-run：将 POST /wheel/draw 抽 %d 次，间隔 ≥1s，不发请求）\n",
			uid8, bal, bal)
		st.Pending++
		return nil
	}

	cfg, chance, err := a.SchoolLotteryConfig()
	if err != nil {
		fmt.Printf("[school2026] %s lottery config 失败: %v\n", uid8, err)
		st.Fail++
		return nil
	}
	if !schoolTruthy(cfg["in_period"]) {
		fmt.Printf("[school2026] %s 活动非进行期（in_period=false），抽奖跳过\n", uid8)
		return nil
	}

	bal := schoolOrZero(chance["balance"])
	totalEarned := schoolOrZero(chance["total_earned"])
	lotteryLimit := chance["lottery_limit"]
	if bal <= 0 {
		fmt.Printf("[school2026] %s lottery balance=0（累计获得 %d 次），无需抽奖\n", uid8, totalEarned)
		return nil
	}
	fmt.Printf("[school2026] %s lottery balance=%d total_earned=%d lottery_limit=%s（开始抽奖，抽到空为止）\n",
		uid8, bal, totalEarned, schoolStr(lotteryLimit))

	var results []struct {
		credit int
		label  string
	}
	totalCredit := 0
	prevBal := bal
	stall := 0
	for bal > 0 {
		// draw_uuid 每轮一次性，抽后即弃
		r, err := a.SchoolLotteryDraw(schoolUUID4())
		if err != nil {
			fmt.Printf("[school2026] %s draw 失败: %v\n", uid8, err)
			st.Fail++
			break
		}
		if r.Status != http.StatusOK || (r.dict() != nil && !schoolCodeIsZero(r.field("code"))) {
			code := r.field("code")
			msg := r.field("message")
			if schoolCodeIs(code, "40900") {
				fmt.Printf("[school2026] %s draw http=%d code=40900（次数耗尽：no chance），按边界正常结束\n",
					uid8, r.Status)
				bal = 0 // 「no chance」边界，如实结束
				break
			}
			fmt.Printf("[school2026] %s draw http=%d code=%s msg=%s（未中奖，停）\n",
				uid8, r.Status, schoolStr(code), schoolStr(msg))
			break
		}
		d := schoolAsMap(r.field("data"))
		prizeCode := "?"
		if schoolTruthy(d["prize_code"]) {
			prizeCode = schoolStr(d["prize_code"])
		}
		credit := schoolOrZero(d["credit_amount"])
		// 服务端回读余额驱动循环；缺字段时沿用上一轮余额（脚本的 get(key, bal)）
		if v, ok := d["chance_balance"]; ok {
			bal = schoolInt(v)
		}
		if bal >= prevBal { // 余额未降：服务端异常，防死循环
			stall++
			if stall >= 3 {
				fmt.Printf("[school2026] %s draw 余额未递减（服务端异常），提前停\n", uid8)
				break
			}
		} else {
			stall = 0
		}
		prevBal = bal
		label := schoolPrizeText(prizeCode, credit)
		results = append(results, struct {
			credit int
			label  string
		}{credit: credit, label: label})
		totalCredit += credit
		fmt.Printf("[school2026] %s draw -> %s（balance %d）\n", uid8, label, bal)
		if bal > 0 {
			schoolSleep(opt.Gap)
		}
	}

	// 汇总（如实列出每个中奖物）
	if len(results) > 0 {
		parts := make([]string, 0, len(results))
		for _, x := range results {
			parts = append(parts, strconv.Itoa(x.credit))
		}
		fmt.Printf("[school2026] %s lottery 汇总: %d 抽，积分增量 %d（%s）\n",
			uid8, len(results), totalCredit, strings.Join(parts, "+"))
		for _, x := range results {
			fmt.Printf("    - %s\n", x.label)
		}
		if bal == 0 {
			st.OK++ // 只有余额归零才算抽完
		} else {
			st.Pending++ // 中途被服务端打断，未抽完
		}
	} else {
		fmt.Printf("[school2026] %s lottery 无一抽出奖（上次余额 %d）\n", uid8, bal)
	}
	return nil
}

// PrintNotes 打印任务/未打通说明（对应脚本头部风险提示，不编造）。
func PrintNotes() {
	fmt.Println()
	fmt.Printf("[school2026] 说明：非人工任务（share/chat/desktop/expert）自动点亮并领奖；" +
		"task_student_verify 学生认证为人工环节，--run 跳过；" +
		"幸运大转盘将在任务收尾或 --lottery-only 时抽到余额为 0。\n")
}
