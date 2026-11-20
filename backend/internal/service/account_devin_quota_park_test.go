package service

import (
	"fmt"
	"testing"
	"time"
)

// devinQuotaParkReason 按 TempUnschedState 的 JSON 形状构造停调原因。
// until_unix 固定为 1（早已过期），关键字来自同源 marker 常量，
// 因此该字符串等价于 {"until_unix":1,"matched_keyword":"devin_upstream_quota_exhausted"}。
func devinQuotaParkReason(marker string) string {
	return fmt.Sprintf(`{"until_unix":1,"matched_keyword":%q}`, marker)
}

func devinQuotaParkPastUntil() *time.Time {
	past := time.Now().Add(-time.Hour)
	return &past
}

func devinQuotaParkAccount(platform, reason string, until *time.Time) *Account {
	return &Account{
		Status:                  StatusActive,
		Schedulable:             true,
		Platform:                platform,
		TempUnschedulableUntil:  until,
		TempUnschedulableReason: reason,
	}
}

func TestIsSchedulableDevinQuotaPark(t *testing.T) {
	past := devinQuotaParkPastUntil()

	t.Run("quota_exhausted_marker_blocks_after_until_expired", func(t *testing.T) {
		// until 已过期，但 reason 仍带 Devin 额度耗尽 marker：持续停调。
		a := devinQuotaParkAccount(PlatformDevin, devinQuotaParkReason(DevinUpstreamQuotaExhaustedMarker), past)
		if a.IsSchedulable() {
			t.Fatal("Devin 额度耗尽 marker 未清除时账号应保持停调，即使 until 已过期")
		}
	})

	t.Run("other_marker_does_not_block", func(t *testing.T) {
		// 其他 marker 不受 marker 驱动停调影响，until 过期即可恢复。
		a := devinQuotaParkAccount(PlatformDevin, devinQuotaParkReason("openai_apikey_health_breaker"), past)
		if !a.IsSchedulable() {
			t.Fatal("非 Devin 额度耗尽 marker 不应触发持续停调，until 已过期应可调度")
		}
	})

	t.Run("non_devin_platform_unaffected", func(t *testing.T) {
		// 非 devin 平台即使带同款 marker 也不受影响。
		a := devinQuotaParkAccount(PlatformOpenAI, devinQuotaParkReason(DevinUpstreamQuotaExhaustedMarker), past)
		if !a.IsSchedulable() {
			t.Fatal("非 devin 平台带 Devin 额度耗尽 marker 时不应被持续停调")
		}
	})

	t.Run("cleared_park_restores_schedulable", func(t *testing.T) {
		// 探针清除停调后（until=nil、reason=""）恢复可调度。
		a := devinQuotaParkAccount(PlatformDevin, "", nil)
		if !a.IsSchedulable() {
			t.Fatal("清除停调后账号应恢复可调度")
		}
	})
}

func TestIsParkedPendingDevinQuotaRecovery(t *testing.T) {
	t.Run("nil_account_returns_false", func(t *testing.T) {
		var a *Account
		if a.IsParkedPendingDevinQuotaRecovery() {
			t.Fatal("nil 账号应安全返回 false")
		}
	})

	t.Run("devin_marker_returns_true", func(t *testing.T) {
		a := devinQuotaParkAccount(PlatformDevin, devinQuotaParkReason(DevinUpstreamQuotaExhaustedMarker), devinQuotaParkPastUntil())
		if !a.IsParkedPendingDevinQuotaRecovery() {
			t.Fatal("带 Devin 额度耗尽 marker 的 devin 账号应处于等待探针恢复状态")
		}
	})

	t.Run("other_marker_returns_false", func(t *testing.T) {
		a := devinQuotaParkAccount(PlatformDevin, devinQuotaParkReason("openai_apikey_health_breaker"), devinQuotaParkPastUntil())
		if a.IsParkedPendingDevinQuotaRecovery() {
			t.Fatal("其他 marker 不应被判为 Devin 额度耗尽等待恢复")
		}
	})
}
