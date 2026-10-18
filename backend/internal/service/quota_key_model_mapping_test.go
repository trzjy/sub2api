//go:build unit

package service

// cursor 拉号账号 credentials.model_mapping 接入修复的定向测试（方案
// docs-local/cursor-model-mapping-repair-plan.md，2026-10-18）。
//
// 入口纪律（派发单 Done when）：全部用例从真实生产入库入口进入（RefreshAccounts /
// PullAccountsForModel / pull 共享状态机），以测试上游替身返回凭证（替身仅存测试，
// 合同 §5）；绝不在测试账号上手工预填 model_mapping 绕过待修链路。
//
// 覆盖：①手动刷新 + 自动补池两入口均写入权威恒等映射；②持久化重读后选中模型可被
// IsModelSupported / ResolveMappedModel 识别、未选中/无权威映射模型失败关闭；
// ③凭证加密（enc:v1:）与 credentials_mac（对解密后明文整份 HMAC）定向回归；
// ④同批号配置单快照（映射与 ApplyEntryDefaults 同源，方案审 F2）；
// ⑤配置读取失败（上游发号后）挂 ErrQuotaKeyMountFailed 哨兵 + 事务回滚号不入库
//（方案审 F1）；⑥autoEntry 未装配时不写映射（现状失败关闭，不放宽）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	entgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
	"github.com/stretchr/testify/require"
)

// fakeAutoEntryProvider 是 AutoEntryConfigProvider 的测试替身：按调用次序弹出配置，
// 记录 GetConfig 次数与 ApplyEntryDefaults 实际收到的配置指针（单快照复用断言用）。
type fakeAutoEntryProvider struct {
	cfgs          []*CursorAutoEntryConfig // 每次 GetConfig 依次返回；耗尽后重复最后一个
	getConfigErr  error                    // 非 nil 时 GetConfig 返回该错误
	getConfigHits int
	applyCfgs     []*CursorAutoEntryConfig // ApplyEntryDefaults 收到的 cfg 指针序列
}

func (f *fakeAutoEntryProvider) GetConfig(_ context.Context) (*CursorAutoEntryConfig, error) {
	if f.getConfigErr != nil {
		return nil, f.getConfigErr
	}
	f.getConfigHits++
	if len(f.cfgs) == 0 {
		return DefaultAutoEntryConfig(), nil
	}
	cfg := f.cfgs[0]
	if len(f.cfgs) > 1 {
		f.cfgs = f.cfgs[1:]
	}
	return cfg, nil
}

func (f *fakeAutoEntryProvider) ApplyEntryDefaults(_ context.Context, _ *ent.Tx, acc *ent.Account, cfg *CursorAutoEntryConfig) (*ent.Account, error) {
	f.applyCfgs = append(f.applyCfgs, cfg)
	return acc, nil
}

// cursorMappingTestConfig 构造只勾选给定实供模型的入库配置（厂商按实供清单真实归属自动启用）。
func cursorMappingTestConfig(t *testing.T, ids ...string) *CursorAutoEntryConfig {
	t.Helper()
	realVendor := CursorRealSupplyVendorByID()
	cfg := DefaultAutoEntryConfig()
	cfg.Models = make([]CursorModelRef, 0, len(ids))
	for _, id := range ids {
		vendor, ok := realVendor[id]
		require.True(t, ok, "test precondition: %s must be in the real supply list", id)
		cfg.Models = append(cfg.Models, CursorModelRef{ID: id, Vendor: vendor})
	}
	// 关闭未勾选模型所属厂商，保证勾选集合即唯一模型来源。
	for v := range cfg.Vendors {
		cfg.Vendors[v] = false
	}
	for _, id := range ids {
		cfg.Vendors[realVendor[id]] = true
	}
	return cfg
}

// cursorUpstreamStubAccount 构造上游替身返回的真实形态凭证（无 model_mapping——
// 与 buildAccount 产物一致，映射必须由落库链路写入）。
func cursorUpstreamStubAccount(name string) *UpstreamAccount {
	return &UpstreamAccount{
		Name:     name,
		Platform: "cursor",
		Type:     "oauth",
		Credentials: map[string]any{
			"mail":          name,
			"access_token":  "PLAIN_ACCESS_TOKEN_" + name,
			"refresh_token": "PLAIN_REFRESH_TOKEN_" + name,
		},
	}
}

// cursorDecryptStoredCredentials 把落库存储形态的凭证还原为明文（生产读路径
// decryptCredentialsMap 的测试等价：敏感键遇 enc:v1: 前缀解密，其余原样）。
func cursorDecryptStoredCredentials(t *testing.T, stored map[string]any) map[string]any {
	t.Helper()
	out := make(map[string]any, len(stored))
	for k, v := range stored {
		s, ok := v.(string)
		if ok && credcrypt.IsEncrypted(s) {
			plain, err := credcrypt.Decrypt(s)
			require.NoError(t, err, "decrypt stored credential key %s", k)
			out[k] = plain
			continue
		}
		out[k] = v
	}
	return out
}

// requireCursorModelMappingGateway 用 service.Account 契约断言映射网关行为：
// 选中模型可命中（IsModelSupported=true / ResolveMappedModel 恒等命中），其余失败关闭。
func requireCursorModelMappingGateway(t *testing.T, creds map[string]any, selected string, unselected ...string) {
	t.Helper()
	acc := &Account{Platform: domain.PlatformCursor, Credentials: creds}
	require.True(t, acc.IsModelSupported(selected), "selected model %s must be supported", selected)
	mapped, matched := acc.ResolveMappedModel(selected)
	require.True(t, matched, "selected model %s must hit the account mapping explicitly", selected)
	require.Equal(t, selected, mapped, "identity mapping must resolve to the same wire id")
	for _, m := range unselected {
		require.False(t, acc.IsModelSupported(m), "unselected model %s must fail closed", m)
		_, matched := acc.ResolveMappedModel(m)
		require.False(t, matched, "unselected model %s must not hit the mapping", m)
	}
}

// requireCursorMappingEncryptionMAC 定向回归：access_token/refresh_token 落库为 enc:v1: 密文，
// credentials_mac == 对解密后明文整份 canonical JSON 的 HMAC（与写路径同算法）。
func requireCursorMappingEncryptionMAC(t *testing.T, stored map[string]any, storedMAC string) {
	t.Helper()
	at, _ := stored["access_token"].(string)
	rt, _ := stored["refresh_token"].(string)
	require.True(t, strings.HasPrefix(at, credcrypt.Prefix), "access_token must be stored encrypted")
	require.True(t, strings.HasPrefix(rt, credcrypt.Prefix), "refresh_token must be stored encrypted")
	plain := cursorDecryptStoredCredentials(t, stored)
	canonical, err := json.Marshal(plain)
	require.NoError(t, err)
	require.Equal(t, credentialsHMACOfJSON(canonical), storedMAC,
		"credentials_mac must be the HMAC of the final plaintext map (including model_mapping)")
}

// cleanupCursorMappingGroup 清理本用例自建分组（共享持久库 quota_test 隔离纪律）。
func cleanupCursorMappingGroup(t *testing.T, client *ent.Client, groupID int64) {
	t.Helper()
	ctx := context.Background()
	_, _ = client.AccountGroup.Delete().Where(accountgroup.GroupIDEQ(groupID)).Exec(ctx)
	_, _ = client.Group.Delete().Where(entgroup.IDEQ(groupID)).Exec(ctx)
}

// 用例 ①（手动刷新入口）：RefreshAccounts 拉到的账号在落库链路写入权威恒等映射；
// 持久化重读后选中模型过网关、未选中/官方清单名失败关闭；加密 + MAC 覆盖含映射的最终明文；
// 白名单交集挂组照常。
func TestQuotaKeyPull_ModelMapping_ManualRefresh(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	grp, err := client.Group.Create().
		SetName(fmt.Sprintf("cursor-map-manual-%d", time.Now().UnixNano())).
		SetModelAllowlist(domain.GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6-sol"}}).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupCursorMappingGroup(t, client, grp.ID) })

	provider := &fakeAutoEntryProvider{cfgs: []*CursorAutoEntryConfig{
		cursorMappingTestConfig(t, "gpt-6-sol", "claude-sonnet-5-5"),
	}}
	ua := cursorUpstreamStubAccount("map-manual@x.com")
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{ua}},
	})
	svc.SetAutoEntry(provider)
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	require.NoError(t, svc.RefreshAccounts(ctx, qk.ID), "manual refresh must persist the account")

	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).WithGroups().All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1)
	stored := accs[0].Credentials

	// 映射形态：恒等、只含实供清单门控后的勾选集。
	mm, ok := stored["model_mapping"].(map[string]any)
	require.True(t, ok, "credentials.model_mapping must be persisted as a map, got %T", stored["model_mapping"])
	require.Equal(t, map[string]any{"gpt-6-sol": "gpt-6-sol", "claude-sonnet-5-5": "claude-sonnet-5-5"}, mm)

	// 上游产物不被就地污染（落库走浅拷贝，上游替身的原始 map 保持无映射键）。
	require.NotContains(t, ua.Credentials, "model_mapping")

	// 持久化重读 → 网关契约：选中命中；未勾选实供模型与官方清单名均失败关闭。
	requireCursorModelMappingGateway(t, cursorDecryptStoredCredentials(t, stored),
		"gpt-6-sol", "grok-4.7", "gpt-4.1", "claude-opus-5-5")

	// 加密 + MAC 定向回归（MAC 覆盖含 model_mapping 的最终明文）。
	requireCursorMappingEncryptionMAC(t, stored, accs[0].CredentialsMAC)

	// 白名单交集挂组不回归。
	require.True(t, accountGroupLinked(t, ctx, client, accs[0].ID, grp.ID),
		"manual refresh must still mount to the whitelist-intersecting group")
}

// 用例 ②（自动补池入口）：PullAccountsForModel 同样写入映射，且映射与
// ApplyEntryDefaults 消费同一份配置快照（单次 GetConfig；方案审 F2）。
func TestQuotaKeyPullForModel_ModelMapping_SingleConfigSnapshot(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	modelM := "gpt-6-sol"
	grp, err := client.Group.Create().
		SetName(fmt.Sprintf("cursor-map-auto-%d", time.Now().UnixNano())).
		SetModelAllowlist(domain.GroupModelAllowlist{Enabled: true, Models: []string{modelM}}).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupCursorMappingGroup(t, client, grp.ID) })

	// 第一次读取（落库前，供映射）返回含 gpt-6-sol 的配置；若实现错误地二次读取，
	// 后续读取只含 claude-sonnet-5-5 —— 映射与 entry_defaults 立即暴露口径分裂。
	cfgA := cursorMappingTestConfig(t, "gpt-6-sol")
	cfgB := cursorMappingTestConfig(t, "claude-sonnet-5-5")
	provider := &fakeAutoEntryProvider{cfgs: []*CursorAutoEntryConfig{cfgA, cfgB}}
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{cursorUpstreamStubAccount("map-auto@x.com")}},
	})
	svc.SetAutoEntry(provider)
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	require.NoError(t, svc.PullAccountsForModel(ctx, qk.ID, modelM))

	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1)
	mm, ok := accs[0].Credentials["model_mapping"].(map[string]any)
	require.True(t, ok, "auto pull must persist credentials.model_mapping")
	require.Equal(t, map[string]any{"gpt-6-sol": "gpt-6-sol"}, mm,
		"mapping must come from the FIRST config snapshot")
	require.Equal(t, 1, provider.getConfigHits,
		"one pull must read the entry config exactly once (single snapshot for mapping + entry defaults)")
	require.Len(t, provider.applyCfgs, 1)
	require.Same(t, cfgA, provider.applyCfgs[0],
		"ApplyEntryDefaults must receive the same snapshot the mapping was built from")
}

// 用例 ③：配置读取失败发生在上游成功发号之后（shareai 非幂等）→ 挂
// ErrQuotaKeyMountFailed 哨兵、事务回滚号不入库（方案审 F1 must_fix）。
func TestQuotaKeyPull_ConfigLoadFailAfterUpstreamCarriesMountFailed(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{cursorUpstreamStubAccount("map-cfgfail@x.com")}},
	})
	svc.SetAutoEntry(&fakeAutoEntryProvider{getConfigErr: errors.New("settings unavailable")})
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	err := svc.RefreshAccounts(ctx, qk.ID)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrQuotaKeyMountFailed),
		"config failure after upstream success must carry the non-idempotent mount-failed sentinel")

	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, err)
	require.Empty(t, accs, "transaction must roll back: no account may be persisted")
}

// 用例 ④：autoEntry 未装配（nil）→ 不写映射，账号对一切模型失败关闭（现状不放宽）。
func TestQuotaKeyPull_NoAutoEntry_NoMappingFailClosed(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	modelM := fix1UniqueModel("cursor-map-nil")
	grp, err := client.Group.Create().
		SetName(fmt.Sprintf("cursor-map-nil-%d", time.Now().UnixNano())).
		SetModelAllowlist(domain.GroupModelAllowlist{Enabled: true, Models: []string{modelM}}).
		Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { cleanupCursorMappingGroup(t, client, grp.ID) })

	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{cursorUpstreamStubAccount("map-nil@x.com")}},
	})
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	require.NoError(t, svc.PullAccountsForModel(ctx, qk.ID, modelM))

	accs, err := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, err)
	require.Len(t, accs, 1)
	_, ok := accs[0].Credentials["model_mapping"]
	require.False(t, ok, "unwired autoEntry must not write model_mapping")
	gw := &Account{Platform: domain.PlatformCursor, Credentials: accs[0].Credentials}
	require.False(t, gw.IsModelSupported(modelM), "unmapped cursor account must fail closed")
}
