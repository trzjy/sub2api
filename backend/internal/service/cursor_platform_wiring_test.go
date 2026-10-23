package service

// cursor 平台接线（E2 卡）的单测。覆盖三组断言：
//  1. 平台分支接线：归一化保留 cursor 原值、IsOpenAICompatible、GetOpenAIBaseURL。
//  2. 调度可达性：cursor 账号在分组候选查询里能被 PlatformIn(cursor) 选中（登记进
//     domain 平台清单后 schedulerSnapshotPlatforms 自动含 cursor，故有桶可入）。
//  3. 鉴权材料按账号解析：quota_key_id → 解密 → 材料，且不同账号互不串号。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	wasmcrypto "github.com/Wei-Shaw/sub2api/internal/service/wasm"
	"github.com/stretchr/testify/require"
)

// NewCursorTestEncryptor 返回明文 passthrough 的 QuotaKeyEncryptor 测试替身
// （CodeEncrypted 即明文激活码，与 cursor_upstream_client_test.go 同款口径）。
func NewCursorTestEncryptor() QuotaKeyEncryptor { return cursorTestEncryptor{} }

type cursorTestEncryptor struct{}

func (cursorTestEncryptor) Encrypt(plain []byte) ([]byte, error) { return plain, nil }

func (cursorTestEncryptor) Decrypt(cipher []byte) ([]byte, error) { return cipher, nil }

// ---------------------------------------------------------------------------
// 1. 平台分支接线
// ---------------------------------------------------------------------------

// TestNormalizeOpenAICompatiblePlatformKeepsCursor 钉死归一化保留 cursor 原值。
// 漏加会让 cursor 被归一成 openai，进而 platform_mismatch 永远选不中 cursor 号
// （这正是生产 D2 断点之一）。
func TestNormalizeOpenAICompatiblePlatformKeepsCursor(t *testing.T) {
	require.Equal(t, PlatformCursor, NormalizeOpenAICompatiblePlatform(PlatformCursor))
	// 未登记前会回落 openai——本断言同时证明 cursor 已进白名单而非恰好同名。
	require.NotEqual(t, PlatformOpenAI, NormalizeOpenAICompatiblePlatform(PlatformCursor))

	// 既有平台归一化行为逐位不变（回归护栏）。
	for _, p := range []string{
		PlatformGrok, PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax,
		PlatformCodeBuddy, PlatformOther, PlatformMuse,
	} {
		require.Equal(t, p, NormalizeOpenAICompatiblePlatform(p), p)
	}
	require.Equal(t, PlatformOpenAI, NormalizeOpenAICompatiblePlatform("unknown-platform"))
}

// TestCursorAccountIsOpenAICompatible 钉死 IsOpenAICompatible 放行 cursor。
func TestCursorAccountIsOpenAICompatible(t *testing.T) {
	require.True(t, (&Account{Platform: PlatformCursor}).IsOpenAICompatible())
	// 既有平台判定不变。
	require.True(t, (&Account{Platform: PlatformCodeBuddy}).IsOpenAICompatible())
	require.True(t, (&Account{Platform: PlatformMuse}).IsOpenAICompatible())
	require.False(t, (&Account{Platform: PlatformAnthropic}).IsOpenAICompatible())
}

// TestGetOpenAIBaseURLCursor 钉死 base 取自节点发现而非写死域名。
func TestGetOpenAIBaseURLCursor(t *testing.T) {
	acc := &Account{Platform: PlatformCursor}
	got := acc.GetOpenAIBaseURL()
	require.NotEmpty(t, got, "cursor base url must resolve to a node")
	// 节点可由上游动态下发，故只断言它是一个 http(s) base，不钉死具体域名。
	require.Contains(t, got, "://")
	// 不得回落官方 OpenAI 端点（那会把 cursor 凭证发到 OpenAI）。
	require.NotEqual(t, "https://api.openai.com", got)
}

// TestCursorEmptyModelMappingFailsClosed 钉死空 model_mapping 失败关闭。
// cursor 出站是 Cursor 原生端点，放行任意模型名必然被上游拒绝并白耗 shareai 额度。
func TestCursorEmptyModelMappingFailsClosed(t *testing.T) {
	acc := &Account{Platform: PlatformCursor}
	require.False(t, acc.IsModelSupported("claude-sonnet-4-5"),
		"cursor with empty model_mapping must serve no model (fail closed)")
}

// TestCursorPlatformRegisteredInDomain 钉死 cursor 已登记进 domain 平台清单。
// 这是 cursor 能建分组（IsGroupPlatform）、能开 target_platform=cursor 的 composite
// 路由（IsConcretePlatform）、能被调度快照建桶（schedulerSnapshotPlatforms 派生自
// CompositePrecedencePlatformIDs）的共同前提。
func TestCursorPlatformRegisteredInDomain(t *testing.T) {
	require.True(t, domain.IsConcretePlatform(PlatformCursor))
	require.True(t, domain.IsGroupPlatform(PlatformCursor))
	require.Contains(t, domain.CompositePrecedencePlatformIDs(), PlatformCursor)

	// 调度快照平台集合派生自平台清单 → cursor 必有桶（否则账号永不被选中）。
	require.Contains(t, schedulerSnapshotPlatforms(), PlatformCursor)

	spec, ok := domain.LookupPlatform(PlatformCursor)
	require.True(t, ok)
	require.Equal(t, domain.PlatformGatewayOpenAI, spec.Gateway)
	require.False(t, spec.CNProvider, "cursor 不是国产单一厂商")
}

// ---------------------------------------------------------------------------
// 2. 调度可达性
// ---------------------------------------------------------------------------

// TestCursorAccountsSelectableByPlatformFilter 证明登记后 cursor 账号进入分组候选查询
// 的 PlatformIn 范围：queryAccountsByGroup 以 opts.platforms 做 PlatformIn 过滤，
// cursor 出现在平台集合里即意味着绑定该分组的 cursor 号可被选中。
//
// 这是对 D2（cursor 号永不被调度选中）的直接反证测试：未登记前 cursor 不在任何
// 平台集合中，PlatformIn 永远匹配不到 cursor 行。
func TestCursorAccountsSelectableByPlatformFilter(t *testing.T) {
	platforms := schedulerSnapshotPlatforms()
	require.Contains(t, platforms, PlatformCursor,
		"cursor must be in the scheduler platform set or its accounts can never be selected")

	// group 86/94/107 这类非 cursor 分组：其候选平台集合由分组平台派生，cursor 号
	// 绑定后靠 account_groups 中间表 + PlatformIn(cursor) 的桶命中。此处只断言
	// 「cursor 在可查询平台集合中」这一必要条件，不臆断具体分组的解析结果。
	var found bool
	for _, p := range platforms {
		if p == PlatformCursor {
			found = true
			break
		}
	}
	require.True(t, found)
}

// ---------------------------------------------------------------------------
// 3. 鉴权材料按账号解析
// ---------------------------------------------------------------------------

// identityCursorQuotaKeys 是 QuotaKeyRepository 的测试替身：只实现 GetByID，
// CodeEncrypted 即明文激活码（与 cursor_upstream_client_test.go 同款替身口径）。
type identityCursorQuotaKeys struct {
	byID map[int64]*ent.QuotaKey
}

func (r *identityCursorQuotaKeys) GetByID(_ context.Context, id int64) (*ent.QuotaKey, error) {
	qk, ok := r.byID[id]
	if !ok {
		return nil, errors.New("quota key not found")
	}
	return qk, nil
}

func (r *identityCursorQuotaKeys) List(_ context.Context) ([]*ent.QuotaKey, error) { return nil, nil }

func (r *identityCursorQuotaKeys) UpdateStatusFields(context.Context, *ent.Tx, int64, string, bool, *string, *time.Time) error {
	return nil
}

func (r *identityCursorQuotaKeys) HardDelete(context.Context, *ent.Tx, int64) error { return nil }

func (r *identityCursorQuotaKeys) UpdateCredentials(context.Context, int64, string, string, string) error {
	return nil
}

func (r *identityCursorQuotaKeys) ListExpired(context.Context, time.Time) ([]*ent.QuotaKey, error) {
	return nil, nil
}

func (r *identityCursorQuotaKeys) ListByStatus(context.Context, string) ([]*ent.QuotaKey, error) {
	return nil, nil
}

func (r *identityCursorQuotaKeys) Create(context.Context, CreateQuotaKeyInput) (*ent.QuotaKey, error) {
	return nil, nil
}

func (r *identityCursorQuotaKeys) UpdateMeta(context.Context, *ent.Tx, int64, **time.Time, **string) error {
	return nil
}

func (r *identityCursorQuotaKeys) ListAccountsByQuotaKey(context.Context, int64) ([]*ent.Account, error) {
	return nil, nil
}

func (r *identityCursorQuotaKeys) ListAccountsByQuotaKeys(context.Context, []int64) ([]*ent.Account, error) {
	return nil, nil
}

func (r *identityCursorQuotaKeys) SoftDeleteAccountsByQuotaKey(context.Context, *ent.Tx, int64) ([]int64, error) {
	return nil, nil
}

func (r *identityCursorQuotaKeys) EnqueueAccountChangedOutbox(context.Context, *ent.Tx, []int64) error {
	return nil
}

func (r *identityCursorQuotaKeys) SoftDelete(context.Context, *ent.Tx, int64) error { return nil }

func (r *identityCursorQuotaKeys) ListEligibleForRefresh(context.Context) ([]*ent.QuotaKey, error) {
	return nil, nil
}

func (r *identityCursorQuotaKeys) MarkExpiredIfStillExpired(context.Context, *ent.Tx, int64, time.Time) (int, error) {
	return 0, nil
}

func (r *identityCursorQuotaKeys) SoftDeleteCursorAccountsByExpiredQuotaKey(context.Context, *ent.Tx, int64, time.Time) ([]int64, error) {
	return nil, nil
}

// SetDeviceIDIfAbsent 模拟 DB 的条件化写入（WHERE device_id IS NULL OR =”），
// 语义与 production repository / service 读判定三者一致：NULL 或空串才写入并返回
// 受影响行数 1；已有非空值则不覆盖、返回 0（由 DeviceIDFor 重读赢家值）。
func (r *identityCursorQuotaKeys) SetDeviceIDIfAbsent(_ context.Context, id int64, deviceID string) (int, error) {
	qk, ok := r.byID[id]
	if !ok {
		return 0, errors.New("quota key not found")
	}
	if qk.DeviceID != nil && *qk.DeviceID != "" {
		return 0, nil
	}
	qk.DeviceID = &deviceID
	return 1, nil
}

func qkID(v int64) *int64 { return &v }

// cursorTestDeviceIDs 用真实的 *QuotaKeyService 作 deviceId 解析器（B-10 生产同款路径），
// 底层挂内存替身仓储。不用桩：解析器正是本次修的接线点，桩会把它绕过去。
func cursorTestDeviceIDs(repo QuotaKeyRepository) CursorDeviceIDResolver {
	return NewQuotaKeyService(repo, NewCursorTestEncryptor(), nil, nil)
}

// TestCursorAuthProviderResolvesPerAccount 钉死鉴权材料按账号解析。
//
// 关键性质：两个不同 quota_key 的账号必须解析出**不同**材料（各自的激活码 + 各自的
// deviceId）。若实现退化为全局单例共享，两账号会拿到同一份材料 —— 即把 A 号的
// 凭证发给 B 号，这是本卡最严重的潜在缺陷，故显式反证。
func TestCursorAuthProviderResolvesPerAccount(t *testing.T) {
	repo := &identityCursorQuotaKeys{byID: map[int64]*ent.QuotaKey{
		1: {ID: 1, CodeEncrypted: "ACTIVATION-CODE-ONE"},
		2: {ID: 2, CodeEncrypted: "ACTIVATION-CODE-TWO"},
	}}

	p1, err := newCursorAccountAuthProvider(&Account{Platform: PlatformCursor, QuotaKeyID: qkID(1)}, repo, NewCursorTestEncryptor(), cursorTestDeviceIDs(repo))
	require.NoError(t, err)
	p2, err := newCursorAccountAuthProvider(&Account{Platform: PlatformCursor, QuotaKeyID: qkID(2)}, repo, NewCursorTestEncryptor(), cursorTestDeviceIDs(repo))
	require.NoError(t, err)
	require.NotNil(t, p1)
	require.NotNil(t, p2)

	// 依赖缺失（enc=nil / deviceIDs=nil）必须构造失败，绝不静默回落。
	_, err = newCursorAccountAuthProvider(&Account{Platform: PlatformCursor, QuotaKeyID: qkID(1)}, repo, nil, cursorTestDeviceIDs(repo))
	require.Error(t, err, "nil encryptor must fail construction (never fall back to plaintext)")
	_, err = newCursorAccountAuthProvider(&Account{Platform: PlatformCursor, QuotaKeyID: qkID(1)}, repo, NewCursorTestEncryptor(), nil)
	require.Error(t, err, "nil device id resolver must fail construction (deviceId 绝不兜底派生)")

	// 无 quota_key_id 的账号必须失败关闭。
	_, err = newCursorAccountAuthProvider(&Account{Platform: PlatformCursor}, repo, NewCursorTestEncryptor(), cursorTestDeviceIDs(repo))
	require.ErrorIs(t, err, ErrCursorQuotaKeyUnresolved)
}

// TestCursorAuthProviderUsesResolvedDeviceIDNotCodeDerived 是本次接线的**核心反证**。
//
// 背景：E1 曾把 x-jg-auth 的 `d` 写成 deviceID(code)（按激活码派生），B-10 已推翻该做法。
// 本用例从**出站材料明文**这一层断言：解密 zk03 产物后，d 必须等于 deviceIDFor 解析出的
// 落库值，且**必须不等于** deviceID(激活码)。
//
// 这条断言是刻意选的：只断言"材料随账号不同"或"调用了 resolver"都抓不住回落派生
// （按码派生同样随账号不同），只有比对明文里的 d 才能钉死。已用变异测试坐实：
// 把实现改回 deviceID(code) 后本用例失败。
func TestCursorAuthProviderUsesResolvedDeviceIDNotCodeDerived(t *testing.T) {
	const activation = "ACTIVATION-CODE-FOR-DEVICEID-PROOF"
	repo := &identityCursorQuotaKeys{byID: map[int64]*ent.QuotaKey{
		1: {ID: 1, CodeEncrypted: activation},
	}}

	p, err := newCursorAccountAuthProvider(
		&Account{Platform: PlatformCursor, QuotaKeyID: qkID(1)},
		repo, NewCursorTestEncryptor(), cursorTestDeviceIDs(repo))
	require.NoError(t, err)

	material, err := p.CursorUpstreamAuth(context.Background())
	require.NoError(t, err)

	// 解开出站材料读出 d（明文是 JSON{d,a}，激活码非敏感、deviceId 亦非凭据）。
	plain, err := wasmcrypto.DecryptLocalData(material)
	require.NoError(t, err)
	var decoded cursorEncryptedCredentialsInput
	require.NoError(t, json.Unmarshal([]byte(plain), &decoded))

	// 期望值 = resolver 解析出且已落库的那一份（DeviceIDFor 生成后写回行内）。
	require.Equal(t, *repo.byID[1].DeviceID, decoded.D,
		"出站 d 必须是 DeviceIDFor 解析/落库的 deviceId")
	require.Equal(t, activation, decoded.A)

	// 反证：按激活码派生的值绝不能出现在出站材料里。
	require.NotEqual(t, deviceID(activation), decoded.D,
		"出站 d 绝不能是 deviceID(激活码)：B-10 已推翻按码派生（插件语义=uuidv4 随机固化）")
}

// TestCursorAuthProviderPersistsAndReusesDeviceID 钉死 deviceId 落库后**永久复用**：
// 第二次取材料必须拿到同一份 d（不得重新生成），且行内 device_id 已被写回。
func TestCursorAuthProviderPersistsAndReusesDeviceID(t *testing.T) {
	repo := &identityCursorQuotaKeys{byID: map[int64]*ent.QuotaKey{
		1: {ID: 1, CodeEncrypted: "ACTIVATION-CODE-REUSE"},
	}}
	resolver := cursorTestDeviceIDs(repo)

	first, err := resolver.DeviceIDFor(context.Background(), 1)
	require.NoError(t, err)
	require.NotEmpty(t, first)
	require.Equal(t, first, *repo.byID[1].DeviceID, "首次解析必须把 deviceId 落库")

	// 新 provider 实例（模拟后续请求）必须复用库里那份，而不是再随机一个。
	second, err := resolver.DeviceIDFor(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, first, second, "deviceId 落库后必须永久复用")
}

// TestCursorAuthProviderDistinctPerQuotaKey 证明材料按账号隔离。
func TestCursorAuthProviderDistinctPerQuotaKey(t *testing.T) {
	repo := &identityCursorQuotaKeys{byID: map[int64]*ent.QuotaKey{
		1: {ID: 1, CodeEncrypted: "ACTIVATION-CODE-ONE"},
		2: {ID: 2, CodeEncrypted: "ACTIVATION-CODE-TWO"},
	}}

	materialOf := func(quotaKeyID int64) string {
		p, err := newCursorAccountAuthProvider(
			&Account{Platform: PlatformCursor, QuotaKeyID: qkID(quotaKeyID)},
			repo, NewCursorTestEncryptor(), cursorTestDeviceIDs(repo))
		require.NoError(t, err)
		got, aerr := p.CursorUpstreamAuth(context.Background())
		require.NoError(t, aerr)
		require.NotEmpty(t, got)
		return got
	}

	m1 := materialOf(1)
	m2 := materialOf(2)
	require.NotEqual(t, m1, m2,
		"materials for different quota keys must differ (per-account auth, never a shared global)")

	// 同一 provider 实例重复取必须稳定（E1 provider 内部缓存生效，避免每次对话跑 wasm）。
	p, err := newCursorAccountAuthProvider(
		&Account{Platform: PlatformCursor, QuotaKeyID: qkID(1)}, repo, NewCursorTestEncryptor(), cursorTestDeviceIDs(repo))
	require.NoError(t, err)
	first, err := p.CursorUpstreamAuth(context.Background())
	require.NoError(t, err)
	second, err := p.CursorUpstreamAuth(context.Background())
	require.NoError(t, err)
	require.Equal(t, first, second, "same provider instance must reuse its cached material")
}

// TestCursorAuthProviderFailsClosedOnUnresolvableKey 钉死不可解析时失败关闭。
func TestCursorAuthProviderFailsClosedOnUnresolvableKey(t *testing.T) {
	repo := &identityCursorQuotaKeys{byID: map[int64]*ent.QuotaKey{
		1: {ID: 1, CodeEncrypted: ""}, // 无激活码
	}}
	p, err := newCursorAccountAuthProvider(
		&Account{Platform: PlatformCursor, QuotaKeyID: qkID(1)}, repo, NewCursorTestEncryptor(), cursorTestDeviceIDs(repo))
	require.NoError(t, err)
	_, aerr := p.CursorUpstreamAuth(context.Background())
	require.ErrorIs(t, aerr, ErrCursorQuotaKeyUnresolved)

	// 取不存在的 key 同样失败关闭。
	p2, err := newCursorAccountAuthProvider(
		&Account{Platform: PlatformCursor, QuotaKeyID: qkID(99)}, repo, NewCursorTestEncryptor(), cursorTestDeviceIDs(repo))
	require.NoError(t, err)
	_, aerr = p2.CursorUpstreamAuth(context.Background())
	require.Error(t, aerr)
}

// ---------------------------------------------------------------------------
// 4. 失败关闭
// ---------------------------------------------------------------------------

// TestForwardCursorFailsClosed 钉死各前置条件缺失时绝不发出请求。
//
// E6 起协议桥已实现，故本用例的失败关闭点前移到**依赖装配**层：
// 未注入转发器/鉴权依赖时必须在发出任何请求前失败。
func TestForwardCursorFailsClosed(t *testing.T) {
	body := []byte(`{"model":"cursor-sonnet","messages":[{"role":"user","content":"hi"}]}`)
	mapped := &Account{
		ID: 1, Platform: PlatformCursor, QuotaKeyID: qkID(1),
		Credentials: map[string]any{"model_mapping": map[string]any{"cursor-sonnet": "upstream-sonnet"}},
	}

	// ① 未装配执行部件 → 失败关闭（旧链的"默认转发器构造"回退已删除，§24.2-2）。
	unwired := NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil /* cursorExecution */)
	_, err := executeCursorForTest(t, unwired, context.Background(), nil, mapped, body, "cursor-sonnet", false, time.Now())
	require.ErrorIs(t, err, ErrCursorExecutionUnavailable)

	// ② 装配执行部件后：无 model_mapping → 模型闸门先失败关闭（绝不发出请求）。
	repo := &identityCursorQuotaKeys{byID: map[int64]*ent.QuotaKey{
		1: {ID: 1, CodeEncrypted: "ACTIVATION-CODE-ONE"},
	}}
	svc := NewOpenAIGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil /* cursorExecution */)
	svc.cursorExecution = attachCursorTestExecution(t, svc, repo, NewCursorTestEncryptor(), cursorTestDeviceIDs(repo),
		NewCursorUpstreamForwarder(WithCursorUpstreamHTTPClient(&http.Client{Transport: &cursorRejectAllTransport{t}})))

	_, err = executeCursorForTest(t, svc, context.Background(), nil,
		&Account{ID: 2, Platform: PlatformCursor, QuotaKeyID: qkID(1)}, body, "whatever", false, time.Now())
	require.ErrorIs(t, err, ErrCursorModelNotMapped)

	// ③ 非 cursor 账号 → 拒绝（分派谓词先于任何出站）。
	_, err = executeCursorForTest(t, svc, context.Background(), nil,
		&Account{ID: 3, Platform: PlatformOpenAI}, body, "m", false, time.Now())
	require.Error(t, err)
}

// cursorRejectAllTransport 让任何出站请求立刻失败并标记 t，
// 用于坐实"失败关闭发生在发出请求之前"。
type cursorRejectAllTransport struct{ t *testing.T }

func (rt *cursorRejectAllTransport) RoundTrip(*http.Request) (*http.Response, error) {
	rt.t.Error("cursor forwarder must not issue an upstream request on the fail-closed paths")
	return nil, errors.New("blocked by cursorRejectAllTransport")
}
