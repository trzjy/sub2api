package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// saveWorkerConfigControlStub 最小化实现 XianyuControlRepository，仅覆盖
// SaveWorkerConfig / SetWorkerActive 触发到的 5 个方法，并暴露 full-update
// 调用计数与 captured cfg，便于断言"健康字段不被覆盖"与"窄方法被使用"。
type saveWorkerConfigControlStub struct {
	configs          []XianyuWorkerConfig
	nextID           int64
	captured         *XianyuWorkerConfig
	fullUpdateCalled bool
}

func (s *saveWorkerConfigControlStub) ListWorkerConfigs(context.Context) ([]XianyuWorkerConfig, error) {
	return s.configs, nil
}

func (s *saveWorkerConfigControlStub) GetWorkerConfigByID(_ context.Context, id int64) (*XianyuWorkerConfig, error) {
	for i := range s.configs {
		if s.configs[i].ID == id {
			// 返回拷贝，避免上层修改影响断言。
			cp := s.configs[i]
			return &cp, nil
		}
	}
	return nil, ErrXianyuWorkerConfigNotFound
}

func (s *saveWorkerConfigControlStub) CreateWorkerConfig(_ context.Context, cfg XianyuWorkerConfig) (*XianyuWorkerConfig, error) {
	s.nextID++
	cfg.ID = s.nextID
	if cfg.HealthStatus == "" {
		cfg.HealthStatus = XianyuWorkerHealthUnknown
	}
	s.configs = append(s.configs, cfg)
	cp := cfg
	return &cp, nil
}

// UpdateWorkerConfig 全字段 UPDATE；窄方法不应走到此函数（admin save 走 UpdateWorkerConfigUserFields）。
func (s *saveWorkerConfigControlStub) UpdateWorkerConfig(_ context.Context, cfg XianyuWorkerConfig) (*XianyuWorkerConfig, error) {
	s.fullUpdateCalled = true
	for i := range s.configs {
		if s.configs[i].ID == cfg.ID {
			s.configs[i] = cfg
			cp := cfg
			s.captured = &cp
			return &cp, nil
		}
	}
	return nil, ErrXianyuWorkerConfigNotFound
}

// UpdateWorkerConfigUserFields 模拟真实 SQL 行为：只写 base_url / api_token_encrypted / status；
// health_status 与 last_checked_at 保留 DB 原值。
func (s *saveWorkerConfigControlStub) UpdateWorkerConfigUserFields(_ context.Context, cfg XianyuWorkerConfig) (*XianyuWorkerConfig, error) {
	for i := range s.configs {
		if s.configs[i].ID == cfg.ID {
			s.configs[i].BaseURL = cfg.BaseURL
			s.configs[i].APITokenEncrypted = cfg.APITokenEncrypted
			s.configs[i].Status = cfg.Status
			s.configs[i].UpdatedAt = time.Now()
			cp := s.configs[i]
			s.captured = &cp
			return &cp, nil
		}
	}
	return nil, ErrXianyuWorkerConfigNotFound
}

func (s *saveWorkerConfigControlStub) GetActiveWorkerConfig(context.Context) (*XianyuWorkerConfig, error) {
	for i := range s.configs {
		if s.configs[i].Status == XianyuWorkerStatusActive {
			cp := s.configs[i]
			return &cp, nil
		}
	}
	return nil, ErrXianyuWorkerConfigNotFound
}

// testEncryptor 是 SaveWorkerConfig 用的恒等 SecretEncryptor。
type testEncryptor struct{}

func (testEncryptor) Encrypt(plaintext string) (string, error) { return "ENC:" + plaintext, nil }
func (testEncryptor) Decrypt(ciphertext string) (string, error) {
	if len(ciphertext) > 4 && ciphertext[:4] == "ENC:" {
		return ciphertext[4:], nil
	}
	return ciphertext, nil
}

func newSaveWorkerConfigTestService(stub *saveWorkerConfigControlStub) *XianyuControlService {
	return NewXianyuControlService(
		stub, nil, nil, nil, testEncryptor{},
		nil, nil, nil, nil, nil,
	)
}

func sampleHealthyConfig() XianyuWorkerConfig {
	now := time.Now()
	return XianyuWorkerConfig{
		ID:                1,
		BaseURL:           "http://xianyu-worker-backend:8089",
		APITokenEncrypted: "ENC:old-token",
		Status:            XianyuWorkerStatusActive,
		HealthStatus:      XianyuWorkerHealthHealthy,
		LastCheckedAt:     &now,
	}
}

// Fix 1 + Fix 2：token 存在时更新，status 留空必须保留旧值；健康字段必须不被覆盖。
func TestSaveWorkerConfigTokenUpdatePreservesHealth(t *testing.T) {
	stub := &saveWorkerConfigControlStub{configs: []XianyuWorkerConfig{sampleHealthyConfig()}}
	svc := newSaveWorkerConfigTestService(stub)

	// 调用方提交：id=1、新地址、新 token、无 status 字段
	updated, err := svc.SaveWorkerConfig(context.Background(), XianyuWorkerConfig{
		ID:                1,
		BaseURL:           "http://xianyu-worker-backend:9090",
		APITokenEncrypted: "new-token",
		// Status 留空
	})
	require.NoError(t, err)

	// admin save 绝不能走全字段 UPDATE。
	require.False(t, stub.fullUpdateCalled, "SaveWorkerConfig 应走 UpdateWorkerConfigUserFields 窄方法")
	// 窄方法返回的实体健康字段应保留 DB 原值。
	require.Equal(t, XianyuWorkerHealthHealthy, updated.HealthStatus)
	require.NotNil(t, updated.LastCheckedAt)
	// status 留空 → 保留 existing.Status（active），而非默认 disabled。
	require.Equal(t, XianyuWorkerStatusActive, updated.Status)
	// DB 中实际行也未丢失健康字段。
	require.Equal(t, XianyuWorkerHealthHealthy, stub.configs[0].HealthStatus)
	require.NotNil(t, stub.configs[0].LastCheckedAt)
	// token 已加密入库。
	require.Equal(t, "ENC:new-token", stub.configs[0].APITokenEncrypted)
}

// Fix 1：留空 token 路径不应再回填健康字段（窄方法天然保留），且 status 留空保留 existing。
func TestSaveWorkerConfigEmptyTokenPreservesHealthAndStatus(t *testing.T) {
	stub := &saveWorkerConfigControlStub{configs: []XianyuWorkerConfig{sampleHealthyConfig()}}
	svc := newSaveWorkerConfigTestService(stub)

	updated, err := svc.SaveWorkerConfig(context.Background(), XianyuWorkerConfig{
		ID:      1,
		BaseURL: "http://xianyu-worker-backend:9090",
		// APITokenEncrypted 留空
		// Status 留空
	})
	require.NoError(t, err)

	require.False(t, stub.fullUpdateCalled, "留空 token 路径也应走窄方法")
	require.Equal(t, XianyuWorkerStatusActive, updated.Status)
	require.Equal(t, "ENC:old-token", updated.APITokenEncrypted, "原加密 token 应保留")
	require.Equal(t, XianyuWorkerHealthHealthy, updated.HealthStatus)
	require.NotNil(t, updated.LastCheckedAt)
}

// 回归保护：创建路径上 status 留空应默认 disabled（不是 active）。
func TestSaveWorkerConfigCreateEmptyStatusDefaultsDisabled(t *testing.T) {
	stub := &saveWorkerConfigControlStub{configs: nil}
	svc := newSaveWorkerConfigTestService(stub)

	created, err := svc.SaveWorkerConfig(context.Background(), XianyuWorkerConfig{
		BaseURL:           "http://xianyu-worker-backend:8089",
		APITokenEncrypted: "first-token",
		// Status 留空
	})
	require.NoError(t, err)
	require.Equal(t, XianyuWorkerStatusDisabled, created.Status)
	require.False(t, stub.fullUpdateCalled, "创建路径走 CreateWorkerConfig，不应触发 UpdateWorkerConfig 全字段")
}

// SetWorkerActive 改用窄方法，健康字段保留。
func TestSetWorkerActiveUsesUserFieldsUpdate(t *testing.T) {
	cfg := sampleHealthyConfig()
	cfg.Status = XianyuWorkerStatusDisabled
	stub := &saveWorkerConfigControlStub{configs: []XianyuWorkerConfig{cfg}}
	svc := newSaveWorkerConfigTestService(stub)

	updated, err := svc.SetWorkerActive(context.Background(), 1)
	require.NoError(t, err)
	require.False(t, stub.fullUpdateCalled, "SetWorkerActive 应走窄方法")
	require.Equal(t, XianyuWorkerStatusActive, updated.Status)
	require.Equal(t, XianyuWorkerHealthHealthy, stub.configs[0].HealthStatus)
	require.NotNil(t, stub.configs[0].LastCheckedAt)
}

// ── 未触发的接口方法：返回零值即可 ──────────────────────────────

func (s *saveWorkerConfigControlStub) ListAccounts(context.Context, int64) ([]XianyuAccount, error) { return nil, nil }
func (s *saveWorkerConfigControlStub) GetAccountByWorkerAndAccountID(context.Context, int64, string) (*XianyuAccount, error) {
	return nil, ErrXianyuAccountNotFound
}
func (s *saveWorkerConfigControlStub) UpsertAccount(context.Context, XianyuAccount) (*XianyuAccount, error) { return nil, nil }
func (s *saveWorkerConfigControlStub) UpdateAccount(context.Context, XianyuAccount) (*XianyuAccount, error) { return nil, nil }
func (s *saveWorkerConfigControlStub) ListItemPools(context.Context) ([]XianyuItemPool, error)           { return nil, nil }
func (s *saveWorkerConfigControlStub) GetItemPoolByID(context.Context, int64) (*XianyuItemPool, error) {
	return nil, ErrXianyuItemPoolNotFound
}
func (s *saveWorkerConfigControlStub) GetItemPoolBySlug(context.Context, string) (*XianyuItemPool, error) {
	return nil, ErrXianyuItemPoolNotFound
}
func (s *saveWorkerConfigControlStub) CreateItemPool(context.Context, XianyuItemPool) (*XianyuItemPool, error) { return nil, nil }
func (s *saveWorkerConfigControlStub) UpdateItemPool(context.Context, XianyuItemPool) (*XianyuItemPool, error) { return nil, nil }
func (s *saveWorkerConfigControlStub) PoolStockCounts(context.Context, string) (int, int, int, error)         { return 0, 0, 0, nil }
func (s *saveWorkerConfigControlStub) DeliveryStats(context.Context, time.Time) (int, int, error)             { return 0, 0, nil }
func (s *saveWorkerConfigControlStub) PendingDeliveryCount(context.Context) (int, error)                      { return 0, nil }
func (s *saveWorkerConfigControlStub) ListProducts(context.Context) ([]XianyuProduct, error)                 { return nil, nil }
func (s *saveWorkerConfigControlStub) ListProductsByAccount(context.Context, int64) ([]XianyuProduct, error) { return nil, nil }
func (s *saveWorkerConfigControlStub) GetProductByIdentity(context.Context, int64, string, string, string) (*XianyuProduct, error) {
	return nil, ErrXianyuProductNotFound
}
func (s *saveWorkerConfigControlStub) UpsertProduct(context.Context, XianyuProduct) (*XianyuProduct, error) { return nil, nil }
func (s *saveWorkerConfigControlStub) UpdateProduct(context.Context, XianyuProduct) (*XianyuProduct, error) { return nil, nil }
func (s *saveWorkerConfigControlStub) UpdateProductBinding(context.Context, int64, string, string, *int64) error {
	return nil
}
func (s *saveWorkerConfigControlStub) ListBindingRules(context.Context) ([]XianyuBindingRule, error) { return nil, nil }
func (s *saveWorkerConfigControlStub) CreateBindingRule(context.Context, XianyuBindingRule) (*XianyuBindingRule, error) {
	return nil, nil
}
func (s *saveWorkerConfigControlStub) UpdateBindingRule(context.Context, XianyuBindingRule) (*XianyuBindingRule, error) {
	return nil, nil
}
