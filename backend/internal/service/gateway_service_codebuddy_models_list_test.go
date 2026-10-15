package service

import (
	"context"
	"testing"
	"time"

	gocache "github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
)

// codeBuddySnapshotAccount 构造一个 codebuddy 账号：无 model_mapping，仅有
// 上游模型目录快照（生产 codebuddy 账号的典型形态）。
func codeBuddySnapshotAccount(id int64, models map[string]UpstreamModelMetadata) Account {
	acc := Account{
		ID:          id,
		Platform:    PlatformCodeBuddy,
		Extra:       map[string]any{},
		Credentials: map[string]any{},
	}
	acc.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
		Source:   "upstream",
		SyncedAt: "2026-10-15T11:46:42Z",
		Models:   models,
	})
	return acc
}

func newCodeBuddyModelsListService(repo AccountRepository) *GatewayService {
	return &GatewayService{
		accountRepo:        repo,
		modelsListCache:    gocache.New(time.Minute, time.Minute),
		modelsListCacheTTL: time.Minute,
	}
}

// TestGetAvailableModels_CodeBuddyUsesUpstreamSnapshotWithoutModelMapping 钉住
// 缺陷修复：platform=codebuddy 且账号无 model_mapping 时，公开模型列表必须来自
// accounts.extra.upstream_model_metadata 快照（含 space-bunny），而不是空列表。
// 空列表会让分组白名单求交后返回 0 个模型。
func TestGetAvailableModels_CodeBuddyUsesUpstreamSnapshotWithoutModelMapping(t *testing.T) {
	groupID := int64(109)
	repo := &modelsListAccountRepoStub{
		byGroup: map[int64][]Account{
			groupID: {
				codeBuddySnapshotAccount(256, map[string]UpstreamModelMetadata{
					"space-bunny": {ID: "space-bunny"},
					"hy3":         {ID: "hy3"},
				}),
				// 同组其他 codebuddy 账号无快照：不得因此污染列表，也不得让整体返回空。
				{ID: 257, Platform: PlatformCodeBuddy, Extra: map[string]any{}, Credentials: map[string]any{}},
			},
		},
	}
	svc := newCodeBuddyModelsListService(repo)

	models := svc.GetAvailableModels(context.Background(), &groupID, PlatformCodeBuddy)
	require.Equal(t, []string{"hy3", "space-bunny"}, models,
		"codebuddy 无 model_mapping 时列表须取自上游快照，且排序稳定")
}

// TestGetAvailableModels_CodeBuddySnapshotTrimmedAndEmptyKeys 钉住快照键的
// 归一化：空白键不进入列表（避免污染公开目录）。
func TestGetAvailableModels_CodeBuddySnapshotTrimmedAndEmptyKeys(t *testing.T) {
	groupID := int64(109)
	repo := &modelsListAccountRepoStub{
		byGroup: map[int64][]Account{
			groupID: {
				codeBuddySnapshotAccount(256, map[string]UpstreamModelMetadata{
					"space-bunny": {ID: "space-bunny"},
					"":            {ID: ""},
					"   ":         {ID: "   "},
				}),
			},
		},
	}
	svc := newCodeBuddyModelsListService(repo)

	models := svc.GetAvailableModels(context.Background(), &groupID, PlatformCodeBuddy)
	require.Equal(t, []string{"space-bunny"}, models)
}

// TestGetAvailableModels_CodeBuddySnapshotJoinsModelMapping 钉住并集语义：
// codebuddy 账号同时有 model_mapping 与快照时，两者都进入列表。
func TestGetAvailableModels_CodeBuddySnapshotJoinsModelMapping(t *testing.T) {
	groupID := int64(109)
	acc := codeBuddySnapshotAccount(256, map[string]UpstreamModelMetadata{
		"space-bunny": {ID: "space-bunny"},
	})
	acc.Credentials["model_mapping"] = map[string]any{"legacy-cb": "legacy-cb"}

	repo := &modelsListAccountRepoStub{
		byGroup: map[int64][]Account{groupID: {acc}},
	}
	svc := newCodeBuddyModelsListService(repo)

	models := svc.GetAvailableModels(context.Background(), &groupID, PlatformCodeBuddy)
	require.Equal(t, []string{"legacy-cb", "space-bunny"}, models)
}

// TestGetAvailableModels_CodeBuddyNoSnapshotStillReturnsNil 钉住失败闭合：
// codebuddy 账号既无 mapping 也无快照时返回 nil，由 handler 走「空列表」分支，
// 不得回落到 Claude 默认模型。
func TestGetAvailableModels_CodeBuddyNoSnapshotStillReturnsNil(t *testing.T) {
	groupID := int64(109)
	repo := &modelsListAccountRepoStub{
		byGroup: map[int64][]Account{
			groupID: {
				{ID: 257, Platform: PlatformCodeBuddy, Extra: map[string]any{}, Credentials: map[string]any{}},
			},
		},
	}
	svc := newCodeBuddyModelsListService(repo)

	require.Nil(t, svc.GetAvailableModels(context.Background(), &groupID, PlatformCodeBuddy))
}

// TestGetAvailableModels_NonCodeBuddyIgnoresUpstreamSnapshot 钉住平台隔离：
// 其他平台的账号即使带 codebuddy 上游快照，也不得把快照键并入其公开列表
// （本次改动只影响 codebuddy 分支）。
func TestGetAvailableModels_NonCodeBuddyIgnoresUpstreamSnapshot(t *testing.T) {
	groupID := int64(34)
	acc := Account{
		ID:          1,
		Platform:    PlatformAnthropic,
		Extra:       map[string]any{},
		Credentials: map[string]any{"model_mapping": map[string]any{"claude-sonnet-5": "claude-sonnet-5"}},
	}
	acc.SetUpstreamModelMetadataSnapshot(UpstreamModelMetadataSnapshot{
		Source:   "upstream",
		SyncedAt: "2026-10-15T11:46:42Z",
		Models:   map[string]UpstreamModelMetadata{"space-bunny": {ID: "space-bunny"}},
	})

	repo := &modelsListAccountRepoStub{
		byGroup: map[int64][]Account{groupID: {acc}},
	}
	svc := newCodeBuddyModelsListService(repo)

	models := svc.GetAvailableModels(context.Background(), &groupID, PlatformAnthropic)
	require.Equal(t, []string{"claude-sonnet-5"}, models,
		"非 codebuddy 平台的行为须与改动前一致：只取 model_mapping")
}
