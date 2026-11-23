package pool

// 选号范围扩展层（开闭原则）：在不改动既有选号语义的前提下，给 Pick/可用集合/
// 会话粘性加一层「哪些账号可参与」的过滤器。
//
// 为什么单独抽 Scope 而不是给每个选号函数加一个 []string：
//   - 现有选号簇（pick / pickEarliestExpiryLocked / AvailableUIDs* / PickByUID*）
//     已经有一整套 realm、模型豁免、成本分层、保底积分语义；把白名单塞进每个函数
//     的签名会让既有调用方全部改一遍，也容易在不同路径上漏判。
//   - Scope 是一个**扩展点**：nil = 现状（不限），非 nil = 新层。老调用全部显式传
//     nil（或走不带 Scoped 后缀的包装），行为逐位不变。
//
// 语义要点：nil 表示「不限」，**空集合（NewUIDSet(nil)）表示「一个都不允许」**。
// 两者必须区分——把空的接入 key 白名单误当成 nil，就是「漏配 = 全池放出去」。

// Scope 选号范围：Allows 为真的账号才参与选号/可用集合/兜底。
type Scope interface {
	Allows(uid string) bool
}

// UIDSet 以 UID 集合实现 Scope。nil map 与空 map 都「不包含任何 uid」，
// 因此都会拒绝全部账号；要表示「不限」请传 nil Scope 本身。
type UIDSet map[string]struct{}

// NewUIDSet 把 UID 列表转成集合（自动去重、忽略空串）。永远返回非 nil map，
// 便于调用方用 `scope != nil` 表达「有范围」，同时空列表自然表示「全部拒绝」。
func NewUIDSet(uids []string) UIDSet {
	set := make(UIDSet, len(uids))
	for _, uid := range uids {
		if uid == "" {
			continue
		}
		set[uid] = struct{}{}
	}
	return set
}

// Allows 报告 uid 是否在集合内。
func (s UIDSet) Allows(uid string) bool {
	if s == nil {
		return false
	}
	_, ok := s[uid]
	return ok
}

// scopeAllows 是各选号路径共用的判定：nil = 不限；非 nil 才查 Allows。
func scopeAllows(scope Scope, uid string) bool {
	return scope == nil || scope.Allows(uid)
}
