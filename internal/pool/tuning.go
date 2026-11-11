// tuning.go 池的运行期阈值/节奏的**注入点**。
//
// 这些值曾经硬编码在本包的 const 里；现在默认值来自 config.example.json，
// 由 cmd/server 在启动时用用户配置调用 Configure 覆盖。
//
// 包内仍保留同名变量的初值：裸用 pool.New（单元测试/极端降级）时按真实默认值工作。
// 生产路径 100% 走配置文件。
package pool

import "time"

// creditSink 积分流水回调（nil = 不记录）。由 Configure 注入，供日账本统计。
var creditSink func(uid, realm string, delta, spent, balance int64)

// emitCredit 只在接了回调时上报（热路径上零开销）。
func emitCredit(uid, realm string, delta, spent, balance int64) {
	if creditSink != nil {
		creditSink(uid, realm, delta, spent, balance)
	}
}

// Options 池运行期阈值（0 表示"不设置"，由调用方保证传入有效值）。
type Options struct {
	MinPickGap           time.Duration // 选号防惊群间隔（跳过刚被选中的号）
	SessionDeadThreshold int           // 连续 session dead 多少次禁用账号
	RotateBackoffBase    time.Duration // 轮换失败的基础退避
	ExpiringWeight       float64       // 快过期积分在选号权重里的倍数
	SmoothAlpha          float64       // credits 平滑系数
	WatchInterval        time.Duration // auths 目录热加载轮询间隔
	FlushInterval        time.Duration // state.json 落盘间隔（关键状态的上界）
	SoftFlushInterval    time.Duration // 软计数（余额估算/计数/成本台账）的合并落盘间隔

	// CreditSink 积分流水回调（cmd/server 接到 creditledger 日账本）：
	// delta=余额变化（>0 赚 / <0 调整）、spent=本次请求消耗、balance=当前余额。
	CreditSink func(uid, realm string, delta, spent, balance int64)
}

// Configure 用生效配置覆盖包级阈值（cmd/server 启动时调用一次）。
func Configure(o Options) {
	if o.MinPickGap > 0 {
		minPickGap = o.MinPickGap
	}
	if o.SessionDeadThreshold > 0 {
		sessionDeadThreshold = o.SessionDeadThreshold
	}
	if o.ExpiringWeight > 0 {
		expiringWeight = o.ExpiringWeight
	}
	if o.WatchInterval > 0 {
		watchInterval = o.WatchInterval
	}
	if o.FlushInterval > 0 {
		flushInterval = o.FlushInterval
	}
	if o.SoftFlushInterval > 0 {
		softFlushInterval = o.SoftFlushInterval
	}
	if o.CreditSink != nil {
		creditSink = o.CreditSink
	}
}
