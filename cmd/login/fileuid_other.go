//go:build !unix

package main

// fileUID 非 Unix 平台没有 POSIX uid 语义，恒返回 false（不做属主提示）。
// 本项目目标平台是 Linux/Docker，这里只是让跨平台编译仍然成立。
func fileUID(string) (int, bool) { return 0, false }
