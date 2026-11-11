// Package adminauth 管理后台的凭据：**加盐 10 轮 SHA256 的哈希**，持久化在
// data/config.json 的 admin.password 字段里。
//
// 为什么不再"启动随机生成、只存内存、打印日志"：
//   - 随机密码意味着每次重启都要去翻日志，容器编排/远程运维都很别扭；
//   - 打印到日志等于"能看日志的人就能进后台"，日志一旦外发就是凭据泄漏；
//   - 现在改成"安装后由用户初始化一次"，凭据只以哈希形式落盘，重启/升级不失效。
//
// 存储格式（单行，便于放进 JSON 字符串）：
//
//	sha256x10$<salt-hex>$<digest-hex>
//
// 计算方式（salt 为 16 字节随机数）：
//
//	H0 = SHA256(salt || password)
//	Hi = SHA256(Hi-1)        i = 1..9      // 合计 10 次 SHA256
//
// 安全边界（必须清楚）：
//   - 盐防止"同一密码在不同实例得到同一哈希"，也让预计算表失效；
//   - 但 10 轮 SHA256 依然是**快哈希**：拿到配置文件的人可以离线爆破弱口令。
//     因此初始化/改密时强制最小长度（admin.min_password_length，默认 8），
//     并建议运维使用长口令。需要更强抗爆破时可后续升级 scheme 前缀（如 scrypt$…）。
//   - 配置里只存哈希：拷走 config.json 不能直接得到明文密码。
package adminauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

const (
	// scheme 前缀：写进存储值，方便将来换算法时识别旧哈希。
	scheme = "sha256x10"
	// saltLen 盐长度（字节）。
	saltLen = 16
	// rounds SHA256 迭代次数。
	rounds = 10
)

// minPlainLen 明文密码最小长度（config: admin.min_password_length）。
// 太短的密码在这套"快哈希"下几乎等于没设密码，宁可拒绝也不接受弱口令。
var minPlainLen = 8

// MinLength 当前要求的最小明文长度（供前端提示与测试读取）。
func MinLength() int { return minPlainLen }

// CheckPlain 校验明文密码是否满足最低要求。出错文案直接给用户看。
func CheckPlain(plain string) error {
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return errors.New("密码不能为空")
	}
	if len([]rune(plain)) < minPlainLen {
		return fmt.Errorf("密码至少 %d 个字符（当前 %d）", minPlainLen, len([]rune(plain)))
	}
	return nil
}

// Store 保存一份密码哈希。零值/nil = 未初始化，Verify 恒 false。
type Store struct {
	mu     sync.RWMutex
	stored string
}

// HashPassword 生成新的盐并计算存储值。每次调用盐都不同（同一密码得到不同哈希）。
func HashPassword(plain string) (string, error) {
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return "", errors.New("密码不能为空")
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("生成盐失败: %w", err)
	}
	digest := derive(salt, plain)
	return scheme + "$" + hex.EncodeToString(salt) + "$" + hex.EncodeToString(digest), nil
}

// derive 计算 10 轮 SHA256：H0 = SHA256(salt||plain)，随后对上一轮摘要再做 9 次。
func derive(salt []byte, plain string) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(plain))
	sum := h.Sum(nil)
	for i := 1; i < rounds; i++ {
		next := sha256.Sum256(sum)
		sum = next[:]
	}
	return sum
}

// parseStored 解析存储值；返回盐与摘要。格式不对 → 错误（调用方按"配置损坏"处理）。
func parseStored(stored string) (salt, digest []byte, err error) {
	stored = strings.TrimSpace(stored)
	parts := strings.Split(stored, "$")
	if len(parts) != 3 || parts[0] != scheme {
		return nil, nil, fmt.Errorf("密码哈希格式不认识（期望 %s$<salt>$<digest>）", scheme)
	}
	salt, err = hex.DecodeString(parts[1])
	if err != nil || len(salt) != saltLen {
		return nil, nil, errors.New("密码哈希里的盐损坏")
	}
	digest, err = hex.DecodeString(parts[2])
	if err != nil || len(digest) != sha256.Size {
		return nil, nil, errors.New("密码哈希里的摘要损坏")
	}
	return salt, digest, nil
}

// NewFromStored 从持久化的哈希构造 Store。
//
// stored 为空 → 返回未初始化的 Store（不是错误）：这正是"安装后还没设密码"的正常状态。
// 非空但格式损坏 → 返回错误：宁可启动失败并给出可操作提示，也不要静默当成"未初始化"
// 让人以为被重置了。
func NewFromStored(stored string) (*Store, error) {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return &Store{}, nil
	}
	if _, _, err := parseStored(stored); err != nil {
		return nil, err
	}
	return &Store{stored: stored}, nil
}

// MustNew 用明文构造一个已初始化 Store（测试与内部工具使用）。
func MustNew(plain string) *Store {
	enc, err := HashPassword(plain)
	if err != nil {
		panic(err)
	}
	s, err := NewFromStored(enc)
	if err != nil {
		panic(err)
	}
	return s
}

// Configured 是否已经初始化过（有哈希即算初始化）。
func (s *Store) Configured() bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return strings.TrimSpace(s.stored) != ""
}

// Stored 返回可落盘的哈希字符串（未初始化时为空串）。
func (s *Store) Stored() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stored
}

// SetStored 热更新哈希（改密/初始化后调用），无需重启。
func (s *Store) SetStored(stored string) error {
	if s == nil {
		return errors.New("adminauth: nil Store")
	}
	stored = strings.TrimSpace(stored)
	if stored != "" {
		if _, _, err := parseStored(stored); err != nil {
			return err
		}
	}
	s.mu.Lock()
	s.stored = stored
	s.mu.Unlock()
	return nil
}

// Verify 常量时间比对：把用户输入按同一盐重算 10 轮，与存储摘要比较。
// 未初始化时恒 false —— 这条是硬约束：绝不能出现"空密码即放行"。
func (s *Store) Verify(plain string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	stored := s.stored
	s.mu.RUnlock()
	if strings.TrimSpace(stored) == "" {
		return false
	}
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return false
	}
	salt, digest, err := parseStored(stored)
	if err != nil {
		return false
	}
	got := derive(salt, plain)
	return subtle.ConstantTimeCompare(got, digest) == 1
}
