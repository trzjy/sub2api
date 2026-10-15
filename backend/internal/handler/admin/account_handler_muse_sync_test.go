//go:build unit

package admin

// masked muse 账号上游模型目录同步入口（POST /api/v1/admin/accounts/:id/models/sync-upstream）
// 策略拒绝回归（派发单 OC-T1 FIX-D）：对 mask_upstream_identity=true 的 muse 账号触发同步，
// 管理端应返回 HTTP 400（策略拒绝），且全程零上游 HTTP 调用、零 repository 写入——同步守卫
// 在入口、任何目录变更/持久化之前即失败关闭（muse_models.go syncMuseModelCatalog 守卫）。

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// recordingHTTPStub 记录上游 HTTP 调用次数（FIX-D：零上游调用）。
type recordingHTTPStub struct {
	mu    sync.Mutex
	calls int
}

func (s *recordingHTTPStub) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return nil, nil
}

func (s *recordingHTTPStub) DoWithTLS(_ *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	return nil, nil
}

// recordingMuseRepo 记录 repository 写入次数（FIX-D：零写入）。仅覆盖同步路径会触达的
// UpdateExtra；其余方法经嵌入的 AccountRepository 提升（守卫返回后不会触达，绝不 panic）。
type recordingMuseRepo struct {
	service.AccountRepository
	mu      sync.Mutex
	updates int
}

func (r *recordingMuseRepo) UpdateExtra(_ context.Context, _ int64, _ map[string]any) error {
	r.mu.Lock()
	r.updates++
	r.mu.Unlock()
	return nil
}

func maskedMuseAccount() service.Account {
	return service.Account{
		ID:       777,
		Platform: service.PlatformMuse,
		Type:     service.AccountTypeAPIKey,
		Extra:    map[string]any{"mask_upstream_identity": true},
		Credentials: map[string]any{
			"api_key":  "muse-go-key",
			"base_url": "https://opencode.ai/zen/go/v1",
		},
	}
}

func TestSyncUpstreamModels_MaskedMuseAccountRejected(t *testing.T) {
	httpStub := &recordingHTTPStub{}
	repo := &recordingMuseRepo{}

	adminSvc := newStubAdminService()
	acct := maskedMuseAccount()
	adminSvc.getAccountResult = &acct

	testSvc := service.NewAccountTestService(repo, nil, nil, nil, nil, httpStub, &config.Config{}, nil)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewAccountHandler(adminSvc, nil, nil, nil, nil, nil, nil, nil, testSvc, nil, nil, nil, nil, nil)
	router.POST("/api/v1/admin/accounts/:id/models/sync-upstream", handler.SyncUpstreamModels)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/accounts/777/models/sync-upstream", bytes.NewReader(nil))
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "masked muse 账号同步应被策略拒绝返回 400")
	require.Contains(t, rec.Body.String(), "muse catalog sync rejected by policy", "应回传策略拒绝原因")

	require.Equal(t, 0, httpStub.calls, "零上游 HTTP 调用")
	require.Equal(t, 0, repo.updates, "零 repository 写入")
}
