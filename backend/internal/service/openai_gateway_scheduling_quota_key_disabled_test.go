//go:build unit

package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
)

// TestQuotaKeyStatusBlocksCursorScheduling 覆盖激活码"停供"判定的四个状态：
// active 不拦、expired 拦、disabled 拦、exhausted 不拦。
func TestQuotaKeyStatusBlocksCursorScheduling(t *testing.T) {
	cases := []struct {
		name   string
		status string
		want   bool
	}{
		{name: "active 不拦", status: domain.QuotaKeyStatusActive, want: false},
		{name: "expired 拦", status: domain.QuotaKeyStatusExpired, want: true},
		{name: "disabled 拦", status: domain.QuotaKeyStatusDisabled, want: true},
		{name: "exhausted 不拦", status: domain.QuotaKeyStatusExhausted, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := quotaKeyStatusBlocksCursorScheduling(c.status); got != c.want {
				t.Fatalf("quotaKeyStatusBlocksCursorScheduling(%q) = %v, want %v", c.status, got, c.want)
			}
		})
	}
}
