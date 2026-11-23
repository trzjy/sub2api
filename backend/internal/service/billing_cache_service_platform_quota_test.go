//go:build unit

package service

import (
	"context"
	"testing"
)

// TestCheckUserPlatformQuotaEligibilityExported 验证导出包装正确委托到既有私有检查，
// 不引入第二套逻辑。svc 的 userPlatformQuotaRepo 为 nil：私有检查在 platform 非空
// 但 repo 为 nil 或 platform 为空时均直接放行，且全程不 panic。
func TestCheckUserPlatformQuotaEligibilityExported(t *testing.T) {
	svc := &BillingCacheService{} // userPlatformQuotaRepo 为 nil

	// 非空 platform：私有检查因 userPlatformQuotaRepo==nil 直接返回 nil，导出包装不应 panic。
	if err := svc.CheckUserPlatformQuotaEligibility(context.Background(), 1, "anthropic"); err != nil {
		t.Fatalf("non-empty platform: unexpected error: %v", err)
	}

	// 空 platform：私有检查直接返回 nil，导出包装不应 panic。
	if err := svc.CheckUserPlatformQuotaEligibility(context.Background(), 1, ""); err != nil {
		t.Fatalf("empty platform: unexpected error: %v", err)
	}
}
