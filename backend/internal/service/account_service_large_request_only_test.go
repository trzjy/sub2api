//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// largeRequestOnlyCreateRepo 只实现 Create，其余方法经接口嵌入留空（被调用即 panic）。
type largeRequestOnlyCreateRepo struct {
	AccountRepository
	created []*Account
}

func (r *largeRequestOnlyCreateRepo) Create(_ context.Context, account *Account) error {
	r.created = append(r.created, account)
	return nil
}

// TestAccountServiceCreateDropsLargeRequestOnly 验证通用建号入口剥离「仅接大请求」键：
// 该开关只能经 admin 建号 / SetAccountLargeRequestOnly（含池覆盖校验）设置，
// 通用 AccountService.Create 携带该键必须被丢弃，其余 extra 键原样保留。
func TestAccountServiceCreateDropsLargeRequestOnly(t *testing.T) {
	repo := &largeRequestOnlyCreateRepo{}
	svc := &AccountService{accountRepo: repo}

	account, err := svc.Create(context.Background(), CreateAccountRequest{
		Name:     "lro-create",
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra: map[string]any{
			LargeRequestOnlyExtraKey: true,
			"custom":                 "value",
		},
	})

	require.NoError(t, err)
	require.Len(t, repo.created, 1)
	require.NotContains(t, account.Extra, LargeRequestOnlyExtraKey)
	require.NotContains(t, repo.created[0].Extra, LargeRequestOnlyExtraKey)
	require.Equal(t, "value", account.Extra["custom"], "其它 extra 键必须原样保留")
}

// TestAccountServiceCreateLoneLargeRequestOnlyKey 验证请求仅含该键时建号成功且 Extra 为空。
func TestAccountServiceCreateLoneLargeRequestOnlyKey(t *testing.T) {
	repo := &largeRequestOnlyCreateRepo{}
	svc := &AccountService{accountRepo: repo}

	account, err := svc.Create(context.Background(), CreateAccountRequest{
		Name:     "lro-create-lone",
		Platform: PlatformOpenAI,
		Type:     AccountTypeAPIKey,
		Extra:    map[string]any{LargeRequestOnlyExtraKey: false},
	})

	require.NoError(t, err)
	require.NotContains(t, account.Extra, LargeRequestOnlyExtraKey)
	require.Empty(t, account.Extra)
}
