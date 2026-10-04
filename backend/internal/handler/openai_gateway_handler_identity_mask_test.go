//go:build unit

package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// QK-S2: masked-identity accounts must not receive an upstream-derived failover
// error. handleFailoverExhausted reads the masked flag from the gin Context
// (stashed by the service for the account that produced the failover error) and
// skips the body-derived branches (model-not-found-400 passthrough and the
// errorPassthrough MatchRule), falling through to the fixed platform-standard
// mapUpstreamError text.

func TestOpenAIFailoverExhausted_MaskedSkipsModelNotFound400(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"error":{"message":"model qwen3.8-flash not found","code":"model_not_found"}}`)
	failoverErr := &service.UpstreamFailoverError{
		StatusCode:   http.StatusBadRequest,
		ResponseBody: body,
	}

	t.Run("masked_account_skips_body_branch", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Set(service.MaskedAccountFailoverKey, true)

		(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, failoverErr, false)

		// Skipped the model-not-found-400 branch (which would have echoed the
		// upstream body); fell through to the fixed platform-standard text.
		require.Equal(t, http.StatusBadGateway, recorder.Code)
		require.NotContains(t, recorder.Body.String(), "qwen3.8-flash")
		require.NotContains(t, recorder.Body.String(), "model_not_found")
		require.Contains(t, recorder.Body.String(), infraerrors.UpstreamRequestFailed)
	})

	t.Run("plain_account_keeps_body_branch_zero_change", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Set(service.MaskedAccountFailoverKey, false)

		(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, failoverErr, false)

		// Genuine (non-masked) account: the model-not-found-400 branch is
		// preserved byte-for-byte — it echoes the upstream body message.
		require.Equal(t, http.StatusBadRequest, recorder.Code)
		require.Contains(t, recorder.Body.String(), "qwen3.8-flash")
	})
}

func TestOpenAIFailoverExhausted_MaskedSkipsPassthroughMatchRule(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// A 400 whose body matches an errorPassthrough rule would otherwise extract
	// the upstream message. A masked account must skip that branch entirely.
	body := []byte(`{"error":{"message":"qwen3.8-flash over quota","code":"free_tier_limit_reached"}}`)
	failoverErr := &service.UpstreamFailoverError{
		StatusCode:   http.StatusBadRequest,
		ResponseBody: body,
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set(service.MaskedAccountFailoverKey, true)

	(&OpenAIGatewayHandler{}).handleFailoverExhausted(c, failoverErr, false)

	require.Equal(t, http.StatusBadGateway, recorder.Code)
	require.NotContains(t, recorder.Body.String(), "qwen3.8-flash")
	require.NotContains(t, recorder.Body.String(), "free_tier_limit_reached")
	require.Contains(t, recorder.Body.String(), infraerrors.UpstreamRequestFailed)
}
