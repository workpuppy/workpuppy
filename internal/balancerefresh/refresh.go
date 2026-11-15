// Package balancerefresh 刷新全部账号的**权威余额**（上游 billing 只读接口）。
//
// 为什么需要独立一层：余额以前只在「签到 / 手动刷新余额」时回写，global 账号不签到，
// 于是积分余额长期停在导入时的 0 或旧值；而积分日账本的「调整」（非请求的余额减少）
// 只有权威回写产生负差额时才会出现 —— 不刷新就永远是 0。
//
// 语义：
//   - 只调上游只读 billing 接口（UserResourceDetailed），不签到、不发对话；
//   - 回写走 ReenableIfCredits（解冻 + 记 earned/adjust 差额）→ SetCreditsDetailed
//     （补写快过期分桶），与签到路径完全一致，避免两套口径；
//   - 进程级单飞：手动按钮与定时任务共用一把锁，避免同时扫描上游。
package balancerefresh

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"wp/internal/logfmt"
	"wp/internal/pool"
	"wp/internal/upstream"
)

// ErrBusy 已有一轮刷新在进行（手动/定时撞车时返回，不排队）。
var ErrBusy = errors.New("余额刷新正在进行中")

// mu 进程级单飞锁。
var mu sync.Mutex

// accountDelay 账号之间的间隔：与签到同量级，避免一轮刷新把上游打热。
const accountDelay = 200 * time.Millisecond

// Row 单账号刷新结果（管理面按钮直接回给前端展示）。
type Row struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Status   string `json:"status"` // ok / skipped / fail
	Credits  *int64 `json:"credits,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// Result 一轮刷新的汇总。
type Result struct {
	OK      int   `json:"ok"`
	Skipped int   `json:"skipped"`
	Fail    int   `json:"fail"`
	Total   int   `json:"total"`
	Rows    []Row `json:"accounts"`
}

// Refresh 逐账号拉一次权威余额并回写池。ctx 取消时在账号边界安全退出。
func Refresh(ctx context.Context, p *pool.Pool, up *upstream.Client) (Result, error) {
	if !mu.TryLock() {
		return Result{}, ErrBusy
	}
	defer mu.Unlock()

	if p == nil || up == nil {
		return Result{}, errors.New("余额刷新未接线（pool/upstream 为空）")
	}
	statuses := p.List()
	res := Result{Rows: make([]Row, 0, len(statuses))}

	for _, st := range statuses {
		if ctx.Err() != nil {
			break
		}
		if st.Disabled || p.RiskPaused(st.UID) {
			res.Rows = append(res.Rows, Row{UID: st.UID, Nickname: st.Nickname, Status: "skipped", Detail: "disabled"})
			res.Skipped++
			continue
		}
		a := p.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			res.Rows = append(res.Rows, Row{UID: st.UID, Nickname: st.Nickname, Status: "skipped", Detail: "no credentials"})
			res.Skipped++
			continue
		}
		remain, buckets, err := up.UserResourceDetailed(a, 0) // 0 = 不分桶，只要总量
		if err != nil {
			res.Rows = append(res.Rows, Row{UID: st.UID, Nickname: st.Nickname, Status: "fail", Detail: err.Error()})
			res.Fail++
			log.Printf("refresh-balance %s: %v", logfmt.Label(st.UID, st.Nickname), err)
		} else {
			// 与签到同一顺序：先解冻并上报差额（earned/adjust），再补写快过期分桶。
			p.ReenableIfCredits(st.UID, remain)
			p.SetCreditsDetailed(st.UID, remain, buckets.Expiring)
			v := remain
			res.Rows = append(res.Rows, Row{UID: st.UID, Nickname: st.Nickname, Status: "ok", Credits: &v})
			res.OK++
		}
		if !sleepCtx(ctx, accountDelay) {
			break
		}
	}
	res.Total = len(res.Rows)
	return res, nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
