package adminauth

import (
	"strings"
	"testing"
	"time"
)

// 默认构造必须给出可用凭据：非空、长度达标、来源标记为 generated。
func TestNewGeneratesPassword(t *testing.T) {
	s := MustNew("")
	if !s.Configured() {
		t.Fatal("随机生成后 Configured() 必须为 true")
	}
	pw := s.Password()
	if len([]rune(pw)) != passwordLen {
		t.Fatalf("密码长度 = %d，want %d", len([]rune(pw)), passwordLen)
	}
	for _, r := range pw {
		if !strings.ContainsRune(alphabet, r) {
			t.Fatalf("密码含字母表外字符 %q：%s", r, pw)
		}
	}
	if s.Source() != "generated" {
		t.Fatalf("Source() = %q，want generated", s.Source())
	}
	if time.Since(s.IssuedAt()) > time.Minute {
		t.Fatalf("IssuedAt 不合理：%v", s.IssuedAt())
	}
}

// 两次构造必须得到不同密码（否则"每次启动随机"就是假的）。
func TestNewIsRandom(t *testing.T) {
	a, b := MustNew(""), MustNew("")
	if a.Password() == b.Password() {
		t.Fatal("两次生成的密码相同，随机源有问题")
	}
}

// Verify：正确密码通过，错误/空/前后空格行为明确。
func TestVerify(t *testing.T) {
	s := MustNew("")
	pw := s.Password()

	if !s.Verify(pw) {
		t.Fatal("正确密码应通过")
	}
	if s.Verify("") {
		t.Fatal("空密码必须拒绝")
	}
	if s.Verify(pw + "x") {
		t.Fatal("错误密码必须拒绝")
	}
	if s.Verify(strings.ToUpper(pw)) && strings.ToUpper(pw) != pw {
		t.Fatal("大小写变化必须拒绝")
	}
}

// 固定密码：够长可用，太短直接报错（不让弱口令绕过"只认内存凭据"的设计）。
func TestOverridePassword(t *testing.T) {
	s, err := New("  dev-password-123  ")
	if err != nil {
		t.Fatalf("New(override) 报错：%v", err)
	}
	if s.Password() != "dev-password-123" {
		t.Fatalf("应去掉首尾空格，实际 %q", s.Password())
	}
	if s.Source() != "override" {
		t.Fatalf("Source() = %q，want override", s.Source())
	}

	if _, err := New("short"); err == nil {
		t.Fatal("短密码应报错")
	}
}

// 未初始化（nil / 空密码）时 Verify 恒 false —— 这是"绝不放行"的底座。
func TestVerifyRejectsUnconfigured(t *testing.T) {
	var nilStore *Store
	if nilStore.Verify("") || nilStore.Verify("anything") {
		t.Fatal("nil Store 必须拒绝一切")
	}
	if nilStore.Configured() {
		t.Fatal("nil Store 不应视为已配置")
	}
	empty := &Store{}
	if empty.Configured() || empty.Verify("") || empty.Verify("x") {
		t.Fatal("空 Store 必须拒绝一切")
	}
}

// 并发读（HTTP 处理并发调用 Verify/Password）不应触发竞态。
func TestConcurrentVerify(t *testing.T) {
	s := MustNew("")
	pw := s.Password()
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 200; j++ {
				_ = s.Verify(pw)
				_ = s.Password()
				_ = s.Configured()
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
