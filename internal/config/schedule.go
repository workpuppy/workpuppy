// Package config 存放跨命令共享的配置段与默认值逻辑。
//
// 起因 issue #49：cmd/activity 一次性触发器曾自复制一份精简 schedule 结构体，
// 只 json.Unmarshal 无默认值，键缺席 → Go 零值 0 → scheduler 归一为 1，
// 与 cmd/server 主程序缺省 5 条漂移。把 Schedule 段 + 默认值/归一化抽到本包，
// 两个命令共用同一份定义，消除漂移源头。
package config

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
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

// DefaultSchedule 返回排程段的默认值。
//
// 开关「缺省 true」靠这里实现：调用方先取 DefaultSchedule 再用 json.Unmarshal 覆盖，
// 键缺席（或为 null）时字段原样保留 true，只有显式 false 才关。
// ActivityReportCount 默认 5：领猫前置需 5 次对话，5 连发刷满 chat_5。
func DefaultSchedule() Schedule {
	// 时刻（*_time）**刻意留空**、由 Normalize 统一填默认值：
	// 这样「旧 config 只写了 *_hours」时，Normalize 仍能识别出「*_time 未配置」并把旧键
	// 折算过来；若这里预置了默认时刻，迁移分支永远走不到（默认值把空判断填满了）。
	return Schedule{
		CheckinEnabled:      true,
		TravelEnabled:       true,
		ActivityEnabled:     true,
		KeepaliveEnabled:    true,
		SchoolEnabled:       true,
		CatEnabled:          true,
		ActivityReportCount: 5, // 领猫前置需 5 次对话，5 连发刷满 chat_5
	}
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

// Normalize 归一化排程段：解析时刻、兼容旧的小时字段、补默认值、校验并排序。
//
// 「空数组与 null 会覆盖 DefaultSchedule 的值（只有键缺席才保留）」，所以这里要为
// 空值补默认；「禁用」一律走 *_enabled=false，与空值语义互不混淆。
func (s *Schedule) Normalize() error {
	if s.AccountDelayMinSeconds < 0 || s.AccountDelayMaxSeconds > 600 ||
		s.AccountDelayMaxSeconds < s.AccountDelayMinSeconds {
		return fmt.Errorf("schedule account delays require 0 <= min <= max <= 600 seconds")
	}
	if s.RandomDelayMinutes < 0 || s.RandomDelayMinutes > 60 {
		return fmt.Errorf("schedule.random_delay_minutes must be between 0 and 60")
	}

	s.migrated = nil
	type pair struct {
		field   string
		times   *[]string
		hours   []int
		minutes *[]int
		def     []string
	}
	pairs := []pair{
		{"checkin", &s.CheckinTime, s.CheckinHours, &s.CheckinAt, []string{"09:00", "21:00"}},
		{"travel", &s.TravelTime, s.TravelHours, &s.TravelAt, []string{"09:00", "21:00"}},
		{"activity", &s.ActivityTime, s.ActivityHours, &s.ActivityAt, []string{"10:00"}},
		{"keepalive", &s.KeepaliveTime, s.KeepaliveHours, &s.KeepaliveAt, []string{"22:00"}},
		{"school", &s.SchoolTime, s.SchoolHours, &s.SchoolAt, []string{"12:00"}},
		{"cat", &s.CatTime, s.CatHours, &s.CatAt, []string{"01:00"}},
	}
	for _, p := range pairs {
		// 旧字段 → 新字段（"9" → "09:00"）；只在 *_time 缺席时生效，两者并存以新值为准。
		if len(*p.times) == 0 && len(p.hours) > 0 {
			converted := make([]string, 0, len(p.hours))
			for _, h := range p.hours {
				if h < 0 || h > 23 {
					return fmt.Errorf("schedule.%s_hours: %d 不是合法小时（0-23）；"+
						"新写法是 schedule.%s_time: [\"09:30\"]，要关任务请设 schedule.%s_enabled=false",
						p.field, h, p.field, p.field)
				}
				converted = append(converted, FormatClock(h*60))
			}
			*p.times = converted
			s.migrated = append(s.migrated, "schedule."+p.field+"_hours")
		}
		if len(*p.times) == 0 {
			*p.times = p.def
		}
		minutes, err := parseClockList("schedule."+p.field+"_time", p.field, *p.times)
		if err != nil {
			return err
		}
		*p.minutes = minutes
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
