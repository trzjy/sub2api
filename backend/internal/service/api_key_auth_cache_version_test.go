package service

import "testing"

func TestAPIKeyService_RejectsV10AuthSnapshotWithoutModelAllowlist(t *testing.T) {
	groupID := int64(9)
	svc := &APIKeyService{}

	apiKey, ok, err := svc.applyAuthCacheEntry("k-legacy-models-list", &APIKeyAuthCacheEntry{
		Snapshot: &APIKeyAuthSnapshot{
			Version:  10,
			APIKeyID: 1,
			UserID:   2,
			GroupID:  &groupID,
			Status:   StatusActive,
			User: APIKeyAuthUserSnapshot{
				ID:          2,
				Status:      StatusActive,
				Role:        RoleUser,
				Balance:     10,
				Concurrency: 3,
			},
			Group: &APIKeyAuthGroupSnapshot{
				ID:               groupID,
				Name:             "openai",
				Platform:         PlatformOpenAI,
				Status:           StatusActive,
				SubscriptionType: SubscriptionTypeStandard,
				RateMultiplier:   1,
			},
		},
	})

	if err != nil {
		t.Fatalf("expected stale snapshot to be ignored without error, got %v", err)
	}
	if ok {
		t.Fatalf("expected v10 auth snapshot to be rejected after model_allowlist was added")
	}
	if apiKey != nil {
		t.Fatalf("expected no API key from stale snapshot, got %#v", apiKey)
	}
}

func TestAPIKeyService_RejectsV15AuthSnapshotWithoutReasoningEffortPolicy(t *testing.T) {
	svc := &APIKeyService{}

	apiKey, ok, err := svc.applyAuthCacheEntry("k-legacy-reasoning-mappings", &APIKeyAuthCacheEntry{
		Snapshot: &APIKeyAuthSnapshot{Version: 15},
	})

	if err != nil {
		t.Fatalf("expected stale snapshot to be ignored without error, got %v", err)
	}
	if ok {
		t.Fatal("expected v15 auth snapshot to be rejected after reasoning effort policy was added")
	}
	if apiKey != nil {
		t.Fatalf("expected no API key from stale snapshot, got %#v", apiKey)
	}
}

// TestAPIKeyService_RejectsV24SnapshotBeforeGroupConcurrency 验证：v25 之前（含 v24）的
// 快照缺少分组并发字段，必须被判定为过期而被拒绝（版本升级强制刷新存量快照）。
func TestAPIKeyService_RejectsV24SnapshotBeforeGroupConcurrency(t *testing.T) {
	svc := &APIKeyService{}
	groupID := int64(7)

	apiKey, ok, err := svc.applyAuthCacheEntry("k-legacy-group-concurrency", &APIKeyAuthCacheEntry{
		Snapshot: &APIKeyAuthSnapshot{
			Version:  24,
			APIKeyID: 1,
			UserID:   2,
			GroupID:  &groupID,
			Status:   StatusActive,
			User: APIKeyAuthUserSnapshot{
				ID:          2,
				Status:      StatusActive,
				Role:        RoleUser,
				Balance:     10,
				Concurrency: 3,
			},
			Group: &APIKeyAuthGroupSnapshot{
				ID:               groupID,
				Name:             "openai",
				Platform:         PlatformOpenAI,
				Status:           StatusActive,
				SubscriptionType: SubscriptionTypeSubscription,
				RateMultiplier:   1,
				Concurrency:      5,
			},
		},
	})

	if err != nil {
		t.Fatalf("expected stale snapshot to be ignored without error, got %v", err)
	}
	if ok {
		t.Fatal("expected v24 auth snapshot to be rejected after group concurrency field was added")
	}
	if apiKey != nil {
		t.Fatalf("expected no API key from stale snapshot, got %#v", apiKey)
	}
}

// TestAPIKeyService_RoundTripsV25GroupConcurrency 验证：当前版本（v25）快照携带分组并发字段，
// 解析出的 APIKey.Group.Concurrency 与快照一致（分组并发字段全链路贯通）。
func TestAPIKeyService_RoundTripsV25GroupConcurrency(t *testing.T) {
	svc := &APIKeyService{}
	groupID := int64(7)

	apiKey, ok, err := svc.applyAuthCacheEntry("k-current-group-concurrency", &APIKeyAuthCacheEntry{
		Snapshot: &APIKeyAuthSnapshot{
			Version:  apiKeyAuthSnapshotVersion,
			APIKeyID: 1,
			UserID:   2,
			GroupID:  &groupID,
			Status:   StatusActive,
			User: APIKeyAuthUserSnapshot{
				ID:          2,
				Status:      StatusActive,
				Role:        RoleUser,
				Balance:     10,
				Concurrency: 3,
			},
			Group: &APIKeyAuthGroupSnapshot{
				ID:               groupID,
				Name:             "openai",
				Platform:         PlatformOpenAI,
				Status:           StatusActive,
				SubscriptionType: SubscriptionTypeSubscription,
				RateMultiplier:   1,
				Concurrency:      5,
			},
		},
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected v25 auth snapshot to be accepted")
	}
	if apiKey == nil || apiKey.Group == nil {
		t.Fatalf("expected API key with group, got %#v", apiKey)
	}
	if apiKey.Group.Concurrency != 5 {
		t.Fatalf("expected group concurrency 5, got %d", apiKey.Group.Concurrency)
	}
}
