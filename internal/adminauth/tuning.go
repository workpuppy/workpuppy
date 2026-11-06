// tuning.go 管理密码生成参数（长度）的注入点。
//
// 默认值来自 config.default.json（admin.password_length / admin.min_password_length），
// 由 cmd/server 启动时调用 Configure 覆盖。包内初值保留给测试裸用。
package adminauth

// Options 管理密码长度约束。
type Options struct {
	PasswordLength    int // 自动生成密码的字符数
	MinOverrideLength int // 用户固定密码的最小长度
}

// Configure 用生效配置覆盖包级长度约束（cmd/server 启动时调用一次）。
func Configure(o Options) {
	if o.PasswordLength > 0 {
		passwordLen = o.PasswordLength
	}
	if o.MinOverrideLength > 0 {
		minOverrideLen = o.MinOverrideLength
	}
}
