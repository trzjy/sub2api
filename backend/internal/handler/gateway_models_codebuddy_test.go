package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// codeBuddyModelsHandlerRepoStub 返回生产形态的 codebuddy 账号：无
// model_mapping，仅有上游模型目录快照（accounts.extra.upstream_model_metadata）。
type codeBuddyModelsHandlerRepoStub struct {
	gatewayModelsAccountRepoStub
}

func newCodeBuddySnapshotAccount(id int64, models map[string]service.UpstreamModelMetadata) service.Account {
	acc := service.Account{
		ID:          id,
		Platform:    service.PlatformCodeBuddy,
		Status:      service.StatusActive,
		Schedulable: true,
		Extra:       map[string]any{},
		Credentials: map[string]any{},
	}
	acc.SetUpstreamModelMetadataSnapshot(service.UpstreamModelMetadataSnapshot{
		Source:   "upstream",
		SyncedAt: "2026-10-15T11:46:42Z",
		Models:   models,
	})
	return acc
}

// TestGatewayModels_CodeBuddyWithAllowlistReturnsUpstreamSnapshotModels 复现并钉住
// 生产缺陷（分组 109）：codebuddy 分组开启 model_allowlist=["space-bunny"] 且账号既无
// model_mapping 也有上游快照时，GET /v1/models 曾返回 0 个模型——因为 codebuddy 没有
// 平台默认目录、缺 mapping 时列表落空，再与白名单求交得空。
func TestGatewayModels_CodeBuddyWithAllowlistReturnsUpstreamSnapshotModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(109)
	repo := &codeBuddyModelsHandlerRepoStub{gatewayModelsAccountRepoStub: gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {
				newCodeBuddySnapshotAccount(256, map[string]service.UpstreamModelMetadata{
					"space-bunny": {ID: "space-bunny"},
					"hy3":         {ID: "hy3"},
					"kimi-k3":     {ID: "kimi-k3"},
				}),
			},
		},
	}}
	h := newGatewayModelsHandlerForTest(repo)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{
			ID:       groupID,
			Platform: service.PlatformCodeBuddy,
			ModelAllowlist: service.GroupModelAllowlist{
				Enabled: true,
				Models:  []string{"space-bunny"},
			},
		},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, []string{"space-bunny"}, modelIDsForTest(got.Data),
		"白名单求交后必须返回 space-bunny，而不是 0 个模型")
}

// TestGatewayModels_CodeBuddyWithoutAllowlistReturnsUpstreamSnapshotModels 钉住
// 未开启白名单的 codebuddy 分组：列表直接来自上游快照，且不得混入 Claude 默认模型。
func TestGatewayModels_CodeBuddyWithoutAllowlistReturnsUpstreamSnapshotModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(110)
	repo := &codeBuddyModelsHandlerRepoStub{gatewayModelsAccountRepoStub: gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {
				newCodeBuddySnapshotAccount(256, map[string]service.UpstreamModelMetadata{
					"space-bunny": {ID: "space-bunny"},
					"hy3":         {ID: "hy3"},
				}),
			},
		},
	}}
	h := newGatewayModelsHandlerForTest(repo)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformCodeBuddy},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Equal(t, []string{"hy3", "space-bunny"}, modelIDsForTest(got.Data))
	for _, id := range modelIDsForTest(got.Data) {
		require.NotContains(t, id, "claude-",
			"codebuddy 列表不得回落到 Claude 默认目录")
	}
}

// TestGatewayModels_CodeBuddyNoCatalogReturnsEmptyNotClaude 钉住失败闭合：
// codebuddy 账号既无 mapping 也无快照时返回空列表，而非 Claude 默认模型。
func TestGatewayModels_CodeBuddyNoCatalogReturnsEmptyNotClaude(t *testing.T) {
	gin.SetMode(gin.TestMode)

	groupID := int64(111)
	repo := &codeBuddyModelsHandlerRepoStub{gatewayModelsAccountRepoStub: gatewayModelsAccountRepoStub{
		byGroup: map[int64][]service.Account{
			groupID: {
				{ID: 257, Platform: service.PlatformCodeBuddy, Status: service.StatusActive,
					Schedulable: true, Extra: map[string]any{}, Credentials: map[string]any{}},
			},
		},
	}}
	h := newGatewayModelsHandlerForTest(repo)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(string(middleware2.ContextKeyAPIKey), &service.APIKey{
		Group: &service.Group{ID: groupID, Platform: service.PlatformCodeBuddy},
	})

	h.Models(c)

	require.Equal(t, http.StatusOK, rec.Code)

	var got gatewayModelsResponseForTest
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Empty(t, modelIDsForTest(got.Data),
		"无可用目录时返回空列表，不得对外声称提供 Claude 默认模型")
}

// TestDefaultModelIDsForCodeBuddyIsEmpty 钉住平台默认目录：codebuddy 无内置目录，
// 必须返回空（否则又会回落到 Claude 默认模型）。
func TestDefaultModelIDsForCodeBuddyIsEmpty(t *testing.T) {
	require.Empty(t, defaultModelIDsForPlatform(service.PlatformCodeBuddy))
}
