//go:build unit

package handler

// 本文件覆盖 **§25.17.5 A06** 在 /v1/messages gemini 分支的唯一结算注册：
//
//  1. Cursor 账号：registerMessagesGeminiCursorSettlement 返回 true，handover 上确有
//     结算钩子（finalizer 侧访问器 SettlementFunc() 非 nil）；
//  2. 非 Cursor 账号：返回 false 且不注册（SettlementFunc() 为 nil），调用方照旧直接提交；
//  3. "直接提交被跳过"的判定 cursorAccountBilledBySession：cursor=true / anthropic=false。
//
// 背景：gemini 分支此前漏注册 → 带工具的多轮会话（首轮只交付工具批、后续轮走续轮
// 不经过本 handler）整段不计费，且会话终结时报 "settlement func not registered" ERROR。

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestRegisterMessagesGeminiCursorSettlementRegistersForCursor(t *testing.T) {
	h := &GatewayHandler{}
	account := &service.Account{ID: 440, Platform: service.PlatformCursor}
	apiKey := &service.APIKey{ID: 7, User: &service.User{ID: 9}, GroupID: int64Ptr(3)}
	handover := newCursorResourceHandover(nil, nil)

	registered := h.registerMessagesGeminiCursorSettlement(handover, account, cursorSettlementSnapshot{
		apiKey:             apiKey,
		user:               apiKey.User,
		account:            account,
		pricingAt:          time.Now(),
		quotaPlatform:      service.QuotaPlatform(context.Background(), apiKey),
		inboundEndpoint:    "/v1/messages",
		upstreamEndpoint:   "/v1/messages",
		userAgent:          "cursor-test",
		ipAddress:          "127.0.0.1",
		sessionID:          "sess-cursor",
		requestPayloadHash: "hash-cursor",
		channelMapping:     service.ChannelMappingResult{},
		requestedModel:     "claude-sonnet",
		userID:             9,
		groupID:            apiKey.GroupID,
		reqModel:           "claude-sonnet",
	})

	require.True(t, registered, "Cursor 账号必须注册会话唯一结算")
	require.NotNil(t, handover.SettlementFunc(), "注册后 finalizer 侧必须能取出已注册的结算钩子")
	require.NoError(t, handover.SettlementError(), "正常时机的注册不得产生装配错误")
}

func TestRegisterMessagesGeminiCursorSettlementSkipsNonCursor(t *testing.T) {
	h := &GatewayHandler{}
	account := &service.Account{ID: 12, Platform: service.PlatformAnthropic}
	handover := newCursorResourceHandover(nil, nil)

	registered := h.registerMessagesGeminiCursorSettlement(handover, account, cursorSettlementSnapshot{
		account:  account,
		reqModel: "claude-sonnet",
	})

	require.False(t, registered, "非 Cursor 账号不得注册会话唯一结算")
	require.Nil(t, handover.SettlementFunc(), "非 Cursor 账号不得留下结算钩子")
}

func TestCursorAccountBilledBySession(t *testing.T) {
	require.True(t, cursorAccountBilledBySession(&service.Account{Platform: service.PlatformCursor}),
		"Cursor 账号的账只由会话终态结算驱动，直接提交必须被跳过")
	require.False(t, cursorAccountBilledBySession(&service.Account{Platform: service.PlatformAnthropic}),
		"非 Cursor 账号保持原直接提交路径")
	require.False(t, cursorAccountBilledBySession(nil), "nil 账号不得被判定为会话结算")
}
