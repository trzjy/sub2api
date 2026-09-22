package service

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// seedSucceededChallenge 用 claim 模型建出一个 status=succeeded 的会话：
// BeginConsume 拿到 claim → SetConsumeResult 写入挑战值与 succeeded → FinishConsume(true)
// 释放 claimToken。返回的 id 已绑定于入参的 adminID/platform/phone。
func seedSucceededChallenge(t *testing.T, store *WebLoginChallengeSessionStore, in WebLoginChallengeCreateInput, challenge WebSMSChallenge) string {
	t.Helper()
	sess, err := store.Create(in)
	require.NoError(t, err)
	claim, err := store.BeginConsume(sess.ID, in.AdminID, in.Platform, in.Phone)
	require.NoError(t, err)
	require.NoError(t, store.SetConsumeResult(sess.ID, claim.Token, challenge))
	require.NoError(t, store.FinishConsume(sess.ID, claim.Token, true))
	return sess.ID
}

func TestWebLoginChallengeSessionStoreCreateBindsOpaqueSession(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	store := NewWebLoginChallengeSessionStore()
	store.now = func() time.Time { return now }
	accountID := int64(42)
	proxyID := int64(77)
	accountDraft := map[string]any{"name": "draft"}

	sess, err := store.Create(WebLoginChallengeCreateInput{
		AdminID:      7,
		Platform:     "KIMI",
		Phone:        " 13800138000 ",
		AccountID:    &accountID,
		ProxyID:      &proxyID,
		AccountDraft: accountDraft,
	})
	require.NoError(t, err)
	require.NotEmpty(t, sess.ID)
	require.Len(t, sess.ID, 43)
	require.Equal(t, PlatformKimi, sess.Platform)
	require.Equal(t, "13800138000", sess.Phone)
	require.Equal(t, int64(42), *sess.AccountID)
	require.Equal(t, int64(77), *sess.ProxyID)
	require.Equal(t, "pending", sess.Status)
	require.Equal(t, now.Add(WebLoginChallengeTTL), sess.ExpiresAt)
	// 公开 clone 不泄露内部挑战/claim 字段。
	require.Equal(t, WebSMSChallenge{}, sess.Challenge)
	// IDs are opaque and must not contain the bound phone or account metadata.
	require.NotContains(t, sess.ID, sess.Phone)
}

// BeginConsume 拿到 claim；完整序列后会话写入 Challenge + Status=succeeded + resultReady=true。
func TestWebLoginChallengeSessionStoreClaimConsumeLifecycle(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	store := NewWebLoginChallengeSessionStore()
	store.now = func() time.Time { return now }

	id := seedSucceededChallenge(t, store, WebLoginChallengeCreateInput{
		AdminID:  7,
		Platform: PlatformKimi,
		Phone:    "13800138000",
	}, WebSMSChallenge{KimiCaptchaValidate: "validate-1"})

	internal := store.sessions[id]
	require.Equal(t, "succeeded", internal.Status)
	require.True(t, internal.resultReady)
	require.Equal(t, WebSMSChallenge{KimiCaptchaValidate: "validate-1"}, internal.Challenge)
	// 公开读取（Status）仍不泄露挑战与 claim 字段。
	status, err := store.Status(id, 7)
	require.NoError(t, err)
	require.Equal(t, WebSMSChallenge{}, status.Challenge)
	require.Equal(t, "", status.claimStage)
	require.Equal(t, "", status.claimToken)
}

// 串行两次 BeginConsume：第一次成功，提交结果后第二次应返回 ErrWebLoginChallengeMismatch
// （resultReady 已置位，拒绝重放）。
func TestWebLoginChallengeSessionStoreSerialDoubleBeginConsumeMismatch(t *testing.T) {
	store := NewWebLoginChallengeSessionStore()
	sess, err := store.Create(WebLoginChallengeCreateInput{AdminID: 7, Platform: PlatformKimi, Phone: "13800138000"})
	require.NoError(t, err)

	// 第一次 BeginConsume 成功（会话尚未提交结果）。
	claim, err := store.BeginConsume(sess.ID, 7, PlatformKimi, "13800138000")
	require.NoError(t, err)
	require.NotEmpty(t, claim.Token)

	// 提交结果 → resultReady 置位。
	require.NoError(t, store.SetConsumeResult(sess.ID, claim.Token, WebSMSChallenge{KimiCaptchaValidate: "validate-1"}))
	require.NoError(t, store.FinishConsume(sess.ID, claim.Token, true))

	// 第二次 BeginConsume 应 Mismatch（resultReady 已置位）。
	_, err = store.BeginConsume(sess.ID, 7, PlatformKimi, "13800138000")
	require.ErrorIs(t, err, ErrWebLoginChallengeMismatch)
}

// 并发 N 次 BeginConsume 只有一次能拿到 claim（其余 ErrWebLoginChallengeInFlight）。
func TestWebLoginChallengeSessionStoreConcurrentBeginConsumeOnlyOne(t *testing.T) {
	store := NewWebLoginChallengeSessionStore()
	sess, err := store.Create(WebLoginChallengeCreateInput{AdminID: 7, Platform: PlatformKimi, Phone: "13800138000"})
	require.NoError(t, err)

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	var mu sync.Mutex
	success := 0
	conflict := 0
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, e := store.BeginConsume(sess.ID, 7, PlatformKimi, "13800138000")
			mu.Lock()
			defer mu.Unlock()
			if e == nil {
				success++
			} else if errors.Is(e, ErrWebLoginChallengeInFlight) {
				conflict++
			} else {
				t.Errorf("unexpected error: %v", e)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, success, "exactly one BeginConsume should win the claim")
	require.Equal(t, n-1, conflict, "remaining calls must see InFlight")
}

// BeginSendCode 要求 Status=succeeded 且 Stage=send_code 匹配；成功 FinishSendCode(true)
// 后 Stage 变 login；再 BeginSendCode 应 Mismatch。BeginLogin 要求 Stage=login，成功后
// Consumed=true；再 BeginLogin 应 ErrWebLoginChallengeConsumed。
func TestWebLoginChallengeSessionStoreSendCodeThenLoginStages(t *testing.T) {
	store := NewWebLoginChallengeSessionStore()
	id := seedSucceededChallenge(t, store, WebLoginChallengeCreateInput{
		AdminID:  7,
		Platform: PlatformKimi,
		Phone:    "13800138000",
		Stage:    "send_code",
	}, WebSMSChallenge{KimiCaptchaValidate: "validate-1"})

	// send_code 阶段匹配。
	claim, err := store.BeginSendCode(id, 7, PlatformKimi, "13800138000")
	require.NoError(t, err)
	require.NotEmpty(t, claim.Token)
	require.NoError(t, store.FinishSendCode(id, claim.Token, true))
	require.Equal(t, "login", store.sessions[id].Stage)

	// 已推进到 login，再 BeginSendCode 应 Mismatch（Stage 不符）。
	_, err = store.BeginSendCode(id, 7, PlatformKimi, "13800138000")
	require.ErrorIs(t, err, ErrWebLoginChallengeMismatch)

	// login 阶段匹配。
	login, err := store.BeginLogin(id, 7, PlatformKimi, "13800138000")
	require.NoError(t, err)
	require.NotEmpty(t, login.Token)
	require.NoError(t, store.FinishLogin(id, login.Token, true))
	require.True(t, store.sessions[id].Consumed)

	// 已消费，再 BeginLogin 应 Consumed。
	_, err = store.BeginLogin(id, 7, PlatformKimi, "13800138000")
	require.ErrorIs(t, err, ErrWebLoginChallengeConsumed)
}

// FinishConsume(false)/FinishSendCode(false)/FinishLogin(false) 释放 claimToken，允许安全重试。
func TestWebLoginChallengeSessionStoreFailReleasesClaimToken(t *testing.T) {
	store := NewWebLoginChallengeSessionStore()

	// result stage 失败释放（使用 pending 会话，BeginConsume 在 pending 下即允许）。
	sess, err := store.Create(WebLoginChallengeCreateInput{AdminID: 7, Platform: PlatformKimi, Phone: "13800138000"})
	require.NoError(t, err)
	c1, err := store.BeginConsume(sess.ID, 7, PlatformKimi, "13800138000")
	require.NoError(t, err)
	require.NoError(t, store.FinishConsume(sess.ID, c1.Token, false))
	require.Equal(t, "", store.sessions[sess.ID].claimToken)
	c2, err := store.BeginConsume(sess.ID, 7, PlatformKimi, "13800138000")
	require.NoError(t, err)
	require.NotEqual(t, c1.Token, c2.Token)
	require.NoError(t, store.FinishConsume(sess.ID, c2.Token, true))

	// send_code 阶段失败释放（需 status=succeeded 会话）。
	scID := seedSucceededChallenge(t, store, WebLoginChallengeCreateInput{
		AdminID: 7, Platform: PlatformKimi, Phone: "13800138000", Stage: "send_code",
	}, WebSMSChallenge{KimiCaptchaValidate: "validate-1"})
	sc, err := store.BeginSendCode(scID, 7, PlatformKimi, "13800138000")
	require.NoError(t, err)
	require.NoError(t, store.FinishSendCode(scID, sc.Token, false))
	require.Equal(t, "", store.sessions[scID].claimToken)
	sc2, err := store.BeginSendCode(scID, 7, PlatformKimi, "13800138000")
	require.NoError(t, err)
	require.NotEqual(t, sc.Token, sc2.Token)
	require.NoError(t, store.FinishSendCode(scID, sc2.Token, true))

	// login 阶段失败释放（需 status=succeeded 且 Stage=login 会话，未 Consumed）。
	lgID := seedSucceededChallenge(t, store, WebLoginChallengeCreateInput{
		AdminID: 7, Platform: PlatformKimi, Phone: "13800138000", Stage: "login",
	}, WebSMSChallenge{KimiCaptchaValidate: "validate-1"})
	lg, err := store.BeginLogin(lgID, 7, PlatformKimi, "13800138000")
	require.NoError(t, err)
	require.NoError(t, store.FinishLogin(lgID, lg.Token, false))
	require.Equal(t, "", store.sessions[lgID].claimToken)
	require.False(t, store.sessions[lgID].Consumed)
	lg2, err := store.BeginLogin(lgID, 7, PlatformKimi, "13800138000")
	require.NoError(t, err)
	require.NotEqual(t, lg.Token, lg2.Token)
	require.NoError(t, store.FinishLogin(lgID, lg2.Token, true))
}

// Status 的公开 clone 不泄露 Challenge、claimStage、claimToken。
func TestWebLoginChallengeSessionStoreStatusCloneDoesNotLeak(t *testing.T) {
	store := NewWebLoginChallengeSessionStore()
	id := seedSucceededChallenge(t, store, WebLoginChallengeCreateInput{
		AdminID:  7,
		Platform: PlatformKimi,
		Phone:    "13800138000",
	}, WebSMSChallenge{KimiCaptchaValidate: "validate-1"})

	status, err := store.Status(id, 7)
	require.NoError(t, err)
	require.Equal(t, "succeeded", status.Status)
	require.Equal(t, WebSMSChallenge{}, status.Challenge)
	require.Equal(t, "", status.claimStage)
	require.Equal(t, "", status.claimToken)
}

// 绑定不匹配（adminID/platform/phone 任一不符）返回 ErrWebLoginChallengeMismatch；
// 过期返回 ErrWebLoginChallengeExpired。
func TestWebLoginChallengeSessionStoreBindingMismatchAndExpiry(t *testing.T) {
	store := NewWebLoginChallengeSessionStore()
	sess, err := store.Create(WebLoginChallengeCreateInput{AdminID: 7, Platform: PlatformKimi, Phone: "13800138000"})
	require.NoError(t, err)

	// adminID 不符。
	_, err = store.BeginConsume(sess.ID, 8, PlatformKimi, "13800138000")
	require.ErrorIs(t, err, ErrWebLoginChallengeMismatch)
	_, err = store.Status(sess.ID, 8)
	require.ErrorIs(t, err, ErrWebLoginChallengeMismatch)

	// platform 不符。
	_, err = store.BeginConsume(sess.ID, 7, PlatformZhipu, "13800138000")
	require.ErrorIs(t, err, ErrWebLoginChallengeMismatch)

	// phone 不符。
	_, err = store.BeginConsume(sess.ID, 7, PlatformKimi, "13800138001")
	require.ErrorIs(t, err, ErrWebLoginChallengeMismatch)

	// wrong claimToken 拒绝 SetConsumeResult。
	claim, err := store.BeginConsume(sess.ID, 7, PlatformKimi, "13800138000")
	require.NoError(t, err)
	require.ErrorIs(t, store.SetConsumeResult(sess.ID, "wrong-token", WebSMSChallenge{KimiCaptchaValidate: "v"}), ErrWebLoginChallengeMismatch)
	require.NoError(t, store.FinishConsume(sess.ID, claim.Token, false))

	// 过期返回 ErrWebLoginChallengeExpired。
	expired, err := store.Create(WebLoginChallengeCreateInput{AdminID: 7, Platform: PlatformZhipu, Phone: "13800138000"})
	require.NoError(t, err)
	store.now = func() time.Time { return expired.ExpiresAt.Add(time.Second) }
	_, err = store.BeginConsume(expired.ID, 7, PlatformZhipu, "13800138000")
	require.ErrorIs(t, err, ErrWebLoginChallengeExpired)
}

// 惰性清理：命中过期的 session 被 delete，Create 时顺带清扫全部过期 session，
// 防止长期运行内存无限积累（无后台定时器）。
func TestWebLoginChallengeSessionStoreLazyEvictionOfExpired(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	store := NewWebLoginChallengeSessionStore()
	store.now = func() time.Time { return now }

	old1, err := store.Create(WebLoginChallengeCreateInput{AdminID: 7, Platform: PlatformKimi, Phone: "13800138000"})
	require.NoError(t, err)
	_, err = store.Create(WebLoginChallengeCreateInput{AdminID: 7, Platform: PlatformZhipu, Phone: "13800138001"})
	require.NoError(t, err)

	// 时间推进使两个 session 全部过期，再创建一个新 session。
	now = old1.ExpiresAt.Add(time.Second)
	fresh, err := store.Create(WebLoginChallengeCreateInput{AdminID: 7, Platform: PlatformKimi, Phone: "13800138002"})
	require.NoError(t, err)
	require.Len(t, store.sessions, 1, "expired sessions must be swept on Create")

	// Get 命中过期 session 时同样删除并返回未找到/过期错误。
	store.now = func() time.Time { return fresh.ExpiresAt.Add(time.Second) }
	_, err = store.Status(fresh.ID, 7)
	require.ErrorIs(t, err, ErrWebLoginChallengeExpired)
	require.Empty(t, store.sessions, "expired session must be deleted on lookup")
}

// 2026-09-22 chatglm.cn 线上取证：官方滑块 onSuccess 仅回调 {rid, pass}，md5 可选。
// zhipu 挑战结果 rid+phone_code（无 md5）须能通过 SetConsumeResult 完整 consume 链路；
// 缺 rid 仍拒绝。
func TestWebLoginChallengeSessionStoreSetConsumeResultZhipuRidOnly(t *testing.T) {
	store := NewWebLoginChallengeSessionStore()

	sess, err := store.Create(WebLoginChallengeCreateInput{AdminID: 7, Platform: PlatformZhipu, Phone: "13800138000"})
	require.NoError(t, err)
	claim, err := store.BeginConsume(sess.ID, 7, PlatformZhipu, "13800138000")
	require.NoError(t, err)
	require.NoError(t, store.SetConsumeResult(sess.ID, claim.Token, WebSMSChallenge{ZhipuCaptchaRid: "rid-1", ZhipuPhoneCode: "86"}))
	require.NoError(t, store.FinishConsume(sess.ID, claim.Token, true))

	// 缺 rid（仅 md5+phone_code）仍失败关闭。
	sess2, err := store.Create(WebLoginChallengeCreateInput{AdminID: 7, Platform: PlatformZhipu, Phone: "13800138001"})
	require.NoError(t, err)
	claim2, err := store.BeginConsume(sess2.ID, 7, PlatformZhipu, "13800138001")
	require.NoError(t, err)
	require.Error(t, store.SetConsumeResult(sess2.ID, claim2.Token, WebSMSChallenge{ZhipuCaptchaMD5: "md5-1", ZhipuPhoneCode: "86"}))
}
