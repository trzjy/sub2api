package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// saveWorkerConfigControlStub 最小化实现 XianyuControlRepository，仅覆盖
// SaveWorkerConfig / SetWorkerActive 触发到的方法，并暴露 captured cfg，
// 便于断言"健康字段不被覆盖"与"窄方法被使用"。
type saveWorkerConfigControlStub struct {
	configs  []XianyuWorkerConfig
	nextID   int64
	captured *XianyuWorkerConfig
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

// UpdateWorkerConfigUserFields 模拟真实 SQL 行为：只写 base_url 与 api_token_encrypted
// （token 留空时原地保留）；status / health_status / last_checked_at 保持 DB 原值。
func (s *saveWorkerConfigControlStub) UpdateWorkerConfigUserFields(_ context.Context, cfg XianyuWorkerConfig) (*XianyuWorkerConfig, error) {
	for i := range s.configs {
		if s.configs[i].ID == cfg.ID {
			s.configs[i].BaseURL = cfg.BaseURL
			if cfg.APITokenEncrypted != "" {
				s.configs[i].APITokenEncrypted = cfg.APITokenEncrypted
			}
			s.configs[i].UpdatedAt = time.Now()
			cp := s.configs[i]
			s.captured = &cp
			return &cp, nil
		}
	}
	return nil, ErrXianyuWorkerConfigNotFound
}

// UpdateWorkerHealth 模拟真实 SQL 行为：只写健康字段，不触碰用户字段。
func (s *saveWorkerConfigControlStub) UpdateWorkerHealth(_ context.Context, id int64, healthStatus string, lastCheckedAt time.Time) error {
	for i := range s.configs {
		if s.configs[i].ID == id {
			s.configs[i].HealthStatus = healthStatus
			s.configs[i].LastCheckedAt = &lastCheckedAt
			s.configs[i].UpdatedAt = time.Now()
			return nil
		}
	}
	return ErrXianyuWorkerConfigNotFound
}

// ActivateWorkerConfig 模拟真实 SQL 行为：只写 status + 新 token，不写 base_url / 健康字段。
func (s *saveWorkerConfigControlStub) ActivateWorkerConfig(_ context.Context, id int64, encryptedToken string) (*XianyuWorkerConfig, error) {
	for i := range s.configs {
		if s.configs[i].ID == id {
			s.configs[i].Status = XianyuWorkerStatusActive
			s.configs[i].APITokenEncrypted = encryptedToken
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

func TestXianyuListMethodsNormalizeNilSlices(t *testing.T) {
	stub := &saveWorkerConfigControlStub{configs: []XianyuWorkerConfig{sampleHealthyConfig()}}
	svc := newSaveWorkerConfigTestService(stub)

	accounts, err := svc.ListAccounts(context.Background())
	require.NoError(t, err)
	require.NotNil(t, accounts)
	require.Empty(t, accounts)

	products, err := svc.ListProducts(context.Background())
	require.NoError(t, err)
	require.NotNil(t, products)
	require.Empty(t, products)

	rules, err := svc.ListBindingRules(context.Background())
	require.NoError(t, err)
	require.NotNil(t, rules)
	require.Empty(t, rules)

	pools, err := svc.ListItemPools(context.Background())
	require.NoError(t, err)
	require.NotNil(t, pools)
	require.Empty(t, pools)
}

func TestXianyuListAccountsWithoutWorkerReturnsEmptySlice(t *testing.T) {
	svc := newSaveWorkerConfigTestService(&saveWorkerConfigControlStub{})
	accounts, err := svc.ListAccounts(context.Background())
	require.NoError(t, err)
	require.NotNil(t, accounts)
	require.Empty(t, accounts)
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

	require.Equal(t, XianyuWorkerStatusActive, updated.Status)
	require.Equal(t, "ENC:old-token", updated.APITokenEncrypted, "原加密 token 应保留")
	require.Equal(t, XianyuWorkerHealthHealthy, updated.HealthStatus)
	require.NotNil(t, updated.LastCheckedAt)
}

// 空库首次创建直接 active：并发重复创建由 uq_xianyu_worker_configs_active 部分唯一索引阻止
// （第二个 INSERT 命中唯一冲突 → 服务层映射 409）。
func TestSaveWorkerConfigCreateDefaultsActive(t *testing.T) {
	stub := &saveWorkerConfigControlStub{configs: nil}
	svc := newSaveWorkerConfigTestService(stub)

	created, err := svc.SaveWorkerConfig(context.Background(), XianyuWorkerConfig{
		BaseURL:           "http://xianyu-worker-backend:8089",
		APITokenEncrypted: "first-token",
		// Status 留空
	})
	require.NoError(t, err)
	require.Equal(t, XianyuWorkerStatusActive, created.Status)
	require.Equal(t, XianyuWorkerStatusActive, stub.configs[0].Status, "插入行应为 active，DB 部分唯一索引才能拦截并发重复")
	require.Equal(t, "ENC:first-token", stub.configs[0].APITokenEncrypted, "真实 token 应加密入库")
}

// SetWorkerActive 提供真实 token 后激活：窄写只动 status + 新 token，
// base_url 与健康字段保持 DB 原值（旧快照不得回滚并发 admin 保存）。
func TestSetWorkerActiveUsesUserFieldsUpdate(t *testing.T) {
	cfg := sampleHealthyConfig()
	cfg.Status = XianyuWorkerStatusDisabled
	stub := &saveWorkerConfigControlStub{configs: []XianyuWorkerConfig{cfg}}
	svc := newSaveWorkerConfigTestService(stub)

	updated, err := svc.SetWorkerActive(context.Background(), 1, "real-token")
	require.NoError(t, err)
	require.Equal(t, XianyuWorkerStatusActive, updated.Status)
	require.Equal(t, "ENC:real-token", stub.configs[0].APITokenEncrypted, "新 token 应加密入库")
	require.Equal(t, cfg.BaseURL, stub.configs[0].BaseURL, "激活窄写不得改动 base_url")
	require.Equal(t, XianyuWorkerHealthHealthy, stub.configs[0].HealthStatus, "健康字段不被窄方法覆盖")
	require.NotNil(t, stub.configs[0].LastCheckedAt)
}

// disabled→active 未提供 token → 拒绝且状态不变。
func TestSetWorkerActiveDisabledRequiresToken(t *testing.T) {
	cfg := sampleHealthyConfig()
	cfg.Status = XianyuWorkerStatusDisabled
	stub := &saveWorkerConfigControlStub{configs: []XianyuWorkerConfig{cfg}}
	svc := newSaveWorkerConfigTestService(stub)

	_, err := svc.SetWorkerActive(context.Background(), 1, "")
	require.Error(t, err)
	require.Equal(t, XianyuWorkerStatusDisabled, stub.configs[0].Status, "未授权激活，状态不得改动")
}

// 保存不得写 status：即便调用方提交 status='active'，disabled 配置仍保持 disabled（激活只走 /activate）。
func TestSaveWorkerConfigDoesNotChangeStatus(t *testing.T) {
	cfg := sampleHealthyConfig()
	cfg.Status = XianyuWorkerStatusDisabled
	stub := &saveWorkerConfigControlStub{configs: []XianyuWorkerConfig{cfg}}
	svc := newSaveWorkerConfigTestService(stub)

	updated, err := svc.SaveWorkerConfig(context.Background(), XianyuWorkerConfig{
		ID:                1,
		BaseURL:           "http://xianyu-worker-backend:9090",
		APITokenEncrypted: "new-token",
		Status:            XianyuWorkerStatusActive, // 客户端仍传 status 也必须被忽略
	})
	require.NoError(t, err)
	require.Equal(t, XianyuWorkerStatusDisabled, stub.configs[0].Status, "保存不得写 status")
	require.Equal(t, "http://xianyu-worker-backend:9090", stub.configs[0].BaseURL, "base_url 正常更新")
	require.Equal(t, "ENC:new-token", stub.configs[0].APITokenEncrypted, "新 token 加密入库")
	require.Equal(t, XianyuWorkerStatusDisabled, updated.Status, "返回实体 status 应为 DB 现值")
}

// ── 未触发的接口方法：返回零值即可 ──────────────────────────────

func (s *saveWorkerConfigControlStub) ListAccounts(context.Context, int64) ([]XianyuAccount, error) {
	return nil, nil
}
func (s *saveWorkerConfigControlStub) GetAccountByWorkerAndAccountID(context.Context, int64, string) (*XianyuAccount, error) {
	return nil, ErrXianyuAccountNotFound
}
func (s *saveWorkerConfigControlStub) UpsertAccount(context.Context, XianyuAccount) (*XianyuAccount, error) {
	return nil, nil
}
func (s *saveWorkerConfigControlStub) UpdateAccount(context.Context, XianyuAccount) (*XianyuAccount, error) {
	return nil, nil
}
func (s *saveWorkerConfigControlStub) ListItemPools(context.Context) ([]XianyuItemPool, error) {
	return nil, nil
}
func (s *saveWorkerConfigControlStub) GetItemPoolByID(context.Context, int64) (*XianyuItemPool, error) {
	return nil, ErrXianyuItemPoolNotFound
}
func (s *saveWorkerConfigControlStub) GetItemPoolBySlug(context.Context, string) (*XianyuItemPool, error) {
	return nil, ErrXianyuItemPoolNotFound
}
func (s *saveWorkerConfigControlStub) CreateItemPool(context.Context, XianyuItemPool) (*XianyuItemPool, error) {
	return nil, nil
}
func (s *saveWorkerConfigControlStub) UpdateItemPool(context.Context, XianyuItemPool) (*XianyuItemPool, error) {
	return nil, nil
}
func (s *saveWorkerConfigControlStub) PoolStockCounts(context.Context, string) (int, int, int, error) {
	return 0, 0, 0, nil
}
func (s *saveWorkerConfigControlStub) DeliveryStats(context.Context, time.Time) (int, int, error) {
	return 0, 0, nil
}
func (s *saveWorkerConfigControlStub) PendingDeliveryCount(context.Context) (int, error) {
	return 0, nil
}
func (s *saveWorkerConfigControlStub) ListProducts(context.Context) ([]XianyuProduct, error) {
	return nil, nil
}
func (s *saveWorkerConfigControlStub) ListProductsByAccount(context.Context, int64) ([]XianyuProduct, error) {
	return nil, nil
}
func (s *saveWorkerConfigControlStub) GetProductByIdentity(context.Context, int64, string, string, string) (*XianyuProduct, error) {
	return nil, ErrXianyuProductNotFound
}
func (s *saveWorkerConfigControlStub) UpsertProduct(context.Context, XianyuProduct) (*XianyuProduct, error) {
	return nil, nil
}
func (s *saveWorkerConfigControlStub) UpdateProduct(context.Context, XianyuProduct) (*XianyuProduct, error) {
	return nil, nil
}
func (s *saveWorkerConfigControlStub) UpdateProductBinding(context.Context, int64, string, string, *int64) error {
	return nil
}
func (s *saveWorkerConfigControlStub) ListBindingRules(context.Context) ([]XianyuBindingRule, error) {
	return nil, nil
}
func (s *saveWorkerConfigControlStub) CreateBindingRule(context.Context, XianyuBindingRule) (*XianyuBindingRule, error) {
	return nil, nil
}
func (s *saveWorkerConfigControlStub) UpdateBindingRule(context.Context, XianyuBindingRule) (*XianyuBindingRule, error) {
	return nil, nil
}
