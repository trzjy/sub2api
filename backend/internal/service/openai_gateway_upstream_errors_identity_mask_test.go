package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- helpers to build test accounts ---

func newMaskedTestAccount(id int64, modelMapping map[string]any) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"model_mapping": modelMapping},
		Extra:       map[string]any{"mask_upstream_identity": true},
	}
}

func newPlainTestAccount(id int64, modelMapping map[string]any) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeAPIKey,
		Credentials: map[string]any{"model_mapping": modelMapping},
		Extra:       map[string]any{"mask_upstream_identity": false},
	}
}

// --- pure helper tests ---

func TestMaskedAccountError_ScrubMessage(t *testing.T) {
	masked := newMaskedTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})
	plain := newPlainTestAccount(2, map[string]any{"gpt-4": "qwen3.8-flash"})

	// masked: upstream model name scrubbed to generic placeholder
	raw := "Use the paid model 'qwen3.8-flash' for this request"
	out := MaskUpstreamErrorMessageForClient(masked, raw)
	assert.NotContains(t, out, "qwen3.8-flash")
	assert.Contains(t, out, maskedUpstreamModelPlaceholder)
	assert.Equal(t, "Use the paid model 'the requested model' for this request", out)

	// masked: tokenharbor trace scrubbed
	out = MaskUpstreamErrorMessageForClient(masked, "tokenharbor rejected the request")
	assert.NotContains(t, out, "tokenharbor")
	assert.Contains(t, out, maskedTokenHarborPlaceholder)

	// masked: suffix-stripped alias (qwen3.8-flash:free) also scrubbed
	out = MaskUpstreamErrorMessageForClient(masked, "model qwen3.8-flash:free is unavailable")
	assert.NotContains(t, out, "qwen3.8-flash")
	assert.NotContains(t, out, "qwen3.8-flash:free")

	// plain (genuine kimi) account: byte-for-byte unchanged (hard boundary)
	out = MaskUpstreamErrorMessageForClient(plain, raw)
	assert.Equal(t, raw, out)

	// nil account: unchanged
	assert.Equal(t, raw, MaskUpstreamErrorMessageForClient(nil, raw))
}

func TestMaskedAccountError_ScrubMessage_CompactMapping(t *testing.T) {
	// compact_model_mapping values must also be scrubbed
	masked := newMaskedTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})
	masked.Credentials["compact_model_mapping"] = map[string]any{"gpt-4-compact": "tokenharbor-mini"}
	out := MaskUpstreamErrorMessageForClient(masked, "tokenharbor-mini is rate limited")
	assert.NotContains(t, out, "tokenharbor-mini")
}

// --- integration: handleErrorResponse ---

func TestMaskedAccountError_HandleErrorResponse_429StandardText(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, masked := range []bool{true, false} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

		svc := &OpenAIGatewayService{}
		body := []byte(`{"error":{"message":"Use the paid model 'qwen3.8-flash'","code":"free_tier_limit_reached"}}`)
		resp := &http.Response{
			StatusCode: 429,
			Body:       io.NopCloser(bytes.NewReader(body)),
			Header:     http.Header{},
		}
		var acct *Account
		if masked {
			acct = newMaskedTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})
		} else {
			acct = newPlainTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})
		}

		_, err := svc.handleErrorResponse(context.Background(), resp, c, acct, nil)
		require.Error(t, err)
		assert.Equal(t, http.StatusTooManyRequests, rec.Code)

		var p map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
		ef, ok := p["error"].(map[string]any)
		require.True(t, ok)
		// 429 already maps to the platform standard rate-limit text for both
		// masked and plain accounts; the masked branch must never echo the model.
		assert.Equal(t, infraerrors.UpstreamRateLimited, ef["message"])
		assert.NotContains(t, rec.Body.String(), "qwen3.8-flash")
		assert.NotContains(t, rec.Body.String(), "tokenharbor")
	}
}

func TestMaskedAccountError_HandleErrorResponse_ModelNameScrubbed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// masked account: model name scrubbed, no tokenharbor trace
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	svc := &OpenAIGatewayService{}
	body := []byte(`{"error":{"message":"model qwen3.8-flash is not available","type":"invalid_request_error","code":"model_not_found","param":"model"}}`)
	resp := &http.Response{StatusCode: 400, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}}
	acct := newMaskedTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})

	_, err := svc.handleErrorResponse(context.Background(), resp, c, acct, nil)
	require.Error(t, err)
	var p map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	ef, ok := p["error"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, ef["message"], "qwen3.8-flash")
	assert.Contains(t, ef["message"], maskedUpstreamModelPlaceholder)
	assert.NotContains(t, rec.Body.String(), "tokenharbor")
	// masked account must not echo upstream type/code/param carrying identity
	assert.NotContains(t, rec.Body.String(), "model_not_found")

	// plain (genuine kimi) account: regression — byte-for-byte unchanged.
	// Fresh response body, since the previous call already drained it.
	rec2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(rec2)
	c2.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	acct2 := newPlainTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})
	resp2 := &http.Response{StatusCode: 400, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}}
	_, err = svc.handleErrorResponse(context.Background(), resp2, c2, acct2, nil)
	require.Error(t, err)
	var p2 map[string]any
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &p2))
	ef2 := p2["error"].(map[string]any)
	assert.Equal(t, "model qwen3.8-flash is not available", ef2["message"])
}

func TestMaskedAccountError_HandleErrorResponse_UnknownBodyNoPassthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	svc := &OpenAIGatewayService{}
	// non-JSON upstream body that leaks identity
	body := []byte("raw upstream failure: qwen3.8-flash tokenharbor down")
	resp := &http.Response{StatusCode: 500, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}}
	acct := newMaskedTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})

	_, err := svc.handleErrorResponse(context.Background(), resp, c, acct, nil)
	require.Error(t, err)
	assert.Equal(t, http.StatusBadGateway, rec.Code)

	var p map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	ef, ok := p["error"].(map[string]any)
	require.True(t, ok)
	// fixed platform-standard message, never the raw body text
	assert.Equal(t, infraerrors.UpstreamRequestFailed, ef["message"])
	assert.NotContains(t, rec.Body.String(), "qwen3.8-flash")
	assert.NotContains(t, rec.Body.String(), "tokenharbor")
}

func TestMaskedAccountError_HandleErrorResponse_PassthroughScrubbed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// A masked account must not receive an upstream-derived passthrough message.
	// Without a configured passthrough rule the branch is skipped; verify the
	// default path still scrubs and never echoes the model name.
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	svc := &OpenAIGatewayService{}
	body := []byte(`{"error":{"message":"qwen3.8-flash is over quota"}}`)
	resp := &http.Response{StatusCode: 402, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}}
	acct := newMaskedTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})

	_, err := svc.handleErrorResponse(context.Background(), resp, c, acct, nil)
	require.Error(t, err)
	assert.NotContains(t, rec.Body.String(), "qwen3.8-flash")
	assert.NotContains(t, rec.Body.String(), "tokenharbor")
}

// --- integration: streaming failure event builder ---

func TestMaskedAccountError_BuildResponseFailedSSE_NoLeak(t *testing.T) {
	masked := newMaskedTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})
	// production path: masked account passes a scrubbed message and a nil source
	msg := MaskUpstreamErrorMessageForClient(masked, "qwen3.8-flash unavailable")
	if msg == "" {
		msg = infraerrors.UpstreamRequestFailed
	}
	out := buildOpenAIResponseFailedSSE("resp_1", "gpt-4", nil, msg)
	assert.NotContains(t, out, "qwen3.8-flash")
	assert.NotContains(t, out, "tokenharbor")
	assert.Contains(t, out, "response.failed")

	// With a nil source, the fallback (already-scrubbed) message is used and a
	// would-be upstream payload is never echoed — this is the production path for
	// masked accounts.
	out2 := buildOpenAIResponseFailedSSE("resp_2", "gpt-4", nil, "the requested model unavailable")
	assert.NotContains(t, out2, "qwen3.8-flash")
	assert.Contains(t, out2, "the requested model unavailable")
}

// --- failover stash: MaskedAccountFailoverKey written on failover-producing paths ---

// TestMaskedFailoverStash_HTTPError verifies that failoverOpenAIUpstreamHTTPError
// stashes the masked-identity flag on the gin.Context at function entry, so that
// handleFailoverExhausted can later read it to mask the final client error.
func TestMaskedFailoverStash_HTTPError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := []byte(`{"error":{"message":"internal server error","type":"server_error"}}`)
	upstreamModel := "gpt-4"
	upstreamMsg := "internal server error"

	for _, masked := range []bool{true, false} {
		t.Run(maskedLabel(masked), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

			svc := &OpenAIGatewayService{}
			var acct *Account
			if masked {
				acct = newMaskedTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})
			} else {
				acct = newPlainTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})
			}

			resp := &http.Response{
				StatusCode: 500,
				Body:       io.NopCloser(bytes.NewReader(body)),
				Header:     http.Header{},
			}
			ferr := svc.failoverOpenAIUpstreamHTTPError(context.Background(), c, acct, resp, body, upstreamMsg, upstreamModel)

			// 500 is unconditionally failover-eligible, so the producer path
			// must yield an *UpstreamFailoverError for both account kinds.
			require.NotNil(t, ferr, "expected *UpstreamFailoverError from failover producer")
			assert.IsType(t, &UpstreamFailoverError{}, ferr)
			assert.Equal(t, masked, c.GetBool(MaskedAccountFailoverKey),
				"MaskedAccountFailoverKey must equal whether the account is identity-masked")
		})
	}
}

// TestMaskedFailoverStash_TransportError verifies that handleOpenAIUpstreamTransportError
// stashes the masked-identity flag on the gin.Context at function entry (before any
// early-return branch), so the failover-consumer path sees it even for transport errors.
func TestMaskedFailoverStash_TransportError(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// transient (non-canceled, non-persistent) transport error -> failover return.
	transientErr := errors.New("unexpected EOF")

	for _, masked := range []bool{true, false} {
		t.Run(maskedLabel(masked), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

			svc := &OpenAIGatewayService{}
			var acct *Account
			if masked {
				acct = newMaskedTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})
			} else {
				acct = newPlainTestAccount(1, map[string]any{"gpt-4": "qwen3.8-flash"})
			}

			err := svc.handleOpenAIUpstreamTransportError(context.Background(), c, acct, transientErr, false, "gpt-4")

			// transient transport failure fails over -> *UpstreamFailoverError.
			require.NotNil(t, err)
			var ferr *UpstreamFailoverError
			assert.ErrorAs(t, err, &ferr)
			assert.Equal(t, masked, c.GetBool(MaskedAccountFailoverKey),
				"MaskedAccountFailoverKey must equal whether the account is identity-masked")
		})
	}
}

// maskedLabel returns a stable subtest name for the masked/non-masked cases.
func maskedLabel(masked bool) string {
	if masked {
		return "masked"
	}
	return "plain"
}
