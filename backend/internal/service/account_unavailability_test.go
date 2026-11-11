package service

import (
	"net/http"
	"testing"
	"time"
)

// 本文件锁死"账号停多久"的唯一规范。这些断言是回归防线：
// 任何人把停用时长改回无限期、或让未知错误比限流更重，都会在这里挂掉。

func TestClassifyAccountUnavailability_429IsShortCooldown(t *testing.T) {
	now := time.Now()
	d := ClassifyAccountUnavailability(UnavailabilityFact{
		Kind: UnavailRateLimited,
		Now:  now,
	})
	requireWithin(t, d.Until.Sub(now), unavailRateLimitDefaultCooldown)
	if d.Recovery != RecoverByExpiry {
		t.Fatalf("429 必须到点自动恢复，得到 %q", d.Recovery)
	}
}

func TestClassifyAccountUnavailability_UnknownIsLightest(t *testing.T) {
	now := time.Now()
	unknown := ClassifyAccountUnavailability(UnavailabilityFact{Kind: UnavailUnknown, Now: now})
	rateLimited := ClassifyAccountUnavailability(UnavailabilityFact{Kind: UnavailRateLimited, Now: now})

	// 核心 invariant：看不懂的原因，处罚绝不能重于限流。
	if unknown.Until.Sub(now) > rateLimited.Until.Sub(now) {
		t.Fatalf("未知类型的停用时长 %v 超过了限流 %v，违反'看不懂就轻罚'原则",
			unknown.Until.Sub(now), rateLimited.Until.Sub(now))
	}
	if unknown.Until.Sub(now) > time.Minute {
		t.Fatalf("未知类型停用 %v，超过 1 分钟，过重", unknown.Until.Sub(now))
	}
}

func TestClassifyAccountUnavailability_NeverExceedsMax(t *testing.T) {
	now := time.Now()
	year9999 := time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)
	d := ClassifyAccountUnavailability(UnavailabilityFact{
		Kind:    UnavailQuotaExhausted,
		ResetAt: &year9999,
		Now:     now,
	})
	if d.Until.Sub(now) > unavailMaxCooldown {
		t.Fatalf("停用时长 %v 超过硬上限 %v，9999 年哨兵值回流", d.Until.Sub(now), unavailMaxCooldown)
	}
	if d.Source != "reset_at_clamped" {
		t.Fatalf("被夹住时应标注来源，得到 %q", d.Source)
	}
}

func TestUnavailabilityMaxCooldownBansYear9999(t *testing.T) {
	if unavailMaxCooldown > 24*time.Hour {
		t.Fatalf("硬上限 %v 过大，失去约束意义", unavailMaxCooldown)
	}
	// 任何类型的默认时长都必须在上限内。
	for _, kind := range []AccountUnavailabilityKind{
		UnavailRateLimited, UnavailQuotaExhausted, UnavailConcurrency,
		UnavailUpstreamFault, UnavailCredentialDead, UnavailTransient, UnavailUnknown,
	} {
		if got := defaultCooldownForKind(kind); got > unavailMaxCooldown || got < time.Second {
			t.Fatalf("类型 %q 的默认时长 %v 超出合法区间", kind, got)
		}
	}
}

func TestClassifyAccountUnavailability_ResetAtWins(t *testing.T) {
	now := time.Now()
	reset := now.Add(45 * time.Minute)
	d := ClassifyAccountUnavailability(UnavailabilityFact{
		Kind:       UnavailRateLimited,
		ResetAt:    &reset,
		RetryAfter: 10 * time.Second, // 有 ResetAt 时不应被采信
		Now:        now,
	})
	requireWithin(t, d.Until.Sub(now), 45*time.Minute)
	if d.Source != "reset_at" {
		t.Fatalf("来源应为 reset_at，得到 %q", d.Source)
	}
}

func TestClassifyAccountUnavailability_RetryAfterWins(t *testing.T) {
	now := time.Now()
	d := ClassifyAccountUnavailability(UnavailabilityFact{
		Kind:       UnavailRateLimited,
		RetryAfter: 20 * time.Second,
		Now:        now,
	})
	requireWithin(t, d.Until.Sub(now), 20*time.Second)
	if d.Source != "retry_after" {
		t.Fatalf("来源应为 retry_after，得到 %q", d.Source)
	}
}

func TestClassifyAccountUnavailability_PastResetAtIgnored(t *testing.T) {
	now := time.Now()
	past := now.Add(-1 * time.Hour)
	d := ClassifyAccountUnavailability(UnavailabilityFact{
		Kind:    UnavailRateLimited,
		ResetAt: &past,
		Now:     now,
	})
	// 上游给的恢复时刻已过期（数据与实际不符）→ 回落类型默认值，不能产生过去时间。
	if !d.Until.After(now) {
		t.Fatalf("恢复时刻必须晚于现在，得到 %v", d.Until)
	}
	if d.Source != "kind_default" {
		t.Fatalf("来源应为 kind_default，得到 %q", d.Source)
	}
}

func TestClassifyAccountUnavailability_AllKindsProduceFutureUntil(t *testing.T) {
	now := time.Now()
	for _, kind := range []AccountUnavailabilityKind{
		UnavailRateLimited, UnavailQuotaExhausted, UnavailConcurrency,
		UnavailUpstreamFault, UnavailCredentialDead, UnavailTransient,
		UnavailUnknown, "", "some_future_kind_nobody_registered",
	} {
		d := ClassifyAccountUnavailability(UnavailabilityFact{Kind: kind, Now: now})
		if !d.Until.After(now) {
			t.Fatalf("类型 %q 产生了非未来时间 %v", kind, d.Until)
		}
		if d.Until.Sub(now) > unavailMaxCooldown {
			t.Fatalf("类型 %q 超过上限 %v", kind, d.Until.Sub(now))
		}
	}
}

func TestKindForUpstreamStatus(t *testing.T) {
	cases := map[int]AccountUnavailabilityKind{
		429: UnavailRateLimited,
		401: UnavailCredentialDead,
		403: UnavailCredentialDead,
		402: UnavailQuotaExhausted,
		408: UnavailTransient,
		504: UnavailTransient,
		502: UnavailTransient,
		500: UnavailUpstreamFault,
		503: UnavailUpstreamFault,
		529: UnavailUpstreamFault,
		418: UnavailUnknown,
		0:   UnavailUnknown,
	}
	for status, want := range cases {
		if got := KindForUpstreamStatus(status); got != want {
			t.Fatalf("status %d: 期望 %q，得到 %q", status, want, got)
		}
	}
}

func TestClassifyUpstreamStatus_429HasNoImmediateDisable(t *testing.T) {
	now := time.Now()
	d := ClassifyUpstreamStatus(http.StatusTooManyRequests, 0, nil, now)
	if d.Kind != UnavailRateLimited {
		t.Fatalf("429 应归为限流，得到 %q", d.Kind)
	}
	if d.Until.Sub(now) > 5*time.Minute {
		t.Fatalf("429 停用 %v，过重；限流必须短冷却", d.Until.Sub(now))
	}
}

func TestClassifyUpstreamStatus_ServerFaultIsFinitene(t *testing.T) {
	now := time.Now()
	d := ClassifyUpstreamStatus(http.StatusInternalServerError, 0, nil, now)
	if d.Recovery != RecoverByExpiry {
		t.Fatalf("5xx 必须到点自动恢复，得到 %q", d.Recovery)
	}
	if d.Until.Sub(now) > unavailMaxCooldown {
		t.Fatalf("5xx 停用超过上限")
	}
}

// requireWithin 断言实际时长与期望相差不超过 2 秒（吸收调用与断言的时钟漂移）。
func requireWithin(t *testing.T, got, want time.Duration) {
	t.Helper()
	delta := got - want
	if delta < 0 {
		delta = -delta
	}
	if delta > 2*time.Second {
		t.Fatalf("时长 %v 与期望 %v 相差过大", got, want)
	}
}
