// tuning.go 管理密码强度参数的注入点。
//
// 默认值来自 config.default.json（admin.min_password_length），
// 由 cmd/server 启动时调用 Configure 覆盖。包内初值留给测试裸用。
package adminauth

// Options 管理密码强度约束。
type Options struct {
	MinLength int // 明文密码最小长度（admin.min_password_length）
}

// Configure 用生效配置覆盖包级约束（cmd/server 启动时调用一次）。
func Configure(o Options) {
	if o.MinLength > 0 {
		minPlainLen = o.MinLength
	}
}
