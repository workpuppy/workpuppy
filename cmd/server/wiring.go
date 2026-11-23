package main

import (
	"wp/internal/pool"
	"wp/internal/server"
	"wp/internal/session"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按前缀剥出
// realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号会被粘性分配给
// CN 前缀请求（跨 realm 泄漏）。裸名/显式 cn → cn 集合；global: → global 集合。
//
// realm 为空串时 pool.AvailableUIDsForModelRealm 退化为现状（AvailableUIDsForModel），
// 老调用（无前缀模型名）语义零改动。
func realmAwareAvailableForModel(p *pool.Pool) func(model string) []string {
	return func(model string) []string {
		realm, bare := server.ResolveModel(model)
		return p.AvailableUIDsForModelRealm(bare, realm)
	}
}

// realmAwareAvailableForModelScoped 是上面的「带账号范围」版本（接入 key 白名单）。
//
// scope 同时包含 realm 与接入 key 两个维度：先由 pool 按 (realm, 模型) 取可用集合，
// 再按 key 的账号白名单过滤。两层的顺序不能反——反过来只会多遍历一遍，结果相同，
// 但先做 realm/模型过滤能少查白名单（46 个账号时差异可忽略，语义上更清楚）。
func realmAwareAvailableForModelScoped(p *pool.Pool) func(model string, scope session.Scope) []string {
	return func(model string, scope session.Scope) []string {
		realm, bare := server.ResolveModel(model)
		return p.AvailableUIDsForModelRealmScoped(bare, realm, scope)
	}
}

// realmAwareWeightedPick 构造会话粘性**首绑**的加权选号闭包（issue #265）。
//
// 背景：粘性未命中时旧实现用 FNV-1a 取模在可用 UID 里选，与积分余额、快过期积分、
// 闲置时长都无关；而默认开启的粘性让带会话标识的请求首轮就被绑定并滚动续期，永远
// 走不到 Pool.pick 的三因子加权。于是「快过期积分优先消耗」（pool.expiring_soon）
// 在默认配置下基本失效——一个没有快过期积分的号可以独占整段会话流量。
//
// 候选集已由 realmAwareAvailableForModel 按 realm + 模型可用性筛过，这里只需剥掉
// realm 前缀拿裸模型名：成本台账与 6004 模型级冷却都以裸名记账。
func realmAwareWeightedPick(p *pool.Pool) func(model string, candidates []string) string {
	return func(model string, candidates []string) string {
		_, bare := server.ResolveModel(model)
		return p.WeightedPickAmong(candidates, bare)
	}
}

// realmAwareWeightedPickScoped 是首绑加权选号的带范围版本。
//
// scope 在这里不必再次过滤：candidates 由 AvailableForModelScoped 生成，已限定在
// key 的账号白名单内；WeightedPickAmong 也只会从候选集里抽签（不会回落到全池）。
// 保留这个签名是为了给 session 一个显式的 scoped 扩展点，后续若加权策略需要按
// key 区分（如不同 key 的成本偏好）可以在这里演进，不必再改 session。
func realmAwareWeightedPickScoped(p *pool.Pool) func(model string, candidates []string, scope session.Scope) string {
	return func(model string, candidates []string, _ session.Scope) string {
		_, bare := server.ResolveModel(model)
		return p.WeightedPickAmong(candidates, bare)
	}
}
