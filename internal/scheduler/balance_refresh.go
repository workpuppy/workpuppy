// balance_refresh.go 余额定期刷新：把"余额只有手动刷新才动"变成后台周期任务。
//
// 为什么放在 scheduler 而不是 main：它与其他自动任务共享同一个 ctx、
// 同一份 pool/upstream 配置，退出时自然停止；也不需要在 main 里再管理一个 goroutine。
//
// 频率由 schedule.balance_refresh_interval 控制（默认 30m；0 = 关闭）。每轮逐号调用
// balancerefresh.Refresh：只读 billing、串行 + 账号间隔；与手动按钮共用单飞锁。
package scheduler

import (
	"context"
	"errors"
	"log"
	"time"

	"wp/internal/balancerefresh"
)

// balanceRefreshFirstDelay 启动后首轮刷新的延迟：避开启动瞬间的 token 刷新/签到，
// 也让导入的账号尽快拿到余额（否则要等一个完整 interval）。
var balanceRefreshFirstDelay = 30 * time.Second

func (s *Scheduler) runBalanceRefreshLoop(ctx context.Context) {
	interval := s.cfg.BalanceRefreshInterval
	log.Printf("余额定期刷新已启用：每 %v（首轮 %v 后；逐号只读 billing 接口）", interval, balanceRefreshFirstDelay)

	// 首轮：等一小段再跑，避免和启动时的 token 刷新/签到抢上游。
	select {
	case <-ctx.Done():
		return
	case <-time.After(balanceRefreshFirstDelay):
	}
	s.refreshBalancesOnce(ctx)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refreshBalancesOnce(ctx)
		}
	}
}

func (s *Scheduler) refreshBalancesOnce(ctx context.Context) {
	res, err := balancerefresh.Refresh(ctx, s.cfg.Pool, s.cfg.Upstream)
	if errors.Is(err, balancerefresh.ErrBusy) {
		log.Printf("余额定期刷新：上一轮仍在进行，跳过本次")
		return
	}
	if err != nil {
		log.Printf("余额定期刷新失败：%v", err)
		return
	}
	log.Printf("余额定期刷新完成：成功 %d / 跳过 %d / 失败 %d（共 %d）",
		res.OK, res.Skipped, res.Fail, res.Total)
}
