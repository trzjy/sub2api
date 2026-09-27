package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/stretchr/testify/require"
)

const (
	accountModelCapabilitySelectSQL = `(?s)SELECT id, account_id, upstream_model, protocol, supports_vision, source, detected_at, updated_at\s+FROM account_model_capabilities`
	accountModelCapabilityInsertSQL = `(?s)INSERT INTO account_model_capabilities\s+\(.*\)\s+VALUES\s+\(.*\)\s+ON CONFLICT \(account_id, upstream_model, protocol\)\s+DO UPDATE SET.*RETURNING id, account_id, upstream_model, protocol, supports_vision, source, detected_at, updated_at`
	accountModelCapabilityDeleteSQL = `(?s)DELETE FROM account_model_capabilities\s+WHERE account_id = \$1`
)

func newAccountModelCapabilityRepo(t *testing.T) (*accountModelCapabilityRepository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return &accountModelCapabilityRepository{db: db}, mock
}

func capabilityRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "account_id", "upstream_model", "protocol",
		"supports_vision", "source", "detected_at", "updated_at",
	})
}

func TestAccountModelCapabilityRepo_Get_Hit(t *testing.T) {
	ctx := context.Background()
	repo, mock := newAccountModelCapabilityRepo(t)

	detectedAt := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	rows := capabilityRows().AddRow(
		1, 137, "deepseek-v4.1-flash", model.CapabilityProtocolChatCompletions,
		true, model.CapabilitySourceDetect, detectedAt, updatedAt,
	)
	mock.ExpectQuery(accountModelCapabilitySelectSQL).
		WithArgs(int64(137), "deepseek-v4.1-flash", "chat_completions").
		WillReturnRows(rows)

	got, err := repo.Get(ctx, 137, "deepseek-v4.1-flash", "chat_completions")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, int64(137), got.AccountID)
	require.Equal(t, "deepseek-v4.1-flash", got.UpstreamModel)
	require.Equal(t, model.CapabilityProtocolChatCompletions, got.Protocol)
	require.True(t, got.SupportsVision)
	require.Equal(t, model.CapabilitySourceDetect, got.Source)
	require.NotNil(t, got.DetectedAt)
	require.Equal(t, detectedAt, *got.DetectedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountModelCapabilityRepo_Get_NoRowsReturnsNil(t *testing.T) {
	ctx := context.Background()
	repo, mock := newAccountModelCapabilityRepo(t)

	mock.ExpectQuery(accountModelCapabilitySelectSQL).
		WithArgs(int64(137), "deepseek-v4.1-flash", "responses").
		WillReturnError(sql.ErrNoRows)

	got, err := repo.Get(ctx, 137, "deepseek-v4.1-flash", "responses")
	require.NoError(t, err)
	require.Nil(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountModelCapabilityRepo_Get_DBError(t *testing.T) {
	ctx := context.Background()
	repo, mock := newAccountModelCapabilityRepo(t)

	mock.ExpectQuery(accountModelCapabilitySelectSQL).
		WithArgs(int64(1), "m", "anthropic").
		WillReturnError(errors.New("connection refused"))

	_, err := repo.Get(ctx, 1, "m", "anthropic")
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountModelCapabilityRepo_Upsert_InsertsAndReturns(t *testing.T) {
	ctx := context.Background()
	repo, mock := newAccountModelCapabilityRepo(t)

	detectedAt := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2026, 9, 26, 8, 1, 0, 0, time.UTC)
	rows := capabilityRows().AddRow(
		42, 137, "deepseek-v4.1-flash", "chat_completions",
		true, "detect", detectedAt, updatedAt,
	)
	mock.ExpectQuery(accountModelCapabilityInsertSQL).
		WithArgs(int64(137), "deepseek-v4.1-flash", "chat_completions", true, "detect", detectedAt).
		WillReturnRows(rows)

	rec := &model.AccountModelCapability{
		AccountID:      137,
		UpstreamModel:  "deepseek-v4.1-flash",
		Protocol:       model.CapabilityProtocolChatCompletions,
		SupportsVision: true,
		Source:         model.CapabilitySourceDetect,
		DetectedAt:     &detectedAt,
	}
	got, err := repo.Upsert(ctx, rec)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, int64(42), got.ID)
	require.Equal(t, int64(137), got.AccountID)
	require.True(t, got.SupportsVision)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ① manual 行被检测 Upsert 命中后仍为 manual 原值（检测不得覆盖人工标记）
func TestAccountModelCapabilityRepo_Upsert_DetectKeepsManual(t *testing.T) {
	ctx := context.Background()
	repo, mock := newAccountModelCapabilityRepo(t)

	manualDetectedAt := time.Date(2026, 9, 25, 8, 0, 0, 0, time.UTC)
	detectDetectedAt := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	// 现有 manual 行：supports_vision=false
	rows := capabilityRows().AddRow(
		1, 137, "deepseek-v4.1-flash", "chat_completions",
		false, model.CapabilitySourceManual, manualDetectedAt, time.Now(),
	)
	// 检测 Upsert 试图写入 supports_vision=true, source='detect'
	mock.ExpectQuery(accountModelCapabilityInsertSQL).
		WithArgs(int64(137), "deepseek-v4.1-flash", "chat_completions", true, model.CapabilitySourceDetect, detectDetectedAt).
		WillReturnRows(rows)

	rec := &model.AccountModelCapability{
		AccountID:      137,
		UpstreamModel:  "deepseek-v4.1-flash",
		Protocol:       model.CapabilityProtocolChatCompletions,
		SupportsVision: true,
		Source:         model.CapabilitySourceDetect,
		DetectedAt:     &detectDetectedAt,
	}
	got, err := repo.Upsert(ctx, rec)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, model.CapabilitySourceManual, got.Source)
	require.False(t, got.SupportsVision, "manual 行的 supports_vision 不得被检测覆盖")
	require.Equal(t, manualDetectedAt, *got.DetectedAt, "manual 行的 detected_at 不得被检测覆盖")
	require.NoError(t, mock.ExpectationsWereMet())
}

// ② detect 行被检测 Upsert 命中后正常刷新
func TestAccountModelCapabilityRepo_Upsert_DetectRefreshesDetect(t *testing.T) {
	ctx := context.Background()
	repo, mock := newAccountModelCapabilityRepo(t)

	newDetectedAt := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	// 现有 detect 行：supports_vision=false
	rows := capabilityRows().AddRow(
		1, 137, "deepseek-v4.1-flash", "chat_completions",
		true, model.CapabilitySourceDetect, newDetectedAt, time.Now(),
	)
	// 检测 Upsert 写入 supports_vision=true, source='detect'
	mock.ExpectQuery(accountModelCapabilityInsertSQL).
		WithArgs(int64(137), "deepseek-v4.1-flash", "chat_completions", true, model.CapabilitySourceDetect, newDetectedAt).
		WillReturnRows(rows)

	rec := &model.AccountModelCapability{
		AccountID:      137,
		UpstreamModel:  "deepseek-v4.1-flash",
		Protocol:       model.CapabilityProtocolChatCompletions,
		SupportsVision: true,
		Source:         model.CapabilitySourceDetect,
		DetectedAt:     &newDetectedAt,
	}
	got, err := repo.Upsert(ctx, rec)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, model.CapabilitySourceDetect, got.Source)
	require.True(t, got.SupportsVision, "detect 行应被检测刷新")
	require.Equal(t, newDetectedAt, *got.DetectedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ③ Override（显式 manual 覆盖）可覆盖已有 manual 行
func TestAccountModelCapabilityRepo_Upsert_OverrideOverwritesManual(t *testing.T) {
	ctx := context.Background()
	repo, mock := newAccountModelCapabilityRepo(t)

	// 现有 manual 行：supports_vision=false
	rows := capabilityRows().AddRow(
		1, 137, "deepseek-v4.1-flash", "chat_completions",
		true, model.CapabilitySourceManual, nil, time.Now(),
	)
	// 显式覆盖 Upsert 写入 supports_vision=true, source='manual'，无 detected_at
	mock.ExpectQuery(accountModelCapabilityInsertSQL).
		WithArgs(int64(137), "deepseek-v4.1-flash", "chat_completions", true, model.CapabilitySourceManual, nil).
		WillReturnRows(rows)

	rec := &model.AccountModelCapability{
		AccountID:      137,
		UpstreamModel:  "deepseek-v4.1-flash",
		Protocol:       model.CapabilityProtocolChatCompletions,
		SupportsVision: true,
		Source:         model.CapabilitySourceManual,
		DetectedAt:     nil,
	}
	got, err := repo.Upsert(ctx, rec)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, model.CapabilitySourceManual, got.Source)
	require.True(t, got.SupportsVision, "override 应覆盖 manual 行的 supports_vision")
	require.Nil(t, got.DetectedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountModelCapabilityRepo_ListByAccount(t *testing.T) {
	ctx := context.Background()
	repo, mock := newAccountModelCapabilityRepo(t)

	updatedAt := time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC)
	rows := capabilityRows().
		AddRow(1, 137, "deepseek-v4.1-flash", "chat_completions", true, "detect", nil, updatedAt).
		AddRow(2, 137, "deepseek-v4.1-flash", "responses", true, "manual", nil, updatedAt)
	mock.ExpectQuery(accountModelCapabilitySelectSQL).
		WithArgs(int64(137)).
		WillReturnRows(rows)

	got, err := repo.ListByAccount(ctx, 137)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "chat_completions", got[0].Protocol)
	require.Equal(t, "responses", got[1].Protocol)
	require.Nil(t, got[0].DetectedAt)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountModelCapabilityRepo_ListByAccount_EmptyIsNotNil(t *testing.T) {
	ctx := context.Background()
	repo, mock := newAccountModelCapabilityRepo(t)

	mock.ExpectQuery(accountModelCapabilitySelectSQL).
		WithArgs(int64(999)).
		WillReturnRows(capabilityRows())

	got, err := repo.ListByAccount(ctx, 999)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Empty(t, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestAccountModelCapabilityRepo_DeleteByAccount(t *testing.T) {
	ctx := context.Background()
	repo, mock := newAccountModelCapabilityRepo(t)

	mock.ExpectExec(accountModelCapabilityDeleteSQL).
		WithArgs(int64(137)).
		WillReturnResult(driver.RowsAffected(3))

	err := repo.DeleteByAccount(ctx, 137)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
