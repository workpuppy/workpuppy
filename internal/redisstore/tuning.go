// tuning.go 会话镜像（Upstash Redis）写入参数的**注入点**。
//
// 默认值来自 config.default.json，由 cmd/server 启动时用用户配置调用 Configure 覆盖。
package redisstore

import "time"

// Options Redis 镜像参数。
type Options struct {
	KeyTTL           time.Duration // 会话镜像 key 的存活时间
	WriteConcurrency int           // 并发写入上限
}

// Configure 用生效配置覆盖包级参数（cmd/server 启动时调用一次）。
func Configure(o Options) {
	if o.KeyTTL > 0 {
		keyTTL = o.KeyTTL
	}
	if o.WriteConcurrency > 0 {
		writeConcurrencyLimit = o.WriteConcurrency
	}
}
