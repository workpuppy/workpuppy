package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// runKey 打印 config.json 的 api_key（空值时打印空行，调用方自行判定）。
func runKey() {
	key, _ := readConfigKeyAndAddr()
	fmt.Println(key)
}

// trimBOM 去掉 UTF-8 BOM（Windows 记事本 / PowerShell 5.1 写出的配置常带它，
// json 包遇到 \ufeff 会直接报解析错误）。
func trimBOM(b []byte) []byte {
	return bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
}

// statusAccountCount 读 /status 并返回账号数（login.sh 重启后的收尾检查用）。
func statusAccountCount(url, key string) (int, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	c := &http.Client{Timeout: 10 * time.Second}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, err
	}
	var payload struct {
		Accounts []json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, err
	}
	return len(payload.Accounts), nil
}
