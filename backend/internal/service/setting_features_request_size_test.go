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

// 组 1：service 本身是 nil、或 settingRepo 是 nil，都直接回退默认阈值，
// 不去读 repo，也不碰缓存。
func TestGetRequestSizeRoutingThreshold_NilServiceAndRepo(t *testing.T) {
	resetRequestSizeRoutingThresholdCacheForTest()
	t.Cleanup(resetRequestSizeRoutingThresholdCacheForTest)

	var nilSvc *SettingService
	require.Equal(t, LargeRequestOnlyInputThreshold,
		nilSvc.GetRequestSizeRoutingThreshold(context.Background()))

	svc := &SettingService{} // settingRepo 为 nil
	require.Equal(t, LargeRequestOnlyInputThreshold,
		svc.GetRequestSizeRoutingThreshold(context.Background()))
}

// 组 2：repo 返回合法正整数，按配置值返回。
func TestGetRequestSizeRoutingThreshold_ValidStoredValue(t *testing.T) {
	svc := newSettingServiceForRequestSizeRoutingThresholdTest(t, map[string]string{
		SettingKeyRequestSizeRoutingThreshold: "128000",
	})

	require.Equal(t, 128000, svc.GetRequestSizeRoutingThreshold(context.Background()))
}

// 组 3：0、负数、非数字都是非法值，一律回退默认阈值。
func TestGetRequestSizeRoutingThreshold_InvalidStoredValuesFallBack(t *testing.T) {
	for _, raw := range []string{"0", "-1", "abc"} {
		t.Run(raw, func(t *testing.T) {
			svc := newSettingServiceForRequestSizeRoutingThresholdTest(t, map[string]string{
				SettingKeyRequestSizeRoutingThreshold: raw,
			})

			require.Equal(t, LargeRequestOnlyInputThreshold,
				svc.GetRequestSizeRoutingThreshold(context.Background()))
		})
	}
}

// 组 4：设置不存在（ErrSettingNotFound）时回退默认阈值。
func TestGetRequestSizeRoutingThreshold_SettingNotFoundFallsBack(t *testing.T) {
	svc := newSettingServiceForRequestSizeRoutingThresholdTest(t, nil)
	svc.settingRepo.(*mockSettingRepo).getValueErr = ErrSettingNotFound

	require.Equal(t, LargeRequestOnlyInputThreshold,
		svc.GetRequestSizeRoutingThreshold(context.Background()))
}
