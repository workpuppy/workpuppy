// tuning.go 上游客户端的超时/阈值/模型目录参数的**注入点**。
//
// 这些值曾经硬编码在本包的 const 里；现在默认值来自 config.default.json，
// 由 cmd/server 在启动时用用户配置调用 Configure 覆盖。包内初值保留给测试裸用。
package upstream

import "time"

// Options 上游客户端阈值。
type Options struct {
	RetryAfterMax          time.Duration // Retry-After 的合理上限（超出视为脏数据）
	RefreshIOTimeout       time.Duration // token 刷新的单独超时
	GzipThresholdBytes     int           // 请求体超过该字节数才压缩（0 = 不压缩）
	DeviceTokenFileTTL     time.Duration // device_token 文件缓存有效期
	DeviceTokenFileMaxLen  int           // device_token 文件最大长度
	DefaultContextWindow   int64         // 模型未声明上下文窗口时的兜底值
	CatalogTimeout         time.Duration // 模型目录抓取超时
	CatalogRefreshCooldown time.Duration // 模型目录抓取冷却
	CatalogNegativeTTL     time.Duration // 抓取失败的负缓存 TTL
}

// Configure 用生效配置覆盖包级阈值（cmd/server 启动时调用一次）。
func Configure(o Options) {
	if o.RetryAfterMax > 0 {
		retryAfterSanity = o.RetryAfterMax
	}
	if o.RefreshIOTimeout > 0 {
		refreshIOTimeout = o.RefreshIOTimeout
	}
	if o.GzipThresholdBytes >= 0 {
		gzipBodyThreshold = o.GzipThresholdBytes
	}
	if o.DeviceTokenFileTTL > 0 {
		deviceTokenFileTTL = o.DeviceTokenFileTTL
	}
	if o.DeviceTokenFileMaxLen > 0 {
		deviceTokenFileMaxLen = o.DeviceTokenFileMaxLen
	}
	if o.DefaultContextWindow > 0 {
		DefaultContextWindow = o.DefaultContextWindow
	}
	if o.CatalogTimeout > 0 {
		modelsDevTimeout = o.CatalogTimeout
	}
	if o.CatalogRefreshCooldown > 0 {
		modelsDevFetchCooldown = o.CatalogRefreshCooldown
	}
	if o.CatalogNegativeTTL > 0 {
		modelsDevNegativeTTL = o.CatalogNegativeTTL
	}
}
