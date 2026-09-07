package service

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

type RedeemCode struct {
	ID        int64
	Code      string
	Type      string
	Value     float64
	Status    string
	UsedBy    *int64
	UsedAt    *time.Time
	Notes     string
	CreatedAt time.Time
	ExpiresAt *time.Time

	GroupID      *int64
	ValidityDays int

	User  *User
	Group *Group
}

func (r *RedeemCode) IsUsed() bool {
	return r.Status == StatusUsed
}

func (r *RedeemCode) IsExpired() bool {
	return r.IsExpiredAt(time.Now())
}

func (r *RedeemCode) IsExpiredAt(now time.Time) bool {
	if r == nil {
		return false
	}
	if r.Status == StatusExpired {
		return true
	}
	return r.Status == StatusUnused && r.ExpiresAt != nil && !r.ExpiresAt.After(now)
}

func (r *RedeemCode) CanUse() bool {
	// delivered = 已通过闲鱼库存池发货、买家尚未兑换，仍然可兑换。
	return (r.Status == StatusUnused || r.Status == StatusDelivered) && !r.IsExpired()
}

func GenerateRedeemCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
