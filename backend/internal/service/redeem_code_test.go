package service

import (
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// codeFormatRE 匹配 GenerateRandomRedeemCode 生成的码格式：大写 hex 四段 XXXX-XXXX-XXXX-XXXX。
var codeFormatRE = regexp.MustCompile(`^[0-9A-F]{8}-[0-9A-F]{8}-[0-9A-F]{8}-[0-9A-F]{8}$`)

// TestGenerateRandomRedeemCodeFormat 验证父包唯一导出生成函数（D6F-A 现场生成卡密复用）
// 的码格式：32 位大写 hex → 四段 XXXX-XXXX-XXXX-XXXX 大写，且全局唯一。
func TestGenerateRandomRedeemCodeFormat(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 200; i++ {
		code, err := GenerateRandomRedeemCode()
		require.NoError(t, err)
		require.Regexp(t, codeFormatRE, code, "码须为大写 XXXX-XXXX-XXXX-XXXX")
		require.Len(t, code, 35, "四段大写 hex + 3 连字符 = 35 字符")
		require.NotEqual(t, "", code)
		seen[code] = struct{}{}
	}
	require.Len(t, seen, 200, "生成码须唯一")
}

// TestGenerateRandomCodeFitsColumn 生成码必须放得进 redeem_codes.code 列
// （迁移 248 放宽到 VARCHAR(64)），否则创建时 ent MaxLen 校验直接失败。
func TestGenerateRandomCodeFitsColumn(t *testing.T) {
	s := &RedeemService{}
	seen := make(map[string]struct{})
	for i := 0; i < 100; i++ {
		code, err := s.GenerateRandomCode()
		require.NoError(t, err)
		require.LessOrEqual(t, len(code), 64)
		require.NotEqual(t, "", code)
		seen[code] = struct{}{}
	}
	require.Len(t, seen, 100)
}

func TestRedeemCodeExpiry(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	tests := []struct {
		name        string
		code        RedeemCode
		wantExpired bool
		wantCanUse  bool
	}{
		{
			name:        "unused without expiry can be used",
			code:        RedeemCode{Status: StatusUnused},
			wantExpired: false,
			wantCanUse:  true,
		},
		{
			name:        "unused before expiry can be used",
			code:        RedeemCode{Status: StatusUnused, ExpiresAt: &future},
			wantExpired: false,
			wantCanUse:  true,
		},
		{
			name:        "unused after expiry cannot be used",
			code:        RedeemCode{Status: StatusUnused, ExpiresAt: &past},
			wantExpired: true,
			wantCanUse:  false,
		},
		{
			name:        "explicit expired status is expired",
			code:        RedeemCode{Status: StatusExpired},
			wantExpired: true,
			wantCanUse:  false,
		},
		{
			name:        "used code remains used even after expiry time",
			code:        RedeemCode{Status: StatusUsed, ExpiresAt: &past},
			wantExpired: false,
			wantCanUse:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.wantExpired, tt.code.IsExpiredAt(now))
			require.Equal(t, tt.wantCanUse, tt.code.CanUse())
		})
	}
}
