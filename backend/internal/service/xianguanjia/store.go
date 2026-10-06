package xianguanjia

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ExternalCardRow 是 xianyu_external_cards 表的一行（卡密映射落库）。
// card_pwd_encrypted 为加密后的卡密，避免明文落库。
type ExternalCardRow struct {
	OrderNo          string
	CardNo           string
	CardPwdEncrypted string
	SoldType         string
	VoidedAt         *time.Time
	CreatedAt        time.Time
}

// ExternalCardStore 卡密映射存储接口。DB 实现与内存实现都满足此接口，便于单测与 handler 解耦。
type ExternalCardStore interface {
	UpsertExternalCard(ctx context.Context, row ExternalCardRow) error
	GetExternalCards(ctx context.Context, orderNo string) ([]ExternalCardRow, error)
}

// ---- DB 实现 ----

type externalCardStoreDB struct {
	db *sql.DB
}

// NewExternalCardStore 返回基于 PostgreSQL 的卡密映射存储。
func NewExternalCardStore(db *sql.DB) *externalCardStoreDB {
	return &externalCardStoreDB{db: db}
}

func (s *externalCardStoreDB) UpsertExternalCard(ctx context.Context, row ExternalCardRow) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("xianguanjia external card store unavailable")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO xianyu_external_cards (order_no, card_no, card_pwd_encrypted, sold_type, voided_at, created_at)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6, now()))
		ON CONFLICT (order_no, card_no) DO UPDATE SET
			card_pwd_encrypted = EXCLUDED.card_pwd_encrypted,
			sold_type = EXCLUDED.sold_type,
			voided_at = EXCLUDED.voided_at`,
		row.OrderNo, row.CardNo, row.CardPwdEncrypted, row.SoldType, row.VoidedAt, row.CreatedAt)
	if err != nil {
		return fmt.Errorf("xianguanjia upsert external card: %w", err)
	}
	return nil
}

func (s *externalCardStoreDB) GetExternalCards(ctx context.Context, orderNo string) ([]ExternalCardRow, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("xianguanjia external card store unavailable")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT order_no, card_no, card_pwd_encrypted, sold_type, voided_at, created_at
		FROM xianyu_external_cards WHERE order_no = $1 ORDER BY card_no`, orderNo)
	if err != nil {
		return nil, fmt.Errorf("xianguanjia get external cards: %w", err)
	}
	defer rows.Close()
	var out []ExternalCardRow
	for rows.Next() {
		var r ExternalCardRow
		var voided sql.NullTime
		if err := rows.Scan(&r.OrderNo, &r.CardNo, &r.CardPwdEncrypted, &r.SoldType, &voided, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("xianguanjia scan external card: %w", err)
		}
		if voided.Valid {
			t := voided.Time
			r.VoidedAt = &t
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("xianguanjia iterate external cards: %w", err)
	}
	return out, nil
}

// ---- 内存实现（单测 / handler 测试用） ----

type externalCardStoreMem struct {
	mu   sync.Mutex
	data map[string]ExternalCardRow // key: orderNo|cardNo
}

// NewExternalCardStoreMemory 返回内存版卡密映射存储。
func NewExternalCardStoreMemory() *externalCardStoreMem {
	return &externalCardStoreMem{data: make(map[string]ExternalCardRow)}
}

func memCardKey(orderNo, cardNo string) string {
	return orderNo + "|" + cardNo
}

func (s *externalCardStoreMem) UpsertExternalCard(ctx context.Context, row ExternalCardRow) error {
	if s == nil {
		return fmt.Errorf("xianguanjia external card store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[memCardKey(row.OrderNo, row.CardNo)] = row
	return nil
}

func (s *externalCardStoreMem) GetExternalCards(ctx context.Context, orderNo string) ([]ExternalCardRow, error) {
	if s == nil {
		return nil, fmt.Errorf("xianguanjia external card store unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ExternalCardRow
	for _, r := range s.data {
		if r.OrderNo == orderNo {
			out = append(out, r)
		}
	}
	return out, nil
}

// EncryptCardPwd 占位加密：真实环境应接入仓库既有凭证加密（secretEncryptor）。
// 此处用随机 salt + base64 包裹，仅为避免明文落库；上线前必须替换。
func EncryptCardPwd(plain string) string {
	salt := randString(8)
	return base64.StdEncoding.EncodeToString([]byte(salt + ":" + plain))
}

// DecryptCardPwd 反向解出明文（与 EncryptCardPwd 配对，仅供落库后回读/测试用）。
func DecryptCardPwd(enc string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("xianguanjia decode card pwd: %w", err)
	}
	s := string(b)
	idx := strings.IndexByte(s, ':')
	if idx < 0 {
		return "", fmt.Errorf("xianguanjia card pwd format invalid")
	}
	return s[idx+1:], nil
}
