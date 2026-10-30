//go:build unit

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func newStandbyHealthRepo(t *testing.T) (*standbyHealthRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &standbyHealthRepository{db: db}, mock
}

func TestStandbyHealthRepo_TTFTBaselineP50(t *testing.T) {
	repo, mock := newStandbyHealthRepo(t)
	from := time.Date(2026, 10, 15, 2, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 29, 2, 0, 0, 0, time.UTC)
	loc, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)

	mock.ExpectQuery(standbyTTFTBaselineSQL).
		WithArgs("deepseek-v4-pro", from, to, "Asia/Shanghai", 10).
		WillReturnRows(sqlmock.NewRows([]string{"baseline_p50", "day_count"}).AddRow(12345.0, 14))

	got, err := repo.TTFTBaselineP50(context.Background(), "deepseek-v4-pro", from, to, loc, 10)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, 12345.0, got.P50Ms)
	require.Equal(t, 14, got.Days)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 天数不足或 p50 为 NULL 时按"基线不可用"返回 nil，交由 evaluate 判 Inconclusive。
func TestStandbyHealthRepo_TTFTBaselineP50_Unavailable(t *testing.T) {
	repo, mock := newStandbyHealthRepo(t)
	from := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(standbyTTFTBaselineSQL).
		WithArgs("kimi-k3", from, to, "UTC", 3).
		WillReturnRows(sqlmock.NewRows([]string{"baseline_p50", "day_count"}).AddRow(nil, 2))

	got, err := repo.TTFTBaselineP50(context.Background(), "kimi-k3", from, to, nil, 3)
	require.NoError(t, err)
	require.Nil(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestStandbyHealthRepo_TTFTWindowSample(t *testing.T) {
	repo, mock := newStandbyHealthRepo(t)
	from := time.Date(2026, 10, 30, 1, 55, 0, 0, time.UTC)
	to := time.Date(2026, 10, 30, 2, 0, 0, 0, time.UTC)

	mock.ExpectQuery(standbyTTFTWindowSQL).
		WithArgs("glm-5.3", from, to).
		WillReturnRows(sqlmock.NewRows([]string{"p50", "samples"}).AddRow(8800.0, 42))

	got, err := repo.TTFTWindowSample(context.Background(), "glm-5.3", from, to)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, 8800.0, got.P50Ms)
	require.Equal(t, 42, got.Samples)

	mock.ExpectQuery(standbyTTFTWindowSQL).
		WithArgs("glm-5.3", from, to).
		WillReturnRows(sqlmock.NewRows([]string{"p50", "samples"}).AddRow(nil, 0))
	got, err = repo.TTFTWindowSample(context.Background(), "glm-5.3", from, to)
	require.NoError(t, err)
	require.Nil(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 窗口请求数 = 成功用量行 + 429/529 错误行（分子的超集，避免 recovered 行双计）。
func TestStandbyHealthRepo_UpstreamThrottleStats(t *testing.T) {
	repo, mock := newStandbyHealthRepo(t)
	from := time.Date(2026, 10, 30, 1, 55, 0, 0, time.UTC)
	to := time.Date(2026, 10, 30, 2, 0, 0, 0, time.UTC)

	mock.ExpectQuery(standbyThrottleStatsSQL).
		WithArgs("deepseek-v4-pro", from, to).
		WillReturnRows(sqlmock.NewRows([]string{"throttle_count", "usage_count"}).AddRow(18, 62))

	got, err := repo.UpstreamThrottleStats(context.Background(), "deepseek-v4-pro", from, to)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, 18, got.ThrottleCount)
	require.Equal(t, 80, got.RequestCount)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestStandbyHealthRepo_ListAccountsForStandby(t *testing.T) {
	repo, mock := newStandbyHealthRepo(t)
	expires := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(standbyAccountsSQL).
		WithArgs("deepseek").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "platform", "type", "status", "schedulable", "expires_at",
			"auto_pause_on_expired", "rate_limit_reset_at", "overload_until",
			"temp_unschedulable_until", "extra",
		}).
			AddRow(11, "deepseek", "apikey", "active", true, expires, true, nil, nil, nil,
				[]byte(`{"quota_limit":10,"quota_used":1}`)).
			AddRow(12, "deepseek", "oauth", "disabled", false, nil, false, nil, nil, nil,
				[]byte(`{}`)))

	accounts, err := repo.ListAccountsForStandby(context.Background(), "deepseek")
	require.NoError(t, err)
	require.Len(t, accounts, 2)
	require.True(t, accounts[0].IsSchedulable())
	require.NotNil(t, accounts[0].ExpiresAt)
	require.InDelta(t, 10.0, accounts[0].GetQuotaLimit(), 0.0001)
	require.False(t, accounts[1].IsSchedulable())
	require.NoError(t, mock.ExpectationsWereMet())
}
