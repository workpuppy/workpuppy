//go:build unix

package main

import (
	"os"
	"syscall"
)

// fileUID 返回文件属主 uid。取不到（异常文件系统）返回 ok=false。
//
// 为什么需要它：容器内 app 以 uid 10001 运行，宿主机若用别的 uid 落盘，容器内
// 读不到那份凭证 —— 表现是「登录成功但账号数为 0」（issue #108）。这条检查把
// 提示从「玄学」变成一句可执行的 chown。
func fileUID(path string) (int, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), true
	}
	return 0, false
}
