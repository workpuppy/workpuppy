// Package adminauth 管理后台的凭据：**每次进程启动随机生成，只存在内存里**。
//
// 为什么这样设计（与 /v1 的 api_key 彻底分离）：
//   - `/v1` 的 api_key 是发给下游客户端的，天然会被很多人知道；它**不应该**同时
//     拥有"停号 / 加号 / 看全量账号"的能力。管理侧因此单独一套凭据。
//   - 管理凭据不落盘：写进 config.json 等于让人拷走配置文件即可接管后台；
//     而"启动时随机 + 打印到终端"意味着**只有能看到容器日志的人**才拿得到，
//     且每次重启自动轮换（旧密码立即失效）。
//   - 没有用户名：单人运维场景里用户名只是个多余的字段；密码本身就是唯一凭据。
//
// 边界（必须清楚）：
//   - 密码在**容器日志里可见** —— 能 `docker logs` 的人本来就能控制这个容器，
//     这不算提权；但它也确实意味着"日志权限 = 管理权限"，注意别把日志外发。
//   - 固定密码仅用于开发/自动化（WB2A_ADMIN_PASSWORD），生产建议用随机值。
package adminauth

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"strings"
	"sync"
	"time"
)

// passwordLen 生成密码的长度。20 个字符、58 个字符的字母表 ≈ 117 bit 熵：
// 在线爆破不可能成功，也不至于长到没法手输/复制。
var passwordLen = 20

// alphabet 去掉了容易看错的字符（0/O/1/l/I），便于从日志里抄进浏览器。
const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// minOverrideLen 外部固定密码（env/config）的最短长度。太短的密码会让"只认
// 内存凭据"这套设计失去意义，所以宁可启动即报错也不接受弱口令。
var minOverrideLen = 8

// Store 内存中的管理密码。并发安全（HTTP 处理与启动日志都会读）。
type Store struct {
	mu       sync.RWMutex
	password string
	source   string
	issuedAt time.Time
}

// New 构造 Store：override 非空则用它（长度不足报错），否则随机生成。
func New(override string) (*Store, error) {
	override = strings.TrimSpace(override)
	if override != "" {
		if len([]rune(override)) < minOverrideLen {
			return nil, fmt.Errorf("管理密码至少 %d 个字符（当前 %d）", minOverrideLen, len([]rune(override)))
		}
		return &Store{password: override, source: "override", issuedAt: time.Now()}, nil
	}
	pw, err := generate(passwordLen)
	if err != nil {
		return nil, fmt.Errorf("生成管理密码失败: %w", err)
	}
	return &Store{password: pw, source: "generated", issuedAt: time.Now()}, nil
}

// MustNew 供启动路径使用：密码生成失败意味着没有可用的管理凭据，直接 fatal 比
// "起得来但后台进不去"更好排查。
func MustNew(override string) *Store {
	s, err := New(override)
	if err != nil {
		panic(err)
	}
	return s
}

// Password 返回当前密码（供启动横幅打印与测试读取）。
func (s *Store) Password() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.password
}

// Configured 是否已经有一份可用凭据。false 时管理面必须**拒绝**而不是放行。
func (s *Store) Configured() bool {
	return s != nil && strings.TrimSpace(s.Password()) != ""
}

// Source 凭据来源：generated（随机生成）/ override（外部固定）。
func (s *Store) Source() string {
	if s == nil {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.source
}

// IssuedAt 密码签发时刻（重启即变，日志里能对上时间）。
func (s *Store) IssuedAt() time.Time {
	if s == nil {
		return time.Time{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.issuedAt
}

// Verify 常量时间比对。**未配置时恒 false** —— 这条是硬约束：
// 以前 /v1 的语义是"api_key 为空 = 不鉴权"，管理侧绝不能沿用，
// 否则一个漏配就让整个后台裸奔在公网上。
func (s *Store) Verify(provided string) bool {
	if !s.Configured() {
		return false
	}
	provided = strings.TrimSpace(provided)
	if provided == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(s.Password())) == 1
}

// generate 用 crypto/rand 生成 n 个字符的密码（拒绝采样，避免取模偏差）。
func generate(n int) (string, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, 32)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			// 只取能被字母表长度整除的区间：[0, 232) —— 58*4=232，其余丢弃。
			if int(b) >= 232 {
				continue
			}
			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}
