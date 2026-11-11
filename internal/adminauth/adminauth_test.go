package adminauth

import (
	"strings"
	"sync"
	"testing"
)

func TestHashPasswordFormatAndVerify(t *testing.T) {
	enc, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(enc, "sha256x10$") {
		t.Fatalf("存储值应以 scheme 开头：%q", enc)
	}
	if strings.Count(enc, "$") != 2 {
		t.Fatalf("存储值应为三段：%q", enc)
	}
	s, err := NewFromStored(enc)
	if err != nil {
		t.Fatalf("NewFromStored: %v", err)
	}
	if !s.Configured() {
		t.Fatal("解析出哈希后 Configured 应为 true")
	}
	if !s.Verify("correct horse battery") {
		t.Fatal("正确密码应通过")
	}
	if !s.Verify("  correct horse battery  ") {
		t.Fatal("首尾空格应被裁剪后通过")
	}
	if s.Verify("wrong") {
		t.Fatal("错误密码必须拒绝")
	}
	if s.Verify("") {
		t.Fatal("空密码必须拒绝")
	}
}

// 每次哈希都换新盐：同一明文必须得到不同存储值，但都能验过。
func TestHashPasswordUsesFreshSalt(t *testing.T) {
	a, err := HashPassword("same-password")
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword("same-password")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("两次哈希相同：盐没有随机化")
	}
	sa, _ := NewFromStored(a)
	sb, _ := NewFromStored(b)
	if !sa.Verify("same-password") || !sb.Verify("same-password") {
		t.Fatal("两份哈希都应能验过原始密码")
	}
}

// 未初始化（空存储 / nil）时 Verify 恒 false —— "绝不放行"的底座。
func TestUnconfiguredNeverVerifies(t *testing.T) {
	var nilStore *Store
	if nilStore.Configured() || nilStore.Verify("") || nilStore.Verify("x") {
		t.Fatal("nil Store 必须拒绝一切")
	}
	s, err := NewFromStored("")
	if err != nil {
		t.Fatalf("空存储不应报错：%v", err)
	}
	if s.Configured() || s.Verify("") || s.Verify("x") {
		t.Fatal("未初始化 Store 必须拒绝一切")
	}
}

// 存储值损坏要明确报错，而不是静默当成未初始化。
func TestMalformedStoredRejected(t *testing.T) {
	for _, bad := range []string{
		"plain-text",
		"sha256x10$nothex$deadbeef",
		"sha256x10$00112233445566778899aabbccddeeff$short",
		"md5$00112233445566778899aabbccddeeff$" + strings.Repeat("ab", 32),
	} {
		if _, err := NewFromStored(bad); err == nil {
			t.Fatalf("应拒绝损坏的存储值：%q", bad)
		}
	}
}

// SetStored 支持热换密码（改密无需重启）。
func TestSetStoredHotSwap(t *testing.T) {
	s := MustNew("old-password")
	if !s.Verify("old-password") {
		t.Fatal("旧密码应通过")
	}
	enc, err := HashPassword("new-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetStored(enc); err != nil {
		t.Fatalf("SetStored: %v", err)
	}
	if s.Verify("old-password") {
		t.Fatal("换密码后旧密码必须失效")
	}
	if !s.Verify("new-password") {
		t.Fatal("新密码应通过")
	}
	if err := s.SetStored("garbage"); err == nil {
		t.Fatal("写入损坏哈希应报错")
	}
}

// 最小长度约束在 CheckPlain 里统一实现（初始化/改密共用）。
func TestCheckPlainMinLength(t *testing.T) {
	Configure(Options{MinLength: 8})
	if err := CheckPlain("short"); err == nil {
		t.Fatal("短密码应报错")
	}
	if err := CheckPlain("  long-enough  "); err != nil {
		t.Fatalf("够长密码应通过：%v", err)
	}
	if err := CheckPlain("   "); err == nil {
		t.Fatal("纯空白应报错")
	}
	if MinLength() != 8 {
		t.Fatalf("MinLength() = %d", MinLength())
	}
	Configure(Options{MinLength: 12})
	if err := CheckPlain("elevenchars"); err == nil {
		t.Fatal("提高下限后 11 位应报错")
	}
	Configure(Options{MinLength: 8}) // 复位，避免影响其它用例
}

// 并发读写不应触发竞态（HTTP 并发请求下 Verify 与改密可能同时发生）。
func TestConcurrentVerifyAndSwap(t *testing.T) {
	s := MustNew("concurrent-password")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = s.Verify("concurrent-password")
				_ = s.Configured()
				_ = s.Stored()
			}
		}()
	}
	wg.Wait()
}
