package service

import (
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 请求级"含图"判定 hint。与 openAIImageIntentHintContextKey 同构，供后续
// 调度按 RequireVision 过滤（派发单 C）读取。缺失表示 unknown。
const openAIHasImageInputContextKey = "openai_has_image_input"

// HasOpenAIInputImage 检测请求 body 是否含图片输入。
// 兼容三种协议形态：
//   - chat_completions: messages[].content[].type == "image_url"（含嵌套 content
//     数组、tool result 内图片）
//   - responses: input[].content[].type == "input_image"（含 function_call_output
//     output 内图片）
//   - anthropic: content[].type == "image"
func HasOpenAIInputImage(body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	var found bool
	gjson.ParseBytes(body).ForEach(func(_, value gjson.Result) bool {
		if openAIJSONValueHasImageInput(value) {
			found = true
			return false
		}
		return true
	})
	return found
}

// openAIJSONValueHasImageInput 递归遍历 gjson 值，命中任意含图 block type 即返回 true。
func openAIJSONValueHasImageInput(value gjson.Result) bool {
	if value.Type == gjson.String || value.Type == gjson.Number ||
		value.Type == gjson.True || value.Type == gjson.False || value.Type == gjson.Null {
		return false
	}
	if value.IsObject() {
		if typ := value.Get("type"); typ.Exists() && typ.Type == gjson.String {
			switch typ.String() {
			case "image_url", "input_image", "image":
				return true
			}
		}
		found := false
		value.ForEach(func(_, v gjson.Result) bool {
			if openAIJSONValueHasImageInput(v) {
				found = true
				return false
			}
			return true
		})
		return found
	}
	if value.IsArray() {
		found := false
		value.ForEach(func(_, v gjson.Result) bool {
			if openAIJSONValueHasImageInput(v) {
				found = true
				return false
			}
			return true
		})
		return found
	}
	return false
}

// SetOpenAIHasImageInputHint 将请求级含图判定写入 request context，
// 供后续调度按 RequiredImage 过滤读取（派发单 C）。
func SetOpenAIHasImageInputHint(c *gin.Context, hasImageInput bool) {
	if c == nil {
		return
	}
	c.Set(openAIHasImageInputContextKey, hasImageInput)
}

// GetOpenAIHasImageInputHint 读取请求级含图判定。known=false 表示尚未判定
// （默认放行语义，与 imageIntent 的 unknown 行为一致）。
func GetOpenAIHasImageInputHint(c *gin.Context) (hasImageInput bool, known bool) {
	if c == nil {
		return false, false
	}
	value, ok := c.Get(openAIHasImageInputContextKey)
	if !ok {
		return false, false
	}
	hasImageInput, ok = value.(bool)
	return hasImageInput, ok
}