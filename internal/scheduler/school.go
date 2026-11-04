// school.go 开学季任务与夜猫子任务的脚本类排程：从系统 crontab 迁入 Go scheduler。
//
// 背景：school（12:00）与 cat（01:00 夜猫窗口）原由系统 crontab 调
// scripts/school_open_day_cron.sh 执行——依赖外部系统 cron、容器重建可能丢失、
// 不在 config 里配置。迁入后成为第五、第六类任务，时点由 schedule.school_hours /
// schedule.cat_hours 配置。
//
// **Go 化改造**：这两类任务的实现原本是 python 脚本（scripts/school_open_day_2026.py、
// scripts/task_runner.py），现在搬进了 internal/growth，编译成两个独立二进制
// （school_bin / task_runner），与本服务一起打进镜像。调度器仍然用「子进程」方式
// 跑它们 —— 而不是进程内直接调用 —— 原因有三：① 它们各自可能跑十几分钟、输出
// 大量日志，子进程可以整体超时终止；② 崩溃隔离（脚本 panic 不该带倒网关）；
// ③ 与 `docker exec` 手动触发（school_open_day_cron.sh）走完全同一条路径，
// 手工与定时的行为不会分叉。
package scheduler

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// TaskBinNames 脚本类任务的可执行文件名（与 cmd/ 目录一一对应）。
const (
	TaskBinSchool = "school_bin"
	TaskBinRunner = "task_runner"
)

// scriptRunner 脚本子进程的最小执行面：可被测试替换，避免测试真正拉起子进程。
type scriptRunner interface {
	SetDir(string)
	Run() error
}

// scriptCmd exec.Cmd 适配器：把 exec.Cmd 的 Dir 字段包装成 SetDir 方法，
// 满足 scriptRunner 接口（exec.Cmd 本身只有字段没有方法）。
type scriptCmd struct{ cmd *exec.Cmd }

func (c *scriptCmd) SetDir(dir string) { c.cmd.Dir = dir }
func (c *scriptCmd) SetContext(ctx context.Context) {
	c.cmd = exec.CommandContext(ctx, c.cmd.Path, c.cmd.Args[1:]...)
}
func (c *scriptCmd) Run() error { return c.cmd.Run() }

// newScriptCmd 构建子进程。包级变量便于测试注入 fake（installFakeExec 覆盖）。
var newScriptCmd = func(program string, args ...string) scriptRunner {
	return &scriptCmd{cmd: exec.Command(program, args...)}
}

// taskBin 解析任务二进制的绝对路径。
//
// 顺序：WB2A_TASK_BIN_DIR → 与本进程同一目录 → 当前工作目录 → PATH。
// 为什么先看「本进程所在目录」：镜像里全部二进制平铺在 /app，容器以 /app 为
// WORKDIR 启动，这条命中率最高；源码模式下 `go run ./cmd/server` 的临时二进制
// 目录里没有它们，于是落到 cwd——所以 cwd 必须是仓库根（dev.sh 就是这么跑的）。
func taskBin(name string) (string, bool) {
	if dir := strings.TrimSpace(os.Getenv("WB2A_TASK_BIN_DIR")); dir != "" {
		p := filepath.Join(dir, name)
		if isExecutable(p) {
			return p, true
		}
	}
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), name); isExecutable(p) {
			return p, true
		}
	}
	if wd, err := os.Getwd(); err == nil {
		if p := filepath.Join(wd, name); isExecutable(p) {
			return p, true
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, true
	}
	return "", false
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() || !fi.Mode().IsRegular() {
		return false
	}
	// Windows 上 FAT/NTFS 的可执行位是合成的（恒为 0666），按权限位判断会一律为假；
	// 目标平台是 Linux，这里只为了让本地（Windows）跑测试时不至于找不到二进制。
	return runtime.GOOS == "windows" || fi.Mode()&0o111 != 0
}

// runScript 依次执行若干命令：任一命令失败只记一行 WARN，不向上抛、
// 不影响调度主循环继续跑下一个时点。单命令失败不中断后续命令。
func runScript(name, dir string, commands [][]string) {
	runScriptContext(context.Background(), name, dir, commands)
}

func runScriptContext(ctx context.Context, name, dir string, commands [][]string) {
	for _, cmdArgs := range commands {
		if ctx.Err() != nil {
			return
		}
		c := newScriptCmd(cmdArgs[0], cmdArgs[1:]...)
		if setter, ok := c.(interface{ SetContext(context.Context) }); ok {
			setter.SetContext(ctx)
		}
		c.SetDir(dir)
		if err := c.Run(); err != nil {
			log.Printf("WARN: %s (%s): %v", name, cmdArgs[0], err)
			continue
		}
		log.Printf("%s: ok (%s)", name, cmdArgs[0])
	}
}

// pausedByRisk 任一账号处于「刷新令牌风险暂停」时跳过整批任务。
//
// 为什么整批跳过而不是跳过那一个号：批量任务本身会刷新 token，暂停中的号
// 最怕的就是被别的路径顺手刷新（那正是暂停要防的事）。
func (s *Scheduler) pausedByRisk(task string) bool {
	if s.cfg.Pool == nil {
		return false
	}
	for _, account := range s.cfg.Pool.List() {
		if s.cfg.Pool.RiskPaused(account.UID) {
			log.Printf("scheduled %s skipped: token refresh safety pause", task)
			return true
		}
	}
	return false
}

// RunSchoolNow 立即执行开学季任务：school_bin ALL --run --yes。
// 全量跑任务点亮 + 领奖 + 自动抽空抽奖余额。活动下线（in_period=false）时各段
// 全量跳过、正常退出，不视为失败。失败只记 WARN。
func (s *Scheduler) RunSchoolNow() { s.runSchool(context.Background()) }

func (s *Scheduler) runSchool(ctx context.Context) {
	if s.pausedByRisk("school") {
		return
	}
	bin, ok := taskBin(TaskBinSchool)
	if !ok {
		log.Printf("WARN: 开学季任务跳过：找不到 %s（应与 wb2api 同目录，或用 WB2A_TASK_BIN_DIR 指定）", TaskBinSchool)
		return
	}
	runScriptContext(ctx, "school", workDir(), [][]string{
		s.accountScriptArgs(ctx, []string{bin, "ALL", "--run", "--yes"}),
	})
}

// RunCatNow 立即执行夜猫子任务：task_runner ALL --yes --only black_cat。
// black_cat 时段敏感：夜猫窗口 23:00–08:00 CST 内最多补 1 次（任务内部判定，
// 非窗口期打印 skip 正常退出）。失败只记 WARN。
func (s *Scheduler) RunCatNow() { s.runCat(context.Background()) }

func (s *Scheduler) runCat(ctx context.Context) {
	if s.pausedByRisk("cat") {
		return
	}
	bin, ok := taskBin(TaskBinRunner)
	if !ok {
		log.Printf("WARN: 夜猫子任务跳过：找不到 %s（应与 wb2api 同目录，或用 WB2A_TASK_BIN_DIR 指定）", TaskBinRunner)
		return
	}
	runScriptContext(ctx, "cat", workDir(), [][]string{
		s.accountScriptArgs(ctx, []string{bin, "ALL", "--yes", "--only", "black_cat"}),
	})
}

// workDir 子进程的工作目录：用当前进程的 cwd。
// 任务二进制自己解析账号目录（WB2A_AUTHS 优先），所以这里不需要「仓库根」这种概念了。
func workDir() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}
