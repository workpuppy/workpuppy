package scheduler

import (
	"testing"
	"time"

	"wp/internal/auth"
)

// 今天已签到成功的账号：一键/自动签到都直接跳过（不白打上游），且状态持久在池里。
func TestCheckinSkipsAlreadyCheckedInToday(t *testing.T) {
	s, p := newCheckinS(t, &checkinStub{checkinBody: `{"code":0,"data":{}}`, resourceRemain: 100})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	// 标记 u1 今天已签
	p.MarkCheckin("u1", "ok", time.Now())
	if ok, _ := p.CheckedInToday("u1", time.Now()); !ok {
		t.Fatal("precondition: u1 应标记为今日已签")
	}

	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	var u1, u2 bool
	for _, o := range out {
		switch o.UID {
		case "u1":
			u1 = true
			if o.Status != CheckinAlready {
				t.Errorf("u1 应跳过为 already，实际 %s", o.Status)
			}
			if o.Detail == "" || !contains(o.Detail, "checked_in_today") {
				t.Errorf("u1 detail 应说明原因，实际 %q", o.Detail)
			}
		case "u2":
			u2 = true
			if o.Status != CheckinOK {
				t.Errorf("u2 应正常签到，实际 %s", o.Status)
			}
		}
	}
	if !u1 || !u2 {
		t.Fatalf("两个账号都应有结果：u1=%v u2=%v", u1, u2)
	}
	// 签到成功后状态被记录（u2 现在也是"今日已签"）
	if ok, status := p.CheckedInToday("u2", time.Now()); !ok || status != "ok" {
		t.Errorf("u2 签到后应记录状态，实际 ok=%v status=%s", ok, status)
	}
}

// 跨自然日：昨天的签到不算今天，会重新签到。
func TestCheckinResumesNextDay(t *testing.T) {
	s, p := newCheckinS(t, &checkinStub{checkinBody: `{"code":0,"data":{}}`, resourceRemain: 100})
	// 用昨天的时刻打标
	yesterday := time.Now().AddDate(0, 0, -1)
	p.MarkCheckin("u1", "ok", yesterday)
	if ok, _ := p.CheckedInToday("u1", time.Now()); ok {
		t.Fatal("昨天的签到不应算今天")
	}
	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("CheckinAll: %v", err)
	}
	if len(out) != 1 || out[0].Status != CheckinOK {
		t.Fatalf("跨天后应重新签到，实际 %+v", out)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
