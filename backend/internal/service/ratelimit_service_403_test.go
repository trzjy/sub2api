//go:build unit

package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type runtimeBlockRecorder struct {
	accounts   []*Account
	until      []time.Time
	reasons    []string
	clearedIDs []int64
}

func (r *runtimeBlockRecorder) BlockAccountScheduling(account *Account, until time.Time, reason string) {
	r.accounts = append(r.accounts, account)
	r.until = append(r.until, until)
	r.reasons = append(r.reasons, reason)
}

func (r *runtimeBlockRecorder) ClearAccountSchedulingBlock(accountID int64) {
	r.clearedIDs = append(r.clearedIDs, accountID)
}

func TestRateLimitService_HandleUpstreamError_OpenAI403FirstHitTempUnschedulable(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	counter := &openAI403CounterCacheStub{counts: []int64{1}}
	blocker := &runtimeBlockRecorder{}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	service.SetOpenAI403CounterCache(counter)
	service.SetAccountRuntimeBlocker(blocker)
	account := &Account{
		ID:       301,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
	}

	shouldDisable := service.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusForbidden,
		http.Header{},
		[]byte(`{"error":{"message":"temporary edge rejection"}}`),
	)

	require.True(t, shouldDisable)
	require.Equal(t, 0, repo.setErrorCalls)
	require.Equal(t, 1, repo.tempCalls)
	require.Contains(t, repo.lastTempReason, "temporary edge rejection")
	require.Contains(t, repo.lastTempReason, "(1/3)")
	require.Len(t, blocker.accounts, 1)
	require.Equal(t, account.ID, blocker.accounts[0].ID)
	require.Equal(t, "openai_403_temp", blocker.reasons[0])
	require.True(t, blocker.until[0].After(time.Now()))
}

func TestRateLimitService_HandleUpstreamError_OpenAI403ThresholdDisables(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	counter := &openAI403CounterCacheStub{counts: []int64{3}}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	service.SetOpenAI403CounterCache(counter)
	account := &Account{
		ID:       302,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
	}

	shouldDisable := service.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusForbidden,
		http.Header{},
		[]byte(`{"error":{"message":"workspace forbidden by policy"}}`),
	)

	require.True(t, shouldDisable)
	require.Equal(t, 1, repo.setErrorCalls)
	require.Equal(t, 0, repo.tempCalls)
	require.Contains(t, repo.lastErrorMsg, "workspace forbidden by policy")
	require.Contains(t, repo.lastErrorMsg, "consecutive_403=3/3")
}

const codeBuddyContentSafety403Body = `{"code":11140,"msg":"request illegal","requestId":"req-123","displayMsg":{"en":"The content did not pass the safety review, please adjust and try again.","zh":"内容未通过安全审核，请调整后重试。"}}`

// 内容审核 403 三连（镜像上方 consecutive_403 用例风格）：请求级错误不得计数、
// 不得 temp-unschedulable、不得 SetError 永久禁用——即使计数器阈值次触达也如此
// （检测在计数之前短路）。
func TestRateLimitService_HandleUpstreamError_OpenAI403ContentSafetySkipsAccountPenalty(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	counter := &openAI403CounterCacheStub{counts: []int64{1, 2, 3}}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	service.SetOpenAI403CounterCache(counter)
	account := &Account{
		ID:       303,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
	}

	for i := 0; i < 3; i++ {
		shouldDisable := service.HandleUpstreamError(
			context.Background(),
			account,
			http.StatusForbidden,
			http.Header{},
			[]byte(codeBuddyContentSafety403Body),
		)
		require.False(t, shouldDisable, "hit %d: content-safety 403 must not disable", i+1)
	}
	require.Equal(t, 0, counter.incrementCalls, "counter must not be incremented")
	require.Equal(t, 0, repo.setErrorCalls)
	require.Equal(t, 0, repo.tempCalls)
}

// HTML 403 与 11140 内容审核同时命中时：两个豁免分支都是跳过账号处罚（与 #5334
// HTML 先例同构），断言可观测副作用为零——不计数、不冷却、不禁用、应 failover。
func TestRateLimitService_HandleUpstreamError_OpenAI403HTMLAndContentSafetyBothSkip(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	counter := &openAI403CounterCacheStub{counts: []int64{1}}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	service.SetOpenAI403CounterCache(counter)
	account := &Account{
		ID:       304,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
	}

	htmlWithSafetyText := []byte(`<!doctype html><html><body>error 11140 the content did not pass the safety review</body></html>`)
	shouldDisable := service.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusForbidden,
		http.Header{},
		htmlWithSafetyText,
	)

	require.False(t, shouldDisable)
	require.Equal(t, 0, counter.incrementCalls)
	require.Equal(t, 0, repo.setErrorCalls)
	require.Equal(t, 0, repo.tempCalls)
}

// CodeBuddy 原生路径（platform=codebuddy 直连账号）：403 + 11140 走 handle403 通用
// 分支前必须被内容审核检测短路——原行为是首次 403 即 handleAuthError → SetError
// 永久禁用（生产事故根因）。
func TestRateLimitService_HandleUpstreamError_CodeBuddyNative403ContentSafetySkipsAccountPenalty(t *testing.T) {
	repo := &rateLimitAccountRepoStub{}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	account := &Account{
		ID:       305,
		Platform: PlatformCodeBuddy,
		Type:     AccountTypeOAuth,
	}

	shouldDisable := service.HandleUpstreamError(
		context.Background(),
		account,
		http.StatusForbidden,
		http.Header{},
		[]byte(codeBuddyContentSafety403Body),
	)

	require.False(t, shouldDisable)
	require.Equal(t, 0, repo.setErrorCalls)
	require.Equal(t, 0, repo.tempCalls)
}

// TestRateLimitService_HandleUpstreamError_CodeBuddy403_11140Semantics 是 2026-10-18
// 生产事故回归：403 + 顶层 code=11140 语义已分化，两条入站路径必须得到完全相同的处置。
//   - native：platform=codebuddy 直连账号 → handle403 的 CodeBuddy 分支；
//   - openai_shadow：platform=openai 的 codebuddy 影子（isCodeBuddyShadowAccount，
//     codebuddy_gateway_forward.go:38）经 OpenAI 兼容路径 → handleOpenAI403 分支。
// 期望：账号限制文案 → 判死（SetError）+ 当前请求继续 failover 换号；
//       含 safety review → 请求级内容拒绝，不计数、不冷却、不禁用。
func TestRateLimitService_HandleUpstreamError_CodeBuddy403_11140Semantics(t *testing.T) {
	const testModel = "space-bunny"

	newAccount := func(path string, id int64) *Account {
		if path == "native" {
			return &Account{ID: id, Platform: PlatformCodeBuddy, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
		}
		parentID := int64(900)
		return &Account{
			ID:              id,
			Platform:        PlatformOpenAI,
			Type:            AccountTypeOAuth,
			Status:          StatusActive,
			Schedulable:     true,
			ParentAccountID: &parentID,
			QuotaDimension:  QuotaDimensionCodeBuddy,
		}
	}

	cases := []struct {
		name         string
		body         string
		wantDisable  bool
		wantSetError int
	}{
		{
			// 生产原文：顶层 11140 + displayMsg「账号存在异常行为，已被限制使用」。
			name:         "账号限制_11140_限制文案",
			body:         codeBuddyAccountRestricted403Body,
			wantDisable:  true,
			wantSetError: 1,
		},
		{
			name:         "内容审核_11140_safety_review",
			body:         codeBuddyContentSafety403Body,
			wantDisable:  false,
			wantSetError: 0,
		},
	}

	for _, path := range []string{"native", "openai_shadow"} {
		for _, tc := range cases {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				repo := &rateLimitAccountRepoStub{}
				counter := &openAI403CounterCacheStub{counts: []int64{1, 2, 3}}
				service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
				service.SetOpenAI403CounterCache(counter)
				account := newAccount(path, 401)
				if path == "openai_shadow" {
					require.True(t, isCodeBuddyShadowAccount(account), "must exercise the codebuddy shadow inbound path")
				}

				body := []byte(tc.body)
				shouldDisable := service.HandleUpstreamError(
					context.Background(),
					account,
					http.StatusForbidden,
					http.Header{},
					body,
				)

				require.Equal(t, tc.wantDisable, shouldDisable, "两条路径的 failover 信号必须一致")
				require.Equal(t, tc.wantSetError, repo.setErrorCalls)
				require.Equal(t, 0, repo.tempCalls)
				require.Equal(t, 0, counter.incrementCalls)

				if tc.wantSetError == 0 {
					return
				}

				// 判死只作用于被限制的账号。
				require.Equal(t, account.ID, repo.lastErrorID)
				require.Contains(t, repo.lastErrorMsg, "异常行为")

				// 复现生产 SetError 的持久化效果（repository/account_repo.go:1698：
				// UPDATE accounts SET status=error, schedulable=FALSE），再用现成调度判定入口
				// 断言该账号不再可被选中。
				account.Status = StatusError
				account.Schedulable = false
				require.False(t, account.IsSchedulable(), "判死账号必须不可调度")
				require.False(t, account.IsSchedulableForModel(testModel))

				// 健康账号不受影响，failover 仍可换到它。
				healthy := newAccount(path, 402)
				require.True(t, healthy.IsSchedulable())
				require.True(t, healthy.IsSchedulableForModel(testModel))
				require.Equal(t, 1, repo.setErrorCalls)

				// failover 信号：本层返回 true 后由调用方构造 UpstreamFailoverError 换号。
				// OpenAI 兼容路径用现成构造器坐实（openai_gateway_upstream_errors.go:326）；
				// 原生路径由 GatewayService 在该分支直接构造
				// （gateway_upstream_response.go:429-431：if shouldDisable → UpstreamFailoverError）。
				if path == "openai_shadow" {
					failoverErr := newOpenAIUpstreamFailoverError(http.StatusForbidden, http.Header{}, body, "", false)
					require.NotNil(t, failoverErr)
					require.Equal(t, http.StatusForbidden, failoverErr.StatusCode)
				}
			})
		}
	}
}

// ---------------------------------------------------------------------------
// 以下为独立验收代理自写用例（2026-10-18），不复用实现者的断言，用于交叉验证
// 403 + code=11140 的两条入站路径口径一致 + 判死后不再可调度 + failover 仍可换号。
// ---------------------------------------------------------------------------

// acceptCodeBuddyRestricted403Body：生产原文（账号级限制，无任何内容审核语义）。
const acceptCodeBuddyRestricted403Body = `{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been restricted due to unusual activity","zh":"账号存在异常行为，已被限制使用"},"displayTips":{"zh":"系统检测到该账号存在异常使用行为，如需申诉请提交反馈"}}`

// acceptCodeBuddySafety403Body：同一顶层码，但含「安全审核 / safety review」语义。
const acceptCodeBuddySafety403Body = `{"code":11140,"msg":"request illegal","requestId":"req-abc","displayMsg":{"en":"The content did not pass the safety review, please adjust and try again.","zh":"内容未通过安全审核，请调整后重试。"}}`

// acceptNewAccount 构造两条入站路径的账号：
//   - native：platform=codebuddy 直连账号 → handle403 的 CodeBuddy 分支；
//   - shadow：platform=openai + ParentAccountID + quota_dimension=codebuddy →
//     isCodeBuddyShadowAccount（codebuddy_gateway_forward.go:38）→ handleOpenAI403。
func acceptNewAccount(path string, id int64) *Account {
	if path == "native" {
		return &Account{ID: id, Platform: PlatformCodeBuddy, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	}
	parentID := int64(800)
	return &Account{
		ID:              id,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		Status:          StatusActive,
		Schedulable:     true,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionCodeBuddy,
	}
}

// acceptApplySetError 复刻生产 SetError 的持久化字段效果
// （repository/account_repo.go:1703：status=error + schedulable=FALSE）。
func acceptApplySetError(account *Account) {
	account.Status = StatusError
	account.Schedulable = false
}

// TestAcceptance_CodeBuddy403_11140_RestrictedKillsAccountOnBothPaths
// 需求 1/4/5：403 + 11140 + 账号限制文案 → 判死（SetError）+ 当前请求继续 failover，
// 且两条入站路径口径一致（不经过三振计数、不临时冷却）。
func TestAcceptance_CodeBuddy403_11140_RestrictedKillsAccountOnBothPaths(t *testing.T) {
	const testModel = "space-bunny"
	for _, path := range []string{"native", "shadow"} {
		t.Run(path, func(t *testing.T) {
			repo := &rateLimitAccountRepoStub{}
			counter := &openAI403CounterCacheStub{counts: []int64{1, 2, 3}}
			service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			service.SetOpenAI403CounterCache(counter)
			account := acceptNewAccount(path, 601)
			if path == "shadow" {
				require.True(t, isCodeBuddyShadowAccount(account), "必须走 codebuddy 影子入站路径")
			}

			shouldDisable := service.HandleUpstreamError(
				context.Background(), account, http.StatusForbidden, http.Header{}, []byte(acceptCodeBuddyRestricted403Body),
			)

			require.True(t, shouldDisable, "账号限制必须判死并返回 failover 信号")
			require.Equal(t, 1, repo.setErrorCalls, "必须走既有判死路径 handleAuthError → SetError")
			require.Equal(t, account.ID, repo.lastErrorID)
			require.Contains(t, repo.lastErrorMsg, "异常行为")
			require.Equal(t, 0, repo.tempCalls, "不得退化为临时不可调度")
			require.Equal(t, 0, counter.incrementCalls, "不得落入三振计数链")

			acceptApplySetError(account)
			require.False(t, account.IsSchedulable(), "判死账号必须不可调度（后续请求不得再选中）")
			require.False(t, (&GatewayService{}).isAccountSchedulableForSelection(account))
			require.True(t, shouldClearStickySession(account, testModel), "判死账号的粘性会话必须被清除")
		})
	}
}

// TestAcceptance_CodeBuddy403_11140_SafetyReviewSkipsPenaltyOnBothPaths
// 需求 2：只有明确含内容审核语义才降级为请求级拒绝，账号不受任何处罚，
// 两条入站路径口径一致。
func TestAcceptance_CodeBuddy403_11140_SafetyReviewSkipsPenaltyOnBothPaths(t *testing.T) {
	const testModel = "space-bunny"
	for _, path := range []string{"native", "shadow"} {
		t.Run(path, func(t *testing.T) {
			repo := &rateLimitAccountRepoStub{}
			counter := &openAI403CounterCacheStub{counts: []int64{1, 2, 3}}
			service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
			service.SetOpenAI403CounterCache(counter)
			account := acceptNewAccount(path, 602)
			if path == "shadow" {
				require.True(t, isCodeBuddyShadowAccount(account), "必须走 codebuddy 影子入站路径")
			}

			shouldDisable := service.HandleUpstreamError(
				context.Background(), account, http.StatusForbidden, http.Header{}, []byte(acceptCodeBuddySafety403Body),
			)

			require.False(t, shouldDisable, "内容审核不得判死账号")
			require.Equal(t, 0, repo.setErrorCalls, "内容审核不得调用 SetError")
			require.Equal(t, 0, repo.tempCalls, "内容审核不得调用 SetTempUnschedulable")
			require.Equal(t, 0, counter.incrementCalls, "内容审核不得计入三振")

			require.True(t, account.IsSchedulable())
			require.True(t, (&GatewayService{}).isAccountSchedulableForSelection(account))
			require.False(t, shouldClearStickySession(account, testModel))
		})
	}
}

// TestAcceptance_CodeBuddy403_11140_FailoverStillSelectsHealthyAccount
// 需求 1：坏账号被排除后，同一请求仍能沿既有调度入口选到健康账号（不是自造判定）。
func TestAcceptance_CodeBuddy403_11140_FailoverStillSelectsHealthyAccount(t *testing.T) {
	for _, path := range []string{"native", "shadow"} {
		t.Run(path, func(t *testing.T) {
			repo := &rateLimitAccountRepoStub{}
			service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)

			bad := acceptNewAccount(path, 701)
			healthyA := acceptNewAccount(path, 702)
			healthyB := acceptNewAccount(path, 703)
			pool := []*Account{bad, healthyA, healthyB}

			// 判死前：三个账号都可选。
			for _, acc := range pool {
				require.True(t, (&GatewayService{}).isAccountSchedulableForSelection(acc))
			}

			shouldDisable := service.HandleUpstreamError(
				context.Background(), bad, http.StatusForbidden, http.Header{}, []byte(acceptCodeBuddyRestricted403Body),
			)
			require.True(t, shouldDisable, "返回 true 交由上层换号")
			require.Equal(t, 1, repo.setErrorCalls)
			acceptApplySetError(bad)

			// 判死后：坏账号被排除，首个健康账号被选中。
			var selected *Account
			for _, acc := range pool {
				if (&GatewayService{}).isAccountSchedulableForSelection(acc) {
					selected = acc
					break
				}
			}
			require.NotNil(t, selected, "坏账号被排除后仍必须能选到账号")
			require.Equal(t, healthyA.ID, selected.ID)
			require.NotEqual(t, bad.ID, selected.ID, "已判死账号不得再被选中")
			require.True(t, healthyB.IsSchedulable(), "其余健康账号不受牵连")
		})
	}
}

// ---------------------------------------------------------------------------
// 独立验收代理自写用例（verifier round 2026-10-18）
// 不复用实现者的用例：自建 verifierRepoStub 复刻生产 SetError 的入库效果
// （status=error + schedulable=FALSE，见 repository/account_repo.go:1705-1713），
// 因此「判死后不可调度」是被测对象自己的状态变化驱动，而非测试手工赋值。
// ---------------------------------------------------------------------------

const (
	verifierRestricted403Body = `{"code":11140,"msg":"request illegal","displayMsg":{"en":"Your account has been restricted due to unusual activity","zh":"账号存在异常行为，已被限制使用"},"displayTips":{"zh":"系统检测到该账号存在异常使用行为，如需申诉请提交反馈"}}`
	verifierSafety403Body     = `{"code":11140,"msg":"request illegal","requestId":"req-v-1","displayMsg":{"en":"The content did not pass the safety review, please adjust and try again.","zh":"内容未通过安全审核，请调整后重试。"}}`
	// 普通 OpenAI 形态 403：既不含 11140 也不含审核语义，用于区分两条入站入口。
	verifierGeneric403Body = `{"error":{"message":"permission denied by policy","type":"invalid_request_error"}}`
)

// verifierRepoStub 在父 stub 记录调用的基础上，额外把 SetError 的持久化字段效果
// 写回被测账号对象（与生产 UPDATE 语句一致），供后续可调度判定直接观察。
type verifierRepoStub struct {
	*rateLimitAccountRepoStub
	tracked map[int64]*Account
}

func (r *verifierRepoStub) SetError(ctx context.Context, id int64, errorMsg string) error {
	if acc, ok := r.tracked[id]; ok && acc != nil {
		acc.Status = StatusError
		acc.Schedulable = false
		acc.ErrorMessage = errorMsg
	}
	return r.rateLimitAccountRepoStub.SetError(ctx, id, errorMsg)
}

func verifierNewService(accounts ...*Account) (*RateLimitService, *verifierRepoStub, *openAI403CounterCacheStub) {
	tracked := map[int64]*Account{}
	for _, acc := range accounts {
		tracked[acc.ID] = acc
	}
	repo := &verifierRepoStub{rateLimitAccountRepoStub: &rateLimitAccountRepoStub{}, tracked: tracked}
	counter := &openAI403CounterCacheStub{counts: []int64{1, 2, 3}}
	service := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)
	service.SetOpenAI403CounterCache(counter)
	return service, repo, counter
}

// verifierAccount 构造两条入站路径的账号：
//   - native：platform=codebuddy 直连 → handle403 的 CodeBuddy 分支；
//   - shadow：platform=openai + IsShadow() + quota_dimension=codebuddy
//     （codebuddy_gateway_forward.go:38）→ handleOpenAI403。
func verifierAccount(route string, id int64) *Account {
	if route == "native" {
		return &Account{ID: id, Platform: PlatformCodeBuddy, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true}
	}
	parentID := int64(5000 + id)
	return &Account{
		ID:              id,
		Platform:        PlatformOpenAI,
		Type:            AccountTypeOAuth,
		Status:          StatusActive,
		Schedulable:     true,
		ParentAccountID: &parentID,
		QuotaDimension:  QuotaDimensionCodeBuddy,
	}
}

// TestVerifier_CodeBuddy403Restricted_KillsAccountOnBothRoutes 需求 1/3/4：
// 403 + code=11140 + 「账号存在异常行为」→ 判死（SetError）且当前请求返回 failover 信号，
// 两条入站入口结果完全一致（不得绕过三振计数以外的任何环节，也不得临时冷却）。
func TestVerifier_CodeBuddy403Restricted_KillsAccountOnBothRoutes(t *testing.T) {
	for _, route := range []string{"native", "shadow"} {
		t.Run(route, func(t *testing.T) {
			account := verifierAccount(route, 1101)
			require.Equal(t, route == "shadow", isCodeBuddyShadowAccount(account), "账号形态与所选入站路径必须一致")

			service, repo, counter := verifierNewService(account)

			shouldDisable := service.HandleUpstreamError(
				context.Background(), account, http.StatusForbidden, http.Header{}, []byte(verifierRestricted403Body),
			)

			require.True(t, shouldDisable, "账号限制必须判死并返回 failover 信号")
			require.Equal(t, 1, repo.setErrorCalls, "必须走既有判死路径 handleAuthError → SetError")
			require.Equal(t, account.ID, repo.lastErrorID)
			require.Contains(t, repo.lastErrorMsg, "异常行为")
			require.Equal(t, 0, repo.tempCalls, "不得退化为临时不可调度")
			require.Equal(t, 0, counter.incrementCalls, "不得落入三振计数链")

			// SetError 的持久化效果（status=error + schedulable=FALSE）已在 stub 中落账。
			require.Equal(t, StatusError, account.Status)
			require.False(t, account.Schedulable)
		})
	}
}

// TestVerifier_CodeBuddy403SafetyReview_NoPenaltyOnBothRoutes 需求 2/3/4：
// 只有明确含「安全审核 / safety review」才降级为请求级拒绝，账号不受任何处罚，
// 两条入站入口结果一致（不判死、不临时冷却、403 计数器不递增）。
func TestVerifier_CodeBuddy403SafetyReview_NoPenaltyOnBothRoutes(t *testing.T) {
	for _, route := range []string{"native", "shadow"} {
		t.Run(route, func(t *testing.T) {
			account := verifierAccount(route, 1201)
			require.Equal(t, route == "shadow", isCodeBuddyShadowAccount(account))

			service, repo, counter := verifierNewService(account)

			shouldDisable := service.HandleUpstreamError(
				context.Background(), account, http.StatusForbidden, http.Header{}, []byte(verifierSafety403Body),
			)

			require.False(t, shouldDisable, "内容审核不得判死账号")
			require.Equal(t, 0, repo.setErrorCalls, "内容审核不得调用 SetError")
			require.Equal(t, 0, repo.tempCalls, "内容审核不得调用 SetTempUnschedulable")
			require.Equal(t, 0, counter.incrementCalls, "内容审核不得计入三振")
			require.Equal(t, StatusActive, account.Status)
			require.True(t, account.Schedulable)
		})
	}
}

// TestVerifier_CodeBuddy403Restricted_AccountNoLongerSchedulable 需求 1：
// 判死账号在同一请求与后续请求中都不得再被选中——用仓库现成的三个可调度判定入口断言。
func TestVerifier_CodeBuddy403Restricted_AccountNoLongerSchedulable(t *testing.T) {
	const testModel = "space-bunny"
	for _, route := range []string{"native", "shadow"} {
		t.Run(route, func(t *testing.T) {
			account := verifierAccount(route, 1301)
			service, repo, _ := verifierNewService(account)

			require.True(t, account.IsSchedulable(), "判死前必须可调度")
			require.True(t, account.IsSchedulableForModel(testModel))
			require.True(t, (&GatewayService{}).isAccountSchedulableForSelection(account))

			shouldDisable := service.HandleUpstreamError(
				context.Background(), account, http.StatusForbidden, http.Header{}, []byte(verifierRestricted403Body),
			)
			require.True(t, shouldDisable)
			require.Equal(t, 1, repo.setErrorCalls)

			// SetError 已由 verifierRepoStub 按生产语义落账。
			require.False(t, account.IsSchedulable(), "判死账号必须不可调度（account.go:189）")
			require.False(t, account.IsSchedulableForModel(testModel), "判死账号对所有模型均不可调度（antigravity_quota_scope.go:33）")
			require.False(t, (&GatewayService{}).isAccountSchedulableForSelection(account), "调度选择入口必须排除该账号（gateway_scheduling.go:1168）")

			// 幂等：同一请求后续重复送达（failover 重放）不得出现第二次处罚之外的状态漂移。
			shouldDisableAgain := service.HandleUpstreamError(
				context.Background(), account, http.StatusForbidden, http.Header{}, []byte(verifierRestricted403Body),
			)
			require.True(t, shouldDisableAgain)
			require.False(t, account.IsSchedulable())
		})
	}
}

// TestVerifier_CodeBuddy403Restricted_FailoverSelectsHealthyAccount 需求 1：
// 坏账号被排除后，同一池内的健康账号仍能被既有可调度入口选中，且健康账号未被误罚。
func TestVerifier_CodeBuddy403Restricted_FailoverSelectsHealthyAccount(t *testing.T) {
	const testModel = "space-bunny"
	for _, route := range []string{"native", "shadow"} {
		t.Run(route, func(t *testing.T) {
			bad := verifierAccount(route, 1401)
			healthyA := verifierAccount(route, 1402)
			healthyB := verifierAccount(route, 1403)
			pool := []*Account{bad, healthyA, healthyB}
			service, repo, counter := verifierNewService(pool...)

			shouldDisable := service.HandleUpstreamError(
				context.Background(), bad, http.StatusForbidden, http.Header{}, []byte(verifierRestricted403Body),
			)
			require.True(t, shouldDisable, "返回 true 交由上层 failover 换号")
			require.Equal(t, 1, repo.setErrorCalls)
			require.Equal(t, bad.ID, repo.lastErrorID)

			// failover 后按既有选择入口重选：坏账号出局，命中首个健康账号。
			remaining := make([]*Account, 0, len(pool))
			for _, acc := range pool {
				if (&GatewayService{}).isAccountSchedulableForSelection(acc) {
					remaining = append(remaining, acc)
				}
			}
			require.Len(t, remaining, 2, "只有被判死的账号出局")
			require.Equal(t, healthyA.ID, remaining[0].ID)
			require.False(t, (&GatewayService{}).isAccountSchedulableForSelection(bad))
			require.True(t, healthyB.IsSchedulableForModel(testModel))
			require.Equal(t, StatusActive, healthyA.Status)
			require.Equal(t, StatusActive, healthyB.Status)
			require.Equal(t, 0, counter.incrementCalls)
			require.Equal(t, 0, repo.tempCalls, "健康账号不得被冷却")
		})
	}
}

// TestVerifier_CodeBuddy403_RouteProbe_NativeVsShadowEntries 需求 4 的前置坐实：
// 用与 11140 无关的普通 403 报文探针确认两条路径确实分别进入各自的处理函数
// （native → handle403 通用分支；shadow → handleOpenAI403 三振分支），
// 否则「两条路径口径一致」的结论会因为根本没走到第二个入口而失去意义。
func TestVerifier_CodeBuddy403_RouteProbe_NativeVsShadowEntries(t *testing.T) {
	t.Run("native", func(t *testing.T) {
		account := verifierAccount("native", 1501)
		service, repo, counter := verifierNewService(account)

		shouldDisable := service.HandleUpstreamError(
			context.Background(), account, http.StatusForbidden, http.Header{}, []byte(verifierGeneric403Body),
		)

		require.True(t, shouldDisable)
		require.Equal(t, 1, repo.setErrorCalls, "platform=codebuddy 的通用 403 走 handle403 通用判死分支")
		require.Equal(t, 0, repo.tempCalls)
		require.Equal(t, 0, counter.incrementCalls)
	})

	t.Run("shadow", func(t *testing.T) {
		account := verifierAccount("shadow", 1502)
		require.True(t, isCodeBuddyShadowAccount(account))
		service, repo, counter := verifierNewService(account)

		shouldDisable := service.HandleUpstreamError(
			context.Background(), account, http.StatusForbidden, http.Header{}, []byte(verifierGeneric403Body),
		)

		require.True(t, shouldDisable, "三振未达阈值时返回 failover 信号但不判死")
		require.Equal(t, 1, repo.tempCalls, "影子账号走 handleOpenAI403 三振链")
		require.Equal(t, 1, counter.incrementCalls)
		require.Equal(t, 0, repo.setErrorCalls)
	})
}
