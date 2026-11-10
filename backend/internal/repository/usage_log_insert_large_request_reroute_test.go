//go:build unit

package repository

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

var (
	usageLogRerouteInsertShapeRe = regexp.MustCompile(`(?s)INSERT INTO usage_logs \((.*?)\) VALUES \((.*?)\)`)
	usageLogReroutePlaceholderRe = regexp.MustCompile(`\$(\d+)`)
)

// captureUsageLogInsertShape 用 sqlmock 抓取 execUsageLogInsertNoResult 实际下发的 SQL，
// 返回 INSERT 列清单与 VALUES 占位符编号，用于断言对账两列的落库位置与参数顺序。
func captureUsageLogInsertShape(t *testing.T, prepared usageLogInsertPrepared) ([]string, []int) {
	t.Helper()

	var captured []string
	matcher := sqlmock.QueryMatcherFunc(func(_, actualSQL string) error {
		captured = append(captured, actualSQL)
		return nil
	})
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectExec("INSERT INTO usage_logs").WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, execUsageLogInsertNoResult(context.Background(), db, prepared))
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, captured, 1)

	m := usageLogRerouteInsertShapeRe.FindStringSubmatch(captured[0])
	require.Len(t, m, 3, "unrecognised INSERT shape:\n%s", captured[0])

	columns := make([]string, 0, len(usageLogInsertArgTypes))
	for _, col := range strings.Split(m[1], ",") {
		if trimmed := strings.TrimSpace(col); trimmed != "" {
			columns = append(columns, trimmed)
		}
	}

	positions := make([]int, 0, len(columns))
	for _, ph := range usageLogReroutePlaceholderRe.FindAllStringSubmatch(m[2], -1) {
		n, convErr := strconv.Atoi(ph[1])
		require.NoError(t, convErr)
		positions = append(positions, n)
	}
	return columns, positions
}

// usageLogInsertColumnIndex 返回列在 INSERT 清单中的下标，并钉住该下标对应的参数类型。
func usageLogInsertColumnIndex(t *testing.T, columns []string, name, wantType string) int {
	t.Helper()

	idx := -1
	for i, col := range columns {
		if col == name {
			idx = i
			break
		}
	}
	require.GreaterOrEqual(t, idx, 0, "column %s missing from INSERT column list", name)
	require.Equal(t, wantType, usageLogInsertArgTypes[idx], "arg type of %s must be %s", name, wantType)
	return idx
}

// parseUsageLogInsertShapeString 从抓取的 INSERT SQL 中解析列清单与 VALUES 占位符编号，
// 用于直接断言 createSingle 路径下发的语句形态。
func parseUsageLogInsertShapeString(t *testing.T, sqlText string) ([]string, []int) {
	t.Helper()
	m := usageLogRerouteInsertShapeRe.FindStringSubmatch(sqlText)
	require.Len(t, m, 3, "unrecognised INSERT shape:\n%s", sqlText)

	columns := make([]string, 0, len(usageLogInsertArgTypes))
	for _, col := range strings.Split(m[1], ",") {
		if trimmed := strings.TrimSpace(col); trimmed != "" {
			columns = append(columns, trimmed)
		}
	}

	positions := make([]int, 0, len(columns))
	for _, ph := range usageLogReroutePlaceholderRe.FindAllStringSubmatch(m[2], -1) {
		n, convErr := strconv.Atoi(ph[1])
		require.NoError(t, convErr)
		positions = append(positions, n)
	}
	return columns, positions
}

// TestUsageLogRepositoryCreateLargeRequestRerouteWiring 覆盖 createSingle 路径下
// 对账两列的落库：列数 = 参数数（与类型表一致），且 estimated_input_tokens /
// large_request_reroute 位置的参数携带原值。
func TestUsageLogRepositoryCreateLargeRequestRerouteWiring(t *testing.T) {
	createdAt := time.Now().UTC()
	log := &service.UsageLog{
		UserID:               1,
		APIKeyID:             2,
		AccountID:            3,
		RequestID:            "client:create-single-reroute",
		Model:                "gpt-5",
		EstimatedInputTokens: 12345,
		LargeRequestReroute:  true,
		CreatedAt:            createdAt,
	}
	prepared := prepareUsageLogInsert(log)
	args := anySliceToDriverValues(prepared.args)

	var captured []string
	db, mock := newSQLCapturingMock(t, &captured)
	repo := &usageLogRepository{sql: db}

	mock.ExpectQuery("INSERT INTO usage_logs").
		WithArgs(args...).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(7), createdAt))

	inserted, err := repo.Create(context.Background(), log)
	require.NoError(t, err)
	require.True(t, inserted)
	require.NoError(t, mock.ExpectationsWereMet())
	require.Len(t, captured, 1)

	require.Len(t, prepared.args, len(usageLogInsertArgTypes))
	columns, positions := parseUsageLogInsertShapeString(t, captured[0])
	require.Len(t, columns, len(usageLogInsertArgTypes), "createSingle INSERT column list must match arg types")
	require.Len(t, positions, len(usageLogInsertArgTypes), "createSingle VALUES placeholders must match arg types")

	estIdx := usageLogInsertColumnIndex(t, columns, "estimated_input_tokens", "integer")
	require.Equal(t, 12345, prepared.args[estIdx], "estimated_input_tokens arg must carry the estimated value")

	rerouteIdx := usageLogInsertColumnIndex(t, columns, "large_request_reroute", "boolean")
	require.Equal(t, true, prepared.args[rerouteIdx], "large_request_reroute arg must carry the flag")
}

// TestPrepareUsageLogInsertLargeRequestRerouteArgsWiring 覆盖对账两列真正落库：
// estimated_input_tokens=12345、large_request_reroute=true 必须出现在列清单中，
// 且对应位置的参数携带原值，占位符与列数严格一致。
func TestPrepareUsageLogInsertLargeRequestRerouteArgsWiring(t *testing.T) {
	prepared := prepareUsageLogInsert(&service.UsageLog{
		UserID:               1,
		APIKeyID:             2,
		AccountID:            3,
		RequestID:            "client:lro-reroute",
		Model:                "gpt-5",
		EstimatedInputTokens: 12345,
		LargeRequestReroute:  true,
		CreatedAt:            time.Now().UTC(),
	})

	require.Len(t, prepared.args, len(usageLogInsertArgTypes))

	columns, positions := captureUsageLogInsertShape(t, prepared)
	require.Len(t, columns, len(usageLogInsertArgTypes), "INSERT column list must match arg types")
	require.Len(t, positions, len(usageLogInsertArgTypes), "VALUES placeholders must match arg types")
	for i, pos := range positions {
		require.Equal(t, i+1, pos, "placeholder $%d must appear in column order", pos)
	}

	estIdx := usageLogInsertColumnIndex(t, columns, "estimated_input_tokens", "integer")
	require.Equal(t, 12345, prepared.args[estIdx], "estimated_input_tokens arg must carry the estimated value")

	rerouteIdx := usageLogInsertColumnIndex(t, columns, "large_request_reroute", "boolean")
	require.Equal(t, true, prepared.args[rerouteIdx], "large_request_reroute arg must carry the flag")
}

// TestPrepareUsageLogInsertLargeRequestRerouteZeroValues 覆盖零值路径：未估算、未重路由
// 的请求落 estimated_input_tokens=0 与 large_request_reroute=false（列默认值语义）。
func TestPrepareUsageLogInsertLargeRequestRerouteZeroValues(t *testing.T) {
	prepared := prepareUsageLogInsert(&service.UsageLog{
		UserID:    1,
		APIKeyID:  2,
		AccountID: 3,
		RequestID: "client:lro-reroute-zero",
		Model:     "gpt-5",
		CreatedAt: time.Now().UTC(),
	})

	require.Len(t, prepared.args, len(usageLogInsertArgTypes))

	columns, _ := captureUsageLogInsertShape(t, prepared)
	require.Len(t, columns, len(usageLogInsertArgTypes), "INSERT column list must match arg types")

	estIdx := usageLogInsertColumnIndex(t, columns, "estimated_input_tokens", "integer")
	require.Equal(t, 0, prepared.args[estIdx], "zero estimated_input_tokens must persist as 0")

	rerouteIdx := usageLogInsertColumnIndex(t, columns, "large_request_reroute", "boolean")
	require.Equal(t, false, prepared.args[rerouteIdx], "absent reroute must persist as false")
}
