//go:build unit

package service

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// qksPtrInt 是测试内构造 *int 的局部辅助（避免与同包其它 _test.go 中的 ptrInt 重名）。
func qksPtrInt(v int) *int { return &v }

// qksUniqueCode 生成每次运行唯一的激活码，避免共享测试库中 uq_quota_keys_code_mac 唯一索引冲突
// （固定码在第二次运行时必然冲突，导致假红）。
func qksUniqueCode(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, atomic.AddInt64(&qksSeq, 1), time.Now().UnixNano())
}

var qksSeq int64

// TestQuotaKeyServiceSettings_CreateWithSettingsRejectsEmptyModelMapping 验证：
// CreateWithSettings 在 ModelMappingSet=true 且规范化后为空（空映射 / 仅空键空值）时，
// 必须返回 400 QUOTA_KEY_MODELS_REQUIRED，禁止落库（没有任何支持的模型就禁止落库）。
func TestQuotaKeyServiceSettings_CreateWithSettingsRejectsEmptyModelMapping(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, nil)
	ctx := context.Background()

	// 空映射。
	_, err := svc.CreateWithSettings(ctx, "somecode", nil, nil, 0, "",
		&UpdateQuotaKeySettingsInput{ModelMappingSet: true, ModelMapping: map[string]string{}})
	require.Error(t, err)
	require.Equal(t, 400, infraerrors.Code(err), "空映射必须返回 400")
	require.Equal(t, "QUOTA_KEY_MODELS_REQUIRED", infraerrors.Reason(err))

	// 仅含空键 / 空值（规范化后仍为 0 条）。
	_, err = svc.CreateWithSettings(ctx, "somecode", nil, nil, 0, "",
		&UpdateQuotaKeySettingsInput{ModelMappingSet: true, ModelMapping: map[string]string{"": "x", "y": ""}})
	require.Error(t, err)
	require.Equal(t, 400, infraerrors.Code(err))
	require.Equal(t, "QUOTA_KEY_MODELS_REQUIRED", infraerrors.Reason(err))
}

// TestQuotaKeyServiceSettings_UpdateWithSettingsRejectsEmptyModelMapping 验证：
// UpdateWithSettings 的同一套校验同样拦截空 model_mapping（在开事务前即失败关闭，
// 不触碰仓储）。
func TestQuotaKeyServiceSettings_UpdateWithSettingsRejectsEmptyModelMapping(t *testing.T) {
	repo := newFakeQuotaKeyRepo()
	svc := NewQuotaKeyService(repo, NewCredcryptEncryptor(), nil, nil)
	ctx := context.Background()

	_, err := svc.UpdateWithSettings(ctx, 1, nil, nil, nil, nil,
		&UpdateQuotaKeySettingsInput{ModelMappingSet: true, ModelMapping: map[string]string{}})
	require.Error(t, err)
	require.Equal(t, 400, infraerrors.Code(err))
	require.Equal(t, "QUOTA_KEY_MODELS_REQUIRED", infraerrors.Reason(err))
}

// TestQuotaKeyToStatus_ProjectsSettings 验证 quotaKeyToStatus 把码级设置（Concurrency /
// LoadFactor / RateMultiplier / ModelMapping）正确投影到脱敏 DTO，包括 RateMultiplier=0
// 与 ModelMapping 深拷贝（不共享底层 map）。
func TestQuotaKeyToStatus_ProjectsSettings(t *testing.T) {
	srcMapping := map[string]string{"gpt-4": "gpt-4", "claude-3": "claude-3"}
	qk := &ent.QuotaKey{
		ID:             42,
		Status:         domain.QuotaKeyStatusActive,
		Concurrency:    qksPtrInt(5),
		LoadFactor:     qksPtrInt(3),
		RateMultiplier: 0, // 0 是合法计费倍率，必须稳定投影（不回落 1.0）。
		ModelMapping:   srcMapping,
	}
	st := quotaKeyToStatus(qk)
	require.Equal(t, qksPtrInt(5), st.Concurrency)
	require.Equal(t, qksPtrInt(3), st.LoadFactor)
	require.Equal(t, 0.0, st.RateMultiplier, "RateMultiplier=0 必须原样投影")
	require.Equal(t, srcMapping, st.ModelMapping)
	// 深拷贝：修改返回的 map 不应影响原始实体。
	st.ModelMapping["injected"] = "x"
	_, has := qk.ModelMapping["injected"]
	require.False(t, has, "quotaKeyToStatus 必须深拷贝 ModelMapping，调用方不得改到实体内部")
}

// TestQuotaKeyServiceSettings_PullInheritsCodeLevelSettings 验证：拉号新建账号时，
// 若激活码 model_mapping 非空，优先采用码级映射写入账号 credentials.model_mapping；
// 且新账号继承码级 Concurrency / LoadFactor / RateMultiplier / Priority / ExpiresAt。
func TestQuotaKeyServiceSettings_PullInheritsCodeLevelSettings(t *testing.T) {
	expiresAt := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	svc, client := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{
			Accounts: []*UpstreamAccount{
				{Name: "pulled-acc", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"access_token": "tok", "refresh_token": "ref"}},
			},
			ExpiresAt: &expiresAt,
		},
	})
	ctx := context.Background()

	// 直接经 ent client 建一个带码级设置的激活码（testDBRepo.Create 不写这些列，故绕过）。
	// 不写 code_mac（quota_keys 对 code_mac 有唯一约束，置 NULL 即可，沿用 mustCreateQuotaKey 口径）。
	codeMapping := map[string]string{"gpt-4": "gpt-4", "claude-3": "claude-3"}
	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted("enc:v1:test").
		SetStatus(domain.QuotaKeyStatusActive).
		SetConcurrency(5).
		SetLoadFactor(3).
		SetRateMultiplier(2.0).
		SetPriority(7).
		SetExpiresAt(expiresAt).
		SetModelMapping(codeMapping).
		Save(ctx)
	require.NoError(t, err)

	require.NoError(t, svc.PullAccounts(ctx, qk.ID))

	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1, "应只落 1 个账号")
	acc := accs[0]

	// 继承码级设置（码级权威值，覆盖账号列默认）。Concurrency/Priority/RateMultiplier 为值类型，
	// LoadFactor 为可空 *int。
	require.Equal(t, 5, acc.Concurrency, "应继承码级 Concurrency")
	require.NotNil(t, acc.LoadFactor)
	require.Equal(t, 3, *acc.LoadFactor, "应继承码级 LoadFactor")
	require.Equal(t, 2.0, acc.RateMultiplier, "应继承码级 RateMultiplier")
	require.Equal(t, 7, acc.Priority, "应继承码级 Priority")
	require.NotNil(t, acc.ExpiresAt, "应继承到期时间")
	require.Equal(t, expiresAt.Unix(), acc.ExpiresAt.Unix(), "应继承上游返回的到期时间")

	// 优先采用码级 model_mapping（而非上游静态清单）。
	creds := cursorDecryptStoredCredentials(t, acc.Credentials)
	mm, ok := creds["model_mapping"].(map[string]any)
	require.True(t, ok, "model_mapping 必须以明文 map 落库")
	require.Len(t, mm, 2, "应恰好 2 个码级模型")
	require.Equal(t, "gpt-4", mm["gpt-4"])
	require.Equal(t, "claude-3", mm["claude-3"])
}

// TestQuotaKeyServiceSettings_ResolvePreviewAccountForModelSync 验证：
// 优先选 active && schedulable 的 cursor 账号；健康账号缺失时退一步用任意未软删 cursor 账号；
// 无任何未软删 cursor 账号时返回 400 QUOTA_KEY_NO_ACCOUNT_FOR_SYNC。
func TestQuotaKeyServiceSettings_ResolvePreviewAccountForModelSync(t *testing.T) {
	svc, client := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "x", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"access_token": "t"}},
		}},
	})
	ctx := context.Background()

	// 无账号 → 400。
	qkNone := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	_, err := svc.ResolvePreviewAccountForModelSync(ctx, qkNone.ID)
	require.Error(t, err)
	require.Equal(t, 400, infraerrors.Code(err))
	require.Equal(t, "QUOTA_KEY_NO_ACCOUNT_FOR_SYNC", infraerrors.Reason(err))

	// 健康可调度账号优先。
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	healthy, err := client.Account.Create().
		SetName("healthy").SetPlatform(domain.PlatformCursor).SetType("oauth").
		SetCredentials(map[string]interface{}{"access_token": "t1"}).SetCredentialsMAC("m1").
		SetQuotaKeyID(qk.ID).SetStatus(domain.StatusActive).SetSchedulable(true).Save(ctx)
	require.NoError(t, err)
	_, err = client.Account.Create().
		SetName("disabled").SetPlatform(domain.PlatformCursor).SetType("oauth").
		SetCredentials(map[string]interface{}{"access_token": "t2"}).SetCredentialsMAC("m2").
		SetQuotaKeyID(qk.ID).SetStatus("disabled").SetSchedulable(false).Save(ctx)
	require.NoError(t, err)

	acc, err := svc.ResolvePreviewAccountForModelSync(ctx, qk.ID)
	require.NoError(t, err)
	require.Equal(t, healthy.ID, acc.ID, "应优先选 active && schedulable 账号")

	// 仅兜底账号（无健康可调度）时返回首个未软删 cursor 账号。
	qk2 := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	fallback, err := client.Account.Create().
		SetName("fallback").SetPlatform(domain.PlatformCursor).SetType("oauth").
		SetCredentials(map[string]interface{}{"access_token": "t3"}).SetCredentialsMAC("m3").
		SetQuotaKeyID(qk2.ID).SetStatus("error").SetSchedulable(false).Save(ctx)
	require.NoError(t, err)
	acc2, err := svc.ResolvePreviewAccountForModelSync(ctx, qk2.ID)
	require.NoError(t, err)
	require.Equal(t, fallback.ID, acc2.ID, "无健康账号时应返回兜底 cursor 账号")

	// 非 cursor 平台账号不算（即使 active && schedulable）。
	qk3 := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)
	_, err = client.Account.Create().
		SetName("openai-acc").SetPlatform(domain.PlatformOpenAI).SetType("oauth").
		SetCredentials(map[string]interface{}{"access_token": "t4"}).SetCredentialsMAC("m4").
		SetQuotaKeyID(qk3.ID).SetStatus(domain.StatusActive).SetSchedulable(true).Save(ctx)
	require.NoError(t, err)
	_, err = svc.ResolvePreviewAccountForModelSync(ctx, qk3.ID)
	require.Error(t, err)
	require.Equal(t, 400, infraerrors.Code(err), "仅非 cursor 账号时应判无可用账号")
}

// ---------------------------------------------------------------------------
// QKG-B2a 追加：激活码服务接收 group_ids 并触发白名单追加。
// ---------------------------------------------------------------------------

// settingsTestCreateGroup 创建 active 未软删分组并返回其 ID（带自动清理）。
func settingsTestCreateGroup(t *testing.T, client *ent.Client, platform string) int64 {
	t.Helper()
	ctx := context.Background()
	g, err := client.Group.Create().
		SetName("qkg-b2a-g").
		SetStatus(domain.StatusActive).
		SetPlatform(platform).
		SetModelAllowlist(domain.GroupModelAllowlist{Enabled: false, Models: nil}).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Group.DeleteOneID(g.ID).Exec(ctx) })
	return g.ID
}

// TestNormalizeQuotaKeyGroupIDs 覆盖 normalizeQuotaKeyGroupIDs：去重保序 / 非正数拒绝 /
// 不存在拒绝 / simple 模式 composite 拒绝 / 标准模式允许 composite / 测试替身（nil client）跳过存在性。
func TestNormalizeQuotaKeyGroupIDs(t *testing.T) {
	t.Run("dedup preserves order and drops duplicates", func(t *testing.T) {
		svc := NewQuotaKeyService(&testDBRepo{}, NewCredcryptEncryptor(), &fakeUpstreamClient{}, nil)
		out, err := svc.normalizeQuotaKeyGroupIDs(context.Background(), []int64{3, 1, 3, 2, 1, 3})
		require.NoError(t, err)
		require.Equal(t, []int64{3, 1, 2}, out)
	})

	t.Run("non-positive id rejected", func(t *testing.T) {
		svc := NewQuotaKeyService(&testDBRepo{}, NewCredcryptEncryptor(), &fakeUpstreamClient{}, nil)
		for _, bad := range [][]int64{{0}, {-1}, {1, 0, 2}, {-5}} {
			out, err := svc.normalizeQuotaKeyGroupIDs(context.Background(), bad)
			require.Error(t, err)
			require.Nil(t, out)
			require.Equal(t, "QUOTA_KEY_GROUP_IDS_INVALID", infraerrors.Reason(err))
		}
	})

	t.Run("nil client skips existence check", func(t *testing.T) {
		// client==nil（测试替身）应跳过存在性校验并原样返回（与 checkQuotaKeyExists 同口径）。
		svc := NewQuotaKeyService(&testDBRepo{}, NewCredcryptEncryptor(), &fakeUpstreamClient{}, nil)
		out, err := svc.normalizeQuotaKeyGroupIDs(context.Background(), []int64{42, 43})
		require.NoError(t, err)
		require.Equal(t, []int64{42, 43}, out)
	})

	t.Run("not found rejected", func(t *testing.T) {
		svc, client := qkNewTestService(t, &fakeUpstreamClient{})
		gID := settingsTestCreateGroup(t, client, PlatformAnthropic)
		out, err := svc.normalizeQuotaKeyGroupIDs(context.Background(), []int64{gID, 987654321})
		require.Error(t, err)
		require.Nil(t, out)
		require.Equal(t, "QUOTA_KEY_GROUP_NOT_FOUND", infraerrors.Reason(err))
	})

	t.Run("existing groups accepted", func(t *testing.T) {
		svc, client := qkNewTestService(t, &fakeUpstreamClient{})
		g1 := settingsTestCreateGroup(t, client, PlatformAnthropic)
		g2 := settingsTestCreateGroup(t, client, PlatformOpenAI)
		out, err := svc.normalizeQuotaKeyGroupIDs(context.Background(), []int64{g2, g1})
		require.NoError(t, err)
		require.Equal(t, []int64{g2, g1}, out)
	})

	t.Run("simple mode rejects composite", func(t *testing.T) {
		svc, client := qkNewTestService(t, &fakeUpstreamClient{})
		svc.SetRunMode(config.RunModeSimple)
		compositeID := settingsTestCreateGroup(t, client, PlatformComposite)
		normalID := settingsTestCreateGroup(t, client, PlatformAnthropic)
		out, err := svc.normalizeQuotaKeyGroupIDs(context.Background(), []int64{compositeID})
		require.Error(t, err)
		require.Nil(t, out)
		require.Equal(t, "SIMPLE_MODE_GROUP_NOT_BINDABLE", infraerrors.Reason(err))
		out, err = svc.normalizeQuotaKeyGroupIDs(context.Background(), []int64{normalID})
		require.NoError(t, err)
		require.Equal(t, []int64{normalID}, out)
	})

	t.Run("standard mode allows composite", func(t *testing.T) {
		svc, client := qkNewTestService(t, &fakeUpstreamClient{})
		svc.SetRunMode(config.RunModeStandard)
		compositeID := settingsTestCreateGroup(t, client, PlatformComposite)
		out, err := svc.normalizeQuotaKeyGroupIDs(context.Background(), []int64{compositeID})
		require.NoError(t, err)
		require.Equal(t, []int64{compositeID}, out)
	})
}

// TestQuotaKeyToStatus_GroupIDs 验证 quotaKeyToStatus 正确投影 GroupIDs，
// 含 nil 与空切片可区分、以及深拷贝隔离。
func TestQuotaKeyToStatus_GroupIDs(t *testing.T) {
	t.Run("nil group_ids projected as nil", func(t *testing.T) {
		qk := &ent.QuotaKey{ID: 1}
		st := quotaKeyToStatus(qk)
		require.Nil(t, st.GroupIDs, "nil group_ids 应保持 nil（未指定语义）")
	})

	t.Run("empty slice projected as non-nil empty slice", func(t *testing.T) {
		qk := &ent.QuotaKey{ID: 2, GroupIds: []int64{}}
		st := quotaKeyToStatus(qk)
		require.NotNil(t, st.GroupIDs, "空切片应投影为非 nil 空切片（显式解绑语义）")
		require.Len(t, *st.GroupIDs, 0)
	})

	t.Run("non-empty projected and deep-copied", func(t *testing.T) {
		src := []int64{7, 8, 9}
		qk := &ent.QuotaKey{ID: 3, GroupIds: src}
		st := quotaKeyToStatus(qk)
		require.NotNil(t, st.GroupIDs)
		require.Equal(t, []int64{7, 8, 9}, *st.GroupIDs)
		// 深拷贝隔离：修改实体原切片不影响已投影的 DTO。
		src[0] = 999
		require.Equal(t, int64(7), (*st.GroupIDs)[0], "投影应为深拷贝，不受实体内部切片改动影响")
	})
}

// TestQuotaKeyServiceSettings_GroupIDsGate 验证 CreateWithSettings 在落库前经
// normalizeQuotaKeyGroupIDs 门控：非法 / 不存在的分组直接失败关闭，不触碰仓储。
func TestQuotaKeyServiceSettings_GroupIDsGate(t *testing.T) {
	t.Run("non-positive group id rejected before create", func(t *testing.T) {
		svc, _ := qkNewTestService(t, &fakeUpstreamClient{})
		settings := &UpdateQuotaKeySettingsInput{GroupIDs: &[]int64{1, 0}}
		st, err := svc.CreateWithSettings(context.Background(), qksUniqueCode("sk-b2a"), nil, nil, 0, "", settings)
		require.Error(t, err)
		require.Nil(t, st)
		require.Equal(t, "QUOTA_KEY_GROUP_IDS_INVALID", infraerrors.Reason(err))
	})

	t.Run("non-existent group id rejected before create", func(t *testing.T) {
		svc, client := qkNewTestService(t, &fakeUpstreamClient{})
		gID := settingsTestCreateGroup(t, client, PlatformAnthropic)
		settings := &UpdateQuotaKeySettingsInput{GroupIDs: &[]int64{gID, 987650123}}
		st, err := svc.CreateWithSettings(context.Background(), qksUniqueCode("sk-b2a"), nil, nil, 0, "", settings)
		require.Error(t, err)
		require.Nil(t, st)
		require.Equal(t, "QUOTA_KEY_GROUP_NOT_FOUND", infraerrors.Reason(err))
	})

	t.Run("valid group ids pass normalization gate", func(t *testing.T) {
		svc, client := qkNewTestService(t, &fakeUpstreamClient{})
		gID := settingsTestCreateGroup(t, client, PlatformAnthropic)
		settings := &UpdateQuotaKeySettingsInput{GroupIDs: &[]int64{gID, gID}} // 含重复，应去重
		st, err := svc.CreateWithSettings(context.Background(), qksUniqueCode("sk-b2a"), nil, nil, 0, "", settings)
		require.NoError(t, err)
		require.NotNil(t, st)
	})
}

// ---------------------------------------------------------------------------
// QKG-B2b：拉号挂组改用码级分组与码级模型推导。
// ---------------------------------------------------------------------------

// qkgB2bCreateGroup 创建 active 未软删分组并返回其 ID（用时间戳后缀避免共享库唯一索引冲突，
// 并用 t.Cleanup 在用例结束时清理 account_groups 链接与该分组）。
func qkgB2bCreateGroup(t *testing.T, client *ent.Client, platform string, al domain.GroupModelAllowlist) int64 {
	t.Helper()
	ctx := context.Background()
	g, err := client.Group.Create().
		SetName(fmt.Sprintf("qkg-b2b-%d-%s", time.Now().UnixNano(), platform)).
		SetStatus(domain.StatusActive).
		SetPlatform(platform).
		SetModelAllowlist(al).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = client.AccountGroup.Delete().Where(accountgroup.GroupIDEQ(g.ID)).Exec(ctx)
		_ = client.Group.DeleteOneID(g.ID).Exec(ctx)
	})
	return g.ID
}

// qkgB2bCreateKeyWithGroupIDs 直建一个 active 激活码并显式写码级 GroupIds（绕过 testDBRepo.Create，
// 后者不写该列），返回其 ID，并在结束时清理。
func qkgB2bCreateKeyWithGroupIDs(t *testing.T, client *ent.Client, groupIDs []int64) int64 {
	t.Helper()
	ctx := context.Background()
	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted("enc:v1:test").
		SetStatus(domain.QuotaKeyStatusActive).
		SetGroupIds(groupIDs).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx) })
	return qk.ID
}

// TestPull_CodeLevelGroupIDs_SingleGroup 验证：码级分组存在（GroupIds=[g1]）时，新号只挂 g1，
// 不调用模型推导 resolver（直接采用码级分组权威值）。
func TestPull_CodeLevelGroupIDs_SingleGroup(t *testing.T) {
	svc, client := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "pulled", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"access_token": "t"}},
		}},
	})
	ctx := context.Background()
	g1 := qkgB2bCreateGroup(t, client, domain.PlatformAnthropic, domain.GroupModelAllowlist{Enabled: false})
	qkID := qkgB2bCreateKeyWithGroupIDs(t, client, []int64{g1})

	require.NoError(t, svc.PullAccountsForModel(ctx, qkID, "cursor-model-x"))

	accs, err := client.Account.Query().Where(account.QuotaKeyID(qkID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1, "应只落 1 个账号")

	require.True(t, accountGroupLinked(t, ctx, client, accs[0].ID, g1), "新号必须挂入码级分组 g1")

	// 只挂码级指定的单一分组（聚合/其它分组不得被牵连挂入）。
	links, err := client.AccountGroup.Query().Where(accountgroup.AccountIDEQ(accs[0].ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, links, 1, "应只挂入码级指定的单一分组")
	require.Equal(t, g1, links[0].GroupID, "链接分组必须为码级指定的 g1")
}

// TestPull_CodeLevelGroupIDs_EmptyNoUpstream 验证：码级分组显式解绑（GroupIds=[]）时，
// 必须在发上游之前被拦截——不调用上游、返回错误、且不落号（避免白耗每日拉号额度）。
func TestPull_CodeLevelGroupIDs_EmptyNoUpstream(t *testing.T) {
	var calls int
	svc, client := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "pulled", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"access_token": "t"}},
		}},
		onPull: func(*ent.QuotaKey) { calls++ },
	})
	ctx := context.Background()
	qkID := qkgB2bCreateKeyWithGroupIDs(t, client, []int64{})

	err := svc.PullAccountsForModel(ctx, qkID, "cursor-model-x")
	require.Error(t, err, "空分组绑定必须失败关闭")
	require.Equal(t, 0, calls, "空分组绑定不得在发上游前被放行（不调用上游）")

	accs, err := client.Account.Query().Where(account.QuotaKeyID(qkID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 0, "失败关闭时不得落号")
}

// TestPull_CodeLevelGroupIDs_NilWithClaudeMappingMountsAnthropicOnly 验证：GroupIds=nil 且码级
// model_mapping 仅含 claude-* 家族模型键时，按码级 model_mapping 推导模型族挂载，只挂 anthropic
// 家族平台组，不挂 openai 家族组。
func TestPull_CodeLevelGroupIDs_NilWithClaudeMappingMountsAnthropicOnly(t *testing.T) {
	svc, client := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "pulled", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"access_token": "t"}},
		}},
	})
	ctx := context.Background()
	gAnthropic := qkgB2bCreateGroup(t, client, domain.PlatformAnthropic, domain.GroupModelAllowlist{
		Enabled: true, Models: []string{"claude-3-5-sonnet", "claude-3-7-sonnet"},
	})
	gOpenAI := qkgB2bCreateGroup(t, client, domain.PlatformOpenAI, domain.GroupModelAllowlist{
		Enabled: true, Models: []string{"gpt-4"},
	})

	qk, err := client.QuotaKey.Create().
		SetCodeEncrypted("enc:v1:test").
		SetStatus(domain.QuotaKeyStatusActive).
		SetModelMapping(map[string]string{
			"claude-3-5-sonnet": "claude-3-5-sonnet",
			"claude-3-7-sonnet": "claude-3-7-sonnet",
		}).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.QuotaKey.DeleteOneID(qk.ID).Exec(ctx) })

	require.NoError(t, svc.PullAccountsForModel(ctx, qk.ID, "cursor-model-x"))

	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1, "应只落 1 个账号")

	require.True(t, accountGroupLinked(t, ctx, client, accs[0].ID, gAnthropic), "应挂入 anthropic 家族组")
	require.False(t, accountGroupLinked(t, ctx, client, accs[0].ID, gOpenAI), "不得挂入 openai 家族组")
}

// TestPull_CodeLevelGroupIDs_SoftDeletedFailsClosed 验证：码级分组指向一个已软删组时，
// filterLiveGroupIDs 过滤后无存活组 → 失败关闭、回滚、号不入库、不浪费上游额度。
func TestPull_CodeLevelGroupIDs_SoftDeletedFailsClosed(t *testing.T) {
	var calls int
	svc, client := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "pulled", Platform: "cursor", Type: "oauth", Credentials: map[string]any{"access_token": "t"}},
		}},
		onPull: func(*ent.QuotaKey) { calls++ },
	})
	ctx := context.Background()
	gSoft := qkgB2bCreateGroup(t, client, domain.PlatformAnthropic, domain.GroupModelAllowlist{Enabled: false})
	// 软删该组：码级分组绑定指向一个已软删的组。
	require.NoError(t, client.Group.UpdateOneID(gSoft).SetDeletedAt(time.Now()).Exec(ctx))
	qkID := qkgB2bCreateKeyWithGroupIDs(t, client, []int64{gSoft})

	err := svc.PullAccountsForModel(ctx, qkID, "cursor-model-x")
	require.Error(t, err, "码级分组全为软删组时应失败关闭")
	require.Equal(t, 0, calls, "软删组导致失败关闭，不应浪费上游额度")

	accs, err := client.Account.Query().Where(account.QuotaKeyID(qkID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 0, "失败关闭时号不入库")
}

// TestListCandidateKeys_SkipsEmptyGroupIDs 验证：ListCandidateKeys 在死码过滤之后跳过
// 显式解绑（GroupIds=[]）的码，使其不参与自动补池（但仍返回正常未绑定分组的码）。
func TestListCandidateKeys_SkipsEmptyGroupIDs(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()
	water := NewQuotaKeyModelWater(client, &curF1ConfigSource{cfg: &CursorAutoEntryConfig{}})

	// 显式解绑（空分组）的激活码：不应出现在候选中。
	qkEmpty := qkgB2bCreateKeyWithGroupIDs(t, client, []int64{})
	// 正常未绑定分组的激活码：应出现在候选中。
	qkNormal := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	cands, err := water.ListCandidateKeys(ctx, "", 0)
	require.NoError(t, err)
	require.NotContains(t, cands, qkEmpty, "显式解绑分组(空)的码不得参与自动补池")
	require.Contains(t, cands, qkNormal.ID, "正常未绑定分组的码应参与自动补池")
}
