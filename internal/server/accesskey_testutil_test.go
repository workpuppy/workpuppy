package server

import (
	"crypto/sha256"
	"encoding/hex"

	"wp/internal/accesskey"
)

// testSecret 派生一个符合生产规则（sk- + 32 位字母/数字）的确定性测试密钥。
//
// 为什么不再直接用 "test-secret" 这类字面量：接入 key 现在强制 sk- 格式，
// 测试里既要用它创建 key，又要在 Authorization 头里原样使用；集中派生一处，
// 避免两边各写一份而漂移。hex 输出天然是 [0-9a-f]，满足字母/数字要求。
func testSecret(label string) string {
	sum := sha256.Sum256([]byte(label))
	return "sk-" + hex.EncodeToString(sum[:16])
}

// testHandlerKey 是多数 handler 测试使用的全权 key（同一份派生值）。
var testHandlerKey = testSecret("test-secret")

// testAccessKeys 构造一把「全部账号 + 全部模型」的测试 key。
//
// 绝大多数 handler 测试只关心业务链路（冷却/轮转/错误分类），不应被白名单干扰；
// 鉴权与分流专项测试（access_key_test.go）会自己构造更精确的 key。
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
