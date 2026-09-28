package service

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const cockpitTestMasterSecret = "test-master-secret-required-minimum-length-32b+"

func newCockpitTestSigner() *CockpitPreviewReceiptSigner {
	return NewCockpitPreviewReceiptSigner(cockpitTestMasterSecret)
}

func cockpitTestSHA(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// TestCockpitPreviewReceiptRoundTrip 正常签发 → 验证通过（主体/版本/未过期一致）。
func TestCockpitPreviewReceiptRoundTrip(t *testing.T) {
	s := newCockpitTestSigner()
	rawSha := cockpitTestSHA("upload-bytes")
	token, err := s.Issue("42", rawSha)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	r, err := s.Verify(token, "42")
	require.NoError(t, err)
	require.Equal(t, "42", r.Operator)
	require.Equal(t, rawSha, r.RawSHA256)
	require.Equal(t, cockpitImportParserVersion, r.ParserVersion)
	require.Greater(t, r.ExpiresAt, r.IssuedAt)
	require.Equal(t, int64(cockpitPreviewReceiptTTL.Seconds()), r.ExpiresAt-r.IssuedAt)
}

// TestCockpitPreviewReceiptTamperedPayload 篡改载荷 → 签名不符 → PREVIEW_RECEIPT_INVALID。
func TestCockpitPreviewReceiptTamperedPayload(t *testing.T) {
	s := newCockpitTestSigner()
	token, err := s.Issue("42", cockpitTestSHA("x"))
	require.NoError(t, err)

	parts := strings.SplitN(token, ".", 2)
	require.Len(t, parts, 2)
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	payload[len(payload)-1] ^= 0xFF // 翻转末尾字节（保持 base64 合法）
	tampered := base64.RawURLEncoding.EncodeToString(payload) + "." + parts[1]

	_, err = s.Verify(tampered, "42")
	require.Error(t, err)
	require.Equal(t, ErrCockpitPreviewReceiptInvalid, err.Error())
}

// TestCockpitPreviewReceiptOperatorMismatch 主体不匹配 → PREVIEW_RECEIPT_INVALID。
func TestCockpitPreviewReceiptOperatorMismatch(t *testing.T) {
	s := newCockpitTestSigner()
	token, err := s.Issue("42", cockpitTestSHA("x"))
	require.NoError(t, err)
	_, err = s.Verify(token, "99")
	require.Error(t, err)
	require.Equal(t, ErrCockpitPreviewReceiptInvalid, err.Error())
}

// TestCockpitPreviewReceiptVersionMismatch 解析器版本不匹配 → PREVIEW_RECEIPT_INVALID。
func TestCockpitPreviewReceiptVersionMismatch(t *testing.T) {
	s := newCockpitTestSigner()
	r := &CockpitPreviewReceipt{
		Operator:      "42",
		RawSHA256:     cockpitTestSHA("x"),
		ParserVersion: cockpitImportParserVersion + 1, // 旧/新版本，与当前不一致
		IssuedAt:      time.Now().Unix(),
		ExpiresAt:     time.Now().Unix() + 100,
	}
	token, err := s.Sign(r)
	require.NoError(t, err)
	_, err = s.Verify(token, "42")
	require.Error(t, err)
	require.Equal(t, ErrCockpitPreviewReceiptInvalid, err.Error())
}

// TestCockpitPreviewReceiptExpired TTL 边界：ExpiresAt == now 视为过期；ExpiresAt == now+1 通过。
func TestCockpitPreviewReceiptExpired(t *testing.T) {
	s := newCockpitTestSigner()

	expired := &CockpitPreviewReceipt{
		Operator: "42", RawSHA256: cockpitTestSHA("x"),
		ParserVersion: cockpitImportParserVersion,
		IssuedAt:      time.Now().Unix(), ExpiresAt: time.Now().Unix(), // 等于 now
	}
	tokE, err := s.Sign(expired)
	require.NoError(t, err)
	_, err = s.Verify(tokE, "42")
	require.Error(t, err)
	require.Equal(t, ErrCockpitPreviewReceiptInvalid, err.Error())

	valid := &CockpitPreviewReceipt{
		Operator: "42", RawSHA256: cockpitTestSHA("x"),
		ParserVersion: cockpitImportParserVersion,
		IssuedAt: time.Now().Unix(), ExpiresAt: time.Now().Unix() + 1, // 差 1 秒，未过期
	}
	tokV, err := s.Sign(valid)
	require.NoError(t, err)
	_, err = s.Verify(tokV, "42")
	require.NoError(t, err)
}

// TestCockpitPreviewReceiptCrossKey 不同主密钥派生出的 signer 互不可验（域名分离）。
func TestCockpitPreviewReceiptCrossKey(t *testing.T) {
	s1 := NewCockpitPreviewReceiptSigner("secret-A-32-bytes-minimum-length-ok")
	s2 := NewCockpitPreviewReceiptSigner("secret-B-32-bytes-minimum-length-ok")
	require.NotEqual(t, s1.key, s2.key, "派生 key 必须随主密钥不同")
	token, err := s1.Issue("42", cockpitTestSHA("x"))
	require.NoError(t, err)
	_, err = s2.Verify(token, "42")
	require.Error(t, err)
	require.Equal(t, ErrCockpitPreviewReceiptInvalid, err.Error())
}

// TestCockpitPreviewReceiptUnconfigured 主密钥为空 → signer 未配置，签发/校验一律失败。
func TestCockpitPreviewReceiptUnconfigured(t *testing.T) {
	s := NewCockpitPreviewReceiptSigner("")
	require.False(t, s.Configured())
	_, err := s.Issue("42", cockpitTestSHA("x"))
	require.Error(t, err)
	_, err = s.Verify("x.y", "42")
	require.Error(t, err)
}
