//go:build unit

package service

// cursor_model_sync_test.go 是 cursor_model_sync.go 的配对单测（P1-B1）。
//
// 覆盖：
//   1. 目录来源换成 aiserver.v1.AiService/AvailableModels 后的成功/失败/原子写分支；
//   2. 基础身份唯一取 f1 name → f18 server_model_name；展示名不作身份；
//   3. variants/legacy_slugs/id_aliases 不扁平化（参数化档位不是无参数基础名）；
//   4. wire 类型严格（bool 收到 string、string 收到 varint 一律解析失败，零写）；
//   5. 真实回包脱敏 fixture（48 条基础项）：整份可解析、跨 base 重复别名不拒、
//      档位不产生额外映射键；
//   6. 只补缺键：人工键/人工空值保持，幂等不写，preview 不写。

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// 测试替身
// ---------------------------------------------------------------------------

// cursorModelSyncRepoStub 记录凭据写链调用：原子性靠「调用次数」与「落库内容」双断言。
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

func (r *cursorModelSyncRepoStub) resetCalls() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = 0
}

func (r *cursorModelSyncRepoStub) writtenMapping() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	raw, _ := r.snapshot["model_mapping"].(map[string]any)
	return raw
}

// cursorCatalogSourceStub 是 cursorCatalogSource 的内存替身。
type cursorCatalogSourceStub struct {
	mu      sync.Mutex
	entries []cursorCatalogModel
	err     error
	calls   int
}

func (s *cursorCatalogSourceStub) CursorCatalog(_ context.Context, _ *Account) ([]cursorCatalogModel, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.entries, s.err
}

// installCursorModelSyncSource 安装替身并在测试结束时还原（包级位点，不可并行）。
func installCursorModelSyncSource(t *testing.T, src cursorCatalogSource) *cursorCatalogSourceStub {
	t.Helper()
	stub, ok := src.(*cursorCatalogSourceStub)
	if ok {
		InstallCursorCatalogSource(stub)
		t.Cleanup(func() { InstallCursorCatalogSource(nil) })
		return stub
	}
	InstallCursorCatalogSource(src)
	t.Cleanup(func() { InstallCursorCatalogSource(nil) })
	return nil
}

// cursorModelSyncUpstreamStub 是 HTTPUpstream 替身：记录出站请求并按 handler 回包。
type cursorModelSyncUpstreamStub struct {
	mu       sync.Mutex
	reqPath  string
	headers  http.Header
	body     []byte
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
	if req.Body != nil {
		u.body, _ = io.ReadAll(req.Body)
	}
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

func (u *cursorModelSyncUpstreamStub) requestBodyHex() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return hex.EncodeToString(u.body)
}

// ---------------------------------------------------------------------------
// 目录报文构造（按 AvailableModels schema 的 wire 形状）
// ---------------------------------------------------------------------------

// cursorFixtureVariant 是**测试侧**的档位夹具：一个 variant 可带多个参数值，
// 而生产类型 cursorCatalogModel.VariantParameterValues 只承载解码后的 (id, value) 对。
type cursorFixtureVariant struct {
	LegacySlug string
	Parameters [][2]string
}

// cursorFixtureModel 是夹具侧的模型形状：基础身份 + 需要编进 wire 的档位。
type cursorFixtureModel struct {
	cursorCatalogModel
	Variants []cursorFixtureVariant
}

// encodeCursorFixtureResponse 按 AvailableModelsResponse 的 wire 形状编码夹具。
// 字段号取自 cursor_model_sync.go 的常量，避免测试与实现漂移。
func encodeCursorFixtureResponse(fixtures []cursorFixtureModel) []byte {
	var out []byte
	for _, fixture := range fixtures {
		entry := fixture.cursorCatalogModel
		var item []byte
		item = cursorAppendStringField(item, cursorFldAvailModelName, entry.Name)
		item = cursorAppendStringField(item, cursorFldAvailModelClientDisplayName, entry.ClientDisplayName)
		item = cursorAppendStringField(item, cursorFldAvailModelServerModelName, entry.ServerModelName)
		if entry.SupportsAgent {
			item = cursorAppendVarintField(item, cursorFldAvailModelSupportsAgent, 1)
		}
		if entry.SupportsNonMaxMode {
			item = cursorAppendVarintField(item, cursorFldAvailModelSupportsNonMaxMode, 1)
		}
		if entry.OnlySupportsCmdK {
			item = cursorAppendVarintField(item, cursorFldAvailModelOnlySupportsCmdK, 1)
		}
		if entry.IsLongContextOnly {
			item = cursorAppendVarintField(item, cursorFldAvailModelLongContextOnly, 1)
		}
		if entry.IsHidden {
			item = cursorAppendVarintField(item, cursorFldAvailModelIsHidden, 1)
		}
		for _, slug := range entry.LegacySlugs {
			item = cursorAppendStringField(item, cursorFldAvailModelLegacySlugs, slug)
		}
		for _, alias := range entry.IDAliases {
			item = cursorAppendStringField(item, cursorFldAvailModelIDAliases, alias)
		}
		for _, variant := range fixture.Variants {
			// f11 legacy_slug
			v := cursorAppendStringField(nil, cursorFldVariantLegacySlug, variant.LegacySlug)
			// f1 repeated parameter_values，每项 f1=id / f2=value
			for _, param := range variant.Parameters {
				var value []byte
				value = cursorAppendStringField(value, cursorFldParameterValueID, param[0])
				value = cursorAppendStringField(value, cursorFldParameterValueValue, param[1])
				v = cursorAppendBytesField(v, cursorFldVariantParameterValues, value)
			}
			// f30 repeated variants
			item = cursorAppendBytesField(item, cursorFldAvailModelVariants, v)
		}
		out = cursorAppendBytesField(out, cursorFldAvailModelsResponseModels, item)
	}
	return out
}

// encodeCursorAvailableModelsResponse 是只含基础字段的便捷封装（不编档位）。
func encodeCursorAvailableModelsResponse(entries []cursorCatalogModel) []byte {
	fixtures := make([]cursorFixtureModel, 0, len(entries))
	for _, entry := range entries {
		fixtures = append(fixtures, cursorFixtureModel{cursorCatalogModel: entry})
	}
	return encodeCursorFixtureResponse(fixtures)
}

// cursorModelSyncFixture 造一套「已注入替身来源 + 记账 repo」的同步夹具。
func cursorModelSyncFixture(t *testing.T) (*AccountTestService, *cursorModelSyncRepoStub) {
	t.Helper()
	repo := &cursorModelSyncRepoStub{}
	svc := NewAccountTestService(repo, nil, nil, nil, nil, nil, upstreamModelSyncTestConfig(), nil)
	return svc, repo
}

// cursorModelSyncTestBearer 是测试用的账号级 access_token 明文（**测试假值**，非真实凭证）。
// 默认来源走真实出站，必须带用户级 Bearer（与对话链路同款维度），故夹具账号预置它。
const cursorModelSyncTestBearer = "TEST-ACCOUNT-BEARER"

// cursorModelSyncAccount 是一条带既有映射的 cursor 账号：既有键用于断言「只补缺键」。
func cursorModelSyncAccount() *Account {
	return &Account{
		ID:         88,
		Platform:   PlatformCursor,
		QuotaKeyID: qkID(7),
		Credentials: map[string]any{
			"access_token": cursorModelSyncTestBearer, // 用户级 Bearer（账号已存 access_token）
			"model_mapping": map[string]any{
				"cursor-sonnet": "legacy-manual-target",  // 存量人工映射
				"gpt-5":         "manually-frozen-value", // 存量人工限制，不得被批量放开
				"cursor-locked": "",                      // 人工置空的键：存在即保留，不得补值
			},
		},
	}
}

// cursorModelSyncEntries 是最小基础目录：两条基础模型，second 的 server 名与 name 不同
// （证明写的是 name→server 而非 name→name），first 带一个参数化档位与别名。
func cursorModelSyncEntries() []cursorCatalogModel {
	return []cursorCatalogModel{
		{
			Name:               "claude-opus-4-8",
			ServerModelName:    "claude-opus-4-8",
			ClientDisplayName:  "Claude Opus 4.8",
			SupportsAgent:      true,
			SupportsNonMaxMode: true,
			LegacySlugs:        []string{"claude-opus-4-8", "claude-opus-4-8-thinking"},
		},
		{
			Name:              "gpt-5.6-sol",
			ServerModelName:   "gpt-5.6-sol-codex",
			ClientDisplayName: "GPT-5.6",
			SupportsAgent:     true,
			IDAliases:         []string{"gpt", "codex"},
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
// 真实回包脱敏 fixture
// ---------------------------------------------------------------------------

// cursorCatalogFixtureDoc 是 testdata/cursor_available_models.json 的形状。
type cursorCatalogFixtureDoc struct {
	ModelCount int                    `json:"model_count"`
	Models     []cursorCatalogFixture `json:"models"`
}

type cursorCatalogFixture struct {
	Name               string                 `json:"name"`
	ClientDisplayName  string                 `json:"client_display_name"`
	ServerModelName    string                 `json:"server_model_name"`
	SupportsAgent      bool                   `json:"supports_agent"`
	SupportsNonMaxMode bool                   `json:"supports_non_max_mode"`
	OnlySupportsCmdK   bool                   `json:"only_supports_cmd_k"`
	IsLongContextOnly  bool                   `json:"is_long_context_only"`
	IsHidden           bool                   `json:"is_hidden"`
	LegacySlugs        []string               `json:"legacy_slugs"`
	IDAliases          []string               `json:"id_aliases"`
	Variants           []cursorVariantFixture `json:"variants"`
}

type cursorVariantFixture struct {
	LegacySlug      string      `json:"legacy_slug"`
	ParameterValues [][2]string `json:"parameter_values"`
	IsMaxMode       bool        `json:"is_max_mode"`
}

// loadCursorAvailableModelsFixture 读取真实回包脱敏 fixture 并编成 wire 报文。
func loadCursorAvailableModelsFixture(t *testing.T) (cursorCatalogFixtureDoc, []byte) {
	t.Helper()
	raw, err := os.ReadFile("testdata/cursor_available_models.json")
	require.NoError(t, err, "real AvailableModels fixture must be present")
	var doc cursorCatalogFixtureDoc
	require.NoError(t, json.Unmarshal(raw, &doc))
	require.Equal(t, doc.ModelCount, len(doc.Models))
	require.NotEmpty(t, doc.Models)

	fixtures := make([]cursorFixtureModel, 0, len(doc.Models))
	for _, m := range doc.Models {
		fixture := cursorFixtureModel{cursorCatalogModel: cursorCatalogModel{
			Name:               m.Name,
			ServerModelName:    m.ServerModelName,
			ClientDisplayName:  m.ClientDisplayName,
			SupportsAgent:      m.SupportsAgent,
			SupportsNonMaxMode: m.SupportsNonMaxMode,
			OnlySupportsCmdK:   m.OnlySupportsCmdK,
			IsLongContextOnly:  m.IsLongContextOnly,
			IsHidden:           m.IsHidden,
			LegacySlugs:        append([]string(nil), m.LegacySlugs...),
			IDAliases:          append([]string(nil), m.IDAliases...),
		}}
		for _, v := range m.Variants {
			fixture.Variants = append(fixture.Variants, cursorFixtureVariant{
				LegacySlug: v.LegacySlug,
				Parameters: append([][2]string(nil), v.ParameterValues...),
			})
		}
		fixtures = append(fixtures, fixture)
	}
	return doc, encodeCursorFixtureResponse(fixtures)
}

// ---------------------------------------------------------------------------
// 成功分支：基础身份 + 原子写 mapping
// ---------------------------------------------------------------------------

// TestCursorModelSyncSuccessWritesMappingOnce 钉死成功路径：只返回**基础 name** 清单，
// 并恰好一次落库；mapping 只补缺键，既有人工映射（含人工空值）原样保留。
func TestCursorModelSyncSuccessWritesMappingOnce(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorCatalogSourceStub{entries: cursorModelSyncEntries()})
	account := cursorModelSyncAccount()

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.NotNil(t, catalog)

	// 对外模型 = 基础 name（排序），不是展示名、不是档位名。
	require.Equal(t, []string{"claude-opus-4-8", "gpt-5.6-sol"}, catalog.Models)

	// 原子写 = 一次 UpdateCredentials，不多不少。
	require.Equal(t, 1, repo.writeCalls())
	require.EqualValues(t, account.ID, repo.id)

	mapping := repo.writtenMapping()
	require.NotNil(t, mapping, "model_mapping must be persisted through the credential write chain")

	// 基础身份 name → server_model_name 必须写：转发侧闸门要求显式命中才出站。
	require.Equal(t, "claude-opus-4-8", mapping["claude-opus-4-8"])
	require.Equal(t, "gpt-5.6-sol-codex", mapping["gpt-5.6-sol"], "name maps to server_model_name, not to itself")

	// 档位名/别名不得成为映射键。
	require.NotContains(t, mapping, "claude-opus-4-8-thinking", "legacy_slugs must not become mapping keys")
	require.NotContains(t, mapping, "claude-opus-4-8-high", "a parameterized tier must not be mapped to a parameter-free base")
	require.NotContains(t, mapping, "codex", "id_aliases must not become mapping keys")
	require.NotContains(t, mapping, "gpt")

	// 不自动恢复运营停用状态 / 不批量放开存量人工限制：既有键一律不改写。
	require.Equal(t, "legacy-manual-target", mapping["cursor-sonnet"])
	require.Equal(t, "manually-frozen-value", mapping["gpt-5"],
		"existing manual mapping value must never be overwritten by sync")
	require.Contains(t, mapping, "cursor-locked", "an existing key with an empty value must survive")
	require.Equal(t, "", mapping["cursor-locked"], "an empty manual value must not be filled in")
}

// TestCursorModelSyncPunctuationIsNotNormalized 钉死不做标点相似归一：
// `claude-fable-5.1` 与 `claude-fable-5-1` 是两个不同标识，不能互相映射。
func TestCursorModelSyncPunctuationIsNotNormalized(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorCatalogSourceStub{entries: []cursorCatalogModel{
		{Name: "claude-fable-5.1", ServerModelName: "claude-fable-5.1"},
		{Name: "claude-fable-5-1", ServerModelName: "claude-fable-5-1"},
	}})

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), cursorModelSyncAccount())
	require.NoError(t, err)
	require.Equal(t, []string{"claude-fable-5-1", "claude-fable-5.1"}, catalog.Models)

	mapping := repo.writtenMapping()
	require.Equal(t, "claude-fable-5.1", mapping["claude-fable-5.1"])
	require.Equal(t, "claude-fable-5-1", mapping["claude-fable-5-1"])
}

// TestCursorModelSyncSuccessIsIdempotent 钉死幂等：目录无新增键时不发起写链调用。
func TestCursorModelSyncSuccessIsIdempotent(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorCatalogSourceStub{entries: []cursorCatalogModel{
		{Name: "cursor-small", ServerModelName: "cursor-small"},
	}})
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
	installCursorModelSyncSource(t, &cursorCatalogSourceStub{entries: cursorModelSyncEntries()})

	preview := cursorModelSyncAccount()
	preview.ID = 0
	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), preview)
	require.NoError(t, err)
	require.Len(t, catalog.Models, 2)
	require.Equal(t, 0, repo.writeCalls())
}

// ---------------------------------------------------------------------------
// 失败分支：不落半份
// ---------------------------------------------------------------------------

// TestCursorModelSyncUpstreamFailureWritesNothing 钉死「失败不落半份」。
func TestCursorModelSyncUpstreamFailureWritesNothing(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorCatalogSourceStub{err: errors.New("dial tcp: upstream unreachable")})
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
	installCursorModelSyncSource(t, &cursorCatalogSourceStub{entries: nil})
	account := cursorModelSyncAccount()

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.Nil(t, catalog)
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorUpstream)
	require.Equal(t, 0, repo.writeCalls())
	require.NotNil(t, account.Credentials["model_mapping"], "existing mapping must survive an empty catalog")
}

// TestCursorModelSyncMissingBaseIdentityErrors 钉死缺基础身份（name 或 server 为空）
// 整体失败：既不跳过脏项当成功，也不写半份。
func TestCursorModelSyncMissingBaseIdentityErrors(t *testing.T) {
	cases := map[string][]byte{
		"missing server_model_name": encodeCursorAvailableModelsResponse([]cursorCatalogModel{
			{Name: "cursor-small", ClientDisplayName: "Small"},
		}),
		"missing name": encodeCursorAvailableModelsResponse([]cursorCatalogModel{
			{ServerModelName: "cursor-small"},
		}),
		"blank name": encodeCursorAvailableModelsResponse([]cursorCatalogModel{
			{Name: "   ", ServerModelName: "cursor-small"},
		}),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseCursorAvailableModelsResponse(body)
			require.Error(t, err)
			require.ErrorContains(t, err, "base identity")
		})
	}
}

// TestCursorModelSyncMissingBaseIdentityWritesNothing 钉死同一失败经**同步入口**时的零写语义：
// 一条缺基础身份的脏项让整份目录作废，已有 mapping 原样保留。
func TestCursorModelSyncMissingBaseIdentityWritesNothing(t *testing.T) {
	body := encodeCursorAvailableModelsResponse([]cursorCatalogModel{
		{Name: "healthy-model", ServerModelName: "healthy-model"},
		{Name: "broken-model", ServerModelName: ""},
	})
	svc, repo := cursorModelSyncFixture(t)
	src, _ := newCursorModelSyncUpstreamFixture(t, svc, "AUTH", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", cursorUnaryContentType)
		_, _ = w.Write(body)
	})
	installCursorModelSyncSource(t, src)

	account := cursorModelSyncAccount()
	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.Nil(t, catalog)
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorUpstream)
	require.Equal(t, 0, repo.writeCalls(), "a partially usable catalog must never write half a mapping")
	require.NotNil(t, account.Credentials["model_mapping"])
}

// TestCursorModelSyncConflictingServerNameErrors 钉死同名不同 server 是身份冲突（整体失败）。
func TestCursorModelSyncConflictingServerNameErrors(t *testing.T) {
	body := encodeCursorAvailableModelsResponse([]cursorCatalogModel{
		{Name: "gpt-5", ServerModelName: "gpt-5-a"},
		{Name: "gpt-5", ServerModelName: "gpt-5-b"},
	})
	_, err := parseCursorAvailableModelsResponse(body)
	require.Error(t, err)
	require.ErrorContains(t, err, "conflicting server model names")
}

// TestCursorModelSyncMalformedBodyErrors 钉死解析失败关闭：非法/截断 protobuf 不当成功。
func TestCursorModelSyncMalformedBodyErrors(t *testing.T) {
	valid := encodeCursorAvailableModelsResponse([]cursorCatalogModel{
		{Name: "cursor-small", ServerModelName: "cursor-small"},
	})
	cases := map[string][]byte{
		"empty":                {},
		"garbage":              []byte("\xff\xff\xff\xff\xff"),
		"truncated response":   valid[:len(valid)-1],
		"models not a message": cursorAppendVarintField(nil, cursorFldAvailModelsResponseModels, 1),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseCursorAvailableModelsResponse(payload)
			require.Error(t, err)
		})
	}
}

// TestCursorModelSyncWireTypeIsStrict 钉死已知字段的 wire 类型必须与 schema 一致：
// bool 收到 string、string 收到 varint 一律解析失败（不「尽力而为」地忽略）。
func TestCursorModelSyncWireTypeIsStrict(t *testing.T) {
	t.Run("bool field carries string", func(t *testing.T) {
		var item []byte
		item = cursorAppendStringField(item, cursorFldAvailModelName, "cursor-small")
		item = cursorAppendStringField(item, cursorFldAvailModelServerModelName, "cursor-small")
		item = cursorAppendBytesField(item, cursorFldAvailModelSupportsAgent, []byte("true"))
		_, err := parseCursorAvailableModelsResponse(cursorAppendBytesField(nil, cursorFldAvailModelsResponseModels, item))
		require.Error(t, err)
		require.ErrorContains(t, err, "supports_agent")
	})
	t.Run("string field carries varint", func(t *testing.T) {
		var item []byte
		item = cursorAppendStringField(item, cursorFldAvailModelName, "cursor-small")
		item = cursorAppendVarintField(item, cursorFldAvailModelServerModelName, 7)
		_, err := parseCursorAvailableModelsResponse(cursorAppendBytesField(nil, cursorFldAvailModelsResponseModels, item))
		require.Error(t, err)
		require.ErrorContains(t, err, "server_model_name")
	})
	t.Run("unknown field is skipped", func(t *testing.T) {
		var item []byte
		item = cursorAppendStringField(item, cursorFldAvailModelName, "cursor-small")
		item = cursorAppendStringField(item, cursorFldAvailModelServerModelName, "cursor-small")
		item = cursorAppendStringField(item, 999, "an unknown future field") // 上游加字段不得让本站整体失败
		entries, err := parseCursorAvailableModelsResponse(cursorAppendBytesField(nil, cursorFldAvailModelsResponseModels, item))
		require.NoError(t, err)
		require.Len(t, entries, 1)
	})
}

// TestCursorModelSyncPersistenceFailureIsInternal 钉死落库失败：归类 internal，不外泄内部错误。
func TestCursorModelSyncPersistenceFailureIsInternal(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorCatalogSourceStub{entries: cursorModelSyncEntries()})
	repo.err = errors.New("database is down")

	_, err := svc.SyncUpstreamModelCatalog(context.Background(), cursorModelSyncAccount())
	syncErr := requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorInternal)
	require.Contains(t, syncErr.Message, "Failed to save cursor model mapping")
	require.NotEmpty(t, syncErr.SafeMessage())
	require.NotContains(t, syncErr.SafeMessage(), "database is down", "internal cause must not reach clients")
}

// ---------------------------------------------------------------------------
// 失败分类
// ---------------------------------------------------------------------------

// TestCursorModelSyncMissingAuthDepsIsConfiguration 钉死依赖缺失 → configuration 类错误。
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
	installCursorModelSyncSource(t, &cursorCatalogSourceStub{entries: cursorModelSyncEntries()})

	_, err := svc.SyncUpstreamModelCatalog(context.Background(), nil)
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorConfiguration)
	require.Equal(t, 0, repo.writeCalls())
}

// TestCursorModelSyncTypedErrorPassthrough 替身来源本身返回的 *UpstreamModelSyncError
// 必须原样透传（不丢具体分类与状态码）。
func TestCursorModelSyncTypedErrorPassthrough(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorCatalogSourceStub{
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
) (*cursorDefaultCatalogSource, *cursorModelSyncUpstreamStub) {
	t.Helper()
	stub := &cursorModelSyncUpstreamStub{handler: handler}
	svc.httpUpstream = stub

	src := newCursorDefaultCatalogSource(
		svc,
		withCursorModelSyncBaseURL(func(context.Context) string { return "https://cursor.example.test" }),
		withCursorModelSyncAuth(func(context.Context, *Account) (CursorUpstreamAuthProvider, error) {
			return staticCursorAuthProvider{value: authValue}, nil
		}),
	)
	return src, stub
}

// TestCursorModelSyncDefaultSourceRoundTrip 钉死 Unary 出站形状 + 目录解析：
// 路径 / 请求体 hex / Content-Type / x-jg-auth 逐项对齐。
func TestCursorModelSyncDefaultSourceRoundTrip(t *testing.T) {
	svc, _ := cursorModelSyncFixture(t)
	body := encodeCursorAvailableModelsResponse([]cursorCatalogModel{
		{Name: "claude-sonnet-4-6", ServerModelName: "claude-sonnet-4-6", ClientDisplayName: "Sonnet 4.6"},
		{Name: "cursor-small", ServerModelName: "cursor-small", ClientDisplayName: "Cursor Small"},
	})
	src, stub := newCursorModelSyncUpstreamFixture(t, svc, "TEST-AUTH-CIPHER", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", cursorUnaryContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})

	entries, err := src.CursorCatalog(context.Background(), cursorModelSyncAccount())
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, "claude-sonnet-4-6", entries[0].Name)
	require.Equal(t, "Sonnet 4.6", entries[0].ClientDisplayName)
	require.Equal(t, "cursor-small", entries[1].ServerModelName)

	// 基础目录走 aiserver.v1 端点（不是 agent.v1 的 GetUsableModels 档位目录）。
	require.Equal(t, "/aiserver.v1.AiService/AvailableModels", stub.path())
	require.Equal(t, cursorModelSyncPathAvailableModels, stub.path())
	// 真实请求体 hex（已向上游跑通，不得自行增删字段）。
	require.Equal(t, cursorModelSyncRequestHex, stub.requestBodyHex())
	// Unary 裸 protobuf：既不能写成 connect+proto（会被上游 415），也不能带 5 字节 envelope。
	require.Equal(t, cursorUnaryContentType, stub.headers.Get("Content-Type"))
	require.Equal(t, "TEST-AUTH-CIPHER", stub.headers.Get(cursorJgAuthHeader))
	// 用户级 Bearer（账号 access_token）：缺它上游 401 拒，必须随站带出。
	require.Equal(t, "Bearer "+cursorModelSyncTestBearer, stub.headers.Get("Authorization"))
	// 客户端版本：值与对话链路常量一致（同款，不另写）。
	require.Equal(t, cursorClientVersion, stub.headers.Get(cursorModelSyncClientVersionHeader))
	// 网络优化头：对齐真实探针 11-base-catalog.cjs。
	require.Equal(t, cursorModelSyncNetOptimizeValue, stub.headers.Get(cursorModelSyncNetOptimizeHeader))
}

// TestCursorModelSyncBoolWireFields 钉死 optional bool 字段按 varint 解析（f3/f5/f19/f27/f35）。
func TestCursorModelSyncBoolWireFields(t *testing.T) {
	entries, err := parseCursorAvailableModelsResponse(encodeCursorAvailableModelsResponse([]cursorCatalogModel{
		{
			Name: "gemini-3-pro", ServerModelName: "gemini-3-pro",
			SupportsAgent: true, SupportsNonMaxMode: true,
			OnlySupportsCmdK: true, IsLongContextOnly: true, IsHidden: true,
		},
		{Name: "gemini-3-flash", ServerModelName: "gemini-3-flash"},
	}))
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.True(t, entries[0].SupportsAgent)
	require.True(t, entries[0].SupportsNonMaxMode)
	require.True(t, entries[0].OnlySupportsCmdK)
	require.True(t, entries[0].IsLongContextOnly)
	require.True(t, entries[0].IsHidden)
	require.False(t, entries[1].SupportsAgent, "absent optional bool decodes as false, not as a string")
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
		_, err := src.CursorCatalog(context.Background(), cursorModelSyncAccount())
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
		_, err := src.CursorCatalog(context.Background(), cursorModelSyncAccount())
		requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorCredential)
	})

	t.Run("500 is upstream", func(t *testing.T) {
		svc, _ := cursorModelSyncFixture(t)
		src, _ := newCursorModelSyncUpstreamFixture(t, svc, secret, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		})
		_, err := src.CursorCatalog(context.Background(), cursorModelSyncAccount())
		syncErr := requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorUpstream)
		require.Equal(t, http.StatusInternalServerError, syncErr.StatusCode)
	})
}

// TestCursorModelSyncAuthFailureIsConfigured 钉死鉴权材料不可得 → 不发请求（不白耗额度）。
func TestCursorModelSyncAuthFailureIsConfigured(t *testing.T) {
	svc, _ := cursorModelSyncFixture(t)
	stub := &cursorModelSyncUpstreamStub{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encodeCursorAvailableModelsResponse([]cursorCatalogModel{{Name: "cursor-small", ServerModelName: "cursor-small"}}))
	}}
	svc.httpUpstream = stub
	src := newCursorDefaultCatalogSource(svc,
		withCursorModelSyncBaseURL(func(context.Context) string { return "https://cursor.example.test" }),
		withCursorModelSyncAuth(func(context.Context, *Account) (CursorUpstreamAuthProvider, error) {
			return nil, errors.New("key material unavailable")
		}),
	)

	_, err := src.CursorCatalog(context.Background(), cursorModelSyncAccount())
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorConfiguration)
	require.Equal(t, 0, stub.requests, "no request must leave the process without auth material")
}

// TestCursorModelSyncMissingBearerFailsClosed 钉死「用户级 Bearer 缺失」失败关闭：
// 设备级 x-jg-auth 材料可得、但账号无 access_token 时，绝不空 Bearer 出站、绝不回落
// "只带 x-jg-auth 试试"——请求次数必须为 0，且归类 configuration 类错误。
func TestCursorModelSyncMissingBearerFailsClosed(t *testing.T) {
	svc, _ := cursorModelSyncFixture(t)
	stub := &cursorModelSyncUpstreamStub{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encodeCursorAvailableModelsResponse([]cursorCatalogModel{{Name: "cursor-small", ServerModelName: "cursor-small"}}))
	}}
	svc.httpUpstream = stub
	src := newCursorDefaultCatalogSource(svc,
		withCursorModelSyncBaseURL(func(context.Context) string { return "https://cursor.example.test" }),
		withCursorModelSyncAuth(func(context.Context, *Account) (CursorUpstreamAuthProvider, error) {
			return staticCursorAuthProvider{value: "AUTH"}, nil
		}),
	)

	account := cursorModelSyncAccount()
	delete(account.Credentials, "access_token") // 账号缺失用户级 Bearer

	_, err := src.CursorCatalog(context.Background(), account)
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorConfiguration)
	require.Equal(t, 0, stub.requests, "no request must leave the process without the account bearer")
}

// TestCursorModelSyncEmptyBaseURLFailsClosed 钉死节点基址缺失 → 不发请求。
func TestCursorModelSyncEmptyBaseURLFailsClosed(t *testing.T) {
	svc, _ := cursorModelSyncFixture(t)
	stub := &cursorModelSyncUpstreamStub{}
	svc.httpUpstream = stub
	src := newCursorDefaultCatalogSource(svc,
		withCursorModelSyncBaseURL(func(context.Context) string { return "  " }),
		withCursorModelSyncAuth(func(context.Context, *Account) (CursorUpstreamAuthProvider, error) {
			return staticCursorAuthProvider{value: "AUTH"}, nil
		}),
	)

	_, err := src.CursorCatalog(context.Background(), cursorModelSyncAccount())
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorConfiguration)
	require.Equal(t, 0, stub.requests)
}

// TestCursorModelSyncMissingHTTPUpstreamIsConfiguration 钉死出站客户端缺失归类 configuration。
func TestCursorModelSyncMissingHTTPUpstreamIsConfiguration(t *testing.T) {
	svc, _ := cursorModelSyncFixture(t)
	require.Nil(t, svc.httpUpstream)
	src := newCursorDefaultCatalogSource(svc,
		withCursorModelSyncBaseURL(func(context.Context) string { return "https://cursor.example.test" }),
		withCursorModelSyncAuth(func(context.Context, *Account) (CursorUpstreamAuthProvider, error) {
			return staticCursorAuthProvider{value: "AUTH"}, nil
		}),
	)

	_, err := src.CursorCatalog(context.Background(), cursorModelSyncAccount())
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorConfiguration)
}

// TestCursorModelSyncDefaultSourceEndToEndWrites 钉死「默认来源 → 适配 → 落库」全链路。
func TestCursorModelSyncDefaultSourceEndToEndWrites(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	src, _ := newCursorModelSyncUpstreamFixture(t, svc, "AUTH", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", cursorUnaryContentType)
		_, _ = w.Write(encodeCursorAvailableModelsResponse(cursorModelSyncEntries()))
	})
	installCursorModelSyncSource(t, src)

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), cursorModelSyncAccount())
	require.NoError(t, err)
	require.Equal(t, []string{"claude-opus-4-8", "gpt-5.6-sol"}, catalog.Models)
	require.Equal(t, 1, repo.writeCalls())

	mapping := repo.writtenMapping()
	require.Equal(t, "claude-opus-4-8", mapping["claude-opus-4-8"])
	require.Equal(t, "gpt-5.6-sol-codex", mapping["gpt-5.6-sol"])
	require.Equal(t, "manually-frozen-value", mapping["gpt-5"], "existing manual key must survive untouched")
	require.NotContains(t, mapping, "claude-opus-4-8-high")
}

// ---------------------------------------------------------------------------
// 真实回包：48 条基础项端到端
// ---------------------------------------------------------------------------

// TestCursorModelSyncRealCatalogParsesAllBaseIdentities 钉死真实回包整份可解析：
// 每条都有非空 name/server，基础身份一对一，且不产生任何档位/别名映射键。
func TestCursorModelSyncRealCatalogParsesAllBaseIdentities(t *testing.T) {
	doc, body := loadCursorAvailableModelsFixture(t)

	entries, err := parseCursorAvailableModelsResponse(body)
	require.NoError(t, err)
	require.Len(t, entries, doc.ModelCount)

	byName := make(map[string]cursorCatalogModel, len(entries))
	for _, e := range entries {
		require.NotEmpty(t, e.Name)
		require.NotEmpty(t, e.ServerModelName)
		byName[e.Name] = e
	}
	require.Len(t, byName, doc.ModelCount, "base names must be unique")

	adapted, err := adaptCursorCatalogModels(entries)
	require.NoError(t, err)
	require.Len(t, adapted.Models, doc.ModelCount)
	require.Len(t, adapted.Mapping, doc.ModelCount, "only base identities become mapping keys")

	for _, e := range entries {
		require.Equal(t, e.ServerModelName, adapted.Mapping[e.Name])
	}
	// 展示名不得出现在映射键里。
	for _, e := range entries {
		if e.ClientDisplayName != "" && e.ClientDisplayName != e.Name {
			require.NotContains(t, adapted.Mapping, e.ClientDisplayName)
		}
	}
}

// TestCursorModelSyncRealCatalogKeepsTiersOutOfMapping 钉死参数化档位不扁平化：
// 真实目录里 variant legacy_slug 带 parameter_values，绝不能落成无参数基础模型的映射键。
func TestCursorModelSyncRealCatalogKeepsTiersOutOfMapping(t *testing.T) {
	doc, body := loadCursorAvailableModelsFixture(t)
	entries, err := parseCursorAvailableModelsResponse(body)
	require.NoError(t, err)

	adapted, err := adaptCursorCatalogModels(entries)
	require.NoError(t, err)

	tierWithParams := 0
	for _, m := range doc.Models {
		for _, v := range m.Variants {
			if len(v.ParameterValues) == 0 {
				continue
			}
			tierWithParams++
			if v.LegacySlug == m.Name {
				// 档位名与基础名同名（如 default）：它就是该基础身份本身的键，不是被
				// 扁平化的产物——参数化档位仍然一个都没被写进来。
				require.Equal(t, m.ServerModelName, adapted.Mapping[v.LegacySlug])
				continue
			}
			require.NotContains(t, adapted.Mapping, v.LegacySlug,
				"a parameterized tier must not be mapped to a parameter-free base model")
		}
	}
	require.Greater(t, tierWithParams, 0, "fixture must actually contain parameterized tiers")

	// legacy_slugs / id_aliases 同样不进映射（跨 base 重复者不做 first-wins）。
	for _, m := range doc.Models {
		for _, slug := range m.LegacySlugs {
			if slug != m.Name {
				require.NotContains(t, adapted.Mapping, slug)
			}
		}
		for _, alias := range m.IDAliases {
			require.NotContains(t, adapted.Mapping, alias)
		}
	}
}

// TestCursorModelSyncRealCatalogCrossBaseAliasDuplicationIsLegal 钉死跨 base 重复别名合法：
// 真实目录里 `opus`、`gpt` 等被多个 base 同时声明，不据此拒绝整份目录。
func TestCursorModelSyncRealCatalogCrossBaseAliasDuplicationIsLegal(t *testing.T) {
	doc, body := loadCursorAvailableModelsFixture(t)
	entries, err := parseCursorAvailableModelsResponse(body)
	require.NoError(t, err)

	aliasOwners := make(map[string]int)
	for _, e := range entries {
		for _, alias := range e.IDAliases {
			aliasOwners[alias]++
		}
	}
	shared := 0
	for _, owners := range aliasOwners {
		if owners > 1 {
			shared++
		}
	}
	require.Greater(t, shared, 0, "fixture must contain cross-base duplicated aliases")

	adapted, err := adaptCursorCatalogModels(entries)
	require.NoError(t, err)
	require.Len(t, adapted.Models, doc.ModelCount)
}

// TestCursorModelSyncRealCatalogEndToEndWrites 钉死真实 48 条经同步入口落库：
// 只新增基础键，人工键与人工空值保持，一次写入；用落库结果再同步一次零写。
func TestCursorModelSyncRealCatalogEndToEndWrites(t *testing.T) {
	doc, body := loadCursorAvailableModelsFixture(t)
	svc, repo := cursorModelSyncFixture(t)
	src, stub := newCursorModelSyncUpstreamFixture(t, svc, "AUTH", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", cursorUnaryContentType)
		_, _ = w.Write(body)
	})
	installCursorModelSyncSource(t, src)

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), cursorModelSyncAccount())
	require.NoError(t, err)
	require.Len(t, catalog.Models, doc.ModelCount)
	require.Equal(t, 1, repo.writeCalls())
	require.Equal(t, cursorModelSyncPathAvailableModels, stub.path())

	mapping := repo.writtenMapping()
	for _, m := range doc.Models {
		require.Equal(t, m.ServerModelName, mapping[m.Name])
	}
	require.Equal(t, "legacy-manual-target", mapping["cursor-sonnet"])
	require.Equal(t, "manually-frozen-value", mapping["gpt-5"])
	require.Equal(t, "", mapping["cursor-locked"])

	// 幂等：把落库结果当作账号现有 mapping 再同步一次，必须零写。
	repo.resetCalls()
	account := cursorModelSyncAccount()
	persisted := make(map[string]any, len(mapping))
	for k, v := range mapping {
		persisted[k] = v
	}
	account.Credentials["model_mapping"] = persisted

	second, err := svc.SyncUpstreamModelCatalog(context.Background(), account)
	require.NoError(t, err)
	require.Len(t, second.Models, doc.ModelCount)
	require.Equal(t, 0, repo.writeCalls(), "a repeated sync with no new key must not write again")
}

// ---------------------------------------------------------------------------
// 分派：SyncUpstreamModelCatalog / FetchUpstreamSupportedModels 命中 cursor 分支
// ---------------------------------------------------------------------------

// TestUpstreamModelSyncCursorSkipsOpenAIPath 钉死分派：cursor 账号不会掉进
// OpenAI /v1/models 通用路径（该路径在无 httpUpstream 时报 configuration）。
func TestUpstreamModelSyncCursorSkipsOpenAIPath(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	svc.httpUpstream = nil // 通用路径在这里必炸；cursor 分支不应依赖它
	installCursorModelSyncSource(t, &cursorCatalogSourceStub{entries: cursorModelSyncEntries()})

	catalog, err := svc.SyncUpstreamModelCatalog(context.Background(), cursorModelSyncAccount())
	require.NoError(t, err)
	require.Len(t, catalog.Models, 2)
	require.Equal(t, 1, repo.writeCalls())
}

// TestUpstreamModelSyncCursorFetchSupportedModels 钉死 FetchUpstreamSupportedModels
// 与同步入口同源（同一份基础目录），且不落库。
func TestUpstreamModelSyncCursorFetchSupportedModels(t *testing.T) {
	svc, repo := cursorModelSyncFixture(t)
	svc.httpUpstream = nil
	installCursorModelSyncSource(t, &cursorCatalogSourceStub{entries: cursorModelSyncEntries()})

	models, err := svc.FetchUpstreamSupportedModels(context.Background(), cursorModelSyncAccount())
	require.NoError(t, err)
	require.Equal(t, []string{"claude-opus-4-8", "gpt-5.6-sol"}, models)
	require.Equal(t, 0, repo.writeCalls(), "fetch-only path must never persist")
}

// TestUpstreamModelSyncCursorPropagatesSourceError 钉死 FetchUpstreamSupportedModels 的错误透传。
func TestUpstreamModelSyncCursorPropagatesSourceError(t *testing.T) {
	svc, _ := cursorModelSyncFixture(t)
	installCursorModelSyncSource(t, &cursorCatalogSourceStub{err: errors.New("upstream unreachable")})

	models, err := svc.FetchUpstreamSupportedModels(context.Background(), cursorModelSyncAccount())
	require.Nil(t, models)
	requireUpstreamModelSyncError(t, err, UpstreamModelSyncErrorUpstream)
}

// TestCursorModelSyncNoBackgroundTimer 防止回归：本卡不得引入任何 ticker/定时器，
// 目录来源只在手动入口被调用一次即止。
func TestCursorModelSyncNoBackgroundTimer(t *testing.T) {
	svc, _ := cursorModelSyncFixture(t)
	stub := &cursorCatalogSourceStub{entries: cursorModelSyncEntries()}
	installCursorModelSyncSource(t, stub)

	_, err := svc.SyncUpstreamModelCatalog(context.Background(), cursorModelSyncAccount())
	require.NoError(t, err)
	require.Equal(t, 1, stub.calls, "catalog source must be invoked exactly once per manual sync")

	source, readErr := os.ReadFile("cursor_model_sync.go")
	require.NoError(t, readErr, "cursor_model_sync.go must be readable for the no-timer guard")
	require.NotContains(t, string(source), "Ticker", "sync layer must not introduce timers")
	require.NotContains(t, string(source), "time.AfterFunc", "sync layer must not introduce timers")
}

// TestCursorModelSyncNoLegacyCatalogChain 旧链归零护栏（§18.3 第 7 条）：
// 生产**代码**不得再残留 GetUsableModels 承担基础目录同步的入口、假折叠或别名 first-wins。
//
// 只扫代码行：文件头注释按契约要求保留这段历史取证（"为什么不再用它"），那是文档
// 而不是职责，扫进去会让正确的说明文字变成违规证据。
func TestCursorModelSyncNoLegacyCatalogChain(t *testing.T) {
	source, err := os.ReadFile("cursor_model_sync.go")
	require.NoError(t, err)

	var codeLines []string
	for _, line := range strings.Split(string(source), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		codeLines = append(codeLines, line)
	}
	code := strings.Join(codeLines, "\n")

	require.NotContains(t, code, "GetUsableModels",
		"the basic catalog must no longer be sourced from GetUsableModels")
	require.NotContains(t, code, "CollapsedTierRows",
		"wire-ID collapsing produced zero duplicates and can never yield a basic catalog")
	require.NotContains(t, code, "cursorUsableModel",
		"the usable-models tier entry type must be gone")
}
