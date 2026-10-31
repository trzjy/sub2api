package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// 本文件是 sensenova（token.sensenova.cn）上游 400 "inference request is invalid" 的
// 临时取证钩子：上游不指明具体字段、网关也不留请求体，根因无法定位，因此需要把
// 命中请求的完整请求体原样落盘供人工分析。
//
// 纪律：
//   - 只观察，不改行为：不修改请求/响应，不影响任何出站路径的返回值。
//   - 落盘原文包含用户内容（messages、工具参数、工具结果），属于敏感数据，
//     只允许落在受控数据目录（0700 目录 / 0600 文件），且总量上限 sensenovaInvalidCaptureMax 份。
//   - 取证结束后本文件整体删除（含常量、计数器、落盘目录清理），不留长期代码。
const (
	// sensenovaInvalidMarker 是上游返回体中携带的错误标记。
	sensenovaInvalidMarker = "inference request is invalid"
	// sensenovaInvalidCaptureMax 是本进程与落盘目录累计允许的落盘份数上限。
	sensenovaInvalidCaptureMax = 5
	// sensenovaInvalidCaptureDir 是落盘子目录名，挂在数据目录下。
	sensenovaInvalidCaptureDir = "sensenova-invalid-captures"
)

var (
	sensenovaInvalidCaptureMu    sync.Mutex
	sensenovaInvalidCaptureCount int
)

// observeSensenovaInvalidRequest 在出站响应命中 sensenova 的
// "inference request is invalid" 时记录一条告警并落盘完整请求体。
// 只读判定：账号非空、状态码 400、上游 base_url 为 sensenova、响应体含标记，
// 任一不满足即直接返回。落盘失败不影响主流程（只少一条 capture_file 字段）。
func observeSensenovaInvalidRequest(account *Account, path string, statusCode int, requestBody, responseBody []byte) {
	if account == nil || statusCode != 400 {
		return
	}
	if !isSensenovaBaseURL(account.GetOpenAIBaseURL()) {
		return
	}
	if !bytes.Contains(responseBody, []byte(sensenovaInvalidMarker)) {
		return
	}

	fields := []zap.Field{
		zap.Int64("account_id", account.ID),
		zap.String("path", path),
		zap.Any("request_shape", sensenovaRequestShape(requestBody)),
	}
	if captureFile := captureSensenovaInvalidRequest(account.ID, path, requestBody, responseBody); captureFile != "" {
		fields = append(fields, zap.String("capture_file", captureFile))
	}
	logger.L().Warn("sensenova.inference_request_invalid", fields...)
}

// sensenovaRequestShape 计算请求体的结构指纹。
// 硬性约束：结果中不得出现任何消息文本、工具参数文本，只保留形态与计数，
// 因此可安全写入日志。
func sensenovaRequestShape(body []byte) map[string]any {
	shape := map[string]any{"body_bytes": len(body)}
	if !gjson.ValidBytes(body) {
		shape["valid_json"] = false
		return shape
	}
	shape["valid_json"] = true

	topKeys := make([]string, 0, 8)
	gjson.ParseBytes(body).ForEach(func(key, _ gjson.Result) bool {
		topKeys = append(topKeys, key.String())
		return true
	})
	shape["top_keys"] = topKeys

	// 复合值（对象/数组）可能携带 json_schema 定义或用户文本，指纹里只记类型，
	// 原文只允许出现在 0700 目录的落盘文件中。
	params := map[string]any{}
	for _, key := range []string{
		"model", "stream", "max_tokens", "max_completion_tokens", "temperature", "top_p",
		"reasoning_effort", "tool_choice", "parallel_tool_calls", "stream_options",
		"response_format", "thinking", "enable_thinking",
	} {
		if result := gjson.GetBytes(body, key); result.Exists() {
			switch {
			case result.IsObject():
				params[key] = "object"
			case result.IsArray():
				params[key] = "array"
			default:
				params[key] = result.Value()
			}
		}
	}
	shape["params"] = params

	messageSequence := make([]string, 0, 8)
	messageCount := 0
	emptyMessages := 0
	images := 0
	maxContentBytes := 0
	toolCallIDs := map[string]struct{}{}
	toolResultIDs := map[string]struct{}{}
	gjson.GetBytes(body, "messages").ForEach(func(_, message gjson.Result) bool {
		messageCount++
		desc := message.Get("role").String()

		content := message.Get("content")
		switch {
		case !content.Exists() || content.Type == gjson.Null:
			desc += ":null"
		case content.Type == gjson.String:
			desc += ":text"
			if len(content.Str) == 0 {
				emptyMessages++
			}
			if len(content.Str) > maxContentBytes {
				maxContentBytes = len(content.Str)
			}
		case content.IsArray():
			blockTypes := make([]string, 0, 4)
			blockCount := 0
			content.ForEach(func(_, block gjson.Result) bool {
				blockCount++
				blockType := block.Get("type").String()
				blockTypes = append(blockTypes, blockType)
				if blockType == "image_url" || blockType == "image" {
					images++
				}
				if textLen := len(block.Get("text").Str); textLen > maxContentBytes {
					maxContentBytes = textLen
				}
				return true
			})
			desc += fmt.Sprintf("%v", blockTypes)
			if blockCount == 0 {
				emptyMessages++
			}
		default:
			desc += ":" + content.Type.String()
		}

		if toolCalls := message.Get("tool_calls"); toolCalls.IsArray() {
			callCount := 0
			toolCalls.ForEach(func(_, call gjson.Result) bool {
				callCount++
				if id := call.Get("id").String(); id != "" {
					toolCallIDs[id] = struct{}{}
				}
				return true
			})
			if callCount > 0 {
				desc += fmt.Sprintf("+tool_calls(%d)", callCount)
			}
		}
		if message.Get("reasoning_content").Exists() {
			desc += "+reasoning"
		}
		if toolCallID := message.Get("tool_call_id"); toolCallID.Exists() {
			if id := toolCallID.String(); id != "" {
				toolResultIDs[id] = struct{}{}
			}
		}

		messageSequence = append(messageSequence, desc)
		return true
	})
	shape["message_sequence"] = messageSequence
	shape["message_count"] = messageCount
	shape["empty_messages"] = emptyMessages
	shape["images"] = images
	shape["max_content_bytes"] = maxContentBytes

	unmatchedToolCalls := 0
	for id := range toolCallIDs {
		if _, ok := toolResultIDs[id]; !ok {
			unmatchedToolCalls++
		}
	}
	unmatchedToolResults := 0
	for id := range toolResultIDs {
		if _, ok := toolCallIDs[id]; !ok {
			unmatchedToolResults++
		}
	}
	shape["unmatched_tool_calls"] = unmatchedToolCalls
	shape["unmatched_tool_results"] = unmatchedToolResults

	toolNames := make([]string, 0, 4)
	maxToolSchemaBytes := 0
	toolCount := 0
	gjson.GetBytes(body, "tools").ForEach(func(_, tool gjson.Result) bool {
		toolCount++
		toolNames = append(toolNames, tool.Get("function.name").String())
		if schemaBytes := len(tool.Get("function.parameters").Raw); schemaBytes > maxToolSchemaBytes {
			maxToolSchemaBytes = schemaBytes
		}
		return true
	})
	shape["tool_count"] = toolCount
	shape["tool_names"] = toolNames
	shape["max_tool_schema_bytes"] = maxToolSchemaBytes

	return shape
}

// captureSensenovaInvalidRequest 把命中的请求/响应原文落盘，返回写入的文件路径，
// 未写（达上限或任一步出错）返回空串。份数上限同时按进程内计数器与目录内已有
// 条目数判定，后者用于跨进程重启生效。
func captureSensenovaInvalidRequest(accountID int64, path string, requestBody, responseBody []byte) string {
	sensenovaInvalidCaptureMu.Lock()
	defer sensenovaInvalidCaptureMu.Unlock()

	if sensenovaInvalidCaptureCount >= sensenovaInvalidCaptureMax {
		return ""
	}

	dir := filepath.Join(sensenovaInvalidCaptureBaseDir(), sensenovaInvalidCaptureDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logger.L().Warn("sensenova.inference_request_invalid.capture_failed", zap.Error(err))
		return ""
	}
	// MkdirAll 对已存在的目录不会收紧权限，目录若事先以宽权限建成就一直保持，
	// 故显式收紧，保证原文只落在 0700 目录里。
	if err := os.Chmod(dir, 0o700); err != nil {
		logger.L().Warn("sensenova.inference_request_invalid.capture_failed", zap.Error(err))
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.L().Warn("sensenova.inference_request_invalid.capture_failed", zap.Error(err))
		return ""
	}
	if len(entries) >= sensenovaInvalidCaptureMax {
		sensenovaInvalidCaptureCount = sensenovaInvalidCaptureMax
		return ""
	}

	record, err := json.Marshal(map[string]any{
		"captured_at":   time.Now().UTC().Format(time.RFC3339Nano),
		"account_id":    accountID,
		"path":          path,
		"request_body":  sensenovaInvalidRawOrString(requestBody),
		"response_body": sensenovaInvalidRawOrString(responseBody),
	})
	if err != nil {
		logger.L().Warn("sensenova.inference_request_invalid.capture_failed", zap.Error(err))
		return ""
	}

	file := filepath.Join(dir, fmt.Sprintf("%s-acc%d.json", time.Now().UTC().Format("20060102T150405.000000000"), accountID))
	if err := os.WriteFile(file, record, 0o600); err != nil {
		logger.L().Warn("sensenova.inference_request_invalid.capture_failed", zap.Error(err))
		return ""
	}
	sensenovaInvalidCaptureCount++
	return file
}

// sensenovaInvalidRawOrString 让合法 JSON 原文原样嵌入落盘记录，非法 JSON 退化为 JSON 字符串。
func sensenovaInvalidRawOrString(body []byte) any {
	if json.Valid(body) {
		return json.RawMessage(body)
	}
	return string(body)
}

// sensenovaInvalidCaptureBaseDir 复用 internal/setup/setup.go 里 GetDataDir 的取值规则：
// DATA_DIR 环境变量优先，其次容器默认 /app/data，最后当前目录。
// service 包不能 import internal/setup（会导致 import cycle），故在此复制该规则。
func sensenovaInvalidCaptureBaseDir() string {
	if dir := os.Getenv("DATA_DIR"); dir != "" {
		return dir
	}
	if info, err := os.Stat("/app/data"); err == nil && info.IsDir() {
		return "/app/data"
	}
	return "."
}
