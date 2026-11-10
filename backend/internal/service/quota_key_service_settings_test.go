//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

// qksPtrInt 是测试内构造 *int 的局部辅助（避免与同包其它 _test.go 中的 ptrInt 重名）。
func qksPtrInt(v int) *int { return &v }

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
