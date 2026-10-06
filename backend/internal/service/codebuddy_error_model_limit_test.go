package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// codeBuddyModelLimitRepoStub 记录 SetModelRateLimit / SetTempUnschedulable / UpdateExtra，
// 并把模型级限流镜像到内存 account（与真实 repo 行为一致），便于断言 IsSchedulableForModel。
type codeBuddyModelLimitRepoStub struct {
	stubOpenAIAccountRepo
	account        *Account
	modelCalls     int
	lastModelScope string
	lastModelReset time.Time
	tempCalls      int
	updateExtra    int
	lastExtra      map[string]any
}

func (r *codeBuddyModelLimitRepoStub) SetModelRateLimit(_ context.Context, _ int64, scope string, resetAt time.Time, reason ...string) error {
	r.modelCalls++
	r.lastModelScope = scope
	r.lastModelReset = resetAt
	if r.account != nil {
		// 模拟真实 repo 把模型级限流落进 account.Extra["model_rate_limits"][model]。
		setAccountModelRateLimitSnapshot(r.account, scope, resetAt, strings.Join(reason, " "), time.Now())
	}
	return nil
}

func (r *codeBuddyModelLimitRepoStub) SetTempUnschedulable(_ context.Context, _ int64, _ time.Time, _ string) error {
	r.tempCalls++
	return nil
}

func (r *codeBuddyModelLimitRepoStub) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.updateExtra++
	r.lastExtra = updates
	return nil
}

// TestHandleCodeBuddyModelLimit_ExcludesOnlyTargetModel 验证 PR3 task 1.5 裁定：
// 429+6004 模型级限流只冷却该模型（写入 model_rate_limits[model]），不做账号级 SetTempUnschedulable，
// 其它模型仍可调度；同时保留可读的 codebuddy_model_limit 标记做可观测。
func TestHandleCodeBuddyModelLimit_ExcludesOnlyTargetModel(t *testing.T) {
	account := &Account{ID: 9001, Platform: PlatformCodeBuddy, Status: StatusActive, Schedulable: true}
	repo := &codeBuddyModelLimitRepoStub{account: account}
	svc := NewRateLimitService(repo, nil, &config.Config{}, nil, nil)

	until := time.Now().UTC().Add(2 * time.Hour)
	svc.handleCodeBuddyModelLimit(context.Background(), account, "glm-5", until)

	// 按模型维度写入 model_rate_limits[model]，而非账号级 SetTempUnschedulable。
	require.Equal(t, 1, repo.modelCalls)
	require.Equal(t, "glm-5", repo.lastModelScope)
	require.WithinDuration(t, until, repo.lastModelReset, time.Second)
	require.Equal(t, 0, repo.tempCalls, "PR3 task1.5: 不得做账号级 SetTempUnschedulable")

	// 仅该模型不可调度，其它模型正常。
	require.False(t, account.IsSchedulableForModel("glm-5"))
	require.True(t, account.IsSchedulableForModel("glm-4"))

	// 可观测标记写入 Extra（非调度依据）。
	require.GreaterOrEqual(t, repo.updateExtra, 1)
	require.Contains(t, fmt.Sprint(repo.lastExtra["codebuddy_model_limit"]), "glm-5")
}
