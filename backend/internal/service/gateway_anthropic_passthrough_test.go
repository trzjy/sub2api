//go:build unit

package service

// 文件：internal/service/gateway_anthropic_passthrough_test.go
//
// CF524 G2a 挂点 2（gateway_anthropic_passthrough.go APIKey 直通，/v1/messages）
// 单测。共享 g2a 前缀的 mock 与注入 helper（见 gateway_forward_test.go）。

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func g2aAnthropicPassthroughAccount() *Account {
	return &Account{
		ID: 902, Name: "g2a-anthropic-passthrough", Platform: PlatformAnthropic, Type: AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{"api_key": "g2a-upstream-key", "base_url": "https://api.anthropic.com"},
		Extra:       map[string]any{"anthropic_passthrough": true},
		Status:      StatusActive, Schedulable: true,
	}
}

// 挂点 2 claude-sonnet-5 API-key 直通形态：上游悬挂 > guard → 心跳先起、guard 触发后
// stop-and-wait 停心跳再返回可换号错误。
func TestG2AMount2ClaudeSonnet5APIKeyPassthroughHangHeartbeatThenGuardFailover(t *testing.T) {
	upstream := &g2aMockUpstream{} // 默认悬挂
	svc := g2aService(upstream)
	body := g2aStreamBody("claude-sonnet-5")
	parsed := g2aParsed(t, body)
	_, c, obs := g2aGinContext(http.MethodPost, "/v1/messages", body)

	snap := g2aSnapshot(1, 1, 30*time.Second)
	hb := g2aHeartbeat(context.Background(), obs, 60*time.Millisecond, 40*time.Millisecond)
	ctx := g2aInject(context.Background(), snap, hb)

	start := time.Now()
	_, err := svc.Forward(ctx, c, g2aAnthropicPassthroughAccount(), parsed)
	elapsed := time.Since(start)

	var fe *UpstreamFailoverError
	require.ErrorAs(t, err, &fe)
	require.Equal(t, http.StatusGatewayTimeout, fe.StatusCode)
	require.Contains(t, string(fe.ResponseBody), "upstream_first_byte_timeout")
	require.True(t, fe.ShouldRetryNextAccount(), "剩余预算 ≥5s 必须允许换号")
	require.True(t, obs.HeaderWritten(), "guard 触发前心跳必须已提交 SSE 200")
	require.Greater(t, obs.heartbeatFrameCount(), 0, "guard 触发前必须已收到 keep-alive 帧")
	require.True(t, hb.IsCommitted())

	select {
	case <-hb.doneCh:
	default:
		t.Fatal("heartbeat goroutine must have exited before failover (stop-and-wait)")
	}
	require.GreaterOrEqual(t, elapsed, 900*time.Millisecond)
	require.Less(t, elapsed, 5*time.Second)
}

// 挂点 2 剩余预算 <5s → NextAccountStop（耗尽）。
func TestG2AMount2ClaudeSonnet5APIKeyPassthroughHangExhausted(t *testing.T) {
	upstream := &g2aMockUpstream{}
	svc := g2aService(upstream)
	body := g2aStreamBody("claude-sonnet-5")
	_, c, obs := g2aGinContext(http.MethodPost, "/v1/messages", body)

	snap := g2aSnapshot(1, 1, 1200*time.Millisecond)
	hb := g2aHeartbeat(context.Background(), obs, 60*time.Millisecond, 40*time.Millisecond)
	ctx := g2aInject(context.Background(), snap, hb)

	_, err := svc.Forward(ctx, c, g2aAnthropicPassthroughAccount(), g2aParsed(t, body))

	var fe *UpstreamFailoverError
	require.ErrorAs(t, err, &fe)
	require.False(t, fe.ShouldRetryNextAccount(), "剩余预算 <5s 必须耗尽")
	require.Equal(t, NextAccountStop, fe.NextAccountAction)
}

// 挂点 2 重放一致性：buildUpstreamRequestAnthropicAPIKeyPassthrough 从同一 body 重建。
func TestG2AMount2ReplayWireBodyConsistent(t *testing.T) {
	svc := &GatewayService{cfg: &config.Config{}}
	account := g2aAnthropicPassthroughAccount()
	body := g2aStreamBody("claude-sonnet-5")
	_, c, _ := g2aGinContext(http.MethodPost, "/v1/messages", nil)

	req1, wire1, err1 := svc.buildUpstreamRequestAnthropicAPIKeyPassthrough(context.Background(), c, account, body, "g2a-upstream-key")
	require.NoError(t, err1)
	req2, wire2, err2 := svc.buildUpstreamRequestAnthropicAPIKeyPassthrough(context.Background(), c, account, body, "g2a-upstream-key")
	require.NoError(t, err2)

	require.Equal(t, wire1, wire2, "换号前后 wireBody 必须完全一致（可重放契约）")
	require.Equal(t, req1.URL.String(), req2.URL.String())
}

// 挂点 2 健康路径零回归：上游 <delay 正常返回 → 零心跳帧、状态码/流透传一致。
func TestG2AMount2HealthyPathZeroHeartbeat(t *testing.T) {
	upstream := &g2aMockUpstream{steps: []g2aCall{g2aSuccessFn(g2aAnthropicSSE)}}
	svc := g2aService(upstream)
	body := g2aStreamBody("claude-sonnet-5")
	rec, c, obs := g2aGinContext(http.MethodPost, "/v1/messages", body)

	snap := g2aSnapshot(1, 1, 30*time.Second)
	hb := g2aHeartbeat(context.Background(), obs, 600*time.Millisecond, 100*time.Millisecond)
	ctx := g2aInject(context.Background(), snap, hb)

	result, err := svc.Forward(ctx, c, g2aAnthropicPassthroughAccount(), g2aParsed(t, body))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, hb.IsCommitted())
	require.Equal(t, 0, obs.heartbeatFrameCount())
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "message_stop")
}
