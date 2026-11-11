package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeScriptExec 记录命令构建参数并按需模拟执行失败，替代真实 exec 拉起子进程。
type fakeScriptExec struct {
	lastName string
	lastArgs []string
	lastDir  string
	runN     int
	err      error
}

func (f *fakeScriptExec) SetDir(dir string) { f.lastDir = dir }
func (f *fakeScriptExec) Run() error        { f.runN++; return f.err }

// installFakeExec 替换 newScriptCmd，测试结束还原。
func installFakeExec(t *testing.T) *fakeScriptExec {
	t.Helper()
	f := &fakeScriptExec{}
	orig := newScriptCmd
	newScriptCmd = func(name string, args ...string) scriptRunner {
		f.lastName, f.lastArgs = name, args
		return f
	}
	t.Cleanup(func() { newScriptCmd = orig })
	return f
}

// installTaskBins 造一个临时目录，放入名为 bins 的可执行占位文件，并把
// WP_TASK_BIN_DIR 指过去（taskBin 的第一优先级）。
func installTaskBins(t *testing.T, bins ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range bins {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatalf("chmod %s: %v", p, err)
		}
	}
	t.Setenv("WP_TASK_BIN_DIR", dir)
	return dir
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestNextWakeSchoolSlot 开学季任务在 school_hours（默认 12 点）处有独立时点。
func TestNextWakeSchoolSlot(t *testing.T) {
	s := New(Config{
		CheckinAt:         []int{540},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolAt:          []int{720},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 11, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 14, 12, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（school 12:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskSchool {
		t.Errorf("kinds=%v want [school]", kinds)
	}
}

// TestNextWakeCatSlot 夜猫子任务在 cat_hours（默认 1 点）处有独立时点。
func TestNextWakeCatSlot(t *testing.T) {
	s := New(Config{
		CheckinAt:         []int{540},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		CatAt:             []int{60},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 23, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 15, 1, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（cat 01:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCat {
		t.Errorf("kinds=%v want [cat]", kinds)
	}
}

// TestNextWakeSchoolCatDisabled 显式禁用 school/cat 后排程只剩签到时点（互不影响）。
func TestNextWakeSchoolCatDisabled(t *testing.T) {
	s := New(Config{
		CheckinAt:         []int{1260},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolAt:          []int{720},
		CatAt:             []int{60},
		SchoolDisabled:    true,
		CatDisabled:       true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 14, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（school/cat 禁用 → 只有签到 21:00）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestRunSchoolNowBuildsCommand RunSchoolNow 执行 `<bin>/school_bin ALL --run --yes`。
// 脚本类任务已 Go 化：解释器不再出现，第一个参数必须是二进制路径。
func TestRunSchoolNowBuildsCommand(t *testing.T) {
	dir := installTaskBins(t, TaskBinSchool)
	f := installFakeExec(t)
	s := New(Config{})
	s.RunSchoolNow()

	want := filepath.Join(dir, TaskBinSchool)
	if f.lastName != want {
		t.Errorf("name=%q want %q（应执行同目录二进制而非解释器）", f.lastName, want)
	}
	if wantArgs := []string{"ALL", "--run", "--yes"}; !equalArgs(f.lastArgs, wantArgs) {
		t.Errorf("args=%v want %v", f.lastArgs, wantArgs)
	}
	if f.lastDir != workDir() {
		t.Errorf("dir=%q want %q", f.lastDir, workDir())
	}
	if f.runN != 1 {
		t.Errorf("runN=%d want 1", f.runN)
	}
}

// TestRunCatNowBuildsCommand RunCatNow 执行 `<bin>/task_runner ALL --yes --only black_cat`。
func TestRunCatNowBuildsCommand(t *testing.T) {
	dir := installTaskBins(t, TaskBinRunner)
	f := installFakeExec(t)
	s := New(Config{})
	s.RunCatNow()

	want := filepath.Join(dir, TaskBinRunner)
	if f.lastName != want {
		t.Errorf("name=%q want %q", f.lastName, want)
	}
	if wantArgs := []string{"ALL", "--yes", "--only", "black_cat"}; !equalArgs(f.lastArgs, wantArgs) {
		t.Errorf("args=%v want %v", f.lastArgs, wantArgs)
	}
}

// TestTaskBinMissingWarnsOnly 二进制缺失时只记 WARN、不 panic、不执行任何命令。
func TestTaskBinMissingWarnsOnly(t *testing.T) {
	t.Setenv("WP_TASK_BIN_DIR", t.TempDir()) // 空目录
	f := installFakeExec(t)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	s := New(Config{})
	s.RunSchoolNow()
	if f.runN != 0 {
		t.Errorf("缺二进制时不应执行任何命令，runN=%d", f.runN)
	}
	if !strings.Contains(buf.String(), "WARN") || !strings.Contains(buf.String(), TaskBinSchool) {
		t.Errorf("缺二进制应记 WARN 并点名文件：\n%s", buf.String())
	}
}

// TestDispatchSchoolCatAndFailureWarnsOnly dispatch 把 school/cat 分发给对应二进制；
// 子进程失败只记 WARN（不 panic/不向上抛），且不影响后续任务继续分发。
func TestDispatchSchoolCatAndFailureWarnsOnly(t *testing.T) {
	installTaskBins(t, TaskBinSchool, TaskBinRunner)
	f := installFakeExec(t)
	f.err = errors.New("boom boom")
	s := New(Config{})

	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	s.dispatch(context.Background(), taskSchool)
	if f.runN != 1 || !strings.HasSuffix(f.lastName, TaskBinSchool) {
		t.Errorf("dispatch(school) 未执行: runN=%d last=%v", f.runN, f.lastName)
	}
	s.dispatch(context.Background(), taskCat)
	if f.runN != 2 || !strings.HasSuffix(f.lastName, TaskBinRunner) {
		t.Errorf("dispatch(cat) 未执行: runN=%d last=%v", f.runN, f.lastName)
	}
	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, TaskBinSchool) {
		t.Errorf("school 失败未按 WARN 记录:\n%s", out)
	}
	if !strings.Contains(out, TaskBinRunner) {
		t.Errorf("cat 失败未按 WARN 记录:\n%s", out)
	}
}

// TestTaskBinPrefersEnvDir taskBin 的第一优先级是 WP_TASK_BIN_DIR。
func TestTaskBinPrefersEnvDir(t *testing.T) {
	dir := installTaskBins(t, TaskBinSchool)
	got, ok := taskBin(TaskBinSchool)
	if !ok {
		t.Fatal("应能找到 WP_TASK_BIN_DIR 下的二进制")
	}
	if got != filepath.Join(dir, TaskBinSchool) {
		t.Errorf("taskBin=%q want %q", got, filepath.Join(dir, TaskBinSchool))
	}

	if _, ok := taskBin("no-such-bin-xyz"); ok {
		t.Error("不存在的二进制定位应当失败")
	}
}
