//go:build unit

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// healthBreakerSettingRepo is a minimal in-memory SettingRepository double. It
// supports GetValue/Get/GetMultiple/Set/SetMultiple so both the GET round-trip
// (through GetAllSettings -> GetMultiple) and the PUT (SetOpenAIAPIKeyHealthBreakerSettings
// -> Set, plus UpdateSettingsWithAuthSourceDefaultsOmitting -> SetMultiple) work.
type healthBreakerSettingRepo struct {
	service.SettingRepository
	values map[string]string
}

func (r *healthBreakerSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	if v, ok := r.values[key]; ok {
		return v, nil
	}
	return "", nil
}
func (r *healthBreakerSettingRepo) Get(_ context.Context, key string) (*service.Setting, error) {
	return &service.Setting{Key: key, Value: r.values[key]}, nil
}
func (r *healthBreakerSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := r.values[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}
func (r *healthBreakerSettingRepo) GetAll(_ context.Context) (map[string]string, error) {
	out := make(map[string]string, len(r.values))
	for k, v := range r.values {
		out[k] = v
	}
	return out, nil
}
func (r *healthBreakerSettingRepo) Set(_ context.Context, key, value string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return nil
}
func (r *healthBreakerSettingRepo) SetMultiple(_ context.Context, m map[string]string) error {
	if r.values == nil {
		r.values = map[string]string{}
	}
	for k, v := range m {
		r.values[k] = v
	}
	return nil
}

// TestSettingHandler_HealthBreaker_PutGetRoundTrip verifies the admin settings
// GET/PUT echoes the account health circuit breaker configuration (API-layer
// read/write round-trip required by the acceptance criteria).
func TestSettingHandler_HealthBreaker_PutGetRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &healthBreakerSettingRepo{values: map[string]string{}}
	svc := service.NewSettingService(repo, &config.Config{})
	handler := NewSettingHandler(svc, nil, nil, nil, nil, nil, nil)

	putBody := map[string]any{
		"openai_apikey_health_breaker_settings": map[string]any{
			"enabled":           true,
			"window_minutes":    5,
			"failure_threshold": 4,
			"cooldown_minutes":  5,
			"scope_platforms":   []any{"openai", "deepseek"},
			"include_grok":      false,
			"watch_ratio":       0.4,
			"warning_ratio":     0.7,
			"probe": map[string]any{
				"enabled":          false,
				"interval_seconds": 60,
				"max_attempts":     10,
			},
		},
	}
	raw, err := json.Marshal(putBody)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/admin/settings", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateSettings(c)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The dedicated settings key must be persisted.
	require.NotEmpty(t, repo.values[service.SettingKeyOpenAIAPIKeyHealthBreakerSettings], "health breaker settings key not persisted")

	// GET must echo the same configuration back.
	rec2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(rec2)
	c2.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	handler.GetSettings(c2)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	var resp response.Response
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp))
	data, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	hb, ok := data["openai_apikey_health_breaker_settings"].(map[string]any)
	require.True(t, ok, "health breaker settings missing from GET response")
	require.Equal(t, true, hb["enabled"])
	require.Equal(t, float64(5), hb["window_minutes"])
	require.Equal(t, float64(4), hb["failure_threshold"])
	require.Equal(t, float64(5), hb["cooldown_minutes"])
	require.Equal(t, []any{"openai", "deepseek"}, hb["scope_platforms"])
	require.Equal(t, false, hb["include_grok"])
	require.Equal(t, 0.4, hb["watch_ratio"])
	require.Equal(t, 0.7, hb["warning_ratio"])

	probe, ok := hb["probe"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, false, probe["enabled"])
	require.Equal(t, float64(60), probe["interval_seconds"])
	require.Equal(t, float64(10), probe["max_attempts"])
}
