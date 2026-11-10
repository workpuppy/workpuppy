// school 开学季活动任务自动化 CLI —— 逐功能移植自
// scripts/school_open_day_2026.py 的 main()，二进制名 school_bin。
//
// 用法：
//
//	school_bin                          # 只读盘点全部账号（auths/workbuddy-*.json）
//	school_bin 00e26541                 # 只盘点指定 uid 前缀（可多个）
//	school_bin ALL --run --yes          # 点亮任务 + 领奖 + 抽奖（真实写）
//	school_bin --run                    # dry-run：只打印将发送的动作
//	school_bin --lottery-only --yes     # 只抽奖（清空抽奖次数）
//	school_bin --run --yes --account-delay-min 30 --account-delay-max 120
//
// 参数与脚本 argparse 逐项对齐：accounts / --list / --run / --lottery-only /
// --yes / --token / --uid / --gap / --account-delay-min / --account-delay-max。
// --only 是 Go 侧扩展（脚本没有；空值行为与脚本完全一致），用于只处理某类任务。
//
// 退出码与脚本一致：有 fail 退 2；无可用账号退 1；否则 0。
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"wp/internal/growth"
)

// valueFlags 需要额外吃一个值的选项名（用于 argv 重排）。
var valueFlags = map[string]bool{
	"token":             true,
	"uid":               true,
	"gap":               true,
	"only":              true,
	"account-delay-min": true,
	"account-delay-max": true,
}

// permuteArgs 把 argv 拆成「flag 段」与「位置参数」。
//
// 为什么需要：标准库 flag 遇到第一个非 flag 就停止解析，而脚本 argparse 允许
// `uid1 --run --yes` 这种混排；不重排就会把 --run 当成 uid 前缀传下去。
func permuteArgs(argv []string) (flags, positional []string) {
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--" { // 终止符之后全按位置参数
			positional = append(positional, argv[i+1:]...)
			return flags, positional
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if strings.ContainsRune(name, '=') {
				continue // --gap=2.0 自带值
			}
			if valueFlags[name] && i+1 < len(argv) {
				i++
				flags = append(flags, argv[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return flags, positional
}

// pyFloat 按 python 的 float 文本形态输出（2 -> "2.0"，1.5 -> "1.5"），
// 让 gap=… 这行日志与脚本一致。
func pyFloat(f float64) string {
	if !math.IsInf(f, 0) && !math.IsNaN(f) && f == math.Trunc(f) {
		return strconv.FormatFloat(f, 'f', 1, 64)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// uid8 日志短标签（脚本 uid8：uid 前 8 位，空则 "?"）。
func uid8(a *growth.Auth) string {
	if a.UID == "" {
		return "?"
	}
	if len(a.UID) > 8 {
		return a.UID[:8]
	}
	return a.UID
}

func main() {
	fs := flag.NewFlagSet("school_open_day", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "用法: %s [ALL|uid...] [选项]\n\n", "school_bin")
		fmt.Fprintln(fs.Output(), "小程序开学季任务中心：只读盘点 + 事件上报自动化")
		fmt.Fprintln(fs.Output(), "\n位置参数:")
		fmt.Fprintln(fs.Output(), "  ALL|uid...          uid 前缀（可多个）或 ALL；--token 时可不传")
		fmt.Fprintln(fs.Output(), "\n选项:")
		fs.PrintDefaults()
	}
	// --list 与脚本同：只读盘点是默认行为，这个开关不改变任何逻辑（保留兼容）。
	fs.Bool("list", false, "只读盘点（默认行为）")
	runFlag := fs.Bool("run", false, "执行上报/领取动作（需 --yes 放行真实写）")
	lotteryOnly := fs.Bool("lottery-only", false, "只抽奖不做任务（读余额+抽到空）")
	yes := fs.Bool("yes", false, "放行写操作（配合 --run/--lottery-only）")
	token := fs.String("token", "", "显式覆盖 token（不同源凭证逃生门）")
	luid := fs.String("uid", "", "配合 --token 指定 uid（仅日志用）")
	gap := fs.Float64("gap", 1.5, "写动作间隔秒数（默认 1.5，最小 1.0）")
	delayMin := fs.Int("account-delay-min", 0, "Minimum delay between accounts in seconds (default 0)")
	delayMax := fs.Int("account-delay-max", 0, "Maximum randomized account delay in seconds (0 disables)")
	only := fs.String("only", "", "只处理匹配的任务（task_code / share / report / mini_chat / desktop_seq / expert，逗号分隔；Go 侧扩展）")

	flags, positional := permuteArgs(os.Args[1:])
	if err := fs.Parse(flags); err != nil {
		os.Exit(2) // flag.ExitOnError 已退出；保底
	}
	accounts := positional

	// 账号间隔参数非法直接退（脚本走 parser.error -> 退 2）。
	spacing, err := growth.NewAccountSpacing(*delayMin, *delayMax)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERR: %v\n", err)
		os.Exit(2)
	}
	if *gap < 1.0 {
		*gap = 1.0 // 写操作间隔 ≥1s
	}
	opt := growth.SchoolOptions{
		Run:         *runFlag,
		LotteryOnly: *lotteryOnly,
		Yes:         *yes,
		Gap:         time.Duration(*gap * float64(time.Second)),
		Only:        *only,
	}

	st := &growth.SchoolStats{}
	// 抽奖也是执行动作（写 draw）
	runMode := *runFlag || *lotteryOnly
	mode := "LIST"
	if *lotteryOnly {
		mode = "LOTTERY"
	} else if *runFlag {
		mode = "RUN"
	}
	if runMode && !*yes {
		fmt.Printf("mode=%s dry-run（写操作需 --yes 放行；%s 不带 --yes 只打印将发送的动作）\n", mode, mode)
	} else {
		real := ""
		if *yes {
			real = "REAL"
		}
		fmt.Printf("mode=%s %s gap=%s\n", mode, real, pyFloat(*gap))
	}

	var auths []*growth.Auth
	if *token != "" {
		// 不同源凭证逃生门：--token 时不读 auths/，uid 仅供日志。
		auths = append(auths, &growth.Auth{Token: *token, UID: *luid, Nick: "--token--", File: "<cli>"})
	} else {
		var prefixes []string
		if len(accounts) == 0 || (len(accounts) == 1 && strings.EqualFold(accounts[0], "ALL")) {
			// ALL：按 auths/workbuddy-*.json 文件名推导 uid 前缀（脚本 basename[10:18]），
			// 排序后逐个 load_auth。短 uid 也正确（见 growth.UIDPrefixFromFile）。
			hits, _ := filepath.Glob(filepath.Join(growth.AuthsDir(), "workbuddy-*.json"))
			sort.Strings(hits)
			for _, p := range hits {
				prefixes = append(prefixes, growth.UIDPrefixFromFile(filepath.Base(p)))
			}
		} else {
			prefixes = accounts
		}
		// 去重但保序（脚本用 seen 集合 + 列表）
		seen := make(map[string]bool, len(prefixes))
		uniq := make([]string, 0, len(prefixes))
		for _, p := range prefixes {
			if !seen[p] {
				seen[p] = true
				uniq = append(uniq, p)
			}
		}
		for _, p := range uniq {
			a, err := growth.LoadAuth(p)
			if err != nil {
				fmt.Printf("ERR: %v\n", err)
				continue
			}
			auths = append(auths, a)
		}
	}

	if len(auths) == 0 {
		fmt.Println("ERR: 无可用账号（检查 auths/ 或 --token）")
		os.Exit(1)
	}

	for _, a := range auths {
		// global realm 不适用 CN 开学季任务/转盘：明确跳过、不发起任何请求。
		if a.IsGlobal() {
			fmt.Printf("[skip] %s global realm 不适用 CN 任务\n", uid8(a))
			st.Skip++
			continue
		}
		spacing.Wait()
		var err error
		switch {
		case opt.LotteryOnly:
			err = growth.LotteryAccount(a, opt, st, true)
		case runMode:
			err = growth.RunAccount(a, opt, st)
		default:
			err = growth.ListAccount(a, st)
		}
		if err != nil {
			fmt.Printf("ERR: [school2026] %s 处理失败: %v\n", uid8(a), err)
			st.Fail++
		}
	}

	fmt.Printf("school2026 done: accounts=%d ok=%d already=%d skipped=%d pending=%d fail=%d\n",
		st.Accounts, st.OK, st.Already, st.Skip, st.Pending, st.Fail)
	growth.PrintNotes()
	if st.Fail > 0 {
		os.Exit(2) // 与脚本一致：有失败退 2
	}
}
