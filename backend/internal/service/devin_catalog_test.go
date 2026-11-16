//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// testModel 是合成模型配置的测试载体。
type testModel struct {
	label           string
	selector        string
	alias           string
	provider        uint32
	contextWindow   uint32
	maxOutputTokens uint32
	disabled        bool
	supportsImages  bool
}

// encodeTestModelFields 用生产编码器把 testModel 写成 protobuf 字段。
func encodeTestModelFields(e *ProtoEncoder, m testModel) {
	e.String(1, m.label) // label
	if m.disabled {
		e.Uint32(4, 1) // disabled
	}
	e.Bool(5, m.supportsImages) // supportsImages
	if m.provider != 0 {
		e.Uint32(10, m.provider) // provider enum
	}
	if m.contextWindow != 0 {
		e.Uint32(18, m.contextWindow) // contextWindow
	}
	e.String(22, m.selector) // selector
	if m.alias != "" || m.maxOutputTokens != 0 {
		e.Message(23, func(sub *ProtoEncoder) {
			if m.maxOutputTokens != 0 {
				sub.Uint32(13, m.maxOutputTokens) // maxOutputTokens
			}
			sub.String(23, m.alias) // alias
		})
	}
}

// encodeTestModel 返回单个模型消息的原始字节（不含外层 field 1）。
func encodeTestModel(m testModel) []byte {
	e := &ProtoEncoder{}
	encodeTestModelFields(e, m)
	return e.Bytes()
}

// encodeTestCatalog 合成 GetCliModelConfigs 响应：重复 field 1（message）。
func encodeTestCatalog(models ...testModel) []byte {
	e := &ProtoEncoder{}
	for _, m := range models {
		mm := m
		e.Message(1, func(sub *ProtoEncoder) { encodeTestModelFields(sub, mm) })
	}
	return e.Bytes()
}

// TestDevinCatalogDecodeFields 验证单个模型全字段解码。
func TestDevinCatalogDecodeFields(t *testing.T) {
	msg := encodeTestModel(testModel{
		label:           "Claude Sonnet",
		selector:        "claude-sonnet-4.5",
		provider:        3,
		contextWindow:   200000,
		supportsImages:  true,
		alias:           "claude-sonnet",
		maxOutputTokens: 8192,
	})
	cfg, disabled, err := decodeDevinModelConfig(msg)
	require.NoError(t, err)
	require.False(t, disabled)
	require.Equal(t, DevinModelConfig{
		Selector:        "claude-sonnet-4.5",
		Label:           "Claude Sonnet",
		Provider:        "anthropic",
		Alias:           "claude-sonnet",
		SupportsImages:  true,
		ContextWindow:   200000,
		MaxOutputTokens: 8192,
	}, cfg)
}

// TestDevinCatalogProviderMapping 覆盖 provider 枚举全部已知值与未知值。
func TestDevinCatalogProviderMapping(t *testing.T) {
	cases := map[uint32]string{
		1:  "cognition",
		2:  "openai",
		3:  "anthropic",
		4:  "google",
		7:  "moonshot",
		9:  "zhipu",
		0:  "",
		5:  "",
		99: "",
	}
	for v, want := range cases {
		require.Equal(t, want, devinProviderName(uint64(v)))
	}
}

// TestDevinCatalogSkipsDisabledAndEmptySelector 验证 disabled==1 与 selector 空的模型被跳过。
func TestDevinCatalogSkipsDisabledAndEmptySelector(t *testing.T) {
	catalog := encodeTestCatalog(
		testModel{label: "Keep", selector: "keep-me", provider: 2},
		testModel{label: "Disabled", selector: "disabled-me", provider: 2, disabled: true},
		testModel{label: "NoSelector", provider: 2},
	)
	got, err := decodeGetCliModelConfigsResponse(catalog)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "keep-me", got[0].Selector)
	require.Equal(t, "Keep", got[0].Label)
}

// TestDevinCatalogDecodeUnknownFields 验证未知字段不中断解码。
func TestDevinCatalogDecodeUnknownFields(t *testing.T) {
	e := &ProtoEncoder{}
	e.String(1, "Label")
	e.Uint64(99, 1234)     // 未知 varint
	e.String(100, "noise") // 未知 length-delimited
	e.String(22, "sel")
	cfg, disabled, err := decodeDevinModelConfig(e.Bytes())
	require.NoError(t, err)
	require.False(t, disabled)
	require.Equal(t, "Label", cfg.Label)
	require.Equal(t, "sel", cfg.Selector)
}

// TestDevinCatalogEndpoint 验证 baseURL 空回落与去尾斜杠。
func TestDevinCatalogEndpoint(t *testing.T) {
	require.Equal(t, DevinAPIURL+DevinModelCatalogPath, devinCatalogEndpoint(""))
	require.Equal(t, "https://example.com"+DevinModelCatalogPath, devinCatalogEndpoint("https://example.com"))
	require.Equal(t, "https://example.com"+DevinModelCatalogPath, devinCatalogEndpoint("https://example.com/"))
}

// TestDevinCatalogFetchRequest 验证请求方法、路径、头（无 Authorization）与请求体字节一致。
func TestDevinCatalogFetchRequest(t *testing.T) {
	var method, path, contentType, protocol, accept, auth string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		contentType = r.Header.Get("Content-Type")
		protocol = r.Header.Get("Connect-Protocol-Version")
		accept = r.Header.Get("Accept")
		auth = r.Header.Get("Authorization")
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encodeTestCatalog())
	}))
	defer srv.Close()

	_, err := FetchDevinModelCatalog(context.Background(), "raw-token", srv.URL)
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, method)
	require.Equal(t, DevinModelCatalogPath, path)
	require.Equal(t, "application/proto", contentType)
	require.Equal(t, "1", protocol)
	require.Equal(t, "*/*", accept)
	require.Empty(t, auth, "不得携带 Authorization 头")

	want := encodeGetUserJwtRequest(&Metadata{
		IDEName:          DevinClientName,
		IDEVersion:       DevinIDEVersion,
		ExtensionName:    DevinClientName,
		ExtensionVersion: DevinExtensionVersion,
		APIKey:           NormalizeDevinToken("raw-token"),
		Locale:           "en",
	})
	require.Equal(t, want, body, "请求体应与 GetUserJwt 编码器字节一致且 token 已规范化")
}

// TestDevinCatalogFetchNon200 验证非 200 错误含状态码且 body 截断到前 200 字节。
func TestDevinCatalogFetchNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(strings.Repeat("x", 250)))
	}))
	defer srv.Close()

	_, err := FetchDevinModelCatalog(context.Background(), "tok", srv.URL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "503")
	require.Contains(t, err.Error(), strings.Repeat("x", 200))
	require.NotContains(t, err.Error(), strings.Repeat("x", 201), "错误 body 不得超过前 200 字节")
}

// TestDevinCatalogFetchDecodeResponse 验证 200 响应体被正确解码为模型列表。
func TestDevinCatalogFetchDecodeResponse(t *testing.T) {
	catalog := encodeTestCatalog(
		testModel{
			label: "GPT-5", selector: "gpt-5", provider: 2,
			contextWindow: 400000, supportsImages: true, alias: "g5", maxOutputTokens: 128000,
		},
		testModel{label: "Disabled", selector: "disabled-1", provider: 2, disabled: true},
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(catalog)
	}))
	defer srv.Close()

	got, err := FetchDevinModelCatalog(context.Background(), "tok", srv.URL)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, DevinModelConfig{
		Selector:        "gpt-5",
		Label:           "GPT-5",
		Provider:        "openai",
		Alias:           "g5",
		SupportsImages:  true,
		ContextWindow:   400000,
		MaxOutputTokens: 128000,
	}, got[0])
}
