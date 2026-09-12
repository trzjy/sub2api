package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// codeBuddyQuotaRepoStub 是 CodeBuddyQuotaService 测试用 AccountRepository 桩：
// 在 stubOpenAIAccountRepo（无 build tag 全量桩）之上仅覆盖 QueryUsage/FetchModels 实际调用的几个方法。
type codeBuddyQuotaRepoStub struct {
	stubOpenAIAccountRepo
	getByID     *Account
	updateExtra int
	lastExtra   map[string]any
}

func (r *codeBuddyQuotaRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	if r.getByID != nil {
		return r.getByID, nil
	}
	return &Account{
		ID:          id,
		Platform:    PlatformCodeBuddy,
		Status:      StatusActive,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "t"},
	}, nil
}

func (r *codeBuddyQuotaRepoStub) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.updateExtra++
	r.lastExtra = updates
	return nil
}

func healthyCodeBuddyQuotaAccount(id int64) *Account {
	return &Account{
		ID:          id,
		Platform:    PlatformCodeBuddy,
		Status:      StatusActive,
		Type:        AccountTypeOAuth,
		Schedulable: true,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token":  "access-token",
			"refresh_token": "refresh-token",
			"uid":           "u-1",
			"enterprise_id": "e-1",
			"domain":        "tencent.com",
		},
	}
}

// TestCodeBuddyQuotaService_QueryUsageWritesSnapshot 验证积分额度探测将快照写入
// Extra（codebuddy_credit_used_percent / reset_at），并在成功时清除错误标记。
func TestCodeBuddyQuotaService_QueryUsageWritesSnapshot(t *testing.T) {
	account := healthyCodeBuddyQuotaAccount(8801)
	repo := &codeBuddyQuotaRepoStub{getByID: account}

	resetAt := time.Now().UTC().Add(5 * time.Hour).Format(time.RFC3339)
	meterBody := `{"code":0,"data":{"creditUsedPercent":92,"reset_at":"` + resetAt + `"}}`
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(meterBody)),
	}}

	svc := NewCodeBuddyQuotaService(repo, nil, upstream, &config.Config{})

	result, err := svc.QueryUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Success)
	require.Equal(t, 92.0, result.UsedPercent)

	require.Equal(t, 1, repo.updateExtra, "额度快照应通过 UpdateExtra 落库")
	require.Equal(t, 92.0, repo.lastExtra[codebuddyCreditUsedPercentKey])
	require.Equal(t, resetAt, repo.lastExtra[codebuddyCreditResetAtKey])
	require.Nil(t, repo.lastExtra[codebuddyCreditErrorKey], "成功时应清除错误标记")
}

// codeBuddyModelsRecorder 构造一个返回动态模型列表 JSON 的 HTTP 桩（每次调用返回全新响应体，
// 避免响应体被单次读取后耗尽导致复用失败）。
func codeBuddyModelsRecorder(body string) *httpUpstreamRecorder {
	return &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}}
}

// TestCodeBuddyQuotaService_SupportedEffortsFromModelList 验证动态模型列表拉取与
// SupportedEffortsForModel 的缓存命中 / 缓存未命中惰性拉取两条路径。
func TestCodeBuddyQuotaService_SupportedEffortsFromModelList(t *testing.T) {
	account := healthyCodeBuddyQuotaAccount(8802)
	repo := &codeBuddyQuotaRepoStub{getByID: account}

	modelsBody := `{"code":0,"data":{"models":[{"id":"codebuddy-model-a","name":"Model A","supportedEfforts":["low","medium","high"]}]}}`

	// 1) 显式 FetchModels 后命中进程内缓存。
	svc := NewCodeBuddyQuotaService(repo, nil, codeBuddyModelsRecorder(modelsBody), &config.Config{})
	models, err := svc.FetchModels(context.Background(), account.ID)
	require.NoError(t, err)
	require.Len(t, models, 1)
	require.Equal(t, "codebuddy-model-a", models[0].ID)
	require.Equal(t, []string{"low", "medium", "high"}, svc.SupportedEffortsForModel(context.Background(), account, "codebuddy-model-a"))

	// 2) 独立实例（独立缓存）在缓存未命中时惰性拉取模型列表。
	svc2 := NewCodeBuddyQuotaService(repo, nil, codeBuddyModelsRecorder(modelsBody), &config.Config{})
	require.Equal(t, []string{"low", "medium", "high"}, svc2.SupportedEffortsForModel(context.Background(), account, "codebuddy-model-a"))
}
