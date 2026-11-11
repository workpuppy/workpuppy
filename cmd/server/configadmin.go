// configadmin.go 设置页的后端实现（server.ConfigStore）。
//
// 读写模型：
//   - 读取：data/config.json 叠在内置默认值之上（与启动时的 Load 同一套语义）；
//   - 保存：按 schema 逐字段合并进文件 → 反序列化 → normalize 校验 → 整份原子落盘。
//     校验失败时**一个字节都不写**，错误按字段路径返回给前端。
//   - 恢复默认：从文档里删掉对应键（回落默认值），再走同一套校验/落盘。
//
// 为什么写"整份配置"而不是增量：文件是给人看/改的，完整快照最直观；写入后
// 文件里就是当前生效值，重置某个键即回落默认。代价是未知键会在保存后消失
// （README 已注明）。
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"wp/internal/adminauth"
	"wp/internal/configschema"
	"wp/internal/server"
)

type configStore struct {
	path string
	mu   sync.Mutex
}

func newConfigStore(path string) *configStore { return &configStore{path: path} }

// readDoc 读文件为通用文档（容忍 BOM；null 视为未设置）。
func (s *configStore) readDoc() (map[string]any, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	raw = trimBOM(raw)
	clean, err := stripNulls(raw)
	if err != nil {
		return nil, fmt.Errorf("配置文件不是合法 JSON: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(clean, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func trimBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

// effective 把文档解析成 Config 并做完整校验（字段级 + normalize）。
func (s *configStore) effective(doc map[string]any) (*Config, []server.ConfigFieldError) {
	errs := make([]server.ConfigFieldError, 0, 4)
	for _, fe := range configschema.ValidateDoc(doc) {
		errs = append(errs, server.ConfigFieldError{Path: fe.Path, Message: fe.Message})
	}
	if len(errs) > 0 {
		// 类型/范围已经不对时不再反序列化：否则会多出一条没有 path 的
		// "json: cannot unmarshal ..." 噪音，前端反而看不出是哪一项错了。
		return nil, errs
	}

	cfg := Default()
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, append(errs, server.ConfigFieldError{Message: "配置无法序列化: " + err.Error()})
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, append(errs, server.ConfigFieldError{Message: "配置解析失败: " + err.Error()})
	}
	if err := cfg.normalize(); err != nil {
		errs = append(errs, server.ConfigFieldError{Path: fieldPathOf(err.Error()), Message: err.Error()})
	}
	return cfg, errs
}

// fieldPathOf 从 normalize 的错误文案里猜字段路径（"pool.breaker_cooldown: ..."）。
func fieldPathOf(msg string) string {
	if strings.Contains(msg, "api_key 为空") {
		return "admin.enabled"
	}
	if i := strings.Index(msg, ":"); i > 0 {
		prefix := strings.TrimSpace(msg[:i])
		if _, ok := configschema.Lookup(prefix); ok {
			return prefix
		}
	}
	return ""
}

// writeCfg 整份原子落盘（0600：文件里有 api_key 与密码哈希）。
func (s *configStore) writeCfg(cfg *Config) error {
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return writeFileAtomic(s.path, raw, 0o600)
}

// writable 目录是否可写（只用于前端提示，不参与校验）。
func (s *configStore) writable() bool {
	f, err := os.CreateTemp(filepath.Dir(s.path), ".wp-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

func (s *configStore) snapshot() (*server.ConfigSnapshot, error) {
	doc, err := s.readDoc()
	if err != nil {
		return nil, err
	}
	cfg, errs := s.effective(doc)
	if cfg == nil || len(errs) > 0 {
		// 配置文件坏到解析不了时，仍把默认值给前端，让用户能在页面上改回来。
		cfg = Default()
	}
	values := map[string]any{}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	// 管理密码哈希永远不下发给前端。
	if admin, ok := values["admin"].(map[string]any); ok {
		delete(admin, "password")
	}
	return &server.ConfigSnapshot{
		Values:            values,
		Defaults:          configschema.Defaults(),
		Schema:            configschema.Schema(),
		Path:              s.path,
		Writable:          s.writable(),
		MinPasswordLength: cfg.Admin.MinPasswordLength,
	}, nil
}

func (s *configStore) Snapshot() (*server.ConfigSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot()
}

func (s *configStore) Save(values map[string]any) (*server.ConfigSnapshot, []server.ConfigFieldError, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.readDoc()
	if err != nil {
		return nil, nil, err
	}
	// 只接受 schema 声明过的字段；未知键直接报错（避免拼错的键被静默忽略）。
	var errs []server.ConfigFieldError
	for _, f := range configschema.Fields() {
		v, ok := configschema.GetPath(values, f.Path)
		if !ok {
			continue
		}
		if v == nil {
			configschema.DeletePath(doc, f.Path)
			continue
		}
		configschema.SetPath(doc, f.Path, v)
	}
	for _, p := range unknownPaths(values) {
		errs = append(errs, server.ConfigFieldError{Path: p, Message: "未知配置项"})
	}
	if len(errs) > 0 {
		return nil, errs, nil
	}

	cfg, vErrs := s.effective(doc)
	if len(vErrs) > 0 || cfg == nil {
		return nil, vErrs, nil
	}
	if err := s.writeCfg(cfg); err != nil {
		return nil, nil, err
	}
	snap, err := s.snapshot()
	return snap, nil, err
}

// unknownPaths 找出 patch 里 schema 不认识的叶子路径。
func unknownPaths(values map[string]any) []string {
	var out []string
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			if child, ok := v.(map[string]any); ok {
				// 键值表（proxies 之类）整块是叶子，不再展开。
				if f, ok := configschema.Lookup(path); ok && f.Kind == configschema.KindStringMap {
					continue
				}
				walk(path, child)
				continue
			}
			if _, ok := configschema.Lookup(path); !ok {
				out = append(out, path)
			}
		}
	}
	walk("", values)
	sort.Strings(out)
	return out
}

func (s *configStore) Reset(req server.ConfigResetRequest) (*server.ConfigSnapshot, []server.ConfigFieldError, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.readDoc()
	if err != nil {
		return nil, nil, err
	}
	switch req.Scope {
	case "all":
		for _, f := range configschema.Fields() {
			if f.Path == "admin.enabled" {
				continue // 恢复默认不能把管理面关掉（会把自己锁在门外）
			}
			configschema.DeletePath(doc, f.Path)
		}
	case "section":
		if _, ok := groupExists(req.Section); !ok {
			return nil, []server.ConfigFieldError{{Path: req.Section, Message: "分组不存在"}}, nil
		}
		for _, f := range configschema.Fields() {
			if f.Group != req.Section || f.Path == "admin.enabled" {
				continue
			}
			configschema.DeletePath(doc, f.Path)
		}
	case "fields":
		for _, p := range req.Paths {
			if p == "admin.enabled" {
				continue
			}
			if _, ok := configschema.Lookup(p); !ok {
				return nil, []server.ConfigFieldError{{Path: p, Message: "未知配置项"}}, nil
			}
			configschema.DeletePath(doc, p)
		}
	}

	cfg, vErrs := s.effective(doc)
	if len(vErrs) > 0 || cfg == nil {
		return nil, vErrs, nil
	}
	if err := s.writeCfg(cfg); err != nil {
		return nil, nil, err
	}
	snap, err := s.snapshot()
	return snap, nil, err
}

func groupExists(key string) (configschema.Group, bool) {
	for _, g := range configschema.Schema().Groups {
		if g.Key == key {
			return g, true
		}
	}
	return configschema.Group{}, false
}

// SetPassword 把新的密码哈希写进配置；enable=true 时同时开启管理面。
func (s *configStore) SetPassword(hash string, enable bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.readDoc()
	if err != nil {
		return err
	}
	admin, ok := doc["admin"].(map[string]any)
	if !ok {
		admin = map[string]any{}
		doc["admin"] = admin
	}
	if _, err := adminauth.NewFromStored(hash); err != nil {
		return err
	}
	admin["password"] = hash
	if enable {
		admin["enabled"] = true
	}
	cfg, vErrs := s.effective(doc)
	if len(vErrs) > 0 {
		return fmt.Errorf("配置校验失败：%s", vErrs[0].Message)
	}
	return s.writeCfg(cfg)
}
