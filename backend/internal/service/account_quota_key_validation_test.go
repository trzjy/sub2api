package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// TestNonCursorAccountCreateRejectsQuotaKey 端到端锁定 CreateAccount 已接入反向拒绝：
// 非 cursor 平台建号携带 quota_key_id 必须 400，且错误指向 quota_key_id。
func TestNonCursorAccountCreateRejectsQuotaKey(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{}
	qkID := int64(7)

	_, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "openai-with-quota-key",
		Platform:             PlatformOpenAI,
		Type:                 AccountTypeAPIKey,
		Credentials:          map[string]any{"api_key": "sk-test"},
		SkipDefaultGroupBind: true,
		QuotaKeyID:           &qkID,
	})
	require.Error(t, err, "non-cursor account creation with quota_key_id must be rejected")
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, "ACCOUNT_QUOTA_KEY_NOT_ALLOWED_FOR_PLATFORM", infraerrors.Reason(err))
	require.Contains(t, err.Error(), "quota_key_id")

	// 不携带归属 → 正常建号（NULL 保持）。
	created, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "openai-null-quota-key",
		Platform:             PlatformOpenAI,
		Type:                 AccountTypeAPIKey,
		Credentials:          map[string]any{"api_key": "sk-test"},
		SkipDefaultGroupBind: true,
	})
	require.NoError(t, err, "non-cursor account creation with NULL quota_key_id must pass")
	require.NotNil(t, created)
	require.Nil(t, created.QuotaKeyID)
}

// TestQuotaKeyBindingValidation 锁定 v3.3 §4.2 / C2 与 v3.4 §2.1 的应用层校验逻辑：
//   - cursor 平台 + quota_key_id 为 nil → 报错（必填）
//   - 非 cursor 平台 + quota_key_id 为 nil → 通过（保持 NULL）
//   - 非 cursor 平台 + quota_key_id 非 nil → 报错（D-CURSOR-035，不得携带归属）
//   - cursor 平台 + quota_key_id 非 nil → 通过
func TestQuotaKeyBindingValidation(t *testing.T) {
	// cursor 平台缺归属激活码 → 必须报错
	err := validateAccountQuotaKeyBinding(domain.PlatformCursor, nil)
	require.Error(t, err, "cursor platform account without quota_key_id must be rejected")
	require.Equal(t, "ACCOUNT_QUOTA_KEY_REQUIRED", infraerrors.Reason(err))

	// 非 cursor 平台缺归属激活码 → 允许（NULL）
	err = validateAccountQuotaKeyBinding(PlatformOpenAI, nil)
	require.NoError(t, err, "non-cursor platform account may have NULL quota_key_id")

	// cursor 平台已归属激活码 → 通过
	qkID := int64(42)
	err = validateAccountQuotaKeyBinding(domain.PlatformCursor, &qkID)
	require.NoError(t, err, "cursor platform account bound to a quota key must pass")

	// 非 cursor 平台携带归属激活码 → 必须报错（D-CURSOR-035 / 终审 r5 F1）
	err = validateAccountQuotaKeyBinding(PlatformOpenAI, &qkID)
	require.Error(t, err, "non-cursor platform account must not carry a quota_key_id")
	require.Equal(t, http.StatusBadRequest, infraerrors.Code(err))
	require.Equal(t, "ACCOUNT_QUOTA_KEY_NOT_ALLOWED_FOR_PLATFORM", infraerrors.Reason(err))
}

// TestCursorAccountCreateRejectsMissingQuotaKey 端到端锁定 CreateAccount 入口已接入
// cursor 归属激活码校验（应用层前置，DB CHECK 兜底）：cursor 平台建号未传 quota_key_id
// 必须被拒绝，且错误信息指向 quota_key_id。
//
// 使用同包内复用的假 repo（upstreamBillingProbeAccountRepo）：本校验位于
// CreateAccount 链路最前段（rejectWebCredential 之后、凭证规范化之前），
// cursor + nil 会立即返回，不依赖任一 repo 方法。
func TestCursorAccountCreateRejectsMissingQuotaKey(t *testing.T) {
	repo := &upstreamBillingProbeAccountRepo{}

	_, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "cursor-no-quota-key",
		Platform:             domain.PlatformCursor,
		Type:                 AccountTypeAPIKey,
		Credentials:          map[string]any{"api_key": "sk-test"},
		SkipDefaultGroupBind: true,
	})
	require.Error(t, err, "cursor account creation without quota_key_id must be rejected")
	require.Contains(t, err.Error(), "quota_key_id",
		"error must indicate the missing quota_key_id binding")

	// 传入 quota_key_id 后应通过本层校验（后续由 DB 层 FK/CHECK 兜底）。
	qkID := int64(7)
	created, err := (&adminServiceImpl{accountRepo: repo}).CreateAccount(context.Background(), &CreateAccountInput{
		Name:                 "cursor-with-quota-key",
		Platform:             domain.PlatformCursor,
		Type:                 AccountTypeAPIKey,
		Credentials:          map[string]any{"api_key": "sk-test"},
		SkipDefaultGroupBind: true,
		QuotaKeyID:           &qkID,
	})
	require.NoError(t, err, "cursor account creation with quota_key_id must pass app-layer validation")
	require.NotNil(t, created)
}
