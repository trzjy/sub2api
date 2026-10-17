//go:build unit

package service

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/ent/accountgroup"
	entgroup "github.com/Wei-Shaw/sub2api/ent/group"
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/pkg/credcrypt"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// CUR-F7 R4：三条经主会话核实成立的 must_fix 回修。
//
//   - R4-1：手动挂组口径对齐实供模型白名单（findCursorPlatformGroups）。
//   - R4-2：配置顶层 null 必须报错，不得静默成空模型集（GetConfig）。
//   - R4-3：加密未启用时 cursor 拉号落库必须失败关闭（prepareCursorAccountCredentials）。
// ---------------------------------------------------------------------------

// qkF7MustCreateGroup 创建 active 未软删分组并设置模型白名单，返回其 ID。
func qkF7MustCreateGroup(t *testing.T, client *ent.Client, name string, al domain.GroupModelAllowlist) int64 {
	t.Helper()
	ctx := context.Background()
	g, err := client.Group.Create().
		SetName(name).
		SetStatus(domain.StatusActive).
		SetModelAllowlist(al).
		Save(ctx)
	require.NoError(t, err)
	return g.ID
}

// TestCURF7_R4_1_ManualMountAlignsRealSupplyWhitelist：手动拉号挂载目标按实供模型白名单
// 覆盖判定——含实供模型的分组入选、仅含 "cursor" 字样的分组不挂、未启用白名单的分组入选。
func TestCURF7_R4_1_ManualMountAlignsRealSupplyWhitelist(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// R10-1（CUR-F13）：本用例自建分组须自行按 ID 清理，不得遗留为共享库污染（否则他用例
	// 全等断言会读到而失败）。旧版依赖后续用例的"清全表"段兜底，现已删除，故显式复原本用例数据。
	createdGroupIDs := []int64{}
	t.Cleanup(func() {
		cctx := context.Background()
		for _, gid := range createdGroupIDs {
			_, _ = client.AccountGroup.Delete().
				Where(accountgroup.GroupIDEQ(gid)).Exec(cctx)
			_, _ = client.Group.Delete().
				Where(entgroup.IDEQ(gid)).Exec(cctx)
		}
	})

	// 分组 A：白名单启用，含实供模型 grok-4.7 → 应入选。
	idA := qkF7MustCreateGroup(t, client, "f7-A-realsupply", domain.GroupModelAllowlist{
		Enabled: true, Models: []string{"grok-4.7"},
	})
	createdGroupIDs = append(createdGroupIDs, idA)
	// 分组 B：白名单启用，仅含 "cursor" 字样、不含任何实供模型 → 不应入选（路由不出账号）。
	idB := qkF7MustCreateGroup(t, client, "f7-B-cursor-only", domain.GroupModelAllowlist{
		Enabled: true, Models: []string{"cursor"},
	})
	createdGroupIDs = append(createdGroupIDs, idB)
	// 分组 C：未启用白名单（接受一切）→ 应入选。
	idC := qkF7MustCreateGroup(t, client, "f7-C-disabled", domain.GroupModelAllowlist{
		Enabled: false, Models: nil,
	})
	createdGroupIDs = append(createdGroupIDs, idC)

	svc, _ := qkNewTestService(t, &fakeUpstreamClient{})
	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	got, err := svc.findCursorPlatformGroups(ctx, tx)
	require.NoError(t, err)
	_ = tx.Rollback()

	gotSet := make(map[int64]struct{}, len(got))
	for _, id := range got {
		gotSet[id] = struct{}{}
	}
	require.Contains(t, gotSet, idA, "group with real-supply model must be mounted")
	require.Contains(t, gotSet, idC, "group with disabled allowlist must be mounted")
	require.NotContains(t, gotSet, idB, "group with only 'cursor' string and no real-supply model must NOT be mounted")
}

// TestCURF7_R4_1_ZeroCoverageFailClosed：所有候选分组仅含非实供字样 → 零覆盖，
// 手动拉号在挂载段失败关闭（不得落裸号）。
func TestCURF7_R4_1_ZeroCoverageFailClosed(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// R10-1（CUR-F13）：删除"清全表分组/挂载链接"的隔离段（越界清掉共享库他用例数据）。
	// 改为按 ID 清理本用例自建分组。本用例的零覆盖判定不依赖"清空全表"——findCursorPlatformGroups
	// 在配置勾选非空时按白名单覆盖判定，本用例构造的分组仅含非实供字样，天然零覆盖；共享库即使
	// 残留他用例分组，也由各自用例的 t.Cleanup 负责，不应由本用例越界清理。
	createdGroupIDs := []int64{}
	t.Cleanup(func() {
		cctx := context.Background()
		for _, gid := range createdGroupIDs {
			_, _ = client.AccountGroup.Delete().
				Where(accountgroup.GroupIDEQ(gid)).Exec(cctx)
			_, _ = client.Group.Delete().
				Where(entgroup.IDEQ(gid)).Exec(cctx)
		}
	})

	// 启用 credcrypt，使落库前置的凭证加密通过、流程真正到达挂载段（否则加密未启用会
	// 抢先走 R4-3 失败关闭，掩盖零覆盖判定）。defer 复原未配置态。
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = 0x5a
	}
	_, err := credcrypt.Configure(base64.StdEncoding.EncodeToString(raw), "")
	require.NoError(t, err)
	defer credcrypt.Configure("", "")

	// 仅含 "cursor" / "deepseek" 等字样的分组，不覆盖任何实供模型。
	idZ1 := qkF7MustCreateGroup(t, client, "f7-Z1-cursor-only", domain.GroupModelAllowlist{
		Enabled: true, Models: []string{"cursor"},
	})
	createdGroupIDs = append(createdGroupIDs, idZ1)
	idZ2 := qkF7MustCreateGroup(t, client, "f7-Z2-deepseek", domain.GroupModelAllowlist{
		Enabled: true, Models: []string{"deepseek"},
	})
	createdGroupIDs = append(createdGroupIDs, idZ2)

	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "f7-z@x.com", Platform: "cursor", Type: "oauth",
				Credentials: map[string]any{"access_token": "AT"}},
		}},
	})
	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	err = svc.RefreshAccounts(ctx, qk.ID)
	require.Error(t, err, "zero coverage must fail-closed")
	require.True(t, errors.Is(err, ErrQuotaKeyModelNoCoverage),
		"zero coverage must surface ErrQuotaKeyModelNoCoverage; got=%v", err)
	require.True(t, errors.Is(err, ErrQuotaKeyMountFailed),
		"zero coverage must also surface ErrQuotaKeyMountFailed (non-idempotent boundary); got=%v", err)

	// 失败关闭：号不入库。
	accs, qerr := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, qerr)
	require.Len(t, accs, 0, "zero coverage must not persist any account (no bare account)")
}

// TestCURF7_R4_2_NullConfigReturnsInvalid：settings 值字面 null → GetConfig 返回
// CURSOR_AUTO_ENTRY_CONFIG_INVALID 错误，不得静默成空模型集。
func TestCURF7_R4_2_NullConfigReturnsInvalid(t *testing.T) {
	repo := newFakeSettingRepo()
	repo.data[CursorAutoEntryConfigSettingKey] = "null"
	svc := NewCursorAutoEntryService(repo, newFakeQuotaKeyRepo(), testEncryptor{}, nil, nil, "")

	cfg, err := svc.GetConfig(context.Background())
	require.Error(t, err, "literal null config must error")
	require.Nil(t, cfg, "null config must not return a parsed (zero) config")
	require.Contains(t, err.Error(), "CURSOR_AUTO_ENTRY_CONFIG_INVALID",
		"null config must return the INVALID error type; got=%v", err)
}

// TestCURF7_R4_2_ReconcilerSeesErrorNotEmpty：reconciler 视角 ListAllowedModels 在
// null 配置下返回错误（失败心跳），而非空集成功当成功停轮。
func TestCURF7_R4_2_ReconcilerSeesErrorNotEmpty(t *testing.T) {
	repo := newFakeSettingRepo()
	repo.data[CursorAutoEntryConfigSettingKey] = "null"
	svc := NewCursorAutoEntryService(repo, newFakeQuotaKeyRepo(), testEncryptor{}, nil, nil, "")

	// client 为 nil 安全：selectedEntryModels 先失败返回，ListAllowedModels 不会触及 client。
	w := &quotaKeyModelWater{client: nil, cfgSrc: svc}
	models, err := w.ListAllowedModels(context.Background())
	require.Error(t, err, "reconciler must fail (heartbeat) on invalid config, not succeed with empty set")
	require.Nil(t, models, "reconciler must not treat invalid config as an allowed empty model set")
}

// TestCURF7_R4_2_LegalEmptySetUnchanged：用户全关（合法空集）语义不变——返回配置而非错误。
func TestCURF7_R4_2_LegalEmptySetUnchanged(t *testing.T) {
	repo := newFakeSettingRepo()
	// 合法：显式勾选空集（所有 vendor=false、models=[]），非 null。
	repo.data[CursorAutoEntryConfigSettingKey] = `{"vendors":{},"models":[]}`
	svc := NewCursorAutoEntryService(repo, newFakeQuotaKeyRepo(), testEncryptor{}, nil, nil, "")

	cfg, err := svc.GetConfig(context.Background())
	require.NoError(t, err, "legal empty config must NOT error")
	require.NotNil(t, cfg)
	require.Empty(t, ComputeEntryModels(cfg), "legal empty config yields empty model set (unchanged semantics)")
}

// TestCURF7_R4_3_EncryptionDisabledFailsClosed：credcrypt 未启用时，cursor 拉号落库
// 必须失败关闭——PullBatch 成功但落库阶段失败、事务回滚零账号、key 侧 last_refresh_error
// 含 "encryption" 字样（走插入失败标记路径）。
func TestCURF7_R4_3_EncryptionDisabledFailsClosed(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	// 分组允许实供模型，使挂载可成功（否则更早的零覆盖路径会先拦截，掩盖加密判定）。
	modelM := fix1UniqueModel("f7-r43")
	idR43 := qkF7MustCreateGroup(t, client, "f7-r43-group", domain.GroupModelAllowlist{
		Enabled: true, Models: []string{modelM},
	})
	// R10-1（CUR-F13）：本用例自建分组须自行按 ID 清理，不遗留为共享库污染。
	t.Cleanup(func() {
		cctx := context.Background()
		_, _ = client.AccountGroup.Delete().
			Where(accountgroup.GroupIDEQ(idR43)).Exec(cctx)
		_, _ = client.Group.Delete().
			Where(entgroup.IDEQ(idR43)).Exec(cctx)
	})

	// qkNewTestService 公共夹具默认启用 credcrypt（R6-2 修复：让既有拉号用例可越过加密门槛）；
	// 本用例要验证的是"加密未启用 → 落库失败关闭"，故在其之后显式关闭进程级主密钥
	// （passthrough 态），使 prepareCursorAccountCredentials 命中 !credcrypt.Enabled() 分支。
	// qkNewTestService 的 t.Cleanup 会在用例结束时复位为未配置态，无状态泄漏。
	svc, _ := qkNewTestService(t, &fakeUpstreamClient{
		batch: &UpstreamPullResult{Accounts: []*UpstreamAccount{
			{Name: "f7-r43@x.com", Platform: "cursor", Type: "oauth",
				Credentials: map[string]any{
					"access_token":  "PLAIN_AT",
					"refresh_token": "PLAIN_RT",
				}},
		}},
	})
	if _, derr := credcrypt.Configure("", ""); derr != nil {
		t.Fatalf("disable credcrypt for R4-3 disabled-path: %v", derr)
	}
	defer credcrypt.Configure("", "") // 双重保险：确保复位为未配置态。

	qk := mustCreateQuotaKey(t, client, domain.QuotaKeyStatusActive)

	err := svc.PullAccountsForModel(ctx, qk.ID, modelM)
	require.Error(t, err, "encryption disabled must fail-closed at insert")
	// R4-3：加密未启用 → 落库段失败关闭。内部走 errInsertFailureRollback 路径（事务回滚 +
	// insertFailErr 标记写入），对外按全仓约定上抛 ErrQuotaKeyMountFailed（非幂等边界哨兵，
	// 与 quota_key_service_test.go 的插入失败用例口径一致），故此处断言该哨兵可达。
	require.True(t, errors.Is(err, ErrQuotaKeyMountFailed),
		"encryption-disabled failure must surface ErrQuotaKeyMountFailed (insert-failure-rollback path); got=%v", err)

	// 事务回滚：零账号入库（不留明文裸号）。
	accs, qerr := client.Account.Query().Where(account.QuotaKeyID(qk.ID)).All(ctx)
	require.NoError(t, qerr)
	require.Len(t, accs, 0, "rollback must persist zero accounts when encryption disabled")

	// key 侧 last_refresh_error 含 "encryption" 字样（走插入失败标记路径）。
	got, gerr := client.QuotaKey.Get(ctx, qk.ID)
	require.NoError(t, gerr)
	require.NotNil(t, got.LastRefreshError, "insert-failure marker must be written")
	require.Contains(t, strings.ToLower(*got.LastRefreshError), "encryption",
		"insert-failure marker must mention encryption; got=%q", *got.LastRefreshError)
}
