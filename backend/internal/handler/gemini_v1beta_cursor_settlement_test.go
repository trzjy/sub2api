//go:build unit

package handler

// 本文件覆盖 CARD-KC-28 在 Gemini 原生入口（/v1beta/models/...:generateContent）的用量结算
// 分派：把"该账号的用量是否由会话终态唯一结算驱动"的判定，从 isCursorPlatformAccount 改为
// cursorAccountBilledBySession。
//
// 场景矩阵（与 KC-06 在 /v1/chat/completions 入口修法一致）：
//  1. cursor + oauth（Connect 会话式）账号：仍注册会话结算钩子、不直接提交（既有行为不变）；
//  2. cursor + apikey（kn-cy 等 OpenAI 兼容反代）账号：不注册会话结算钩子、走 handler 直接提交；
//  3. 非 cursor 账号：行为不变（不注册钩子、直接提交）。
//
// 直接提交被跳过/被启用的判定封装在 geminiV1BetaBillsByDirectSubmit，与注册侧
// registerGeminiV1BetaCursorSettlement 共用 cursorAccountBilledBySession，两端一致。

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// cursorAPIKeyAccount 是 KC-28 主角的账号形态：cursor 平台 + APIKey 反代（如 kn-cy.com）。
func cursorAPIKeyAccount() *service.Account {
	return &service.Account{ID: 548, Platform: service.PlatformCursor, Type: service.AccountTypeAPIKey}
}

// cursorOAuthAccount 是既有会话式 Connect 账号（ShareAI 号池 OAuth）。
func cursorOAuthAccount() *service.Account {
	return &service.Account{ID: 501, Platform: service.PlatformCursor, Type: service.AccountTypeOAuth}
}

func nonCursorAccount() *service.Account {
	return &service.Account{ID: 99, Platform: service.PlatformGemini}
}

// TestRegisterGeminiV1BetaCursorSettlement 断言 Gemini 原生入口的会话结算钩子注册分派：
//   - cursor+oauth 注册（既有行为不变）；
//   - cursor+apikey 不注册（否则其无 Connect 会话可挂钩子 → 永远漏账）；
//   - 非 cursor 不注册（沿用直接提交）。
func TestRegisterGeminiV1BetaCursorSettlement(t *testing.T) {
	tests := []struct {
		name     string
		account  *service.Account
		wantReg  bool
		wantHook bool
	}{
		{"cursor+oauth 注册会话唯一结算", cursorOAuthAccount(), true, true},
		{"cursor+apikey 不注册（避免漏账）", cursorAPIKeyAccount(), false, false},
		{"非 cursor 不注册（沿用直接提交）", nonCursorAccount(), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &GatewayHandler{}
			handover := newCursorResourceHandover(nil, nil)
			registered := h.registerGeminiV1BetaCursorSettlement(
				handover,
				tt.account,
				geminiCursorSettlementParams{account: tt.account, pricingAt: time.Now()},
				func(context.Context, service.UsageRecordTask) {},
			)
			require.Equal(t, tt.wantReg, registered, "注册返回值须与判定一致")
			if tt.wantHook {
				require.NotNil(t, handover.SettlementFunc(), "cursor+oauth 必须挂上会话结算钩子")
			} else {
				require.Nil(t, handover.SettlementFunc(), "非会话式账号不得留下结算钩子")
			}
		})
	}
}

// TestGeminiV1BetaBillsByDirectSubmit 断言 handler 直接提交用量的门控（与注册侧互补）：
//   - cursor+oauth 为 false（绝不直接提交，否则与终结结算冲突、重复计费）；
//   - cursor+apikey 为 true（无 Connect 会话 → 必须 handler 直接提交，否则漏账）；
//   - 非 cursor 为 true（行为不变）。
func TestGeminiV1BetaBillsByDirectSubmit(t *testing.T) {
	require.False(t, geminiV1BetaBillsByDirectSubmit(cursorOAuthAccount()),
		"cursor+oauth 会话式账号绝不直接提交（与终结结算冲突）")
	require.True(t, geminiV1BetaBillsByDirectSubmit(cursorAPIKeyAccount()),
		"cursor+apikey 反代账号必须改由 handler 直接提交（避免漏账）")
	require.True(t, geminiV1BetaBillsByDirectSubmit(nonCursorAccount()),
		"非 cursor 账号保持原有直接提交路径不变")
}

// TestGeminiV1BetaSettlementRoutingConsistency 文档化并验证两个判定互补、不重不漏：
// 任一账号"注册会话结算"与"直接提交"恰好互斥（不会既漏账又重复计费）。
func TestGeminiV1BetaSettlementRoutingConsistency(t *testing.T) {
	accounts := []*service.Account{cursorOAuthAccount(), cursorAPIKeyAccount(), nonCursorAccount()}
	for _, acc := range accounts {
		registered := cursorAccountBilledBySession(acc) // registerGeminiV1BetaCursorSettlement 的判定
		direct := geminiV1BetaBillsByDirectSubmit(acc)
		require.NotEqual(t, registered, direct,
			"账号 %+v：注册会话结算与直接提交必须互斥", acc)
	}
}
