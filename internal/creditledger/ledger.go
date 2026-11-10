// creditledger 记录「每账号每日积分赚/花」，并按天汇总落盘。
//
// 口径（为什么要分三列）：
//   - earned：权威回写（签到查余额）时余额**增加**的部分——签到奖励、活动赠送都在里面；
//   - spent：每次成功请求的上游 usage.credit（真实扣费）累计；
//   - adjust：权威回写时余额**减少**但又不是请求消耗的部分（积分过期、上游回收/修正）。
//     单独一列，避免把"过期"算成"用户花的"。
//
// 存储：
//   - 当日数据在**内存热态**里（每次事件 O(1) 累加），读取接口取的就是热值；
//   - 落盘只存**每日汇总**（每账号 earned/spent/adjust + 日末余额），保留 30 天；
//   - 全局总量由各账号加总得出（先账号、再全局），不单独存一份"第二真相"。
package creditledger

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// cstZone 上游自然日按 CST（+8，无夏令时）。
var cstZone = time.FixedZone("CST", 8*60*60)

// DefaultRetain 保留天数。
const DefaultRetain = 30

// AccountDay 单账号单日汇总。
type AccountDay struct {
	Realm   string `json:"realm,omitempty"`
	Earned  int64  `json:"earned"`
	Spent   int64  `json:"spent"`
	Adjust  int64  `json:"adjust"`
	Credits int64  `json:"credits_end"` // 日末余额（尽力而为：落盘时快照）
}

// Day 一天的全部账号汇总。
type Day struct {
	Date     string                 `json:"date"`
	Accounts map[string]*AccountDay `json:"accounts"`
}

// Totals 全局汇总（由账号加总）。
type Totals struct {
	Earned int64 `json:"earned"`
	Spent  int64 `json:"spent"`
	Adjust int64 `json:"adjust"`
	Net    int64 `json:"net"`
}

// Snapshot 一次性读出（当日热值 + 历史汇总）。
type Snapshot struct {
	Today     string                 `json:"today"`
	Totals    Totals                 `json:"totals"`
	Accounts  map[string]*AccountDay `json:"accounts"`
	Histories []DaySummary           `json:"histories"`
}

// DaySummary 历史某天的汇总（不含每账号明细，接口按需给）。
type DaySummary struct {
	Date     string           `json:"date"`
	Totals   Totals           `json:"totals"`
	Accounts []AccountDayView `json:"accounts"`
}

// AccountDayView 单账号某天的明细（已带 uid）。
type AccountDayView struct {
	UID string `json:"uid"`
	AccountDay
}

// fileData 落盘结构。
type fileData struct {
	UpdatedAt string `json:"updated_at"`
	Days      []Day  `json:"days"`
}

// Ledger 日账本。
type Ledger struct {
	mu     sync.Mutex
	path   string
	retain int
	now    func() time.Time

	day   string // 当前热态所属日期（CST yyyy-mm-dd）
	today map[string]*AccountDay
	days  []Day // 历史（不含今天），按日期升序
}

// New 创建账本（不读盘；读盘见 Load）。
func New(path string, retain int) *Ledger {
	if retain <= 0 {
		retain = DefaultRetain
	}
	return &Ledger{
		path:   path,
		retain: retain,
		now:    time.Now,
		today:  map[string]*AccountDay{},
	}
}

// SetClock 注入时钟（测试用）。
func (l *Ledger) SetClock(f func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.now = f
}

func dayOf(t time.Time) string { return t.In(cstZone).Format("2006-01-02") }

// Load 从盘上恢复历史；当日记录直接作为热态继续累加（重启不丢当日已统计部分）。
func (l *Ledger) Load() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.path == "" {
		return nil
	}
	raw, err := os.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			l.day = dayOf(l.now())
			return nil
		}
		return err
	}
	var fd fileData
	if err := json.Unmarshal(raw, &fd); err != nil {
		return fmt.Errorf("credit-daily 解析失败: %w", err)
	}
	today := dayOf(l.now())
	l.days = l.days[:0]
	for _, d := range fd.Days {
		if d.Accounts == nil {
			d.Accounts = map[string]*AccountDay{}
		}
		if d.Date == today {
			l.today = d.Accounts
			continue
		}
		l.days = append(l.days, d)
	}
	if l.today == nil {
		l.today = map[string]*AccountDay{}
	}
	l.day = today
	l.trimLocked()
	sort.Slice(l.days, func(i, j int) bool { return l.days[i].Date < l.days[j].Date })
	return nil
}

// rollLocked 跨天滚动：把昨天的热态冻结进历史，开一个新的一天。
func (l *Ledger) rollLocked() {
	today := dayOf(l.now())
	if l.day == "" {
		l.day = today
		return
	}
	if l.day == today {
		return
	}
	if len(l.today) > 0 {
		l.days = append(l.days, Day{Date: l.day, Accounts: l.today})
	}
	l.today = map[string]*AccountDay{}
	l.day = today
	l.trimLocked()
}

func (l *Ledger) trimLocked() {
	if len(l.days) <= l.retain {
		return
	}
	l.days = l.days[len(l.days)-l.retain:]
}

func (l *Ledger) accountLocked(uid, realm string) *AccountDay {
	a := l.today[uid]
	if a == nil {
		a = &AccountDay{}
		l.today[uid] = a
	}
	if realm != "" {
		a.Realm = realm
	}
	return a
}

// RecordEarned / RecordSpent / RecordAdjust 记一笔（n<=0 忽略）。
func (l *Ledger) RecordEarned(uid, realm string, n int64) { l.record(uid, realm, n, 0, 0) }
func (l *Ledger) RecordSpent(uid, realm string, n int64)  { l.record(uid, realm, 0, n, 0) }
func (l *Ledger) RecordAdjust(uid, realm string, n int64) { l.record(uid, realm, 0, 0, n) }

// RecordBalanceDelta 处理权威回写：余额变化拆成"赚"与"调整"。
func (l *Ledger) RecordBalanceDelta(uid, realm string, delta int64) {
	if delta > 0 {
		l.RecordEarned(uid, realm, delta)
	} else if delta < 0 {
		l.RecordAdjust(uid, realm, -delta)
	}
}

func (l *Ledger) record(uid, realm string, earned, spent, adjust int64) {
	if uid == "" || (earned == 0 && spent == 0 && adjust == 0) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollLocked()
	a := l.accountLocked(uid, realm)
	a.Earned += earned
	a.Spent += spent
	a.Adjust += adjust
}

// SetCreditsEnd 记录某账号当前余额（供日末快照与看板）。
func (l *Ledger) SetCreditsEnd(uid string, credits int64) {
	if uid == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollLocked()
	l.accountLocked(uid, "").Credits = credits
}

func totalsOf(accounts map[string]*AccountDay) Totals {
	var t Totals
	for _, a := range accounts {
		t.Earned += a.Earned
		t.Spent += a.Spent
		t.Adjust += a.Adjust
	}
	t.Net = t.Earned - t.Spent - t.Adjust
	return t
}

// Snap 返回当日热值 + 历史汇总（历史按日期升序，每账号明细保留）。
func (l *Ledger) Snap() Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollLocked()

	accounts := make(map[string]*AccountDay, len(l.today))
	for k, v := range l.today {
		cp := *v
		accounts[k] = &cp
	}
	out := Snapshot{
		Today:    l.day,
		Totals:   totalsOf(l.today),
		Accounts: accounts,
	}
	for _, d := range l.days {
		sum := DaySummary{Date: d.Date, Totals: totalsOf(d.Accounts)}
		uids := make([]string, 0, len(d.Accounts))
		for uid := range d.Accounts {
			uids = append(uids, uid)
		}
		sort.Strings(uids)
		for _, uid := range uids {
			sum.Accounts = append(sum.Accounts, AccountDayView{UID: uid, AccountDay: *d.Accounts[uid]})
		}
		out.Histories = append(out.Histories, sum)
	}
	return out
}

// History 只返回最近 n 天（含今天）的汇总，历史明细按天给出。
func (l *Ledger) History(n int) []DaySummary {
	s := l.Snap()
	out := make([]DaySummary, 0, n)
	if n <= 0 || n > len(s.Histories)+1 {
		n = len(s.Histories) + 1
	}
	// 今天放最后，取最近 n 天
	all := append([]DaySummary{}, s.Histories...)
	all = append(all, DaySummary{Date: s.Today, Totals: s.Totals, Accounts: todayViews(s.Accounts)})
	if n < len(all) {
		all = all[len(all)-n:]
	}
	out = append(out, all...)
	return out
}

func todayViews(m map[string]*AccountDay) []AccountDayView {
	uids := make([]string, 0, len(m))
	for uid := range m {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]AccountDayView, 0, len(uids))
	for _, uid := range uids {
		out = append(out, AccountDayView{UID: uid, AccountDay: *m[uid]})
	}
	return out
}

// Flush 把当日热态 + 历史写盘（原子替换）。
func (l *Ledger) Flush() error {
	if l.path == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rollLocked()

	days := make([]Day, 0, len(l.days)+1)
	days = append(days, l.days...)
	// 当日也一起写：重启后热态可恢复（"当日取热值"指读取走内存，但落盘保证重启不丢）
	if len(l.today) > 0 {
		cp := make(map[string]*AccountDay, len(l.today))
		for k, v := range l.today {
			c := *v
			cp[k] = &c
		}
		days = append(days, Day{Date: l.day, Accounts: cp})
	}

	fd := fileData{UpdatedAt: l.now().In(cstZone).Format(time.RFC3339), Days: days}
	raw, err := json.MarshalIndent(fd, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// Run 周期性落盘，直到 ctx 结束。
func (l *Ledger) Run(ctx context.Context, every time.Duration) {
	if l.path == "" || every <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = l.Flush()
			return
		case <-t.C:
			if err := l.Flush(); err != nil {
				log.Printf("WARN: credit-daily 落盘失败: %v", err)
			}
		}
	}
}
