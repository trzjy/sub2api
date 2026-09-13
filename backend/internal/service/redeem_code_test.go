package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
