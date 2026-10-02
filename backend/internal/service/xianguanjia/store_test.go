package xianguanjia

import (
	"context"
	"testing"
	"time"
)

func TestExternalCardStoreMemoryRoundTrip(t *testing.T) {
	store := NewExternalCardStoreMemory()
	ctx := context.Background()

	now := time.Now()
	row := ExternalCardRow{
		OrderNo:          "O1",
		CardNo:           "C1",
		CardPwdEncrypted: EncryptCardPwd("P1"),
		SoldType:         "1",
		CreatedAt:        now,
	}
	if err := store.UpsertExternalCard(ctx, row); err != nil {
		t.Fatalf("UpsertExternalCard returned error: %v", err)
	}
	// 同 order_no+card_no 的 upsert 应覆盖而非新增。
	row2 := ExternalCardRow{
		OrderNo:          "O1",
		CardNo:           "C1",
		CardPwdEncrypted: EncryptCardPwd("P1-bis"),
		SoldType:         "2",
		CreatedAt:        now,
	}
	if err := store.UpsertExternalCard(ctx, row2); err != nil {
		t.Fatalf("UpsertExternalCard (dup) returned error: %v", err)
	}
	// 不同 card_no 新增一行。
	if err := store.UpsertExternalCard(ctx, ExternalCardRow{OrderNo: "O1", CardNo: "C2", CardPwdEncrypted: EncryptCardPwd("P2"), SoldType: "1"}); err != nil {
		t.Fatalf("UpsertExternalCard (second card) returned error: %v", err)
	}

	cards, err := store.GetExternalCards(ctx, "O1")
	if err != nil {
		t.Fatalf("GetExternalCards returned error: %v", err)
	}
	if len(cards) != 2 {
		t.Fatalf("expected 2 cards, got %d", len(cards))
	}
	// 校验覆盖生效：C1 的 sold_type 应为 "2"。
	for _, c := range cards {
		if c.CardNo == "C1" && c.SoldType != "2" {
			t.Fatalf("expected C1 overwritten sold_type=2, got %q", c.SoldType)
		}
		plain, err := DecryptCardPwd(c.CardPwdEncrypted)
		if err != nil {
			t.Fatalf("DecryptCardPwd returned error: %v", err)
		}
		if plain == "" {
			t.Fatalf("decrypted card pwd is empty")
		}
	}
}

func TestPushIdempotencyStoreMemory(t *testing.T) {
	store := NewPushIdempotencyStoreMemory()
	ctx := context.Background()
	ok, err := store.Record(ctx, "O1", "5", "23", "t1")
	if err != nil || !ok {
		t.Fatalf("first Record expected (true, nil), got (%v, %v)", ok, err)
	}
	ok, err = store.Record(ctx, "O1", "5", "23", "t1")
	if err != nil || ok {
		t.Fatalf("second Record expected (false, nil), got (%v, %v)", ok, err)
	}
	// 不同 modify_time 视为不同事件。
	ok, err = store.Record(ctx, "O1", "5", "23", "t2")
	if err != nil || !ok {
		t.Fatalf("third Record (new modify_time) expected (true, nil), got (%v, %v)", ok, err)
	}
}
