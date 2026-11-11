//go:build unit

package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 本文件覆盖「单飞只合并取数、按账号各自应用」的三处修复：
//  1. 兄弟账号（同控制台账号、不同 api_key）并发探测各自归一/落库/调度；
//  2. 同账号同 api_key 的重复探测仍被 singleflight 合并；
//  3. 非 ok 响应不回写 storage_state；
//  4. 快照落库失败时跳过调度联动（失败关闭）。
//
// 测试替身一律在本文件内定义，不改既有测试文件。

// ---------------------------------------------------------------------------
// 假 worker：可按 api_key_sha256 返回不同响应，并支持并发闸门
// ---------------------------------------------------------------------------

// wcqApplyWorker 是 worker 的 httptest 替身：按请求里的 api_key_sha256 选择响应体。
type wcqApplyWorker struct {
	server *httptest.Server

	mu     sync.Mutex
	calls  int
	byHash map[string]string // api_key_sha256 → 响应体
	fixed  string            // 未命中 byHash 时的响应体

	// gate 非 nil 时，每次请求先阻塞到 gate 关闭，用于制造并发窗口。
	gate <-chan struct{}
	// started 非 nil 时，每次请求进入后投递一次信号（非阻塞）。
	started chan<- struct{}
}

func wcqNewApplyWorker(t *testing.T, w *wcqApplyWorker) *wcqApplyWorker {
	t.Helper()
	w.server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)

		w.mu.Lock()
		w.calls++
		w.mu.Unlock()

		if w.started != nil {
			select {
			case w.started <- struct{}{}:
			default:
			}
		}
		if w.gate != nil {
			<-w.gate
		}

		body := w.fixed
		if hash, _ := decoded["api_key_sha256"].(string); hash != "" && w.byHash != nil {
			if b, ok := w.byHash[hash]; ok {
				body = b
			}
		}

		rw.Header().Set("Content-Type", "application/json")
		rw.WriteHeader(http.StatusOK)
		_, _ = rw.Write([]byte(body))
	}))
	t.Cleanup(w.server.Close)
	return w
}

func (w *wcqApplyWorker) callCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls
}

// ---------------------------------------------------------------------------
// 构造辅助
// ---------------------------------------------------------------------------

const (
	wcqApplyHitAPIKey  = "sk-apply-hit"
	wcqApplyMissAPIKey = "sk-apply-miss"
	wcqApplySharedUser = "A"
)

// wcqApplyAccount 构造同控制台账号、指定 api_key 的 compshare 账号。
func wcqApplyAccount(id int64, apiKey string) *Account {
	return &Account{
		ID:       id,
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"base_url":              "https://cp.compshare.cn/api/v1",
			"api_key":               apiKey,
			"console_username":      wcqApplySharedUser,
			"console_password":      "pw-should-never-leak",
			"console_storage_state": `{"cookies":[]}`,
			"model_mapping":         map[string]any{"claude-3-5-sonnet": "deepseek-v3"},
		},
	}
}

// wcqApplyUnmatchedBody 构造多套餐且 key_hashes 不含目标 hash 的 data（→ unmatched）。
func wcqApplyUnmatchedBody() string {
	expire := time.Now().Add(30 * 24 * time.Hour).Unix()
	data := map[string]any{
		"auth_state": "ok",
		"plans": []map[string]any{
			{"plan_name": "A", "expire_at": expire, "plan_status": 1,
				"limit_5h": 100, "used_5h": 100, "key_hashes": []string{"deadbeef"}},
			{"plan_name": "B", "expire_at": expire, "plan_status": 1,
				"limit_5h": 100, "used_5h": 100, "key_hashes": []string{"cafebabe"}},
		},
	}
	raw, _ := json.Marshal(data)
	return wcqProbeBody(string(raw))
}

// wcqApplyProbeConcurrently 并发探测多个账号，返回值与入参下标一一对应。
func wcqApplyProbeConcurrently(t *testing.T, svc *WebConsoleQuotaService, accounts []*Account) []*WebConsoleQuotaProbeResult {
	t.Helper()
	results := make([]*WebConsoleQuotaProbeResult, len(accounts))
	errs := make([]error, len(accounts))

	var wg sync.WaitGroup
	for i := range accounts {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = svc.ProbeLoadedAccount(context.Background(), accounts[idx])
		}(i)
	}
	wg.Wait()

	for _, err := range errs {
		require.NoError(t, err)
	}
	return results
}

// wcqApplySnapshotByAccount 从 repo 记录的 UpdateExtra 调用还原「账号 → 落库快照」。
func wcqApplySnapshotByAccount(t *testing.T, repo *wcqRepoStub) map[int64]*WebConsoleQuotaSnapshot {
	t.Helper()
	repo.mu.Lock()
	defer repo.mu.Unlock()

	out := make(map[int64]*WebConsoleQuotaSnapshot, len(repo.updateExtraID))
	for i, id := range repo.updateExtraID {
		snap, ok := repo.updateExtras[i][webConsoleQuotaSnapshotExtraKey].(*WebConsoleQuotaSnapshot)
		require.True(t, ok, "UpdateExtra 必须写入快照对象")
		out[id] = snap
	}
	return out
}

// ---------------------------------------------------------------------------
// 用例 1：兄弟账号各自应用（不同 api_key 不合并）
// ---------------------------------------------------------------------------

func TestWebConsoleQuotaApply_SiblingAccountsApplyIndependently(t *testing.T) {
	hitHash := sha256Hex([]byte(wcqApplyHitAPIKey))
	missHash := sha256Hex([]byte(wcqApplyMissAPIKey))
	require.NotEqual(t, hitHash, missHash)

	// 命中账号：单套餐且 5h 窗口已用满 → matched + Exhausted。
	hitBody := wcqProbeBody(wcqCompsharePlanData(100, 100, time.Now().Add(2*time.Hour).Format(time.RFC3339)))
	worker := wcqNewApplyWorker(t, &wcqApplyWorker{
		byHash: map[string]string{
			hitHash:  hitBody,
			missHash: wcqApplyUnmatchedBody(),
		},
	})

	repo := &wcqRepoStub{setApplied: true}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	hit := wcqApplyAccount(101, wcqApplyHitAPIKey)
	miss := wcqApplyAccount(102, wcqApplyMissAPIKey)
	results := wcqApplyProbeConcurrently(t, svc, []*Account{hit, miss})

	// 不同 api_key 不得被合并：worker 必须被调用两次。
	require.Equal(t, 2, worker.callCount(), "不同 api_key 的兄弟账号必须各自请求 worker")

	for _, res := range results {
		require.NotNil(t, res)
	}

	// 两个账号各有自己的快照落库，内容按各自的 api_key 匹配。
	snaps := wcqApplySnapshotByAccount(t, repo)
	require.Len(t, snaps, 2, "两个账号都必须各自落库快照")
	require.Equal(t, "matched", snaps[101].PlanMatchStatus)
	require.True(t, snaps[101].Exhausted)
	require.Equal(t, "unmatched", snaps[102].PlanMatchStatus)
	require.False(t, snaps[102].Exhausted)

	// 只有命中套餐的账号执行调度判定；unmatched 账号零触碰停调原语。
	require.Equal(t, 1, repo.setCount(), "unmatched 账号不得触碰停调原语")
	call, ok := repo.lastSetCall()
	require.True(t, ok)
	require.Equal(t, int64(101), call.accountID)
	require.Equal(t, 0, repo.clearCount())
}

// ---------------------------------------------------------------------------
// 用例 2：同账号重复探测被合并
// ---------------------------------------------------------------------------

func TestWebConsoleQuotaApply_DuplicateProbeCoalesced(t *testing.T) {
	data := wcqCompsharePlanData(1200, 10, time.Now().Add(3*time.Hour).Format(time.RFC3339))
	body := wcqProbeBody(data)

	started := make(chan struct{}, 2)
	gate := make(chan struct{})
	worker := wcqNewApplyWorker(t, &wcqApplyWorker{
		fixed:   body,
		gate:    gate,
		started: started,
	})

	repo := &wcqRepoStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	account := wcqApplyAccount(201, wcqApplyHitAPIKey)

	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]*WebConsoleQuotaProbeResult, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ready <- struct{}{}
			<-start
			results[idx], _ = svc.ProbeLoadedAccount(context.Background(), account)
		}(i)
	}
	// 两个调用方都就绪后再同时进入：确保第二个调用落在首个 Do 的执行窗口内。
	<-ready
	<-ready
	close(start)

	// 等首个请求进入 worker（此时它已占用 singleflight），再让第二个调用进入 Do。
	<-started
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()

	require.Equal(t, 1, worker.callCount(), "同 provider/username/api_key 的重复探测必须合并")
	for _, res := range results {
		require.NotNil(t, res, "被合并的调用方也必须拿到自己的结果")
		require.True(t, res.Success)
	}
}

// ---------------------------------------------------------------------------
// 用例 3：非 ok 不写凭据
// ---------------------------------------------------------------------------

func TestWebConsoleQuotaApply_NotOKDoesNotPersistCredentials(t *testing.T) {
	body := `{"status":"auth_failed","provider":"compshare","account_label":"A",` +
		`"updated_storage_state":{"cookies":[{"name":"U_JWT_TOKEN","value":"stale"}],"origins":[]},` +
		`"error":"bad password"}`
	worker := wcqNewApplyWorker(t, &wcqApplyWorker{fixed: body})

	repo := &wcqRepoStub{}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	res, err := svc.ProbeLoadedAccount(context.Background(), wcqApplyAccount(301, wcqApplyHitAPIKey))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.Success)
	require.Equal(t, webConsoleQuotaAuthStateAuthFailed, res.AuthState)

	// 非 ok：即使带 updated_storage_state 也绝不刷新会话凭据。
	require.Equal(t, 0, repo.patchCount(), "非 ok 响应不得回写 storage_state")

	// 快照仍写入认证态，供前端提示重新登录。
	require.True(t, res.Persisted)
	snap := wcqApplySnapshotByAccount(t, repo)[301]
	require.NotNil(t, snap)
	require.Equal(t, webConsoleQuotaAuthStateAuthFailed, snap.AuthState)
	require.Equal(t, "bad password", snap.AuthError)

	require.Equal(t, 0, repo.setCount())
	require.Equal(t, 0, repo.clearCount())
}

// ---------------------------------------------------------------------------
// 用例 4：快照落库失败跳过调度联动
// ---------------------------------------------------------------------------

func TestWebConsoleQuotaApply_SnapshotPersistFailureSkipsScheduling(t *testing.T) {
	// 5h 窗口已用满：若无失败关闭会写停调。
	data := wcqCompsharePlanData(100, 100, time.Now().Add(3*time.Hour).Format(time.RFC3339))
	worker := wcqNewApplyWorker(t, &wcqApplyWorker{fixed: wcqProbeBody(data)})

	repo := &wcqRepoStub{updateErr: errors.New("db down"), setApplied: true}
	svc := NewWebConsoleQuotaService(repo, wcqConfig(worker.server.URL))

	res, err := svc.ProbeLoadedAccount(context.Background(), wcqApplyAccount(401, wcqApplyHitAPIKey))
	require.NoError(t, err)
	require.NotNil(t, res)
	require.True(t, res.Exhausted)
	require.False(t, res.Persisted)

	require.Equal(t, 0, repo.setCount(), "快照落库失败时不得写停调")
	require.Equal(t, 0, repo.clearCount(), "快照落库失败时不得清除停调")
}
