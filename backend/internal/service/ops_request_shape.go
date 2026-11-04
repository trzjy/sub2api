package service

import (
	"strings"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"

	"github.com/gin-gonic/gin"
)

// 上游泛化错误的「请求结构快照」（2026-11-04 用户裁定：加日志，后续再出现才有判断依据）。
//
// 背景：上游（多为第三方中转）会回一句没有任何字段信息的 400，生产 7 天 525 次 /
// 26 用户，全部是 `inference request is invalid`：
//
//	{"error":{"message":"inference request is invalid","type":"invalid_request_error"}}
//
// 这句文案无法回答"是我方拼错了，还是上游不支持这种形状"。要定位只能看**请求长什么样**，
// 但请求正文属于用户数据，绝不能落库。
//
// 折中：只记录**形状**——条数、开关、有没图片、max_tokens 多大、字节数——不含任何正文、
// 不含文件名、不含工具名。这些计数足以区分几类常见成因：
//   - tools 为 0 而客户端期待工具调用 → 工具声明在上游侧被拒；
//   - images > 0 → 多模态不被该中转支持；
//   - max_tokens 贴顶 → 参数上限问题；
//   - body_bytes 特别大 → 上行体积被截断。
const OpsRequestShapeKey = "ops_request_shape"

// DescribeRequestShape 从请求体提取不含内容的形状摘要。非法 JSON 时只返回字节数。
func DescribeRequestShape(body []byte, protocol string) map[string]any {
	shape := map[string]any{
		"protocol":   protocol,
		"body_bytes": len(body),
	}
	if len(body) == 0 || !gjson.ValidBytes(body) {
		shape["json_valid"] = false
		return shape
	}
	shape["json_valid"] = true

	// 消息/输入条数：三种协议的位置不同。
	switch protocol {
	case "responses":
		shape["items"] = len(gjson.GetBytes(body, "input").Array())
	case "gemini":
		shape["items"] = len(gjson.GetBytes(body, "contents").Array())
	default:
		shape["items"] = len(gjson.GetBytes(body, "messages").Array())
	}
	if system := gjson.GetBytes(body, "system"); system.Exists() {
		shape["system_bytes"] = len(system.Raw)
	}
	if instructions := gjson.GetBytes(body, "instructions"); instructions.Exists() && instructions.String() != "" {
		shape["instructions_bytes"] = len(instructions.String())
	}

	// 工具声明：条数与类型分布（只数个数，不记名字）。
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		shape["tools"] = len(tools.Array())
		typed := map[string]int{}
		for _, tool := range tools.Array() {
			kind := tool.Get("type").String()
			if kind == "" {
				kind = "function"
			}
			if fn := tool.Get("function.type").String(); fn != "" {
				kind = fn
			}
			typed[kind]++
		}
		if len(typed) > 0 {
			shape["tool_types"] = typed
		}
	}
	if choice := gjson.GetBytes(body, "tool_choice"); choice.Exists() {
		shape["tool_choice"] = firstNonEmpty(choice.String(), choice.Get("type").String())
	}

	// 多模态：只数图片块，不看内容。messages 用 content[].type，responses 用 input[].content[]。
	images := 0
	gjson.GetBytes(body, `messages.#.content`).ForEach(func(_, content gjson.Result) bool {
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(_, block gjson.Result) bool {
			switch block.Get("type").String() {
			case "image", "image_url", "input_image":
				images++
			}
			return true
		})
		return true
	})
	if images > 0 {
		shape["images"] = images
	}

	// 推理/思考开关与预算。
	if thinking := gjson.GetBytes(body, "thinking"); thinking.Exists() {
		shape["thinking_type"] = thinking.Get("type").String()
		if budget := thinking.Get("budget_tokens"); budget.Exists() {
			shape["thinking_budget"] = budget.Int()
		}
	}
	if reasoning := gjson.GetBytes(body, "reasoning"); reasoning.Exists() {
		shape["reasoning_effort"] = reasoning.Get("effort").String()
	}
	if effort := gjson.GetBytes(body, "output_config.effort"); effort.Exists() {
		shape["output_effort"] = effort.String()
	}

	// 输出上限：三种字段名都要看（不同协议/不同中转用不同键）。
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if value := gjson.GetBytes(body, key); value.Exists() {
			shape[key] = value.Int()
		}
	}

	// 上一轮的会话接力字段：有些上游不认，需要单独区分。
	if id := gjson.GetBytes(body, "previous_response_id").String(); id != "" {
		shape["has_previous_response_id"] = true
	}
	if stream := gjson.GetBytes(body, "stream"); stream.Exists() {
		shape["stream"] = stream.Bool()
	}
	return shape
}

// SetOpsRequestShape 把请求形状挂到本次请求的 gin context，供上游错误事件与日志取用。
func SetOpsRequestShape(c *gin.Context, shape map[string]any) {
	if c == nil || len(shape) == 0 {
		return
	}
	c.Set(OpsRequestShapeKey, shape)
}

// OpsRequestShapeFrom 取回本次请求的形状摘要（无则返回 nil）。
func OpsRequestShapeFrom(c *gin.Context) map[string]any {
	if c == nil {
		return nil
	}
	if value, ok := c.Get(OpsRequestShapeKey); ok {
		if shape, ok := value.(map[string]any); ok {
			return shape
		}
	}
	return nil
}

// genericUpstreamRejectionMarkers 是上游「只说不行、不说哪不行」的固定话术。
// 这类错误无法从响应体反推成因，只能靠请求形状判断，出现时必须留一条带形状的日志。
var genericUpstreamRejectionMarkers = []string{
	"inference request is invalid",
	"the request could not be processed",
	"invalid request body",
	"无效的请求",
}

// IsGenericUpstreamRejectionMessage 判定上游错误消息是否属于"无字段信息的泛化拒绝"。
func IsGenericUpstreamRejectionMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if lower == "" {
		return false
	}
	for _, marker := range genericUpstreamRejectionMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// logGenericUpstreamRejection 为泛化拒绝落一条结构化日志（形状 + 归属，不含正文）。
//
// 落库口径在 ops_error_logs.upstream_errors[].request_shape；这条日志是给人实时看的一份，
// 两者同时存在：前者抗容器重建，后者方便当场 grep。
func logGenericUpstreamRejection(ev OpsUpstreamErrorEvent) {
	if !IsGenericUpstreamRejectionMessage(ev.Message) {
		return
	}
	logger.L().Warn("upstream_generic_rejection",
		zap.String("message", ev.Message),
		zap.String("platform", ev.Platform),
		zap.Int64("account_id", ev.AccountID),
		zap.String("account_name", ev.AccountName),
		zap.String("upstream_url", ev.UpstreamURL),
		zap.Int("upstream_status", ev.UpstreamStatusCode),
		zap.Any("request_shape", ev.RequestShape))
}
