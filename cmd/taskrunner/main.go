// task_runner —— 批量「查询 → 完成(点亮) → 领取」成长任务的一体化 CLI。
//
// 移植自 scripts/task_runner.py 的 main()（argparse 逐项对齐）。为什么用 Go：目标形态是
// 「单镜像 + Go 二进制」，镜像不装 python3；原脚本由面板/subprocess exec python 调用，
// 与 internal/growth 的同类调用长期来回漂移。判据与端点实现全部在 internal/growth/tasks.go。
//
// 编译（二进制名是 task_runner，目录名只是 taskrunner，靠 -o 定名；Dockerfile 已这么打）：
//
//	go build -o task_runner ./cmd/taskrunner
//
// 用法（与脚本一致）：
//
//	task_runner <uid 前缀|ALL> [更多 uid...] [选项]
//
//	task_runner 00e26541                      # dry-run：查询展示任务现状
//	task_runner 00e26541 --yes               # 全量：accept→点亮→回读→claim
//	task_runner 00e26541 --yes --only-claim  # 只领奖（已完成直接入账，已领幂等）
//	task_runner 00e26541 --yes --only template_5   # 只处理指定任务（可多次）
//	task_runner ALL --yes                    # 批量（写操作慎用，符合预算才执行）
//	task_runner ALL --yes --only black_cat   # 夜猫子任务（调度器就是这么调的）
//
// 选项（与脚本 argparse 一一对应）：
//
//	--yes                    真实执行写操作；缺省 dry-run，不发任何上报/accept/claim
//	--only <task_code>       只处理指定 task_code（可重复；取值集合是 task_runner 的
//	                         MAPPING 键 + 开学季/小程序任务码，未知名按「任务不存在」跳过）
//	--only-claim             只领奖不点亮（已领任务服务端幂等）
//	--gap <秒>               上报/领奖动作间隔，默认 1.0，小于 1.0 一律抬到 1.0
//	--account-delay-min N    账号间最小延迟（秒，默认 0）
//	--account-delay-max N    账号间最大随机延迟（秒，默认 0=关闭）
//	--dry-run                显式 dry-run（脚本没有这个开关：缺省即 dry-run，等同不给 --yes）
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"wp/internal/growth"
)

const usage = `task_runner —— 成长任务一体机：查询 → 完成(点亮) → 领取

用法：
  task_runner [ALL|uid...] [--only <task_code>] [--yes] [--dry-run]
              [--only-claim] [--gap <秒>] [--account-delay-min N] [--account-delay-max N]

参数：
  accounts             uid 前缀（可多个）或 ALL（= auths/workbuddy-*.json 全部）

选项：
  --yes                真实执行写操作（默认 dry-run）
  --only <task_code>   只处理指定 task_code（可多次）
  --only-claim         只领奖不点亮（已完成任务直接 claim，已领幂等）
  --gap <秒>           动作间隔秒数（默认 1.0，最小 1.0）
  --account-delay-min  账号间最小延迟秒数（默认 0）
  --account-delay-max  账号间最大随机延迟秒数（0 表示关闭）
  --dry-run            显式 dry-run（等价于不给 --yes）
`

// errHelp 让 -h/--help 走正常退出（argparse 的 help 也是退出码 0）。
var errHelp = fmt.Errorf("help requested")

func main() {
	if err := run(os.Args[1:]); err != nil {
		if err == errHelp {
			return
		}
		fmt.Fprintf(os.Stderr, "task_runner: %v\n", err)
		os.Exit(2)
	}
}

// cliArgs 解析后的命令行参数（字段与脚本 argparse 的 Namespace 对应）。
type cliArgs struct {
	Accounts        []string
	Yes             bool
	OnlyCodes       []string
	OnlyClaim       bool
	Gap             float64
	AccountDelayMin int
	AccountDelayMax int
	DryRun          bool
}

// run 解析参数并跑完整个批量流程。返回错误时 main 以退出码 2 结束（与 argparse 的
// parser.error 同码）。
func run(args []string) error {
	a, err := parseArgs(args)
	if err != nil {
		if err == errHelp {
			fmt.Print(usage)
			return errHelp
		}
		fmt.Print(usage)
		return err
	}
	if a.Gap < 1.0 {
		a.Gap = 1.0 // 写操作间隔 ≥1s（脚本同款下限）
	}
	// 先按脚本顺序构造运行器（非法间隔区间在打印 mode 行之前就报错退出）。
	runner, err := growth.NewRunner(growth.Options{
		Yes:             a.Yes,
		OnlyCodes:       a.OnlyCodes,
		OnlyClaim:       a.OnlyClaim,
		Gap:             a.Gap,
		AccountDelayMin: a.AccountDelayMin,
		AccountDelayMax: a.AccountDelayMax,
		DryRun:          a.DryRun,
	})
	if err != nil {
		return err
	}

	prefixes := growth.ResolvePrefixes(a.Accounts)
	mode := "DRY-RUN"
	if a.Yes {
		mode = "REAL"
	}
	only := "all"
	if len(a.OnlyCodes) > 0 {
		only = pyList(a.OnlyCodes)
	}
	fmt.Printf("mode=%s accounts=%s only=%s only_claim=%s gap=%s\n",
		mode, pyList(prefixes), only, pyBool(a.OnlyClaim), pyFloat(a.Gap))
	if a.Yes && len(prefixes) > 2 && len(a.OnlyCodes) == 0 && !a.OnlyClaim {
		fmt.Println("WARN: 全量批量 + --yes 未限定 --only，注意 54 号批量——请确认 Hermes 决策后再跑")
	}

	runner.Run(prefixes)
	return nil
}

// parseArgs 手工解析（argparse 允许选项与位置参数交错，Go 的 flag 包做不到；
// 调度器实际就是 `task_runner ALL --yes --only black_cat --account-delay-min N ...`）。
// 支持 `--flag value` 与 `--flag=value` 两种写法。
func parseArgs(args []string) (cliArgs, error) {
	a := cliArgs{Gap: 1.0}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			return a, errHelp
		case arg == "--yes":
			a.Yes = true
		case arg == "--dry-run":
			a.DryRun = true
		case arg == "--only-claim":
			a.OnlyClaim = true
		case arg == "--only" || strings.HasPrefix(arg, "--only="):
			v, next, err := flagValue(args, i, "--only")
			if err != nil {
				return a, err
			}
			a.OnlyCodes = append(a.OnlyCodes, v)
			i = next
		case arg == "--gap" || strings.HasPrefix(arg, "--gap="):
			v, next, err := flagValue(args, i, "--gap")
			if err != nil {
				return a, err
			}
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return a, fmt.Errorf("argument --gap: invalid float value: %q", v)
			}
			a.Gap = f
			i = next
		case arg == "--account-delay-min" || strings.HasPrefix(arg, "--account-delay-min="):
			v, next, err := flagValue(args, i, "--account-delay-min")
			if err != nil {
				return a, err
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return a, fmt.Errorf("argument --account-delay-min: invalid int value: %q", v)
			}
			a.AccountDelayMin = n
			i = next
		case arg == "--account-delay-max" || strings.HasPrefix(arg, "--account-delay-max="):
			v, next, err := flagValue(args, i, "--account-delay-max")
			if err != nil {
				return a, err
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return a, fmt.Errorf("argument --account-delay-max: invalid int value: %q", v)
			}
			a.AccountDelayMax = n
			i = next
		case strings.HasPrefix(arg, "-") && arg != "-":
			return a, fmt.Errorf("unrecognized arguments: %s", arg)
		default:
			a.Accounts = append(a.Accounts, arg)
		}
	}
	if len(a.Accounts) == 0 {
		return a, fmt.Errorf("the following arguments are required: accounts")
	}
	return a, nil
}

// flagValue 取 `--name value` 或 `--name=value` 的值，返回 (值, 新的循环下标, 错误)。
func flagValue(args []string, i int, name string) (string, int, error) {
	arg := args[i]
	if strings.HasPrefix(arg, name+"=") {
		return strings.TrimPrefix(arg, name+"="), i, nil
	}
	if i+1 >= len(args) {
		return "", i, fmt.Errorf("argument %s: expected one argument", name)
	}
	return args[i+1], i + 1, nil
}

// pyList 打印成 python 列表外观（`['a', 'b']`），便于与旧脚本日志逐字对比。
func pyList(xs []string) string {
	parts := make([]string, 0, len(xs))
	for _, x := range xs {
		parts = append(parts, "'"+x+"'")
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// pyBool python 的 bool 外观（True/False），日志口径与旧脚本一致。
func pyBool(b bool) string {
	if b {
		return "True"
	}
	return "False"
}

// pyFloat python 的 float 外观：整数也带 ".0"（脚本打印 gap=1.0）。
func pyFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}
