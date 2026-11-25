//go:build unit

package handler

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// The resume probe installs the authenticated identity even for a fresh request.
// Responses must pass that same context to selection and the eventual Cursor
// session, rather than reuse its pricing/channel context captured before probing.
func TestGatewayResponsesRetainsCursorIdentityAfterResumeProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)
	groupID := int64(5391)
	group := &service.Group{
		ID: groupID, Hydrated: true, Platform: service.PlatformAnthropic, Status: service.StatusActive,
	}
	account := &service.Account{
		ID: 5392, Name: "responses-context", Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
		Status: service.StatusActive, Schedulable: true, Concurrency: 1,
		Credentials:   map[string]any{"intercept_warmup_requests": true, "access_token": "test-token"},
		AccountGroups: []service.AccountGroup{{AccountID: 5392, GroupID: groupID}},
	}
	h, scheduler := cmNewGatewayHandler(t, group, []*service.Account{account}, service.PlatformAnthropic, &cmHTTPUpstream{})
	cmDriveGateway(t, h, h.Responses, group, groupID, "/v1/responses",
		`{"model":"deepseek-v4.1-flash","input":"Warmup","stream":false}`)

	scheduler.mu.Lock()
	contexts := append([]context.Context(nil), scheduler.ctxs...)
	scheduler.mu.Unlock()
	require.NotEmpty(t, contexts, "the real Responses handler must reach account selection")
	for _, ctx := range contexts {
		require.Equal(t, service.CursorSessionIdentity{UserID: 4001, APIKeyID: 3001, GroupID: groupID},
			service.CursorSessionIdentityFromContext(ctx),
			"the session must retain the authenticated identity after the resume probe")
		require.Equal(t, cmModelB, service.ChannelMappedModelFromContext(ctx),
			"refreshing the context must retain the existing channel mapping")
	}
}
