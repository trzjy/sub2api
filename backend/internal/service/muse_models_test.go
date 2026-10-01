package service

// muse 平台上游模型目录同步测试（muse-4）：过滤规则（前缀 muse-spark + 必须 -contributor
// 结尾 + 显式排除 -free）、干净名→contributor 映射自动建立、负向（free 变体与非 muse 前缀
// 不入库）、预览不动库、运行观测结构化日志（不泄露密钥且字段可断言）。

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// museAccount 构造 muse 平台账号（api_key + base_url）。
func museAccount(id int64, mapping map[string]any) *Account {
	if mapping == nil {
		mapping = map[string]any{}
	}
	return &Account{
		ID:       id,
		Platform: PlatformMuse,
		Type:     AccountTypeAPIKey,
		Credentials: map[string]any{
			"api_key":       "muse-go-key",
			"base_url":      "https://opencode.ai/zen/go/v1",
			"model_mapping": mapping,
		},
	}
}

// museSyncHTTPStub 捕获 DoWithTLS 调用并按预设返回 muse /models 响应。
type museSyncHTTPStub struct {
	mu     sync.Mutex
	status int
	body   string
	urls   []string
}

func (u *museSyncHTTPStub) Do(_ *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.handle()
}
func (u *museSyncHTTPStub) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.mu.Lock()
	u.urls = append(u.urls, req.URL.String())
	u.mu.Unlock()
	return u.handle()
}
func (u *museSyncHTTPStub) handle() (*http.Response, error) {
	return &http.Response{
		StatusCode: u.status,
		Body:       io_NopCloser(strings.NewReader(u.body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}

func newMuseSyncSvc(stub *museSyncHTTPStub, repo *volcanoPlanSyncRepoStub) *AccountTestService {
	svc := &AccountTestService{
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
		httpUpstream: stub,
	}
	if repo != nil {
		svc.accountRepo = repo
	}
	return svc
}

// museModelsBody 构造 OpenAI 格式 /models 响应体。
func museModelsBody(ids ...string) string {
	items := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		items = append(items, map[string]string{"id": id})
	}
	b, _ := json.Marshal(map[string]any{"data": items})
	return string(b)
}

// TestFilterMuseUpstreamModels 锚定过滤规则（外审-2）：
//   - 前缀 muse-spark 且 -contributor 结尾 → 通过，派生干净名；
//   - -free 后缀（免费裁定）显式排除；
//   - 非 muse 前缀（deepseek-v4-flash / glm-5.3 / spark-foo-contributor）丢弃；
//   - muse 前缀但不以 -contributor 结尾（muse-spark-1.3）丢弃。
func TestFilterMuseUpstreamModels(t *testing.T) {
	t.Parallel()

	raw := []string{
		"muse-spark-1.3-contributor",
		"muse-spark-1.2-contributor",
		"muse-spark-1.3-contributor-free", // 免费变体
		"muse-spark-1.3",                  // 缺 contributor 后缀
		"deepseek-v4-flash",               // 非 muse 前缀
		"glm-5.3",                         // 非 muse 前缀
		"spark-foo-contributor",            // 非 muse 前缀
	}
	accepted, discarded := filterMuseUpstreamModels(raw)

	require.Len(t, accepted, 2)
	require.Equal(t, "muse-spark-1.2", accepted[0].CleanName)
	require.Equal(t, "muse-spark-1.2-contributor", accepted[0].ContributorID)
	require.Equal(t, "muse-spark-1.3", accepted[1].CleanName)
	require.Equal(t, "muse-spark-1.3-contributor", accepted[1].ContributorID)

	byReason := map[string][]string{}
	for _, d := range discarded {
		byReason[d.Reason] = append(byReason[d.Reason], d.Model)
	}
	require.Equal(t, []string{"muse-spark-1.3-contributor-free"}, byReason["free_variant"])
	require.Equal(t, []string{"muse-spark-1.3"}, byReason["suffix_not_contributor"])
	require.ElementsMatch(t, []string{"deepseek-v4-flash", "glm-5.3", "spark-foo-contributor"}, byReason["prefix"])
}

// TestSyncMuseModelCatalogAppliesCleanMapping 验证同步后 model_mapping 自动建立
// clean→contributor，且负向（free 变体 / 非 muse 前缀 / 缺后缀）均不入库。
func TestSyncMuseModelCatalogAppliesCleanMapping(t *testing.T) {
	t.Parallel()

	repo := &volcanoPlanSyncRepoStub{}
	stub := &museSyncHTTPStub{
		status: 200,
		body: museModelsBody(
			"muse-spark-1.3-contributor",
			"muse-spark-1.2-contributor",
			"muse-spark-1.3-contributor-free",
			"muse-spark-1.3",
			"deepseek-v4-flash",
			"glm-5.3",
		),
	}
	svc := newMuseSyncSvc(stub, repo)
	account := museAccount(201, nil)

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"muse-spark-1.2", "muse-spark-1.3"}, catalog.Models)

	// 落库：从 repo 写入的凭据中取 model_mapping。
	require.NotEmpty(t, repo.credUpdates)
	mapping := lastMuseMapping(t, repo)
	// 干净名 → contributor ID 映射自动建立。
	require.Equal(t, "muse-spark-1.3-contributor", mapping["muse-spark-1.3"])
	require.Equal(t, "muse-spark-1.2-contributor", mapping["muse-spark-1.2"])

	// 负向断言：free 变体、非 muse 前缀、缺后缀 一律不入库（不入为键，也不入为值）。
	require.NotContains(t, mapping, "muse-spark-1.3-contributor-free")
	require.NotContains(t, mapping, "muse-spark-1.3-contributor") // contributor ID 不作干净名键
	require.NotContains(t, mapping, "deepseek-v4-flash")
	require.NotContains(t, mapping, "glm-5.3")
	require.NotContains(t, mapping, "spark-foo-contributor")
	// contributor ID 也不应作为映射“值”以外地混入（如被误当作干净名）。
	for k := range mapping {
		require.True(t, strings.HasPrefix(k, "muse-spark-1."), "仅干净名应作为键：%s", k)
	}

	// 快照入库。
	require.NotEmpty(t, repo.extraUpdates)
}

// TestSyncMuseModelCatalogPreviewDoesNotApply 验证无账号 ID（预览）只返回分类，不动库。
func TestSyncMuseModelCatalogPreviewDoesNotApply(t *testing.T) {
	t.Parallel()

	repo := &volcanoPlanSyncRepoStub{}
	stub := &museSyncHTTPStub{
		status: 200,
		body:   museModelsBody("muse-spark-1.3-contributor", "deepseek-v4-flash"),
	}
	svc := newMuseSyncSvc(stub, repo)
	account := museAccount(0, nil) // 无 ID：预览

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"muse-spark-1.3"}, catalog.Models)

	extraCalls, credCalls := repo.counts()
	require.Zero(t, extraCalls, "预览不得写快照")
	require.Zero(t, credCalls, "预览不得写 model_mapping")
}

// TestSyncMuseModelCatalogLogsSuccessNoKeyLeak 验证成功日志字段齐全且不包含密钥。
// 非并行：museLogger 为包级变量，需独占设置避免与自身其它用例竞争。
func TestSyncMuseModelCatalogLogsSuccessNoKeyLeak(t *testing.T) {
	var buf syncBuffer
	prev := museLogger
	museLogger = slog.New(slog.NewJSONHandler(&buf, nil))
	t.Cleanup(func() { museLogger = prev })

	repo := &volcanoPlanSyncRepoStub{}
	stub := &museSyncHTTPStub{
		status: 200,
		body: museModelsBody(
			"muse-spark-1.3-contributor",
			"muse-spark-1.2-contributor",
			"muse-spark-1.3-contributor-free",
			"deepseek-v4-flash",
		),
	}
	svc := newMuseSyncSvc(stub, repo)
	account := museAccount(202, nil)

	_, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)

	out := buf.String()
	require.Contains(t, out, `"muse_model_sync_success"`)
	require.Contains(t, out, `"platform":"muse"`)
	require.Contains(t, out, `"sync_result":"success"`)
	require.Contains(t, out, `"upstream_status":200`)
	require.Contains(t, out, `"accepted_count":2`)
	require.Contains(t, out, `"discarded_count":2`)
	require.Contains(t, out, `"discarded_free":1`)
	require.Contains(t, out, `"discarded_prefix":1`)
	require.Contains(t, out, `"discarded_suffix":0`)
	require.Contains(t, out, `"consecutive_failures":0`)
	require.Contains(t, out, `"last_success_at"`)
	require.NotContains(t, out, "muse-go-key", "日志不得泄露 API key")
	require.NotContains(t, out, "api_key")
}

// TestSyncMuseModelCatalogLogsFailureNoKeyLeak 验证失败日志含上游状态码与连续失败次数，且不泄露密钥。
// 非并行：与成功日志用例共用 museLogger，需独占设置。
func TestSyncMuseModelCatalogLogsFailureNoKeyLeak(t *testing.T) {
	var buf syncBuffer
	prev := museLogger
	museLogger = slog.New(slog.NewJSONHandler(&buf, nil))
	t.Cleanup(func() { museLogger = prev })

	repo := &volcanoPlanSyncRepoStub{}
	stub := &museSyncHTTPStub{status: 500, body: `{"error":"boom"}`}
	svc := newMuseSyncSvc(stub, repo)
	account := museAccount(203, nil)

	_, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.Error(t, err)

	out := buf.String()
	require.Contains(t, out, `"muse_model_sync_failed"`)
	require.Contains(t, out, `"sync_result":"failed"`)
	require.Contains(t, out, `"upstream_status":500`)
	require.Contains(t, out, `"consecutive_failures":1`)
	require.NotContains(t, out, "muse-go-key", "日志不得泄露 API key")
}

// TestSyncMuseModelCatalogPreservesManualAlias 验证人工 alias 不被移除。
func TestSyncMuseModelCatalogPreservesManualAlias(t *testing.T) {
	t.Parallel()

	repo := &volcanoPlanSyncRepoStub{}
	stub := &museSyncHTTPStub{
		status: 200,
		body:   museModelsBody("muse-spark-1.3-contributor"),
	}
	svc := newMuseSyncSvc(stub, repo)
	// 人工 alias：my-alias -> muse-spark-1.3-contributor
	account := museAccount(204, map[string]any{"my-alias": "muse-spark-1.3-contributor"})

	_, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)

	mapping := lastMuseMapping(t, repo)
	require.Equal(t, "muse-spark-1.3-contributor", mapping["muse-spark-1.3"])
	require.Equal(t, "muse-spark-1.3-contributor", mapping["my-alias"], "人工 alias 必须保留")
}

// lastMuseMapping 从 repo 最后一次凭据写入中取 model_mapping。
func lastMuseMapping(t *testing.T, repo *volcanoPlanSyncRepoStub) map[string]string {
	t.Helper()
	require.NotEmpty(t, repo.credUpdates)
	last := repo.credUpdates[len(repo.credUpdates)-1]
	raw, ok := last["model_mapping"].(map[string]any)
	require.True(t, ok)
	out := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// syncBuffer 是并发安全的 slog 输出缓冲（用于断言日志字段）。
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
