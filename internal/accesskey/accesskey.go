// Package accesskey 管理下游接入 key：每个 key 独立授权「可调用的账号」与
// 「可用的模型」。
//
// 为什么单独成包、并且不放进 config.json：接入 key 是**运行期运维数据**（在管理
// 后台随时增删改），不是启动配置——写进 config.json 就意味着改一次要重启容器，
// 也没法给每把 key 存自己的账号/模型白名单。落盘文件与 state.json 同目录
// （data/access-keys.json），权限 0600：文件里是**明文**密钥，与旧 config.api_key
// 同口径，方便运维随时复制；拖库风险由宿主机权限与 admin 面鉴权承担。
//
// 与旧 api_key 的语义差异：旧 key 只有「能用 / 不能用」两态；这里的每把 key 自带
// 两个白名单，选号、会话粘性、/v1/models、/status 都按它过滤（见 pool.Scope 与
// server.withAccessKey）。**空列表 = 该维度全部拒绝**（显式授权，避免漏配把整池
// 放出去）；要放开某维度必须显式勾选「全部账号 / 全部模型」。
package accesskey

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// FileName 接入 key 的落盘文件名。cmd/server 与本地运维 CLI 共用这一处，
// 避免各自拼字面量（改名时只改这里）。
const FileName = "access-keys.json"

const (
	fileVersion = 1
	maxNameLen  = 64
	maxListLen  = 1000
	maxUIDLen   = 80
	maxModelLen = 200
)

// secretPattern 允许的 key 文本形态（自动生成的 wp-xxxx 与运维自定义值都满足）。
// 只允许 URL/HTTP 头安全的字符，避免复制粘贴时被空格、换行或引号破坏。
var secretPattern = regexp.MustCompile(`^[A-Za-z0-9._\-]{8,128}$`)

// ErrNotFound 目标 key 不存在（管理面映射为 404）。
var ErrNotFound = errors.New("access key not found")

// ErrConflict 密钥文本已存在（管理面映射为 409）。
var ErrConflict = errors.New("access key secret already exists")

// ValidationError 输入不合法（管理面映射为 400，文案可直接给用户看）。
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &ValidationError{Msg: fmt.Sprintf(format, args...)}
}

// Key 一把下游接入 key 及其授权范围。
type Key struct {
	// ID 稳定内部标识（生成后不变），用作管理面 URL 段与会话粘性的命名空间。
	ID string `json:"id"`
	// Name 备注名，只用于管理后台展示。
	Name string `json:"name"`
	// Key 明文密钥（下游用 Authorization: Bearer <Key> 调用）。
	Key string `json:"key"`
	// Enabled false = 立即失效（保留配置，便于临时停用而不是删除）。
	Enabled bool `json:"enabled"`

	// AllAccounts true = 允许所有账号（含以后新加入的号）；false 时只看 Accounts。
	// 与「空列表 = 全部拒绝」配合：两个都为 false/空 → 该 key 一把账号都用不了。
	AllAccounts bool `json:"all_accounts"`
	// AllModels true = 允许所有模型（含上游以后新增的）；false 时只看 Models。
	AllModels bool `json:"all_models"`

	// Accounts 允许调用的账号 UID 白名单（AllAccounts=false 时生效）。
	Accounts []string `json:"accounts"`
	// Models 允许调用的模型 ID 白名单（与 /v1/models 的 id 同形，含 cn:/global:
	// 前缀）。匹配规则见 AllowsModel：裸名与 cn: 等价，global: 必须单独勾选。
	Models []string `json:"models"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AllowsAccount 报告账号是否在该 key 的授权范围内。
func (k Key) AllowsAccount(uid string) bool {
	if k.AllAccounts {
		return true
	}
	for _, u := range k.Accounts {
		if u == uid {
			return true
		}
	}
	return false
}

// AllowsModel 报告请求里的 model 是否在该 key 的授权范围内。
//
// 匹配口径（与产品约定一致）：
//   - 白名单项按 /v1/models 的完整 id 存储（cn:xxx / global:xxx）；
//   - 请求里的裸名等价于 cn: 前缀（老客户端不带前缀，不能因此被拒）；
//   - 跨域仍需分别勾选：勾了 cn:xxx 不代表允许 global:xxx。
func (k Key) AllowsModel(model string) bool {
	if k.AllModels {
		return true
	}
	realm, bare := SplitModel(model)
	if bare == "" {
		return false
	}
	for _, m := range k.Models {
		mr, mb := SplitModel(m)
		if mr == realm && mb == bare {
			return true
		}
	}
	return false
}

// SplitModel 解析网关模型名协议："[realm:]model"。
//
// 取第一个 ":"，前段恰为 "cn"/"global"（大小写敏感）才剥离；否则视为裸名，
// realm=cn、bare=原串。与 internal/server.resolveModel 同口径——后者已改为直接
// 调用本函数，保证两处不会各写一份而漂移。
func SplitModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", model
	}
	return prefix, model[idx+1:]
}

// Store 接入 key 的并发安全存储（内存 + 原子落盘）。
//
// path 为空 = 仅内存（测试与纯 API 部署）；非空时每次写操作都整份原子重写文件，
// 与项目里其它运行期数据（state.json / credit-daily.json）保持一致的落盘风格。
type Store struct {
	mu   sync.RWMutex
	path string
	keys map[string]Key // id → Key
}

// New 构造存储；path 为空表示不落盘。构造后需调用 Load 读取已有文件。
func New(path string) *Store {
	return &Store{path: path, keys: map[string]Key{}}
}

// Path 返回落盘路径（观测/排障用；空 = 内存态）。
func (s *Store) Path() string { return s.path }

// Load 读取落盘文件。文件不存在视为空存储（首次启动的正常形态）；解析失败返回
// 错误，由调用方决定是否阻断启动——密钥文件损坏时**不应静默当成空**，否则所有
// 下游会突然 401，排障却没有线索。
func (s *Store) Load() error {
	if s.path == "" {
		return nil
	}
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var doc fileDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("解析 %s: %w", s.path, err)
	}
	next := make(map[string]Key, len(doc.Keys))
	for _, k := range doc.Keys {
		k = normalizeStored(k)
		if k.ID == "" || k.Key == "" {
			log.Printf("WARN: [accesskey] 忽略 %s 里缺少 id/key 的条目（name=%q）", s.path, k.Name)
			continue
		}
		next[k.ID] = k
	}
	s.mu.Lock()
	s.keys = next
	s.mu.Unlock()
	return nil
}

// fileDoc 落盘结构。带 version 是为了将来扩展（迁移时能识别旧格式）；
// 读出时不校验版本——未知版本按同构处理，字段缺省即零值。
type fileDoc struct {
	Version int   `json:"version"`
	Keys    []Key `json:"keys"`
}

// List 返回全部 key（按创建时间、ID 稳定排序），含明文密钥（仅管理面使用）。
func (s *Store) List() []Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Key, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, k)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Count 返回 key 数量（启动日志用）。
func (s *Store) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keys)
}

// Get 按 ID 取 key。
func (s *Store) Get(id string) (Key, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[id]
	return k, ok
}

// Lookup 按明文密钥查找（数据面鉴权入口）。
//
// 用常量时间比较：与旧 api_key 的鉴权同口径，避免公网暴露下按响应时间逐字节
// 探测密钥前缀。遍历全部条目（不提前 return），使命中位置也不产生时间差。
func (s *Store) Lookup(secret string) (Key, bool) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return Key{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var found Key
	hit := 0
	for _, k := range s.keys {
		if subtle.ConstantTimeCompare([]byte(k.Key), []byte(secret)) == 1 {
			found = k
			hit = 1
		}
	}
	return found, hit == 1
}

// Input 创建/更新 key 的入参（管理面请求体同构）。
//
// Secret 仅创建时使用：空 = 自动生成；非空 = 运维自定义（测试与迁移用）。
// 更新时忽略 Secret（换密钥请走 Regenerate，避免误改）。
type Input struct {
	Name        string   `json:"name"`
	Secret      string   `json:"secret,omitempty"`
	Enabled     *bool    `json:"enabled,omitempty"`
	AllAccounts bool     `json:"all_accounts"`
	AllModels   bool     `json:"all_models"`
	Accounts    []string `json:"accounts"`
	Models      []string `json:"models"`
}

// Create 新建一把 key（默认启用）。
func (s *Store) Create(in Input) (Key, error) {
	name, accounts, models, err := validateInput(in)
	if err != nil {
		return Key{}, err
	}
	secret := strings.TrimSpace(in.Secret)
	if secret == "" {
		if secret, err = generateSecret(); err != nil {
			return Key{}, err
		}
	} else if !secretPattern.MatchString(secret) {
		return Key{}, invalid("key 只能包含字母、数字、点、下划线与连字符，长度 8–128")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secretTakenLocked(secret) {
		return Key{}, ErrConflict
	}
	id, err := s.newIDLocked()
	if err != nil {
		return Key{}, err
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	now := time.Now().UTC()
	k := Key{
		ID:          id,
		Name:        name,
		Key:         secret,
		Enabled:     enabled,
		AllAccounts: in.AllAccounts,
		AllModels:   in.AllModels,
		Accounts:    accounts,
		Models:      models,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	s.keys[id] = k
	if err := s.saveLocked(); err != nil {
		delete(s.keys, id) // 落盘失败不让内存先行（否则重启后"消失"）
		return Key{}, err
	}
	return k, nil
}

// Update 更新名称/启用态/授权范围（ID、密钥、创建时间不变）。
func (s *Store) Update(id string, in Input) (Key, error) {
	name, accounts, models, err := validateInput(in)
	if err != nil {
		return Key{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.keys[id]
	if !ok {
		return Key{}, ErrNotFound
	}
	prev := cur
	cur.Name = name
	cur.AllAccounts = in.AllAccounts
	cur.AllModels = in.AllModels
	cur.Accounts = accounts
	cur.Models = models
	if in.Enabled != nil {
		cur.Enabled = *in.Enabled
	}
	cur.UpdatedAt = time.Now().UTC()
	s.keys[id] = cur
	if err := s.saveLocked(); err != nil {
		s.keys[id] = prev
		return Key{}, err
	}
	return cur, nil
}

// SetEnabled 只切换启用态（管理面「停用/启用」按钮）。
func (s *Store) SetEnabled(id string, enabled bool) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.keys[id]
	if !ok {
		return Key{}, ErrNotFound
	}
	prev := cur
	cur.Enabled = enabled
	cur.UpdatedAt = time.Now().UTC()
	s.keys[id] = cur
	if err := s.saveLocked(); err != nil {
		s.keys[id] = prev
		return Key{}, err
	}
	return cur, nil
}

// Regenerate 换一把新密钥（其它字段不动），用于密钥泄露后的轮换。
func (s *Store) Regenerate(id string) (Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.keys[id]
	if !ok {
		return Key{}, ErrNotFound
	}
	for i := 0; i < 5; i++ {
		secret, err := generateSecret()
		if err != nil {
			return Key{}, err
		}
		if s.secretTakenLocked(secret) {
			continue
		}
		prev := cur
		cur.Key = secret
		cur.UpdatedAt = time.Now().UTC()
		s.keys[id] = cur
		if err := s.saveLocked(); err != nil {
			s.keys[id] = prev
			return Key{}, err
		}
		return cur, nil
	}
	return Key{}, fmt.Errorf("生成唯一密钥失败（重试 5 次）")
}

// Delete 删除 key（立即失效）。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.keys[id]
	if !ok {
		return ErrNotFound
	}
	delete(s.keys, id)
	if err := s.saveLocked(); err != nil {
		s.keys[id] = cur
		return err
	}
	return nil
}

// saveLocked 整份原子落盘（同目录 tmp + fsync + rename），调用方必须已持写锁。
// 0600：文件里是明文密钥；目录不存在时补建（首次在空 data 目录里创建 key）。
func (s *Store) saveLocked() error {
	if s.path == "" {
		return nil
	}
	doc := fileDoc{Version: fileVersion, Keys: s.sortedLocked()}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(s.path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".wp-access-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
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
	return os.Rename(tmpName, s.path)
}

// sortedLocked 与 List 同序，供落盘输出稳定（diff 友好）。调用方必须已持锁。
func (s *Store) sortedLocked() []Key {
	out := make([]Key, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, k)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *Store) secretTakenLocked(secret string) bool {
	for _, k := range s.keys {
		if k.Key == secret {
			return true
		}
	}
	return false
}

func (s *Store) newIDLocked() (string, error) {
	for i := 0; i < 5; i++ {
		buf := make([]byte, 8)
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		id := "ak_" + hex.EncodeToString(buf)
		if _, exists := s.keys[id]; !exists {
			return id, nil
		}
	}
	return "", fmt.Errorf("生成唯一 key id 失败（重试 5 次）")
}

// generateSecret 生成 wp-<48 hex> 形态的密钥（24 字节熵，约 192 bit）。
func generateSecret() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "wp-" + hex.EncodeToString(buf), nil
}

// validateInput 归一化并校验授权范围。空列表 + 未勾「全部」= 显式拒绝，
// 直接报错而不是静默放行——这正是「漏配等于全权」的防线。
func validateInput(in Input) (name string, accounts, models []string, err error) {
	name = strings.TrimSpace(in.Name)
	if name == "" {
		name = "未命名接入"
	}
	if utf8.RuneCountInString(name) > maxNameLen {
		return "", nil, nil, invalid("名称最长 %d 个字符", maxNameLen)
	}
	accounts = cleanList(in.Accounts)
	models = cleanList(in.Models)
	if len(accounts) > maxListLen {
		return "", nil, nil, invalid("账号最多 %d 个", maxListLen)
	}
	if len(models) > maxListLen {
		return "", nil, nil, invalid("模型最多 %d 个", maxListLen)
	}
	for _, uid := range accounts {
		if len(uid) > maxUIDLen {
			return "", nil, nil, invalid("账号 UID 过长（%d > %d）：%s", len(uid), maxUIDLen, uid)
		}
	}
	for _, m := range models {
		if utf8.RuneCountInString(m) > maxModelLen {
			return "", nil, nil, invalid("模型 ID 过长（%d > %d）：%s", utf8.RuneCountInString(m), maxModelLen, m)
		}
	}
	if !in.AllAccounts && len(accounts) == 0 {
		return "", nil, nil, invalid("至少选择一个账号（或勾选「全部账号」）")
	}
	if !in.AllModels && len(models) == 0 {
		return "", nil, nil, invalid("至少选择一个模型（或勾选「全部模型」）")
	}
	return name, accounts, models, nil
}

// cleanList 去空白、去空项、去重，保持原顺序（界面勾选顺序即落盘顺序）。
func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, raw := range in {
		v := strings.TrimSpace(raw)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// normalizeStored 读取期清洗：只做无副作用的归一（不因坏条目丢整份配置）。
// 空列表保留原样（该维度按「全部拒绝」处理），由管理界面引导用户补勾。
func normalizeStored(k Key) Key {
	k.Name = strings.TrimSpace(k.Name)
	k.Key = strings.TrimSpace(k.Key)
	k.Accounts = cleanList(k.Accounts)
	k.Models = cleanList(k.Models)
	return k
}
