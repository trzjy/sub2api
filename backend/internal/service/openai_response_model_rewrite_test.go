package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestMappedResponseModelPreservesOtherData(t *testing.T) {
	svc := &OpenAIGatewayService{}
	body := `{"model":"alias","text":"mapped alias","tool":{"model":"mapped","arguments":"{\"model\":\"alias\"}"}}`
	want := `{"model":"public","text":"mapped alias","tool":{"model":"mapped","arguments":"{\"model\":\"alias\"}"}}`
	// 我方别名守卫：改写目标必须是候选集（发送名 alias + 显式别名）成员，故这里以
	// 上游回显的发送名 alias 作为 from；改写只落顶层 model，嵌套 tool.model 与
	// arguments 内的同名字段一律保留。
	require.Equal(t, want, string(svc.replaceModelInResponseBody([]byte(body), "alias", "public")))
	require.Equal(t, "data: "+want, svc.replaceModelInSSELine("data: "+body, "alias", "public"))
	for _, body := range []string{
		`{"model":"alias",`, `{"model":"alias"} trailing`, `{"model":null}`, `{"model":42}`, `{"model":{}}`, `{"model":[]}`, `{"model":true}`,
		`{"text":"alias","tool":{"model":"alias"}}`,
	} {
		require.Equal(t, body, string(svc.replaceModelInResponseBody([]byte(body), "mapped", "public")))
		require.Equal(t, "data: "+body, svc.replaceModelInSSELine("data: "+body, "mapped", "public"))
	}
	for _, models := range [][2]string{{"same", "same"}, {"", "public"}, {"mapped", ""}} {
		body := `{"model":"alias","response":{"model":"alias"}}`
		require.Equal(t, body, string(svc.replaceModelInResponseBody([]byte(body), models[0], models[1])))
		require.Equal(t, "data: "+body, svc.replaceModelInSSELine("data: "+body, models[0], models[1]))
	}
	require.Equal(t, `data: {"response":{"model":42}}`, svc.replaceModelInSSELine(`data: {"response":{"model":42}}`, "mapped", "public"))
	// 空回显名不在候选集内，同样不改写。
	require.Equal(t, `{"model":""}`, string(svc.replaceModelInResponseBody([]byte(`{"model":""}`), "mapped", "public")))
}

// Exercise both streaming processors so substring fast paths cannot bypass the rewrite.
func TestMappedResponseModelForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, returned := range []string{"zhipu/glm-5.3", "glm-5.3-alias"} {
			// 我方别名守卫：仅当上游回显名落在候选集（发送名 mapped + 显式别名）内才
			// 回写客户端模型名。mapped == returned 命中回写；mapped 为异构拼写
			// （ZHIPU/GLM-5.3）或已等于客户端名（public）时原样透传。
			for _, mapped := range []string{returned, "ZHIPU/GLM-5.3", "public"} {
				for _, kind := range []string{"json", "chat", "responses"} {
					name := kind + "/" + returned + "/" + mapped
					if passthrough {
						name += "/passthrough"
					}
					t.Run(name, func(t *testing.T) {
						model := returned
						if mapped == returned {
							model = "public"
						}
						payload := `{"model":"` + returned + `","choices":[{"delta":{"content":"keep alias","tool_calls":[{"function":{"arguments":"{\"model\":\"alias\"}"}}]}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`
						want := strings.Replace(payload, `"model":"`+returned+`"`, `"model":"`+model+`"`, 1)
						contentType := "application/json"
						body := payload
						if kind == "responses" {
							payload = `{"type":"response.completed","response":{"id":"resp_1","model":"` + returned + `","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`
							want = strings.Replace(payload, `"model":"`+returned+`"`, `"model":"`+model+`"`, 1)
						}
						if kind != "json" {
							contentType = "text/event-stream"
							body = "data: " + payload + "\n\ndata: [DONE]\n\n"
						}
						rec := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(rec)
						c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
						resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
						svc := &OpenAIGatewayService{}
						account := &Account{ID: 1}
						var err error
						if kind == "json" {
							if passthrough {
								_, err = svc.handleNonStreamingResponsePassthrough(context.Background(), resp, c, account, "public", mapped)
							} else {
								_, err = svc.handleNonStreamingResponse(context.Background(), resp, c, account, "public", mapped)
							}
						} else {
							if passthrough {
								_, err = svc.handleStreamingResponsePassthrough(context.Background(), resp, c, account, time.Now(), "public", mapped)
							} else {
								_, err = svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "public", mapped)
							}
						}
						require.NoError(t, err)
						require.Contains(t, rec.Body.String(), want)
						require.Equal(t, returned, observedUpstreamResponseModel(c))
					})
				}
			}
		}
	}
}
