// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"time"

	"wp/internal/auth"
)

// PickExcludingForRealm 按 realm 过滤的轮换选号：候选仅限 Realm()==realm 的账号。
// realm=="" 退化为 PickExcluding（现状语义，老调用零改动）。
// 可选做请求级轮换（tried）与模型感知（reqModel，6004 模型豁免照常生效）；
// reqModel 非空时健康口径换成 healthyForModel。realm 不匹配的全冷却兜底同样排除。
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm)
}

// PickExcludingForRealmScoped 在 PickExcludingForRealm 之上叠加账号范围（scope）。
//
// 接入 key 的账号白名单走这里：realm / 模型 / 成本分层 / 保底积分 / 全冷却兜底
// 全部沿用既有语义，只是在每个候选上多一次 scope.Allows 判定。scope=nil 与
// PickExcludingForRealm 完全等价（开闭原则：新层不改变旧层）。
func (p *Pool) PickExcludingForRealmScoped(tried map[string]bool, reqModel, realm string, scope Scope) *auth.Auth {
	return p.pickScoped(tried, reqModel, realm, scope)
}

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// DeptestOnly: 仅 realm_test.go 引用；生产经 wiring.go 走
// AvailableUIDsForModelRealm。保留作 ForModelRealm 的模型维度退化
// （model=""）语义锚点测试。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	return p.availableUIDsLocked(realm, nil, func(e *entry, now time.Time) bool { return e.healthy(now) })
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	return p.AvailableUIDsForModelRealmScoped(model, realm, nil)
}

// AvailableUIDsForModelRealmScoped 在 AvailableUIDsForModelRealm 之上叠加 scope：
// 会话粘性的「按模型可用集合」必须按 key 的账号范围过滤，否则粘性会把会话分配到
// 该 key 无权使用的账号（命中校验也会把范围外的绑定当成"可用"而继续沿用）。
// scope=nil 时与前一个函数逐位一致。
func (p *Pool) AvailableUIDsForModelRealmScoped(model, realm string, scope Scope) []string {
	return p.availableUIDsLocked(realm, scope,
		func(e *entry, now time.Time) bool { return e.healthyForModel(now, model) })
}
