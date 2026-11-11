// admin_banner.go 启动横幅：告诉运维"后台在哪、密码处于什么状态"。
//
// 为什么打在"端口已绑定之后"：横幅是给人看的，必须只在服务真的能接连接时才出现——
// 否则端口占用导致启动失败时，日志里还躺着一个看起来可用的入口。
//
// 注意：这里**不再打印密码**。密码只在初始化时由用户自己设定，服务端只保存
// 加盐 10 轮 SHA256 的哈希（data/config.json 的 admin.password）。
package main

import (
	"log"

	"wp/internal/adminauth"
)

// printAdminBanner 打印管理后台入口与密码状态。
func printAdminBanner(store *adminauth.Store, listen, configPath string) {
	url := adminURL(listen)
	if store.Configured() {
		log.Printf("\n"+
			"============================================================\n"+
			"  管理后台: %s\n"+
			"  管理密码: 已初始化（哈希存于 %s 的 admin.password）\n"+
			"  忘记密码: 删除该字段后重启 → 页面会重新引导初始化\n"+
			"  提示: /v1 与 /status 仍用 config.json 的 api_key，与本密码相互独立。\n"+
			"============================================================",
			url, configPath)
		return
	}
	log.Printf("\n"+
		"============================================================\n"+
		"  管理后台: %s\n"+
		"  管理密码: 尚未初始化\n"+
		"  下一步:   用浏览器打开上面的地址，按页面提示设置管理员密码\n"+
		"            （设置成功后写入 %s 的 admin.password，并自动开启管理面）\n"+
		"  提示: /v1 与 /status 仍用 config.json 的 api_key，与本密码相互独立。\n"+
		"============================================================",
		url, configPath)
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
