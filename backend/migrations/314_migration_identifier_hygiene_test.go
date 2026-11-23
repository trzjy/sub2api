package migrations

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// retiredGoIdentifiers 是「已从代码中删除、但曾出现在迁移文件注释里」的 Go 标识符。
//
// 背景：迁移 SQL 通过 embed.FS 嵌入二进制，注释里的标识符会随二进制进生产容器，
// 导致部署自检的「新旧串双向抽查」出现假阳性（2026-11-23 部署 313 时实测踩过：
// 误判成「旧代码未清干净」，实际是新代码）。
//
// 规范：此后新增的迁移文件不得出现这些标识符。需要说明历史背景时描述语义
// （如「代码里的一个硬编码兜底常量（值 256000）」），不要写标识符本身。
//
// 本测试只覆盖**本规范生效之后新增**的迁移（见 below），因为已应用的迁移
// 内容进了生产库 checksum，不能再改（改动会导致启动 checksum mismatch）。
var retiredGoIdentifiers = []string{
	"LargeRequestOnlyInputThreshold",
}

// identifierHygieneEnforcedFrom 之前的迁移不受本规范约束（已应用、内容不可改）。
const identifierHygieneEnforcedFrom = 314

// TestMigrationFiles_NoRetiredGoIdentifiers 校验：编号 >= 314 的迁移文件注释里
// 不出现已删除的 Go 标识符名，避免污染「新旧串双向抽查」的判据。
func TestMigrationFiles_NoRetiredGoIdentifiers(t *testing.T) {
	entries, err := FS.ReadDir(".")
	require.NoError(t, err)

	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		num, ok := migrationNumber(name)
		if !ok || num < identifierHygieneEnforcedFrom {
			continue
		}
		content, err := FS.ReadFile(name)
		require.NoError(t, err)

		for _, retired := range retiredGoIdentifiers {
			require.NotContains(t, string(content), retired,
				"迁移 %s 含有已删除的 Go 标识符 %q：注释会随 embed.FS 进二进制，"+
					"污染部署自检的新旧串抽查判据。请改为描述语义，不要写标识符名", name, retired)
		}
		checked++
	}
	require.Greater(t, checked, 0, "规范生效后应至少有一个迁移文件被检查")
}

// migrationNumber 从形如 "314_xxx.sql" 的文件名解析出编号。
var migrationNumberPattern = regexp.MustCompile(`^(\d+)_`)

func migrationNumber(filename string) (int, bool) {
	m := migrationNumberPattern.FindStringSubmatch(filename)
	if m == nil {
		return 0, false
	}
	n := 0
	for _, c := range m[1] {
		n = n*10 + int(c-'0')
	}
	return n, true
}
