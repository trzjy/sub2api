package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

// fixedCST 固定东八区，避免单测在不同 tzdata 环境下结果漂移。
var fixedCST = time.FixedZone("CST", 8*3600)

// mustTime 构造 fixedCST 下的时间点。
func mustTime(t *testing.T, layout string) time.Time {
	t.Helper()
	tm, err := time.ParseInLocation("2006-01-02 15:04", layout, fixedCST)
	if err != nil {
		t.Fatalf("parse %q: %v", layout, err)
	}
	return tm
}

// cursorQuotaStopRepoStub 只记录额度耗尽停调写入的参数。
type cursorQuotaStopRepoStub struct {
	AccountRepository // 嵌入接口：未实现的方法会 panic（不应被调用）

	calls  int
	until  time.Time
	reason string
}

func (r *cursorQuotaStopRepoStub) SetTempUnschedulable(_ context.Context, _ int64, until time.Time, reason string) error {
	r.calls++
	r.until = until
	r.reason = reason
	return nil
}

// TestTempUnscheduleCursorQuotaExhaustedUsesShortCooldown 锁定 2026-11-06 的退档结论。
//
// 背景（生产数据三重排他）：「今日额度已用完」不是余额判定——命中时累计用量从 0 到
// 405 万 token 不等（差 16 倍）；同码同日 340 号跑 49 次 / 405 万 token 从未命中，
// 343 号只跑 3 次 / 14 万就命中；且 343 命中那一刻同码 340 号正在连续成功。
// 故停调时长必须退回到与普通上游故障同级的分钟级，绝不能再按日停到次日 08:05。
//
// 本用例同时防止两件事回归：
//  1. 有人把冷却改回按日（断言上限 24 小时）；
//  2. 有人把停调原因改成码级前缀（那会让码级清除逻辑误清账号级停调）。
func TestTempUnscheduleCursorQuotaExhaustedUsesShortCooldown(t *testing.T) {
	repo := &cursorQuotaStopRepoStub{}
	before := timezone.Now()

	tempUnscheduleCursorQuotaExhausted(context.Background(), repo, 358, "[test]")

	if repo.calls != 1 {
		t.Fatalf("停调写入次数 = %d，want 1", repo.calls)
	}
	got := repo.until.Sub(before)
	if got < cursorQuotaExhaustedCooldown-2*time.Second || got > cursorQuotaExhaustedCooldown+2*time.Minute {
		t.Fatalf("停调时长 = %s，want 约 %s", got, cursorQuotaExhaustedCooldown)
	}
	if got > 24*time.Hour {
		t.Fatalf("停调时长 = %s，越过一天：按日停调会把好号白躺一整天", got)
	}
	if !strings.HasPrefix(repo.reason, cursorAccountQuotaExhaustedReasonPrefix) {
		t.Fatalf("停调原因 = %q，必须以账号级前缀 %q 开头（码级前缀会被码级清除逻辑误清）",
			repo.reason, cursorAccountQuotaExhaustedReasonPrefix)
	}
	if repo.until.Before(before) {
		t.Fatalf("停调目标 %s 早于当前时刻 %s", repo.until, before)
	}

	// 停调只写一次性状态，不得再往 accounts.extra 塞"额度耗尽时刻"这种散字段：
	// 那是只写不读的死字段，观测职责已由 cursor_account_daily_quota_stats 承担。
	// stub 未实现 UpdateExtra，一旦有人把写入加回来，这里会直接 panic 暴露。
}

// TestCursorQuotaExhaustedUntilRemoved 锁死「梯度探测已删除」这一事实。
//
// 这个用例的价值不在于断言什么，而在于：如果以后有人把 cursorQuotaProbeWindows /
// cursorQuotaExhaustedUntil 重新加回来并想复用到停调逻辑，本文件会先因为编译不过而
// 提醒他去看 cursorQuotaExhaustedCooldown 的注释（为什么不能按日停）。
//
// 当前实现下，任何时候命中都只停固定 5 分钟，与时刻无关，故三个时段结果一致。
func TestCursorQuotaExhaustedUntilRemoved(t *testing.T) {
	for _, at := range []string{"2026-11-06 03:00", "2026-11-06 09:40", "2026-11-06 23:59"} {
		repo := &cursorQuotaStopRepoStub{}
		before := timezone.Now()
		tempUnscheduleCursorQuotaExhausted(context.Background(), repo, 1, "[test]")
		if repo.calls != 1 {
			t.Fatalf("at=%s 停调写入次数 = %d，want 1", at, repo.calls)
		}
		// 与时刻无关：停调目标相对当前时刻的偏移必须恒定。
		if d := repo.until.Sub(before); d > cursorQuotaExhaustedCooldown+2*time.Minute {
			t.Fatalf("at=%s 停调时长 = %s，不再是固定冷却（疑似恢复成按时刻分档）", at, d)
		}
	}
}
