package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// envelope mirrors the standard API success wrapper ({code,message,data}).
type envelope struct {
	Data *dto.SystemSettings `json:"data"`
}

func doGetSettings(t *testing.T, h *SettingHandler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/settings", nil)
	h.GetSettings(c)
	return rec
}

// 上限 20：超量必须拒绝。
func TestFooterLinkRejectsOverLimit(t *testing.T) {
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{})

	links := make([]map[string]any, 21)
	for i := range links {
		links[i] = map[string]any{"name": "link", "url": "https://example.com/", "sort_order": i}
	}
	rec := doUpdateSettings(t, h, map[string]any{"footer_links": links}, nil)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "Too many footer links")
}

// name 必填。
func TestFooterLinkRejectsEmptyName(t *testing.T) {
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{})
	rec := doUpdateSettings(t, h, map[string]any{
		"footer_links": []map[string]any{{"name": "", "url": "https://example.com/", "sort_order": 1}},
	}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "Footer link name is required")
}

// url 必须是绝对 http(s)。
func TestFooterLinkRejectsNonAbsoluteURL(t *testing.T) {
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{})
	rec := doUpdateSettings(t, h, map[string]any{
		"footer_links": []map[string]any{{"name": "x", "url": "javascript:alert(1)", "sort_order": 1}},
	}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "absolute http(s) URL")
}

// 数组内 id 必须唯一。
func TestFooterLinkRejectsDuplicateID(t *testing.T) {
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{})
	rec := doUpdateSettings(t, h, map[string]any{
		"footer_links": []map[string]any{
			{"id": "dup", "name": "a", "url": "https://example.com/a", "sort_order": 1},
			{"id": "dup", "name": "b", "url": "https://example.com/b", "sort_order": 2},
		},
	}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "Duplicate footer link ID: dup")
}

// 显式 id 仅允许 a-zA-Z0-9-_。
func TestFooterLinkRejectsInvalidIDChars(t *testing.T) {
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{})
	rec := doUpdateSettings(t, h, map[string]any{
		"footer_links": []map[string]any{{"id": "bad id!", "name": "x", "url": "https://example.com/", "sort_order": 1}},
	}, nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "invalid characters")
}

// 合法写入：服务端自动生成 id，并持久化。
func TestFooterLinkValidWriteGeneratesID(t *testing.T) {
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{})
	rec := doUpdateSettings(t, h, map[string]any{
		"footer_links": []map[string]any{{"name": "评测站", "url": "https://example.com/", "sort_order": 1}},
	}, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	stored := repo.values[service.SettingKeyFooterLinks]
	require.NotEmpty(t, stored)
	var parsed []dto.FooterLink
	require.NoError(t, json.Unmarshal([]byte(stored), &parsed))
	require.Len(t, parsed, 1)
	require.NotEmpty(t, parsed[0].ID, "服务端必须自动生成 id")
}

// 部分更新保留：请求省略 footer_links 时既有配置不得被清空。
func TestFooterLinkPartialUpdatePreservesExisting(t *testing.T) {
	existing := `[{"id":"a1","name":"Existing","url":"https://example.com/","sort_order":1}]`
	h, repo := newStepUpSwitchTestHandler(t, map[string]string{
		service.SettingKeyFooterLinks: existing,
	})

	rec := doUpdateSettings(t, h, map[string]any{"registration_enabled": true}, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, existing, repo.values[service.SettingKeyFooterLinks],
		"省略 footer_links 时必须保留既有配置")
}

// 写读回环：写入（无 id）后，更新响应与管理员 GET 读回均含服务端生成 id，且一致。
func TestFooterLinkWriteReadLoop(t *testing.T) {
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{})

	writeRec := doUpdateSettings(t, h, map[string]any{
		"footer_links": []map[string]any{{"name": "评测站", "url": "https://example.com/", "sort_order": 1}},
	}, nil)
	require.Equal(t, http.StatusOK, writeRec.Code)

	var writeBody dto.SystemSettings
	require.NoError(t, json.Unmarshal(writeRec.Body.Bytes(), &envelope{Data: &writeBody}))
	require.Len(t, writeBody.FooterLinks, 1)
	require.NotEmpty(t, writeBody.FooterLinks[0].ID)
	writtenID := writeBody.FooterLinks[0].ID

	getRec := doGetSettings(t, h)
	require.Equal(t, http.StatusOK, getRec.Code)
	var getBody dto.SystemSettings
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &envelope{Data: &getBody}))
	require.Len(t, getBody.FooterLinks, 1)
	require.Equal(t, writtenID, getBody.FooterLinks[0].ID, "GET 读回应与写入响应携带同一服务端生成 id")
}

// 公开注入：公开响应与注入 payload 均含 footer_links。
func TestFooterLinkPublicInjection(t *testing.T) {
	stored := `[{"id":"a1","name":"公开友链","url":"https://example.com/","sort_order":1}]`
	h, _ := newStepUpSwitchTestHandler(t, map[string]string{
		service.SettingKeyFooterLinks: stored,
	})

	ctx := context.Background()

	pub, err := h.settingService.GetPublicSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, stored, pub.FooterLinks, "公开设置字符串字段应原样透传 footer_links")

	injection, err := h.settingService.GetPublicSettingsForInjection(ctx)
	require.NoError(t, err)
	payload, ok := injection.(*service.PublicSettingsInjectionPayload)
	require.True(t, ok)
	require.NotEmpty(t, payload.FooterLinks, "注入 payload 必须含 footer_links")
	require.JSONEq(t, stored, string(payload.FooterLinks))
}
