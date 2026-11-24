// Package atomicfile 提供断电安全的文件写入：
// 同目录临时文件 → 写入 → fsync → rename → 目录 fsync。
//
// 为什么需要它：os.WriteFile + os.Rename 只保证"进程视角的原子替换"，不保证
// **掉电后**的持久性——rename 的元数据可能先落盘，而文件内容还在页缓存里。
// 断电后典型结果就是目标文件变成 0 字节或半截 JSON；而本项目的运行期数据
// （积分日账、账号池状态、接入 key）在启动时遇到坏文件往往只能从空开始，
// 一次断电就会把历史统计全部抹掉。
//
// 这里的两条 API：
//   - Write：单文件原子写（新内容落盘后才替换旧文件，崩溃最多回到旧版本）；
//   - WriteWithBackup：在替换前把旧文件改名为 <path>.bak，供读侧在"新文件损坏/
//     丢失"时回退。适合积分日账这类整份重写、且历史价值高的数据。
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write 原子写入 path：全程不破坏旧文件，成功返回时新内容已 fsync。
//
// perm 只作用于新建的临时文件；替换后目标文件继承该权限（与 os.WriteFile 一致）。
func Write(path string, data []byte, perm os.FileMode) error {
	return write(path, data, perm, false)
}

// WriteWithBackup 同 Write，但在替换前把现有文件改名为 path+".bak"。
//
// 读侧应优先读主文件、失败再读 .bak；这样即使断电恰好落在"改名完成、新文件
// 尚未就绪"的窗口，重启后仍能从上一份快照恢复，而不是从空数据开始。
func WriteWithBackup(path string, data []byte, perm os.FileMode) error {
	return write(path, data, perm, true)
}

// BackupPath 返回 WriteWithBackup 使用的备份路径（读侧/测试共用，避免拼字面量）。
func BackupPath(path string) string { return path + ".bak" }

func write(path string, data []byte, perm os.FileMode, backup bool) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// rename 成功后目标文件已不依赖 tmp；失败路径统一清理，避免留垃圾。
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	// 关键一步：内容必须真正落盘，否则断电后 rename 可能指向一个空文件。
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	bak := BackupPath(path)
	movedOld := false
	if backup {
		if _, statErr := os.Stat(path); statErr == nil {
			// 先删旧备份：Windows 的 rename 不覆盖已存在的目标。
			_ = os.Remove(bak)
			if err := os.Rename(path, bak); err != nil {
				return err
			}
			movedOld = true
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		if movedOld {
			// 尽力回滚：至少让主文件恢复成上一份，而不是直接消失。
			_ = os.Rename(bak, path)
		}
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir 尽力把目录项（rename 的元数据）刷到磁盘。
//
// 目录 fsync 在部分平台（如 Windows）不被支持，失败时忽略：文件内容已经 Sync，
// 目录项丢失最多回到旧版本，而不是产生半截文件。不在失败时返回错误，避免
// "文件系统不支持 D_FSYNC" 把正常保存判成失败。
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
