//go:build unit

package admin

import (
	"net/http"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/stretchr/testify/require"
)

// request_size_routing_threshold 是 value 型字段：显式传值写入，省略则保留旧值，
// 显式传非正数返回 400。验证其 omitted/显式两条路径与回包映射。
func TestUpdateSettingsRequestSizeRoutingThresholdExplicitValue(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{
		service.SettingKeyRequestSizeRoutingThreshold: "200000",
	})

	rec := doUpdateSettings(t, h, map[string]any{
		"request_size_routing_threshold": 123456,
	}, nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "123456", repo.values[service.SettingKeyRequestSizeRoutingThreshold])
	require.Contains(t, rec.Body.String(), `"request_size_routing_threshold":123456`)
}

func TestUpdateSettingsRequestSizeRoutingThresholdOmittedKeepsPrevious(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{
		service.SettingKeyRequestSizeRoutingThreshold: "200000",
	})

	rec := doUpdateSettings(t, h, map[string]any{
		"risk_control_enabled": true,
	}, nil)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "200000", repo.values[service.SettingKeyRequestSizeRoutingThreshold],
		"字段省略时必须保留上一次的值，不得回退到零值")
}

func TestUpdateSettingsRequestSizeRoutingThresholdRejectsNonPositive(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value int
	}{
		{"zero", 0},
		{"negative", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo := newStepUpSwitchTestHandler(t, map[string]string{
				service.SettingKeyRequestSizeRoutingThreshold: "200000",
			})

			rec := doUpdateSettings(t, h, map[string]any{
				"request_size_routing_threshold": tc.value,
			}, nil)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Contains(t, rec.Body.String(), "request_size_routing_threshold must be positive")
			require.Equal(t, "200000", repo.values[service.SettingKeyRequestSizeRoutingThreshold],
				"被拒绝的请求不得写库")
		})
	}
}
