package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// TestAccountHandlerGetAvailableModels_CodeBuddyAccountDoesNotFallBackToClaudeDefaults
// 回归本次"显示误导"修复：CodeBuddy 账号没有静态默认模型目录，未同步清单时
// /admin/accounts/:id/models 必须返回**空集**，不得落入通用 claude.DefaultModels
// （否则会把通用 Claude 目录误显示为该账号的真实授权——实测 intl 账号曾被显示成
// "提供 11 个 Claude 模型"）。
func TestAccountHandlerGetAvailableModels_CodeBuddyAccountDoesNotFallBackToClaudeDefaults(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       62,
			Name:     "codebuddy-intl",
			Platform: service.PlatformCodeBuddy,
			Type:     service.AccountTypeOAuth,
			Status:   service.StatusActive,
		},
	}
	router := setupAvailableModelsRouter(svc)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/62/models", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Empty(t, resp.Data,
		"CodeBuddy 账号无同步清单时必须返回空集，不得回落通用 Claude 默认目录")
}

// TestAccountHandlerGetAvailableModels_CodeBuddyAccountUsesSyncedSnapshot
// CodeBuddy 账号应展示其**真实已同步**的上游模型快照（extra），而非通用默认目录。
func TestAccountHandlerGetAvailableModels_CodeBuddyAccountUsesSyncedSnapshot(t *testing.T) {
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:       63,
			Name:     "codebuddy-cn",
			Platform: service.PlatformCodeBuddy,
			Type:     service.AccountTypeOAuth,
			Status:   service.StatusActive,
			Extra: map[string]any{
				service.UpstreamModelMetadataExtraKey: map[string]any{
					"models": map[string]any{
						"deepseek-v4-flash": map[string]any{},
						"kimi-k2.5":         map[string]any{},
					},
				},
			},
		},
	}
	router := setupAvailableModelsRouter(svc)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/63/models", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ids := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		ids = append(ids, m.ID)
	}
	require.ElementsMatch(t, []string{"deepseek-v4-flash", "kimi-k2.5"}, ids,
		"CodeBuddy 账号应展示已同步的上游模型快照")
}

// TestAccountHandlerGetAvailableModels_CodeBuddyShadowUsesShadowModelExtra
// CodeBuddy 影子账号创建时仅写入 extra[shadow_model]（无上游快照、无 model_mapping），
// /admin/accounts/:id/models 必须返回该影子真实服务的上游模型，而非空集。
func TestAccountHandlerGetAvailableModels_CodeBuddyShadowUsesShadowModelExtra(t *testing.T) {
	parentID := int64(8277)
	svc := &availableModelsAdminService{
		stubAdminService: newStubAdminService(),
		account: service.Account{
			ID:              9001,
			Name:            "8277:cn:deepseek-v4.1-flash:DeepSeek 计量",
			Platform:        service.PlatformDeepseek,
			Type:            service.AccountTypeOAuth,
			Status:          service.StatusActive,
			ParentAccountID: &parentID,
			QuotaDimension:  service.QuotaDimensionCodeBuddy,
			Extra: map[string]any{
				service.ShadowModelExtraKey: "deepseek-v4.1-flash",
			},
		},
	}
	router := setupAvailableModelsRouter(svc)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/accounts/9001/models", nil))
	require.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ids := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		ids = append(ids, m.ID)
	}
	require.Equal(t, []string{"deepseek-v4.1-flash"}, ids,
		"CodeBuddy 影子账号应返回其 extra[shadow_model] 指定的真实上游模型")
}
