package repository

// D-QL-007 F2 定向测试（repository 层，照 account_repo_e12_test.go / e8 sqlmock 形态）：
// DeleteModelRateLimitsMetaKeys 的 SQL 形态与参数断言——
//   - 单条 UPDATE，jsonb_set + `extra->'model_rate_limits_meta' - $1::text[]`
//     （jsonb 数组按键原子删除），updated_at = NOW()，WHERE id AND deleted_at IS NULL；
//   - keys 参数按传入顺序作为 text[] 绑定（pq.StringArray）；
//   - RowsAffected == 0 → ErrAccountNotFound；
//   - keys 为空 → 不发 SQL，直接 (0, nil)。

import (
	"context"
	"database/sql/driver"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	_ "github.com/Wei-Shaw/sub2api/ent/runtime"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// keysArrayMatcher 断言 $1 绑定值为 pq.StringArray(keys) 的期望 text[] 字面量
// （lib/pq 输出形如 {a,b}，顺序与传入一致）。
type keysArrayMatcher struct{ want string }

// Match 兼容 sqlmock 收到的多种 text[] 绑定形态：lib/pq 的 {a,b} 字面量、
// 驱动转成 []string 的值、以及 pq.StringArray 的 Valuer 展开。文本形态剥掉
// 元素间空格差异后按字典序无关的逗号分割比较（顺序与传入一致）。
func (m keysArrayMatcher) Match(v driver.Value) bool {
	normalize := func(s string) []string {
		s = strings.TrimSpace(s)
		s = strings.TrimPrefix(s, "{")
		s = strings.TrimSuffix(s, "}")
		if s == "" {
			return nil
		}
		parts := strings.Split(s, ",")
		for i := range parts {
			parts[i] = strings.Trim(strings.TrimSpace(parts[i]), "\x22")
		}
		return parts
	}
	switch val := v.(type) {
	case []byte:
		return strings.Join(normalize(string(val)), ",") == strings.Join(normalize(m.want), ",")
	case string:
		return strings.Join(normalize(val), ",") == strings.Join(normalize(m.want), ",")
	case []string:
		return strings.Join(val, ",") == strings.Join(normalize(m.want), ",")
	default:
		return false
	}
}

func TestStaleExtraRepoDeleteMetaKeys_ExecutesAtomicKeyDelete(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	// 单条 UPDATE：jsonb_set + `-` 数组按键删除，命中键集合按传入顺序绑定。
	mock.ExpectExec(regexp.QuoteMeta("UPDATE accounts SET")).
		WithArgs(
			keysArrayMatcher{want: "{tokenharbor_free_tier_glm-5.3-flash,tokenharbor_free_tier_x}"},
			int64(207),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	affected, err := repo.DeleteModelRateLimitsMetaKeys(context.Background(), 207, []string{
		"tokenharbor_free_tier_glm-5.3-flash", "tokenharbor_free_tier_x",
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), affected)
	require.NoError(t, mock.ExpectationsWereMet())
}

// D-QL-009 F1：meta 桶为 JSONB 字面量 null / 非对象时，SQL 必须为 jsonb_typeof
// 归一形态（CASE WHEN ... = 'object' + COALESCE 收口 '{}'::jsonb），否则生产 PG16
// 上 `'{"model_rate_limits_meta":null}'::jsonb->'model_rate_limits_meta' - keys`
// 对 scalar 执行 jsonb `-` 删除报 "cannot delete from scalar"，批量清理在该账号中止。
func TestStaleExtraRepoDeleteMetaKeys_SQLNormalizesJSONBNullBucket(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	// SQL 形态断言：value 表达式必须先 jsonb_typeof 判定对象、CASE 未命中落
	// COALESCE 空对象收口，再执行 `-` 数组按键删除；参数仍按 text[] 绑定。
	// （sqlmock 默认 QueryMatcherRegexp：期望值按正则对实际 SQL 匹配。）
	mock.ExpectExec(`COALESCE\(\s*CASE WHEN jsonb_typeof\(extra->'model_rate_limits_meta'\) = 'object'\s*THEN extra->'model_rate_limits_meta' END,\s*'\{\}'::jsonb\s*\) - \$1::text\[\]`).
		WithArgs(
			keysArrayMatcher{want: "{tokenharbor_free_tier_glm-5.3-flash}"},
			int64(209),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	affected, err := repo.DeleteModelRateLimitsMetaKeys(context.Background(), 209, []string{
		"tokenharbor_free_tier_glm-5.3-flash",
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), affected)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestStaleExtraRepoDeleteMetaKeys_ZeroRowsReturnsNotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE accounts SET")).
		WithArgs(keysArrayMatcher{want: "{tokenharbor_free_tier_x}"}, int64(404)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	affected, err := repo.DeleteModelRateLimitsMetaKeys(context.Background(), 404, []string{"tokenharbor_free_tier_x"})
	require.Error(t, err)
	require.ErrorIs(t, err, service.ErrAccountNotFound)
	require.Zero(t, affected)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestStaleExtraRepoDeleteMetaKeys_EmptyKeysIsNoOpWithoutSQL(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := dbent.NewClient(dbent.Driver(driver))
	t.Cleanup(func() { _ = client.Close() })
	repo := newAccountRepositoryWithSQL(client, db, nil)

	// 空键集合：不发任何 SQL（无 Expect 期望，出现即失败）。
	affected, err := repo.DeleteModelRateLimitsMetaKeys(context.Background(), 207, nil)
	require.NoError(t, err)
	require.Zero(t, affected)

	affected, err = repo.DeleteModelRateLimitsMetaKeys(context.Background(), 207, []string{})
	require.NoError(t, err)
	require.Zero(t, affected)
	require.NoError(t, mock.ExpectationsWereMet(), "empty keys must not execute any SQL")
}
