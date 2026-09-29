package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 265 号迁移：CodeBuddy credit attempt_version sequence 创建。
//
// 本文件是 B4 Card B 的迁移契约测试。它不连库（迁移的真实执行属发布闸门动作），
// 而是对**迁移源码**做契约断言 + mutation 反证：对真实文件断言全部通过；对人工
// 变异（模拟"实现被改坏"）断言必须被检出——证明这些断言不是空转的形态检查，
// 而是能真正抓住"改错 sequence 名 / 起始值 != 1 / 丢 GRANT / 丢 owner / 越界触碰
// extra"等回归。
//
// 语义来源：方案 §1（attempt_version ≥ 1，DB sequence 分配）、§3 Card B
// （sequence 名 seq_codebuddy_credit_attempt_version、起始 1、owner=迁移执行角色、
// GRANT USAGE 予应用运行角色，名称钉死进验收证据）。

const (
	codebuddyAttemptVersionSequenceLiteral = "seq_codebuddy_credit_attempt_version"
	codebuddyAppRunRoleLiteral             = "sub2api"

	migrationFileName265 = "265_codebuddy_credit_attempt_version_sequence.sql"
)

// validateCodebuddyAttemptVersionSequenceMigration 检查迁移源码是否满足 sequence
// 契约，返回违规描述列表（空列表 = 全部通过）。抽成纯函数是为了让 mutation 反证
// 可以直接喂入被改坏的源码，验证每条断言都具备真实检出能力。
func validateCodebuddyAttemptVersionSequenceMigration(fileName, raw string) []string {
	var violations []string
	add := func(cond bool, msg string) {
		if cond {
			violations = append(violations, msg)
		}
	}

	sql := prepareMigrationSQL(raw)

	// 必须是常规迁移（同事务执行）。
	add(strings.HasSuffix(fileName, migrationNotxSuffix),
		"迁移必须是事务性常规迁移（_notx 非事务执行会破坏原子性）")

	// sequence 名与创建语句（起始 1）。
	add(!strings.Contains(sql, "CREATE SEQUENCE IF NOT EXISTS "+codebuddyAttemptVersionSequenceLiteral),
		"缺少 CREATE SEQUENCE IF NOT EXISTS（数量缺失或名不符）")
	add(!strings.Contains(sql, "START WITH 1") && !strings.Contains(sql, "START 1"),
		"sequence 起始值必须为 1（合法 attempt_version ≥ 1，0 是无事件哨兵）")

	// owner = 迁移执行角色（CURRENT_USER，动态解析为执行连接角色）。
	add(!strings.Contains(sql, "OWNER TO CURRENT_USER"),
		"缺少 owner 钉定（owner 必须 = 迁移执行角色 CURRENT_USER）")

	// GRANT USAGE 予应用运行角色（名称钉死 sub2api），且带角色存在守卫（幂等）。
	add(!strings.Contains(sql, "GRANT USAGE ON SEQUENCE "+codebuddyAttemptVersionSequenceLiteral),
		"缺少 GRANT USAGE ON SEQUENCE")
	add(!strings.Contains(sql, codebuddyAppRunRoleLiteral),
		"应用运行角色名未钉死（sub2api）")
	add(!strings.Contains(sql, "pg_roles WHERE rolname = '"+codebuddyAppRunRoleLiteral+"'"),
		"GRANT 缺少角色存在性守卫（测试环境无该角色时迁移会失败）")

	// 幂等：IF NOT EXISTS / CURRENT_USER / DO 守卫。
	add(!strings.Contains(sql, "IF NOT EXISTS"),
		"sequence 创建缺少 IF NOT EXISTS（破坏幂等）")

	// 禁止面：不得触碰 accounts/extra 数据，不得 DROP/TRUNCATE 任何对象。
	for _, forbidden := range []string{
		"DELETE FROM accounts",
		"UPDATE accounts",
		"TRUNCATE",
		"DROP SEQUENCE",
		"DROP TABLE",
		"DROP INDEX",
		"codebuddy_credit_packages",
		"codebuddy_credit_error",
		"codebuddy_credit_version",
	} {
		add(strings.Contains(sql, forbidden),
			"迁移越界：出现禁止模式 "+forbidden)
	}

	return violations
}

// TestCodebuddyAttemptVersionSequenceMigration_Contract 对真实迁移文件执行全部契约断言。
func TestCodebuddyAttemptVersionSequenceMigration_Contract(t *testing.T) {
	content, err := FS.ReadFile(migrationFileName265)
	require.NoError(t, err, "265 迁移文件必须存在且被嵌入")

	violations := validateCodebuddyAttemptVersionSequenceMigration(migrationFileName265, string(content))
	require.Empty(t, violations, "迁移契约违规：%v", violations)

	// sequence 名、角色名、起始值必须钉死（验收证据按名称溯源）。
	sql := prepareMigrationSQL(string(content))
	require.Contains(t, sql, codebuddyAttemptVersionSequenceLiteral)
	require.Contains(t, sql, codebuddyAppRunRoleLiteral)
}

// TestCodebuddyAttemptVersionSequenceMigration_MutationGuards 是断言的反证测试：
// 把迁移源码按常见"改坏"方式变异，要求每条变异都被
// validateCodebuddyAttemptVersionSequenceMigration 检出。若某条变异未被检出，
// 说明对应断言是空转的，本测试即失败。
func TestCodebuddyAttemptVersionSequenceMigration_MutationGuards(t *testing.T) {
	content, err := FS.ReadFile(migrationFileName265)
	require.NoError(t, err)

	base := prepareMigrationSQL(string(content))

	// 先确认基线（未变异）无违规，否则反证无意义。
	require.Empty(t, validateCodebuddyAttemptVersionSequenceMigration(migrationFileName265, base))

	cases := []struct {
		name       string
		fileName   string
		mutate     func(string) string
		wantSubstr string
	}{
		{
			name:     "_notx_非事务执行",
			fileName: strings.TrimSuffix(migrationFileName265, ".sql") + migrationNotxSuffix,
			mutate:   func(s string) string { return s },
			wantSubstr: "迁移必须是事务性常规迁移",
		},
		{
			name:     "改名_sequence",
			fileName: migrationFileName265,
			mutate: func(s string) string {
				return strings.Replace(s, codebuddyAttemptVersionSequenceLiteral,
					"seq_codebuddy_credit_attempt", 1)
			},
			wantSubstr: "缺少 CREATE SEQUENCE IF NOT EXISTS",
		},
		{
			name:     "起始值改_0",
			fileName: migrationFileName265,
			mutate: func(s string) string {
				return strings.Replace(s, "START WITH 1", "START WITH 0", 1)
			},
			wantSubstr: "起始值必须为 1",
		},
		{
			name:     "丢掉_OWNER_TO_CURRENT_USER",
			fileName: migrationFileName265,
			mutate: func(s string) string {
				return strings.Replace(s, "OWNER TO CURRENT_USER", "", 1)
			},
			wantSubstr: "缺少 owner 钉定",
		},
		{
			name:     "丢掉_GRANT",
			fileName: migrationFileName265,
			mutate: func(s string) string {
				idx := strings.Index(s, "GRANT USAGE")
				if idx < 0 {
					return s
				}
				return s[:idx] + s[idx+strings.Index(s[idx:], ";")+1:]
			},
			wantSubstr: "缺少 GRANT USAGE ON SEQUENCE",
		},
		{
			name:     "改_应用运行角色",
			fileName: migrationFileName265,
			mutate: func(s string) string {
				return strings.ReplaceAll(s, "sub2api", "another_role")
			},
			wantSubstr: "应用运行角色名未钉死",
		},
		{
			name:     "丢掉_角色存在守卫",
			fileName: migrationFileName265,
			mutate: func(s string) string {
				return strings.Replace(s,
					"IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'sub2api') THEN",
					"IF true THEN", 1)
			},
			wantSubstr: "缺少角色存在性守卫",
		},
		{
			name:     "丢掉_IF_NOT_EXISTS",
			fileName: migrationFileName265,
			mutate: func(s string) string {
				return strings.Replace(s, "IF NOT EXISTS", "", 1)
			},
			wantSubstr: "缺少 IF NOT EXISTS",
		},
		{
			name:     "越界_写账号",
			fileName: migrationFileName265,
			mutate: func(s string) string {
				return s + " UPDATE accounts SET extra = extra || '{\"codebuddy_credit_version\":0}'::jsonb WHERE false;"
			},
			wantSubstr: "禁止模式 UPDATE accounts",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			mutated := tc.mutate(base)

			violations := validateCodebuddyAttemptVersionSequenceMigration(tc.fileName, mutated)
			require.NotEmptyf(t, violations, "变异未被检出（断言空转）：%s", tc.name)

			joined := strings.Join(violations, "\n")
			require.Containsf(t, joined, tc.wantSubstr,
				"变异 %s 应触发断言 %q，实际违规：%v", tc.name, tc.wantSubstr, violations)
		})
	}
}