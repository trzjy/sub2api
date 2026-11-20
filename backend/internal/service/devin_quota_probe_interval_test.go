//go:build unit

// C1 外审 must_fix：Devin 额度耗尽停调（HTTP 200 + trailer error + marker）
// 必须归入慢类，走 5 分钟额度补探节奏，避免对真实对话探针做 5 秒级高频重试
// 持续消耗上游额度。本文件表驱动锁定 isQuotaExhaustedTrip 的分类行为。

package service

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIsQuotaExhaustedTrip 表驱动覆盖 429 慢类判定及 Devin 200+marker 新增分支。
func TestIsQuotaExhaustedTrip(t *testing.T) {
	marshalReason := func(st TempUnschedState) string {
		b, err := json.Marshal(st)
		require.NoError(t, err)
		return string(b)
	}

	tests := []struct {
		name   string
		reason string
		want   bool
	}{
		{
			name: "Devin额度耗尽200+marker归慢类",
			reason: marshalReason(TempUnschedState{
				StatusCode:     http.StatusOK,
				MatchedKeyword: devinUpstreamQuotaExhaustedMarker,
			}),
			want: true,
		},
		{
			name: "429任意keyword归慢类",
			reason: marshalReason(TempUnschedState{
				StatusCode:     http.StatusTooManyRequests,
				MatchedKeyword: "some_other_keyword",
			}),
			want: true,
		},
		{
			name: "200+其它keyword非慢类",
			reason: marshalReason(TempUnschedState{
				StatusCode:     http.StatusOK,
				MatchedKeyword: "some_other_keyword",
			}),
			want: false,
		},
		{
			name: "401+oauth401marker保持秒级现状",
			reason: marshalReason(TempUnschedState{
				StatusCode:     http.StatusUnauthorized,
				MatchedKeyword: oauth401RecoverableMarker,
			}),
			want: false,
		},
		{
			name:   "非法JSON reason非慢类",
			reason: `{"status_code":`,
			want:   false,
		},
	}

	p := &AccountHealthRecoveryProbeService{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			acc := &Account{ID: 1, TempUnschedulableReason: tc.reason}
			require.Equal(t, tc.want, p.isQuotaExhaustedTrip(acc))
		})
	}
}
