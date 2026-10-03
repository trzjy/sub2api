package xianguanjia

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// D6a: 货源提卡方向（闲管家 → 我方）凭证配置。
//
// 与 ERP 方向（我方 → 闲管家，见 config.go 的 Config/configStoreDB）相反：
// 货源方向我方是被调方，闲管家用「六段签名」调我方 /api/v1/xgj-supply/* 接口，
// 我方在验签中间件（D6b）里需要 supply_app_secret 与 mch_secret 两个密钥。
//
// 存储选型（详见交付说明与 /root/d6-evidence/d6a.md）：
//   - mch_id / mch_secret_encrypted —— 复用 252 表 xianyu_xianguanjia_config
//     既有列。这两列自 252 建表（后被 revert）起即存在但零引用，货源方向
//     「货源授权商户」恰好是其对应物，无需新增。
//   - supply_app_id / supply_app_secret_encrypted —— 252 表无对应列，新增
//     迁移 268_xianguanjia_supply.sql 以 ALTER TABLE 追加两列。理由：两列与
//     现有 ERP 凭证同属「闲管家单行配置」，放同一行可保证「同一次保存、同一
//     事务、同一 active 行」的一致性，且 admin GET/PUT 可一次读写；相比
//     settings KV（无类型、无结构、易与业务键混淆）更贴合既有 252 表的
//     单行配置语义。
//
// 两方向共用 252 表的同一条 active 行（status='active' 的部分唯一索引
// uq_xianyu_xianguanjia_config_active 保证单行）。本文件只写货源方向四列，
// 绝不触碰 ERP 方向列（base_url/app_id/app_secret_encrypted/push_url 等）。
//
// 明文凭证只在内存中出现：Save 时经 SupplySecretEncryptor 加密后落库，
// Get 时解密返回；落库列名统一 *_encrypted。真实凭证绝不写入本文件/测试/证据。

// DefaultSupplyGatewayPath 是货源被调接口的网关前缀（闲管家 → 我方）。
// 与总单契约一致：POST https://corealgos.com/api/v1/xgj-supply/<接口>。
const DefaultSupplyGatewayPath = "/api/v1/xgj-supply"

// ErrSupplyNoConfig 表示货源凭证未配置（无 active 行，或四要素存在缺失）。
// 调用方（D6b 验签中间件 / admin）据此 fail-closed。
var ErrSupplyNoConfig = errors.New("xianguanjia: no supply config")

// SupplyConfig 是货源方向的完整凭证（返回时 secret 字段为解密后明文）。
type SupplyConfig struct {
	SupplyAppID     string // 闲管家下发的应用 AppKey（整数型字符串，六段签名用）
	SupplyAppSecret string // 解密后的应用 AppSecret
	MchID           string // 货源授权商户号（我方自造）
	MchSecret       string // 解密后的货源授权密钥
	GatewayPath     string // 网关前缀，默认 DefaultSupplyGatewayPath
}

// SupplySecretEncryptor 是加密器窄接口。仓库既有的 service.SecretEncryptor
// （Encrypt/Decrypt 同签名）结构上直接满足，无需适配器，也避免 service 与
// service/xianguanjia 之间导入成环。
type SupplySecretEncryptor interface {
	Encrypt(plaintext string) (string, error)
	Decrypt(ciphertext string) (string, error)
}

// SupplyConfigStore 读写 252 表 active 行的货源方向凭证。
type SupplyConfigStore struct {
	db      *sql.DB
	encrypt SupplySecretEncryptor
}

// NewSupplyConfigStore 构造货源凭证存储。db / encrypt 任一为 nil 时各方法
// fail-closed 返回错误。
func NewSupplyConfigStore(db *sql.DB, encrypt SupplySecretEncryptor) *SupplyConfigStore {
	return &SupplyConfigStore{db: db, encrypt: encrypt}
}

// Get 读取 active 行的货源凭证并解密返回。
// 无 active 行、或四要素（supply_app_id / supply_app_secret / mch_id / mch_secret）
// 任一缺失时返回 ErrSupplyNoConfig（部分配置视为未配置，fail-closed）。
// 密文存在但解密失败返回包装错误（区别于「未配置」）。
func (s *SupplyConfigStore) Get(ctx context.Context) (*SupplyConfig, error) {
	if s == nil || s.db == nil || s.encrypt == nil {
		return nil, fmt.Errorf("xianguanjia supply config store unavailable")
	}
	var (
		supplyAppID, supplyAppSecretEnc string
		mchID, mchSecretEnc             string
	)
	row := s.db.QueryRowContext(ctx, `
		SELECT supply_app_id, supply_app_secret_encrypted, mch_id, mch_secret_encrypted
		FROM xianyu_xianguanjia_config
		WHERE status = 'active' LIMIT 1`)
	if err := row.Scan(&supplyAppID, &supplyAppSecretEnc, &mchID, &mchSecretEnc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSupplyNoConfig
		}
		return nil, fmt.Errorf("xianguanjia scan supply config: %w", err)
	}
	// 部分字段缺失：视为未配置（验签缺任一密钥都无法工作）。
	if strings.TrimSpace(supplyAppID) == "" || strings.TrimSpace(mchID) == "" ||
		strings.TrimSpace(supplyAppSecretEnc) == "" || strings.TrimSpace(mchSecretEnc) == "" {
		return nil, ErrSupplyNoConfig
	}
	supplyAppSecret, err := s.encrypt.Decrypt(supplyAppSecretEnc)
	if err != nil || supplyAppSecret == "" {
		return nil, fmt.Errorf("xianguanjia decrypt supply app secret: %w", err)
	}
	mchSecret, err := s.encrypt.Decrypt(mchSecretEnc)
	if err != nil || mchSecret == "" {
		return nil, fmt.Errorf("xianguanjia decrypt mch secret: %w", err)
	}
	return &SupplyConfig{
		SupplyAppID:     supplyAppID,
		SupplyAppSecret: supplyAppSecret,
		MchID:           mchID,
		MchSecret:       mchSecret,
		GatewayPath:     DefaultSupplyGatewayPath,
	}, nil
}

// Save 加密写入货源方向四列到 active 单行。
//
// 语义：
//   - 只写 mch_id / mch_secret_encrypted / supply_app_id / supply_app_secret_encrypted
//     四列，不触碰 ERP 方向列；无 active 行时插入一条 status='active' 的行。
//   - 非空 secret 用 encryptor 加密后落库；secret 留空表示保留已存密文
//     （部分更新，对齐 admin PUT 的「留空保留」惯例）；首次保存且无已存密文
//     时留空报错。
func (s *SupplyConfigStore) Save(ctx context.Context, cfg SupplyConfig) error {
	if s == nil || s.db == nil || s.encrypt == nil {
		return fmt.Errorf("xianguanjia supply config store unavailable")
	}
	supplyAppID := strings.TrimSpace(cfg.SupplyAppID)
	mchID := strings.TrimSpace(cfg.MchID)
	supplySecretPlain := strings.TrimSpace(cfg.SupplyAppSecret)
	mchSecretPlain := strings.TrimSpace(cfg.MchSecret)
	if supplyAppID == "" || mchID == "" {
		return fmt.Errorf("xianguanjia save supply config: app_id and mch_id are required")
	}

	existing, err := s.loadActiveEncrypted(ctx)
	if err != nil {
		return err
	}
	supplySecretEnc := ""
	mchSecretEnc := ""
	if existing != nil {
		supplySecretEnc = existing.supplyAppSecretEnc
		mchSecretEnc = existing.mchSecretEnc
	}
	if supplySecretPlain != "" {
		enc, encErr := s.encrypt.Encrypt(supplySecretPlain)
		if encErr != nil {
			return fmt.Errorf("xianguanjia encrypt supply app secret: %w", encErr)
		}
		supplySecretEnc = enc
	}
	if mchSecretPlain != "" {
		enc, encErr := s.encrypt.Encrypt(mchSecretPlain)
		if encErr != nil {
			return fmt.Errorf("xianguanjia encrypt mch secret: %w", encErr)
		}
		mchSecretEnc = enc
	}
	if supplySecretEnc == "" || mchSecretEnc == "" {
		return fmt.Errorf("xianguanjia save supply config: secrets are required on first save")
	}

	// 先 UPDATE active 行（保留 ERP 列原值，仅覆盖货源四列）。
	res, err := s.db.ExecContext(ctx, `
		UPDATE xianyu_xianguanjia_config
		SET supply_app_id = $1, supply_app_secret_encrypted = $2,
		    mch_id = $3, mch_secret_encrypted = $4, updated_at = now()
		WHERE status = 'active'`,
		supplyAppID, supplySecretEnc, mchID, mchSecretEnc)
	if err != nil {
		return fmt.Errorf("xianguanjia update supply config: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	// 无 active 行：插入单行（status='active'；ERP 列留默认空值）。
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO xianyu_xianguanjia_config
			(supply_app_id, supply_app_secret_encrypted, mch_id, mch_secret_encrypted,
			 status, health_status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'active', 'unknown', now(), now())`,
		supplyAppID, supplySecretEnc, mchID, mchSecretEnc); err != nil {
		return fmt.Errorf("xianguanjia insert supply config: %w", err)
	}
	return nil
}

// activeEncrypted 是 active 行中货源方向四列的密文快照。
type activeEncrypted struct {
	supplyAppSecretEnc string
	mchSecretEnc       string
}

// loadActiveEncrypted 读取 active 行已存的密文（用于 Save 的「留空保留」语义）。
// 无 active 行返回 (nil, nil)。
func (s *SupplyConfigStore) loadActiveEncrypted(ctx context.Context) (*activeEncrypted, error) {
	var supplySecretEnc, mchSecretEnc string
	err := s.db.QueryRowContext(ctx, `
		SELECT supply_app_secret_encrypted, mch_secret_encrypted
		FROM xianyu_xianguanjia_config WHERE status = 'active' LIMIT 1`).
		Scan(&supplySecretEnc, &mchSecretEnc)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("xianguanjia load supply config: %w", err)
	}
	return &activeEncrypted{supplyAppSecretEnc: supplySecretEnc, mchSecretEnc: mchSecretEnc}, nil
}
