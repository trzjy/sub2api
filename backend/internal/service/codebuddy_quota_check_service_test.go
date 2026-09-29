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

// codeBuddyQuotaCheckRepoStub 是 CodeBuddyQuotaCheckService 测试用 AccountRepository 桩。
type codeBuddyQuotaCheckRepoStub struct {
	stubOpenAIAccountRepo
	listCalls      int
	listOut        []Account
	getCalls       int
	getByID        *Account
	updateExtra    int
	lastExtra      map[string]any
	nextVer        int64
	snapshotWrites []CodeBuddyCreditSnapshotWrite
}

func (r *codeBuddyQuotaCheckRepoStub) ListByPlatform(_ context.Context, _ string) ([]Account, error) {
	r.listCalls++
	return r.listOut, nil
}

func (r *codeBuddyQuotaCheckRepoStub) GetByID(_ context.Context, id int64) (*Account, error) {
	r.getCalls++
	if r.getByID != nil {
		return r.getByID, nil
	}
	return &Account{ID: id, Platform: PlatformCodeBuddy, Status: StatusActive, Type: AccountTypeOAuth}, nil
}

func (r *codeBuddyQuotaCheckRepoStub) UpdateExtra(_ context.Context, _ int64, updates map[string]any) error {
	r.updateExtra++
	r.lastExtra = updates
	return nil
}

// --- CodeBuddyConditionalExtraWriter 窄接口实现（Card B） ---

func (r *codeBuddyQuotaCheckRepoStub) NextCodeBuddyCreditAttemptVersion(_ context.Context) (int64, error) {
	return r.nextVer, nil
}

func (r *codeBuddyQuotaCheckRepoStub) WriteCodeBuddyCreditSnapshot(_ context.Context, _ int64, write CodeBuddyCreditSnapshotWrite) (bool, error) {
	r.snapshotWrites = append(r.snapshotWrites, write)
	return true, nil
}

func (r *codeBuddyQuotaCheckRepoStub) WriteCodeBuddyCreditAttemptError(_ context.Context, _ int64, _ CodeBuddyCreditAttemptErrorWrite) (bool, error) {
	return true, nil
}

// TestCodeBuddyQuotaCheckService_StartDisabledIsNoOp 验证配置 gate 关闭时 Start() 直接返回，
// 不启动探测循环（不调用 ListByPlatform）。
func TestCodeBuddyQuotaCheckService_StartDisabledIsNoOp(t *testing.T) {
	repo := &codeBuddyQuotaCheckRepoStub{}
	cfg := &config.Config{Gateway: config.GatewayConfig{CodeBuddy: config.GatewayCodeBuddyConfig{
		QuotaCheckEnabled: false,
	}}}
	quota := NewCodeBuddyQuotaService(repo, nil, nil, cfg)
	rl := NewRateLimitService(repo, nil, cfg, nil, nil)
	svc := NewCodeBuddyQuotaCheckService(repo, quota, rl, cfg, time.Minute)

	svc.Start()
	svc.Stop()

	require.Equal(t, 0, repo.listCalls, "disabled 配置不应启动探测循环")
}

// TestCodeBuddyQuotaCheckService_RunOnceProbesAndAppliesThreshold 验证 runOnce 的骨架行为：
// 枚举 codebuddy 账号 → 逐账号 QueryUsage 落额度快照 → reload 后调用 ApplyAccountSchedulingThreshold。
func TestCodeBuddyQuotaCheckService_RunOnceProbesAndAppliesThreshold(t *testing.T) {
	account := healthyCodeBuddyQuotaAccount(8901)
	repo := &codeBuddyQuotaCheckRepoStub{
		getByID: account,
		listOut: []Account{*account},
	}
	cfg := &config.Config{Gateway: config.GatewayConfig{CodeBuddy: config.GatewayCodeBuddyConfig{
		QuotaCheckEnabled:   true,
		DailyCheckinEnabled: false,
	}}}

	// 真实计费 schema（data.Response.Data.Accounts[]，PascalCase 容量计数器，
	// *Precise 定点字段，§4.1 分包解析用例）。
	meterBody := `{"code":0,"data":{"Response":{"Data":{"TotalCount":1,"TotalDosage":100,"Accounts":[
		{"AccountId":1,"PackageName":"p","CapacityUnit":"credits","CapacitySize":100,"CapacityUsed":99,"CapacityRemain":1,
		 "CapacitySizePrecise":"100","CapacityRemainPrecise":"1","CycleEndTime":"2026-10-13 07:46:09"}
	]}}}}`
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(meterBody)),
	}}

	quota := NewCodeBuddyQuotaService(repo, nil, upstream, cfg)
	rl := NewRateLimitService(repo, nil, cfg, nil, nil)
	svc := NewCodeBuddyQuotaCheckService(repo, quota, rl, cfg, time.Minute)

	svc.runOnce()

	require.Equal(t, 1, repo.listCalls, "runOnce 必须枚举 codebuddy 账号")
	require.GreaterOrEqual(t, repo.getCalls, 2, "QueryUsage 与 reload 各调一次 GetByID")
	require.GreaterOrEqual(t, len(repo.snapshotWrites), 1, "QueryUsage 必须经条件更新写口落额度快照")
	require.Equal(t, 0, repo.updateExtra, "额度快照不得再走旧 UpdateExtra 写口")
	require.Equal(t, 99.0, repo.snapshotWrites[0].UsedPercent)
}
