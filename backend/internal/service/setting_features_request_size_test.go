//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// resetRequestSizeRoutingThresholdCacheForTest 清掉进程级缓存和 singleflight 记录，
// 让每个用例都真正回源，避免用例之间串值。
// 注意：atomic.Value.Store(nil) 会直接 panic，必须存一个 expiresAt=0 的空缓存对象
// （和 setting_service_platform_threshold_test.go 的清缓存写法一致），
// 这样缓存判定不命中，会重新去 repo 读。
func resetRequestSizeRoutingThresholdCacheForTest() {
	requestSizeRoutingThresholdSF.Forget(SettingKeyRequestSizeRoutingThreshold)
	requestSizeRoutingThresholdCache.Store(&cachedRequestSizeRoutingThreshold{})
}

func newSettingServiceForRequestSizeRoutingThresholdTest(t *testing.T, seed map[string]string) *SettingService {
	t.Helper()
	resetRequestSizeRoutingThresholdCacheForTest()
	t.Cleanup(resetRequestSizeRoutingThresholdCacheForTest)
	repo := newMockSettingRepo()
	for k, v := range seed {
		repo.data[k] = v
	}
	return NewSettingService(repo, &config.Config{})
}

// 组 1：service 本身是 nil、或 settingRepo 是 nil，都拿不到可信阈值，
// 返回 ok=false（= 阈值不可知），不去读 repo，也不碰缓存。
// 语义要点：拿不到就说不知道，绝不编一个默认数字出来。
func TestGetRequestSizeRoutingThreshold_NilServiceAndRepo(t *testing.T) {
	resetRequestSizeRoutingThresholdCacheForTest()
	t.Cleanup(resetRequestSizeRoutingThresholdCacheForTest)

	var nilSvc *SettingService
	value, ok := nilSvc.GetRequestSizeRoutingThreshold(context.Background())
	require.False(t, ok, "nil service 必须报告阈值不可知")
	require.Equal(t, RequestSizeRoutingThresholdUnknown, value)

	svc := &SettingService{} // settingRepo 为 nil
	value, ok = svc.GetRequestSizeRoutingThreshold(context.Background())
	require.False(t, ok, "nil settingRepo 必须报告阈值不可知")
	require.Equal(t, RequestSizeRoutingThresholdUnknown, value)
}

// 组 2：repo 返回合法正整数，按配置值返回。
func TestGetRequestSizeRoutingThreshold_ValidStoredValue(t *testing.T) {
	svc := newSettingServiceForRequestSizeRoutingThresholdTest(t, map[string]string{
		SettingKeyRequestSizeRoutingThreshold: "128000",
	})

	value, ok := svc.GetRequestSizeRoutingThreshold(context.Background())
	require.True(t, ok)
	require.Equal(t, 128000, value)
}

// 组 3：0、负数、非数字都是非法值，一律报告「不可知」（不编默认数）。
func TestGetRequestSizeRoutingThreshold_InvalidStoredValuesAreUnknown(t *testing.T) {
	for _, raw := range []string{"0", "-1", "abc"} {
		t.Run(raw, func(t *testing.T) {
			svc := newSettingServiceForRequestSizeRoutingThresholdTest(t, map[string]string{
				SettingKeyRequestSizeRoutingThreshold: raw,
			})

			value, ok := svc.GetRequestSizeRoutingThreshold(context.Background())
			require.False(t, ok, "非法值必须报告阈值不可知")
			require.Equal(t, RequestSizeRoutingThresholdUnknown, value)
		})
	}
}

// 组 4：设置不存在（ErrSettingNotFound）时同样报告「不可知」。
func TestGetRequestSizeRoutingThreshold_SettingNotFoundIsUnknown(t *testing.T) {
	svc := newSettingServiceForRequestSizeRoutingThresholdTest(t, nil)
	svc.settingRepo.(*mockSettingRepo).getValueErr = ErrSettingNotFound

	value, ok := svc.GetRequestSizeRoutingThreshold(context.Background())
	require.False(t, ok, "配置行缺失必须报告阈值不可知")
	require.Equal(t, RequestSizeRoutingThresholdUnknown, value)
}
