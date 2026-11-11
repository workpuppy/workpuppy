// disabled_test.go 账号「禁用」状态位（issue #138/#118，合并后模型）的语义测试。
//
// 覆盖的不变量：
//  1. 禁用后账号不被选号（含全冷却兜底/模型豁免旁路），但仍留在池里且状态可读；
//  2. 手动禁用与系统禁用共用**一个** disabled 位，只有 disabled_source 区分来源；
//     系统置位不覆盖已有的 manual 来源，运维置位会改写来源与原因；
//  3. 「启用」（ClearDisabled）是唯一解除入口，手动与系统禁用一并解除；
//     签到解冻（ReenableIfCredits）与 refresh 成功都不会解除禁用；
//  4. 禁用清冷却域（比冷却更强的不可用终态），熔断观测保留；
//  5. 禁用状态与来源持久化，重启保留；旧快照的 manual_disabled 迁移为 disabled+manual。
package pool

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"wp/internal/auth"
)

// TestDisabledStopsSelection 禁用后不参与选号（含全冷却兜底路径），
// 但仍在池里且状态可读。
func TestDisabledStopsSelection(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	if got := p.Pick(""); got == nil {
		t.Fatal("precondition: 禁用前应可选")
	}

	found, changed := p.SetDisabled("u1", true, "观察几天")
	if !found || !changed {
		t.Fatalf("SetDisabled = (%v,%v), want (true,true)", found, changed)
	}

	// 仍在池里：状态可读、计数把它算作 disabled
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("禁用后账号不应从池里消失")
	}
	if !st.Disabled {
		t.Fatalf("disabled 未置位: %+v", st)
	}
	if st.DisabledSource != string(DisabledByManual) {
		t.Errorf("disabled_source=%q want manual", st.DisabledSource)
	}
	if st.DisabledReason != "观察几天" {
		t.Errorf("disabled_reason=%q want %q", st.DisabledReason, "观察几天")
	}

	// 不参与正常选号
	if got := p.Pick(""); got != nil {
		t.Fatalf("禁用后不应被选中, got %+v", got)
	}
	// 也不参与全冷却兜底（pickEarliestExpiryLocked 路径）
	if got := p.pick(nil, "", ""); got != nil {
		t.Fatalf("禁用后不应参与兜底选号, got %+v", got)
	}

	// 计数口径：total 含它、healthy 不含、disabled 含
	total, healthy, _, disabled, _ := p.CountsDetailed()
	if total != 1 || healthy != 0 || disabled != 1 {
		t.Fatalf("计数 = total%d healthy%d disabled%d, want 1/0/1", total, healthy, disabled)
	}

	// 启用后立即可选
	if _, changed := p.SetDisabled("u1", false, ""); !changed {
		t.Fatal("启用应报告状态变化")
	}
	if got := p.Pick(""); got == nil || got.UID != "u1" {
		t.Fatalf("启用后应回到池子, got %+v", got)
	}
}

// TestDisabledIsIdempotent 重复调用幂等：第二次不报告变化、不报错。
func TestDisabledIsIdempotent(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	if _, changed := p.SetDisabled("u1", true, "r1"); !changed {
		t.Fatal("首次禁用应报告变化")
	}
	if found, changed := p.SetDisabled("u1", true, "r1"); !found || changed {
		t.Fatalf("重复禁用 = (%v,%v), want (true,false)", found, changed)
	}
	// 同状态但原因不同 → 仍算变化（文案要更新给运维看）
	if _, changed := p.SetDisabled("u1", true, "r2"); !changed {
		t.Fatal("原因变化应报告变化")
	}
	if _, reason, _, _ := p.DisabledState("u1"); reason != "r2" {
		t.Errorf("reason=%q want r2", reason)
	}
	// 未知 uid：found=false
	if found, _ := p.SetDisabled("nope", true, "x"); found {
		t.Error("未知 uid 应返回 found=false")
	}
	// 本就是启用态时再启用：changed=false
	if _, changed := p.SetDisabled("nope", false, ""); changed {
		t.Error("未知 uid 启用应 changed=false")
	}
}

// TestDisabledSourcePriority 来源优先级：系统置位不覆盖已有的 manual 来源与原因；
// 运维置位会改写系统来源。
func TestDisabledSourcePriority(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 先运维手动禁用，再被系统判定（连续 12153 达阈）
	p.SetDisabled("u1", true, "我主动禁的")
	for i := 0; i < 3; i++ {
		p.NoteSessionDead("u1")
	}
	disabled, reason, source, _ := p.DisabledState("u1")
	if !disabled {
		t.Fatal("仍应处于禁用态")
	}
	if source != DisabledByManual || reason != "我主动禁的" {
		t.Errorf("系统置位不应覆盖 manual 来源与原因: source=%q reason=%q", source, reason)
	}

	// 反过来：系统先禁，运维再禁 → 来源改写为 manual，原因更新
	p2 := New("")
	p2.Add(&auth.Auth{UID: "u1"})
	for i := 0; i < 3; i++ {
		p2.NoteSessionDead("u1")
	}
	if _, _, source, _ := p2.DisabledState("u1"); source != DisabledBySystem {
		t.Fatalf("precondition: 应为系统来源, got %q", source)
	}
	p2.SetDisabled("u1", true, "人工接管")
	if _, reason, source, _ := p2.DisabledState("u1"); source != DisabledByManual || reason != "人工接管" {
		t.Errorf("运维置位应改写来源与原因: source=%q reason=%q", source, reason)
	}
}

// TestClearDisabledReleasesEverything 「启用」是唯一解除入口：手动/系统禁用一并解除，
// 连续 12153 计数清零；签到解冻与 refresh 成功都不解除禁用。
func TestClearDisabledReleasesEverything(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	// 系统禁用
	for i := 0; i < 3; i++ {
		p.NoteSessionDead("u1")
	}
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("precondition: 应已系统禁用")
	}

	// 签到解冻 / refresh 成功都不解除禁用（没有自动复活路径）
	p.ReenableIfCredits("u1", 100)
	p.NoteRefreshSuccess("u1")
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("签到解冻/refresh 成功不应解除禁用")
	}
	if got := p.Pick(""); got != nil {
		t.Fatalf("仍应不可选, got %+v", got)
	}

	found, changed, wasDisabled, reason := p.ClearDisabled("u1")
	if !found || !changed || !wasDisabled {
		t.Fatalf("ClearDisabled = found%v changed%v wasDisabled%v", found, changed, wasDisabled)
	}
	if reason != "12153 session dead" {
		t.Errorf("应带回原禁用原因, got %q", reason)
	}
	st, _ := p.Status("u1")
	if st.Disabled || st.DisabledSource != "" || st.DisabledReason != "" {
		t.Errorf("启用后应清空禁用位/来源/原因: %+v", st)
	}
	if got := p.Pick(""); got == nil {
		t.Fatal("启用后应回池")
	}
	// 再启用一次：changed=false（幂等）
	if _, changed, _, _ := p.ClearDisabled("u1"); changed {
		t.Error("已是启用态时 ClearDisabled 应 changed=false")
	}
	// 未知 uid
	if found, _, _, _ := p.ClearDisabled("nope"); found {
		t.Error("未知 uid 应 found=false")
	}
}

// TestDisabledClearsCoolingDimensions 禁用清冷却域（比冷却更强的不可用终态），
// 但保留熔断观测（熔断是连续 5xx 信号，与授权/会话无关）。
func TestDisabledClearsCoolingDimensions(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.Cooldown("u1", CoolSoft, 10*time.Minute, "429 rate limit")
	p.NoteError("u1")
	before, _ := p.Status("u1")

	p.SetDisabled("u1", true, "临时禁用")
	after, _ := p.Status("u1")

	if !after.Until.IsZero() {
		t.Errorf("禁用应清冷却截止: %v", after.Until)
	}
	if after.Cooling {
		t.Error("禁用后不应仍处于冷却态")
	}
	if after.BreakerFails != before.BreakerFails {
		t.Errorf("禁用不应改熔断计数: %d → %d", before.BreakerFails, after.BreakerFails)
	}
}

// TestDisabledPersists 禁用状态与来源落盘持久化——重启保留运维意图。
func TestDisabledPersists(t *testing.T) {
	dir := t.TempDir()
	state := dir + "/state.json"

	p := New(state)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetDisabled("u1", true, "重启也要保留")
	p.Flush()

	// 模拟重启：新池读同一 state 文件
	p2 := New(state)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.Add(&auth.Auth{UID: "u2"})

	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("no status after reload")
	}
	if !st.Disabled {
		t.Fatal("重启后禁用状态丢失")
	}
	if st.DisabledSource != string(DisabledByManual) {
		t.Errorf("disabled_source=%q 未保留", st.DisabledSource)
	}
	if st.DisabledReason != "重启也要保留" {
		t.Errorf("disabled_reason=%q 未保留", st.DisabledReason)
	}
	// 未禁用的账号不受影响
	if st2, _ := p2.Status("u2"); st2.Disabled {
		t.Error("u2 不应被连带禁用")
	}
	// 重启后仍不可选
	if got := p2.Pick(""); got == nil || got.UID != "u2" {
		t.Fatalf("重启后应只能选到 u2, got %+v", got)
	}
	// 启用后落盘，再重启确认已清
	p2.ClearDisabled("u1")
	p2.Flush()
	p3 := New(state)
	p3.Add(&auth.Auth{UID: "u1"})
	if st3, _ := p3.Status("u1"); st3.Disabled {
		t.Fatal("清除后重启不应复活禁用位")
	}
}

// TestLegacyManualDisabledMigrates 旧快照迁移：manual_disabled=true 的号升级后
// 必须仍是禁用态（来源 manual），否则一次重启就把运维明确摘掉的号放回选号池。
func TestLegacyManualDisabledMigrates(t *testing.T) {
	dir := t.TempDir()
	state := dir + "/state.json"

	legacy := map[string]any{"accounts": map[string]any{
		"u1": map[string]any{"credits": 10, "manual_disabled": true, "manual_reason": "旧快照"},
		"u2": map[string]any{"credits": 20},
	}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	p := New(state)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 状态不可读")
	}
	if !st.Disabled {
		t.Fatal("旧 manual_disabled=true 应迁移为 disabled=true")
	}
	if st.DisabledSource != string(DisabledByManual) {
		t.Errorf("迁移后来源应为 manual, got %q", st.DisabledSource)
	}
	if st.DisabledReason != "旧快照" {
		t.Errorf("旧的 manual_reason 应迁移为 disabled_reason, got %q", st.DisabledReason)
	}
	if got := p.Pick(""); got == nil || got.UID != "u2" {
		t.Fatalf("迁移后 u1 不应可选, got %+v", got)
	}
	if st2, _ := p.Status("u2"); st2.Disabled || st2.DisabledSource != "" {
		t.Errorf("u2 不应被连带：%+v", st2)
	}

	// 再落盘时应写成新字段，不再输出 manual_disabled
	p.Flush()
	saved, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var got stateFile
	if err := json.Unmarshal(saved, &got); err != nil {
		t.Fatal(err)
	}
	if got.Accounts["u1"].LegacyManualDisabled {
		t.Error("落盘不应再输出 manual_disabled")
	}
	if !got.Accounts["u1"].Disabled || got.Accounts["u1"].DisabledSource != string(DisabledByManual) {
		t.Errorf("落盘应写 disabled + disabled_source, got %+v", got.Accounts["u1"])
	}
}

// TestDisabledRealmScoped 禁用某个域的账号不影响另一域。
func TestDisabledRealmScoped(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "gl1", Domain: "www.workbuddy.ai"})

	p.SetDisabled("cn1", true, "只禁 CN")

	if got := p.pick(nil, "", "cn"); got != nil {
		t.Fatalf("CN 域应无可选账号, got %+v", got)
	}
	if got := p.pick(nil, "", "global"); got == nil || got.UID != "gl1" {
		t.Fatalf("Global 域应不受影响, got %+v", got)
	}
}

// TestDisabledServable 禁用对探活谓词的行为锚（审查改造点 4）：
// modelExempt 是三旁路谓词中唯一无测试的一条（healthy/pickEarliestExpiryLocked
// 已有锚）。锁死两个形态：
//  1. 唯一号有 6004 模型级冷却（modelExempt 形态，本应计入 ServableNow）→
//     禁用后 ServableNow/ServableForRealm 必须 false；
//  2. 对应 realm 的 ServableForRealm 同样 false（healthy 旁路经 modelExempt 之外的路径）。
func TestDisabledServable(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 6004 模型级冷却：u1 是「未禁用、未熔断、存在模型冷却条目」的豁免形态，
	// 无禁用时 ServableNow 因 modelExempt 为 true。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	if !p.ServableNow() {
		t.Fatal("precondition: 模型豁免形态应 ServableNow=true")
	}

	p.SetDisabled("u1", true, "摘除")
	if p.ServableNow() {
		t.Error("唯一号禁用后 ServableNow 应 false（modelExempt 排除禁用号）")
	}
	if p.ServableForRealm("cn") {
		t.Error("唯一号禁用后 ServableForRealm(cn) 应 false")
	}

	// 启用后探活回归
	p.SetDisabled("u1", false, "")
	if !p.ServableNow() {
		t.Error("解除禁用后 ServableNow 应回归 true")
	}
}
