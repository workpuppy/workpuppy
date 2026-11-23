package server

import "wp/internal/accesskey"

// testAccessKeys 构造一把「全部账号 + 全部模型」的测试 key。
//
// 绝大多数 handler 测试只关心业务链路（冷却/轮转/错误分类），不应被白名单干扰；
// 鉴权与分流专项测试（access_key_test.go）会自己构造更精确的 key。
//
// 密钥文本由调用方指定，便于测试同时断言 "Bearer <secret>" 的行为。长度需满足
// accesskey 的 8–128 校验；短于 8 会在测试启动时直接 panic（fail fast）。
func testAccessKeys(secret string) *accesskey.Store {
	st := accesskey.New("")
	if _, err := st.Create(accesskey.Input{
		Name:        "test",
		Secret:      secret,
		AllAccounts: true,
		AllModels:   true,
	}); err != nil {
		panic("testAccessKeys(" + secret + "): " + err.Error())
	}
	return st
}
