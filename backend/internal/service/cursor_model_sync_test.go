//go:build unit

package service

// cursor_model_sync_test.go 是 cursor_model_sync.go 的配对单测（P1-B1）。
//
// 覆盖卡内 Done when：
//   1. cursor 账号走 sync-upstream 返回目录并原子落 mapping——成功写入 / 失败不落半份 /
//      空目录报错三分支；
//   2. 失败路径按 UpstreamModelSyncError 明确分类（upstream / configuration /
//      credential / internal）；
//   3. 默认目录来源的真实 HTTP 往返口径（Unary 路径 + application/proto +
//      x-jg-auth），以及 241 档位项折叠、既有映射键不被改写两条固定语义。

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// cursorModelSyncRepoStub 记录凭据写链调用：原子性靠“调用次数”与“落库内容”双断言。
type cursorModelSyncRepoStub struct {
	AccountRepository
	mu       sync.Mutex
	calls    int
	id       int64
	snapshot map[string]any
	err      error
}

func (r *cursorModelSyncRepoStub) UpdateCredentials(_ context.Context, id int64, credentials map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.id = id
	r.snapshot = shallowCopyMap(credentials)
	return r.err
}

func (r *cursorModelSyncRepoStub) writeCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *cursorModelSyncRepoStub) writtenMapping() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, _ := r.snapshot["model_mapping"].(map[string]any)
	return raw
}

// cursorModelSyncSourceStub 是 cursorUsableModelsSource 的内存替身。
type cursorModelSyncSourceStub struct {
	mu      sync.Mutex
	entries []cursorUsableModel
	err     error
	calls   int
}

func (s *cursorModelSyncSourceStub) CursorUsableModels(_ context.Context, _ *Account) ([]cursorUsableModel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.entries, s.err
}

// installCursorModelSyncSource 安装替身并在测试结束时还原（包级位点，不可并行）。
func installCursorModelSyncSource(t *testing.T, src cursorUsableModelsSource) *cursorModelSyncSourceStub {
	t.Helper()
	stub, ok := src.(*cursorModelSyncSourceStub)
	if ok {
		InstallCursorUsableModelsSource(stub)
		t.Cleanup(func() { InstallCursorUsableModelsSource(nil) })
		return stub
	}
	InstallCursorUsableModelsSource(src)
	t.Cleanup(func() { InstallCursorUsableModelsSource(nil) })
	return nil
}

// cursorModelSyncUpstreamStub 是 HTTPUpstream 替身：记录出站请求并按 handler 回包。
type cursorModelSyncUpstreamStub struct {
	mu       sync.Mutex
	reqPath  string
	headers  http.Header
	requests int

	handler func(w http.ResponseWriter, r *http.Request)
}

func (u *cursorModelSyncUpstreamStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	return u.DoWithTLS(req, "", 0, 0, nil)
}

func (u *cursorModelSyncUpstreamStub) DoWithTLS(req *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	u.mu.Lock()
	u.reqPath = req.URL.Path
	u.headers = req.Header.Clone()
	u.requests++
	u.mu.Unlock()

	rec := httptest.NewRecorder()
	if u.handler != nil {
		u.handler(rec, req)
	} else {
		rec.WriteHeader(http.StatusOK)
	}
	return rec.Result(), nil
}

func (u *cursorModelSyncUpstreamStub) path() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.reqPath
}

// ---------------------------------------------------------------------------
// 目录报文构造
// ---------------------------------------------------------------------------

// encodeCursorUsableModelsResponse 按 GetUsableModelsResponse 的 wire 形状编码报文。
// 字段号与 repeated 语义取自 cursor_model_sync.go 的常量，避免测试与实现漂移。
func encodeCursorUsableModelsResponse(entries []cursorUsableModel) []byte {
	var out []byte
	for _, entry := range entries {
		var item []byte
		item = cursorAppendStringField(item, cursorFldUsableModelID, entry.ServerModelName)
		item = cursorAppendStringField(item, cursorFldUsableDisplayModelID, entry.DisplayModelID)
		item = cursorAppendStringField(item, cursorFldUsableDisplayName, entry.DisplayName)
		for _, slug := range entry.LegacySlugs {
			item = cursorAppendStringField(item, cursorFldUsableAliases, slug)
		}
		if entry.MaxMode {
			item = cursorAppendVarintField(item, cursorFldUsableMaxMode, 1)
		}
		out = cursorAppendBytesField(out, cursorFldUsableModelsResponseModels, item)
	}
	return out
}

// cursorModelSyncFixture 造一套「已注入替身来源 + 记账 repo」的同步夹具。
func cursorModelSyncFixture(t *testing.T) (*AccountTestService, *cursorModelSyncRepoStub) {
	t.Helper()
	repo := &cursorModelSyncRepoStub{}
	svc := NewAccountTestService(repo, nil, nil, nil, nil, nil, upstreamModelSyncTestConfig(), nil)
	return svc, repo
}

// cursorModelSyncAccount 是一条带既有映射的 cursor 账号：既有键用于断言「只补缺键」。
func cursorModelSyncAccount() *Account {
	return &Account{
		ID:         88,
		Platform:   PlatformCursor,
		QuotaKeyID: qkID(7),
		Credentials: map[string]any{
			"model_mapping": map[string]any{
				"cursor-sonnet": "legacy-manual-target",  // 存量人工映射
				"gpt-5":         "manually-frozen-value", // 存量人工限制，不得被批量放开
			},
		},
	}
}

// cursorModelSyncEntries 模拟 241 档位项的关键形状：同名 wire ID 的两个档位（普通 /
// max_mode）+ 一个独立模型；另有一条缺 model_id 的脏项。
func cursorModelSyncEntries() []cursorUsableModel {
	return []cursorUsableModel{
		{
			ServerModelName: "claude-opus-4-8-thinking",
			DisplayModelID:  "claude-opus-4-8-thinking",
			DisplayName:     "Opus 4.8 Thinking",
			LegacySlugs:     []string{"claude-opus-4-8"},
		},
		{ // 同一 wire ID 的 max 档位：折叠，不新增对外模型
			ServerModelName: "claude-opus-4-8-thinking",
			MaxMode:         true,
		},
		{
			ServerModelName: "gpt-5-high",
			DisplayName:     "GPT-5 High",
			LegacySlugs:     []string{"gpt-5-high-fast", "gpt-5"},
		},
		{ // 无 wire ID：不可转发，跳过且不作废整份目录
			DisplayName: "ghost-tier-row",
		},
	}
}

// requireUpstreamModelSyncError 断言错误已按 UpstreamModelSyncError 归类。
func requireUpstreamModelSyncError(t *testing.T, err error, kind UpstreamModelSyncErrorKind) *UpstreamModelSyncError {
	t.Helper()
	require.Error(t, err)
	var syncErr *UpstreamModelSyncError
	require.True(t, errors.As(err, &syncErr), "error must be *UpstreamModelSyncError, got %T: %v", err, err)
	require.Equal(t, kind, syncErr.Kind)
	return syncErr
}

// ---------------------------------------------------------------------------
// Done when 1：成功分支——原子写 mapping
// ---------------------------------------------------------------------------

// TestCursorModelSyncSuccessWritesMappingOnce 钉死成功路径：返回折叠后的对外模型清单，
// 并**恰好一次**落库；mapping 只补缺键，既有人工映射原样保留。
func TestCursorModelSyncSuccessWritesMappingOnce(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorModelSyncSourceStub{entries: cursorModelSyncEntries()})
	account := cursorModelSyncAccount()

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.NotNil(t, catalog)

	// 241 档位项不照抄：同名 wire ID 的两个档位折叠成一条对外模型。
	require.Equal(t, []string{"claude-opus-4-8-thinking", "gpt-5-high"}, catalog.Models)

	// 原子写 = 一次 UpdateCredentials，不多不少。
	require.Equal(t, 1, repo.writeCalls())
	require.EqualValues(t, account.ID, repo.id)

	mapping := repo.writtenMapping()
	require.NotNil(t, mapping, "model_mapping must be persisted through the credential write chain")

	// 身份键（server_model_name → 自身）必须写：转发侧闸门要求显式命中才出站。
	require.Equal(t, "claude-opus-4-8-thinking", mapping["claude-opus-4-8-thinking"])
	require.Equal(t, "gpt-5-high", mapping["gpt-5-high"])

	// legacy_slugs 落成别名键，指向同一 wire ID。
	require.Equal(t, "claude-opus-4-8-thinking", mapping["claude-opus-4-8"])
	require.Equal(t, "gpt-5-high", mapping["gpt-5-high-fast"])

	// 不自动恢复运营停用状态 / 不批量放开存量人工限制：既有键一律不改写。
	require.Equal(t, "legacy-manual-target", mapping["cursor-sonnet"])
	require.Equal(t, "manually-frozen-value", mapping["gpt-5"],
		"existing manual mapping value must never be overwritten by sync")

	// 缺 wire ID 的脏项不进目录，也不进 mapping。
	require.NotContains(t, catalog.Models, "")
	require.Len(t, catalog.Models, 2)
}

// TestCursorModelSyncSuccessIsIdempotent 钉死幂等：目录无新增键时不发起写链调用。
func TestCursorModelSyncSuccessIsIdempotent(t *testing.T) {
	entries := []cursorUsableModel{{ServerModelName: "cursor-small", LegacySlugs: []string{"cursor-small"}}}
	svc, repo := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorModelSyncSourceStub{entries: entries})
	account := cursorModelSyncAccount()
	account.Credentials["model_mapping"] = map[string]any{
		"cursor-small": "cursor-small", // 已完整存在
	}

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, []string{"cursor-small"}, catalog.Models)
	require.Equal(t, 0, repo.writeCalls(), "no new key means no credential write")
}

// TestCursorModelSyncPreviewDoesNotPersist 钉死 preview 口径（无 ID / 无 repo）：只返回目录不落库。
func TestCursorModelSyncPreviewDoesNotPersist(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorModelSyncSourceStub{entries: cursorModelSyncEntries()})

	preview := cursorModelSyncAccount()
	preview.ID = 0
	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), preview)
	require.NoError(t, err)
	require.Len(t, catalog.Models, 2)
	require.Equal(t, 0, repo.writeCalls())
}

// ---------------------------------------------------------------------------
// Done when 1：失败分支——不落半份
// ---------------------------------------------------------------------------

// TestCursorModelSyncUpstreamFailureWritesNothing 钉死「失败不落半份」：
// 来源报错 → 整体报错，一次都不碰写链，既有 credentials 原样。
func TestCursorModelSyncUpstreamFailureWritesNothing(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorModelSyncSourceStub{err: errors.New("dial tcp: upstream unreachable")})
	account := cursorModelSyncAccount()
	before := shallowCopyMap(account.Credentials)

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.Nil(t, catalog)
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorUpstream)
	require.Equal(t, 0, repo.writeCalls(), "failed sync must not touch the credential write chain")
	require.Equal(t, before["model_mapping"], account.Credentials["model_mapping"])
}

// TestCursorModelSyncEmptyCatalogErrors 钉死空目录：既不允许写空 mapping，也不允许当成功。
func TestCursorModelSyncEmptyCatalogErrors(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorModelSyncSourceStub{entries: nil})
	account := cursorModelSyncAccount()

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.Nil(t, catalog)
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorUpstream)
	require.Equal(t, 0, repo.writeCalls())
	require.NotNil(t, account.Credentials["model_mapping"], "existing mapping must survive an empty catalog")
}

// TestCursorModelSyncUnusableRowsOnlyStillErrors 钉死「只有脏项」= 空目录的同一失败语义。
func TestCursorModelSyncUnusableRowsOnlyStillErrors(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorModelSyncSourceStub{entries: []cursorUsableModel{
		{DisplayName: "ghost-tier-row"},
		{ServerModelName: "   "},
	}})

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), &Account{ID: 91, Platform: PlatformCursor})
	require.Nil(t, catalog)
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorUpstream)
	require.Equal(t, 0, repo.writeCalls())
}

// TestCursorModelSyncPersistenceFailureIsInternal 钉死落库失败：归类 internal，不外泄内部错误。
func TestCursorModelSyncPersistenceFailureIsInternal(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorModelSyncSourceStub{entries: cursorModelSyncEntries()})
	repo.err = errors.New("database is down")

	_, err := svc.SyncUpstreamModelCatalog(context.Background(), cursorModelSyncAccount())
	syncErr := requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorInternal)
	require.Contains(t, syncErr.Message, "Failed to save cursor model mapping")
	require.NotEmpty(t, syncErr.SafeMessage())
	require.NotContains(t, syncErr.SafeMessage(), "database is down", "internal cause must not reach clients")
}

// ---------------------------------------------------------------------------
// Done when 2：失败分类
// ---------------------------------------------------------------------------

// TestCursorModelSyncMissingAuthDepsIsConfiguration 钉死依赖缺失：未注入 cursor 鉴权依赖
// → configuration 类错误，绝不回落无凭证请求。
func TestCursorModelSyncMissingAuthDepsIsConfiguration(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, nil) // 恢复默认实现，走依赖校验分支

	_, err := svc.SyncUpstreamModelCatalog(context.Background(), cursorModelSyncAccount())
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorConfiguration)
	require.Equal(t, 0, repo.writeCalls())
}

// TestCursorModelSyncNilAccountIsConfiguration 钉死入参防御：nil 账号不入网也不落库。
func TestCursorModelSyncNilAccountIsConfiguration(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorModelSyncSourceStub{entries: cursorModelSyncEntries()})

	_, err := svc.SyncUpstreamModelCatalog(context.Background(), nil)
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorConfiguration)
	require.Equal(t, 0, repo.writeCalls())
}

// TestCursorModelSyncErrorSourceInstallation 替身来源本身返回的 *UpstreamModelSyncError
// 必须**原样透传**（不丢具体分类与状态码）。
func TestCursorModelSyncTypedErrorPassthrough(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorModelSyncSourceStub{
		err: &UpstreamModelSyncError{
			Kind:       UpstreamModelSyncErrorCredential,
			Message:    "Cursor upstream rejected the account credentials",
			StatusCode: http.StatusForbidden,
		},
	})

	_, err := svc.SyncUpstreamModelCatalog(context.Background(), cursorModelSyncAccount())
	syncErr := requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorCredential)
	require.Equal(t, http.StatusForbidden, syncErr.StatusCode)
	require.Equal(t, 0, repo.writeCalls())
}

// ---------------------------------------------------------------------------
// 默认目录来源：真实 HTTP 往返口径
// ---------------------------------------------------------------------------

// newCursorModelSyncUpstreamFixture 造一个「节点基址指向 httptest」的默认目录来源。
func newCursorModelSyncUpstreamFixture(
	t *testing.T,
	svc *AccountTestService,
	authValue string,
	handler func(w http.ResponseWriter, r *http.Request),
) (*cursorDefaultUsableModelsSource, *cursorModelSyncUpstreamStub) {
	t.Helper()
	stub := &cursorModelSyncUpstreamStub{handler: handler}
	svc.httpUpstream = stub

	src := newCursorDefaultUsableModelsSource(
		svc,
		withCursorModelSyncBaseURL(func(context.Context) string { return "https://cursor.example.test" }),
		withCursorModelSyncAuth(func(context.Context, *Account) (CursorUpstreamAuthProvider, error) {
			return staticCursorAuthProvider{value: authValue}, nil
		}),
	)
	return src, stub
}

// TestCursorModelSyncDefaultSourceRoundTrip 钉死 Unary 出站形状 + 目录解析：
// 路径 / Content-Type / x-jg-auth 逐项对齐，响应按 ModelDetails 解析。
func TestCursorModelSyncDefaultSourceRoundTrip(t *testing.T) {
	svc, _ := cursorModelSyncFixture(t)
	body := encodeCursorUsableModelsResponse([]cursorUsableModel{
		{ServerModelName: "claude-sonnet-4-6", DisplayName: "Sonnet 4.6", LegacySlugs: []string{"claude-sonnet-4-6-20250101"}},
		{ServerModelName: "cursor-small", DisplayName: "Cursor Small"},
	})
	src, stub := newCursorModelSyncUpstreamFixture(t, svc, "TEST-AUTH-CIPHER", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", cursorUnaryContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})

	entries, err := src.CursorUsableModels(context.Background(), cursorModelSyncAccount())
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "claude-sonnet-4-6", entries[0].ServerModelName)
	require.Equal(t, []string{"claude-sonnet-4-6-20250101"}, entries[0].LegacySlugs)
	require.Equal(t, "cursor-small", entries[1].ServerModelName)

	require.Equal(t, cursorModelSyncPathGetUsableModels, stub.path())
	// Unary 裸 protobuf：既不能写成 connect+proto（会被上游 415），也不能带 5 字节 envelope。
	require.Equal(t, cursorUnaryContentType, stub.headers.Get("Content-Type"))
	require.Equal(t, "TEST-AUTH-CIPHER", stub.headers.Get(cursorJgAuthHeader))
	require.Equal(t, cursorJgModelHeaderValue, stub.headers.Get(cursorJgModelHeader))
}

// TestCursorModelSyncMaxModeRowRoundTrip 钉死 max_mode 变体解析（bool varint 字段 7）。
func TestCursorModelSyncMaxModeRowRoundTrip(t *testing.T) {
	entries, err := parseCursorUsableModelsResponse(encodeCursorUsableModelsResponse([]cursorUsableModel{
		{ServerModelName: "gemini-3-pro", MaxMode: true},
		{ServerModelName: "gemini-3-flash", MaxMode: false},
	}))
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.True(t, entries[0].MaxMode)
	require.False(t, entries[1].MaxMode)
}

// TestCursorModelSyncMalformedBodyErrors 钉死解析失败关闭：非法 protobuf 不当 0 模型成功。
func TestCursorModelSyncMalformedBodyErrors(t *testing.T) {
	for name, payload := range map[string][]byte{
		"empty":   {},
		"garbage": []byte("\xff\xff\xff\xff\xff"),
		"truncated": func() []byte {
			full := encodeCursorUsableModelsResponse([]cursorUsableModel{{ServerModelName: "cursor-small"}})
			return full[:len(full)-1]
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseCursorUsableModelsResponse(payload)
			require.Error(t, err)
		})
	}
}

// TestCursorModelSyncHTTPStatusIsClassified 钉死 HTTP 状态分类：
// 401/403 → credential（且不回显 x-jg-auth 材料）；5xx → upstream。
func TestCursorModelSyncHTTPStatusIsClassified(t *testing.T) {
	const secret = "SUPER-SECRET-AUTH-MATERIAL"
	leakyBody := []byte("rejected:" + secret)

	t.Run("401 is credential", func(t *testing.T) {
		svc, _ := cursorModelSyncFixture(t)
		src, _ := newCursorModelSyncUpstreamFixture(t, svc, secret, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write(leakyBody)
		})
		_, err := src.CursorUsableModels(context.Background(), cursorModelSyncAccount())
		syncErr := requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorCredential)
		require.Equal(t, http.StatusUnauthorized, syncErr.StatusCode)
		require.NotContains(t, err.Error(), secret, "auth material must never be echoed back")
	})

	t.Run("403 is credential", func(t *testing.T) {
		svc, _ := cursorModelSyncFixture(t)
		src, _ := newCursorModelSyncUpstreamFixture(t, svc, secret, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write(leakyBody)
		})
		_, err := src.CursorUsableModels(context.Background(), cursorModelSyncAccount())
		requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorCredential)
	})

	t.Run("500 is upstream", func(t *testing.T) {
		svc, _ := cursorModelSyncFixture(t)
		src, _ := newCursorModelSyncUpstreamFixture(t, svc, secret, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		})
		_, err := src.CursorUsableModels(context.Background(), cursorModelSyncAccount())
		syncErr := requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorUpstream)
		require.Equal(t, http.StatusInternalServerError, syncErr.StatusCode)
	})
}

// TestCursorModelSyncAuthFailureIsConfigured 钉死鉴权材料不可得 → 不发请求（不白耗额度）。
func TestCursorModelSyncAuthFailureIsConfigured(t *testing.T) {
	svc, _ := cursorModelSyncFixture(t)
	stub := &cursorModelSyncUpstreamStub{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encodeCursorUsableModelsResponse([]cursorUsableModel{{ServerModelName: "cursor-small"}}))
	}}
	svc.httpUpstream = stub
	src := newCursorDefaultUsableModelsSource(
		svc,
		withCursorModelSyncBaseURL(func(context.Context) string { return "https://cursor.example.test" }),
		withCursorModelSyncAuth(func(context.Context, *Account) (CursorUpstreamAuthProvider, error) {
			return nil, errors.New("key material unavailable")
		}),
	)

	_, err := src.CursorUsableModels(context.Background(), cursorModelSyncAccount())
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorConfiguration)
	require.Equal(t, 0, stub.requests, "no request must leave the process without auth material")
}

// TestCursorModelSyncEmptyBaseURLFailsClosed 钉死节点基址缺失 → 不发请求。
func TestCursorModelSyncEmptyBaseURLFailsClosed(t *testing.T) {
	svc, _ := cursorModelSyncFixture(t)
	stub := &cursorModelSyncUpstreamStub{}
	svc.httpUpstream = stub
	src := newCursorDefaultUsableModelsSource(
		svc,
		withCursorModelSyncBaseURL(func(context.Context) string { return "  " }),
		withCursorModelSyncAuth(func(context.Context, *Account) (CursorUpstreamAuthProvider, error) {
			return staticCursorAuthProvider{value: "AUTH"}, nil
		}),
	)

	_, err := src.CursorUsableModels(context.Background(), cursorModelSyncAccount())
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorConfiguration)
	require.Equal(t, 0, stub.requests)
}

// TestCursorModelSyncMissingHTTPUpstreamIsConfiguration 钉死出站客户端缺失：
// 归类 configuration 而不是 nil 指针崩溃，与通用 /models 路径口径一致。
func TestCursorModelSyncMissingHTTPUpstreamIsConfiguration(t *testing.T) {
	svc, _ := cursorModelSyncFixture(t)
	require.Nil(t, svc.httpUpstream)
	src := newCursorDefaultUsableModelsSource(
		svc,
		withCursorModelSyncBaseURL(func(context.Context) string { return "https://cursor.example.test" }),
		withCursorModelSyncAuth(func(context.Context, *Account) (CursorUpstreamAuthProvider, error) {
			return staticCursorAuthProvider{value: "AUTH"}, nil
		}),
	)

	_, err := src.CursorUsableModels(context.Background(), cursorModelSyncAccount())
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorConfiguration)
}

// TestCursorModelSyncDefaultSourceEndToEndWrites 钉死「默认来源 → 适配 → 落库」全链路，
// 且不新增定时器/ticker（本卡只挂现有手动入口）。
func TestCursorModelSyncDefaultSourceEndToEndWrites(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	src, _ := newCursorModelSyncUpstreamFixture(t, svc, "AUTH", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", cursorUnaryContentType)
		_, _ = w.Write(encodeCursorUsableModelsResponse(cursorModelSyncEntries()))
	})
	installCursorModelSyncSource(t, src)

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), cursorModelSyncAccount())
	require.NoError(t, err)
	require.Equal(t, []string{"claude-opus-4-8-thinking", "gpt-5-high"}, catalog.Models)
	require.Equal(t, 1, repo.writeCalls())

	mapping := repo.writtenMapping()
	require.Equal(t, "claude-opus-4-8-thinking", mapping["claude-opus-4-8"])
	require.Equal(t, "manually-frozen-value", mapping["gpt-5"])
}

// ---------------------------------------------------------------------------
// 分派：SyncUpstreamModelCatalog / FetchUpstreamSupportedModels 命中 cursor 分支
// ---------------------------------------------------------------------------

// TestUpstreamModelSyncCursorSkipsOpenAIPath 钉死分派：cursor 账号不会掉进
// OpenAI /v1/models 通用路径（该路径在无 httpUpstream 时报 configuration）。
func TestUpstreamModelSyncCursorSkipsOpenAIPath(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	svc.httpUpstream = nil // 通用路径在这里必炸；cursor 分支不应依赖它
	installCursorModelSyncSource(t, &cursorModelSyncSourceStub{entries: cursorModelSyncEntries()})

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), cursorModelSyncAccount())
	require.NoError(t, err)
	require.Len(t, catalog.Models, 2)
	require.Equal(t, 1, repo.writeCalls())
}

// TestUpstreamModelSyncCursorFetchSupportedModels 钉死 FetchUpstreamSupportedModels
// 也走 GetUsableModels（同一份折叠口径），且不落库。
func TestUpstreamModelSyncCursorFetchSupportedModels(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	svc.httpUpstream = nil
	installCursorModelSyncSource(t, &cursorModelSyncSourceStub{entries: cursorModelSyncEntries()})

	models, err := svc.FetchUpstreamSupportedModels(context.Background(), cursorModelSyncAccount())
	require.NoError(t, err)
	require.Equal(t, []string{"claude-opus-4-8-thinking", "gpt-5-high"}, models)
	require.Equal(t, 0, repo.writeCalls(), "fetch-only path must never persist")
}

// TestUpstreamModelSyncCursorPropagatesSourceError 钉死 FetchUpstreamSupportedModels 的错误透传。
func TestUpstreamModelSyncCursorPropagatesSourceError(t *testing.T) {
	svc, _ := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorModelSyncSourceStub{err: errors.New("upstream unreachable")})

	models, err := svc.FetchUpstreamSupportedModels(context.Background(), cursorModelSyncAccount())
	require.Nil(t, models)
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorUpstream)
}

// TestCursorModelSyncNoBackgroundTimer 防止回归：本卡不得引入任何 ticker/定时器，
// 目录来源只在手动入口被调用一次即止。
func TestCursorModelSyncNoBackgroundTimer(t *testing.T) {
	svc, _ := cursorModelSyncFixture(t)
	stub := &cursorModelSyncSourceStub{entries: cursorModelSyncEntries()}
	installCursorModelSyncSource(t, stub)

	_, err := svc.SyncUpstreamModelCatalog(context.Background(), cursorModelSyncAccount())
	require.NoError(t, err)
	require.Equal(t, 1, stub.calls, "catalog source must be invoked exactly once per manual sync")

	source, readErr := os.ReadFile("cursor_model_sync.go")
	require.NoError(t, readErr, "cursor_model_sync.go must be readable for the no-timer guard")
	require.NotContains(t, string(source), "Ticker", "sync layer must not introduce timers")
	require.NotContains(t, string(source), "time.AfterFunc", "sync layer must not introduce timers")
}
