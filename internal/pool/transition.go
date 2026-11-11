// 账号状态机迁移的唯一权威实现。
//
// entry 的「可选择性」由五个正交维度决定：禁用(disabled)、休养(resting)、账号级冷却(until/coolKind)、
// 模型级冷却(modelCooldowns)、熔断(breakerUntil)。
// 维度之间以「迁移原语」收拢，禁止在其他文件散写这些字段——所有入口（applyErrorPolicy /
// refresh / keepalive / 签到 / 选号 / 运维端点）对状态的改动都必须经本文件的原语或经
// Cooldown/NoteError/NoteSuccess 等封装（它们在持锁下调用本文件原语）。
//
// 迁移矩阵（事件 → 动作 → 字段）：
//
//	disabled           ← setDisabledLocked（禁用：运维端点/CLI 传 manual，Disable/NoteAuthFailure 达阈传 system）
//	resting            ← SetResting（运维「休养」；不碰 disabled/冷却/熔断）
//	until/coolKind     ← Cooldown(CoolSoft/Hard，固定时长) / CooldownSoftRate / CooldownSoftForModel 无解析分支
//	modelCooldowns     ← CooldownSoftForModel 有解析分支；被 setDisabledLocked/Cooldown/clearCoolingLocked 清
//	breakerUntil       ← recordBreakerFailureLocked（NoteError 喂入）；NoteSuccess 清
//	softStreak         ← CooldownSoftRate / CooldownSoftForModel 无解析分支；NoteSuccess/reviveCoolingLocked 清
//	sessionDeadFails   ← NoteSessionDead；ClearSessionDead/NoteSuccess/setDisabledLocked(启用) 清
//
// 关键正交性（疑点 4 修正）：
//   - 冷却域（until/coolKind/softStreak/modelCooldowns）与熔断器（fails/retryCount/
//     breakerUntil）正交：冷却管「近期被限流/余额耗尽」，熔断管「反复 5xx 失败」。
//     setDisabledLocked 只清冷却域、不动熔断——禁用是授权/session 终态，不应覆盖熔断观测。
//   - clearCoolingLocked 是「冷却域归零」的单一来源，被 setDisabledLocked 与
//     reviveCoolingLocked（签到解冻）共用，二者对冷却域的处置因此永远一致。
//   - disabled 只有**一个**状态位：手动禁用与系统禁用共用它（disabledSource 仅记录来源用于
//     展示），因此「禁用」对下游与自动任务的效果完全一致；只有「启用」能解除，不存在
//     自动复活路径。休养(resting) 是另一维度：只摘流量、任务照跑，见 SetResting。
package pool

import "time"

// clearCoolingLocked 清冷却域：until/coolKind/softStreak/modelCooldowns 全归零，
// reason 一并清空。熔断器（fails/retryCount/breakerUntil）不属冷却域，不动。
// 调用方必须已持有 p.mu。
func (e *entry) clearCoolingLocked() {
	e.until = time.Time{}
	e.coolKind = 0
	e.reason = ""
	e.softStreak = 0
	e.modelCooldowns = nil // 冷却域清零时一并清模型级独立冷却（模型豁免随之消失）
}

// DisabledSource 禁用来源：只用于展示与定责，**不参与任何判定**。
// 两种来源的 disabled 效果完全一致（不接下游流量 + 全部自动任务不跑），
// 且都只能由运维「启用」解除。
type DisabledSource string

const (
	DisabledByManual DisabledSource = "manual" // 运维「禁用」（管理端点 / CLI）
	DisabledBySystem DisabledSource = "system" // 系统判定：连续 12153 / 凭证被拒达阈
)

// disableLocked 系统判定禁用：置 disabled + source=system，并清冷却域。
// 系统路径（Disable / NoteSessionDead 达阈）统一走这里。
func (p *Pool) disableLocked(e *entry, reason string) {
	p.setDisabledLocked(e, true, reason, DisabledBySystem)
}

// setDisabledLocked 禁用/启用的唯一迁移原语。
//
// 禁用（on=true）：置 disabled + source + reason，并清冷却域——禁用是比冷却更强的
// 不可用终态（账号已退出选号，冷却截止不再被读取）。若此前已是运维手动禁用
// （source=manual），系统判定置位**不降级**来源与原因：运维意图优先，系统只保证
// 「位是 true」，不会把「我为什么禁它」覆盖成系统文案。
//
// 启用（on=false）：清 disabled/source/reason 与连续 12153 计数——「启用」是唯一
// 解除入口，手动禁用与系统禁用都由它一并解除（运维不该为两种禁用猜两个按钮）。
// 熔断器保留：熔断是「连续 5xx 失败」信号（与授权/会话无关），启用后仍有效。
// 调用方必须已持有 p.mu。
func (p *Pool) setDisabledLocked(e *entry, on bool, reason string, source DisabledSource) {
	if !on {
		e.disabled = false
		e.disabledSource = ""
		e.reason = ""
		e.sessionDeadFails = 0
		p.dirty.Store(true)
		return
	}
	// 已是运维禁用态时，系统判定只「保证位为 true」，不改来源/原因，也不清冷却域
	// （否则会把运维写下的禁用原因冲成系统文案）。
	if source == DisabledBySystem && e.disabled && e.disabledSource == DisabledByManual {
		return
	}
	e.clearCoolingLocked() // 冷却域清零（顺带清 reason，下面按来源重写）
	e.disabled = true
	if source == DisabledByManual || e.disabledSource == "" {
		e.disabledSource = source
		e.reason = reason
	}
	p.dirty.Store(true)
}

// reviveCoolingLocked 只清冷却域（until/coolKind/reason/softStreak/modelCooldowns）
// 并更新 credits，不动熔断器（fails/retryCount/breakerUntil）。签到解冻走这里：
// 签到成功只证明余额恢复与 billing 通道健康，不证明 chat 通道健康，熔断（连续 5xx
// 信号）不应被签到覆盖。
// softStreak 属冷却域（与 until/coolKind 同域），随冷却一并清零——与「解冻只清冷却
// 不清熔断」的既有 C5 语义一致；硬冷却（CoolHard）本就不参与 streak，这里清的是
// 历史软冷却累积。调用方必须已持有 p.mu。
func (p *Pool) reviveCoolingLocked(e *entry, credits int64) {
	e.credits = credits
	e.clearCoolingLocked()
}
