package service

import (
	"context"
	"testing"
)

func TestClawbackXianyuRedeemCodeTxGuards(t *testing.T) {
	svc := NewRedeemService(nil, nil, nil, nil, nil, nil, nil, nil)

	// 缺少核销用户：无法定位追回对象
	code := &RedeemCode{ID: 1, Code: "C1", Type: RedeemTypeBalance, Value: 10}
	if _, err := svc.ClawbackXianyuRedeemCodeTx(context.Background(), code); err == nil {
		t.Fatal("expected error for missing used_by")
	}

	// 不支持的类型
	usedBy := int64(1)
	code = &RedeemCode{ID: 2, Code: "C2", Type: "invitation", UsedBy: &usedBy}
	if _, err := svc.ClawbackXianyuRedeemCodeTx(context.Background(), code); err == nil {
		t.Fatal("expected error for unsupported clawback type")
	}
}
