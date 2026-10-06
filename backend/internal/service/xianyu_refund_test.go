package service

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type xianyuRefundRepoStub struct {
	begin func(orderNo, accountID string) (*XianyuRefundOutcome, error)
}

func (s *xianyuRefundRepoStub) ProcessRefundAtomically(_ context.Context, orderNo, accountID string, _ XianyuRedeemClawback) (*XianyuRefundOutcome, error) {
	if s.begin != nil {
		return s.begin(orderNo, accountID)
	}
	return &XianyuRefundOutcome{Action: XianyuRefundActionNoClaim}, nil
}

type xianyuClawbackStub struct {
	txCode      *RedeemCode
	txDetall    string
	txErr       error
	invalidated []*RedeemCode
}

func (c *xianyuClawbackStub) ClawbackXianyuRedeemCodeTx(ctx context.Context, code *RedeemCode) (string, error) {
	c.txCode = code
	if c.txErr != nil {
		return "", c.txErr
	}
	return "追回余额 12.00", nil
}

func (c *xianyuClawbackStub) InvalidateAfterClawback(ctx context.Context, code *RedeemCode) {
	c.invalidated = append(c.invalidated, code)
}

func newRefundTestService(repo XianyuRefundEventRepository, clawback XianyuRedeemClawback) *XianyuDeliveryService {
	return NewXianyuDeliveryService(&xianyuClaimRepoStub{}, &xianyuControlStub{}, nil, nil, repo, clawback, nil, newXianyuSettingsStub(true), nil)
}

func TestProcessRefundEventValidatesInput(t *testing.T) {
	svc := newRefundTestService(&xianyuRefundRepoStub{}, &xianyuClawbackStub{})

	cases := []struct {
		name           string
		orderNo        string
		accountID      string
		status         string
		wantErrContain string
	}{
		{"empty order", "  ", "acc", "refunded", "XIANYU_REFUND_ORDER_NO_REQUIRED"},
		{"long order", strings.Repeat("x", 65), "acc", "refunded", "XIANYU_ORDER_ID_TOO_LONG"},
		{"empty account", "O1", "  ", "refunded", "XIANYU_ACCOUNT_ID_REQUIRED"},
		{"long account", "O1", strings.Repeat("x", 81), "refunded", "XIANYU_ACCOUNT_ID_TOO_LONG"},
		{"non refunded status", "O1", "acc", "refunding", "XIANYU_REFUND_STATUS_UNSUPPORTED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.ProcessRefundEvent(context.Background(), tc.orderNo, tc.accountID, tc.status)
			if err == nil || !strings.Contains(err.Error(), tc.wantErrContain) {
				t.Fatalf("want error containing %s, got %v", tc.wantErrContain, err)
			}
		})
	}
}

func TestProcessRefundEventNoClaim(t *testing.T) {
	repo := &xianyuRefundRepoStub{begin: func(orderNo, accountID string) (*XianyuRefundOutcome, error) {
		return nil, ErrXianyuRefundClaimNotFound
	}}
	svc := newRefundTestService(repo, &xianyuClawbackStub{})

	action, err := svc.ProcessRefundEvent(context.Background(), "O-NO-CLAIM", "acc", "refunded")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != XianyuRefundActionNoClaim {
		t.Fatalf("expected no_claim action, got %q", action)
	}
}

func TestProcessRefundEventIdempotentReplay(t *testing.T) {
	repo := &xianyuRefundRepoStub{begin: func(orderNo, accountID string) (*XianyuRefundOutcome, error) {
		return &XianyuRefundOutcome{Action: XianyuRefundActionVoided, Detail: "回放"}, nil
	}}
	svc := newRefundTestService(repo, &xianyuClawbackStub{})

	action, err := svc.ProcessRefundEvent(context.Background(), "O1", "acc", "refunded")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != XianyuRefundActionVoided {
		t.Fatalf("expected replayed action voided, got %q", action)
	}
}

func TestProcessRefundEventAccountMismatch(t *testing.T) {
	repo := &xianyuRefundRepoStub{begin: func(orderNo, accountID string) (*XianyuRefundOutcome, error) {
		return nil, ErrXianyuRefundAccountMismatch
	}}
	svc := newRefundTestService(repo, &xianyuClawbackStub{})

	_, err := svc.ProcessRefundEvent(context.Background(), "O1", "acc", "refunded")
	if err == nil || !strings.Contains(err.Error(), "XIANYU_REFUND_ACCOUNT_MISMATCH") {
		t.Fatalf("expected account mismatch error, got %v", err)
	}
}

func TestProcessRefundEventUsedClawback(t *testing.T) {
	repo := &xianyuRefundRepoStub{begin: func(orderNo, accountID string) (*XianyuRefundOutcome, error) {
		return &XianyuRefundOutcome{
			Action: XianyuRefundActionClawedBack, Detail: "追回余额 12.00",
			Code: &RedeemCode{ID: 9, UsedBy: int64Ptr(42)},
		}, nil
	}}
	clawback := &xianyuClawbackStub{}
	svc := newRefundTestService(repo, clawback)

	action, err := svc.ProcessRefundEvent(context.Background(), "O1", "acc", "refunded")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != XianyuRefundActionClawedBack {
		t.Fatalf("expected clawed_back action, got %q", action)
	}
	if len(clawback.invalidated) != 1 || clawback.invalidated[0].ID != 9 {
		t.Fatalf("expected post-commit cache invalidation, got %+v", clawback.invalidated)
	}
}

func TestProcessRefundEventVoidedNoInvalidation(t *testing.T) {
	repo := &xianyuRefundRepoStub{begin: func(orderNo, accountID string) (*XianyuRefundOutcome, error) {
		return &XianyuRefundOutcome{Action: XianyuRefundActionVoided, Detail: "作废"}, nil
	}}
	clawback := &xianyuClawbackStub{}
	svc := newRefundTestService(repo, clawback)

	action, err := svc.ProcessRefundEvent(context.Background(), "O1", "acc", "refunded")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if action != XianyuRefundActionVoided {
		t.Fatalf("expected voided action, got %q", action)
	}
	if len(clawback.invalidated) != 0 {
		t.Fatal("voided path must not trigger clawback cache invalidation")
	}
}

func TestProcessRefundEventPropagatesErrors(t *testing.T) {
	repo := &xianyuRefundRepoStub{begin: func(orderNo, accountID string) (*XianyuRefundOutcome, error) {
		return nil, errors.New("db down")
	}}
	svc := newRefundTestService(repo, &xianyuClawbackStub{})

	if _, err := svc.ProcessRefundEvent(context.Background(), "O1", "acc", "refunded"); err == nil {
		t.Fatal("expected repo error to propagate")
	}
}
