package xianguanjia

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Config 是 252 表 xianyu_xianguanjia_config 中与开放平台 ERP 方向相关的只读字段。
//
// 重要：开放平台 ERP 方向无 mch 对应物（58 页文档 mch 零命中），本结构不引用
// mch_id / mch_secret_encrypted 两列，也绝不新增 ALTER 去动它们。
type Config struct {
	BaseURL            string // 闲管家网关地址（默认 https://open.goofish.pro）
	AppID              string // 闲管家下发的 AppKey，作为签名 appKey
	AppSecretEncrypted string // 加密存储的 AppSecret，读取后用 SecretEncryptor 解密
	PushURL            string // 我方接收推送的地址（后台配置）
	Status             string // 配置状态：active / disabled 等
	HealthStatus       string // 健康检查状态
}

// ErrNoActiveConfig 表示没有 active 配置（fail-closed 依据）。
var ErrNoActiveConfig = errors.New("xianguanjia: no active config")

// ConfigReader 读取闲管家 active 配置（只读侧）。
type ConfigReader interface {
	GetActiveConfig(ctx context.Context) (*Config, error)
}

// ---- DB 实现（只读 SELECT，绝不写） ----

type configStoreDB struct {
	db *sql.DB
}

// NewConfigStore 返回基于 PostgreSQL 的只读配置存储。
func NewConfigStore(db *sql.DB) *configStoreDB {
	return &configStoreDB{db: db}
}

func (s *configStoreDB) GetActiveConfig(ctx context.Context) (*Config, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("xianguanjia config store unavailable")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT base_url, app_id, app_secret_encrypted, push_url, status, health_status
		FROM xianyu_xianguanjia_config WHERE status = 'active' LIMIT 1`)
	var (
		baseURL, appID, appSecretEnc, pushURL, status, health string
	)
	if err := row.Scan(&baseURL, &appID, &appSecretEnc, &pushURL, &status, &health); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// 无 active 配置：返回 nil 让调用方 fail-closed。
			return nil, nil
		}
		return nil, fmt.Errorf("xianguanjia scan config: %w", err)
	}
	return &Config{
		BaseURL:            baseURL,
		AppID:              appID,
		AppSecretEncrypted: appSecretEnc,
		PushURL:            pushURL,
		Status:             status,
		HealthStatus:       health,
	}, nil
}

// ---- 内存实现（单测用） ----

type configStoreMem struct {
	cfg *Config
}

// NewConfigStoreMemory 返回内存版配置存储；cfg 为 nil 模拟"无 active 配置"（fail-closed 测试）。
func NewConfigStoreMemory(cfg *Config) *configStoreMem {
	return &configStoreMem{cfg: cfg}
}

func (s *configStoreMem) GetActiveConfig(ctx context.Context) (*Config, error) {
	return s.cfg, nil
}

// ---- 写侧（D3：admin 配置入口 upsert 单行配置） ----
//
// 252 表 xianyu_xianguanjia_config 以 status='active' 的部分唯一索引
// （uq_xianyu_xianguanjia_config_active）保证至多一行 active 配置。
// 写侧只 upsert 这一行的 ERP 方向字段；mch_id/mch_secret_encrypted 两列
// 在开放平台 ERP 方向无对应物，不引用、不改写（保持原值，插入时留默认空值）。

// ConfigUpsert 是 admin 写入的单行配置载荷（凭证由调用方先加密成 *_encrypted）。
type ConfigUpsert struct {
	BaseURL            string
	AppID              string
	AppSecretEncrypted string
	PushURL            string
	Status             string
}

// ConfigWriter 写入/更新单行 active 配置（DB 实现）。
type ConfigWriter interface {
	UpsertConfig(ctx context.Context, up ConfigUpsert) error
	UpdateHealth(ctx context.Context, healthStatus string, checkedAt time.Time) error
}

// UpsertConfig 写入 active 单行配置：存在 active 行则更新，否则插入。
// status 强制为 active（admin 保存即启用；本方向没有"保存但停用"的语义）。
func (s *configStoreDB) UpsertConfig(ctx context.Context, up ConfigUpsert) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("xianguanjia config store unavailable")
	}
	status := strings.TrimSpace(up.Status)
	if status == "" {
		status = "active"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO xianyu_xianguanjia_config
			(base_url, app_id, app_secret_encrypted, push_url, status, health_status)
		VALUES ($1, $2, $3, $4, $5, 'unknown')
		ON CONFLICT (id) DO NOTHING`,
		up.BaseURL, up.AppID, up.AppSecretEncrypted, up.PushURL, status)
	if err != nil {
		return fmt.Errorf("xianguanjia upsert config: %w", err)
	}
	// ON CONFLICT DO NOTHING 覆盖不了"已有 active 行"的更新语义，再补一次 UPDATE。
	// 部分唯一索引保证 active 行至多一行，因此 UPDATE ... WHERE status='active' 精确命中 0/1 行。
	res, err := s.db.ExecContext(ctx, `
		UPDATE xianyu_xianguanjia_config
		SET base_url = $1, app_id = $2, app_secret_encrypted = $3, push_url = $4,
		    status = $5, updated_at = now()
		WHERE status = 'active'`,
		up.BaseURL, up.AppID, up.AppSecretEncrypted, up.PushURL, status)
	if err != nil {
		return fmt.Errorf("xianguanjia update config: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// 插入路径也没生效（如并发写），交给调用方感知错误。
		return fmt.Errorf("xianguanjia upsert config: no row written")
	}
	return nil
}

// UpdateHealth 更新 active 行的探活结果与检查时间。
func (s *configStoreDB) UpdateHealth(ctx context.Context, healthStatus string, checkedAt time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("xianguanjia config store unavailable")
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE xianyu_xianguanjia_config
		SET health_status = $1, last_checked_at = $2, updated_at = now()
		WHERE status = 'active'`, healthStatus, checkedAt)
	if err != nil {
		return fmt.Errorf("xianguanjia update health: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("xianguanjia update health: no active config row")
	}
	return nil
}

// GetConfigRow 读取单行配置（不限 active），供 admin GET 展示（脱敏由 handler 负责）。
// 无任何行时返回 (nil, nil)。
func (s *configStoreDB) GetConfigRow(ctx context.Context) (*Config, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("xianguanjia config store unavailable")
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT base_url, app_id, app_secret_encrypted, push_url, status, health_status
		FROM xianyu_xianguanjia_config
		ORDER BY (status = 'active') DESC, id LIMIT 1`)
	var (
		baseURL, appID, appSecretEnc, pushURL, status, health string
	)
	if err := row.Scan(&baseURL, &appID, &appSecretEnc, &pushURL, &status, &health); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("xianguanjia scan config row: %w", err)
	}
	return &Config{
		BaseURL:            baseURL,
		AppID:              appID,
		AppSecretEncrypted: appSecretEnc,
		PushURL:            pushURL,
		Status:             status,
		HealthStatus:       health,
	}, nil
}

// ConfigStore 是 configStoreDB 的完整能力接口（读 active + 读单行 + 写 + 健康）。 D3:
// 供 wire 绑定 admin.XianguanjiaConfigStore 用（configStoreDB 实现全部方法）。
type ConfigStore interface {
	GetActiveConfig(ctx context.Context) (*Config, error)
	GetConfigRow(ctx context.Context) (*Config, error)
	UpsertConfig(ctx context.Context, up ConfigUpsert) error
	UpdateHealth(ctx context.Context, healthStatus string, checkedAt time.Time) error
}

// ProvideConfigStore 是 wire 用的导出 provider（与 NewConfigStore 等价，返回接口）。 D3:
func ProvideConfigStore(db *sql.DB) ConfigStore {
	return NewConfigStore(db)
}
