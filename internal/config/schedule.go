// Package config 存放跨命令共享的配置段与默认值逻辑。
//
// 起因 issue #49：cmd/activity 一次性触发器曾自复制一份精简 schedule 结构体，
// 只 json.Unmarshal 无默认值，键缺席 → Go 零值 0 → scheduler 归一为 1，
// 与 cmd/server 主程序缺省 5 条漂移。把 Schedule 段 + 默认值/归一化抽到本包，
// 两个命令共用同一份定义，消除漂移源头。
package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	// 内嵌 IANA 时区数据库：容器（alpine/distroless/scratch）里常常没有
	// /usr/share/zoneinfo，不嵌就得依赖用户装 tzdata，否则 "Asia/Shanghai" 解析失败。
	// 代价约 450KB 二进制体积，换「排程时区永远可用」。
	_ "time/tzdata"

	"wp/internal/configdata"
)

// Schedule 排程配置段（对应 config.json 的 "schedule" 对象）。
//
// 六类独立排程：签到 / 活跃上报 / 猫猫旅行 / token keepalive / 开学季 / 夜猫子。
// cmd/server 与 cmd/activity 共用本结构，默认值由 DefaultSchedule 填充、
// 缺省归一由 Normalize 完成——两命令走同一份语义，不再各自复制。
type Schedule struct {
	// Timezone 六类排程共用的时区：所有 *_time 时刻都按它解释——"09:00" 指
	// **该时区的** 09:00，而不是容器本地时区（Docker 默认 UTC，会把中国的
	// 早上 9 点签到错跑到北京时间 17:00）。
	//
	// 取值：IANA 名（"Asia/Shanghai"、"UTC"）｜固定偏移（"UTC+8"、"GMT-5"、"+05:30"）｜
	// "Local"（进程本地时区）。空 = 默认 "Asia/Shanghai"（GMT+8）。
	//
	// 注意：**上游的"自然日"边界不跟这项走**——签到幂等、连登奖励、领养次数这些由
	// 上游按 CST 判定（见 scheduler.travelDay / pool.checkinDay），与这里无关。
	Timezone string `json:"timezone"`
	// Location Timezone 的解析结果（不落盘），供 scheduler 直接用，避免每轮唤醒再解析。
	Location               *time.Location `json:"-"`
	AccountDelayMinSeconds int            `json:"account_delay_min_seconds"`
	AccountDelayMaxSeconds int            `json:"account_delay_max_seconds"`
	// RandomDelayMinutes spreads each enabled daily slot within its scheduled
	// hour. Zero preserves the exact-hour schedule; the supported maximum is 60.
	RandomDelayMinutes int `json:"random_delay_minutes"`
	// SpreadWindow 账号之间的随机分布窗口（时长字符串，如 "15m"）：
	// 定时任务到点后，每个账号的执行时刻 = 时点 + uniform(0, SpreadWindow)，
	// 避免整池账号在同一秒打上游。空 = 默认 15m；"0" = 关闭（回到固定间隔顺序执行）。
	// 与上面的 RandomDelayMinutes 分工：那个抖的是**整批起点**的小偏移，
	// 这个摊的是**账号之间**的分布。
	SpreadWindow string `json:"spread_window"`
	// IntraRequestJitterMin/Max 同一账号内连续请求之间的随机间隔区间（时长字符串，
	// 如 "1s" / "3s"）。签到链路「刷新 → 签到 → 查余额」、旅行「查有无猫 → 查状态 →
	// 派出/领奖」这些同号同秒背靠背的请求，各步之间按 uniform[min,max] 等待。
	// 两者都 "0" = 关闭；min>max 在加载期报错。
	IntraRequestJitterMin string `json:"intra_request_jitter_min"`
	IntraRequestJitterMax string `json:"intra_request_jitter_max"`

	// ── 遍历节奏（时长字符串；"0" = 关闭/不等）────────────────────────────
	// 这些以前硬编码在 internal/scheduler 里，现在统一由 config.default.json 提供。
	CheckinAccountDelay  string `json:"checkin_account_delay"`  // 签到相邻账号最小间隔（默认 3s）
	TravelAccountDelay   string `json:"travel_account_delay"`   // 旅行相邻账号间隔（默认 800ms）
	ActivityAccountDelay string `json:"activity_account_delay"` // 活跃上报相邻账号间隔（默认 800ms）
	CheckinRefreshSkew   string `json:"checkin_refresh_skew"`   // 签到前多久判定 token 将过期（默认 10m）
	// BalanceRefreshInterval 定期刷新所有账号余额的间隔（时长字符串，默认 30m；"0" = 关闭）。
	// 每次逐号调用只读 billing 接口（UserResource），把权威余额回写池；负差额会记进
	// 积分日账本的"调整"（积分过期/上游回收/本地估算误差）。
	BalanceRefreshInterval string `json:"balance_refresh_interval"`
	WakeupGrace            string `json:"wakeup_grace"`   // 迟到唤醒补跑前的网络宽限（默认 5s）
	CatchupWindow          string `json:"catchup_window"` // 补跑窗口，超过则放弃老槽位（默认 24h）
	// CheckinRetryDelays 签到遇到上游瞬时拒绝（5xx / code 10001）时的退避序列（默认 5s,15s,60s）。
	CheckinRetryDelays []string `json:"checkin_retry_delays"`

	// 以下为解析结果（json:"-"），供 scheduler 直接使用。
	CheckinAccountDelayDur    time.Duration   `json:"-"`
	TravelAccountDelayDur     time.Duration   `json:"-"`
	ActivityAccountDelayDur   time.Duration   `json:"-"`
	CheckinRefreshSkewDur     time.Duration   `json:"-"`
	BalanceRefreshIntervalDur time.Duration   `json:"-"`
	WakeupGraceDur            time.Duration   `json:"-"`
	CatchupWindowDur          time.Duration   `json:"-"`
	CheckinRetryDelaysDur     []time.Duration `json:"-"`

	// ── 六类任务的执行时刻 ────────────────────────────────────────────────
	// 值形如 "09:30"（24 小时制，精确到分）；一个任务可配多个时刻。
	// 空数组/null = 未配置 → 回落默认（见 DefaultSchedule）。
	// 「关闭某类任务」请设对应的 *_enabled=false，不要靠删空数组表意。
	CheckinTime   []string `json:"checkin_time"`
	TravelTime    []string `json:"travel_time"`
	ActivityTime  []string `json:"activity_time"`
	KeepaliveTime []string `json:"keepalive_time"`
	SchoolTime    []string `json:"school_time"`
	CatTime       []string `json:"cat_time"`

	// ── 解析结果：自 00:00 起的分钟数（0..1439），升序去重 ──────────────────
	// 由 Normalize 填充，供 scheduler 直接使用（避免运行期反复解析字符串）。
	CheckinAt   []int `json:"-"`
	TravelAt    []int `json:"-"`
	ActivityAt  []int `json:"-"`
	KeepaliveAt []int `json:"-"`
	SchoolAt    []int `json:"-"`
	CatAt       []int `json:"-"`

	// 显式禁用开关（缺省 true）。
	//
	// 为什么用独立 bool 而不是空数组/哨兵值表意"禁用"：
	//   - 空数组与 null 在老语义里已被"未配置 → 回落默认"占用，改判会静默翻转
	//     所有老 config 的行为（用户只想删掉一行，结果关掉了签到）；bool 缺省 true
	//     则对老配置零影响，向后完全兼容。
	//   - 开关与取值解耦：禁用时仍保留用户显式配的时刻，重新启用无需补配。
	CheckinEnabled   bool `json:"checkin_enabled"`   // 缺省 true；false = 关签到
	TravelEnabled    bool `json:"travel_enabled"`    // 缺省 true；false = 完全停猫猫旅行
	ActivityEnabled  bool `json:"activity_enabled"`  // 缺省 true；false = 停活跃上报
	KeepaliveEnabled bool `json:"keepalive_enabled"` // 缺省 true；false = 关 token 保活
	SchoolEnabled    bool `json:"school_enabled"`    // 缺省 true；false = 停开学季任务
	CatEnabled       bool `json:"cat_enabled"`       // 缺省 true；false = 停夜猫子任务
	// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
	// 默认 5 条把 chat_5 刷满；0/缺省=1 兼容旧行为。
	ActivityReportCount int `json:"activity_report_count"`

	// StartupCatchup 补跑：进程启动时（容器升级 / 删了重建 / 崩溃重启）**以及运行期新
	// 纳管账号时**（扫码加号 / 手工放凭证文件），如果发现"今天已经过点、但签到或猫猫
	// 旅行还没做完"，立刻按正常节奏补一轮。
	//
	// 解决的是两种空洞：升级通常几分钟，正好盖住 09:00 这种时点；新号若在当天最后一个
	// 时点之后才入池，当天同样等于白给。而签到 / 旅行都按上游自然日结算，错过当天
	// 补不回来（详见 internal/scheduler/startup.go）。
	// 缺省 true；不想在启动/加号瞬间产生上游流量时置 false。
	StartupCatchup bool `json:"startup_catchup"`
}

// DefaultTimezone 排程默认时区：Asia/Shanghai（GMT+8，中国无夏令时）。
//
// 选它而不是容器本地时区的理由：定时任务的时刻是**人的作息**（早上 9 点签到、
// 晚上 22 点保活），只有按北京时间解释才符合预期；而容器默认 TZ 是 UTC，
// 同样一份 "09:00" 会变成北京时间 17:00 跑。上游本身也按 CST 刷新自然日，
// 两者对齐后"一天的签到"与"一天的连登"落在同一个自然日里。
const DefaultTimezone = "Asia/Shanghai"

// tzOffsetRe 固定偏移写法：UTC+8 / GMT-5 / +05:30 / -0800（无夏令时地区够用）。
var tzOffsetRe = regexp.MustCompile(`(?i)^(?:utc|gmt)?([+-])(\d{1,2})(?::?(\d{2}))?$`)

// ParseTimezone 解析排程时区。
//
//	""        → DefaultTimezone（Asia/Shanghai）
//	"Local"   → 进程本地时区（老行为，容器里通常就是 UTC）
//	IANA 名   → time.LoadLocation（时区库已随二进制内嵌，见文件头 import）
//	"UTC+8"   → 固定偏移（LoadLocation 不认这些写法，单独解析）
//
// 解析不出来直接报错：静默回落会让"我明明配了时区"变成更难查的错。
func ParseTimezone(name string) (*time.Location, error) {
	s := strings.TrimSpace(name)
	if s == "" {
		s = DefaultTimezone
	}
	if strings.EqualFold(s, "local") {
		return time.Local, nil
	}
	// "utc"/"Utc" 这类大小写变体：IANA 名区分大小写，配置里手写很容易写错，直接兜住。
	if strings.EqualFold(s, "utc") {
		return time.UTC, nil
	}
	if loc, err := time.LoadLocation(s); err == nil {
		return loc, nil
	}
	if m := tzOffsetRe.FindStringSubmatch(s); m != nil {
		h, errH := strconv.Atoi(m[2])
		min := 0
		var errM error
		if m[3] != "" {
			min, errM = strconv.Atoi(m[3])
		}
		if errH == nil && errM == nil && h <= 14 && min < 60 {
			offset := h*3600 + min*60
			if m[1] == "-" {
				offset = -offset
			}
			return time.FixedZone(s, offset), nil
		}
	}
	return nil, fmt.Errorf("无法识别的时区 %q；用 IANA 名（如 \"Asia/Shanghai\"、\"UTC\"）、"+
		"固定偏移（如 \"UTC+8\"）或 \"Local\"", name)
}

// DescribeTimezone 时区的日志展示：`Asia/Shanghai (UTC+08:00)`。
func DescribeTimezone(loc *time.Location) string {
	if loc == nil {
		loc = time.Local
	}
	_, off := time.Now().In(loc).Zone()
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	return fmt.Sprintf("%s (UTC%s%02d:%02d)", loc.String(), sign, off/3600, (off%3600)/60)
}

// DefaultSchedule 返回排程段的默认值 —— **直接取自内嵌的 config.default.json**。
//
// 默认值只有一处权威来源（internal/configdata/config.default.json），这里不再写死任何
// 数字：想改默认时刻/开关/上报条数，改那份 JSON 即可（根目录的 config.default.json 是
// 它的对外副本，用 `go run ./cmd/config-default -write` 同步）。
//
// 开关「缺省 true」也来自示例里的 true：调用方先取 DefaultSchedule 再用 json.Unmarshal
// 覆盖，键缺席（或为 null）时字段原样保留，只有显式 false 才关。
func DefaultSchedule() Schedule {
	var wrapper struct {
		Schedule Schedule `json:"schedule"`
	}
	if err := json.Unmarshal(configdata.Bytes(), &wrapper); err != nil {
		// 内嵌文件在编译期就确定了：解析失败只能是构建问题，带病运行更难查。
		panic("内置 config.default.json 的 schedule 段无法解析: " + err.Error())
	}
	return wrapper.Schedule
}

// ParseClock 解析 "HH:MM"（也接受 "H:MM"）为自 00:00 起的分钟数。
func ParseClock(v string) (int, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return 0, fmt.Errorf("空时刻")
	}
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("%q 不是 HH:MM 形式", v)
	}
	h, errH := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, errM := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errH != nil || errM != nil {
		return 0, fmt.Errorf("%q 不是 HH:MM 形式", v)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q 超出范围（HH 0-23 / MM 0-59）", v)
	}
	return h*60 + m, nil
}

// FormatClock 把分钟数渲染成 "HH:MM"（日志与报错里用）。
func FormatClock(min int) string {
	if min < 0 {
		min = 0
	}
	min %= 24 * 60
	return fmt.Sprintf("%02d:%02d", min/60, min%60)
}

// schedulePairs 六类任务的字段对应关系（时刻 *_time / 解析结果 *_At / 默认值）。
//
// 默认值只用于「显式写了 null 或空数组」这种边角情况；**主路径的默认值来自
// config.default.json**（内嵌，见 DefaultSchedule/Default）。
func (s *Schedule) schedulePairs() []struct {
	field   string
	times   *[]string
	minutes *[]int
	def     []string
} {
	return []struct {
		field   string
		times   *[]string
		minutes *[]int
		def     []string
	}{
		{"checkin", &s.CheckinTime, &s.CheckinAt, []string{"09:00", "21:00"}},
		{"travel", &s.TravelTime, &s.TravelAt, []string{"09:00", "21:00"}},
		{"activity", &s.ActivityTime, &s.ActivityAt, []string{"10:00"}},
		{"keepalive", &s.KeepaliveTime, &s.KeepaliveAt, []string{"22:00"}},
		{"school", &s.SchoolTime, &s.SchoolAt, []string{"12:00"}},
		{"cat", &s.CatTime, &s.CatAt, []string{"01:00"}},
	}
}

// Normalize 归一化排程段：补默认值、解析成分钟数、校验并排序。
//
// 「禁用」一律走 *_enabled=false，与空值语义互不混淆（空数组/缺省 = 回落默认）。
func (s *Schedule) Normalize() error {
	if s.AccountDelayMinSeconds < 0 || s.AccountDelayMaxSeconds > 600 ||
		s.AccountDelayMaxSeconds < s.AccountDelayMinSeconds {
		return fmt.Errorf("schedule account delays require 0 <= min <= max <= 600 seconds")
	}
	if s.RandomDelayMinutes < 0 || s.RandomDelayMinutes > 60 {
		return fmt.Errorf("schedule.random_delay_minutes must be between 0 and 60")
	}

	// 时区：解析一次存进 Location（scheduler 直接用），空值归一成默认值，
	// 这样管理后台/落盘文件里看到的也是真正生效的时区名。
	loc, err := ParseTimezone(s.Timezone)
	if err != nil {
		return fmt.Errorf("schedule.timezone: %w", err)
	}
	s.Location = loc
	if strings.TrimSpace(s.Timezone) == "" {
		s.Timezone = DefaultTimezone
	}

	for _, p := range s.schedulePairs() {
		if len(*p.times) == 0 {
			*p.times = p.def
		}
		minutes, err := parseClockList("schedule."+p.field+"_time", p.field, *p.times)
		if err != nil {
			return err
		}
		*p.minutes = minutes
	}

	// 遍历节奏：解析 + 校验（负值无意义，一律报错；"0" 表示关闭/不等）。
	durs := []struct {
		key string
		raw string
		out *time.Duration
	}{
		{"checkin_account_delay", s.CheckinAccountDelay, &s.CheckinAccountDelayDur},
		{"travel_account_delay", s.TravelAccountDelay, &s.TravelAccountDelayDur},
		{"activity_account_delay", s.ActivityAccountDelay, &s.ActivityAccountDelayDur},
		{"checkin_refresh_skew", s.CheckinRefreshSkew, &s.CheckinRefreshSkewDur},
		{"balance_refresh_interval", s.BalanceRefreshInterval, &s.BalanceRefreshIntervalDur},
		{"wakeup_grace", s.WakeupGrace, &s.WakeupGraceDur},
		{"catchup_window", s.CatchupWindow, &s.CatchupWindowDur},
	}
	for _, d := range durs {
		if strings.TrimSpace(d.raw) == "" {
			continue // 未配置：保持零值，由调用方兜底（正常路径不会走到：默认文件里有值）
		}
		v, err := time.ParseDuration(d.raw)
		if err != nil {
			return fmt.Errorf("schedule.%s: %w", d.key, err)
		}
		if v < 0 {
			return fmt.Errorf("schedule.%s: 不能为负（关闭请写 0）", d.key)
		}
		*d.out = v
	}
	// 余额定期刷新太密会把上游 billing 打成扫描器：允许关闭（0），但开着时至少 5m。
	if s.BalanceRefreshIntervalDur > 0 && s.BalanceRefreshIntervalDur < 5*time.Minute {
		return fmt.Errorf("schedule.balance_refresh_interval: 至少 5m（0 = 关闭）")
	}
	if len(s.CheckinRetryDelays) > 0 {
		out := make([]time.Duration, 0, len(s.CheckinRetryDelays))
		for i, raw := range s.CheckinRetryDelays {
			v, err := time.ParseDuration(raw)
			if err != nil {
				return fmt.Errorf("schedule.checkin_retry_delays[%d]: %w", i, err)
			}
			if v < 0 {
				return fmt.Errorf("schedule.checkin_retry_delays[%d]: 不能为负", i)
			}
			out = append(out, v)
		}
		s.CheckinRetryDelaysDur = out
	}

	// 0/负数 → 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if s.ActivityReportCount <= 0 {
		s.ActivityReportCount = 1
	}
	return nil
}

// parseClockList 解析并校验一组时刻：非法格式快速失败、去重、升序。
func parseClockList(field, name string, times []string) ([]int, error) {
	out := make([]int, 0, len(times))
	seen := make(map[int]bool, len(times))
	for _, raw := range times {
		m, err := ParseClock(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %v；应为 \"HH:MM\"（如 \"09:30\"），"+
				"要关闭该任务请设 schedule.%s_enabled=false", field, err, name)
		}
		if seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: 至少要有一个时刻（\"HH:MM\"）", field)
	}
	sort.Ints(out)
	return out, nil
}
