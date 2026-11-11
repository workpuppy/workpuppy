// unknown_keys.go 启动时检查配置里的"未知键"，把被忽略的键显式告诉用户。
//
// 为什么需要：本项目**不保留旧键兼容**（尚未正式上线，键名按语义直接演进）。
// 代价是旧键（如 schedule.checkin_hours、pool.expiring_soon）会被 JSON 静默忽略，
// 用户以为配了其实没生效。这里用内嵌的 config.default.json 当"键的清单"，
// 把用户配置里多出来的键列出来打 WARN —— 既有提示，又不用为每个旧键写兼容垫片。
package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"wp/internal/configdata"
)

// freeFormSections 值结构不固定的段：整段跳过比较（其下的键由业务自己校验）。
//   - proxies: 线路名 → 地址，名字随用户定义
var freeFormSections = map[string]bool{"proxies": true}

// unknownKeys 返回 raw 中"默认配置里没有"的键路径（形如 "schedule.checkin_hours"）。
//
// 只比较两层（段 + 一层子键）：再往下的自由结构（数组、映射）不参与，避免误报。
func unknownKeys(raw []byte) []string {
	var user, known map[string]json.RawMessage
	if err := json.Unmarshal(raw, &user); err != nil {
		return nil // 解析失败由 Load 报错，这里不重复
	}
	if err := json.Unmarshal(configdata.Bytes(), &known); err != nil {
		return nil
	}

	var out []string
	for k, uv := range user {
		if freeFormSections[k] {
			continue
		}
		kv, ok := known[k]
		if !ok {
			out = append(out, k)
			continue
		}
		// 值为对象才继续往下比一层
		var um, km map[string]json.RawMessage
		if json.Unmarshal(uv, &um) != nil || json.Unmarshal(kv, &km) != nil {
			continue
		}
		for sub := range um {
			if _, ok := km[sub]; !ok {
				out = append(out, k+"."+sub)
			}
		}
	}
	sort.Strings(out)
	return out
}

// unknownKeysWarning 把未知键渲染成一句可读的 WARN（空串表示没有未知键）。
func unknownKeysWarning(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	const max = 8
	shown := keys
	suffix := ""
	if len(keys) > max {
		shown = keys[:max]
		suffix = fmt.Sprintf(" 等 %d 个", len(keys))
	}
	return fmt.Sprintf("配置里有未知键（会被忽略，多半是拼错或旧版键名）：%s%s；"+
		"全部可用键见 config.default.json", strings.Join(shown, ", "), suffix)
}
