package middleware

// 请求形状入口中间件的定向测试（2026-11-04）。
//
// 上线后第一条真实错误的证据就是：形状没带上——因为首版只打在 4 个转发函数上，
// 而实际有"消息转对话兜底""codebuddy 专线"等多条路径。这些用例钉死两件事：
//   1) 只要经过入口，形状一定在（不依赖具体走哪条转发分支）；
//   2) 中间件读完之后，处理器仍能读到完整原始请求体（不能把请求吃掉）。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const shapeMiddlewareProbe = "SHAPE-PROBE-DO-NOT-LEAK"

func TestRequestShapeCaptureKeepsBodyReadable(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := `{"model":"deepseek-v4.1-flash","max_tokens":999999,"messages":[{"role":"user","content":"` + shapeMiddlewareProbe + `"}]}`

	var (
		handlerSawBody   string
		handlerShapeRaw  []byte
		handlerShapeKeys []string
	)
	r := gin.New()
	r.Use(RequestShapeCapture())
	r.POST("/v1/chat/completions", func(c *gin.Context) {
		raw, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		handlerSawBody = string(raw)

		shape := service.OpsRequestShapeFrom(c)
		require.NotNil(t, shape, "入口必须留下请求形状，处理器才可能被诊断")
		encoded, err := json.Marshal(shape)
		require.NoError(t, err)
		handlerShapeRaw = encoded
		for key := range shape {
			handlerShapeKeys = append(handlerShapeKeys, key)
		}
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, body, handlerSawBody, "中间件读完之后必须原样回填请求体")

	// 形状里必须有诊断要素，且绝不能有正文。
	shapeText := string(handlerShapeRaw)
	require.Contains(t, shapeText, `"items":1`)
	require.Contains(t, shapeText, `"max_tokens":999999`)
	require.NotContains(t, shapeText, shapeMiddlewareProbe, "形状里不得出现正文")
	require.Contains(t, handlerShapeKeys, "protocol")
}

func TestRequestShapeCaptureSkipsNonJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	r.Use(RequestShapeCapture())
	r.POST("/v1/audio/transcriptions", func(c *gin.Context) {
		require.Nil(t, service.OpsRequestShapeFrom(c), "二进制上传不采形状，避免整包进内存")
		raw, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		require.Equal(t, "binary-payload", string(raw))
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader("binary-payload"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
}

func TestRequestShapeCaptureDetectsProtocol(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		path string
		body string
		want string
	}{
		{
			path: "/v1/messages",
			body: `{"model":"claude-x","messages":[{"role":"user","content":"hi"}]}`,
			want: "chat",
		},
		{
			path: "/v1/responses",
			body: `{"model":"gpt-x","input":[{"type":"message","role":"user","content":"hi"}]}`,
			want: "responses",
		},
		{
			path: "/v1beta/models/gemini-x:generateContent",
			body: `{"contents":[{"parts":[{"text":"hi"}]}]}`,
			want: "gemini",
		},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			var got string
			r := gin.New()
			r.Use(RequestShapeCapture())
			r.POST(tc.path, func(c *gin.Context) {
				shape := service.OpsRequestShapeFrom(c)
				require.NotNil(t, shape)
				got, _ = shape["protocol"].(string)
				c.Status(http.StatusOK)
			})
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(httptest.NewRecorder(), req)
			require.Equal(t, tc.want, got)
		})
	}
}
