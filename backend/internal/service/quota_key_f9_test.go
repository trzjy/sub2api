//go:build unit

package service

import (
	"context"
	"strings"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/migrations"
)

// splitSQLStatements 把迁移 SQL 切成单条可执行语句（去除 -- 注释，按 ; 断句）。
// 迁移文件不含字符串内分号，简单切分即可满足测试需要。
func splitSQLStatements(sql string) []string {
	var raw []string
	for _, line := range strings.Split(sql, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(raw) == 0 || strings.HasSuffix(raw[len(raw)-1], ";") {
			raw = append(raw, line)
		} else {
			raw[len(raw)-1] += " " + line
		}
	}
	var out []string
	for _, s := range raw {
		s = strings.TrimSpace(s)
		s = strings.TrimSuffix(s, ";")
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// applyMigration290 读取并执行 R6-1 迁移 SQL（幂等，可重复执行）。
func applyMigration290(t *testing.T, client *ent.Client) {
	t.Helper()
	content, err := migrations.FS.ReadFile("290_quota_keys_reclassify_non_cooldown_stock.sql")
	require.NoError(t, err)
	ctx := context.Background()
	for _, stmt := range splitSQLStatements(string(content)) {
		var result entsql.Result
		require.NoError(t, client.Driver().Exec(ctx, stmt, []any{}, &result), "exec migration stmt: %s", stmt)
	}
}

// TestCURF9_ReclassifyNonCooldownStock 验证 R6-1 迁移对三类存量行的处置：
//   - 冷却形态（命中运行时冷却集「仅可刷新 / 限频 / 冷却」任一，不含过期/耗尽措辞）→ 保持 active + exhausted=false；
//   - 过期/耗尽（含「已过期/额度耗尽/已耗尽」）→ 标 status='expired'；
//   - 未知错误 → 标 exhausted=true。
//
// R11（CUR-F14）：额外锁死只含「限频」「冷却」文案（无"每…仅可刷新"句式）的存量行——对齐运行时
// 冷却集后须识别为冷却形态并保持，不再被 ②-b 误标 exhausted=true。并验证迁移幂等（二次执行不改变已落位结果）。
func TestCURF9_ReclassifyNonCooldownStock(t *testing.T) {
	client := quotaKeyTestEntClient(t)
	ctx := context.Background()

	const (
		cooldownCode = "enc:v1:curf9-r61-cooldown"
		expiredCode  = "enc:v1:curf9-r61-expired"
		unknownCode  = "enc:v1:curf9-r61-unknown"
		// createAccountCode：旧落库失败标记形态（create account: ... 文案），用于锁死迁移 290
		// ②-b 覆盖面——外审曾误以为此类行会被当成"冷却/过期"而豁免，实则落入 ②-b 的未知错误支。
		createAccountCode = "enc:v1:curf9-r61-createaccount"
		// mixedCode：R10-2（CUR-F13）混合文案行——同时含过期措辞「密钥已过期」与冷却文案
		// 「每5分钟内仅可刷新1次」。旧迁移 ②-a 的冷却前置会将其排除出过期支、②-b 又因含冷却
		// 文案排除出未知支，两头不落残留为候选；对齐运行时后 ②-a' 应将其归 expired。
		mixedCode = "enc:v1:curf9-r61-mixed"
		// rateLimitCode / coolingCode：R11（CUR-F14）只含「限频」「冷却」文案、无"每…仅可刷新"
		// 句式的存量行。旧迁移冷却判定唯一依赖 `LIKE '%每%分钟内仅可刷新%次%'`，会把这两类行漏出
		// ① 冷却支、落入 ②-b 误标 exhausted=true → 五分钟窗口期满也不回候选，违背裁定 3。对齐运行时
		// 冷却集「仅可刷新 / 限频 / 冷却」后，迁移 ① 应将其识别为冷却形态并保持（active + exhausted=false）。
		rateLimitCode    = "enc:v1:curf9-r11-ratelimit"
		coolingCode      = "enc:v1:curf9-r11-cooling"
		cooldownErr      = "shareai: upstream cooldown, retry later: 每5分钟内仅可刷新1次"
		expiredErr       = "激活码已过期，请续费后重试"
		unknownErr       = "网络抖动，请稍后重试"
		createAccountErr = "shareai: create account: dup"
		mixedErr         = "shareai: upstream: 密钥已过期，每5分钟内仅可刷新1次"
		rateLimitErr     = "上游限频，请稍后再试"
		coolingErr       = "冷却中"
	)
	// 清理可能残留的同类行（保证测试可重复）。
	_ = client.Driver().Exec(ctx,
		`DELETE FROM quota_keys WHERE code_encrypted IN ($1,$2,$3,$4,$5,$6,$7)`,
		[]any{cooldownCode, expiredCode, unknownCode, createAccountCode, mixedCode, rateLimitCode, coolingCode}, new(entsql.Result))

	// 构造三类存量行（均为旧形态：exhausted=false + status='active' + 非空 last_refresh_error）。
	insert := func(code, errText string) {
		var result entsql.Result
		require.NoError(t, client.Driver().Exec(ctx,
			`INSERT INTO quota_keys (code_encrypted, status, exhausted, last_refresh_error, created_at, updated_at)
			 VALUES ($1, 'active', false, $2, now(), now())`,
			[]any{code, errText}, &result))
	}
	insert(cooldownCode, cooldownErr)
	insert(expiredCode, expiredErr)
	insert(unknownCode, unknownErr)
	// R9-2（CUR-F12，2026-10-17）：旧落库失败标记文案（create account: ...）。外审未看到迁移
	// 290，误以为此类行会被当成冷却/过期而豁免候选；实则 ②-b 的未知错误支（非冷却文案、非
	// 过期/耗尽文案）把它标 exhausted=true，排除出候选。补此行锁死迁移覆盖面、防同类误报再犯。
	insert(createAccountCode, createAccountErr)
	// R10-2（CUR-F13，2026-10-17）：混合文案行（同时含「密钥已过期」与「每5分钟内仅可刷新1次」）。
	// 对齐运行时 classifyUpstreamError「过期优先于冷却」后，②-a' 应将其归 expired 而非残留候选。
	insert(mixedCode, mixedErr)
	// R11（CUR-F14，2026-10-17）：只含「限频」/「冷却」文案的存量行（无"每…仅可刷新"句式）。
	// 对齐运行时冷却集后，迁移 ① 应将其识别为冷却形态并保持（active + exhausted=false）。
	insert(rateLimitCode, rateLimitErr)
	insert(coolingCode, coolingErr)

	// 首次应用迁移。
	applyMigration290(t, client)

	fetch := func(code string) (status string, exhausted bool) {
		var rows entsql.Rows
		require.NoError(t, client.Driver().Query(ctx,
			`SELECT status, exhausted FROM quota_keys WHERE code_encrypted = $1`,
			[]any{code}, &rows))
		defer func() { _ = rows.Close() }()
		require.True(t, rows.Next(), "row must exist for %s", code)
		var s string
		var e bool
		require.NoError(t, rows.Scan(&s, &e))
		require.NoError(t, rows.Err())
		return s, e
	}

	// fetchCooldownRow 取冷却行全字段（含 last_refresh_error / last_refresh_at），供 R12-1 改写形态与
	// 窗口行为断言使用。
	type cooldownRow struct {
		id            int64
		err           string
		lastRefreshAt *time.Time
	}
	fetchCooldownRow := func(code string) cooldownRow {
		var rows entsql.Rows
		require.NoError(t, client.Driver().Query(ctx,
			`SELECT id, last_refresh_error, last_refresh_at FROM quota_keys WHERE code_encrypted = $1`,
			[]any{code}, &rows))
		defer func() { _ = rows.Close() }()
		require.True(t, rows.Next(), "row must exist for %s", code)
		var r cooldownRow
		require.NoError(t, rows.Scan(&r.id, &r.err, &r.lastRefreshAt))
		require.NoError(t, rows.Err())
		return r
	}

	// ① 冷却行：保持原状。
	cStatus, cExhausted := fetch(cooldownCode)
	require.Equal(t, "active", cStatus, "cooldown row must stay active")
	require.False(t, cExhausted, "cooldown row must stay exhausted=false")

	// ① 冷却行（R12-1/CUR-F15）：只含「限频」文案 → 改写为标准冷却形态。
	// R12-1 修正：旧版"保持原文"会使该行为 active+exhausted=false+无哨兵前缀 → 被 isCooldownRefreshError
	// 判为非冷却 → 直接重入且无 5min 窗口保护。迁移后须带 ErrUpstreamCooldown 哨兵前缀、且补写
	// last_refresh_at 窗口锚点，status/exhausted 保持不变。
	rlStatus, rlExhausted := fetch(rateLimitCode)
	require.Equal(t, "active", rlStatus, "rate-limit-form row must stay active (cooldown shape)")
	require.False(t, rlExhausted, "rate-limit-form row must stay exhausted=false (cooldown shape)")
	rlRow := fetchCooldownRow(rateLimitCode)
	require.True(t, strings.HasPrefix(rlRow.err, "shareai: upstream cooldown, retry later:"),
		"rate-limit-form row must be rewritten with sentinel cooldown prefix, got: %q", rlRow.err)
	require.Equal(t, "shareai: upstream cooldown, retry later: "+rateLimitErr, rlRow.err,
		"rate-limit-form row rewrite must be prefix || original text")
	require.NotNil(t, rlRow.lastRefreshAt, "rate-limit-form row last_refresh_at must be set (window anchor)")

	// ① 冷却行（R12-1/CUR-F15）：只含「冷却」文案 → 同样改写为标准冷却形态（前缀 + 窗口锚点）。
	coStatus, coExhausted := fetch(coolingCode)
	require.Equal(t, "active", coStatus, "cooling-form row must stay active (cooldown shape)")
	require.False(t, coExhausted, "cooling-form row must stay exhausted=false (cooldown shape)")
	coRow := fetchCooldownRow(coolingCode)
	require.True(t, strings.HasPrefix(coRow.err, "shareai: upstream cooldown, retry later:"),
		"cooling-form row must be rewritten with sentinel cooldown prefix, got: %q", coRow.err)
	require.Equal(t, "shareai: upstream cooldown, retry later: "+coolingErr, coRow.err,
		"cooling-form row rewrite must be prefix || original text")
	require.NotNil(t, coRow.lastRefreshAt, "cooling-form row last_refresh_at must be set (window anchor)")

	// 冷却行（已带前缀的存量样本 cooldownCode）：① 幂等 WHERE 跳过已带前缀的行 → 保持原文、last_refresh_at 仍为 NULL。
	cRow := fetchCooldownRow(cooldownCode)
	require.Equal(t, cooldownErr, cRow.err, "already-prefixed cooldown row must be left untouched by ①")
	require.Nil(t, cRow.lastRefreshAt, "already-prefixed cooldown row keeps its (null) last_refresh_at")

	// ②-a 过期行：标 expired（exhausted 维度保留原值 false）。
	eStatus, eExhausted := fetch(expiredCode)
	require.Equal(t, "expired", eStatus, "expired-form row must be marked expired")
	require.False(t, eExhausted, "expired-form row keeps exhausted=false")

	// ②-b 未知行：标 exhausted=true（status 维度保留原值 active）。
	uStatus, uExhausted := fetch(unknownCode)
	require.Equal(t, "active", uStatus, "unknown-form row keeps status active")
	require.True(t, uExhausted, "unknown-form row must be marked exhausted=true")

	// ②-b 旧落库失败标记行（create account: dup）：同属未知错误支 → 标 exhausted=true
	// （status 维度保留原值 active），被 ListCandidateKeys 的 Exhausted(false) 查询自然排除。
	caStatus, caExhausted := fetch(createAccountCode)
	require.Equal(t, "active", caStatus, "create-account-form row keeps status active")
	require.True(t, caExhausted, "create-account-form row must be marked exhausted=true (reclassified out of candidates)")

	// ②-a' 混合文案行（同时含「密钥已过期」与冷却文案）：对齐运行时「过期优先于冷却」归 expired
	// （exhausted 维度保留原值 false），不再残留为候选重入。R10-2（CUR-F13）锁死此覆盖面。
	mStatus, mExhausted := fetch(mixedCode)
	require.Equal(t, "expired", mStatus, "mixed expired+cooldown row must be reclassified as expired (R10-2)")
	require.False(t, mExhausted, "mixed row keeps exhausted=false")

	// 幂等性：二次执行迁移，结果不变。
	applyMigration290(t, client)
	cStatus2, cExhausted2 := fetch(cooldownCode)
	require.Equal(t, "active", cStatus2)
	require.False(t, cExhausted2)
	eStatus2, _ := fetch(expiredCode)
	require.Equal(t, "expired", eStatus2)
	_, uExhausted2 := fetch(unknownCode)
	require.True(t, uExhausted2)
	_, caExhausted2 := fetch(createAccountCode)
	require.True(t, caExhausted2)
	// 幂等性：混合文案行二次执行后仍 expired。
	mStatus2, mExhausted2 := fetch(mixedCode)
	require.Equal(t, "expired", mStatus2, "mixed row must stay expired after second migration run")
	require.False(t, mExhausted2, "mixed row keeps exhausted=false after second migration run")
	// 幂等性：限频/冷却文案行二次执行后仍保持冷却形态（active + exhausted=false）。
	rlStatus2, rlExhausted2 := fetch(rateLimitCode)
	require.Equal(t, "active", rlStatus2, "rate-limit-form row must stay active after second migration run")
	require.False(t, rlExhausted2, "rate-limit-form row must stay exhausted=false after second migration run")
	coStatus2, coExhausted2 := fetch(coolingCode)
	require.Equal(t, "active", coStatus2, "cooling-form row must stay active after second migration run")
	require.False(t, coExhausted2, "cooling-form row must stay exhausted=false after second migration run")

	// 幂等性（R12-1）：二次执行①不得重复追加哨兵前缀——改写的冷却行以哨兵前缀开头，① 的
	// `NOT LIKE 'shareai: upstream cooldown%'` 跳过它，故前缀在文本中仅出现一次。
	rlRow2 := fetchCooldownRow(rateLimitCode)
	require.Equal(t, "shareai: upstream cooldown, retry later: "+rateLimitErr, rlRow2.err,
		"rate-limit-form row prefix must NOT be appended twice after second migration run")
	require.Equal(t, 1, strings.Count(rlRow2.err, "shareai: upstream cooldown, retry later:"),
		"cooldown sentinel prefix must appear exactly once after idempotent re-run")
	coRow2 := fetchCooldownRow(coolingCode)
	require.Equal(t, 1, strings.Count(coRow2.err, "shareai: upstream cooldown, retry later:"),
		"cooldown sentinel prefix must appear exactly once (cooling form) after idempotent re-run")

	// 窗口行为断言（R12-1，核心回归）：迁移产物须进入冷却窗口语义——改写后行带哨兵前缀 +
	// last_refresh_at 锚点，ListCandidateKeys 按 5min 窗口判定。
	//   · last_refresh_at = now()        → 仍在窗口内 → 排除出候选；
	//   · last_refresh_at = now() - 6min  → 窗口已过 → 自动重新入选（不等人工 clear）。
	w := &quotaKeyModelWater{client: client}
	rlID := fetchCooldownRow(rateLimitCode).id
	// 窗口内：置 last_refresh_at = now()。
	require.NoError(t, client.Driver().Exec(ctx,
		`UPDATE quota_keys SET last_refresh_at = now() WHERE code_encrypted = $1`,
		[]any{rateLimitCode}, new(entsql.Result)))
	candsInWindow, lerr := w.ListCandidateKeys(ctx, "", 0)
	require.NoError(t, lerr)
	require.NotContains(t, candsInWindow, rlID,
		"cooldown row within 5min window must be excluded from ListCandidateKeys (R12-1)")
	// 窗口已过：置 last_refresh_at = now() - interval '6 minutes'。
	require.NoError(t, client.Driver().Exec(ctx,
		`UPDATE quota_keys SET last_refresh_at = now() - interval '6 minutes' WHERE code_encrypted = $1`,
		[]any{rateLimitCode}, new(entsql.Result)))
	candsAfterWindow, lerr2 := w.ListCandidateKeys(ctx, "", 0)
	require.NoError(t, lerr2)
	require.Contains(t, candsAfterWindow, rlID,
		"cooldown row past 5min window must be re-selected by ListCandidateKeys (R12-1)")

	// 清理测试行。
	_ = client.Driver().Exec(ctx,
		`DELETE FROM quota_keys WHERE code_encrypted IN ($1,$2,$3,$4,$5,$6,$7)`,
		[]any{cooldownCode, expiredCode, unknownCode, createAccountCode, mixedCode, rateLimitCode, coolingCode}, new(entsql.Result))
}
