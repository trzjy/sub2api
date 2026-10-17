//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// CUR-F6 R3-1：activate 限频的 5min 冷却窗口必须生效。
//
// 复现真实 activate 路径（refresh 返回空 metadata → activate 返回 code=1 + 限频文案），
// 验证该限频错误经多层包装后，writeCooldownMarker 落库的 last_refresh_error 仍以
// ErrUpstreamCooldown 哨兵前缀开头 → isCooldownRefreshError 判真 → ListCandidateKeys
// 在 5min 窗口内排除该 key、窗口外自动回入选。
// ---------------------------------------------------------------------------

// TestCURF6_ActivateCooldown_WritesCooldownMarkerPrefix：activate 限频落库文本必须以
// ErrUpstreamCooldown 哨兵前缀开头（R3-1 核心修复点）。
func TestCURF6_ActivateCooldown_WritesCooldownMarkerPrefix(t *testing.T) {
	cooldownMsg := "每5分钟内仅可刷新1次，请稍后再试"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/refresh":
			// 首次 refresh 返回空 metadata → 触发 activate。
			json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 0, "msg": "ok",
				"data": map[string]interface{}{"metadata": "", "mail": "", "timestamp": ""},
			})
		case "/api/activate":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 1, "msg": cooldownMsg, "data": nil,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	up := newTestClient(t, handler)
	svc, client := qkNewTestService(t, up)
	ctx := context.Background()
	qk := mustCreateQuotaKey(t, client, "active")

	err := svc.PullAccounts(ctx, qk.ID)
	require.Error(t, err, "activate cooldown must surface the sentinel")
	require.True(t, errors.Is(err, ErrUpstreamCooldown),
		"activate cooldown must remain recognizable via errors.Is")

	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastRefreshError, "cooldown must write last_refresh_error")
	require.NotNil(t, got.LastRefreshAt, "cooldown must write last_refresh_at")
	require.False(t, got.Exhausted, "cooldown must NOT set exhausted (B2)")

	// 关键：落库文本必须以冷却哨兵前缀开头（R3-1 修复：不再受 activate 中间包装层影响）。
	require.True(t, isCooldownRefreshError(got.LastRefreshError),
		"activate cooldown marker must be recognized by isCooldownRefreshError; got=%q", *got.LastRefreshError)
	// 原文必须保留在哨兵之后。
	require.Contains(t, *got.LastRefreshError, cooldownMsg,
		"cooldown marker must retain the original upstream msg")
}

// TestCURF6_ActivateCooldown_ExcludedByCandidateWindow：窗口内排除、窗口外自动回入选。
func TestCURF6_ActivateCooldown_ExcludedByCandidateWindow(t *testing.T) {
	cooldownMsg := "每5分钟内仅可刷新1次，请稍后再试"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/refresh":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 0, "msg": "ok",
				"data": map[string]interface{}{"metadata": "", "mail": "", "timestamp": ""},
			})
		case "/api/activate":
			json.NewEncoder(w).Encode(map[string]interface{}{
				"code": 1, "msg": cooldownMsg, "data": nil,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	up := newTestClient(t, handler)
	svc, client := qkNewTestService(t, up)
	ctx := context.Background()
	qk := mustCreateQuotaKey(t, client, "active")

	require.Error(t, svc.PullAccounts(ctx, qk.ID))

	water := &quotaKeyModelWater{client: client}

	// 窗口内（last_refresh_at 刚刚写入）→ 排除候选。
	cands, err := water.ListCandidateKeys(ctx, "cursor-f6-cool", 0)
	require.NoError(t, err)
	require.NotContains(t, cands, qk.ID, "activate cooldown within 5min window must exclude the key")

	// 窗口外（把 last_refresh_at 推到 10 分钟前）→ 自动回入选（不等人工 clear）。
	_, err = client.QuotaKey.UpdateOneID(qk.ID).
		SetLastRefreshAt(time.Now().Add(-10 * time.Minute)).
		Save(ctx)
	require.NoError(t, err)
	cands, err = water.ListCandidateKeys(ctx, "cursor-f6-cool", 0)
	require.NoError(t, err)
	require.Contains(t, cands, qk.ID, "activate cooldown past 5min window must auto re-include the key")
}

// ---------------------------------------------------------------------------
// CUR-F6 R3-2：非 code=1 业务码的手动响应也必须原文无前缀。
//
// 上游返回 code=2 + 中文 msg（如"参数错误"）→ 手动刷新响应的 message 必须为原文
// （不含 "business code" / "shareai:"）；自动链路落库文本保留业务码上下文（排障不丢失）。
// ---------------------------------------------------------------------------

func TestCURF6_ManualResponse_NonZeroCodeOriginalMessage(t *testing.T) {
	bizMsg := "参数错误"
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// refresh 直接返回非 code=1 业务码（code=2），default 分支处理。
		json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 2, "msg": bizMsg, "data": nil,
		})
	})
	up := newTestClient(t, handler)
	svc, client := qkNewTestService(t, up)
	ctx := context.Background()
	qk := mustCreateQuotaKey(t, client, "active")

	err := svc.RefreshAccounts(ctx, qk.ID)
	require.Error(t, err, "manual refresh must surface the upstream error (not success)")

	var upe *UpstreamPullError
	require.True(t, errors.As(err, &upe), "non-zero business code must return *UpstreamPullError")
	require.Equal(t, UpstreamUnknown, upe.Kind, "non code=1 business code is UpstreamUnknown")
	// 手动响应 message 必须为原文，不带英文前缀。
	require.Equal(t, bizMsg, upe.Message(), "manual response message must be the original msg verbatim")
	require.NotContains(t, upe.Message(), "business code",
		"manual response must NOT carry the english 'business code' prefix")
	require.NotContains(t, upe.Message(), "shareai:",
		"manual response must NOT carry the 'shareai:' prefix")

	// 自动链路落库文本保留业务码上下文（排障不丢失）。
	got, err := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, err)
	require.NotNil(t, got.LastRefreshError, "non-zero business code must write last_refresh_error")
	require.Contains(t, *got.LastRefreshError, "business code 2",
		"DB last_refresh_error must retain business code context for troubleshooting")
	require.Contains(t, *got.LastRefreshError, bizMsg,
		"DB last_refresh_error must retain the original msg")
}
