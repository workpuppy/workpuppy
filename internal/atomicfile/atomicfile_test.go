package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteAndOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := Write(path, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := read(t, path); got != `{"v":1}` {
		t.Fatalf("content=%q", got)
	}
	if err := Write(path, []byte(`{"v":2}`), 0o600); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if got := read(t, path); got != `{"v":2}` {
		t.Fatalf("overwrite content=%q", got)
	}
	// 除目标文件外的临时文件必须清理干净（掉电残留可以由用户手工清，正常运行不该有）。
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "state.json" {
		t.Fatalf("目录残留：%v", ents)
	}
}

func TestWriteWithBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credit-daily.json")
	if err := WriteWithBackup(path, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatalf("first: %v", err)
	}
	// 首次写入没有旧文件，不应产生备份。
	if _, err := os.Stat(BackupPath(path)); !os.IsNotExist(err) {
		t.Fatalf("首次写入不应有 .bak: %v", err)
	}
	if err := WriteWithBackup(path, []byte(`{"v":2}`), 0o600); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := read(t, path); got != `{"v":2}` {
		t.Fatalf("main=%q", got)
	}
	if got := read(t, BackupPath(path)); got != `{"v":1}` {
		t.Fatalf("bak=%q want 上一份", got)
	}
	// 第三次：备份滚动为第二份，主文件为第三份。
	if err := WriteWithBackup(path, []byte(`{"v":3}`), 0o600); err != nil {
		t.Fatalf("third: %v", err)
	}
	if got := read(t, path); got != `{"v":3}` {
		t.Fatalf("main=%q", got)
	}
	if got := read(t, BackupPath(path)); got != `{"v":2}` {
		t.Fatalf("bak=%q want 第二份", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("权限=%v want 0600", perm)
		}
	}
}

// 目标路径是目录时写入必须失败，且不能把原目录替换掉。
func TestWriteFailsOnDirectoryTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state.json")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Write(target, []byte("x"), 0o600); err == nil {
		t.Fatal("目标是目录时应失败")
	}
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		t.Fatalf("原目录被破坏：%v %v", info, err)
	}
}

func TestBackupPath(t *testing.T) {
	if got := BackupPath("/a/b.json"); got != "/a/b.json.bak" {
		t.Fatalf("BackupPath=%q", got)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
