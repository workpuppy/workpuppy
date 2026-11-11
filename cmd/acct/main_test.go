// main_test.go acct 工具的纯函数契约：listen 归一化 + 状态文案。
package main

import (
	"testing"
)

// TestNormalizeListen listen 的各种写法都要收敛成本机可访问的 http 基址。
// 通配地址（空 host / 0.0.0.0 / ::）必须收敛到回环——本工具总是与网关同机运行，
// 用 0.0.0.0 发请求在部分平台会失败。
func TestNormalizeListen(t *testing.T) {
	cases := []struct{ in, want string }{
		{":7863", "http://127.0.0.1:7863"},
		{"0.0.0.0:7863", "http://127.0.0.1:7863"},
		{"::", "http://127.0.0.1:7863"},
		{"[::]:7863", "http://127.0.0.1:7863"},
		{"127.0.0.1:7863", "http://127.0.0.1:7863"},
		{"localhost:9999", "http://localhost:9999"},
		{"", "http://127.0.0.1:7863"},           // 空 → 默认端口
		{"127.0.0.1:", "http://127.0.0.1:7863"}, // 有 host 无端口
		{"7863", "http://127.0.0.1:7863"},       // 只有端口（无冒号 host 段）
	}
	for _, c := range cases {
		if got := normalizeListen(c.in); got != c.want {
			t.Errorf("normalizeListen(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestStateLabel 状态文案：禁用带来源（manual/system）与原因。
func TestStateLabel(t *testing.T) {
	cases := []struct {
		name string
		in   accountStatus
		want string
	}{
		{"正常", accountStatus{}, "正常"},
		{"手动禁用", accountStatus{Disabled: true, DisabledSource: "manual"}, "禁用(手动)"},
		{"系统禁用", accountStatus{Disabled: true, DisabledSource: "system"}, "禁用(系统)"},
		{"来源缺失", accountStatus{Disabled: true}, "禁用"},
		{"带原因", accountStatus{Disabled: true, DisabledSource: "manual", DisabledReason: "观察"}, "禁用(手动)(观察)"},
		{"冷却", accountStatus{Cooling: true}, "冷却中"},
		{"二者", accountStatus{Disabled: true, DisabledSource: "system", Cooling: true},
			"禁用(系统) + 冷却中"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stateLabel(c.in); got != c.want {
				t.Errorf("stateLabel() = %q, want %q", got, c.want)
			}
		})
	}
}
