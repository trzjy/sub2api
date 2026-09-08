package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 回归：Worker 对"账号不存在"返回 404 {"detail":"账号不存在"} 时必须归类为
// ErrXianyuWorkerAccountNotFound（域错误，前端显示干净 404），而非通用 500/internal error。
func TestMapErrorClassifiesWorkerAccountNotFound(t *testing.T) {
	c := &XianyuWorkerClient{}

	err := c.mapError(http.StatusNotFound, []byte(`{"detail":"账号不存在"}`), "838831211")
	if !errors.Is(err, ErrXianyuWorkerAccountNotFound) {
		t.Fatalf("expected ErrXianyuWorkerAccountNotFound, got %v", err)
	}

	// 非该 detail 的 404（如路由缺失 {"detail":"Not Found"}）不得误判为账号缺失。
	err = c.mapError(http.StatusNotFound, []byte(`{"detail":"Not Found"}`), "x")
	if errors.Is(err, ErrXianyuWorkerAccountNotFound) {
		t.Fatalf("generic 404 misclassified as account-not-found: %v", err)
	}
	var we *XianyuWorkerError
	if !errors.As(err, &we) {
		t.Fatalf("expected *XianyuWorkerError, got %T", err)
	}

	// 500 必须是真实故障，绝不归类为"账号已清除"。
	err = c.mapError(http.StatusInternalServerError, []byte(`boom`), "x")
	if errors.Is(err, ErrXianyuWorkerAccountNotFound) {
		t.Fatal("500 must not be classified as account-not-found")
	}
}

type clearCredControlStub struct {
	*xianyuWorkerControlStub
	account         *XianyuAccount
	gotConverged    bool
	enableCalled    bool
	enableAccountID string
}

func (s *clearCredControlStub) GetProductByID(context.Context, int64) (*XianyuProduct, error) {
	return nil, nil
}

func (s *clearCredControlStub) GetAccountByWorkerAndAccountID(context.Context, int64, string) (*XianyuAccount, error) {
	return s.account, nil
}

func (s *clearCredControlStub) UpdateAccount(_ context.Context, a XianyuAccount) (*XianyuAccount, error) {
	if a.Status == XianyuAccountStatusLoggedOut && a.TaskStatus == XianyuTaskStatusStopped {
		s.gotConverged = true
	}
	return &a, nil
}

// 回归：账号已在 Worker 侧被清除后再次"退出"，应幂等成功并仍把主程序投影收敛到 logged_out/stopped，
// 而不是向上抛出 internal error。
func TestClearCredentialsIdempotentWhenWorkerAccountMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"账号不存在"}`))
	}))
	defer srv.Close()

	cfg := &XianyuWorkerConfig{
		ID:                1,
		BaseURL:           srv.URL,
		APITokenEncrypted: "ENC:token",
		Status:            XianyuWorkerStatusActive,
	}
	ctrl := &clearCredControlStub{
		xianyuWorkerControlStub: &xianyuWorkerControlStub{cfg: cfg},
		account: &XianyuAccount{
			WorkerConfigID: 1,
			AccountID:      "838831211",
			Status:         XianyuAccountStatusDisabled,
			TaskStatus:     XianyuTaskStatusRunning,
		},
	}
	svc := &XianyuWorkerService{
		control:    ctrl,
		encryptor:  testSecretEncryptor{},
		forbidLoop: true,
		clientFor: func(baseURL, token string) *XianyuWorkerClient {
			return NewXianyuWorkerClient(baseURL, token, 5*time.Second)
		},
	}

	if err := svc.ClearCredentials(context.Background(), "838831211"); err != nil {
		t.Fatalf("expected idempotent success, got %v", err)
	}
	if !ctrl.gotConverged {
		t.Fatal("expected main projection to be marked logged_out/stopped after idempotent clear")
	}
}

// 已退出（logged_out）的账号不允许直接启用：必须重新扫码登录。
func TestEnableAccountRejectedWhenLoggedOut(t *testing.T) {
	var workerHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		workerHit = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := &XianyuWorkerConfig{
		ID:                1,
		BaseURL:           srv.URL,
		APITokenEncrypted: "ENC:token",
		Status:            XianyuWorkerStatusActive,
	}
	ctrl := &clearCredControlStub{
		xianyuWorkerControlStub: &xianyuWorkerControlStub{cfg: cfg},
		account: &XianyuAccount{
			WorkerConfigID: 1,
			AccountID:      "838831211",
			Status:         XianyuAccountStatusLoggedOut,
			TaskStatus:     XianyuTaskStatusStopped,
		},
	}
	svc := &XianyuWorkerService{
		control:    ctrl,
		encryptor:  testSecretEncryptor{},
		forbidLoop: true,
		clientFor: func(baseURL, token string) *XianyuWorkerClient {
			return NewXianyuWorkerClient(baseURL, token, 5*time.Second)
		},
	}

	err := svc.EnableAccount(context.Background(), "838831211")
	if !errors.Is(err, ErrXianyuAccountLoggedOut) {
		t.Fatalf("expected ErrXianyuAccountLoggedOut, got %v", err)
	}
	if workerHit {
		t.Fatal("worker must not be called for logged-out account")
	}
}

// 启用时 Worker 报"账号不存在"：投影必须收敛为 logged_out，避免残留"可启用"的停用态。
func TestEnableAccountConvergesToLoggedOutWhenWorkerAccountMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"账号不存在"}`))
	}))
	defer srv.Close()

	cfg := &XianyuWorkerConfig{
		ID:                1,
		BaseURL:           srv.URL,
		APITokenEncrypted: "ENC:token",
		Status:            XianyuWorkerStatusActive,
	}
	ctrl := &clearCredControlStub{
		xianyuWorkerControlStub: &xianyuWorkerControlStub{cfg: cfg},
		account: &XianyuAccount{
			WorkerConfigID: 1,
			AccountID:      "838831211",
			Status:         XianyuAccountStatusDisabled,
			TaskStatus:     XianyuTaskStatusStopped,
		},
	}
	svc := &XianyuWorkerService{
		control:    ctrl,
		encryptor:  testSecretEncryptor{},
		forbidLoop: true,
		clientFor: func(baseURL, token string) *XianyuWorkerClient {
			return NewXianyuWorkerClient(baseURL, token, 5*time.Second)
		},
	}

	err := svc.EnableAccount(context.Background(), "838831211")
	if !errors.Is(err, ErrXianyuWorkerAccountNotFound) {
		t.Fatalf("expected ErrXianyuWorkerAccountNotFound, got %v", err)
	}
	if !ctrl.gotConverged {
		t.Fatal("expected main projection to converge to logged_out after enable hit missing worker account")
	}
}

// 外审 P2：收敛（DB 投影更新）失败时，ClearCredentials 必须透传错误而非谎报成功，
// 否则主程序残留 enabled/disabled 与 Worker 状态不一致（UI 与后续操作陷入不可知）。
type clearCredControlFailingStub struct {
	*xianyuWorkerControlStub
	account *XianyuAccount
}

func (s *clearCredControlFailingStub) GetAccountByWorkerAndAccountID(_ context.Context, _ int64, _ string) (*XianyuAccount, error) {
	return s.account, nil
}

func (s *clearCredControlFailingStub) UpdateAccount(_ context.Context, _ XianyuAccount) (*XianyuAccount, error) {
	return nil, errors.New("db update failed")
}

func TestClearCredentialsPropagatesConvergeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"账号不存在"}`))
	}))
	defer srv.Close()

	cfg := &XianyuWorkerConfig{
		ID:                1,
		BaseURL:           srv.URL,
		APITokenEncrypted: "ENC:token",
		Status:            XianyuWorkerStatusActive,
	}
	ctrl := &clearCredControlFailingStub{
		xianyuWorkerControlStub: &xianyuWorkerControlStub{cfg: cfg},
		account: &XianyuAccount{
			WorkerConfigID: 1,
			AccountID:      "838831211",
			Status:         XianyuAccountStatusDisabled,
			TaskStatus:     XianyuTaskStatusRunning,
		},
	}
	svc := &XianyuWorkerService{
		control:    ctrl,
		encryptor:  testSecretEncryptor{},
		forbidLoop: true,
		clientFor: func(baseURL, token string) *XianyuWorkerClient {
			return NewXianyuWorkerClient(baseURL, token, 5*time.Second)
		},
	}
	err := svc.ClearCredentials(context.Background(), "838831211")
	if err == nil {
		t.Fatal("expected ClearCredentials to propagate the converge (UpdateAccount) error, got nil")
	}
}
