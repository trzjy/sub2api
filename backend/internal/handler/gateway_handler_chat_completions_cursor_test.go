//go:build unit

package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	middleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGeminiChatCompletionsPlatformAdmission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tt := range []struct {
		name, platform, errorType, message string
	}{
		// Missing Cursor credentials deliberately fail at the existing forwarder;
		// reaching that error proves the handler did not discard the selected account.
		{"cursor reaches forwarder", service.PlatformCursor, "upstream_error", infraerrors.UpstreamRequestFailed},
		{"gemini reaches compat service", service.PlatformGemini, "upstream_error", infraerrors.CompatServiceNotConfigured},
		{"other mixed supply remains excluded", service.PlatformAntigravity, "server_error", infraerrors.AllAccountsExhausted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			groupID := int64(9410)
			group := &service.Group{ID: groupID, Hydrated: true, Platform: service.PlatformGemini, Status: service.StatusActive}
			account := &service.Account{
				ID: 9411, Platform: tt.platform, Type: service.AccountTypeAPIKey,
				Status: service.StatusActive, Schedulable: true, Concurrency: 1,
				Credentials:   map[string]any{"model_mapping": map[string]any{"gemini-3.8-flash": "gemini-3.8-flash"}},
				Extra:         map[string]any{"mixed_scheduling": true},
				AccountGroups: []service.AccountGroup{{AccountID: 9411, GroupID: groupID}},
			}
			h, cleanup := newTestGatewayHandler(t, group, []*service.Account{account})
			defer cleanup()
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			body := `{"model":"gemini-3.8-flash","messages":[{"role":"user","content":"Calculate 23 * 19."}],"stream":false,"max_tokens":128}`
			ctx := context.WithValue(context.Background(), ctxkey.Group, group)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(body)).WithContext(ctx)
			apiKey := &service.APIKey{
				ID: 9412, UserID: 9413, GroupID: &groupID, Group: group, Status: service.StatusActive,
				User: &service.User{ID: 9413, Concurrency: 10, Balance: 100},
			}
			c.Set(string(middleware.ContextKeyAPIKey), apiKey)
			c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: apiKey.UserID, Concurrency: 10})

			h.ChatCompletions(c)

			selected, ok := c.Get(opsAccountIDKey)
			require.True(t, ok, "test must reach account selection")
			require.Equal(t, account.ID, selected)
			require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
			var response struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
			require.Equal(t, tt.errorType, response.Error.Type)
			require.Equal(t, tt.message, response.Error.Message)
		})
	}
}
