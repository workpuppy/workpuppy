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
	"sort"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/configdata"
)

// Schedule 排程配置段（对应 config.json 的 "schedule" 对象）。
//
// 六类独立排程：签到 / 活跃上报 / 猫猫旅行 / token keepalive / 开学季 / 夜猫子。
// cmd/server 与 cmd/activity 共用本结构，默认值由 DefaultSchedule 填充、
// 缺省归一由 Normalize 完成——两命令走同一份语义，不再各自复制。
type Schedule struct {
	AccountDelayMinSeconds int `json:"account_delay_min_seconds"`
	AccountDelayMaxSeconds int `json:"account_delay_max_seconds"`
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
	WakeupGrace          string `json:"wakeup_grace"`           // 迟到唤醒补跑前的网络宽限（默认 5s）
	CatchupWindow        string `json:"catchup_window"`         // 补跑窗口，超过则放弃老槽位（默认 24h）
	// CheckinRetryDelays 签到遇到上游瞬时拒绝（5xx / code 10001）时的退避序列（默认 5s,15s,60s）。
	CheckinRetryDelays []string `json:"checkin_retry_delays"`

	// 以下为解析结果（json:"-"），供 scheduler 直接使用。
	CheckinAccountDelayDur  time.Duration   `json:"-"`
	TravelAccountDelayDur   time.Duration   `json:"-"`
	ActivityAccountDelayDur time.Duration   `json:"-"`
	CheckinRefreshSkewDur   time.Duration   `json:"-"`
	WakeupGraceDur          time.Duration   `json:"-"`
	CatchupWindowDur        time.Duration   `json:"-"`
	CheckinRetryDelaysDur   []time.Duration `json:"-"`

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

	// ── 旧字段：整数小时（checkin_hours: [9, 21]）──────────────────────────
	// 仅为**兼容存量 config.json** 保留：Normalize 会把它们折算成 "HH:00" 并写进
	// 上面的 *_time（并打一条迁移提示）。新配置请直接用 *_time。
	CheckinHours   []int `json:"checkin_hours,omitempty"`
	TravelHours    []int `json:"travel_hours,omitempty"`
	ActivityHours  []int `json:"activity_hours,omitempty"`
	KeepaliveHours []int `json:"keepalive_hours,omitempty"`
	SchoolHours    []int `json:"school_hours,omitempty"`
	CatHours       []int `json:"cat_hours,omitempty"`

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

	// migrated 记录本次加载里哪些旧字段被折算（供启动日志提示迁移）。
	migrated []string
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

// MigratedFields 返回本次加载中被折算成 *_time 的旧字段名（用于启动日志提示）。
func (s *Schedule) MigratedFields() []string { return s.migrated }

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

// schedulePairs 六类任务的字段对应关系（新 *_time / 旧 *_hours / 解析结果 *_At / 默认值）。
//
// 默认值只用于「显式写了 null 或空数组」这种边角情况；**主路径的默认值来自
// config.default.json**（内嵌，见 DefaultSchedule/Default）。
func (s *Schedule) schedulePairs() []struct {
	field   string
	times   *[]string
	hours   *[]int
	minutes *[]int
	def     []string
} {
	return []struct {
		field   string
		times   *[]string
		hours   *[]int
		minutes *[]int
		def     []string
	}{
		{"checkin", &s.CheckinTime, &s.CheckinHours, &s.CheckinAt, []string{"09:00", "21:00"}},
		{"travel", &s.TravelTime, &s.TravelHours, &s.TravelAt, []string{"09:00", "21:00"}},
		{"activity", &s.ActivityTime, &s.ActivityHours, &s.ActivityAt, []string{"10:00"}},
		{"keepalive", &s.KeepaliveTime, &s.KeepaliveHours, &s.KeepaliveAt, []string{"22:00"}},
		{"school", &s.SchoolTime, &s.SchoolHours, &s.SchoolAt, []string{"12:00"}},
		{"cat", &s.CatTime, &s.CatHours, &s.CatAt, []string{"01:00"}},
	}
}

// UnmarshalJSON 在**解析配置文件的当下**处理旧的小时写法。
//
// 为什么必须在反序列化时做、不能在 Normalize 里做：默认配置本身（内嵌 config.default.json）
// 已经把 *_time 填好了，等到 Normalize 时根本无法区分「用户写了 *_time」与「这是默认值」。
// 只有在这里（只看用户文件里真正出现的键）才能准确判断「用户只写了旧键」。
//
// 语义：
//   - 同时出现 *_time 与 *_hours → 以 *_time 为准，旧键忽略（不记迁移）；
//   - 只出现 *_hours → 折算成 "HH:00" 填进 *_time，并记入 MigratedFields（启动日志 WARN）；
//   - 旧键越界 → 直接报错（带新写法的提示），不让它静默生效。
func (s *Schedule) UnmarshalJSON(b []byte) error {
	// 注意：反序列化到**已存在的值**上（而不是先清零）——上一层的默认配置（内嵌
	// config.default.json）已经填好了各字段，用户文件里没写的键必须保持默认值，
	// 否则「键缺席 = 回落默认」的语义会被本方法破坏。
	type alias Schedule // 避免递归调用本方法
	if err := json.Unmarshal(b, (*alias)(s)); err != nil {
		return err
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal(b, &present); err != nil {
		return err
	}
	s.migrated = nil

	for _, p := range s.schedulePairs() {
		if _, hasTime := present[p.field+"_time"]; hasTime {
			continue // 新写法优先
		}
		if _, hasHours := present[p.field+"_hours"]; !hasHours {
			continue
		}
		converted := make([]string, 0, len(*p.hours))
		for _, h := range *p.hours {
			if h < 0 || h > 23 {
				return fmt.Errorf("schedule.%s_hours: %d 不是合法小时（0-23）；"+
					"新写法是 schedule.%s_time: [\"09:30\"]，要关任务请设 schedule.%s_enabled=false",
					p.field, h, p.field, p.field)
			}
			converted = append(converted, FormatClock(h*60))
		}
		if len(converted) > 0 {
			*p.times = converted
			s.migrated = append(s.migrated, "schedule."+p.field+"_hours")
		}
	}
	return nil
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
