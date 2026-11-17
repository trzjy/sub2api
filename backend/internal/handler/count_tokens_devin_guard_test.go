//go:build unit

package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestIsDevinCountTokensUnsupported(t *testing.T) {
	tests := []struct {
		name    string
		account *service.Account
		want    bool
	}{
		{name: "devin", account: &service.Account{Platform: service.PlatformDevin}, want: true},
		{name: "anthropic", account: &service.Account{Platform: service.PlatformAnthropic}, want: false},
		{name: "gemini", account: &service.Account{Platform: service.PlatformGemini}, want: false},
		{name: "cursor", account: &service.Account{Platform: service.PlatformCursor}, want: false},
		{name: "nil", account: nil, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isDevinCountTokensUnsupported(tt.account))
		})
	}
}
