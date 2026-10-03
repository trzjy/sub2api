package xianguanjia

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Config 是 252 表 xianyu_xianguanjia_config 中与开放平台 ERP 方向相关的只读字段。
//
// 重要：开放平台 ERP 方向无 mch 对应物（58 页文档 mch 零命中），本结构不引用
// mch_id / mch_secret_encrypted 两列，也绝不新增 ALTER 去动它们。
type Config struct {
	BaseURL           string // 闲管家网关地址（默认 https://open.goofish.pro）
	AppID             string // 闲管家下发的 AppKey，作为签名 appKey
	AppSecretEncrypted string // 加密存储的 AppSecret，读取后用 SecretEncryptor 解密
	PushURL           string // 我方接收推送的地址（后台配置）
	Status            string // 配置状态：active / disabled 等
	HealthStatus      string // 健康检查状态
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
