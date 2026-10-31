//go:build unit

package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// sensenovaInvalidResetCaptureCount 在加锁下清零落盘计数器，让每个用例从干净状态开始。
func sensenovaInvalidResetCaptureCount(t *testing.T) {
	t.Helper()
	sensenovaInvalidCaptureMu.Lock()
	sensenovaInvalidCaptureCount = 0
	sensenovaInvalidCaptureMu.Unlock()
}

// sensenovaInvalidCaptureFiles 返回当前数据目录下的落盘文件条目（目录不存在视为 0 份）。
func sensenovaInvalidCaptureFiles(t *testing.T) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(sensenovaInvalidCaptureBaseDir(), sensenovaInvalidCaptureDir))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	return entries
}

// sensenovaInvalidCaptureRecord 是落盘记录的结构，用于读回校验。
type sensenovaInvalidCaptureRecord struct {
	CapturedAt   string          `json:"captured_at"`
	AccountID    int64           `json:"account_id"`
	Path         string          `json:"path"`
	RequestBody  json.RawMessage `json:"request_body"`
	ResponseBody json.RawMessage `json:"response_body"`
}

func TestObserveSensenovaInvalidRequestCapture(t *testing.T) {
	invalidResponse := []byte(`{"error":{"message":"inference request is invalid","code":400}}`)
	otherResponse := []byte(`{"error":{"message":"field MaxTokens invalid, should be in [1, 65536]"}}`)
	requestBody := []byte(`{"model":"SenseNova-V6","messages":[{"role":"user","content":"hello"}]}`)

	tests := []struct {
		name       string
		account    *Account
		statusCode int
		response   []byte
		wantFiles  int
	}{
		{
			name:       "sensenova 400 with marker is captured",
			account:    sensenovaTestAccount(PlatformDeepseek, 501),
			statusCode: 400,
			response:   invalidResponse,
			wantFiles:  1,
		},
		{
			name:       "non sensenova account is ignored",
			account:    sensenovaInvalidNonSensenovaAccount(502),
			statusCode: 400,
			response:   invalidResponse,
			wantFiles:  0,
		},
		{
			name:       "sensenova 500 is ignored",
			account:    sensenovaTestAccount(PlatformDeepseek, 503),
			statusCode: 500,
			response:   invalidResponse,
			wantFiles:  0,
		},
		{
			name:       "sensenova 400 without marker is ignored",
			account:    sensenovaTestAccount(PlatformKimi, 504),
			statusCode: 400,
			response:   otherResponse,
			wantFiles:  0,
		},
		{
			name:       "nil account is ignored",
			account:    nil,
			statusCode: 400,
			response:   invalidResponse,
			wantFiles:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATA_DIR", t.TempDir())
			sensenovaInvalidResetCaptureCount(t)

			observeSensenovaInvalidRequest(tt.account, "/v1/chat/completions", tt.statusCode, requestBody, tt.response)

			require.Len(t, sensenovaInvalidCaptureFiles(t), tt.wantFiles)
		})
	}
}

// sensenovaInvalidNonSensenovaAccount 把 sensenova 测试账号的上游换成非 sensenova 域名。
func sensenovaInvalidNonSensenovaAccount(id int64) *Account {
	account := sensenovaTestAccount(PlatformDeepseek, id)
	account.Credentials["base_url"] = "https://api.example.com/v1"
	return account
}

func TestObserveSensenovaInvalidRequestCaptureLimitAndPermissions(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	sensenovaInvalidResetCaptureCount(t)

	requestBody := []byte(`{"model":"SenseNova-V6","messages":[{"role":"user","content":"hello"}]}`)
	responseBody := []byte(`{"error":{"message":"inference request is invalid"}}`)
	const path = "/v1/messages"

	for i := 0; i < 8; i++ {
		observeSensenovaInvalidRequest(sensenovaTestAccount(PlatformDeepseek, 601), path, 400, requestBody, responseBody)
	}

	dir := filepath.Join(sensenovaInvalidCaptureBaseDir(), sensenovaInvalidCaptureDir)
	entries := sensenovaInvalidCaptureFiles(t)
	require.Len(t, entries, sensenovaInvalidCaptureMax)

	dirInfo, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())

	for _, entry := range entries {
		info, err := os.Stat(filepath.Join(dir, entry.Name()))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	raw, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	require.NoError(t, err)
	var record sensenovaInvalidCaptureRecord
	require.NoError(t, json.Unmarshal(raw, &record))
	require.NotEmpty(t, record.CapturedAt)
	require.Equal(t, int64(601), record.AccountID)
	require.Equal(t, path, record.Path)
	require.JSONEq(t, string(requestBody), string(record.RequestBody))
	require.JSONEq(t, string(responseBody), string(record.ResponseBody))
}

func TestObserveSensenovaInvalidRequestCaptureLimitSurvivesRestart(t *testing.T) {
	t.Setenv("DATA_DIR", t.TempDir())
	sensenovaInvalidResetCaptureCount(t)

	requestBody := []byte(`{"model":"SenseNova-V6","messages":[{"role":"user","content":"hello"}]}`)
	responseBody := []byte(`{"error":{"message":"inference request is invalid"}}`)
	account := sensenovaTestAccount(PlatformDeepseek, 701)

	for i := 0; i < sensenovaInvalidCaptureMax; i++ {
		observeSensenovaInvalidRequest(account, "/v1/responses", 400, requestBody, responseBody)
	}
	require.Len(t, sensenovaInvalidCaptureFiles(t), sensenovaInvalidCaptureMax)

	// 模拟进程重启：进程内计数器归零，但目录内已有条目数仍应拦住新的落盘。
	sensenovaInvalidResetCaptureCount(t)
	observeSensenovaInvalidRequest(account, "/v1/responses", 400, requestBody, responseBody)

	require.Len(t, sensenovaInvalidCaptureFiles(t), sensenovaInvalidCaptureMax)
}

func TestSensenovaRequestShape(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"model": "SenseNova-V6",
		"stream": false,
		"max_tokens": 4096,
		"messages": [
			{"role": "system", "content": "you are a helpful assistant"},
			{"role": "user", "content": [
				{"type": "text", "text": "look at this picture"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAAA"}}
			]},
			{"role": "assistant", "content": null, "reasoning_content": "let me inspect the file", "tool_calls": [
				{"id": "c1", "type": "function", "function": {"name": "Bash", "arguments": "{\"command\":\"ls\"}"}},
				{"id": "c2", "type": "function", "function": {"name": "Read", "arguments": "{\"file_path\":\"/tmp/a\"}"}}
			]},
			{"role": "tool", "tool_call_id": "c1", "content": "listed 3 files"},
			{"role": "tool", "tool_call_id": "c9", "content": ""}
		],
		"tools": [
			{"type": "function", "function": {"name": "Bash", "parameters": {"type": "object", "properties": {"command": {"type": "string"}}}}}
		]
	}`)

	shape := sensenovaRequestShape(body)

	require.Equal(t, true, shape["valid_json"])
	require.Equal(t, []string{"model", "stream", "max_tokens", "messages", "tools"}, shape["top_keys"])
	require.Equal(t, []string{
		"system:text",
		"user[text image_url]",
		"assistant:null+tool_calls(2)+reasoning",
		"tool:text",
		"tool:text",
	}, shape["message_sequence"])
	require.Equal(t, 5, shape["message_count"])
	require.Equal(t, 1, shape["empty_messages"])
	require.Equal(t, 1, shape["images"])
	require.Equal(t, 1, shape["unmatched_tool_calls"])
	require.Equal(t, 1, shape["unmatched_tool_results"])
	require.Equal(t, 1, shape["tool_count"])
	require.Equal(t, []string{"Bash"}, shape["tool_names"])

	params, ok := shape["params"].(map[string]any)
	require.True(t, ok)
	require.EqualValues(t, 4096, params["max_tokens"])
	require.EqualValues(t, false, params["stream"])
	require.EqualValues(t, "SenseNova-V6", params["model"])
	require.NotContains(t, params, "temperature")

	// 指纹中不得出现任何消息正文/工具参数文本。
	encoded, err := json.Marshal(shape)
	require.NoError(t, err)
	for _, leaked := range []string{
		"you are a helpful assistant",
		"look at this picture",
		"let me inspect the file",
		"listed 3 files",
		"data:image/png;base64,AAAA",
		"/tmp/a",
	} {
		require.NotContains(t, string(encoded), leaked)
	}
}

func TestSensenovaRequestShapeInvalidJSON(t *testing.T) {
	t.Parallel()

	shape := sensenovaRequestShape([]byte("not json"))

	require.Len(t, shape, 2)
	require.Equal(t, false, shape["valid_json"])
	require.Equal(t, len("not json"), shape["body_bytes"])
}
