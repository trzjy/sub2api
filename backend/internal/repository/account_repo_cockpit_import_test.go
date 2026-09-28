package repository

import (
	"strings"
	"testing"

	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	dbent "github.com/Wei-Shaw/sub2api/ent"
	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// TestAccountUIDFieldGenerated 验证 ent codegen 确实生成了 uid 字段（迁移 262 的模型对齐）。
func TestAccountUIDFieldGenerated(t *testing.T) {
	require.Equal(t, "uid", dbaccount.FieldUID)
	// 谓词构造不应 panic，且能生成合法 SQL 片段。
	p := dbaccount.UID("abc123")
	require.NotNil(t, p)
}

// TestCockpitImportCommitDedup 验证同 (platform, uid) 批次内去重、保留末值、不同键各自保留。
func TestCockpitImportCommitDedup(t *testing.T) {
	in := []service.CockpitImportAccountPayload{
		{Platform: "claude", UID: "u1", Name: "first", Credentials: map[string]any{"api_key": "a"}},
		{Platform: "claude", UID: "u1", Name: "second", Credentials: map[string]any{"api_key": "b"}}, // 同键，取末值
		{Platform: "claude", UID: "u2", Name: "third", Credentials: map[string]any{"api_key": "c"}},
		{Platform: "openai", UID: "u1", Name: "fourth", Credentials: map[string]any{"api_key": "d"}}, // 不同 platform 视为不同键
	}
	out := dedupCockpitPayloads(in)
	require.Len(t, out, 3)

	byKey := map[cockpitImportKey]service.CockpitImportAccountPayload{}
	for _, p := range out {
		byKey[cockpitImportKey{p.Platform, p.UID}] = p
	}
	require.Equal(t, "second", byKey[cockpitImportKey{"claude", "u1"}].Name, "同键应保留末值")
	require.Equal(t, "third", byKey[cockpitImportKey{"claude", "u2"}].Name)
	require.Equal(t, "fourth", byKey[cockpitImportKey{"openai", "u1"}].Name)

	// 空切片安全。
	require.Nil(t, dedupCockpitPayloads(nil))
}

// TestCockpitImportCommitConstraintClassification 验证双路径约束判定（同 redeem_code_repo.go:379 惯例）。
func TestCockpitImportCommitConstraintClassification(t *testing.T) {
	// 路径一：pq 23505 唯一约束 → isUniqueViolation 命中。
	require.True(t, cockpitImportIsConstraintConflict(&pq.Error{Code: "23505"}))
	// 路径二：ent 包装的 ConstraintError → dbent.IsConstraintError 命中。
	require.True(t, cockpitImportIsConstraintConflict(&dbent.ConstraintError{}))
	// 非约束错误 → 不命中。
	require.False(t, cockpitImportIsConstraintConflict(errPlain("connection reset")))
}

type errPlain string

func (e errPlain) Error() string { return string(e) }

// TestAccountUIDPartialIndexConflictSQL 断言 ON CONFLICT 冲突目标含部分索引谓词
// WHERE uid != ''，与迁移 262 的 accounts_platform_uid_active 索引完全一致（零降级）。
func TestAccountUIDPartialIndexConflictSQL(t *testing.T) {
	b := entsql.Dialect(dialect.Postgres).Insert("accounts").
		Columns("platform", "uid", "name", "type", "credentials", "credentials_mac", "extra").
		Values("claude", "u1", "n", "oauth", "{}", "mac", "{}").
		OnConflict(
			entsql.ConflictColumns(dbaccount.FieldPlatform, dbaccount.FieldUID),
			entsql.ConflictWhere(entsql.NEQ(dbaccount.FieldUID, "")),
			entsql.DoNothing(),
		)

	q, args := b.Query()
	t.Logf("generated SQL: %s args=%v", q, args)

	upper := strings.ToUpper(q)
	require.Contains(t, upper, "ON CONFLICT", "必须含 ON CONFLICT 子句")
	require.Contains(t, upper, "DO NOTHING", "必须含 DO NOTHING（禁止无谓词宽泛降级）")
	// 冲突目标列必须显式列出 (platform, uid)。
	require.Regexp(t, `ON CONFLICT \("?platform"?,\s*"?uid"?\)`, q)
	// 部分索引谓词必须出现：uid <> ''（NEQ 渲染为 <>），禁止缺失该谓词。
	require.Regexp(t, `WHERE "uid" <> \$\d+`, q, "冲突目标必须含部分索引谓词 WHERE uid <> ''")
	// 参数中应包含空串占位（NEQ 的第二个操作数）。
	require.Contains(t, args, "")
}
