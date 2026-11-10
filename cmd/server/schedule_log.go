// schedule_log.go 启动日志里把排程时刻渲染成人读形式，并提示旧字段迁移。
package main

import (
	"strings"
)

// scheduleTimes 把 ["09:00","21:00"] 渲染成 "09:00, 21:00"（空则 "（默认）"）。
func scheduleTimes(times []string) string {
	if len(times) == 0 {
		return "（默认）"
	}
	return strings.Join(times, ", ")
}
