package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 264 号迁移：错误键原子迁移 —— 存量 codebuddy_quota_error → codebuddy_credit_error。
//
// 本文件是 B4 Card E 的迁移契约测试。它不连库（迁移的真实执行属发布闸门动作），
// 而是对**迁移源码**做契约断言 + mutation 反证：对真实文件断言全部通过；对人工
// 变异（模拟"实现被改坏"）断言必须被检出——证明这些断言不是空转的形态检查，
// 而是能真正抓住"静默覆盖双键冲突 / 丢掉幂等条件 / 先写后判 / 残留旧键"等回归。
//
// 五态幂等覆盖（与方案 §3 Card E 逐态对应）：
//   态1 旧键仅有        → assert 搬迁 UPDATE 条件命中（旧键存在 AND 非新键存在）。
//   态2 新键仅有        → 同一条件因 `NOT (extra ? 新键)` 为假而不命中，不触碰。
//   态3 双键冲突        → assert 冲突探测 + RAISE EXCEPTION 阻断，且探测先于任何写入。
//   态4 重复执行        → assert 搬迁条件含"旧键存在"谓词：旧键删净后为空操作。
//   态5 中途失败重试    → assert 本文件是常规迁移（非 _notx）：runner 把整份内容放在
//                        同一事务内执行（migrations_runner.go），失败即整批回滚，
//                        从完整初始态重试不会叠加出分叉状态。

const (
	codebuddyLegacyErrorKeyLiteral = "codebuddy_quota_error"
	codebuddyNewErrorKeyLiteral    = "codebuddy_credit_error"

	migrationFileName264 = "264_migrate_codebuddy_quota_error_to_credit_error.sql"

	migrationNotxSuffix = "_notx.sql"
)

// prepareMigrationSQL 去掉 `--` 行注释并把空白折叠为单空格，使结构断言不受注释
// 散文影响（注释里也会提到 RAISE EXCEPTION / 键名，若不剥离会掩盖真实代码缺失）。
func prepareMigrationSQL(raw string) string {
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		if idx := strings.Index(line, "--"); idx >= 0 {
			lines[i] = line[:idx]
		}
	}
	return strings.Join(strings.Fields(strings.Join(lines, "\n")), " ")
}

// mutateStatementAfter 只在 marker 之后的语句片段上应用变异，避免命中 marker
// 之前的同形谓词（例如 DO 块里的存量扫描与搬迁 UPDATE 的 WHERE 形态相同）。
func mutateStatementAfter(normalized, marker string, fn func(string) string) string {
	idx := strings.Index(normalized, marker)
	if idx < 0 {
		return normalized
	}
	return normalized[:idx] + fn(normalized[idx:])
}

// migrationStatementAfter 截取「从 marker 起，到其后首个分号」的语句片段。
func migrationStatementAfter(normalized, marker string) string {
	start := strings.Index(normalized, marker)
	if start < 0 {
		return ""
	}
	rest := normalized[start:]
	if end := strings.Index(rest, ";"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// validateCodebuddyErrorKeyMigration 检查迁移源码是否满足错误键原子迁移契约，
// 返回违规描述列表（空列表 = 全部通过）。抽成纯函数是为了让 mutation 反证可以
// 直接喂入被改坏的源码，验证每条断言都具备真实检出能力。
func validateCodebuddyErrorKeyMigration(fileName, raw string) []string {
	var violations []string
	add := func(cond bool, msg string) {
		if cond {
			violations = append(violations, msg)
		}
	}

	sql := prepareMigrationSQL(raw)

	// 态5：必须是常规迁移（同事务执行），不得是 *_notx（非事务逐条执行会破坏原子性）。
	add(strings.HasSuffix(fileName, migrationNotxSuffix),
		"迁移必须是事务性常规迁移（_notx 非事务执行会破坏原子性）")

	// 态3：双键冲突探测谓词 + 失败阻断（禁止静默覆盖）。
	add(!strings.Contains(sql, "extra ? '"+codebuddyLegacyErrorKeyLiteral+"' AND extra ? '"+codebuddyNewErrorKeyLiteral+"'"),
		"缺少双键冲突探测谓词（legacy 与 new 同时存在）")
	add(!strings.Contains(sql, "RAISE EXCEPTION"),
		"缺少双键冲突的失败阻断（RAISE EXCEPTION），存在静默覆盖风险")

	// 态1/态2/态4：搬迁 UPDATE 是条件更新——旧键存在 AND 非新键存在。
	updateStmt := migrationStatementAfter(sql, "UPDATE accounts")
	add(updateStmt == "", "找不到搬迁 UPDATE 语句")
	add(!strings.Contains(updateStmt, "extra ? '"+codebuddyLegacyErrorKeyLiteral+"'"),
		"搬迁 UPDATE 缺少「旧键存在」条件（幂等前提：重复执行为空操作）")
	add(!strings.Contains(updateStmt, "NOT (extra ? '"+codebuddyNewErrorKeyLiteral+"')"),
		"搬迁 UPDATE 缺少「新键不存在」条件（会覆盖已存在的新键）")

	// 态1：原值搬迁 + 删除旧键 + 保留其余 extra 键（JSONB merge 语义）。
	add(!strings.Contains(updateStmt, "(extra - '"+codebuddyLegacyErrorKeyLiteral+"')"),
		"搬迁 UPDATE 未从 extra 删除旧键（旧键残留 → 双名共存）")
	add(!strings.Contains(updateStmt, "jsonb_build_object('"+codebuddyNewErrorKeyLiteral+"', extra -> '"+codebuddyLegacyErrorKeyLiteral+"')"),
		"搬迁 UPDATE 未按原值写入新键（值搬运缺失或形态错误）")
	add(!strings.Contains(updateStmt, "||"),
		"搬迁 UPDATE 未使用 JSONB 合并，可能丢弃 extra 中其他键")

	// 态3：冲突探测必须先于任何写入（保证冲突时零部分写入）。
	conflictPos := strings.Index(sql, "RAISE EXCEPTION")
	writePos := strings.Index(sql, "UPDATE accounts")
	add(conflictPos < 0 || writePos < 0 || conflictPos > writePos,
		"双键冲突探测必须位于搬迁写入之前（否则冲突时已产生部分写入）")

	// 禁止面：不得删除/清空账号，不得写回旧键，不得波及本卡之外的 B4 键。
	for _, forbidden := range []string{
		"DELETE FROM accounts",
		"TRUNCATE",
		"DROP TABLE",
		"jsonb_build_object('" + codebuddyLegacyErrorKeyLiteral + "'",
		"codebuddy_credit_packages",
		"codebuddy_credit_last_attempt_at",
		"codebuddy_credit_version",
		"codebuddy_credit_used_percent",
		"codebuddy_credit_reset_at",
		"codebuddy_credit_updated_at",
		"codebuddy_credit_total",
		"codebuddy_credit_used",
	} {
		add(strings.Contains(sql, forbidden),
			"迁移越界：出现禁止模式 "+forbidden)
	}

	return violations
}

// TestCodebuddyErrorKeyMigration_Contract 对真实迁移文件执行全部契约断言。
func TestCodebuddyErrorKeyMigration_Contract(t *testing.T) {
	content, err := FS.ReadFile(migrationFileName264)
	require.NoError(t, err, "264 迁移文件必须存在且被嵌入")

	violations := validateCodebuddyErrorKeyMigration(migrationFileName264, string(content))
	require.Empty(t, violations, "迁移契约违规：%v", violations)

	// 迁移必须以旧键为搬迁对象——这是本卡存在的前提（旧键唯一合法指称点）。
	sql := prepareMigrationSQL(string(content))
	require.Contains(t, sql, codebuddyLegacyErrorKeyLiteral)
	require.Contains(t, sql, codebuddyNewErrorKeyLiteral)
}

// TestCodebuddyErrorKeyMigration_FiveStates 逐态断言迁移语义，供验收按态溯源。
func TestCodebuddyErrorKeyMigration_FiveStates(t *testing.T) {
	content, err := FS.ReadFile(migrationFileName264)
	require.NoError(t, err)

	sql := prepareMigrationSQL(string(content))
	guardStmt := migrationStatementAfter(sql, "SELECT count(*) INTO v_legacy_only")
	conflictStmt := migrationStatementAfter(sql, "SELECT count(*), coalesce")
	updateStmt := migrationStatementAfter(sql, "UPDATE accounts")

	legacyPredicate := "extra ? '" + codebuddyLegacyErrorKeyLiteral + "'"
	newPredicate := "extra ? '" + codebuddyNewErrorKeyLiteral + "'"

	t.Run("态1_旧键仅有_搬迁值并删除旧键", func(t *testing.T) {
		// 命中条件：旧键存在 AND 新键不存在。
		require.Contains(t, updateStmt, legacyPredicate)
		require.Contains(t, updateStmt, "NOT ("+newPredicate+")")
		// 原值搬迁 + 删除旧键 + 合并保留其余键。
		require.Contains(t, updateStmt, "extra -> '"+codebuddyLegacyErrorKeyLiteral+"'")
		require.Contains(t, updateStmt, "jsonb_build_object('"+codebuddyNewErrorKeyLiteral+"'")
		require.Contains(t, updateStmt, "(extra - '"+codebuddyLegacyErrorKeyLiteral+"')")
		require.Contains(t, updateStmt, "||")
	})

	t.Run("态2_新键仅有_不触碰", func(t *testing.T) {
		// 同一搬迁条件对「新键仅有」因 NOT 子句为假而不命中；且全文件只有一条
		// 写 accounts 的语句（不得有第二条无条件写入绕过守卫）。
		require.Contains(t, updateStmt, "NOT ("+newPredicate+")")
		require.Equal(t, 1, strings.Count(sql, "UPDATE accounts"),
			"只允许一条 accounts 写入语句，且必须带条件")
	})

	t.Run("态3_双键冲突_失败阻断且先判后写", func(t *testing.T) {
		// 冲突计数语句只做探测（两键同时存在）。
		require.Contains(t, conflictStmt, legacyPredicate)
		require.Contains(t, conflictStmt, newPredicate)
		// 冲突存在即中止整批（非静默覆盖）。
		require.Contains(t, sql, "IF v_conflict > 0 THEN RAISE EXCEPTION")
		require.Less(t, strings.Index(sql, "RAISE EXCEPTION"), strings.Index(sql, "UPDATE accounts"),
			"冲突阻断必须先于搬迁写入（零部分写入）")
	})

	t.Run("态4_重复执行_幂等空操作", func(t *testing.T) {
		// 搬迁条件含「旧键存在」；态1 已删净旧键，故重复执行不命中任何行。
		require.Contains(t, updateStmt, legacyPredicate)
		// DO 块只读不写（除 RAISE），保证重复执行不产生副作用。
		require.NotContains(t, guardStmt, "UPDATE accounts")
		require.NotContains(t, guardStmt, "SET ")
	})

	t.Run("态5_中途失败重试_单事务整批回滚", func(t *testing.T) {
		// 常规迁移（非 _notx）由 runner 包在单一事务内执行：失败整批回滚，
		// 重试从完整初始态重新开始，不叠加分叉。
		require.False(t, strings.HasSuffix(migrationFileName264, migrationNotxSuffix))
		upper := strings.ToUpper(sql)
		require.NotContains(t, upper, "COMMIT")
		require.NotContains(t, upper, "ROLLBACK")
		require.NotContains(t, upper, "CONCURRENTLY")
	})
}

// TestCodebuddyErrorKeyMigration_MutationGuards 是断言的反证测试：把迁移源码
// 按常见"改坏"方式变异，要求每条变异都被 validateCodebuddyErrorKeyMigration 检出。
// 若某条变异未被检出，说明对应断言是空转的，本测试即失败。
func TestCodebuddyErrorKeyMigration_MutationGuards(t *testing.T) {
	content, err := FS.ReadFile(migrationFileName264)
	require.NoError(t, err)

	base := prepareMigrationSQL(string(content))

	// 先确认基线（未变异）无违规，否则反证无意义。
	require.Empty(t, validateCodebuddyErrorKeyMigration(migrationFileName264, base))

	const (
		notxViolation = "迁移必须是事务性常规迁移"
	)

	cases := []struct {
		name       string
		fileName   string
		mutate     func(string) string
		wantSubstr string
	}{
		{
			name:     "丢掉_新键不存在_条件（会覆盖已存在新键）",
			fileName: migrationFileName264,
			mutate: func(s string) string {
				return mutateStatementAfter(s, "UPDATE accounts", func(stmt string) string {
					return strings.Replace(stmt, "AND NOT (extra ? '"+codebuddyNewErrorKeyLiteral+"');", ";", 1)
				})
			},
			wantSubstr: "缺少「新键不存在」条件",
		},
		{
			name:     "丢掉_旧键存在_条件（UPDATE 变无条件，破坏幂等）",
			fileName: migrationFileName264,
			mutate: func(s string) string {
				return mutateStatementAfter(s, "UPDATE accounts", func(stmt string) string {
					return strings.Replace(stmt,
						"WHERE extra ? '"+codebuddyLegacyErrorKeyLiteral+"' AND NOT (extra ? '"+codebuddyNewErrorKeyLiteral+"');",
						";", 1)
				})
			},
			wantSubstr: "缺少「旧键存在」条件",
		},
		{
			name:     "去掉_RAISE_EXCEPTION（双键冲突不再阻断）",
			fileName: migrationFileName264,
			mutate: func(s string) string {
				idx := strings.Index(s, "RAISE EXCEPTION")
				if idx < 0 {
					return s
				}
				end := strings.Index(s[idx:], ";")
				return s[:idx] + s[idx+end+1:]
			},
			wantSubstr: "缺少双键冲突的失败阻断",
		},
		{
			name:     "去掉_删除旧键（旧键残留 → 双名共存）",
			fileName: migrationFileName264,
			mutate: func(s string) string {
				return mutateStatementAfter(s, "UPDATE accounts", func(stmt string) string {
					return strings.Replace(stmt, "(extra - '"+codebuddyLegacyErrorKeyLiteral+"')", "extra", 1)
				})
			},
			wantSubstr: "未从 extra 删除旧键",
		},
		{
			name:     "去掉_JSONB 合并（丢其余 extra 键）",
			fileName: migrationFileName264,
			mutate: func(s string) string {
				return mutateStatementAfter(s, "UPDATE accounts", func(stmt string) string {
					return strings.Replace(stmt, " || ", " ", 1)
				})
			},
			wantSubstr: "未使用 JSONB 合并",
		},
		{
			name:     "丢失_原值写入新键（搬迁值缺失/形态错误）",
			fileName: migrationFileName264,
			mutate: func(s string) string {
				return mutateStatementAfter(s, "UPDATE accounts", func(stmt string) string {
					return strings.Replace(stmt,
						"jsonb_build_object('"+codebuddyNewErrorKeyLiteral+"', extra -> '"+codebuddyLegacyErrorKeyLiteral+"')",
						"jsonb_build_object('"+codebuddyNewErrorKeyLiteral+"', 'constant')", 1)
				})
			},
			wantSubstr: "未按原值写入新键",
		},
		{
			name:     "冲突探测后移到写入之后",
			fileName: migrationFileName264,
			mutate: func(s string) string {
				updateIdx := strings.Index(s, "UPDATE accounts")
				raiseIdx := strings.Index(s, "RAISE EXCEPTION")
				if updateIdx < 0 || raiseIdx < 0 {
					return s
				}
				return s[updateIdx:] + " " + s[:updateIdx]
			},
			wantSubstr: "双键冲突探测必须位于搬迁写入之前",
		},
		{
			name:     "迁移越界删除账号",
			fileName: migrationFileName264,
			mutate: func(s string) string {
				return s + " DELETE FROM accounts WHERE false;"
			},
			wantSubstr: "禁止模式 DELETE FROM accounts",
		},
		{
			name:     "越界写回旧键",
			fileName: migrationFileName264,
			mutate: func(s string) string {
				return s + " UPDATE accounts SET extra = extra || jsonb_build_object('" + codebuddyLegacyErrorKeyLiteral + "', 'x');"
			},
			wantSubstr: "禁止模式 jsonb_build_object('codebuddy_quota_error'",
		},
		{
			name:     "越界波及本卡之外的 B4 键",
			fileName: migrationFileName264,
			mutate: func(s string) string {
				return s + " UPDATE accounts SET extra = extra || jsonb_build_object('codebuddy_credit_version', 0);"
			},
			wantSubstr: "禁止模式 codebuddy_credit_version",
		},
		{
			name:       "_notx_非事务执行",
			fileName:   strings.TrimSuffix(migrationFileName264, ".sql") + migrationNotxSuffix,
			mutate:     func(s string) string { return s },
			wantSubstr: notxViolation,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			mutated := tc.mutate(base)

			violations := validateCodebuddyErrorKeyMigration(tc.fileName, mutated)
			require.NotEmptyf(t, violations, "变异未被检出（断言空转）：%s", tc.name)

			joined := strings.Join(violations, "\n")
			require.Containsf(t, joined, tc.wantSubstr,
				"变异 %s 应触发断言 %q，实际违规：%v", tc.name, tc.wantSubstr, violations)
		})
	}
}
