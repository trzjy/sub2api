//go:build unit

package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/stretchr/testify/require"
)

// creditsPatchCall 记录一次条目级限流 patch 调用。
type creditsPatchCall struct {
	accountID   int64
	scope       string
	expectedGen int64
	patch       ModelProbePatch
}

// creditsPatchRepo 为 clearCreditsExhausted 的条目级删除提供 PatchModelRateLimitEntry；
// 复用 unit build 中 stubAntigravityAccountRepo 的 UpdateExtra/SetModelRateLimit 调用记录。
// 不改动 stubAntigravityAccountRepo 本体，避免触碰派发单范围外的测试文件。
type creditsPatchRepo struct {
	*stubAntigravityAccountRepo
	patchCalls []creditsPatchCall
}

func (r *creditsPatchRepo) PatchModelRateLimitEntry(_ context.Context, accountID int64, scope string, expectedGen int64, patch ModelProbePatch) (bool, error) {
	r.patchCalls = append(r.patchCalls, creditsPatchCall{
		accountID:   accountID,
		scope:       scope,
		expectedGen: expectedGen,
		patch:       patch,
	})
	return true, nil
}

func TestClassifyAntigravity429(t *testing.T) {
	t.Run("明确配额耗尽", func(t *testing.T) {
		body := []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"QUOTA_EXHAUSTED"}}`)
		require.Equal(t, antigravity429QuotaExhausted, classifyAntigravity429(body))
	})

	t.Run("结构化限流", func(t *testing.T) {
		body := []byte(`{
			"error": {
				"status": "RESOURCE_EXHAUSTED",
				"details": [
					{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "metadata": {"model": "claude-sonnet-4-5"}, "reason": "RATE_LIMIT_EXCEEDED"},
					{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "0.5s"}
				]
			}
		}`)
		require.Equal(t, antigravity429RateLimited, classifyAntigravity429(body))
	})

	t.Run("未知429", func(t *testing.T) {
		body := []byte(`{"error":{"message":"too many requests"}}`)
		require.Equal(t, antigravity429Unknown, classifyAntigravity429(body))
	})
}

func TestIsCreditsExhausted_UsesAICreditsKey(t *testing.T) {
	t.Run("无 AICredits key 则积分可用", func(t *testing.T) {
		account := &Account{
			ID:       1,
			Platform: PlatformAntigravity,
			Extra: map[string]any{
				"allow_overages": true,
			},
		}
		require.False(t, account.isCreditsExhausted())
	})

	t.Run("AICredits key 生效则积分耗尽", func(t *testing.T) {
		account := &Account{
			ID:       2,
			Platform: PlatformAntigravity,
			Extra: map[string]any{
				"allow_overages": true,
				modelRateLimitsKey: map[string]any{
					creditsExhaustedKey: map[string]any{
						"rate_limited_at":     time.Now().UTC().Format(time.RFC3339),
						"rate_limit_reset_at": time.Now().Add(5 * time.Hour).UTC().Format(time.RFC3339),
					},
				},
			},
		}
		require.True(t, account.isCreditsExhausted())
	})

	t.Run("AICredits key 过期则积分可用", func(t *testing.T) {
		account := &Account{
			ID:       3,
			Platform: PlatformAntigravity,
			Extra: map[string]any{
				"allow_overages": true,
				modelRateLimitsKey: map[string]any{
					creditsExhaustedKey: map[string]any{
						"rate_limited_at":     time.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
						"rate_limit_reset_at": time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339),
					},
				},
			},
		}
		require.False(t, account.isCreditsExhausted())
	})

	t.Run("AICredits key reset 已过期但带 probe_due_at 则仍阻断（区分新旧谓词）", func(t *testing.T) {
		// 新谓词 isRateLimitBlockingForKey 在 reset_at 已过期但 probe_due_at 非空时仍返回 true
		// （恢复探测未完成）；旧谓词 isRateLimitActiveForKey 仅看 reset_at，在此输入下会返回 false。
		// 这是 D6 将 isCreditsExhausted 从旧谓词切到新谓词后，两种实现唯一可区分的边界输入。
		account := &Account{
			ID:       4,
			Platform: PlatformAntigravity,
			Extra: map[string]any{
				"allow_overages": true,
				modelRateLimitsKey: map[string]any{
					creditsExhaustedKey: map[string]any{
						"rate_limited_at":         time.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339),
						"rate_limit_reset_at":     time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339),
						modelRateLimitProbeDueKey: "2030-01-01T00:00:00Z",
					},
				},
			},
		}
		require.True(t, account.isCreditsExhausted())
	})
}

func TestHandleSmartRetry_QuotaExhausted_UsesCreditsAndStoresIndependentState(t *testing.T) {
	successResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}
	upstream := &mockSmartRetryUpstream{
		responses: []*http.Response{successResp},
		errors:    []error{nil},
	}
	repo := &stubAntigravityAccountRepo{}
	account := &Account{
		ID:       101,
		Name:     "acc-101",
		Type:     AccountTypeOAuth,
		Platform: PlatformAntigravity,
		Extra: map[string]any{
			"allow_overages": true,
		},
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"claude-opus-4-6": "claude-sonnet-4-5",
			},
		},
	}

	respBody := []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"QUOTA_EXHAUSTED"}}`)
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(respBody)),
	}
	params := antigravityRetryLoopParams{
		ctx:            context.Background(),
		prefix:         "[test]",
		account:        account,
		accessToken:    "token",
		action:         "generateContent",
		body:           []byte(`{"model":"claude-opus-4-6","request":{}}`),
		httpUpstream:   upstream,
		accountRepo:    repo,
		requestedModel: "claude-opus-4-6",
		handleError: func(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
			return nil
		},
	}

	svc := &AntigravityGatewayService{}
	result := svc.handleSmartRetry(params, resp, respBody, "https://ag-1.test", 0, []string{"https://ag-1.test"})

	require.NotNil(t, result)
	require.Equal(t, smartRetryActionBreakWithResp, result.action)
	require.NotNil(t, result.resp)
	require.Nil(t, result.switchError)
	require.Len(t, upstream.requestBodies, 1)
	require.Contains(t, string(upstream.requestBodies[0]), "enabledCreditTypes")
	require.Empty(t, repo.modelRateLimitCalls, "overages 成功后不应写入普通 model_rate_limits")
}

func TestHandleSmartRetry_RateLimited_DoesNotUseCredits(t *testing.T) {
	successResp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
	}
	upstream := &mockSmartRetryUpstream{
		responses: []*http.Response{successResp},
		errors:    []error{nil},
	}
	repo := &stubAntigravityAccountRepo{}
	account := &Account{
		ID:       102,
		Name:     "acc-102",
		Type:     AccountTypeOAuth,
		Platform: PlatformAntigravity,
		Extra: map[string]any{
			"allow_overages": true,
		},
	}

	respBody := []byte(`{
		"error": {
			"status": "RESOURCE_EXHAUSTED",
			"details": [
				{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "metadata": {"model": "claude-sonnet-4-5"}, "reason": "RATE_LIMIT_EXCEEDED"},
				{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "0.1s"}
			]
		}
	}`)
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(respBody)),
	}
	params := antigravityRetryLoopParams{
		ctx:          context.Background(),
		prefix:       "[test]",
		account:      account,
		accessToken:  "token",
		action:       "generateContent",
		body:         []byte(`{"model":"claude-sonnet-4-5","request":{}}`),
		httpUpstream: upstream,
		accountRepo:  repo,
		handleError: func(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
			return nil
		},
	}

	svc := &AntigravityGatewayService{}
	result := svc.handleSmartRetry(params, resp, respBody, "https://ag-1.test", 0, []string{"https://ag-1.test"})

	require.NotNil(t, result)
	require.Equal(t, smartRetryActionBreakWithResp, result.action)
	require.NotNil(t, result.resp)
	require.Len(t, upstream.requestBodies, 1)
	require.NotContains(t, string(upstream.requestBodies[0]), "enabledCreditTypes")
	require.Empty(t, repo.extraUpdateCalls)
	require.Empty(t, repo.modelRateLimitCalls)
}

func TestAntigravityRetryLoop_ModelRateLimited_InjectsCredits(t *testing.T) {
	oldBaseURLs := append([]string(nil), antigravity.BaseURLs...)
	oldAvailability := antigravity.DefaultURLAvailability
	defer func() {
		antigravity.BaseURLs = oldBaseURLs
		antigravity.DefaultURLAvailability = oldAvailability
	}()

	antigravity.BaseURLs = []string{"https://ag-1.test"}
	antigravity.DefaultURLAvailability = antigravity.NewURLAvailability(time.Minute)

	upstream := &queuedHTTPUpstreamStub{
		responses: []*http.Response{
			{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
			},
		},
		errors: []error{nil},
	}
	// 模型已限流 + overages 启用 + 无 AICredits key → 应直接注入积分
	account := &Account{
		ID:          103,
		Name:        "acc-103",
		Type:        AccountTypeOAuth,
		Platform:    PlatformAntigravity,
		Status:      StatusActive,
		Schedulable: true,
		Extra: map[string]any{
			"allow_overages": true,
			modelRateLimitsKey: map[string]any{
				"claude-sonnet-4-5": map[string]any{
					"rate_limited_at":     time.Now().UTC().Format(time.RFC3339),
					"rate_limit_reset_at": time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339),
				},
			},
		},
	}

	svc := &AntigravityGatewayService{}
	result, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
		ctx:            context.Background(),
		prefix:         "[test]",
		account:        account,
		accessToken:    "token",
		action:         "generateContent",
		body:           []byte(`{"model":"claude-sonnet-4-5","request":{}}`),
		httpUpstream:   upstream,
		requestedModel: "claude-sonnet-4-5",
		handleError: func(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
			return nil
		},
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.requestBodies, 1)
	require.Contains(t, string(upstream.requestBodies[0]), "enabledCreditTypes")
}

func TestAntigravityRetryLoop_CreditsExhausted_DoesNotInject(t *testing.T) {
	oldBaseURLs := append([]string(nil), antigravity.BaseURLs...)
	oldAvailability := antigravity.DefaultURLAvailability
	defer func() {
		antigravity.BaseURLs = oldBaseURLs
		antigravity.DefaultURLAvailability = oldAvailability
	}()

	antigravity.BaseURLs = []string{"https://ag-1.test"}
	antigravity.DefaultURLAvailability = antigravity.NewURLAvailability(time.Minute)

	// 模型限流 + overages 启用 + AICredits key 生效 → 不应注入积分，应切号
	account := &Account{
		ID:          104,
		Name:        "acc-104",
		Type:        AccountTypeOAuth,
		Platform:    PlatformAntigravity,
		Status:      StatusActive,
		Schedulable: true,
		Extra: map[string]any{
			"allow_overages": true,
			modelRateLimitsKey: map[string]any{
				"claude-sonnet-4-5": map[string]any{
					"rate_limited_at":     time.Now().UTC().Format(time.RFC3339),
					"rate_limit_reset_at": time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339),
				},
				creditsExhaustedKey: map[string]any{
					"rate_limited_at":     time.Now().UTC().Format(time.RFC3339),
					"rate_limit_reset_at": time.Now().Add(5 * time.Hour).UTC().Format(time.RFC3339),
				},
			},
		},
	}

	svc := &AntigravityGatewayService{}
	_, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
		ctx:            context.Background(),
		prefix:         "[test]",
		account:        account,
		accessToken:    "token",
		action:         "generateContent",
		body:           []byte(`{"model":"claude-sonnet-4-5","request":{}}`),
		requestedModel: "claude-sonnet-4-5",
		handleError: func(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
			return nil
		},
	})

	// 模型限流 + 积分耗尽 → 应触发切号错误
	require.Error(t, err)
	var switchErr *AntigravityAccountSwitchError
	require.ErrorAs(t, err, &switchErr)
}

func TestAntigravityRetryLoop_CreditErrorMarksExhausted(t *testing.T) {
	oldBaseURLs := append([]string(nil), antigravity.BaseURLs...)
	oldAvailability := antigravity.DefaultURLAvailability
	defer func() {
		antigravity.BaseURLs = oldBaseURLs
		antigravity.DefaultURLAvailability = oldAvailability
	}()

	antigravity.BaseURLs = []string{"https://ag-1.test"}
	antigravity.DefaultURLAvailability = antigravity.NewURLAvailability(time.Minute)

	repo := &stubAntigravityAccountRepo{}
	upstream := &queuedHTTPUpstreamStub{
		responses: []*http.Response{
			{
				StatusCode: http.StatusForbidden,
				Header:     http.Header{},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"Insufficient GOOGLE_ONE_AI credits"}}`)),
			},
		},
		errors: []error{nil},
	}
	// 模型限流 + overages 启用 + 积分可用 → 注入积分但上游返回积分不足
	account := &Account{
		ID:          105,
		Name:        "acc-105",
		Type:        AccountTypeOAuth,
		Platform:    PlatformAntigravity,
		Status:      StatusActive,
		Schedulable: true,
		Extra: map[string]any{
			"allow_overages": true,
			modelRateLimitsKey: map[string]any{
				"claude-sonnet-4-5": map[string]any{
					"rate_limited_at":     time.Now().UTC().Format(time.RFC3339),
					"rate_limit_reset_at": time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339),
				},
			},
		},
	}

	svc := &AntigravityGatewayService{accountRepo: repo}
	result, err := svc.antigravityRetryLoop(antigravityRetryLoopParams{
		ctx:            context.Background(),
		prefix:         "[test]",
		account:        account,
		accessToken:    "token",
		action:         "generateContent",
		body:           []byte(`{"model":"claude-sonnet-4-5","request":{}}`),
		httpUpstream:   upstream,
		accountRepo:    repo,
		requestedModel: "claude-sonnet-4-5",
		handleError: func(ctx context.Context, prefix string, account *Account, statusCode int, headers http.Header, body []byte, requestedModel string, groupID int64, sessionHash string, isStickySession bool) *handleModelRateLimitResult {
			return nil
		},
	})

	require.NoError(t, err)
	require.NotNil(t, result)
	// 验证 AICredits key 已通过 SetModelRateLimit 写入数据库
	require.Len(t, repo.modelRateLimitCalls, 1, "应通过 SetModelRateLimit 写入 AICredits key")
	require.Equal(t, creditsExhaustedKey, repo.modelRateLimitCalls[0].modelKey)
}

func TestShouldMarkCreditsExhausted(t *testing.T) {
	t.Run("reqErr 不为 nil 时不标记", func(t *testing.T) {
		resp := &http.Response{StatusCode: http.StatusForbidden}
		require.False(t, shouldMarkCreditsExhausted(resp, []byte(`{"error":"Insufficient credits"}`), io.ErrUnexpectedEOF))
	})

	t.Run("resp 为 nil 时不标记", func(t *testing.T) {
		require.False(t, shouldMarkCreditsExhausted(nil, []byte(`{"error":"Insufficient credits"}`), nil))
	})

	t.Run("5xx 响应不标记", func(t *testing.T) {
		resp := &http.Response{StatusCode: http.StatusInternalServerError}
		require.False(t, shouldMarkCreditsExhausted(resp, []byte(`{"error":"Insufficient credits"}`), nil))
	})

	t.Run("408 RequestTimeout 不标记", func(t *testing.T) {
		resp := &http.Response{StatusCode: http.StatusRequestTimeout}
		require.False(t, shouldMarkCreditsExhausted(resp, []byte(`{"error":"Insufficient credits"}`), nil))
	})

	t.Run("Resource has been exhausted 应标记为积分耗尽", func(t *testing.T) {
		resp := &http.Response{StatusCode: http.StatusTooManyRequests}
		body := []byte(`{"error":{"message":"Resource has been exhausted"}}`)
		require.True(t, shouldMarkCreditsExhausted(resp, body, nil))
	})

	t.Run("Resource has been exhausted (check quota) 完整格式应标记", func(t *testing.T) {
		resp := &http.Response{StatusCode: http.StatusTooManyRequests}
		body := []byte(`{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`)
		require.True(t, shouldMarkCreditsExhausted(resp, body, nil))
	})

	t.Run("结构化限流不标记", func(t *testing.T) {
		resp := &http.Response{StatusCode: http.StatusTooManyRequests}
		body := []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.5s"}]}}`)
		require.False(t, shouldMarkCreditsExhausted(resp, body, nil))
	})

	t.Run("含 credits 关键词时标记", func(t *testing.T) {
		resp := &http.Response{StatusCode: http.StatusForbidden}
		for _, keyword := range []string{
			"Insufficient GOOGLE_ONE_AI credits",
			"insufficient credit balance",
			"not enough credits for this request",
			"Credits exhausted",
			"minimumCreditAmountForUsage requirement not met",
		} {
			body := []byte(`{"error":{"message":"` + keyword + `"}}`)
			require.True(t, shouldMarkCreditsExhausted(resp, body, nil), "should mark for keyword: %s", keyword)
		}
	})

	t.Run("无 credits 关键词时不标记", func(t *testing.T) {
		resp := &http.Response{StatusCode: http.StatusForbidden}
		body := []byte(`{"error":{"message":"permission denied"}}`)
		require.False(t, shouldMarkCreditsExhausted(resp, body, nil))
	})
}

func TestInjectEnabledCreditTypes(t *testing.T) {
	t.Run("正常 JSON 注入成功", func(t *testing.T) {
		body := []byte(`{"model":"claude-sonnet-4-5","request":{}}`)
		result := injectEnabledCreditTypes(body)
		require.NotNil(t, result)
		require.Contains(t, string(result), `"enabledCreditTypes"`)
		require.Contains(t, string(result), `GOOGLE_ONE_AI`)
	})

	t.Run("非法 JSON 返回 nil", func(t *testing.T) {
		require.Nil(t, injectEnabledCreditTypes([]byte(`not json`)))
	})

	t.Run("空 body 返回 nil", func(t *testing.T) {
		require.Nil(t, injectEnabledCreditTypes([]byte{}))
	})

	t.Run("已有 enabledCreditTypes 会被覆盖", func(t *testing.T) {
		body := []byte(`{"enabledCreditTypes":["OLD"],"model":"test"}`)
		result := injectEnabledCreditTypes(body)
		require.NotNil(t, result)
		require.Contains(t, string(result), `GOOGLE_ONE_AI`)
		require.NotContains(t, string(result), `OLD`)
	})
}

func TestClearCreditsExhausted(t *testing.T) {
	t.Run("account 为 nil 不操作", func(t *testing.T) {
		repo := &stubAntigravityAccountRepo{}
		svc := &AntigravityGatewayService{accountRepo: repo}
		svc.clearCreditsExhausted(context.Background(), nil)
		require.Empty(t, repo.extraUpdateCalls)
	})

	t.Run("Extra 为 nil 不操作", func(t *testing.T) {
		repo := &stubAntigravityAccountRepo{}
		svc := &AntigravityGatewayService{accountRepo: repo}
		svc.clearCreditsExhausted(context.Background(), &Account{ID: 1})
		require.Empty(t, repo.extraUpdateCalls)
	})

	t.Run("无 modelRateLimitsKey 不操作", func(t *testing.T) {
		repo := &stubAntigravityAccountRepo{}
		svc := &AntigravityGatewayService{accountRepo: repo}
		svc.clearCreditsExhausted(context.Background(), &Account{
			ID:    1,
			Extra: map[string]any{"some_key": "value"},
		})
		require.Empty(t, repo.extraUpdateCalls)
	})

	t.Run("无 AICredits key 不操作", func(t *testing.T) {
		repo := &stubAntigravityAccountRepo{}
		svc := &AntigravityGatewayService{accountRepo: repo}
		svc.clearCreditsExhausted(context.Background(), &Account{
			ID: 1,
			Extra: map[string]any{
				modelRateLimitsKey: map[string]any{
					"claude-sonnet-4-5": map[string]any{
						"rate_limited_at":     "2026-03-15T00:00:00Z",
						"rate_limit_reset_at": "2099-03-15T00:00:00Z",
					},
				},
			},
		})
		require.Empty(t, repo.extraUpdateCalls)
	})

	t.Run("有 AICredits key 但无 probe_gen 时仅本地删除不写 DB", func(t *testing.T) {
		repo := &creditsPatchRepo{stubAntigravityAccountRepo: &stubAntigravityAccountRepo{}}
		svc := &AntigravityGatewayService{accountRepo: repo}
		account := &Account{
			ID: 1,
			Extra: map[string]any{
				modelRateLimitsKey: map[string]any{
					"claude-sonnet-4-5": map[string]any{
						"rate_limited_at":     "2026-03-15T00:00:00Z",
						"rate_limit_reset_at": "2099-03-15T00:00:00Z",
					},
					creditsExhaustedKey: map[string]any{
						"rate_limited_at":     "2026-03-15T00:00:00Z",
						"rate_limit_reset_at": time.Now().Add(5 * time.Hour).UTC().Format(time.RFC3339),
					},
				},
			},
		}
		svc.clearCreditsExhausted(context.Background(), account)
		// 无 probe_gen：仅本地删除，不写 DB（既无整桶 UpdateExtra，也无条目级 patch）。
		require.Empty(t, repo.extraUpdateCalls)
		require.Empty(t, repo.patchCalls)
		// AICredits key 应被删除
		rawLimits := account.Extra[modelRateLimitsKey].(map[string]any)
		_, exists := rawLimits[creditsExhaustedKey]
		require.False(t, exists, "AICredits key 应被删除")
		// 普通模型限流应保留
		_, exists = rawLimits["claude-sonnet-4-5"]
		require.True(t, exists, "普通模型限流应保留")
	})

	t.Run("有 AICredits key 且带 probe_gen 时按 gen 条目级删除", func(t *testing.T) {
		repo := &creditsPatchRepo{stubAntigravityAccountRepo: &stubAntigravityAccountRepo{}}
		svc := &AntigravityGatewayService{accountRepo: repo}
		account := &Account{
			ID: 2,
			Extra: map[string]any{
				modelRateLimitsKey: map[string]any{
					"claude-sonnet-4-5": map[string]any{
						"rate_limited_at":     "2026-03-15T00:00:00Z",
						"rate_limit_reset_at": "2099-03-15T00:00:00Z",
					},
					creditsExhaustedKey: map[string]any{
						"rate_limited_at":         "2026-03-15T00:00:00Z",
						"rate_limit_reset_at":     time.Now().Add(5 * time.Hour).UTC().Format(time.RFC3339),
						modelRateLimitProbeGenKey: int64(42),
					},
				},
			},
		}
		svc.clearCreditsExhausted(context.Background(), account)
		// 带 probe_gen：恰好一次条目级删除 patch，携带正确的 gen 与 scope，不写整桶。
		require.Len(t, repo.patchCalls, 1)
		require.Equal(t, creditsExhaustedKey, repo.patchCalls[0].scope)
		require.Equal(t, int64(42), repo.patchCalls[0].expectedGen)
		require.Equal(t, ModelProbePatchDeleteScope, repo.patchCalls[0].patch.Op)
		require.Empty(t, repo.extraUpdateCalls)
		// AICredits key 本地已删除，普通模型限流保留。
		rawLimits := account.Extra[modelRateLimitsKey].(map[string]any)
		_, exists := rawLimits[creditsExhaustedKey]
		require.False(t, exists, "AICredits key 应被删除")
		_, exists = rawLimits["claude-sonnet-4-5"]
		require.True(t, exists, "普通模型限流应保留")
	})
}
