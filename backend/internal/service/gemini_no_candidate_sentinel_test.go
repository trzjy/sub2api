//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/stretchr/testify/require"
)

// W2-C（方案 §25.14-B 响应矩阵「原生候选查询失败」行 /「读取与投影」错误契约）：
// 无候选的**明确分类**。
//
// 本文件只钉死一件事：Gemini 选号方法的「没有合格候选」必须是可 errors.Is 的确定分类
// （ErrGeminiNoAvailableAccounts），而「读不到」（账号/来源/分组数据库故障）不得被折叠成
// 同一结论——调用方据此区分 200 models: [] 与 503 目录读取失败，禁止匹配错误字符串。

// geminiNoCandidateRepoStub 只实现 Gemini 选号会用到的那一个查询。
type geminiNoCandidateRepoStub struct {
	AccountRepository

	accounts []Account
	err      error
}

func (s *geminiNoCandidateRepoStub) ListSchedulableByGroupIDAndPlatforms(_ context.Context, _ int64, _ []string) ([]Account, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.accounts, nil
}

// TestGeminiNoCandidateSentinelClassification 钉死无候选分类与读取失败的区分。
func TestGeminiNoCandidateSentinelClassification(t *testing.T) {
	groupID := int64(9901)
	group := &Group{ID: groupID, Platform: PlatformGemini, Status: StatusActive, Hydrated: true}
	ctx := context.WithValue(context.Background(), ctxkey.Group, group)
	cursorOnly := Account{
		ID:          9901,
		Platform:    PlatformCursor,
		Type:        AccountTypeAPIKey,
		Status:      StatusActive,
		Schedulable: true,
	}

	t.Run("空组是可判定的无候选分类", func(t *testing.T) {
		svc := &GeminiMessagesCompatService{accountRepo: &geminiNoCandidateRepoStub{}}

		_, err := svc.SelectAccountForAIStudioEndpoints(context.Background(), &groupID)
		require.Error(t, err)
		require.True(t, errors.Is(err, ErrGeminiNoAvailableAccounts),
			"无候选必须是可 errors.Is 判定的分类，调用方禁止匹配错误字符串")
		require.Contains(t, err.Error(), "no available Gemini accounts", "对外文案保持原样")
	})

	t.Run("只有 cursor 账号同样是原生无候选", func(t *testing.T) {
		svc := &GeminiMessagesCompatService{accountRepo: &geminiNoCandidateRepoStub{accounts: []Account{cursorOnly}}}

		_, err := svc.SelectAccountForAIStudioEndpoints(context.Background(), &groupID)
		require.True(t, errors.Is(err, ErrGeminiNoAvailableAccounts))
	})

	t.Run("读取失败不得折叠成无候选", func(t *testing.T) {
		readErr := errors.New("accounts table unreachable")
		svc := &GeminiMessagesCompatService{accountRepo: &geminiNoCandidateRepoStub{err: readErr}}

		_, err := svc.SelectAccountForAIStudioEndpoints(context.Background(), &groupID)
		require.Error(t, err)
		require.False(t, errors.Is(err, ErrGeminiNoAvailableAccounts),
			"数据库读取失败不是「没有候选」，不得据此返回空目录")
		require.ErrorIs(t, err, readErr, "原始读取错误必须可判定，便于失败关闭")
	})

	t.Run("通用选号链沿用同一分类且文案不变", func(t *testing.T) {
		svc := &GeminiMessagesCompatService{accountRepo: &geminiNoCandidateRepoStub{}}

		_, err := svc.SelectAccountForModelWithExclusions(ctx, &groupID, "", "cursor-sonnet", nil)
		require.Error(t, err)
		require.True(t, errors.Is(err, ErrGeminiNoAvailableAccounts))
		require.Equal(t, "no available Gemini accounts supporting model: cursor-sonnet", err.Error(),
			"带请求模型的错误文案与分类前逐字一致")

		_, err = svc.SelectAccountForModelWithExclusions(ctx, &groupID, "", "", nil)
		require.True(t, errors.Is(err, ErrGeminiNoAvailableAccounts))
		require.Equal(t, "no available Gemini accounts", err.Error())
	})
}
