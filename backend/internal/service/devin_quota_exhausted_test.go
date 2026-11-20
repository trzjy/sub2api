//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// devinQuotaRepoStub 是驱动熔断分支的最小 AccountRepository 替身：只实现
// SetTempUnschedulable，其余方法由嵌入的 nil 接口暴露，误用会立即 panic。
// 自包含定义使本测试不依赖同包其它测试文件的私有类型。
type devinQuotaRepoStub struct {
	AccountRepository
	calls []devinQuotaUnschedCall
}

type devinQuotaUnschedCall struct {
	accountID int64
	until     time.Time
	reason    string
	ctxErr    error // 写入时上下文的 Err()，为 nil 证明停调写入未继承请求取消
}

func (r *devinQuotaRepoStub) SetTempUnschedulable(ctx context.Context, id int64, until time.Time, reason string) error {
	r.calls = append(r.calls, devinQuotaUnschedCall{accountID: id, until: until, reason: reason, ctxErr: ctx.Err()})
	return nil
}

func TestIsDevinUpstreamQuotaExhausted(t *testing.T) {
	devinAccount := &Account{ID: 992, Platform: PlatformDevin}
	realPayload := []byte(`{"error":{"code":"failed_precondition","message":"Your daily usage quota has been exhausted. Visit https://app.devin.ai/settings/usage to purchase on-demand usage or turn on auto-reload. (trace ID: 7f3c9a)"}}`)

	tests := []struct {
		name    string
		account *Account
		body    []byte
		want    bool
	}{
		{
			name:    "real quota exhausted payload with trace id",
			account: devinAccount,
			body:    realPayload,
			want:    true,
		},
		{
			name:    "failed_precondition but safety message",
			account: devinAccount,
			body:    []byte(`{"error":{"code":"failed_precondition","message":"safety check failed"}}`),
			want:    false,
		},
		{
			name:    "quota message but non-matching code",
			account: devinAccount,
			body:    []byte(`{"error":{"code":"other","message":"Your daily usage quota has been exhausted."}}`),
			want:    false,
		},
		{
			name:    "non-devin platform",
			account: &Account{ID: 1, Platform: PlatformOpenAI},
			body:    realPayload,
			want:    false,
		},
		{
			name:    "non-json body",
			account: devinAccount,
			body:    []byte(`not-json-at-all`),
			want:    false,
		},
		{
			name:    "nil account",
			account: nil,
			body:    realPayload,
			want:    false,
		},
		{
			name:    "empty body",
			account: devinAccount,
			body:    nil,
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isDevinUpstreamQuotaExhausted(tt.account, tt.body))
		})
	}
}

func TestDevinQuotaExhaustedProbeWindow(t *testing.T) {
	now := time.Date(2026, 11, 20, 15, 30, 45, 123, time.UTC)
	until := devinQuotaExhaustedProbeWindowUntil(now)

	want := now.Add(unavailQuotaUnknownCooldown)
	require.WithinDuration(t, want, until, 2*time.Second, "probe window must be now + unavailQuotaUnknownCooldown")
	require.True(t, until.After(now), "probe window must be later than now")
	require.True(t, until.Sub(now) <= unavailMaxCooldown, "probe window must not exceed unavailMaxCooldown")
}

func TestDevinQuotaExhausted_HandleUpstreamErrorFailsOverAndUnschedules(t *testing.T) {
	repo := &devinQuotaRepoStub{}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := &Account{ID: 992, Platform: PlatformDevin}
	body := []byte(`{"error":{"code":"failed_precondition","message":"Your daily usage quota has been exhausted. Visit https://app.devin.ai/settings/usage to purchase on-demand usage or turn on auto-reload. (trace ID: 7f3c9a)"}}`)

	got := svc.handleOpenAIAccountUpstreamError(context.Background(), account, http.StatusOK, nil, body)

	require.True(t, got, "quota exhausted must return true so the handler fails over to the next account")
	require.Len(t, repo.calls, 1, "account must be written to temp-unschedulable for the recovery probe")

	call := repo.calls[0]
	require.Equal(t, account.ID, call.accountID)
	require.True(t, call.until.After(time.Now()), "persisted upper bound must be in the future")

	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(call.reason), &state))
	require.Equal(t, devinUpstreamQuotaExhaustedMarker, state.MatchedKeyword, "reason must carry the probe-recoverable marker")
	require.Equal(t, http.StatusOK, state.StatusCode)
	require.NotEmpty(t, state.ErrorMessage)
}

// TestDevinQuotaMarkerIsProbeRecoverable 验证 A-2 闭合：停调写入的 marker 必须登记在
// probeRecoverableMarkers 中，且该 reason 能被 isHealthBreakerTrip 判为「可探测恢复」。
func TestDevinQuotaMarkerIsProbeRecoverable(t *testing.T) {
	found := false
	for _, marker := range probeRecoverableMarkers {
		if marker == devinUpstreamQuotaExhaustedMarker {
			found = true
			break
		}
	}
	require.True(t, found, "devin quota marker must be registered in probeRecoverableMarkers")

	until := time.Now().Add(time.Hour)
	payload, err := json.Marshal(TempUnschedState{
		UntilUnix:       until.Unix(),
		TriggeredAtUnix: time.Now().Unix(),
		StatusCode:      http.StatusOK,
		MatchedKeyword:  devinUpstreamQuotaExhaustedMarker,
		ErrorMessage:    "devin upstream daily usage quota exhausted",
	})
	require.NoError(t, err)

	account := &Account{
		ID:                      992,
		Platform:                PlatformDevin,
		TempUnschedulableUntil:  &until,
		TempUnschedulableReason: string(payload),
	}
	probe := &AccountHealthRecoveryProbeService{}
	require.True(t, probe.isHealthBreakerTrip(account, time.Now()),
		"quota-exhausted park must be picked up by the recovery probe")
}

func TestParkDevinQuotaExhaustedWritesMarker(t *testing.T) {
	repo := &devinQuotaRepoStub{}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := &Account{ID: 992, Platform: PlatformDevin}
	before := time.Now()

	svc.parkDevinQuotaExhausted(context.Background(), account)

	require.Len(t, repo.calls, 1, "must write the account to temp-unschedulable exactly once")
	call := repo.calls[0]
	require.Equal(t, account.ID, call.accountID)
	require.True(t, call.until.After(before), "park window must be later than now")

	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(call.reason), &state))
	require.Equal(t, devinUpstreamQuotaExhaustedMarker, state.MatchedKeyword,
		"reason must carry the probe-recoverable marker")
	require.Equal(t, http.StatusOK, state.StatusCode)
	require.NotEmpty(t, state.ErrorMessage)
}

// TestParkDevinQuotaExhaustedSurvivesCanceledContext 覆盖外审 must_fix：
// 流式排水阶段客户端断开后请求 ctx 已取消，停调写入必须用独立上下文，
// 否则 SetTempUnschedulable 会被中止而丢失停调，账号下次继续被调度。
func TestParkDevinQuotaExhaustedSurvivesCanceledContext(t *testing.T) {
	repo := &devinQuotaRepoStub{}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := &Account{ID: 992, Platform: PlatformDevin}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 模拟客户端断开后已取消的请求 ctx

	svc.parkDevinQuotaExhausted(ctx, account)

	require.Len(t, repo.calls, 1, "canceled parent ctx must not drop the park write")
	require.NoError(t, repo.calls[0].ctxErr,
		"write ctx must not inherit the parent cancellation (ctx.Err() must be nil)")

	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(repo.calls[0].reason), &state))
	require.Equal(t, devinUpstreamQuotaExhaustedMarker, state.MatchedKeyword)
}

func TestParkDevinQuotaExhaustedNilSafety(t *testing.T) {
	repo := &devinQuotaRepoStub{}
	svc := &OpenAIGatewayService{accountRepo: repo}

	require.NotPanics(t, func() {
		svc.parkDevinQuotaExhausted(context.Background(), nil)
	}, "nil account must not panic")
	require.Empty(t, repo.calls, "nil account must not write")

	noRepoSvc := &OpenAIGatewayService{}
	require.NotPanics(t, func() {
		noRepoSvc.parkDevinQuotaExhausted(context.Background(), &Account{ID: 1, Platform: PlatformDevin})
	}, "nil accountRepo must not panic")

	var nilSvc *OpenAIGatewayService
	require.NotPanics(t, func() {
		nilSvc.parkDevinQuotaExhausted(context.Background(), &Account{ID: 1, Platform: PlatformDevin})
	}, "nil service must not panic")

	require.Empty(t, repo.calls, "no invalid input may produce a write")
}

// TestParkDevinQuotaExhaustedOnMidstreamTrailer 覆盖「正文已写出后收到额度耗尽
// trailer」的完整流式路径：响应已提交无法换号，但账号必须被停调。
func TestParkDevinQuotaExhaustedOnMidstreamTrailer(t *testing.T) {
	repo := &devinQuotaRepoStub{}
	svc := &OpenAIGatewayService{accountRepo: repo}
	account := &Account{ID: 992, Platform: PlatformDevin}

	// 第一帧：一段正文，令 written=true（响应已提交，无法换号）。
	first := &ProtoEncoder{}
	first.String(3, "partial answer")
	first.Uint64(5, 3)
	// 第二帧：END_STREAM trailer，Devin 额度耗尽结构化错误。
	trailer := []byte(`{"error":{"code":"failed_precondition","message":"Your daily usage quota has been exhausted."}}`)
	body := append(
		makeDevinFrame(t, 0, first.Bytes()),
		makeDevinFrame(t, ConnectEndStreamFlag, trailer)...,
	)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	result, err := svc.handleDevinStreamingResponse(
		context.Background(), resp, c, account,
		"devin-model", "devin-model", time.Now(),
		webResponseModeChat, false, nil,
	)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Len(t, repo.calls, 1, "midstream quota trailer must park the account even after bytes were written")
	var state TempUnschedState
	require.NoError(t, json.Unmarshal([]byte(repo.calls[0].reason), &state))
	require.Equal(t, devinUpstreamQuotaExhaustedMarker, state.MatchedKeyword)
}
