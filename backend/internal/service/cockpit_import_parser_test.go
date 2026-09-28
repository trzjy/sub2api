// cockpit_import_parser_test.go —— Cockpit 导入解析引擎单元测试（方案 §1.1–1.4、§1.6）。
//
// 覆盖：全部失败关闭分支（ZIP 资源硬上限 / JSON 结构上限 / schema 版本白名单 / 重复 JSON 键）、
// 规范化等价类、同文件同键分组、ZIP 解压炸弹、结构超限。
// 测试函数名均以 TestCockpitImport 开头，匹配验证白名单 -run 'TestCockpitImport|TestCockpitPlatform'。
package service

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// —— 构造辅助 ——

type zipEntry struct {
	name    string
	content []byte
	mode    os.FileMode
}

func buildCockpitZip(t *testing.T, entries []zipEntry) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		var w io.Writer
		var err error
		if e.mode != 0 {
			fh := &zip.FileHeader{Name: e.name, Method: zip.Store}
			fh.SetMode(e.mode)
			w, err = zw.CreateHeader(fh)
		} else {
			w, err = zw.Create(e.name)
		}
		require.NoError(t, err)
		_, err = w.Write(e.content)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// buildCockpitEnvelope 构造合法信封（accounts.platforms.<slug>.exported_data = 给定账号列表）。
func buildCockpitEnvelope(t *testing.T, schema string, version float64, omitVersion, omitSchema bool, accts map[string][]map[string]interface{}) []byte {
	platforms := map[string]interface{}{}
	for slug, list := range accts {
		platforms[slug] = map[string]interface{}{
			"account_count": len(list),
			"exported_data": list,
		}
	}
	env := map[string]interface{}{
		"schema":      schema,
		"exported_at": "2026-09-28T05:07:25Z",
		"sections":    map[string]interface{}{},
		"accounts":    map[string]interface{}{"platforms": platforms},
		"config":      map[string]interface{}{},
	}
	if !omitVersion {
		env["version"] = version
	}
	if omitSchema {
		delete(env, "schema")
	}
	b, err := json.Marshal(env)
	require.NoError(t, err)
	return b
}

func nestedJSON(depth int) string {
	var sb strings.Builder
	for i := 0; i < depth; i++ {
		sb.WriteString(`{"a":`)
	}
	sb.WriteString(`1`)
	for i := 0; i < depth; i++ {
		sb.WriteString(`}`)
	}
	return sb.String()
}

// —— ① raw_sha256 ——

func TestCockpitImport_RawSHA256Computed(t *testing.T) {
	raw := buildCockpitEnvelope(t, "cockpit-tools.data-transfer", 1, false, false, map[string][]map[string]interface{}{
		"grok": {{}, {}, {}},
	})
	res, err := ParseCockpitImport(raw)
	require.NoError(t, err)
	require.Equal(t, "cockpit-tools.data-transfer", res.Schema)
	require.Equal(t, int64(1), res.Version)
	sum := sha256.Sum256(raw)
	require.Equal(t, hex.EncodeToString(sum[:]), res.RawSHA256)
}

// —— ⑤ 逐 slug 三态计数 ——

func TestCockpitImport_PendingSlugCounted(t *testing.T) {
	raw := buildCockpitEnvelope(t, "cockpit-tools.data-transfer", 1, false, false, map[string][]map[string]interface{}{
		"grok": {{}, {}, {}},
	})
	res, err := ParseCockpitImport(raw)
	require.NoError(t, err)
	require.Equal(t, 3, res.PendingPlatformAccounts["grok"])
	require.Contains(t, res.PendingPlatformMessages["grok"], "已识别 3 个账号")
	require.Equal(t, 0, res.UnknownPlatformAccounts)
}

func TestCockpitImport_UnsupportedSlugCounted(t *testing.T) {
	raw := buildCockpitEnvelope(t, "cockpit-tools.data-transfer", 1, false, false, map[string][]map[string]interface{}{
		"cursor": {{}, {}},
	})
	res, err := ParseCockpitImport(raw)
	require.NoError(t, err)
	require.Equal(t, 2, res.UnknownPlatformAccounts)
	require.Equal(t, 2, res.UnknownPlatformAccountSlug["cursor"])
}

func TestCockpitImport_UnknownSlugCounted(t *testing.T) {
	raw := buildCockpitEnvelope(t, "cockpit-tools.data-transfer", 1, false, false, map[string][]map[string]interface{}{
		"future_new_platform": {{}, {}, {}, {}, {}},
	})
	res, err := ParseCockpitImport(raw)
	require.NoError(t, err)
	require.Equal(t, 5, res.UnknownPlatformAccounts)
	require.Equal(t, 5, res.UnknownPlatformAccountSlug["future_new_platform"])
	require.Empty(t, res.PendingPlatformAccounts)
}

// —— ④ schema/version 白名单（失败关闭）——

func TestCockpitImport_SchemaVersion_Zero(t *testing.T) {
	raw := buildCockpitEnvelope(t, "cockpit-tools.data-transfer", 0, false, false, map[string][]map[string]interface{}{"grok": {}})
	_, err := ParseCockpitImport(raw)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitSchemaVersionUnsupported))
}

func TestCockpitImport_SchemaVersion_Negative(t *testing.T) {
	raw := buildCockpitEnvelope(t, "cockpit-tools.data-transfer", -1, false, false, map[string][]map[string]interface{}{"grok": {}})
	_, err := ParseCockpitImport(raw)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitSchemaVersionUnsupported))
}

func TestCockpitImport_SchemaVersion_Missing(t *testing.T) {
	raw := buildCockpitEnvelope(t, "cockpit-tools.data-transfer", 1, true, false, map[string][]map[string]interface{}{"grok": {}})
	_, err := ParseCockpitImport(raw)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitSchemaVersionUnsupported))
}

func TestCockpitImport_SchemaVersion_Two(t *testing.T) {
	raw := buildCockpitEnvelope(t, "cockpit-tools.data-transfer", 2, false, false, map[string][]map[string]interface{}{"grok": {}})
	_, err := ParseCockpitImport(raw)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitSchemaVersionUnsupported))
}

func TestCockpitImport_Schema_Unsupported(t *testing.T) {
	raw := buildCockpitEnvelope(t, "wrong-schema", 1, false, false, map[string][]map[string]interface{}{"grok": {}})
	_, err := ParseCockpitImport(raw)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitSchemaVersionUnsupported))
}

// —— ③ JSON 结构上限（失败关闭）——

func TestCockpitImport_PayloadStructureLimit_NestingDepth(t *testing.T) {
	raw := []byte(nestedJSON(33)) // 嵌套 33 层 > 32
	_, err := ParseCockpitImport(raw)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitPayloadStructureLimit))
}

func TestCockpitImport_PayloadStructureLimit_SingleField(t *testing.T) {
	big := strings.Repeat("A", 70000) // > 64KB
	raw := []byte(`{"schema":"cockpit-tools.data-transfer","version":1,"big":"` + big + `"}`)
	_, err := ParseCockpitImport(raw)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitPayloadStructureLimit))
}

func TestCockpitImport_PayloadStructureLimit_AccountEntryCount(t *testing.T) {
	list := make([]map[string]interface{}, 201) // > 200
	for i := range list {
		list[i] = map[string]interface{}{}
	}
	raw := buildCockpitEnvelope(t, "cockpit-tools.data-transfer", 1, false, false, map[string][]map[string]interface{}{
		"grok": list,
	})
	_, err := ParseCockpitImport(raw)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitPayloadStructureLimit))
}

// —— ③ 重复 JSON 键（失败关闭）——

func TestCockpitImport_DuplicateJSONKey(t *testing.T) {
	raw := []byte(`{"schema":"cockpit-tools.data-transfer","version":1,"accounts":{"platforms":{"grok":{"account_count":0,"exported_data":[],"exported_data":[]}}}}`)
	_, err := ParseCockpitImport(raw)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitDuplicateJSONKey))
}

// —— ② ZIP 资源硬上限（失败关闭）——

func TestCockpitImport_ZIP_OK(t *testing.T) {
	env := buildCockpitEnvelope(t, "cockpit-tools.data-transfer", 1, false, false, map[string][]map[string]interface{}{
		"grok": {{}, {}, {}},
	})
	z := buildCockpitZip(t, []zipEntry{{name: "backup.json", content: env}})
	res, err := ParseCockpitImport(z)
	require.NoError(t, err)
	require.Equal(t, 3, res.PendingPlatformAccounts["grok"])
	sum := sha256.Sum256(z)
	require.Equal(t, hex.EncodeToString(sum[:]), res.RawSHA256)
}

func TestCockpitImport_ZIPResourceLimit_PerEntryUncompressed(t *testing.T) {
	content := bytes.Repeat([]byte{'A'}, 51<<20) // 51MB > 50MB 单条目上限
	z := buildCockpitZip(t, []zipEntry{{name: "backup.json", content: content}})
	_, err := ParseCockpitImport(z)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitUnarchiveResourceLimit))
}

func TestCockpitImport_ZIPResourceLimit_TotalUncompressed(t *testing.T) {
	big := bytes.Repeat([]byte{'A'}, 31<<20) // 31MB，单条目 < 50MB
	z := buildCockpitZip(t, []zipEntry{
		{name: "backup.json", content: big},
		{name: "other.json", content: big},
	})
	_, err := ParseCockpitImport(z)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitUnarchiveResourceLimit))
}

func TestCockpitImport_ZIPBomb_HighAmplification(t *testing.T) {
	// 10MB 全 'A'，Deflate 后极小 → 放大比远超 100，但单条目 < 50MB（专门命中放大比上限）。
	content := bytes.Repeat([]byte{'A'}, 10<<20)
	z := buildCockpitZip(t, []zipEntry{{name: "backup.json", content: content}})
	_, err := ParseCockpitImport(z)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitUnarchiveResourceLimit))
}

func TestCockpitImport_ZIPResourceLimit_EntryCount(t *testing.T) {
	entries := make([]zipEntry, 0, 1001)
	for i := 0; i < 1001; i++ {
		entries = append(entries, zipEntry{name: "f" + string(rune('a'+i%26)) + strconv.Itoa(i), content: []byte("x")})
	}
	z := buildCockpitZip(t, entries)
	_, err := ParseCockpitImport(z)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitUnarchiveResourceLimit))
}

func TestCockpitImport_ZIPPathTraversalRejected(t *testing.T) {
	z := buildCockpitZip(t, []zipEntry{{name: "../evil.json", content: []byte("{}")}})
	_, err := ParseCockpitImport(z)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitUnarchiveResourceLimit))
}

func TestCockpitImport_ZIPDuplicateEntryNameRejected(t *testing.T) {
	z := buildCockpitZip(t, []zipEntry{
		{name: "backup.json", content: []byte("{}")},
		{name: "backup.json", content: []byte("{}")},
	})
	_, err := ParseCockpitImport(z)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitUnarchiveResourceLimit))
}

func TestCockpitImport_ZIPNoBackupJSON(t *testing.T) {
	z := buildCockpitZip(t, []zipEntry{{name: "readme.txt", content: []byte("hi")}})
	_, err := ParseCockpitImport(z)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitUnarchiveResourceLimit))
}

func TestCockpitImport_ZIPSymlinkRejected(t *testing.T) {
	z := buildCockpitZip(t, []zipEntry{
		{name: "backup.json", content: []byte("{}"), mode: os.ModeSymlink | 0o755},
	})
	_, err := ParseCockpitImport(z)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitUnarchiveResourceLimit))
}

// —— ⑥ 规范化等价类（方案 §1.3）——

func TestCockpitImport_NormalizeEquivalenceClasses(t *testing.T) {
	require.Equal(t, "abc", normalizeCockpitField("  abc  "))
	require.Equal(t, "abc", normalizeCockpitField("\tabc\r\n"))
	require.Equal(t, "", normalizeCockpitField("   "))
	require.Equal(t, "codebuddy_cn", normalizeCockpitSlug("CodeBuddy_CN"))

	require.True(t, isValidCockpitToken("abc-XYZ_123!@#"))
	require.False(t, isValidCockpitToken(""))
	require.False(t, isValidCockpitToken("has space"))
	require.False(t, isValidCockpitToken("tab\there"))
	require.False(t, isValidCockpitToken(strings.Repeat("x", 8193)))

	require.True(t, isValidCockpitUID("user_123-AB"))
	require.False(t, isValidCockpitUID(""))
	require.False(t, isValidCockpitUID("u@1"))
	require.False(t, isValidCockpitUID(strings.Repeat("x", 129)))

	require.Equal(t, "Example.COM", normalizeCockpitField("Example.COM"))

	v, ok := parseCockpitExpiresAtString("1700000000")
	require.True(t, ok)
	require.Equal(t, float64(1700000000), v)
	_, ok = parseCockpitExpiresAtString("not-a-number")
	require.False(t, ok)
	_, ok = parseCockpitExpiresAtString("NaN")
	require.False(t, ok)
}

// —— ④ 别名冲突（失败关闭，条目不计写入）——

func TestCockpitImport_AliasConflict(t *testing.T) {
	raw := map[string]interface{}{
		"access_token":  "at-a",
		"accessToken":   "at-b",
		"refresh_token": "rt",
		"uid":           "u1",
		"domain":        "d",
		"expires_at":    "1",
	}
	res := parseCockpitAccountEntry("codebuddy", raw)
	require.True(t, res.Invalid)
	require.Equal(t, ErrCockpitAliasConflict, res.Reason)

	rawOK := map[string]interface{}{
		"access_token":  "at-valid",
		"accessToken":   "at-valid",
		"refresh_token": "rt-valid",
		"refreshToken":  "rt-valid",
		"uid":           "user_123",
		"account.id":    "user_123",
		"enterprise_id": "ent_1",
		"enterpriseId":  "ent_1",
		"domain":        "example.com",
		"expires_at":    "1700000000",
		"expiresAt":     "1700000000",
	}
	resOK := parseCockpitAccountEntry("codebuddy", rawOK)
	require.False(t, resOK.Invalid, "别名一致应接受")
	require.Equal(t, "at-valid", resOK.Entry.AccessToken)
	require.Equal(t, "user_123", resOK.Entry.UID)
	require.True(t, resOK.Entry.HasExpiresAt)
	require.Equal(t, float64(1700000000), resOK.Entry.ExpiresAt)
}

// —— ⑥ 必填/字符集非法（失败关闭）——

func TestCockpitImport_FieldInvalid(t *testing.T) {
	base := func() map[string]interface{} {
		return map[string]interface{}{
			"access_token":  "at-valid",
			"refresh_token": "rt-valid",
			"domain":        "d",
			"expires_at":    "1",
		}
	}
	m := base()
	require.True(t, parseCockpitAccountEntry("codebuddy", m).Invalid)

	m = base()
	m["uid"] = "u@1"
	require.True(t, parseCockpitAccountEntry("codebuddy", m).Invalid)

	m = base()
	m["uid"] = "u1"
	m["access_token"] = "at has space"
	require.True(t, parseCockpitAccountEntry("codebuddy", m).Invalid)

	m = base()
	m["uid"] = "u1"
	m["expires_at"] = "not-number"
	require.True(t, parseCockpitAccountEntry("codebuddy", m).Invalid)
}

// —— ⑦ 同文件同键分组（方案 §1.6）——

func TestCockpitImport_GroupSameKey_IdenticalDedup(t *testing.T) {
	e := CockpitNormalizedEntry{
		Platform: "codebuddy", UID: "u1", AccessToken: "a", RefreshToken: "r",
		Domain: "d", EnterpriseID: "ent", ExpiresAt: 1, HasExpiresAt: true,
	}
	deduped, invalid, reasons := groupCockpitEntries([]CockpitNormalizedEntry{e, e, e})
	require.Equal(t, 2, deduped, "3 条完全一致 → 去重冗余 2 条")
	require.Equal(t, 0, invalid)
	require.Empty(t, reasons)
}

func TestCockpitImport_GroupSameKey_TwoIdentical(t *testing.T) {
	e := CockpitNormalizedEntry{Platform: "codebuddy", UID: "u1", AccessToken: "a", RefreshToken: "r"}
	deduped, _, _ := groupCockpitEntries([]CockpitNormalizedEntry{e, e})
	require.Equal(t, 1, deduped)
}

func TestCockpitImport_GroupSameKey_ConflictingInvalid(t *testing.T) {
	e1 := CockpitNormalizedEntry{Platform: "codebuddy", UID: "u1", AccessToken: "a", RefreshToken: "r"}
	e2 := CockpitNormalizedEntry{Platform: "codebuddy", UID: "u1", AccessToken: "DIFFERENT", RefreshToken: "r"}
	deduped, invalid, reasons := groupCockpitEntries([]CockpitNormalizedEntry{e1, e2})
	require.Equal(t, 0, deduped)
	require.Equal(t, 2, invalid, "整组失败关闭，一个不写")
	require.Equal(t, 2, reasons[ErrCockpitConflictingDuplicateUID])
}

func TestCockpitImport_GroupSameKey_MixedGroups(t *testing.T) {
	a := CockpitNormalizedEntry{Platform: "codebuddy", UID: "ua", AccessToken: "a", RefreshToken: "r"}
	b1 := CockpitNormalizedEntry{Platform: "openai", UID: "ub", AccessToken: "x", RefreshToken: "r"}
	b2 := CockpitNormalizedEntry{Platform: "openai", UID: "ub", AccessToken: "y", RefreshToken: "r"}
	deduped, invalid, reasons := groupCockpitEntries([]CockpitNormalizedEntry{a, a, a, b1, b2})
	require.Equal(t, 2, deduped)
	require.Equal(t, 2, invalid)
	require.Equal(t, 2, reasons[ErrCockpitConflictingDuplicateUID])
}
