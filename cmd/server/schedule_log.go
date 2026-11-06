// schedule_log.go 启动日志里把排程时刻渲染成人读形式，并提示旧字段迁移。
package main

import (
	"log"
	"strings"

	"workbuddy2api/internal/config"
)

// scheduleTimes 把 ["09:00","21:00"] 渲染成 "09:00, 21:00"（空则 "（默认）"）。
func scheduleTimes(times []string) string {
	if len(times) == 0 {
		return "（默认）"
	}
	return strings.Join(times, ", ")
}

// logScheduleMigration 若本次加载把旧的 *_hours 折算成了 *_time，提示用户改配置。
//
// 为什么不直接报错：存量部署的 config.json 里只有旧键，报错会让升级即失败；
// 折算成 "HH:00" 语义完全等价（旧字段本来就是整点），再用一条 WARN 引导迁移。
func logScheduleMigration(sch *config.Schedule) {
	fields := sch.MigratedFields()
	if len(fields) == 0 {
		return
	}
	log.Printf("WARN: 检测到旧的小时写法（%s），已按整点折算为 *_time；"+
		"请改成新写法（如 \"checkin_time\": [\"09:30\", \"21:00\"]），旧键后续版本会移除",
		strings.Join(fields, ", "))
}
