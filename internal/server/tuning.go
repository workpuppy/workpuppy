// tuning.go 网关侧的冷却/风控/限流阈值的**注入点**。
//
// 这些值曾经硬编码在本包的 const 里；现在默认值来自 config.default.json，
// 由 cmd/server 在启动时用用户配置调用 Configure 覆盖。包内初值保留给测试裸用。
package server

import "time"

// Options 冷却与风控阈值。
type Options struct {
	NotFoundCooldown            time.Duration // 404/未知模型后的冷却
	WafCooldownBase             time.Duration // WAF 403 基础冷却
	UpstreamRestrictionCooldown time.Duration // 上游限制类错误的冷却
	WafIPWindow                 time.Duration // 单 IP WAF 命中统计窗口
	WafIPThreshold              int           // 窗口内命中多少次进入风险暂停
	DumpReqMinBytes             int           // 请求体落盘的最小字节数（0 = 关闭）
	MetricsCap                  int           // 指标环形缓冲容量
	AdminBodyLimit              int64         // 管理面请求体上限
	RotateBackoffBase           time.Duration // 轮换失败的基础退避
}

// Configure 用生效配置覆盖包级阈值（cmd/server 启动时调用一次）。
func Configure(o Options) {
	if o.NotFoundCooldown > 0 {
		notFoundCooldown = o.NotFoundCooldown
	}
	if o.WafCooldownBase > 0 {
		wafCooldownBase = o.WafCooldownBase
	}
	if o.UpstreamRestrictionCooldown > 0 {
		upstreamRestrictionCooldown = o.UpstreamRestrictionCooldown
	}
	if o.WafIPWindow > 0 {
		wafIPWindow = o.WafIPWindow
	}
	if o.WafIPThreshold > 0 {
		wafIPThreshold = o.WafIPThreshold
	}
	if o.DumpReqMinBytes >= 0 {
		dumpReqMinBytes = o.DumpReqMinBytes
	}
	if o.MetricsCap > 0 {
		metricsCap = o.MetricsCap
	}
	if o.AdminBodyLimit > 0 {
		adminBodyLimit = o.AdminBodyLimit
	}
	if o.RotateBackoffBase > 0 {
		rotateBackoffBase = o.RotateBackoffBase
	}
}
