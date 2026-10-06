package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeCockpitCommitRepo 是 CockpitImportCommitRepository 的内存假实现。
type fakeCockpitCommitRepo struct {
	lastPayloads []CockpitImportAccountPayload
	lastResult   CockpitImportCommitResult
	lastErr      error
	calls        int
}

func (f *fakeCockpitCommitRepo) CommitCockpitImport(_ context.Context, payloads []CockpitImportAccountPayload) (CockpitImportCommitResult, error) {
	f.calls++
	f.lastPayloads = payloads
	return f.lastResult, f.lastErr
}

func cockpitMustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// cockpitRandomSentinel 运行时生成随机哨兵（§1.7 防止快照式假通过），形如 WBSENTINEL-<hex>。
func cockpitRandomSentinel(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return fmt.Sprintf("WBSENTINEL-%s", hex.EncodeToString(b))
}

// buildCockpitBackupJSON 构造 cockpit 备份导入 JSON；slug 控制平台归属，variant 控制条目合法性。
// sentinel 放置于 access_token 字段（作为"唯一哨兵 token"），用于 §1.7 泄漏负向回归。
// 注意：当前映射表 enabled 为空，字段级校验仅在 enabled slug 下触发；unknown/pending/unsupported
// slug 仅做计数、不回显任何条目字段。schema_error 触发顶层信封失败关闭。
func buildCockpitBackupJSON(sentinel, slug, variant string) []byte {
	doc := map[string]any{
		"schema":  "cockpit-tools.data-transfer",
		"version": 1,
	}
	if variant == "schema_error" {
		doc["schema"] = "wrong-schema" // 顶层信封失败关闭（SCHEMA_VERSION_UNSUPPORTED）
		return cockpitMustJSON(doc)
	}
	var entry map[string]any
	switch variant {
	case "missing_uid":
		entry = map[string]any{"access_token": sentinel, "refresh_token": "r"}
	case "bad_expires":
		entry = map[string]any{"uid": "u1", "access_token": sentinel, "refresh_token": "r", "expires_at": "not-a-number"}
	default: // valid
		entry = map[string]any{"uid": "u1", "access_token": sentinel, "refresh_token": "r", "expires_at": 123}
	}
	doc["accounts"] = map[string]any{
		"platforms": map[string]any{
			slug: map[string]any{"exported_data": []any{entry}},
		},
	}
	return cockpitMustJSON(doc)
}

// TestCockpitCommitNoReceipt 缺少凭证 → 失败关闭 PREVIEW_RECEIPT_INVALID。
func TestCockpitCommitNoReceipt(t *testing.T) {
	svc := NewCockpitImportCommitServiceWithReceipt(&fakeCockpitCommitRepo{}, newCockpitTestSigner())
	sentinel := cockpitRandomSentinel(t)
	_, err := svc.CommitCockpitImport(context.Background(), "42", "", buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "valid"))
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitPreviewReceiptInvalid))
}

// TestCockpitCommitManifestMismatch 字节流与凭证内 raw_sha256 不一致 → MANIFEST_MISMATCH。
func TestCockpitCommitManifestMismatch(t *testing.T) {
	signer := newCockpitTestSigner()
	svc := NewCockpitImportCommitServiceWithReceipt(&fakeCockpitCommitRepo{}, signer)
	// 凭证针对其它内容签发，提交内容不同 → SHA 不匹配。
	receipt, err := signer.Issue("42", cockpitTestSHA("other-content"))
	require.NoError(t, err)
	sentinel := cockpitRandomSentinel(t)
	_, err = svc.CommitCockpitImport(context.Background(), "42", receipt, buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "valid"))
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitPreviewManifestMismatch))
}

// TestCockpitCommitVersionChanged 凭证解析器版本与当前不一致 → PREVIEW_RECEIPT_INVALID。
func TestCockpitCommitVersionChanged(t *testing.T) {
	signer := newCockpitTestSigner()
	svc := NewCockpitImportCommitServiceWithReceipt(&fakeCockpitCommitRepo{}, signer)
	sentinel := cockpitRandomSentinel(t)
	raw := buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "valid")
	rawSha := cockpitTestSHA(string(raw))
	// 手工签发一张版本错误的凭证（signer.Sign 透传任意 ParserVersion）。
	r := &CockpitPreviewReceipt{
		Operator:      "42",
		RawSHA256:     rawSha,
		ParserVersion: cockpitImportParserVersion + 7,
		IssuedAt:      timeNowUnix(),
		ExpiresAt:     timeNowUnix() + 100,
	}
	receipt, err := signer.Sign(r)
	require.NoError(t, err)
	_, err = svc.CommitCockpitImport(context.Background(), "42", receipt, raw)
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitPreviewReceiptInvalid))
}

// TestCockpitCommitSuccessCreatedZero enabled 为空：成功路径 created=0，不调仓储写路径。
func TestCockpitCommitSuccessCreatedZero(t *testing.T) {
	repo := &fakeCockpitCommitRepo{}
	signer := newCockpitTestSigner()
	svc := NewCockpitImportCommitServiceWithReceipt(repo, signer)
	sentinel := cockpitRandomSentinel(t)
	raw := buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "valid")
	rawSha := cockpitTestSHA(string(raw))
	receipt, err := signer.Issue("42", rawSha)
	require.NoError(t, err)
	res, err := svc.CommitCockpitImport(context.Background(), "42", receipt, raw)
	require.NoError(t, err)
	require.Equal(t, 0, res.Created)
	require.Equal(t, 0, res.SkippedExisting)
	// enabled 为空，Commit 以空载荷短路，不应调用仓储写路径。
	require.Equal(t, 0, repo.calls)
}

// TestCockpitPreviewSuccess 预览成功：零副作用，返回计数摘要 + 凭证，且不含任何凭证材料。
func TestCockpitPreviewSuccess(t *testing.T) {
	signer := newCockpitTestSigner()
	svc := NewCockpitImportCommitServiceWithReceipt(&fakeCockpitCommitRepo{}, signer)
	sentinel := cockpitRandomSentinel(t)
	raw := buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "valid")
	res, err := svc.PreviewCockpitImport(context.Background(), "42", raw)
	require.NoError(t, err)
	require.Equal(t, cockpitTestSHA(string(raw)), res.RawSHA256)
	require.Equal(t, cockpitImportParserVersion, res.ParserVersion)
	require.Equal(t, 1, res.UnknownAccounts)
	require.NotEmpty(t, res.Receipt)
	// 响应摘要不得包含哨兵 token（零凭证材料）。
	require.NotContains(t, res.RawSHA256, sentinel)
	b, _ := json.Marshal(res)
	require.NotContains(t, string(b), sentinel)
}

// TestCockpitPreviewParseError 顶层信封失败关闭（schema_error）→ 错误不得泄露哨兵。
func TestCockpitPreviewParseError(t *testing.T) {
	signer := newCockpitTestSigner()
	svc := NewCockpitImportCommitServiceWithReceipt(&fakeCockpitCommitRepo{}, signer)
	sentinel := cockpitRandomSentinel(t)
	_, err := svc.PreviewCockpitImport(context.Background(), "42", buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "schema_error"))
	require.Error(t, err)
	require.True(t, IsCockpitErrorCode(err, ErrCockpitSchemaVersionUnsupported))
	require.NotContains(t, err.Error(), sentinel, "失败关闭错误不得泄露哨兵")
}

// TestCockpitSentinelNoLeakService §1.7 哨兵泄漏负向（service 层）：合法 + 非法条目形态下，
// 返回的凭证 token、摘要、结果 JSON 与错误字符串均不得包含哨兵；并白盒验证字段校验器不回显哨兵。
func TestCockpitSentinelNoLeakService(t *testing.T) {
	signer := newCockpitTestSigner()
	svc := NewCockpitImportCommitServiceWithReceipt(&fakeCockpitCommitRepo{}, signer)
	sentinel := cockpitRandomSentinel(t)

	// (a) 合法条目（unknown slug）→ preview 成功，凭证/摘要/结果均无哨兵；再走 commit 全链路。
	rawValid := buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "valid")
	pres, err := svc.PreviewCockpitImport(context.Background(), "42", rawValid)
	require.NoError(t, err)
	require.NotContains(t, pres.Receipt, sentinel)
	require.NotContains(t, pres.RawSHA256, sentinel)
	pb, _ := json.Marshal(pres)
	require.NotContains(t, string(pb), sentinel)
	receipt, err := signer.Issue("42", cockpitTestSHA(string(rawValid)))
	require.NoError(t, err)
	cres, err := svc.CommitCockpitImport(context.Background(), "42", receipt, rawValid)
	require.NoError(t, err)
	cb, _ := json.Marshal(cres)
	require.NotContains(t, string(cb), sentinel)

	// (b) schema_error：失败关闭，错误字符串无哨兵。
	_, err = svc.PreviewCockpitImport(context.Background(), "42", buildCockpitBackupJSON(sentinel, "zzz-unknown-slug", "schema_error"))
	require.Error(t, err)
	require.NotContains(t, err.Error(), sentinel)

	// (c) pending slug 下 missing_uid / bad_expires：当前 enabled 为空故字段不校验，
	// 解析成功（计为 pending），响应 JSON 无哨兵（条目字段从未被回显）。
	for _, variant := range []string{"missing_uid", "bad_expires"} {
		raw := buildCockpitBackupJSON(sentinel, "codebuddy_cn", variant)
		p, perr := svc.PreviewCockpitImport(context.Background(), "42", raw)
		require.NoError(t, perr, "variant=%s 当前 enabled 为空，字段不校验，应成功", variant)
		pj, _ := json.Marshal(p)
		require.NotContains(t, string(pj), sentinel)
	}

	// (d) 白盒：字段校验器直接处理非法条目时，不得回显哨兵 token（reason 中无哨兵）。
	missingUID := parseCockpitAccountEntry("codebuddy", map[string]any{"access_token": sentinel, "refresh_token": "r"})
	require.True(t, missingUID.Invalid)
	require.NotContains(t, missingUID.Reason, sentinel)

	badExpires := parseCockpitAccountEntry("codebuddy", map[string]any{"uid": "u1", "access_token": sentinel, "refresh_token": "r", "expires_at": "not-a-number"})
	require.True(t, badExpires.Invalid)
	require.NotContains(t, badExpires.Reason, sentinel)
}

// timeNowUnix 是测试内时间辅助。
func timeNowUnix() int64 { return time.Now().Unix() }
