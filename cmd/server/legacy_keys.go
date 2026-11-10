// legacy_keys.go 处理"已弃用配置键"的兼容。
//
// 背景：默认值来自内嵌 config.default.json，Default() 会把所有键都填上默认值；
// 因此**只看字段是否为空，无法判断用户到底写了哪个键**（旧键写得再明确，
// 新字段也早就被默认值占住了）。要精确判断"用户只写了旧键"，只能回到原始 JSON 看键集合。
//
// 规则：新键优先；只写旧键时按旧键生效，并记入 LegacyKeys() 供启动日志 WARN。
package main

import (
	"encoding/json"
	"strings"
)

// legacyAliases 已弃用键 → 新键的映射（path 形如 "pool.expiring_soon"）。
var legacyAliases = []struct {
	oldPath string
	newPath string
}{
	{"pool.expiring_soon", "pool.expiring_soon_window"},
	{"models.default_context", "models.default_context_window"},
}

// hasJSONKey 判断 raw 里是否存在 path（支持一层嵌套："a.b"）。
func hasJSONKey(raw []byte, path string) bool {
	parts := strings.SplitN(path, ".", 2)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return false
	}
	v, ok := top[parts[0]]
	if !ok {
		return false
	}
	if len(parts) == 1 {
		return true
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(v, &nested); err != nil {
		return false
	}
	_, ok = nested[parts[1]]
	return ok
}

// getJSONValue 取 path 对应的原始值（支持一层嵌套）。
func getJSONValue(raw []byte, path string) (json.RawMessage, bool) {
	parts := strings.SplitN(path, ".", 2)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, false
	}
	v, ok := top[parts[0]]
	if !ok {
		return nil, false
	}
	if len(parts) == 1 {
		return v, true
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(v, &nested); err != nil {
		return nil, false
	}
	v, ok = nested[parts[1]]
	return v, ok
}

// applyLegacyAliases 按用户原始 JSON 的**键集合**把旧键值搬到新字段。
//
// 只在"旧键存在 且 新键不存在"时生效——两者并存说明用户已经迁移，以新键为准。
func (c *Config) applyLegacyAliases(raw []byte) {
	for _, a := range legacyAliases {
		if !hasJSONKey(raw, a.oldPath) || hasJSONKey(raw, a.newPath) {
			continue
		}
		v, ok := getJSONValue(raw, a.oldPath)
		if !ok {
			continue
		}
		switch a.oldPath {
		case "pool.expiring_soon":
			var str string
			if err := json.Unmarshal(v, &str); err == nil && strings.TrimSpace(str) != "" {
				c.Pool.ExpiringSoonWindow = str
				c.legacyKeys = append(c.legacyKeys, a.oldPath+" → "+a.newPath)
			}
		case "models.default_context":
			var n int64
			if err := json.Unmarshal(v, &n); err == nil && n != 0 {
				c.Models.DefaultContextWindow = n
				c.legacyKeys = append(c.legacyKeys, a.oldPath+" → "+a.newPath)
			}
		}
	}
}
