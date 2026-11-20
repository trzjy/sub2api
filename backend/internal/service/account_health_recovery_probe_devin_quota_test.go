package service

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// devinQuotaTripAccount 构造一个携带健康熔断 reason 的停调账号，供 isHealthBreakerTrip 判定。
func devinQuotaTripAccount(id int64, platform, marker string, until *time.Time) *Account {
	state := TempUnschedState{
		StatusCode:     http.StatusOK,
		MatchedKeyword: marker,
		ErrorMessage:   "devin upstream daily usage quota exhausted",
	}
	if until != nil {
		state.UntilUnix = until.Unix()
	}
	reason, _ := json.Marshal(state)
	return &Account{
		ID:                      id,
		Platform:                platform,
		TempUnschedulableUntil:  until,
		TempUnschedulableReason: string(reason),
	}
}

// DQ-C2 目标：Devin 额度耗尽账号的扫描资格与 until 是否到期解耦——until 已过期仍被
// health probe sweep 认领，直到探针连续成功清除 marker。
func TestIsHealthBreakerTripDevinQuotaUntilExpiredStillTrips(t *testing.T) {
	expired := time.Now().Add(-time.Hour)
	acc := devinQuotaTripAccount(2201, PlatformDevin, devinUpstreamQuotaExhaustedMarker, &expired)
	p := &AccountHealthRecoveryProbeService{}
	require.True(t, p.isHealthBreakerTrip(acc, time.Now()),
		"Devin 额度耗尽账号 until 已过期仍须被探针扫描认领")
}

// 既有行为不变：until 在未来时，devin 账号照常返回 true。
func TestIsHealthBreakerTripDevinQuotaUntilFutureTrips(t *testing.T) {
	future := time.Now().Add(time.Hour)
	acc := devinQuotaTripAccount(2202, PlatformDevin, devinUpstreamQuotaExhaustedMarker, &future)
	p := &AccountHealthRecoveryProbeService{}
	require.True(t, p.isHealthBreakerTrip(acc, time.Now()),
		"until 未过期时 devin 账号仍可被探针认领（既有行为不变）")
}

// 既有行为不变：非 devin marker + until 已过期 → 短路返回 false。
func TestIsHealthBreakerTripDevinQuotaNonDevinMarkerExpiredDoesNotTrip(t *testing.T) {
	expired := time.Now().Add(-time.Hour)
	acc := devinQuotaTripAccount(2203, PlatformDevin, openAIAPIKeyHealthBreakerReason, &expired)
	p := &AccountHealthRecoveryProbeService{}
	require.False(t, p.isHealthBreakerTrip(acc, time.Now()),
		"非 devin marker 且 until 已过期仍按既有逻辑短路返回 false")
}
