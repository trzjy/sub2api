//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/stretchr/testify/require"
)

type visibilityUserRepo struct {
	UserRepository
	user *User
	err  error
}

func (r *visibilityUserRepo) GetByID(context.Context, int64) (*User, error) { return r.user, r.err }

type visibilitySubRepo struct {
	UserSubscriptionRepository
	subscriptions []UserSubscription
	err           error
	calls         int
}

func (r *visibilitySubRepo) ListActiveByUserID(_ context.Context, userID int64) ([]UserSubscription, error) {
	r.calls++
	// Match the repository's active-status and expiry predicates.
	active := make([]UserSubscription, 0)
	for _, sub := range r.subscriptions {
		if sub.UserID == userID && sub.IsActive() {
			active = append(active, sub)
		}
	}
	return active, r.err
}

type visibilityGroupRepo struct {
	GroupRepository
	groups []Group
}

func (r *visibilityGroupRepo) ListActive(context.Context) ([]Group, error) { return r.groups, nil }

func TestGetUserGroupVisibilityIncludesActiveSubscriptions(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrestricted", true: "restricted"}[restricted], func(t *testing.T) {
			now := time.Now()
			subs := &visibilitySubRepo{subscriptions: []UserSubscription{
				{UserID: 1, GroupID: 42, Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour)},
				{UserID: 1, GroupID: 43, Status: SubscriptionStatusActive, ExpiresAt: now.Add(-time.Hour)},
				{UserID: 1, GroupID: 44, Status: "expired", ExpiresAt: now.Add(time.Hour)},
				{UserID: 2, GroupID: 45, Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour)},
			}}
			svc := &APIKeyService{
				userRepo:    &visibilityUserRepo{user: &User{ID: 1, AllowedGroups: []int64{7}, RestrictPublicGroups: restricted}},
				userSubRepo: subs,
				groupRepo:   &visibilityGroupRepo{groups: []Group{{ID: 42, IsExclusive: true, SubscriptionType: "subscription"}}},
			}
			available, err := svc.GetAvailableGroups(context.Background(), 1)
			require.NoError(t, err)
			require.Len(t, available, 1)
			visible, restrict, err := svc.GetUserGroupVisibility(context.Background(), 1)
			require.NoError(t, err)
			require.Equal(t, restricted, restrict)
			require.Equal(t, map[int64]struct{}{7: {}, 42: {}}, visible)
			require.Contains(t, visible, available[0].ID, "a subscribed group that can be bound must be visible")
		})
	}
}

func TestGetUserGroupVisibilityEmptyAndErrors(t *testing.T) {
	failure := errors.New("repository unavailable")
	for _, tc := range []struct {
		name            string
		userErr, subErr error
	}{
		{name: "empty"}, {name: "user failure", userErr: failure}, {name: "subscription failure", subErr: failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subs := &visibilitySubRepo{err: tc.subErr}
			svc := &APIKeyService{userRepo: &visibilityUserRepo{user: &User{ID: 1}, err: tc.userErr}, userSubRepo: subs}
			got, _, err := svc.GetUserGroupVisibility(context.Background(), 1)
			if tc.userErr != nil || tc.subErr != nil {
				require.ErrorIs(t, err, failure)
				require.Nil(t, got, "repository failures must not become anonymous visibility")
			} else {
				require.NoError(t, err)
				require.NotNil(t, got, "an empty logged-in user must not become anonymous")
				require.Empty(t, got)
			}
			if tc.userErr != nil {
				require.Zero(t, subs.calls)
			}
		})
	}
}

// TestGetAvailableGroupsFiltersInternalSupplyPlatforms 验证 F1：GetAvailableGroups
// 在结果组装循环中剔除 platform 命中 IsInternalSupplyPlatform 的分组（codebuddy/muse/
// cursor），普通分组保留；composite 订阅分组（非内部供给平台）仍随有效订阅返回。
func TestGetAvailableGroupsFiltersInternalSupplyPlatforms(t *testing.T) {
	now := time.Now()
	// 用户对 composite 订阅分组（group 100）有有效订阅；其余为公开分组。
	subs := &visibilitySubRepo{subscriptions: []UserSubscription{
		{UserID: 1, GroupID: 100, Status: SubscriptionStatusActive, ExpiresAt: now.Add(time.Hour)},
	}}
	groups := []Group{
		{ID: 1, Name: "openai-public", Platform: "openai"},                       // 普通公开分组 -> 保留
		{ID: 2, Name: "codebuddy-internal", Platform: "codebuddy"},               // 内部供给 -> 剔除
		{ID: 3, Name: "muse-internal", Platform: "muse"},                         // 内部供给 -> 剔除
		{ID: 4, Name: "cursor-internal", Platform: "cursor"},                     // 内部供给 -> 剔除
		{ID: 100, Name: "composite-sub", Platform: "composite", SubscriptionType: "subscription"}, // 有效订阅 -> 保留
	}
	svc := &APIKeyService{
		userRepo:    &visibilityUserRepo{user: &User{ID: 1}},
		userSubRepo: subs,
		groupRepo:   &visibilityGroupRepo{groups: groups},
	}

	got, err := svc.GetAvailableGroups(context.Background(), 1)
	require.NoError(t, err)

	gotIDs := make([]int64, 0, len(got))
	for _, g := range got {
		gotIDs = append(gotIDs, g.ID)
		// 负向断言：任何内部供给平台都不应出现在结果中。
		require.False(t, domain.IsInternalSupplyPlatform(g.Platform),
			"internal supply platform %q must be filtered out", g.Platform)
	}
	// 正向断言：普通公开分组与 composite 订阅分组保留；内部供给平台全剔除。
	require.ElementsMatch(t, []int64{1, 100}, gotIDs)
}
