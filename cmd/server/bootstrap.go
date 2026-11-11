// bootstrap.go 配置文件的落地：首次启动生成 data/config.json。
//
// 设计取舍：
//   - **由进程自己做，而不是 docker entrypoint 脚本**：裸二进制、docker、开发模式
//     行为一致，也能被 Go 测试直接覆盖；容器只是"第一次启动时顺手生成"。
//   - 生成内容 = 内嵌的 config.example.json（默认值唯一来源），权限 0600：
//     文件里有 api_key 与密码哈希，不应该让同机其他用户读到。
//   - **不做旧 config.json 的自动迁移**（按需求）：升级后如果 data/config.json 不存在，
//     会生成一份新的默认配置；旧的 /app/config.json 原样留在那里，需要时手工搬。
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"wp/internal/configdata"
)

// defaultConfigPath 默认配置路径：data/config.json（与 state.json / auths 同目录挂载）。
const defaultConfigPath = "data/config.json"

// ensureConfigFile 保证配置文件存在；不存在则创建目录并写入示例配置。
func ensureConfigFile(path string) (bool, error) {
	if path == "" {
		path = defaultConfigPath
	}
	st, statErr := os.Stat(path)
	switch {
	case statErr == nil && st.IsDir():
		return false, fmt.Errorf("%s 是个目录而不是文件（常见原因：宿主机挂载了同名目录）", path)
	case statErr == nil:
		return false, nil
	case !errors.Is(statErr, os.ErrNotExist):
		return false, fmt.Errorf("检查配置文件 %s: %w", path, statErr)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, fmt.Errorf("创建配置目录 %s: %w（容器内请 chown -R 10001:10001 ./data）", dir, err)
		}
	}
	if err := writeFileAtomic(path, configdata.Bytes(), 0o600); err != nil {
		return false, fmt.Errorf("生成 %s: %w（目录是否可写？容器内请 chown -R 10001:10001 ./data）", path, err)
	}
	return true, nil
}

// writeFileAtomic 原子落盘：同目录临时文件 → fsync → rename。
//
// 配置与 owner 密码都写这份文件，半截写入会让服务下次启动直接起不来，
// 所以这里不用 os.WriteFile 的"就地覆盖"。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, ".wp-config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName) // rename 成功后这里什么也不做
	}()
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
