package service

import (
	"context"
	"time"
)

// XianGuanJiaConfig 是主程序对闲管家开放平台配置的视图。
type XianGuanJiaConfig struct {
	ID                 int64      `json:"id"`
	BaseURL            string     `json:"base_url"`
	AppID              string     `json:"app_id"`
	AppSecretEncrypted string     `json:"app_secret_encrypted"`
	MchID              string     `json:"mch_id"`
	MchSecretEncrypted string     `json:"mch_secret_encrypted"`
	PushURL            string     `json:"push_url"`
	Status             string     `json:"status"`        // active / disabled
	HealthStatus       string     `json:"health_status"` // unknown / healthy / unhealthy
	LastCheckedAt      *time.Time `json:"last_checked_at"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

const (
	XianGuanJiaConfigStatusActive   = "active"
	XianGuanJiaConfigStatusDisabled = "disabled"

	XianGuanJiaHealthUnknown   = "unknown"
	XianGuanJiaHealthHealthy   = "healthy"
	XianGuanJiaHealthUnhealthy = "unhealthy"
)

// XianGuanJiaRepository 提供闲管家配置的数据访问。
type XianGuanJiaRepository interface {
	GetActiveConfig(ctx context.Context) (*XianGuanJiaConfig, error)
	CreateConfig(ctx context.Context, cfg XianGuanJiaConfig) (*XianGuanJiaConfig, error)
	UpdateConfig(ctx context.Context, cfg XianGuanJiaConfig) (*XianGuanJiaConfig, error)
	ListConfigs(ctx context.Context) ([]XianGuanJiaConfig, error)
}

// XianGuanJiaConfigService 提供闲管家配置管理（admin 面板 + 客户端构建）。
type XianGuanJiaConfigService struct {
	repo      XianGuanJiaRepository
	encryptor SecretEncryptor
}

func NewXianGuanJiaConfigService(repo XianGuanJiaRepository, encryptor SecretEncryptor) *XianGuanJiaConfigService {
	return &XianGuanJiaConfigService{repo: repo, encryptor: encryptor}
}

// GetActiveConfig 读取当前活跃配置。
func (s *XianGuanJiaConfigService) GetActiveConfig(ctx context.Context) (*XianGuanJiaConfig, error) {
	return s.repo.GetActiveConfig(ctx)
}

// SaveConfig 创建或更新闲管家配置（支持首次创建后激活）。
func (s *XianGuanJiaConfigService) SaveConfig(ctx context.Context, cfg XianGuanJiaConfig) (*XianGuanJiaConfig, error) {
	if cfg.ID == 0 {
		return s.repo.CreateConfig(ctx, cfg)
	}
	return s.repo.UpdateConfig(ctx, cfg)
}

// BuildClient 用当前活跃配置构建 XianGuanJiaClient 实例。
// 配置不存在或已禁用时返回 nil，调用方应按"未配置"处理。
func (s *XianGuanJiaConfigService) BuildClient(ctx context.Context) (*XianGuanJiaClient, error) {
	cfg, err := s.repo.GetActiveConfig(ctx)
	if err != nil {
		return nil, err
	}
	if cfg.Status != XianGuanJiaConfigStatusActive {
		return nil, nil
	}
	secret, err := s.encryptor.Decrypt(cfg.AppSecretEncrypted)
	if err != nil {
		return nil, err
	}
	mchSecret := ""
	if cfg.MchSecretEncrypted != "" {
		mchSecret, err = s.encryptor.Decrypt(cfg.MchSecretEncrypted)
		if err != nil {
			return nil, err
		}
	}
	return NewXianGuanJiaClient(cfg.BaseURL, cfg.AppID, secret, cfg.MchID, mchSecret, 0), nil
}

// RefreshHealth 检查闲管家开放平台连通性并落库。
func (s *XianGuanJiaConfigService) RefreshHealth(ctx context.Context) error {
	client, err := s.BuildClient(ctx)
	if err != nil || client == nil {
		return err
	}
	_, err = client.Health(ctx)
	now := time.Now()
	cfg, cfgErr := s.repo.GetActiveConfig(ctx)
	if cfgErr != nil {
		return cfgErr
	}
	cfg.LastCheckedAt = &now
	if err != nil {
		cfg.HealthStatus = XianGuanJiaHealthUnhealthy
	} else {
		cfg.HealthStatus = XianGuanJiaHealthHealthy
	}
	_, updateErr := s.repo.UpdateConfig(ctx, *cfg)
	if updateErr != nil {
		return updateErr
	}
	return err
}
