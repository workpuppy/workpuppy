// admin_banner.go 启动横幅：把本次运行的管理密码打印到终端。
//
// 为什么打在"端口已绑定之后"：横幅是给人看的，必须只在服务真的能接连接时才出现——
// 否则端口占用导致启动失败时，日志里还躺着一个看起来可用的密码，排障会被带偏。
package main

import (
	"log"

	"wp/internal/adminauth"
)

// printAdminBanner 打印管理后台入口与本次运行的凭据。
//
// 注意：密码**只在这里出现一次**，之后只存在进程内存里。重启即换新密码，
// 旧密码立刻失效；要固定密码只能设 WP_ADMIN_PASSWORD（开发/自动化用）。
func printAdminBanner(store *adminauth.Store, listen string) {
	url := adminURL(listen)
	src := "本次启动随机生成"
	if store.Source() == "override" {
		src = "来自 WP_ADMIN_PASSWORD（固定值，仅建议开发/自动化使用）"
	}
	log.Printf("\n"+
		"============================================================\n"+
		"  管理后台: %s\n"+
		"  登录密码: %s    （%s）\n"+
		"  用户名:  不需要，直接填密码\n"+
		"  该密码只存在内存中，重启服务会自动更换；忘记请看本次容器日志。\n"+
		"  提示：/v1 与 /status 仍用 config.json 的 api_key，与本密码相互独立。\n"+
		"============================================================",
		url, store.Password(), src)
}

// adminURL 把监听地址拼成可直接点开的地址（0.0.0.0/:: → 127.0.0.1）。
func adminURL(listen string) string {
	host := listen
	port := ""
	// 拆出 host:port（监听地址形如 ":7863"、"127.0.0.1:7863"、"[::]:7863"）
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			host = listen[:i]
			port = listen[i+1:]
			break
		}
	}
	host = trimBrackets(host)
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	if port == "" {
		return "http://" + host
	}
	return "http://" + host + ":" + port + "/"
}

func trimBrackets(s string) string {
	if len(s) >= 2 && s[0] == '[' && s[len(s)-1] == ']' {
		return s[1 : len(s)-1]
	}
	return s
}
