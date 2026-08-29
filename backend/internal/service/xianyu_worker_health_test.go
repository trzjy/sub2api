package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type xianyuWorkerControlStub struct {
	cfg       *XianyuWorkerConfig
	updated   *XianyuWorkerConfig
	updateErr error
}

type testSecretEncryptor struct{}

func (testSecretEncryptor) Encrypt(plaintext string) (string, error) {
	return "ENC:" + plaintext, nil
}

func (testSecretEncryptor) Decrypt(ciphertext string) (string, error) {
	if len(ciphertext) > 4 && ciphertext[:4] == "ENC:" {
		return ciphertext[4:], nil
	}
	return ciphertext, nil
}

func (s *xianyuWorkerControlStub) ListAccounts(context.Context, int64) ([]XianyuAccount, error) {
	return nil, nil
}

func (s *xianyuWorkerControlStub) GetAccountByWorkerAndAccountID(context.Context, int64, string) (*XianyuAccount, error) {
	return nil, nil
}

func (s *xianyuWorkerControlStub) UpsertAccount(_ context.Context, a XianyuAccount) (*XianyuAccount, error) {
	return &a, nil
}

func (s *xianyuWorkerControlStub) UpdateAccount(_ context.Context, a XianyuAccount) (*XianyuAccount, error) {
	return &a, nil
}

func (s *xianyuWorkerControlStub) ListItemPools(context.Context) ([]XianyuItemPool, error) {
	return nil, nil
}

func (s *xianyuWorkerControlStub) GetItemPoolByID(context.Context, int64) (*XianyuItemPool, error) {
	return nil, nil
}

func (s *xianyuWorkerControlStub) GetItemPoolBySlug(context.Context, string) (*XianyuItemPool, error) {
	return nil, nil
}

func (s *xianyuWorkerControlStub) CreateItemPool(_ context.Context, p XianyuItemPool) (*XianyuItemPool, error) {
	return &p, nil
}

func (s *xianyuWorkerControlStub) UpdateItemPool(_ context.Context, p XianyuItemPool) (*XianyuItemPool, error) {
	return &p, nil
}

func (s *xianyuWorkerControlStub) PoolStockCounts(context.Context, string) (int, int, int, error) {
	return 0, 0, 0, nil
}

func (s *xianyuWorkerControlStub) DeliveryStats(context.Context, time.Time) (int, int, error) {
	return 0, 0, nil
}

func (s *xianyuWorkerControlStub) PendingDeliveryCount(context.Context) (int, error) {
	return 0, nil
}

func (s *xianyuWorkerControlStub) ListProducts(context.Context) ([]XianyuProduct, error) {
	return nil, nil
}

func (s *xianyuWorkerControlStub) ListProductsByAccount(context.Context, int64) ([]XianyuProduct, error) {
	return nil, nil
}

func (s *xianyuWorkerControlStub) GetProductByIdentity(context.Context, int64, string, string, string) (*XianyuProduct, error) {
	return nil, nil
}

func (s *xianyuWorkerControlStub) UpsertProduct(_ context.Context, p XianyuProduct) (*XianyuProduct, error) {
	return &p, nil
}

func (s *xianyuWorkerControlStub) UpdateProduct(_ context.Context, p XianyuProduct) (*XianyuProduct, error) {
	return &p, nil
}

func (s *xianyuWorkerControlStub) UpdateProductBinding(context.Context, int64, string, string, *int64) error {
	return nil
}

func (s *xianyuWorkerControlStub) ListBindingRules(context.Context) ([]XianyuBindingRule, error) {
	return nil, nil
}

func (s *xianyuWorkerControlStub) CreateBindingRule(_ context.Context, r XianyuBindingRule) (*XianyuBindingRule, error) {
	return &r, nil
}

func (s *xianyuWorkerControlStub) UpdateBindingRule(_ context.Context, r XianyuBindingRule) (*XianyuBindingRule, error) {
	return &r, nil
}

func (s *xianyuWorkerControlStub) ListWorkerConfigs(context.Context) ([]XianyuWorkerConfig, error) {
	return nil, nil
}

func (s *xianyuWorkerControlStub) CreateWorkerConfig(_ context.Context, cfg XianyuWorkerConfig) (*XianyuWorkerConfig, error) {
	return &cfg, nil
}

func (s *xianyuWorkerControlStub) UpdateWorkerConfig(_ context.Context, cfg XianyuWorkerConfig) (*XianyuWorkerConfig, error) {
	if s.updateErr != nil {
		return nil, s.updateErr
	}
	s.updated = &cfg
	return &cfg, nil
}

func (s *xianyuWorkerControlStub) GetActiveWorkerConfig(context.Context) (*XianyuWorkerConfig, error) {
	if s.cfg == nil {
		return nil, ErrXianyuWorkerConfigNotFound
	}
	return s.cfg, nil
}

func TestXianyuCheckHealthRejectsMissingWebSocket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":{"backend":true,"websocket":false,"database":true}}`))
	}))
	defer srv.Close()

	control := &xianyuWorkerControlStub{cfg: &XianyuWorkerConfig{
		ID: 1, BaseURL: srv.URL, Status: XianyuWorkerStatusActive,
		APITokenEncrypted: "ENC:plain", HealthStatus: XianyuWorkerHealthHealthy,
	}}
	worker := NewXianyuWorkerService(control, testSecretEncryptor{})
	worker.clientFor = func(baseURL, token string) *XianyuWorkerClient {
		require.Equal(t, srv.URL, baseURL)
		require.Equal(t, "plain", token)
		return NewXianyuWorkerClient(baseURL, token, time.Second)
	}

	err := worker.CheckHealth(context.Background())
	require.Error(t, err)
	require.Equal(t, XianyuWorkerHealthUnhealthy, control.updated.HealthStatus)
}

func TestXianyuCheckHealthUpdateErrorIsReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"data":{"backend":true,"websocket":true,"database":true}}`))
	}))
	defer srv.Close()

	updateErr := errors.New("database unavailable")
	control := &xianyuWorkerControlStub{
		cfg: &XianyuWorkerConfig{
			ID: 1, BaseURL: srv.URL, Status: XianyuWorkerStatusActive,
			APITokenEncrypted: "ENC:plain",
		},
		updateErr: updateErr,
	}
	worker := NewXianyuWorkerService(control, testSecretEncryptor{})
	worker.clientFor = func(baseURL, token string) *XianyuWorkerClient {
		return NewXianyuWorkerClient(baseURL, token, time.Second)
	}

	require.ErrorIs(t, worker.CheckHealth(context.Background()), updateErr)
}
