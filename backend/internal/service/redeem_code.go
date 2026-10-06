package service

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
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

	// BatchID 仅 welfare 类型有值：所属福利批次。
	BatchID *int64
	// GroupGrants 仅 welfare 类型兑换成功后填充：分组权益清单（展示用）。
	GroupGrants []WelfareGroupGrant

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

// GenerateRedeemCode 是历史独立格式的兑换码生成：16 字节随机 → 32 位小写 hex（无连字符）。
// 消费方为管理端 admin 路径（admin_user.go）与 xianyu_control_service.go，其契约与
// 标准格式（GenerateRandomRedeemCode 的大写四段连字符格式）不同（大小写/连字符），
// 因此不合并、不改格式——改格式即变更 admin 语义，超出本任务边界（D6F-R 修复#3）。
// 此函数体与其全部既有消费方零改动。
func GenerateRedeemCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// GenerateRandomRedeemCode 生成标准格式兑换码：16 字节随机 → 32 位 hex →
// 大写 XXXX-XXXX-XXXX-XXXX。它是「货源下单现场生成（xianguanjia SupplyCardGenerator）
// 与 RedeemService.GenerateRandomCode」的唯一实现，二者均复用本函数，避免第二份
// 码生成逻辑。注意：admin 历史路径的 GenerateRedeemCode 为独立旧格式契约（小写无连字符），
// 不在本收敛范围（D6F-R 修复#3）。
func GenerateRandomRedeemCode() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	hexStr := hex.EncodeToString(b)
	parts := []string{
		strings.ToUpper(hexStr[0:8]),
		strings.ToUpper(hexStr[8:16]),
		strings.ToUpper(hexStr[16:24]),
		strings.ToUpper(hexStr[24:32]),
	}
	return strings.Join(parts, "-"), nil
}
