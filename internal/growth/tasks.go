// tasks.go 一次性「成长任务一体机」（task_runner）的 Go 移植：查询 → 完成(点亮) → 领取。
//
// 移植自 scripts/task_runner.py（1773 行）以及它 import 的 scripts/school_open_day_2026.py。
// 那两个 .py 会被统一删除，且本包不允许依赖 python，所以 school 段的**判据与端点**一并搬进
// internal/growth——由同包的 school.go（school_open_day_2026.py 的移植）提供
// （SchoolCloudAgent / SchoolCopilot / SchoolActivityID / (*Auth).SchoolTasks /
// schoolViewed / schoolShareComplete / schoolClaim / schoolMakeReportEvents /
// schoolSendReportEvents / schoolMiniChatEvent / schoolDesktopChatSequence /
// schoolDesktopFingerprint / schoolDeriveID / schoolStatusOf / schoolCodeText …）。
// 本文件直接复用它们，避免同一份抓包形状在包内出现第二份拷贝。
//
// 为什么用 Go：目标形态是「单镜像 + Go 二进制」，镜像不装 python3；原脚本还要靠
// 面板 exec python 调用，与 internal/upstream 的同类调用长期来回漂移。这次连同判据
// 一起搬进 internal/growth，让 growth 域的请求形状只有一个事实源。
//
// 与 api.go 的分工：HTTP/账号加载/任务列表/接取/领奖/连登/上报/真实对话已由 api.go
// 提供，本文件能用就用（ListTasks / TaskStatus / AcceptTasks / ReportActivity / Get / Post）。
// 只有以下场景 api.go 覆盖不到，才在本文件里补底层能力（且刻意不改 api.go）：
//   - 小程序 growth 限定任务要在 list/accept/claim 上带 X-Client-Platform: miniprogram；
//   - 桌面指纹 / web 域指纹上报各有自己的一套请求头与公共字段注入；
//   - claim 需要「失败原因打印 + 400 降级 web 域」这条完整日志链路（api.go 的
//     ClaimReward 只做降级，不产出降级/失败的进度行，也不支持 mp 头）。
//
// 语义、端点、请求头、时序、幂等判定逐条对齐原脚本，逐字段照抄事件形状（这些形状是
// 照抄官方客户端抓包得来的，少一个字段服务端就静默不记账）。凡原脚本里带「为什么」的
// 注释，这里都保留要点。
//
// 平台说明（只面向 Linux/Docker）：原脚本开头把 stdout/stderr 重配成 UTF-8 是为了绕开
// Windows 中文区域 cp936 编码在打印 emoji 昵称时抛 UnicodeEncodeError（issue #147）；
// Linux 下 stdout 恒 UTF-8，无需等价逻辑，故这里没有对应代码。同理脚本里对 /dev/tty
// 的处理并不存在于此文件；所有交互语义（dry-run / --yes）都由 CLI 显式表达。
//
// 函数映射（python -> go）见本文件各函数上方的注释；未实现的只有 school 脚本自己
// 的抽奖段（lottery_account / wheel/draw 等）——它不在 task_runner 的执行路径上。
package growth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// --------------------------------------------------------------------------
// 常量：端点 / 指纹 / 内置回落表
// --------------------------------------------------------------------------

const (
	// marketListPath 专家市场列表（POST，只读；取 ex_ id 与团队 id）。
	marketListPath = "/v2/operation-platform/market/expert/list"
	// skillListPath 技能市场列表（POST，只读；取 skillId）。
	skillListPath = "/v2/operation-platform/market/skill/list"
	// scenesPath 模板场景清单（GET，只读；取 template_5 的对象 id）。
	scenesPath = "/console/as/support/scenes?locale=zh-CN"
	// energyPath 能量余额只读 oracle（M15 实测 data.balance）。
	energyPath = "/v2/activity/growth/energy"
	// appearancePath 主题资源目录（POST billing 域，只读；取 Hp_Appearance 的 resourceKey）。
	appearancePath = "/v2/operation-platform/appearance/resources"

	// cosExpertURL COS 静态专家清单：团队专家市场列表很薄（250 个里仅 2 个 team），
	// M4 实测 COS 有 52 个 team，作为 team_only 的主/补来源。
	cosExpertURL = "https://acc-1258344699.cos.accelerate.myqcloud.com/" +
		"workbuddy/expert-marketplace/expert_center.json"
	// playbookBase 灵感案例注册表基址（playbook_prompt 的案例 id 来源）。
	playbookBase = "https://static.workbuddy.cn/workbuddy/playbook"
	// webBase web 成长中心域（Library_read 的 web 指纹上报与 claim 降级都用它）。
	webBase = "https://www.workbuddy.cn"

	// desktopUA 桌面指纹 UA（fork desktop.go；与 api.go 的 CLI UA 不同源）。
	desktopUA = "WorkBuddy/5.5.6 WorkBuddy/5.5.6 CLI/2.137.1"
	// webUA 浏览器 UA（web 域行为必须像浏览器）。
	webUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36"

	// libraryDocURL Library_read 的空间文档 URL（fork autotask.go:438 实测三账号点亮）。
	libraryDocURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
)

// mpPlatformHeader growth 域小程序限定任务（Sequential_Tasks_*）的必需头：
// 缺头时任务不下发、accept 返回 task not found（web 小程序 H5 在请求拦截器统一注入）。
var mpPlatformHeader = map[string]string{"X-Client-Platform": "miniprogram"}

// clientUAVersion 取 api.go 的 CLI UA 尾段版本号（appearance/resources 的 version 参数）。
func clientUAVersion() string {
	parts := strings.Split(ClientUA, "/")
	return parts[len(parts)-1]
}

// taskSpec 任务完成定义（对应脚本 MAPPING 的 value）。
type taskSpec struct {
	Kind        string // 事件构建器类型（idsFor / buildEvent 分派）
	Target      int    // 兜底次数（优先读服务端 progress.target）
	Src         string // 对象 id 来源说明（仅展示）
	Unforgeable bool   // 不可伪造（真实业务副作用）
	Reason      string
}

// taskMappingOrder 与脚本 MAPPING 的插入顺序一致：默认全量模式按这个顺序处理任务，
// 顺序会体现在日志与写动作时序上（风控看节奏），所以不能靠 map 遍历随机化。
var taskMappingOrder = []string{
	"create_canvas", "template_5", "expert_5", "Expert_team_use_3", "skill_1",
	"automation_1", "playbook_prompt", "Expert_lighthouse", "Buddy_App", "Buddy_App_QQ",
	"Hp_Appearance", "chat_5", "Model_chat_GLM5.2", "black_cat", "first_buddy",
	"RichMeow_Chat", "Library_read", "chat_3_times", "expert_use", "share_invite",
	"desktop_chat_1_time", "Sequential_Tasks_1", "school_season", "Sequential_Tasks_2",
	"Sequential_Tasks_3", "Sequential_Tasks_4", "Sequential_Tasks_5",
	"Expert_Philanthropy",
}

var taskMapping = map[string]taskSpec{
	"create_canvas":       {Kind: "canvas", Target: 1, Src: "自造 wb-<ms>"},
	"template_5":          {Kind: "template", Target: 5, Src: "scenes 场景 id"},
	"expert_5":            {Kind: "expert", Target: 5, Src: "专家市场 ex_ id"},
	"Expert_team_use_3":   {Kind: "team", Target: 3, Src: "团队 id"},
	"skill_1":             {Kind: "skill", Target: 1, Src: "skillId"},
	"automation_1":        {Kind: "automation", Target: 1, Src: "rrule 虚拟对象"},
	"playbook_prompt":     {Kind: "playbook", Target: 1, Src: "playbook 案例 id"},
	"Expert_lighthouse":   {Kind: "lighthouse", Target: 1, Src: "轻量云专家 id"},
	"Buddy_App":           {Kind: "buddy5", Target: 1, Src: "buddy 应用 id"},
	"Buddy_App_QQ":        {Kind: "buddy5", Target: 1, Src: "企鹅教师助手 id"},
	"Hp_Appearance":       {Kind: "skin", Target: 1, Src: "主题 resourceKey"},
	"chat_5":              {Kind: "chat", Target: 5, Src: "无(独立会话)"},
	"Model_chat_GLM5.2":   {Kind: "glmchat", Target: 1, Src: "无(glm-5.2)"},
	"black_cat":           {Kind: "cat", Target: 3, Src: "无(glm-5.2 夜猫)"},
	"first_buddy":         {Kind: "buddyfirst", Target: 1, Src: "无(report→agreement→first)"},
	"RichMeow_Chat":       {Kind: "richmeow", Target: 1, Src: "无(桌面指纹对话链)"},
	"Library_read":        {Kind: "library", Target: 1, Src: "无(资料库介绍点击)"},
	"chat_3_times":        {Kind: "school", Target: 3, Src: "无(mini_chat×3+activityId)"},
	"expert_use":          {Kind: "school", Target: 1, Src: "BackToSchool 专家 id"},
	"share_invite":        {Kind: "school", Target: 1, Src: "无(share-complete)"},
	"desktop_chat_1_time": {Kind: "school", Target: 1, Src: "无(桌面 6 连+activityId)"},
	"Sequential_Tasks_1":  {Kind: "minichat", Target: 1, Src: "无(mini 对话无activityId)"},
	"school_season":       {Kind: "schoolseason", Target: 1, Src: "无(mini chat+activityId)"},
	"Sequential_Tasks_2":  {Kind: "miniexpert", Target: 1, Src: "专家市场 ex_ id(mini)"},
	"Sequential_Tasks_3":  {Kind: "minichat", Target: 5, Src: "无(mini 对话×5 无activityId)"},
	"Sequential_Tasks_4":  {Kind: "miniautomation", Target: 1, Src: "无(mp 自造定时任务名)"},
	"Sequential_Tasks_5":  {Kind: "miniglmchat", Target: 1, Src: "无(glm-5.2 模型上下文)"},
	"Expert_Philanthropy": {Unforgeable: true, Reason: "真实捐款动作(M8)"},
}

// schoolReportKind 开学季任务 → school 段处理方式（school_open_day_2026.KNOWN_TASKS 的
// report_kind 子集）。mode=manual（task_student_verify 学生认证）不在 MAPPING，
// 未映射任务照旧保守跳过。
var schoolReportKind = map[string]string{
	"chat_3_times":        "mini_chat",
	"expert_use":          "expert",
	"share_invite":        "share", // 非事件上报，走 /tasks/share-complete 端点
	"desktop_chat_1_time": "desktop_seq",
}

// schoolCodes 与上表同序（决定 school 段的任务处理顺序）。
var schoolCodes = []string{"chat_3_times", "expert_use", "share_invite", "desktop_chat_1_time"}

// mpKinds growth 域小程序限定任务的 kind 集合：这些任务在默认（无 mp 头）列表里不存在，
// 由 processMiniChatTask 专段处理，不进 growth 主循环。
var mpKinds = map[string]bool{
	"minichat": true, "schoolseason": true, "miniexpert": true,
	"miniautomation": true, "miniglmchat": true,
}

// defaultScenes scenes 接口失败时的内置场景表（(id, name)）。
var defaultScenes = []taskObj{
	{ID: "0", Meta: "幻灯片"}, {ID: "2", Meta: "视频生成"},
	{ID: "4", Meta: "深度研究"}, {ID: "6", Meta: "文档处理"},
	{ID: "8", Meta: "数据分析"}, {ID: "10", Meta: "可视化"},
	{ID: "12", Meta: "金融服务"}, {ID: "14", Meta: "产品管理"},
}

// defaultExpertIDs 专家市场拉取失败时的内置专家 id（普通专家）。
var defaultExpertIDs = []string{
	"ContentCreator", "UiDesigner", "DataAnalyticsReporter", "ChinaEcommerceOperationsExpert",
	"DouyinStrategist", "SalesCoach", "BrandGuardian", "XiaohongshuOperationsExpert",
}

// expertNames 内置专家 id → 中文名（仅展示用）。
var expertNames = map[string]string{
	"ContentCreator":                 "内容创作专家",
	"UiDesigner":                     "UI设计师",
	"DataAnalyticsReporter":          "数据分析报告师",
	"ChinaEcommerceOperationsExpert": "中国电商运营专家",
	"DouyinStrategist":               "抖音策略师",
	"SalesCoach":                     "销售教练",
	"BrandGuardian":                  "品牌策略师",
	"XiaohongshuOperationsExpert":    "小红书运营专家",
}

// defaultTeamIDs 团队专家内置 id（team_only 回落）。
var defaultTeamIDs = []string{
	"CloudOpsTeam", "SoftwareCompany", "TradingAgentTeam",
	"GPTResearcherTeam", "MarketingCampaignTeam",
}

// defaultSkillIDs 技能市场回落表（(skill_id, 名称)）。
var defaultSkillIDs = []taskObj{
	{ID: "skill_2096525080079265792", Meta: "pptx"},
	{ID: "skill_2096528888507297792", Meta: "xlsx"},
	{ID: "skill_2070033533400236032", Meta: "qqmusic"},
	{ID: "skill_2095322904487550976", Meta: "pdf"},
}

// defaultCases 灵感案例注册表回落表。
var defaultCases = []taskObj{{
	ID: "worker-ledger-freedom-dashboard",
	Meta: map[string]any{
		"title": "打工人小账本", "artifact_type": "other",
		"categories": []any{""}, "skills": []any{}, "experts": []any{}, "prompt": "",
	},
}}

// lighthouseIDs 轻量云专家回落表（关键词过滤不到时的已知专家）。
var lighthouseIDs = []taskObj{{
	ID: "ex_2cvvUZQhDyeJ",
	Meta: map[string]any{
		"name": "腾讯轻量云专家", "category": "02-Engineering",
		"expertType": "agent", "version": "1.0.2",
	},
}}

// lighthouseKeywords 轻量云专家的市场搜索关键词。
var lighthouseKeywords = []string{"lighthouse", "轻量云"}

// buddyApps Buddy_App / Buddy_App_QQ 五连事件承载应用（同一组事件同时满足两个任务）。
// fork DesktopBuddyAppSequence 实测用企鹅教师助手，两账号实测点亮。
var buddyApps = map[string][2]string{
	"Buddy_App":    {"cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手"},
	"Buddy_App_QQ": {"cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手"},
}

// peThemes Hp_Appearance 的主题回落表（和平精英激战金秋）。
var peThemes = []taskObj{{
	ID: "theme-tkmw7j",
	Meta: map[string]any{
		"name": "和平精英激战金秋", "vipLevel": "free",
		"series": "craft", "appearance": "light",
	},
}}

// lighthouseIDRe 服务端 chat 接口返回的 requestId 形状：cmb-<32 hex> 或裸 32 hex。
// 自造 UUID 上报 200 但不计数（issue #221），所以必须校验形状。
var lighthouseIDRe = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// --------------------------------------------------------------------------
// 通用小工具（python 值语义的等价物）
//
// 注意 school.go 自带一套 schoolStr/schoolTruthy/schoolInt（它那份是给 school 域用的）。
// 这里是 task_runner 侧的同类工具，名字不同、语义一致，两边都只为「日志外观与 python
// 逐字可比」而存在。
// --------------------------------------------------------------------------

// taskObj 一个「对象 id + 元数据」对（脚本里的 (id, meta) 元组）。
// Meta 可能是 dict（丰富元数据）或 string（如场景名/技能名），与脚本一致。
type taskObj struct {
	ID   string
	Meta any
}

// nowMS 当前毫秒时间戳（脚本里 int(time.time()*1000) 的位置都是它）。
func nowMS() int64 { return time.Now().UnixMilli() }

// mapOf 把 any 断言成 map[string]any（不是就返回 nil）。
func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// anySlice 把 any 断言成 []any（不是就返回 nil）。
func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// mapSlice 把 any 断言成 []map[string]any（脏元素跳过）。
func mapSlice(v any) []map[string]any {
	raw := anySlice(v)
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m := mapOf(item); m != nil {
			out = append(out, m)
		}
	}
	return out
}

// pyVal python print 的数字/字符串外观：None → "None"，整数值的 float 不带 ".0"。
func pyVal(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	default:
		return fmt.Sprint(v)
	}
}

// asStr 宽泛转字符串（nil → ""）。
func asStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return pyVal(v)
}

// zhStr 取本地化名称：可能是裸字符串，也可能是 {"zh": "..."} 的结构。
func zhStr(v any) string {
	if m := mapOf(v); m != nil {
		return asStr(m["zh"])
	}
	return asStr(v)
}

// asInt 宽泛转整数（nil/脏值 → 0）。
func asInt(v any) int {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n)
		}
	}
	return 0
}

// truthy python 真值语义（用于 `x or default` / `if x` 的移植）。
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	case int:
		return t != 0
	case map[string]any:
		return len(t) > 0
	case []any:
		return len(t) > 0
	}
	return true
}

// codeIsZero 业务成功判据：信封里 code 存在且等于 0（脚本 `r.get("code") != 0`）。
// 缺 code 视作失败——这些接口的成功信封一定带 code。
func codeIsZero(r map[string]any) bool {
	if r == nil {
		return false
	}
	v, ok := r["code"]
	if !ok {
		return false
	}
	return codeIsZeroAny(v)
}

// codeIsLenient 类似 codeIsZero，但把「缺字段」当成功（脚本里 `code in (0, None)` 的口径）。
func codeIsLenient(r map[string]any) bool {
	if r == nil {
		return true
	}
	if _, ok := r["code"]; !ok {
		return true
	}
	return codeIsZero(r)
}

// codeIsZeroAny 对「已取出的 code 值」做同一判据（上报通道返回值用）。
func codeIsZeroAny(v any) bool {
	switch t := v.(type) {
	case float64:
		return t == 0
	case int:
		return t == 0
	case int64:
		return t == 0
	case json.Number:
		return t.String() == "0"
	}
	return false
}

// msgOf 取信封里的错误文案（message / msg / raw 依次回落）。
func msgOf(r map[string]any) string {
	if r == nil {
		return ""
	}
	for _, k := range []string{"message", "msg", "raw"} {
		if v, ok := r[k]; ok && truthy(v) {
			return asStr(v)
		}
	}
	return ""
}

// pyList 把字符串切片打印成 python 列表外观（日志与脚本逐字对齐）。
func pyList(xs []string) string {
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		parts = append(parts, "'"+x+"'")
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// tail 取字符串末 n 个字符（脚本 req_id[-8:]）。
func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// metaName 取 taskObj 的展示名（dict→name，string→自身，缺省回落 id）。
func metaName(o taskObj) string {
	if s, ok := o.Meta.(string); ok {
		if s != "" {
			return s
		}
		return o.ID
	}
	if m := mapOf(o.Meta); m != nil {
		if n := asStr(m["name"]); n != "" {
			return n
		}
	}
	return o.ID
}

// uid8Of 账号短标签：uid 前 8 位，空 uid 记 "?"（脚本 `(uid or "")[:8] or "?"`）。
func uid8Of(a *Auth) string {
	if a.UID == "" {
		return "?"
	}
	if len(a.UID) >= 8 {
		return a.UID[:8]
	}
	return a.UID
}

// firstNonEmptyStr 取第一个非空字符串。
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// firstElemStr 取数组第一项的字符串值（`(x or [""])[0] or ""`）。
func firstElemStr(v any) string {
	items := anySlice(v)
	if len(items) == 0 {
		return ""
	}
	return asStr(items[0])
}

// nonNilSlice 保持原始数组（缺省给空数组，与 `x or []` 同义）。
func nonNilSlice(v any) []any {
	if items := anySlice(v); items != nil {
		return items
	}
	return []any{}
}

// joinField 拼接子对象数组里的某个字符串字段（`",".join(x.get(k,"") for x in items if ...)`）。
func joinField(items []any, key string) string {
	parts := make([]string, 0, len(items))
	for _, it := range items {
		m := mapOf(it)
		if m == nil {
			continue
		}
		if v := asStr(m[key]); v != "" {
			parts = append(parts, v)
		}
	}
	return strings.Join(parts, ",")
}

// --------------------------------------------------------------------------
// 时段判定：夜猫窗口 23:00 - 次日 08:00 CST
// --------------------------------------------------------------------------

// withinNightWindow 是否处于夜猫窗口（固定 +08:00，与脚本 datetime.timezone(+8h) 同口径；
// 不依赖宿主机时区，Docker 里 TZ 为 UTC 也判对）。
func withinNightWindow() bool {
	now := time.Now().In(time.FixedZone("CST", 8*3600))
	return now.Hour() >= 23 || now.Hour() < 8
}

// --------------------------------------------------------------------------
// 请求头（桌面指纹 / web 域 / web 领奖）
//
// 桌面指纹本体用 school.go 的 schoolDesktopFingerprint（与脚本 desktop_fingerprint 逐字段
// 相同）；这里只放 task_runner 独有的三套头。
// --------------------------------------------------------------------------

// desktopHeaders chat 域桌面指纹请求头（fork ReportDesktopEvent 同款）。
func desktopHeaders(a *Auth) map[string]string {
	jitter := time.Now().UnixNano() % 1000000
	if jitter < 0 {
		jitter += 1000000
	}
	return map[string]string{
		"Accept":       "application/json, text/plain, */*",
		"Content-Type": "application/json;charset=UTF-8",
		"User-Agent":   desktopUA,
		"X-Domain":     a.ChatBaseOf(),
		"X-Product":    "SaaS",
		"X-Request-ID": schoolDeriveID(a, "req") + strconv.FormatInt(jitter, 10),
		"X-User-Id":    a.UID,
	}
}

// webEventHeaders web 域浏览器请求头（fork ReportWebEvent，Library_read 用）。
//
// X-Domain 显式覆盖为 web 域：api.go 的账号头会把 auth 的 domain（如 copilot.tencent.com）
// 带上，发往 www.workbuddy.cn 会造成跨域不一致。
func webEventHeaders(a *Auth, pageURL string) map[string]string {
	return map[string]string{
		"Accept":            "application/json",
		"Content-Type":      "application/json",
		"x-client-platform": "web",
		"Origin":            webBase,
		"Referer":           pageURL,
		"User-Agent":        webUA,
		"X-User-Id":         a.UID,
		"X-Domain":          webBase,
	}
}

// webClaimHeaders web 域领奖头（fork ClaimReward 同款，claim 400 降级用）。
func webClaimHeaders(a *Auth) map[string]string {
	return map[string]string{
		"Authorization":     "Bearer " + a.Token,
		"Accept":            "application/json, text/plain, */*",
		"Content-Type":      "application/json",
		"Origin":            webBase,
		"Referer":           webBase + "/profile/growth-center",
		"x-client-platform": "web",
		"User-Agent":        webUA,
		"X-User-Id":         a.UID,
		"X-Domain":          webBase,
	}
}

// mpDeviceFingerprint 小程序「公共遥测字段」（小程序源码 module 22015 的真实形状）：
//
//	{ideType:"WorkBuddy_MP", ideVersion:"2.2.8", extName:"workbuddy-mp", extVersion:"2.2.8",
//	 product:"SaaS", os, osVersion, arch, machineId, timezone}
//
// ideVersion/extVersion/product 是源码常量（恒定值）——2.2.8 是小程序包版本而非 SaaS 状态，
// 与 school 段那份 extVersion="SaaS" 的手抄形状不同源（school 段是早前凭观察拼的）。
// os/osVersion/arch 固定成一份真实安卓机指纹（android 14 / arm64），machineId 由 uid 稳定派生。
func mpDeviceFingerprint(a *Auth) map[string]any {
	return map[string]any{
		"ideType": "WorkBuddy_MP", "ideVersion": "2.2.8",
		"extName": "workbuddy-mp", "extVersion": "2.2.8", "product": "SaaS",
		"os": "android", "osVersion": "14", "arch": "arm64",
		"timezone":  "Asia/Shanghai",
		"machineId": schoolDeriveID(a, "machine"),
	}
}

// --------------------------------------------------------------------------
// 对象 id 来源（只读 fetch；均返回 []taskObj，顺序与脚本一致并有内置回落）
// --------------------------------------------------------------------------

// fetchScenes GET /console/as/support/scenes → [(scene_id, name)]（只读）。
func fetchScenes(a *Auth) []taskObj {
	var out []taskObj
	st, d := a.Get(a.ChatBaseOf(), scenesPath, nil)
	if st == http.StatusOK && codeIsLenient(d) {
		for _, item := range anySlice(mapOf(d["data"])["scenes"]) {
			s := mapOf(item)
			if s == nil || s["id"] == nil {
				continue
			}
			out = append(out, taskObj{ID: asStr(s["id"]), Meta: asStr(s["name"])})
		}
	}
	if len(out) == 0 {
		out = append(out, defaultScenes...)
	}
	return out
}

// fetchMarketExperts 运行时专家市场 POST expert/list → []taskObj。teamOnly 只留 expert_type=team。
func fetchMarketExperts(a *Auth, teamOnly bool, keyword string) []taskObj {
	var out []taskObj
	seen := map[string]bool{}
	for page := 1; page <= 3; page++ {
		body := map[string]any{"page": page, "page_size": 50}
		if keyword != "" {
			body["keyword"] = keyword
		}
		st, r := a.Post(a.ChatBaseOf(), marketListPath, body, nil)
		if st != http.StatusOK {
			break
		}
		experts := anySlice(mapOf(r["data"])["experts"])
		if len(experts) == 0 {
			break
		}
		for _, item := range experts {
			e := mapOf(item)
			if e == nil {
				continue
			}
			eid := asStr(e["expert_id"])
			if eid == "" {
				eid = asStr(e["source_id"])
			}
			if eid == "" || seen[eid] {
				continue
			}
			etype := asStr(e["expert_type"])
			if etype == "" {
				etype = "agent"
			}
			if teamOnly && etype != "team" {
				continue
			}
			seen[eid] = true
			name := asStr(e["display_name_zh"])
			if name == "" {
				name = asStr(e["profession_zh"])
			}
			out = append(out, taskObj{ID: eid, Meta: map[string]any{
				"name":       name,
				"category":   firstElemStr(e["categories"]),
				"expertType": etype,
				"version":    asStr(e["version"]),
				"agent_name": asStr(e["agent_name"]),
			}})
		}
		if len(experts) < 50 {
			break
		}
	}
	// COS 静态清单补足（expert_center.json）：团队专家的主来源。
	if data, err := httpGetJSON(cosExpertURL); err == nil {
		for _, item := range anySlice(data["experts"]) {
			e := mapOf(item)
			if e == nil {
				continue
			}
			eid := asStr(e["id"])
			etype := asStr(e["expertType"])
			if etype == "" {
				etype = "agent"
			}
			if eid == "" || (teamOnly && etype != "team") || seen[eid] {
				continue
			}
			seen[eid] = true
			out = append(out, taskObj{ID: eid, Meta: map[string]any{
				"name":       zhStr(mapOf(e["profession"])["zh"]),
				"category":   asStr(e["categoryId"]),
				"expertType": etype,
				"version":    "",
			}})
		}
	} else {
		fmt.Printf("  [warn] COS 专家清单失败(%v)\n", err)
	}
	if len(out) == 0 {
		if teamOnly {
			for _, id := range defaultTeamIDs {
				out = append(out, taskObj{ID: id, Meta: map[string]any{
					"name": expertNames[id], "category": "",
					"expertType": "team", "version": "",
				}})
			}
		} else {
			for _, id := range defaultExpertIDs {
				out = append(out, taskObj{ID: id, Meta: map[string]any{
					"name": expertNames[id], "category": "",
					"expertType": "agent", "version": "",
				}})
			}
		}
	}
	return out
}

// fetchSkills POST market/skill/list → [(skill_id, name)]（只读，name 是字符串）。
func fetchSkills(a *Auth) []taskObj {
	var out []taskObj
	st, r := a.Post(a.ChatBaseOf(), skillListPath, map[string]any{"page": 1, "page_size": 8}, nil)
	if st == http.StatusOK {
		for _, item := range anySlice(mapOf(r["data"])["skills"]) {
			s := mapOf(item)
			if s == nil {
				continue
			}
			if id := asStr(s["skill_id"]); id != "" {
				out = append(out, taskObj{ID: id, Meta: asStr(s["name"])})
			}
		}
	}
	if len(out) == 0 {
		out = append(out, defaultSkillIDs...)
	}
	return out
}

// fetchPlaybookCases 只读拉灵感案例注册表 → [(case_id, meta)]，失败回落内置表。
func fetchPlaybookCases() []taskObj {
	var out []taskObj
	data, err := httpGetJSON(playbookBase + "/registry.json")
	if err != nil {
		fmt.Printf("  [warn] 案例注册表拉取失败(%v)，回落内置表\n", err)
	} else {
		for _, item := range anySlice(data["cases"]) {
			c := mapOf(item)
			if c == nil {
				continue
			}
			id := asStr(c["id"])
			if id == "" {
				continue
			}
			artifact := asStr(c["artifact_type"])
			if artifact == "" {
				artifact = "other"
			}
			out = append(out, taskObj{ID: id, Meta: map[string]any{
				"title":         asStr(c["title"]),
				"artifact_type": artifact,
				"categories":    nonNilSlice(c["categories"]),
				"skills":        nonNilSlice(c["skills"]),
				"experts":       nonNilSlice(c["experts"]),
				"prompt":        asStr(c["prompt"]),
				"source":        "discover",
			}})
		}
	}
	if len(out) == 0 {
		out = append(out, defaultCases...)
	}
	return out
}

// fetchLighthouse 市场关键词过滤轻量云专家 → [(ex_id, meta)]。
func fetchLighthouse(a *Auth) []taskObj {
	var out []taskObj
	for _, kw := range lighthouseKeywords {
		for _, o := range fetchMarketExperts(a, false, kw) {
			meta := mapOf(o.Meta)
			blob := strings.ToLower(o.ID + asStr(meta["name"]) + asStr(meta["agent_name"]))
			for _, needle := range []string{"lighthouse", "轻量", "light", "yun"} {
				if strings.Contains(blob, needle) {
					out = append(out, o)
					break
				}
			}
		}
	}
	if len(out) == 0 {
		out = append(out, lighthouseIDs...)
	}
	return out
}

// fetchPETheme appearance/resources 目录过滤和平精英 → [(resource_key, meta)]。
func fetchPETheme(a *Auth) []taskObj {
	var out []taskObj
	st, r := a.Post(a.BillBaseOf(), appearancePath, map[string]any{
		"platform": "client", "kind": "theme",
		"version": clientUAVersion(), "lang": "zh-CN",
	}, nil)
	if st == http.StatusOK {
		for _, item := range anySlice(mapOf(r["data"])["resources"]) {
			x := mapOf(item)
			if x == nil {
				continue
			}
			nm := asStr(x["name"])
			if strings.Contains(nm, "和平精英") || strings.Contains(strings.ToLower(nm), "pubg") {
				out = append(out, taskObj{ID: asStr(x["id"]), Meta: map[string]any{
					"name": nm, "vipLevel": asStr(x["vip_level"]),
					"series": asStr(x["series"]), "appearance": asStr(x["appearance"]),
				}})
			}
		}
	}
	if len(out) == 0 {
		out = append(out, peThemes...)
	}
	return out
}

// dedupSlice 去重（保序）后按 offset 偏移取 need 个。
func dedupSlice(src []taskObj, offset, need int) []taskObj {
	seen := map[string]bool{}
	final := make([]taskObj, 0, len(src))
	for _, o := range src {
		if seen[o.ID] {
			continue
		}
		seen[o.ID] = true
		final = append(final, o)
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(final) {
		return nil
	}
	end := offset + need
	if end > len(final) {
		end = len(final)
	}
	return final[offset:end]
}

// idsFor 返回需要上报的对象 id 列表（按源顺序，偏移 offset 避免复用）。
func idsFor(kind string, a *Auth, need, offset int) []taskObj {
	if need <= 0 {
		return nil
	}
	switch kind {
	case "canvas":
		base := nowMS()
		out := make([]taskObj, 0, need)
		for i := 0; i < need; i++ {
			out = append(out, taskObj{ID: fmt.Sprintf("wbx-canvas-%d", base+int64(i))})
		}
		return out
	case "template":
		return dedupSlice(fetchScenes(a), offset, need)
	case "expert":
		return dedupSlice(fetchMarketExperts(a, false, ""), offset, need)
	case "team":
		return dedupSlice(fetchMarketExperts(a, true, ""), offset, need)
	case "skill":
		return dedupSlice(fetchSkills(a), offset, need)
	case "playbook":
		return dedupSlice(fetchPlaybookCases(), offset, need)
	case "lighthouse":
		return dedupSlice(fetchLighthouse(a), offset, need)
	case "skin":
		return dedupSlice(fetchPETheme(a), offset, need)
	case "miniexpert":
		// 专家 id 取自小程序自身的专家市场端点（mp 口径，与 school 段的 school 分类不同源）。
		return dedupSlice(fetchMarketExperts(a, false, ""), offset, need)
	}
	// history/current 不是顺序语义：其余 kind 都是固定事件组按需补 N 次（offset 无意义）。
	switch kind {
	case "chat", "glmchat", "automation", "cat", "richmeow", "buddy5", "library",
		"buddyfirst", "minichat", "schoolseason", "miniautomation", "miniglmchat", "school":
		out := make([]taskObj, 0, need)
		for i := 0; i < need; i++ {
			out = append(out, taskObj{})
		}
		return out
	}
	return nil
}

// httpGetJSON 外部静态资源 GET（COS 专家清单 / 灵感案例注册表）：标准库直读，
// 不经过账号头（这些是公开静态 JSON，带 Bearer 反而是异形请求）。
func httpGetJSON(url string) (map[string]any, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// --------------------------------------------------------------------------
// 桌面指纹事件链（fork desktop.go：召唤三连 + buddyapp 五连）
//
// 6 连对话链与桌面指纹复用 school.go（schoolDesktopChatSequence / schoolDesktopFingerprint，
// 与脚本 desktop_chat_sequence / desktop_fingerprint 逐字段相同）。
// --------------------------------------------------------------------------

// desktopExpertSummonSequence 召唤三连（对齐参考实现 DesktopExpertSummonSequence 的抓包形状）。
func desktopExpertSummonSequence(o taskObj) []map[string]any {
	m := mapOf(o.Meta)
	category := "expert-all"
	if v := asStr(m["category"]); v != "" {
		category = v
	}
	version := asStr(m["version"])
	if version == "" {
		version = "1.0.0"
	}
	name := asStr(m["name"])
	if name == "" {
		name = o.ID
	}
	expertType := asStr(m["expertType"])
	if expertType == "" {
		expertType = "agent"
	}
	return []map[string]any{
		{"eventCode": "web_element_click", "source": o.ID, "type": category,
			"version": version, "elementId": "expert_summon_click",
			"elementName": "立即召唤",
			"pageURL":     "/C:/Program%20Files/WorkBuddy/resources/app.asar/renderer/index.html"},
		{"eventCode": "expert_summon_click", "id": o.ID, "name": name,
			"expertTitle": name, "type": "expert-all", "position": 0,
			"expertType": expertType, "version": version,
			"mode": "LOCAL"},
		{"eventCode": "expert_summoned", "id": o.ID, "name": name,
			"expertTitle": name, "type": "expert-all"},
	}
}

// desktopExpertActualUseLocal mode=LOCAL 的 expert_actual_use（轻量云判据：type 为空、cost=0）。
func desktopExpertActualUseLocal(o taskObj, conv, reqID string) map[string]any {
	m := mapOf(o.Meta)
	name := asStr(m["name"])
	if name == "" {
		name = o.ID
	}
	expertType := asStr(m["expertType"])
	if expertType == "" {
		expertType = "agent"
	}
	version := asStr(m["version"])
	if version == "" {
		version = "1.0.0"
	}
	return map[string]any{
		"eventCode": "expert_actual_use", "mode": "LOCAL", "id": o.ID,
		"name": name, "expertTitle": name, "type": "",
		"expertType": expertType, "source": "builtin",
		"version": version, "cost": 0, "characterCount": 14,
		"conversationId": conv, "requestId": reqID,
		"messageId":      "msg-" + tail(reqID, 8),
		"requestModelId": "fast-model", "requestModelName": "fast-model",
	}
}

// desktopBuddy5Sequence fork DesktopBuddyAppSequence 同款五连「进入 Buddy 应用」事件：
// discover → show → enter_click → auth_confirm_click → bindaccount_skip_click，
// 一次上报即点亮；服务端不校验真实授权（fork 两账号实测）。
func desktopBuddy5Sequence(buddyID, buddyName string) []map[string]any {
	ev := make([]map[string]any, 0, 5)
	mk := func(code string, extra map[string]any) {
		e := map[string]any{"eventCode": code, "mode": "LOCAL",
			"buddyId": buddyID, "buddyName": buddyName}
		for k, v := range extra {
			e[k] = v
		}
		ev = append(ev, e)
	}
	mk("buddyapp_discover_click", map[string]any{})
	mk("buddyapp_show", map[string]any{
		"elementId": buddyID, "elementName": buddyName, "position": 2})
	mk("buddyapp_enter_click", map[string]any{
		"elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": "1"})
	mk("buddyapp_auth_confirm_click", map[string]any{
		"elementId": buddyID, "elementName": buddyName})
	mk("buddyapp_bindaccount_skip_click", map[string]any{
		"elementId": buddyID, "elementName": buddyName})
	return ev
}

// --------------------------------------------------------------------------
// 上报通道（fork ReportDesktopEvent / ReportWebEvent / school.report_events）
// --------------------------------------------------------------------------

// reportDesktopEvents 以桌面指纹向 {chat}/v2/report 批量上报；返回 (status, code 值)。
// 每个事件注入桌面指纹公共字段**覆盖同名键**（顺序即 python 的 dict(e); m.update(fp)）。
func reportDesktopEvents(a *Auth, events []map[string]any) (int, any) {
	fp := schoolDesktopFingerprint(a)
	arr := make([]any, 0, len(events))
	for _, e := range events {
		m := make(map[string]any, len(e)+len(fp))
		for k, v := range e {
			m[k] = v
		}
		for k, v := range fp {
			m[k] = v
		}
		arr = append(arr, m)
	}
	st, r := a.Post(a.ChatBaseOf(), PathReport, arr, desktopHeaders(a))
	return st, r["code"]
}

// reportWebEvent 以 web 域指纹向 https://www.workbuddy.cn/v2/report 上报单事件（Library_read）。
// fork ReportWebEvent：浏览器形状（os/machineId/userAgent），带 x-client-platform: web。
func reportWebEvent(a *Auth, eventCode, pageURL, elementID, elementName string) (int, any) {
	now := nowMS()
	ev := map[string]any{
		"eventCode": eventCode, "timestamp": now, "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": webUA,
		"machineId": schoolDeriveID(a, "webmachine"), "userId": a.UID,
		"userNickname": a.Nick,
	}
	st, r := a.Post(webBase, PathReport, []any{ev}, webEventHeaders(a, pageURL))
	return st, r["code"]
}

// reportMPEvent 按小程序域上报单事件：{codebuddy}/v2/report，body=[event]。
//
// 与桌面/web 通道并列的第三条上报路径，复用 school.go 的 schoolSendReportEvents
// （同一套小程序 UA 头与重试口径）。**不**注入 X-Client-Platform：mp 身份由 body 里的
// 指纹表达（ideName=wx_app_cloud + ideType=WorkBuddy_MP + source=mini_program），
// 只有 list/accept/claim 三个任务端点必须带该头。
func reportMPEvent(a *Auth, ev map[string]any) (int, any) {
	resp, err := schoolSendReportEvents(a, SchoolCloudAgent, []any{ev}, nil)
	if err != nil {
		return -1, nil
	}
	return resp.Status, resp.field("code")
}

// --------------------------------------------------------------------------
// 事件构建器（照抄各 task_*.py 已验证形状，必带 userId）
// --------------------------------------------------------------------------

// buildEvent 按 kind 构造一条上报事件。meta 可能是 dict（丰富元数据）或 str（场景/技能名）。
func buildEvent(a *Auth, kind, objID string, meta any, idx int) (map[string]any, error) {
	now := nowMS()
	cid := fmt.Sprintf("wb-run-%d-%d", now, idx)
	rid := fmt.Sprintf("%s-%d", cid, now)
	uid := a.UID
	m := mapOf(meta)
	if m == nil {
		m = map[string]any{}
		if s, ok := meta.(string); ok && s != "" {
			// 字符串元数据（场景名/技能名）统一成 {"name": ...} 供后续 m["name"] 取值。
			m = map[string]any{"name": s}
		}
	}

	switch kind {
	case "canvas":
		return map[string]any{
			"eventCode": "wbx_design_canvas_task_create", "timestamp": now,
			"reportDelay": 0, "conversationId": cid, "requestId": rid,
			"source": "summon_keyword", "isCustomModel": false, "name": "",
			"inputLength": 12, "id": firstNonEmptyStr(objID, fmt.Sprintf("wbx-canvas-%d", now)),
			"cost": 0, "isSuccessful": true, "userId": uid,
		}, nil

	case "template":
		// 原脚本此处写的是 `"name": m or ""`——非空 dict 会被整个塞进 name（线上一直如此，
		// 服务端接受）。为了不改请求形状，这里保持同一行为，不做「修正」。
		var name any = ""
		if len(m) > 0 {
			name = m
		}
		return map[string]any{
			"eventCode": "agent_task_created_with_template", "timestamp": now,
			"reportDelay": 0, "isCustomModel": true, "id": objID,
			"name": name, "requestId": rid, "conversationId": cid, "userId": uid,
		}, nil

	case "expert":
		name := asStr(m["name"])
		if name == "" {
			name = objID
		}
		return map[string]any{
			"eventCode": "expert_actual_use", "timestamp": now, "reportDelay": 0,
			"mode": "CLOUD", "id": objID, "name": name,
			"expertTitle": asStr(m["name"]), "type": asStr(m["category"]),
			"expertType": firstNonEmptyStr(asStr(m["expertType"]), "agent"), "source": "builtin",
			"version": asStr(m["version"]), "cost": 0, "characterCount": 12,
			"conversationId": cid, "requestId": rid, "messageId": rid,
			"requestModelId": "deepseek-v4-flash", "requestModelName": "DeepSeek V4 Flash",
			"userId": uid,
		}, nil

	case "team":
		name := asStr(m["name"])
		if name == "" {
			name = objID
		}
		return map[string]any{
			"eventCode": "expert_actual_use", "timestamp": now, "reportDelay": 0,
			"mode": "CLOUD", "id": objID, "name": name,
			"expertTitle": asStr(m["name"]), "type": asStr(m["category"]),
			"expertType": "team", "source": "builtin", "version": asStr(m["version"]),
			"cost": 0, "characterCount": 12, "conversationId": cid, "requestId": rid,
			"messageId": rid, "requestModelId": "deepseek-v4-flash",
			"requestModelName": "DeepSeek V4 Flash", "userId": uid,
		}, nil

	case "skill":
		sname := asStr(m["name"])
		return map[string]any{
			"eventCode": "skill_info", "timestamp": now, "reportDelay": 0,
			"skillId": objID, "skillName": firstNonEmptyStr(sname, objID),
			"skillVersion": "", "action": "use", "conversationId": cid,
			"requestId": rid, "userId": uid,
		}, nil

	case "automation":
		return map[string]any{
			"eventCode": "automated_task_create_suc", "timestamp": now,
			"reportDelay": 0, "name": "每周五自动生成周报", "source": "manually",
			"modelId": "deepseek-v4-flash", "modelIsThinking": false,
			"expertId": "", "expertMarketplace": "", "connectorIds": "",
			"connectorCount": 0, "skills": "", "skillCount": 0,
			"scheduleType": "recurring", "pushToWeChat": false, "pushToWecomBot": false,
			"conversationId": cid, "requestId": rid,
			"schedule": map[string]any{
				"type": "recurring", "rrule": "FREQ=WEEKLY;BYDAY=FR;BYHOUR=9;BYMINUTE=0",
			},
			"prompt": "每周五自动整理本周工作，生成一份周报。", "userId": uid,
		}, nil

	case "playbook":
		skills := anySlice(m["skills"])
		experts := anySlice(m["experts"])
		catID := firstElemStr(m["categories"])
		expertID, expertName := "", ""
		if len(experts) > 0 {
			e0 := mapOf(experts[0])
			expertID = asStr(e0["expert_id"])
			if expertID == "" {
				expertID = asStr(e0["id"])
			}
			expertName = asStr(e0["name"])
		}
		title := asStr(m["title"])
		if title == "" {
			title = objID
		}
		artifact := asStr(m["artifact_type"])
		if artifact == "" {
			artifact = "other"
		}
		return map[string]any{
			"eventCode": "playbook_prompt_send", "timestamp": now, "reportDelay": 0,
			"id": objID, "name": title, "type": artifact,
			"promptLength": utf8.RuneCountInString(asStr(m["prompt"])), "isOfficial": 1,
			"skills":     joinField(skills, "skill_id"),
			"skillNames": joinField(skills, "name"),
			"expertId":   expertID, "expertName": expertName,
			"categoryId": catID, "categoryName": "", "query": "",
			"source": "discover", "conversationId": cid, "requestId": rid,
			"ext1": "discover", "userId": uid,
		}, nil

	case "lighthouse":
		// 与 expert 分支同形状（轻量云只是在 select 阶段筛出特定专家）；
		// 真正的点亮在 lightUp 的 lighthouse 分支（召唤三连 + 真实对话六连 + LOCAL actual_use）。
		name := asStr(m["name"])
		if name == "" {
			name = objID
		}
		return map[string]any{
			"eventCode": "expert_actual_use", "timestamp": now, "reportDelay": 0,
			"mode": "CLOUD", "id": objID, "name": name,
			"expertTitle": asStr(m["name"]), "type": asStr(m["category"]),
			"expertType": firstNonEmptyStr(asStr(m["expertType"]), "agent"), "source": "builtin",
			"version": asStr(m["version"]), "cost": 0, "characterCount": 12,
			"conversationId": cid, "requestId": rid, "messageId": rid,
			"requestModelId": "deepseek-v4-flash", "requestModelName": "DeepSeek V4 Flash",
			"userId": uid,
		}, nil

	case "skin":
		vip := asStr(m["vipLevel"])
		if vip == "" {
			vip = "free"
		}
		series := asStr(m["series"])
		if series == "" {
			series = "craft"
		}
		name := asStr(m["name"])
		if name == "" {
			name = objID
		}
		return map[string]any{
			"eventCode": "appearance_skin_apply", "timestamp": now, "reportDelay": 0,
			"action": "apply", "source": "settings_close", "id": objID,
			"vipLevel": vip, "series": series,
			"type": "unknown", "name": name, "userId": uid,
		}, nil

	case "chat", "glmchat", "cat":
		modelID, modelName, mode := "deepseek-v4-flash", "DeepSeek V4 Flash", "craft"
		if kind == "glmchat" || kind == "cat" {
			modelID, modelName = "glm-5.2", "GLM-5.2"
		}
		if kind == "cat" {
			mode = "night"
		}
		return map[string]any{
			"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
			"mode": mode, "conversationId": cid, "requestId": cid,
			"inputLength": 12, "requestModelId": modelID, "requestModelName": modelName,
			"isPlan": false, "isAutoExecuteTerminal": false, "isAutoModify": false,
			"codebaseEnable": false, "maxToken": 0, "maxSteps": 0, "temperature": 0,
			"maxRetries": 0, "mentionContexts": []any{}, "knowledgeId": []any{},
			"knowledgeName": []any{}, "codebaseId": "", "mentionContextCount": 0,
			"command": "", "expertId": "", "recommendId": "", "skillId": "",
			"skillCount": 0, "totalCount": 0, "fileUri": "", "presentAt": now,
			"traceId": "", "rootRequestId": cid, "parentConversationId": cid,
			"agentName": "default", "agentType": "conversation", "userId": uid,
		}, nil

	case "minichat":
		// Sequential_Tasks_1 判据：mini 指纹 chat_request_send，无 activityId
		// （growth 域按 source=mini_program 指纹关联）。形状对齐 school.mini_chat_event，
		// 仅去掉 activityId、extVersion 修正为 2.4.0。
		return map[string]any{
			"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
			"source": "mini_program", "ideName": "wx_app_cloud",
			"ideType": "WorkBuddy_MP", "extName": "workbuddy-mp",
			"extVersion": "2.4.0", "mode": "chat",
			"conversationId": cid, "requestId": cid, "inputLength": 12,
			"mentionContexts": []any{}, "mentionContextCount": 0, "userId": uid,
		}, nil

	case "miniexpert":
		// Sequential_Tasks_2（选中专家并完成有效对话）判据：mp 指纹 expert_actual_use。
		// 机读差异字段：无 conversationId/activityId（真实事件就是都不带）、
		// extVersion 用小程序自身版本 2.2.8、type 固定 "send_message"。
		return map[string]any{
			"eventCode": "expert_actual_use", "timestamp": now, "reportDelay": 0,
			"ideName": "wx_app_cloud", "ideType": "WorkBuddy_MP",
			"extName": "workbuddy-mp", "extVersion": "2.2.8", "product": "SaaS",
			"source": "mini_program", "os": "android", "osVersion": "14",
			"arch": "arm64", "timezone": "Asia/Shanghai",
			"machineId": schoolDeriveID(a, "machine"),
			"userId":    uid, "id": objID, "name": objID,
			"expertTitle": firstNonEmptyStr(asStr(m["name"]), objID), "type": "send_message",
			"characterCount": 12, "expertType": firstNonEmptyStr(asStr(m["expertType"]), "agent"),
		}, nil

	case "miniautomation":
		// Sequential_Tasks_4（小程序内创建定时任务）判据：mp 指纹 automated_task_create_suc。
		// 与桌面 automation_1 同 eventCode，但**不带** schedule/rrule 对象、不带
		// modelId/connector/pushTo* 等桌面字段——小程序事件根本没有这些键。
		return map[string]any{
			"eventCode": "automated_task_create_suc", "timestamp": now,
			"reportDelay": 0, "ideName": "wx_app_cloud",
			"ideType": "WorkBuddy_MP", "ideVersion": "2.2.8",
			"extName": "workbuddy-mp", "extVersion": "2.2.8", "product": "SaaS",
			"source": "mini_program", "mode": "CLOUD",
			"os": "android", "osVersion": "14", "arch": "arm64",
			"timezone":  "Asia/Shanghai",
			"machineId": schoolDeriveID(a, "machine"),
			"userId":    uid, "userNickname": a.Nick,
			"name": "每日读书提醒", "skills": "", "skillCount": 0,
			"scheduleType": "daily",
		}, nil

	case "miniglmchat":
		// Sequential_Tasks_5（选择 GLM5.2 模型并完成有效对话）判据：
		// mp 指纹 chat_request_send **+ 模型上下文**（与 Tasks_1 的唯一新增判据）。
		// conversationId/requestId **必带且必须相等**（源码同值传递）。
		ev := map[string]any{
			"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
			"ideName": "wx_app_cloud", "mode": "chat",
			"source":         "mini_program",
			"conversationId": cid, "requestId": cid, "inputLength": 12,
			"requestModelId": "glm-5.2", "requestModelName": "GLM-5.2",
			"mentionContexts": []any{}, "mentionContextCount": 0,
			"command": "", "traceId": cid,
			"userId": uid,
		}
		for k, v := range mpDeviceFingerprint(a) {
			ev[k] = v
		}
		return ev, nil

	case "schoolseason":
		// growth 域 school_season（校园日）判据：mini 指纹 chat_request_send +
		// activityId=school_open_day_2026（与 school 域开学季同 activityId 关联）。
		// 直接复用 school.go 的 schoolMiniChatEvent，保证字段单一事实源。
		return schoolMiniChatEvent(a, fmt.Sprintf("wb-run-%d-%d", now, idx)), nil
	}
	return nil, fmt.Errorf("unknown kind: %s", kind)
}

// --------------------------------------------------------------------------
// 领奖（M15 真实端点：POST {chat}/activity/growth/tasks/{code}/claim，无 body）
// 400 自动降级 web 域：POST https://www.workbuddy.cn/activity/growth/tasks/{code}/claim，
// 带 Origin/Referer/x-client-platform: web（fork ClaimReward 同款，web_claim_fallback）。
//
// 这里不用 api.go 的 ClaimReward：它只做降级、不产出「claim 400 … -> 降级 web 域」这条
// 进度行，也不支持小程序限定的 X-Client-Platform 头；而 claim 的日志与统计口径是运维
// 判断「已领/新入账/失败」的唯一依据，必须逐字保留。
// --------------------------------------------------------------------------

// claimParse 解析 claim 响应信封；返回 1=新入账 2=already_claimed 0=失败。viaWeb 仅用于日志标注。
func (r *Runner) claimParse(a *Auth, code, uid8 string, st int, resp map[string]any, viaWeb bool) int {
	tag := ""
	if viaWeb {
		tag = " web"
	}
	if st != http.StatusOK || !codeIsZero(resp) {
		fmt.Printf("[task_runner] %s %s: claim%s %d %s -> ERR\n", uid8, code, tag, st, pyVal(resp["msg"]))
		r.Stats.Fail++
		return 0
	}
	data := mapOf(resp["data"])
	credit := asInt(data["credit"])
	energy := asInt(data["energy"])
	if truthy(data["already_claimed"]) {
		fmt.Printf("[task_runner] %s %s: claim%s 200 already_claimed(credit=+%d energy=+%d)\n",
			uid8, code, tag, credit, energy)
		r.Stats.Already++
		r.sleep()
		return 2
	}
	fmt.Printf("[task_runner] %s %s: claim%s 200 ok(credit=+%d energy=+%d)\n",
		uid8, code, tag, credit, energy)
	r.Stats.OK++
	r.Stats.Credit += credit
	r.Stats.Energy += energy
	r.sleep()
	return 1
}

// claimViaWeb web 域领奖（fork ClaimReward 同款头：Origin/Referer/x-client-platform: web）。
func (r *Runner) claimViaWeb(a *Auth, code, uid8 string) int {
	st, resp := a.Post(webBase, PathClaimReward+"/"+code+"/claim", nil, webClaimHeaders(a))
	return r.claimParse(a, code, uid8, st, resp, true)
}

// claimOne 领单个任务。返回 1=新入账 2=already_claimed 0=失败。
//
// chat 域（M15 实测成功）先行；400 则自动降级 web 域带完整头（web_claim_fallback）。
// 幂等语义保持：already_claimed 不算失败（返回 2）。mp=true 时带
// X-Client-Platform: miniprogram（小程序限定任务 claim 必需）。
func (r *Runner) claimOne(a *Auth, code, uid8 string, mp bool) int {
	var hdr map[string]string
	if mp {
		hdr = mpPlatformHeader
	}
	st, resp := a.Post(a.ChatBaseOf(), PathClaimReward+"/"+code+"/claim", nil, hdr)
	if st != http.StatusOK || !codeIsZero(resp) {
		// 400 降级到 web 域（fork 实测 Web 成长中心端点）：chat 域路径对部分任务 400
		if st == http.StatusBadRequest {
			fmt.Printf("[task_runner] %s %s: claim 400 %s -> 降级 web 域\n", uid8, code, pyVal(resp["msg"]))
			return r.claimViaWeb(a, code, uid8)
		}
		fmt.Printf("[task_runner] %s %s: claim %d %s -> ERR\n", uid8, code, st, pyVal(resp["msg"]))
		r.Stats.Fail++
		return 0
	}
	return r.claimParse(a, code, uid8, st, resp, false)
}

// --------------------------------------------------------------------------
// 小程序 growth 限定任务（X-Client-Platform: miniprogram 口径）
// --------------------------------------------------------------------------

// mpListTasks 带 mp 头拉任务列表（小程序限定任务仅在该口径下发）。
func mpListTasks(a *Auth) ([]map[string]any, error) {
	st, r := a.Get(a.ChatBaseOf(), PathListTasks, mpPlatformHeader)
	if st != http.StatusOK {
		return nil, fmt.Errorf("mp list_tasks http=%d", st)
	}
	return mapSlice(mapOf(r["data"])["tasks"]), nil
}

// mpTaskStatus mp 口径查单任务；无此任务返回 nil。
func mpTaskStatus(a *Auth, code string) (map[string]any, error) {
	tasks, err := mpListTasks(a)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if asStr(t["task_code"]) == code {
			return t, nil
		}
	}
	return nil, nil
}

// --------------------------------------------------------------------------
// 运行器（对应脚本的 main / stats / opts）
// --------------------------------------------------------------------------

// Options 与脚本 argparse 一一对应的运行选项。
//
// 注意 DryRun 只是**显式**版本的默认行为（脚本没有 --dry-run 开关：缺省即 dry-run，
// 只有 --yes 才写）；保留它是为了 CLI 可读性与「显式确认」场景，语义上等价于 Yes=false。
type Options struct {
	Yes             bool     // 真实执行写操作（缺省 dry-run）
	OnlyCodes       []string // 只处理指定 task_code（可多次）
	OnlyClaim       bool     // 只领奖不点亮
	Gap             float64  // 写动作间隔秒数（>=1.0）
	AccountDelayMin int      // 账号间最小延迟（秒）
	AccountDelayMax int      // 账号间最大随机延迟（秒，0=关闭）
	DryRun          bool     // 显式 dry-run（等价 Yes=false）
}

// Stats 汇总计数（与脚本 stats dict 同字段）。
type Stats struct {
	Accounts int
	Total    int
	OK       int
	Already  int
	Skip     int
	Pending  int
	Fail     int
	Credit   int
	Energy   int
}

// Runner 一次批量执行的上下文（opts + 累计统计 + 账号间节流）。
type Runner struct {
	opts    Options
	Stats   Stats
	spacing *AccountSpacing
}

// NewRunner 校验选项并构造运行器。间隔区间非法直接报错（0 <= min <= max <= 600）。
func NewRunner(opts Options) (*Runner, error) {
	if opts.Gap < 1.0 {
		opts.Gap = 1.0 // 写操作间隔 ≥1s
	}
	spacing, err := NewAccountSpacing(opts.AccountDelayMin, opts.AccountDelayMax)
	if err != nil {
		return nil, err
	}
	return &Runner{opts: opts, spacing: spacing}, nil
}

// gapDuration 写动作间隔（脚本的 time.sleep(opts.gap)）。
func (r *Runner) gapDuration() time.Duration {
	return time.Duration(r.opts.Gap * float64(time.Second))
}

// sleep 写动作之间的间隔。所有写请求（上报/accept/claim）都过它，
// 固定或过密的节奏在风控眼里就是机器行为。
func (r *Runner) sleep() { time.Sleep(r.gapDuration()) }

// settle 服务端归账留时（脚本里每处 `time.sleep(2.0)`）：事件上报是异步入账的，
// 立刻回读会把「已点亮」读成「未变化」。
func (r *Runner) settle() { time.Sleep(2 * time.Second) }

// ResolvePrefixes 展开账号参数：ALL → auths/workbuddy-*.json 的 uid8 段（basename[10:18]），
// 其余按原样当 uid 前缀；最后去重保序（与脚本 glob+去重口径一致）。
func ResolvePrefixes(accounts []string) []string {
	var prefixes []string
	for _, acc := range accounts {
		if strings.EqualFold(acc, "ALL") {
			hits, _ := filepath.Glob(filepath.Join(AuthsDir(), "workbuddy-*.json"))
			sort.Strings(hits)
			for _, p := range hits {
				prefixes = append(prefixes, UIDPrefixFromFile(filepath.Base(p)))
			}
			continue
		}
		prefixes = append(prefixes, acc)
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// Run 逐个账号执行（对应脚本 main 的账号循环 + 结尾汇总）。
func (r *Runner) Run(prefixes []string) {
	for _, p := range prefixes {
		a, err := LoadAuth(p)
		if err != nil {
			fmt.Printf("ERR: %v\n", err)
			continue
		}
		r.Stats.Accounts++
		// global realm 不适用 CN 任务中心：明确跳过、不发起任何请求（P2 门控结论）。
		if a.IsGlobal() {
			fmt.Printf("[skip] %s global realm 不适用 CN 任务\n", uid8Of(a))
			r.Stats.Skip++
			continue
		}
		r.spacing.Wait()
		r.processAccount(a)
	}
	r.PrintSummary()
}

// PrintSummary 汇总行（与脚本逐字对齐，便于日志对比）。
func (r *Runner) PrintSummary() {
	fmt.Printf("task_runner done: accounts=%d total=%d ok=%d already=%d skipped=%d "+
		"pending=%d fail=%d credit=+%d energy=+%d\n",
		r.Stats.Accounts, r.Stats.Total, r.Stats.OK, r.Stats.Already, r.Stats.Skip,
		r.Stats.Pending, r.Stats.Fail, r.Stats.Credit, r.Stats.Energy)
}

// containsStr 切片包含判定（`code not in opts.only_codes`）。
func containsStr(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// codeAnyOf api.go 的 ReportResult.Code 是字符串（缺失为空）→ 还原成脚本里的
// code 值（None/0），日志外观保持一致。
func codeAnyOf(s string) any {
	if s == "" {
		return nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return float64(n)
	}
	return s
}

// --------------------------------------------------------------------------
// 账号级流程
// --------------------------------------------------------------------------

// processAccount 单账号全流程：growth 主循环 → 小程序限定任务段 → 开学季段。
func (r *Runner) processAccount(a *Auth) {
	uid8 := uid8Of(a)
	fmt.Printf("== %s (%s) ==\n", uid8, a.Nick)

	// 只读 oracle：能量余额（上下文参考，dry-run/REAL 都显示；失败不报错，只是不打印）。
	if st, e := a.Get(a.ChatBaseOf(), energyPath, nil); st == http.StatusOK && codeIsZero(e) {
		var bal any = "?"
		if data := mapOf(e["data"]); data != nil {
			if v, ok := data["balance"]; ok {
				bal = v
			}
		}
		fmt.Printf("[task_runner] %s query energy balance=%s\n", uid8, pyVal(bal))
	}

	tasks, err := a.ListTasks()
	if err != nil {
		fmt.Printf("ERR: [task_runner] %s query list_tasks 失败: %v\n", uid8, err)
		r.Stats.Fail++
		return
	}
	byCode := map[string]map[string]any{}
	for _, t := range tasks {
		byCode[asStr(t["task_code"])] = t
	}

	// growth 映射里带 school 段/mp 段的 code：growth 任务列表里没有它们的正常形态，
	// 由专段处理（growth claim 端点对 school 任务 400；mp 任务在无头列表里不下发）。
	var schoolInMap, mpInMap []string
	for _, c := range taskMappingOrder {
		spec := taskMapping[c]
		if spec.Kind == "school" {
			schoolInMap = append(schoolInMap, c)
		}
		if mpKinds[spec.Kind] {
			mpInMap = append(mpInMap, c)
		}
	}
	special := map[string]bool{}
	for _, c := range schoolInMap {
		special[c] = true
	}
	for _, c := range mpInMap {
		special[c] = true
	}

	var codes []string
	var processSchool, processMinichat bool
	if len(r.opts.OnlyCodes) > 0 {
		for _, c := range r.opts.OnlyCodes {
			if !special[c] {
				codes = append(codes, c)
			}
		}
		for _, c := range r.opts.OnlyCodes {
			if containsStr(schoolInMap, c) {
				processSchool = true
			}
			if containsStr(mpInMap, c) {
				processMinichat = true
			}
		}
	} else {
		for _, c := range taskMappingOrder {
			if special[c] {
				continue
			}
			codes = append(codes, c)
		}
		processSchool, processMinichat = true, true
		// 未在映射表但存在于任务列表的（如 Expert_Philanthropy）——只计数展示
		for _, t := range tasks {
			c := asStr(t["task_code"])
			if c == "" {
				continue
			}
			if _, known := taskMapping[c]; known {
				continue
			}
			if asStr(t["accept_status"]) == "claimed" {
				fmt.Printf("[task_runner] %s %s: query claimed -> 已领，跳过(非映射任务)\n", uid8, c)
				r.Stats.Total++
				r.Stats.Already++
			} else {
				fmt.Printf("[task_runner] %s %s: query %s -> 非映射任务，skip\n",
					uid8, c, pyVal(t["accept_status"]))
				r.Stats.Total++
				r.Stats.Skip++
			}
		}
	}

	for _, code := range codes {
		t, ok := byCode[code]
		if !ok || t == nil {
			fmt.Printf("[task_runner] %s %s: query 任务不存在\n", uid8, code)
			r.Stats.Total++
			r.Stats.Skip++
			continue
		}
		r.processTask(a, code, t)
	}

	// 小程序成长任务段（mp 口径）：已完成/可领状态下 only_claim 仍可入账（claim 幂等）；
	// 未完成则点亮判据属写操作，只在非 only_claim 模式触发。
	if processMinichat {
		if r.opts.OnlyClaim && !r.opts.Yes {
			// only_claim + dry-run：无写操作，专段内部自行按 dry-run 跳过
		} else {
			mpCodes := mpInMap
			if len(r.opts.OnlyCodes) > 0 {
				mpCodes = nil
				for _, c := range r.opts.OnlyCodes {
					if containsStr(mpInMap, c) {
						mpCodes = append(mpCodes, c)
					}
				}
			}
			for _, mpCode := range mpCodes {
				t, err := mpTaskStatus(a, mpCode)
				if err != nil {
					fmt.Printf("[task_runner] %s %s: mp 查询失败: %v\n", uid8, mpCode, err)
					r.Stats.Fail++
					continue
				}
				ast := ""
				if t != nil {
					ast = asStr(t["accept_status"])
				}
				if r.opts.OnlyClaim && ast != "completed" && ast != "claimed" {
					// only_claim 只入账已完成任务；未完成不点亮
					fmt.Printf("[task_runner] %s %s: only_claim 跳过（未 completed）\n", uid8, mpCode)
					continue
				}
				r.processMiniChatTask(a, mpCode, uid8)
			}
		}
	}

	// school 段（小程序开学季）：growth 循环之后执行，判据/端点全复用 school 段实现。
	// only_claim 语义下跳过：school 没有「已 completed 未领」的快路径，点亮判据是写操作，
	// 不应在只领奖模式下触发（实测曾由此误触发写）。
	if processSchool && !r.opts.OnlyClaim {
		r.processSchoolTasks(a, uid8)
	}
}

// --------------------------------------------------------------------------
// 单任务处理（growth 映射任务）
// --------------------------------------------------------------------------

// processTask 处理一个 growth 任务：判定状态 → dry-run 展示 / 直接 claim / lightUp。
func (r *Runner) processTask(a *Auth, code string, t map[string]any) {
	uid8 := uid8Of(a)
	spec, ok := taskMapping[code]
	if !ok {
		fmt.Printf("[task_runner] %s %s: query 非映射任务，skip\n", uid8, code)
		r.Stats.Total++
		r.Stats.Skip++
		return
	}
	ast := asStr(t["accept_status"])
	prog := mapOf(t["progress"])
	cur := asInt(prog["current"])
	target := asInt(prog["target"])
	if target == 0 {
		target = spec.Target
	}
	if target == 0 {
		target = 1
	}
	r.Stats.Total++

	// 不可伪造任务：跳过并如实标注
	if spec.Unforgeable {
		fmt.Printf("[task_runner] %s %s: query %s(%d/%d) -> 不可伪造(%s)，skip\n",
			uid8, code, pyVal(ast), cur, target, spec.Reason)
		r.Stats.Skip++
		return
	}

	// 只领奖模式：对已满足/已领任务执行 claim（已领→already_claimed 幂等），
	// 未满足任务跳过（服务端对未完成任务返回 task not completed，不强行）。
	if r.opts.OnlyClaim {
		if ast != "claimed" && (cur < target && ast != "completed") {
			fmt.Printf("[task_runner] %s %s: query %s(%d/%d) -> 未 completed，only_claim 跳过\n",
				uid8, code, pyVal(ast), cur, target)
			r.Stats.Pending++
			return
		}
		if !r.opts.Yes {
			fmt.Printf("[task_runner] %s %s: query %s(%d/%d) -> only_claim dry-run 跳过\n",
				uid8, code, pyVal(ast), cur, target)
			r.Stats.Pending++
			return
		}
		r.claimOne(a, code, uid8, false)
		return
	}

	// 已领：跳过（除 only_claim 外不重复领）
	if ast == "claimed" {
		fmt.Printf("[task_runner] %s %s: query claimed(%d/%d) -> 已领，跳过\n", uid8, code, cur, target)
		r.Stats.Already++
		return
	}

	claimedOrDone := cur >= target || ast == "completed"
	if claimedOrDone {
		if !r.opts.Yes {
			fmt.Printf("[task_runner] %s %s: query completed(%d/%d) -> 可领(claim)，dry-run 跳过\n",
				uid8, code, cur, target)
			r.Stats.Pending++
			return
		}
		r.claimOne(a, code, uid8, false)
		return
	}

	// black_cat 特殊：时段敏感（夜猫窗口 23-08 CST，窗口内最多补 1 次）
	if code == "black_cat" {
		if !withinNightWindow() {
			fmt.Printf("[task_runner] %s %s: query in_progress(%d/%d) -> 非夜猫窗口(23-08 CST)，skip pending\n",
				uid8, code, cur, target)
			r.Stats.Pending++
			return
		}
		if !r.opts.Yes {
			fmt.Printf("[task_runner] %s %s: query in_progress(%d/%d) -> 夜猫窗口内，可补 1 次，dry-run 跳过\n",
				uid8, code, cur, target)
			r.Stats.Pending++
			return
		}
		r.lightUp(a, code, spec, cur, target, uid8, 1)
		return
	}

	// 可点亮：计算需要上报次数（按 target 补齐）
	need := target - cur
	if need < 0 {
		need = 0
	}
	if need <= 0 {
		if !r.opts.Yes {
			fmt.Printf("[task_runner] %s %s: query %s(%d/%d) -> 可领(claim)，dry-run 跳过\n",
				uid8, code, pyVal(ast), cur, target)
			r.Stats.Already++
			return
		}
		r.claimOne(a, code, uid8, false)
		return
	}

	if !r.opts.Yes {
		ids := idsFor(spec.Kind, a, need, cur)
		shown := make([]string, 0, 6)
		for i, o := range ids {
			if i >= 6 {
				break
			}
			shown = append(shown, o.ID)
		}
		fmt.Printf("[task_runner] %s %s: query %s(%d/%d) -> 可点亮 need=%d id源=%s ids=%s，dry-run 跳过\n",
			uid8, code, pyVal(ast), cur, target, need, spec.Src, pyList(shown))
		r.Stats.Pending++
		return
	}

	r.lightUp(a, code, spec, cur, target, uid8, 0)
}

// acceptWithVerify accept 并验证登记生效：读 results[].status（而非只看 HTTP 200/msg——
// 实测上游存在 200 + msg=OK 但 status 非 accepted、accept 未真正登记的形态，
// 此时上报事件全部不归账，任务永远点不亮）。判定以回读 accept_status 为准
// （响应 status 只是初筛）。返回 true=accept 已生效（本轮或此前）。
func (r *Runner) acceptWithVerify(a *Auth, code, uid8 string) bool {
	for attempt := 1; attempt <= 2; attempt++ {
		st, resp := a.AcceptTasks([]string{code})
		status := ""
		if data := mapOf(resp["data"]); data != nil {
			results := mapSlice(data["results"])
			if len(results) > 0 {
				status = asStr(results[0]["status"])
			} else {
				status = asStr(resp["msg"])
			}
		}
		// 回读确认登记生效（响应可能说 accepted 但服务端未落账）
		ast := "not_accepted"
		if t, _ := a.TaskStatus(code); t != nil {
			if v := asStr(t["accept_status"]); v != "" {
				ast = v
			}
		}
		ok := st == http.StatusOK && status == "accepted" && ast != "not_accepted"
		suffix := ""
		if ok {
			suffix = " -> 生效"
		}
		fmt.Printf("[task_runner] %s %s: accept 尝试%d %d status=%s 回读=%s%s\n",
			uid8, code, attempt, st, pyVal(status), ast, suffix)
		if ok {
			return true
		}
		r.sleep()
	}
	return false
}

// lightUp accept（若未接，带登记验证与重试）→ 按 target 补齐上报 → 回读 → 已满则 claim。
//
// cap > 0 时本轮最多上报 cap 次（black_cat 的「窗口内最多补 1 次」）。
func (r *Runner) lightUp(a *Auth, code string, spec taskSpec, cur, target int, uid8 string, cap int) {
	ast := "not_accepted"
	if t, _ := a.TaskStatus(code); t != nil {
		if v := asStr(t["accept_status"]); v != "" {
			ast = v
		}
	}
	if ast == "not_accepted" {
		if !r.acceptWithVerify(a, code, uid8) {
			fmt.Printf("[task_runner] %s %s: accept 未登记生效，本轮跳过待下次\n", uid8, code)
			r.Stats.Fail++
			return
		}
	}

	tNow, _ := a.TaskStatus(code)
	prog := mapOf(tNow["progress"])
	if truthy(prog["current"]) {
		cur = asInt(prog["current"])
	}
	if truthy(prog["target"]) {
		target = asInt(prog["target"])
	}
	need := target - cur
	if need < 0 {
		need = 0
	}
	if cap > 0 && need > cap {
		need = cap
	}

	if need <= 0 {
		fmt.Printf("[task_runner] %s %s: report 无需上报（%d/%d）\n", uid8, code, cur, target)
	} else {
		switch spec.Kind {
		case "buddyfirst":
			// 领养第一只 Buddy：report（前置解锁）→ agreement → buddy/first
			// （fork adoptBuddy。buddy/first 直接发积分 +300c+8e，任务状态随之 completed；
			// 门槛未达时服务端返回 HTTP 400 "first_buddy task not completed yet"，如实标注）。
			var stR, sc any = nil, nil
			if res := a.ReportActivity(1, r.gapDuration(), "", "", ""); len(res) > 0 {
				stR, sc = res[0].Status, codeAnyOf(res[0].Code)
			}
			fmt.Printf("[task_runner] %s %s: report %s code=%s 前置解锁\n", uid8, code, pyVal(stR), pyVal(sc))
			r.settle()
			stA, ag := a.Post(a.ChatBaseOf(), PathBuddyAgreement, map[string]any{"agree": true}, nil)
			fmt.Printf("[task_runner] %s %s: buddy/agreement -> %d code=%s\n", uid8, code, stA, pyVal(ag["code"]))
			r.sleep()
			stB, bf := a.Post(a.ChatBaseOf(), PathBuddyFirst, map[string]any{}, nil)
			data := mapOf(bf["data"])
			cred := asInt(data["credit"])
			energy := asInt(data["energy"])
			fmt.Printf("[task_runner] %s %s: buddy/first -> %d %s credit=+%d energy=+%d\n",
				uid8, code, stB, pyVal(bf["msg"]), cred, energy)
			// 领养直给积分即任务完成：直接计入入账汇总（后续 claim 为 already_claimed）
			if stB == http.StatusOK && codeIsLenient(bf) {
				r.Stats.OK++
				r.Stats.Credit += cred
				r.Stats.Energy += energy
			}
			r.settle()

		case "richmeow":
			// 桌面指纹 6 连对话事件链（fork DesktopChatSequence），单组即一次完整对话。
			// 事件链复用 school.go 的 schoolDesktopChatSequence（模型固定 fast-model，
			// 与 task_runner 侧所有调用点一致）。
			for i := 0; i < need; i++ {
				ms := nowMS()
				conv := fmt.Sprintf("wb-run-rm-%d-%d", ms, i)
				req := fmt.Sprintf("wb-run-rm-req-%d-%d", ms, i)
				msg := fmt.Sprintf("req-%d-%d-user", ms, i)
				events := schoolDesktopChatSequence(conv, req, msg)
				st, sc := reportDesktopEvents(a, events)
				fmt.Printf("[task_runner] %s %s: report %d/%d %d code=%s deskchain conv=%s\n",
					uid8, code, i+1, need, st, pyVal(sc), conv)
				if i < need-1 {
					r.sleep()
				}
			}
			r.settle()

		case "lighthouse":
			// 轻量云专家：召唤三连 → 真实桌面对话（取服务端 requestId）→ 带 has_expert 的
			// 六连 → expert_actual_use(LOCAL)。不能沿用通用分支：自造 requestId 服务端
			// 不计数（issue #221，进度恒 0/1）。
			ids := idsFor("lighthouse", a, need, cur)
			if len(ids) == 0 {
				fmt.Printf("[task_runner] %s %s: report 无可用对象 id，skip\n", uid8, code)
				r.Stats.Fail++
				return
			}
			for i, expert := range ids {
				eid := expert.ID
				stS, scS := reportDesktopEvents(a, desktopExpertSummonSequence(expert))
				fmt.Printf("[task_runner] %s %s: report summon %d/%d %d code=%s id=%s\n",
					uid8, code, i+1, need, stS, pyVal(scS), eid)
				conv, reqID, errMsg := lighthouseRealChat(a, eid)
				if errMsg != "" {
					fmt.Printf("[task_runner] %s %s: 真实对话失败(%s)，本轮跳过\n", uid8, code, errMsg)
					r.Stats.Fail++
					continue
				}
				events := schoolDesktopChatSequence(conv, reqID, "msg-"+tail(reqID, 8))
				for _, ev := range events {
					if asStr(ev["eventCode"]) == "agent_task_created" {
						ev["has_expert"] = true
						ev["expert_id"] = eid
						ev["expert_name"] = metaName(expert)
						ev["expert_industry_id"] = ""
					}
				}
				events = append(events, desktopExpertActualUseLocal(expert, conv, reqID))
				stR, sc := reportDesktopEvents(a, events)
				fmt.Printf("[task_runner] %s %s: report %d/%d %d code=%s lighthouse req=%s\n",
					uid8, code, i+1, need, stR, pyVal(sc), reqID)
				if i < len(ids)-1 {
					r.sleep()
				}
			}
			r.settle()

		case "buddy5":
			// 五连「进入 Buddy 应用」事件（fork DesktopBuddyAppSequence，两账号实测点亮）；
			// 上报失败（非 200 / code!=0）降级为单发 buddyapp_enter_click（旧形状兜底）。
			buddyID, buddyName := "", "Buddy"
			if pair, ok := buddyApps[code]; ok {
				buddyID, buddyName = pair[0], pair[1]
			}
			for i := 0; i < need; i++ {
				st, sc := reportDesktopEvents(a, desktopBuddy5Sequence(buddyID, buddyName))
				if st != http.StatusOK || !codeIsZeroAny(sc) {
					single := map[string]any{
						"eventCode": "buddyapp_enter_click", "timestamp": nowMS(),
						"reportDelay": 0, "elementId": buddyID,
						"elementName": buddyName, "position": 1,
						"isFirstPage": "0", "userId": a.UID,
					}
					st2, sc2 := reportDesktopEvents(a, []map[string]any{single})
					fmt.Printf("[task_runner] %s %s: report %d/%d %d code=%s buddy5 失败 -> 降级单发 %d code=%s\n",
						uid8, code, i+1, need, st, pyVal(sc), st2, pyVal(sc2))
				} else {
					fmt.Printf("[task_runner] %s %s: report %d/%d %d code=%s buddy5 id=%s\n",
						uid8, code, i+1, need, st, pyVal(sc), buddyID)
				}
				if i < need-1 {
					r.sleep()
				}
			}
			r.settle()

		case "library":
			// web 域 web_element_click（fork ReportWebEvent），base 为 www.workbuddy.cn
			for i := 0; i < need; i++ {
				st, sc := reportWebEvent(a, "web_element_click", libraryDocURL,
					"library_doc_intro_click", "WorkBuddy资料库介绍")
				fmt.Printf("[task_runner] %s %s: report %d/%d %d code=%s web_element_click\n",
					uid8, code, i+1, need, st, pyVal(sc))
				if i < need-1 {
					r.sleep()
				}
			}
			r.settle()

		default:
			ids := idsFor(spec.Kind, a, need, cur)
			if len(ids) == 0 {
				fmt.Printf("[task_runner] %s %s: report 无可用对象 id，skip\n", uid8, code)
				r.Stats.Fail++
				return
			}
			for i, o := range ids {
				ev, err := buildEvent(a, spec.Kind, o.ID, o.Meta, i)
				if err != nil {
					fmt.Printf("[task_runner] %s %s: report 事件构造失败: %v\n", uid8, code, err)
					r.Stats.Fail++
					return
				}
				st, resp := a.Post(a.BillBaseOf(), PathReport, []any{ev}, nil)
				fmt.Printf("[task_runner] %s %s: report %d/%d %d code=%s id=%s\n",
					uid8, code, i+1, need, st, pyVal(resp["code"]), o.ID)
				if i < len(ids)-1 {
					r.sleep()
				}
			}
			r.settle()
		}
	}

	// 回读确认（服务端归账可能异步）
	t2, _ := a.TaskStatus(code)
	prog2 := mapOf(t2["progress"])
	cur2 := asInt(prog2["current"])
	tgt2 := target
	if v, ok := prog2["target"]; ok {
		tgt2 = asInt(v)
	}
	ast2 := "?"
	if t2 != nil {
		if v := asStr(t2["accept_status"]); v != "" {
			ast2 = v
		}
	}
	fmt.Printf("[task_runner] %s %s: query re-read %d/%d accept_status=%s\n", uid8, code, cur2, tgt2, ast2)
	if ast2 != "claimed" && cur2 >= tgt2 {
		r.claimOne(a, code, uid8, false)
	} else if ast2 != "claimed" {
		// 未点亮：如实标注，不强行刷
		fmt.Printf("[task_runner] %s %s: report 未达 target（%d/%d），WARN 待下次\n", uid8, code, cur2, tgt2)
	}
}

// --------------------------------------------------------------------------
// 小程序开学季任务段（独立任务空间 /portal/activity/school/tasks）
//
// 与 growth 任务不同点：accept 动作是 POST /tasks/{code}/viewed；领奖走
// POST /tasks/{code}/claim（school 域专属，不是 growth claim 端点）。
// --------------------------------------------------------------------------

// processSchoolTasks 账号级 school 段：拉 school 任务列表并逐个处理。
// 异常/非活动期如实标注（school 是独立任务空间：这些 code 在 growth 列表里根本不出现）。
func (r *Runner) processSchoolTasks(a *Auth, uid8 string) {
	tasks, inPeriod, err := a.SchoolTasks()
	if err != nil {
		fmt.Printf("[task_runner] %s school/tasks 拉取失败: %v\n", uid8, err)
		r.Stats.Fail++
		return
	}
	if !inPeriod {
		fmt.Printf("[task_runner] %s school 活动非进行期（in_period=false），school 段跳过\n", uid8)
		return
	}
	fmt.Printf("[task_runner] %s school/tasks in_period=%s tasks=%d\n", uid8, pyVal(inPeriod), len(tasks))
	for _, code := range schoolCodes {
		t := schoolFindTask(tasks, code)
		if t == nil {
			continue
		}
		if len(r.opts.OnlyCodes) > 0 && !containsStr(r.opts.OnlyCodes, code) {
			continue
		}
		r.processSchoolTask(a, code, t, uid8)
	}
}

// schoolFindTask 在 school 任务列表里按 code 找任务（脚本 _school_find）。
func schoolFindTask(tasks []map[string]any, code string) map[string]any {
	for _, t := range tasks {
		if asStr(t["task_code"]) == code {
			return t
		}
	}
	return nil
}

// processSchoolTask 单个 school 任务：viewed 激活 → 判据触发（按 target-进度 循环）→ 回读 → claim。
//
// 判据触发复用 school.go 的 schoolMakeReportEvents / schoolSendReportEvents /
// (*Auth).schoolShareComplete（与 school_bin 同一份抓包形状，不在这里手抄第二份）；
// 幂等：completed/claimed 是正常态，直接跳过。
func (r *Runner) processSchoolTask(a *Auth, code string, t map[string]any, uid8 string) {
	refetch := func() map[string]any {
		ts, _, err := a.SchoolTasks()
		if err != nil {
			return nil
		}
		return schoolFindTask(ts, code)
	}

	status := schoolStatusOf(t)
	cur := schoolOrZero(t["progress"])
	target := schoolOrOne(t["target_count"])
	reward := schoolOrZero(t["reward_credit"])
	r.Stats.Total++

	if status == "completed" || status == "claimed" {
		fmt.Printf("[task_runner] %s %s: query %s(%d/%d) -> 已完成/已领，跳过\n",
			uid8, code, status, cur, target)
		r.Stats.Already++
		return
	}

	reportKind, mapped := schoolReportKind[code]
	if !mapped {
		fmt.Printf("[task_runner] %s %s: query %s -> school 未映射(人工/未知)，skip\n", uid8, code, status)
		r.Stats.Skip++
		return
	}

	if !r.opts.Yes {
		fmt.Printf("[task_runner] %s %s: query %s(%d/%d) reward=%d -> school %s 判据（dry-run 跳过）\n",
			uid8, code, status, cur, target, reward, reportKind)
		r.Stats.Pending++
		return
	}

	// 1) pending -> viewed 激活（「接任务」，H5 行为；复用 school 模块端点）
	if status == "pending" {
		resp, err := a.schoolViewed(code)
		if err != nil {
			fmt.Printf("[task_runner] %s %s: viewed 失败: %v\n", uid8, code, err)
			r.Stats.Fail++
			return
		}
		fmt.Printf("[task_runner] %s %s: viewed 激活 %d code=%s\n",
			uid8, code, resp.Status, schoolCodeText(resp))
		r.sleep()
	}

	// 2) 判据触发：share 一次即完成；desktop_seq 一次 6 连 = 一次对话；
	//    mini_chat/expert 按 target-当前进度 循环（每次事件独立 +1）。
	rounds := 1
	if reportKind != "share" && reportKind != "desktop_seq" {
		rounds = target - cur
		if rounds < 1 {
			rounds = 1
		}
	}

	progressed := false
	for ri := 0; ri < rounds; ri++ {
		var (
			resp schoolResp
			err  error
		)
		if reportKind == "share" {
			resp, err = a.schoolShareComplete()
		} else {
			var events []any
			var host string
			var extra map[string]string
			events, host, extra, err = schoolMakeReportEvents(a, reportKind)
			if err == nil {
				resp, err = schoolSendReportEvents(a, host, events, extra)
			}
		}
		if err != nil {
			fmt.Printf("[task_runner] %s %s: report 失败 #%d: %v\n", uid8, code, ri+1, err)
			r.Stats.Fail++
			break
		}
		fmt.Printf("[task_runner] %s %s: report %d/%d %d code=%s (%s)\n",
			uid8, code, ri+1, rounds, resp.Status, schoolCodeText(resp), reportKind)
		r.sleep()
		t2 := refetch()
		if t2 == nil {
			continue
		}
		after := schoolStatusOf(t2)
		afterProg := schoolOrZero(t2["progress"])
		if afterProg > cur {
			cur, progressed = afterProg, true
		}
		if after == "completed" || after == "claimed" || afterProg >= target {
			break
		}
	}

	// 3) 回读 + claim（school 域端点，发抽奖机会/积分）
	t2 := refetch()
	if t2 == nil {
		t2 = t
	}
	after := schoolStatusOf(t2)
	afterProg := schoolOrZero(t2["progress"])
	if after == "claimed" {
		fmt.Printf("[task_runner] %s %s: %s/%d -> claimed（本轮已入账）\n", uid8, code, status, cur)
		r.Stats.Already++
		return
	}
	if after == "completed" || afterProg >= target {
		fmt.Printf("[task_runner] %s %s: %s -> %s/%d（点亮）\n", uid8, code, status, after, afterProg)
		resp, err := a.schoolClaim(code)
		if err != nil {
			fmt.Printf("[task_runner] %s %s: claim 失败: %v（可稍后补领）\n", uid8, code, err)
			r.Stats.Fail++
			return
		}
		fmt.Printf("[task_runner] %s %s: claim %d code=%s\n", uid8, code, resp.Status, schoolCodeText(resp))
		r.Stats.OK++
		r.sleep()
		return
	}
	if progressed {
		fmt.Printf("[task_runner] %s %s: %s -> %s/%d（部分点亮，未达 target）\n",
			uid8, code, status, after, afterProg)
		r.Stats.OK++
	} else {
		fmt.Printf("[task_runner] %s %s: %s -> %s/%d（未变化）\n", uid8, code, status, after, afterProg)
		r.Stats.Pending++
	}
}

// --------------------------------------------------------------------------
// 小程序 growth 限定任务（mp 口径）
// --------------------------------------------------------------------------

// processMiniChatTask 小程序成长任务：mp 口径查询 → accept → mini 事件上报 → 回读 → claim。
//
// 与 processTask 的 growth 主循环分开处理：该类任务在默认（无 mp 头）列表里不存在，
// accept/claim 也要求同一头（缺头 accept 返回 task not found，实测）。
// 适用于同一 mp 下发口径的 Sequential_Tasks_1（minichat，无 activityId）、
// school_season（mini chat + activityId）、Sequential_Tasks_2/4/5——仅事件形状不同，
// 链路（accept/claim/回读）完全一致。spec 由 taskMapping 提供，未映射的 mp 任务保守跳过。
func (r *Runner) processMiniChatTask(a *Auth, code, uid8 string) {
	spec := taskMapping[code]
	kind := spec.Kind
	if kind == "" {
		kind = "minichat"
	}
	t, err := mpTaskStatus(a, code)
	if err != nil {
		fmt.Printf("[task_runner] %s %s: mp list_tasks 失败: %v\n", uid8, code, err)
		r.Stats.Fail++
		return
	}
	if t == nil {
		fmt.Printf("[task_runner] %s %s: mp 口径任务不存在，skip\n", uid8, code)
		r.Stats.Skip++
		return
	}

	ast := asStr(t["accept_status"])
	prog := mapOf(t["progress"])
	cur := asInt(prog["current"])
	// 未 accept 的 mp 任务 progress 为 null（实测 Sequential_Tasks_3 未激活时全空），
	// 此时 target 必须由 taskMapping 提供——否则多元任务（Tasks_3 target=5）只上报 1 次。
	target := asInt(prog["target"])
	if target == 0 {
		target = spec.Target
	}
	if target == 0 {
		target = 1
	}
	r.Stats.Total++

	if ast == "claimed" {
		fmt.Printf("[task_runner] %s %s: query claimed(%d/%d) -> 已领，跳过\n", uid8, code, cur, target)
		r.Stats.Already++
		return
	}
	// 未到上线时间（服务端 locked + unlock_at，如 Sequential_Tasks_5 的 2026-09-25）：
	// accept 实测被拒（"task locked until …"），此时上报/claim 均无意义。按日期自终止——
	// 解锁后同一命令无需改参数即自然开始执行；解锁前记 pending（可做但条件未到），
	// 不记 fail（既非错误也非死任务）。
	if truthy(t["locked"]) {
		fmt.Printf("[task_runner] %s %s: query locked（解锁 %s）-> 未到上线时间，跳过\n",
			uid8, code, pyVal(t["unlock_at"]))
		r.Stats.Pending++
		return
	}
	if ast == "completed" || cur >= target {
		if !r.opts.Yes {
			fmt.Printf("[task_runner] %s %s: query completed(%d/%d) -> 可领(claim)，dry-run 跳过\n",
				uid8, code, cur, target)
			r.Stats.Pending++
			return
		}
		r.claimOne(a, code, uid8, true)
		return
	}

	if !r.opts.Yes {
		fmt.Printf("[task_runner] %s %s: query %s(%d/%d) -> 可点亮(%s)，dry-run 跳过\n",
			uid8, code, pyVal(ast), cur, target, kind)
		r.Stats.Pending++
		return
	}

	// 判据上报的对象 id 先经 idsFor 解析（miniexpert 需真实专家 id，空 id 服务端不入账）；
	// 必须在 accept 之前判定——否则会留下「已登记未上报」的半程态（下次仍需 accept 分支
	// 才能补报），宁可整任务不动作。
	need := target - cur
	if need < 1 {
		need = 1
	}
	ids := idsFor(kind, a, need, cur)
	if len(ids) == 0 {
		fmt.Printf("[task_runner] %s %s: %s 无可用对象 id，WARN 跳过点亮\n", uid8, code, kind)
		r.Stats.Pending++
		return
	}
	if len(ids) < need {
		// 对象池不够本轮目标（如专家市场返回数少于 need）：按可用数上报，剩余下次补
		// （回读会如实报出缺口并记 pending，不伪造 id 凑数）。
		fmt.Printf("[task_runner] %s %s: 可用对象 id %d < 需 %d，本轮按可用数上报\n",
			uid8, code, len(ids), need)
	}

	// 1) accept（mp 头；缺头实测 task not found）
	if ast == "not_accepted" {
		st, resp := a.Post(a.ChatBaseOf(), PathAcceptTasks,
			map[string]any{"task_codes": []string{code}}, mpPlatformHeader)
		var res any
		if data := mapOf(resp["data"]); data != nil {
			results := mapSlice(data["results"])
			if len(results) > 0 {
				res = results[0]["status"]
			}
		}
		fmt.Printf("[task_runner] %s %s: accept %d %s\n", uid8, code, st, pyVal(res))
		r.sleep()
		if st != http.StatusOK || asStr(res) != "accepted" {
			r.Stats.Fail++
			return
		}
	}

	// 2) 判据上报：mini 指纹事件（minichat/miniglmchat 无 activityId /
	//    schoolseason 带 activityId；miniexpert/miniautomation 各自事件码），
	//    走 codebuddy.cn 域（小程序域上报通道）
	for i, o := range ids {
		ev, err := buildEvent(a, kind, o.ID, o.Meta, i)
		if err != nil {
			fmt.Printf("[task_runner] %s %s: report 事件构造失败: %v\n", uid8, code, err)
			r.Stats.Fail++
			return
		}
		st, sc := reportMPEvent(a, ev)
		fmt.Printf("[task_runner] %s %s: report %d/%d %d code=%s (mini growth)\n",
			uid8, code, i+1, need, st, pyVal(sc))
		r.sleep()
	}

	r.settle() // 服务端归账留时

	// 3) 回读 + claim（mp 头）
	t2, err := mpTaskStatus(a, code)
	if err != nil || t2 == nil {
		t2 = t
	}
	ast2 := asStr(t2["accept_status"])
	prog2 := mapOf(t2["progress"])
	cur2 := asInt(prog2["current"])
	target2 := asInt(prog2["target"])
	if target2 == 0 {
		target2 = spec.Target
	}
	if target2 == 0 {
		target2 = 1
	}
	fmt.Printf("[task_runner] %s %s: query re-read %d/%d accept_status=%s\n",
		uid8, code, cur2, target2, ast2)
	if ast2 == "claimed" {
		fmt.Printf("[task_runner] %s %s: %s -> claimed（本轮已入账）\n", uid8, code, ast)
		r.Stats.Already++
		return
	}
	if ast2 == "completed" || cur2 >= target2 {
		r.claimOne(a, code, uid8, true)
		return
	}
	fmt.Printf("[task_runner] %s %s: report 未达 target（%d/%d），WARN 待下次\n", uid8, code, cur2, target2)
	r.Stats.Pending++
}

// --------------------------------------------------------------------------
// Expert_lighthouse 的真实对话链（issue #221）
//
// 服务端对这条任务的判据与 expert_5 不同：需要「召唤三连 + 带 has_expert 的桌面对话六连
// + expert_actual_use」，且 actual_use 的 requestId 必须是 chat 接口返回的服务端 id
// （cmb-… / 32 位 hex）。自造 UUID 上报 200 但不计数，进度永远停在 0/1。
// --------------------------------------------------------------------------

// lighthouseRealChat 发一次真实桌面对话，返回 (conversation_id, server_request_id, errMsg)。
//
// 只负责建立对话并取服务端 requestId；失败时返回可读原因，由调用方跳过本轮，
// 不退回自造 UUID（那正是这条任务一直 0/1 的原因）。
func lighthouseRealChat(a *Auth, expertID string) (string, string, string) {
	nowNS := time.Now().UnixNano()
	conv := fmt.Sprintf("wb2api-conv-%d", nowNS)
	body := map[string]any{
		"model": "fast-model",
		"messages": []any{
			map[string]any{"role": "system",
				"content": "You are a helpful assistant. 当前处于中文环境，使用简体中文回答。"},
			map[string]any{"role": "user", "content": "1+1等于几？直接回答。"},
		},
		"agent": "cli", "temperature": 1, "stream": true,
		"stream_options": map[string]any{"include_usage": true},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return conv, "", truncate(err.Error(), 200)
	}
	req, err := http.NewRequest(http.MethodPost, a.ChatBaseOf()+PathChat, bytes.NewReader(raw))
	if err != nil {
		return conv, "", truncate(err.Error(), 200)
	}
	for k, v := range a.headers() {
		req.Header.Set(k, v)
	}
	for k, v := range map[string]string{
		"Accept":              "text/event-stream",
		"User-Agent":          desktopUA,
		"X-Domain":            a.ChatBaseOf(),
		"X-Product":           "SaaS",
		"X-User-Id":           a.UID,
		"X-Conversation-ID":   conv,
		"X-Request-ID":        strconv.FormatInt(nowNS, 10),
		"X-Agent-Intent":      "craft",
		"X-Agent-Type":        "main",
		"X-IDE-Name":          "WorkBuddy",
		"X-IDE-Type":          "WorkBuddy",
		"X-IDE-Version":       "5.5.6",
		"x-codebuddy-request": "1",
		"X-Expert-Id":         expertID,
	} {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		return conv, "", truncate(err.Error(), 200)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return conv, "", fmt.Sprintf("chat http %d", resp.StatusCode)
	}
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
				var obj map[string]any
				if err := json.Unmarshal([]byte(payload), &obj); err != nil {
					continue
				}
				if rid, ok := obj["id"].(string); ok && lighthouseIDRe.MatchString(rid) {
					return conv, rid, ""
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	return conv, "", "SSE 中未找到服务端 requestId"
}
