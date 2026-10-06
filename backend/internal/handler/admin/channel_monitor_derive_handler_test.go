//go:build unit

package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// deriveHandlerRepoStub 实现 Get（GetByID）与渠道推导（ListLatestPerModel）所需的 repo 子集。
type deriveHandlerRepoStub struct {
	service.ChannelMonitorRepository
	monitor   *service.ChannelMonitor
	latest    []*service.ChannelMonitorLatest
	latestErr error
}

func (r *deriveHandlerRepoStub) GetByID(_ context.Context, _ int64) (*service.ChannelMonitor, error) {
	return r.monitor, nil
}

func (r *deriveHandlerRepoStub) ListLatestPerModel(_ context.Context, _ int64) ([]*service.ChannelMonitorLatest, error) {
	if r.latestErr != nil {
		return nil, r.latestErr
	}
	return r.latest, nil
}

type deriveHandlerEncryptor struct{}

func (deriveHandlerEncryptor) Encrypt(plain string) (string, error)  { return "ENC:" + plain, nil }
func (deriveHandlerEncryptor) Decrypt(cipher string) (string, error) { return cipher, nil }

func setupChannelMonitorGetRouter(repo service.ChannelMonitorRepository) *gin.Engine {
	gin.SetMode(gin.TestMode)
	svc := service.NewChannelMonitorService(repo, deriveHandlerEncryptor{})
	h := NewChannelMonitorHandler(svc)
	router := gin.New()
	router.GET("/api/v1/admin/channel-monitors/:id", h.Get)
	return router
}

// #11：推导失败（权威读取错误）不得以 Success 返回不完整渠道状态，必须走明确错误响应。
func TestChannelMonitorGetDeriveFailureReturnsError(t *testing.T) {
	repo := &deriveHandlerRepoStub{
		monitor:   &service.ChannelMonitor{ID: 1, Name: "ch", Enabled: true},
		latestErr: errors.New("db transient failure"),
	}
	router := setupChannelMonitorGetRouter(repo)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/channel-monitors/1", nil)
	router.ServeHTTP(rec, req)

	require.NotEqual(t, http.StatusOK, rec.Code, "derivation failure must not be a 200 Success")
	require.GreaterOrEqual(t, rec.Code, http.StatusInternalServerError, "must surface an explicit server error")
	require.NotContains(t, rec.Body.String(), `"channel_status"`, "must not return an incomplete success payload")
}

// #9：无权威观测 → 成功响应但 channel_status 为空（前端渲染横线）。
func TestChannelMonitorGetNoDataReturnsEmptyChannelStatus(t *testing.T) {
	repo := &deriveHandlerRepoStub{
		monitor: &service.ChannelMonitor{ID: 1, Name: "ch", Enabled: true},
	}
	router := setupChannelMonitorGetRouter(repo)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/channel-monitors/1", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"channel_status":""`,
		"no-data channel must serialize an empty status for the frontend dash")
}
