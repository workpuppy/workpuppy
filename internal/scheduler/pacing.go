// pacing.go 遍历节奏（账号间隔、重试退避、补跑窗口）的**单一来源与注入点**。
//
// 这些值曾经硬编码在本包里（3s / 800ms / 5s,15s,60s …）。现在默认值统一来自
// config.example.json（内嵌），由 cmd/server 在启动时用用户配置覆盖：
//
//	scheduler.Configure(scheduler.Options{
//	    CheckinAccountDelay: cfg.Schedule.CheckinAccountDelayDur, …
//	})
//
// 为什么用包级变量而不是把值塞进 Scheduler 结构：这些节奏是**进程级**部署参数
// （同一进程内所有 Scheduler 实例共享），而测试需要能把它们置 0 以免白等——
// 包级变量让"测试置 0"和"生产注配置"共用一条路径，不必给每个构造点加参数。
package scheduler

import (
	"sync"
	"time"

	"wp/internal/config"
)

// Options 遍历节奏（全部时长；0 = 关闭/不等，负值视为未设置）。
type Options struct {
	CheckinAccountDelay  time.Duration   // 签到相邻账号最小间隔
	TravelAccountDelay   time.Duration   // 旅行相邻账号间隔
	ActivityAccountDelay time.Duration   // 活跃上报相邻账号间隔
	CheckinRefreshSkew   time.Duration   // 签到前多久判定 token 将过期
	CheckinRetryDelays   []time.Duration // 签到瞬时失败的退避序列（空 = 不重试）
	WakeupGrace          time.Duration   // 迟到唤醒补跑前的网络宽限
	CatchupWindow        time.Duration   // 补跑窗口（超过则放弃老槽位）
}

// Configure 用生效配置覆盖包级节奏（由 cmd/server 在启动时调用一次）。
//
// 语义：**逐字段覆盖**，包括 0（0 = 关闭该节奏，是合法配置）；负值忽略（保守：
// 写错一个负时长不该让账号间隔变成"负等待"）。切片为空时覆盖为"不重试"。
func Configure(o Options) {
	if o.CheckinAccountDelay >= 0 {
		checkinAccountDelay = o.CheckinAccountDelay
	}
	if o.TravelAccountDelay >= 0 {
		travelAccountDelay = o.TravelAccountDelay
	}
	if o.ActivityAccountDelay >= 0 {
		activityAccountDelay = o.ActivityAccountDelay
	}
	if o.CheckinRefreshSkew > 0 {
		checkinRefreshSkew = o.CheckinRefreshSkew
	}
	if o.WakeupGrace >= 0 {
		wakeupGraceDelay = o.WakeupGrace
	}
	if o.CatchupWindow > 0 {
		catchupWindow = o.CatchupWindow
	}
	if o.CheckinRetryDelays != nil {
		checkinRetryDelays = o.CheckinRetryDelays
	}
}

// schedDefaults 返回 config.example.json 里的排程节奏默认值（包级变量的初始值）。
//
// 放在这里而不是写成字面量：默认值只允许有一处来源。裸用 New(Config{})（测试/
// 极端降级）时这些变量仍按真实默认值工作。
func schedDefaults() config.Schedule {
	schedDefaultsOnce.Do(func() {
		s := config.DefaultSchedule()
		// 必须 Normalize：解析出的时长在 *_Dur 字段里（未解析前全是 0）。
		if err := s.Normalize(); err != nil {
			panic("config.example.json 的排程节奏无法解析: " + err.Error())
		}
		schedDefaultsVal = s
	})
	return schedDefaultsVal
}

var (
	schedDefaultsOnce sync.Once
	schedDefaultsVal  config.Schedule
)
