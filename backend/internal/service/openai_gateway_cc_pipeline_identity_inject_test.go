package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// 这些测试仅覆盖 QK-S3 账号级内容身份注入的单一落点
// injectAccountIdentitySystemMessage，验证闸门（账号 extra 单键
// identity_prompt）与注入语义（首位插入、不覆盖、不注入异常 body）。

func newIdentityInjectService() *OpenAIGatewayService {
	return &OpenAIGatewayService{}
}

// decodeMessagesRoles 解析 body 的 messages 数组，返回各元素的 role 序列。
func decodeMessagesRoles(t *testing.T, body []byte) []string {
	t.Helper()
	var raw struct {
		Messages []struct {
			Role string `json:"role"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(body, &raw), "body must be valid JSON with messages")
	roles := make([]string, 0, len(raw.Messages))
	for _, m := range raw.Messages {
		roles = append(roles, m.Role)
	}
	return roles
}

// TestIdentityInject_NonEmptyPrompt 验证非空 prompt 在 messages 首位注入 system，
// 且客户端原有 messages 顺序严格保持。
func TestIdentityInject_NonEmptyPrompt(t *testing.T) {
	s := newIdentityInjectService()

	account := &Account{Extra: map[string]any{"identity_prompt": "You are Kimi, a helpful assistant."}}
	body := []byte(`{"model":"kimi","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]}`)

	out := s.injectAccountIdentitySystemMessage(account, body)

	roles := decodeMessagesRoles(t, out)
	require.Equal(t, []string{"system", "user", "assistant"}, roles, "injected system must be first, original order preserved")

	// 注入的 system 内容须精确等于 prompt。
	var parsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Equal(t, "You are Kimi, a helpful assistant.", parsed.Messages[0].Content)

	// model 等其余字段须随原始字节保留。
	var rest map[string]any
	require.NoError(t, json.Unmarshal(out, &rest))
	require.Equal(t, "kimi", rest["model"])
}

// TestIdentityInject_EmptyPromptUnchanged 验证 prompt 为空字符串时 body 逐字节不变。
func TestIdentityInject_EmptyPromptUnchanged(t *testing.T) {
	s := newIdentityInjectService()

	account := &Account{Extra: map[string]any{"identity_prompt": ""}}
	body := []byte(`{"model":"kimi","messages":[{"role":"user","content":"hi"}]}`)

	out := s.injectAccountIdentitySystemMessage(account, body)
	require.Equal(t, body, out, "empty prompt must leave body byte-for-byte unchanged")
}

// TestIdentityInject_UnconfiguredUnchanged 验证未配置 identity_prompt 键时 body 逐字节不变。
func TestIdentityInject_UnconfiguredUnchanged(t *testing.T) {
	s := newIdentityInjectService()

	// 完全无 identity_prompt 键（ Even with other extra keys present）。
	account := &Account{Extra: map[string]any{"other_key": "value"}}
	body := []byte(`{"model":"kimi","messages":[{"role":"user","content":"hi"}]}`)

	out := s.injectAccountIdentitySystemMessage(account, body)
	require.Equal(t, body, out, "unconfigured identity_prompt must leave body byte-for-byte unchanged")

	// Extra 为 nil 也应原样返回。
	nilExtra := &Account{}
	out = s.injectAccountIdentitySystemMessage(nilExtra, body)
	require.Equal(t, body, out, "nil Extra must leave body byte-for-byte unchanged")

	// account 为 nil 也应原样返回。
	out = s.injectAccountIdentitySystemMessage(nil, body)
	require.Equal(t, body, out, "nil account must leave body byte-for-byte unchanged")

	// prompt 为非 string 类型（写入方异常）不应注入。
	badType := &Account{Extra: map[string]any{"identity_prompt": 123}}
	out = s.injectAccountIdentitySystemMessage(badType, body)
	require.Equal(t, body, out, "non-string identity_prompt must leave body byte-for-byte unchanged")
}

// TestIdentityInject_ExistingSystemNotOverwritten 验证客户端已有 system 时追加而非覆盖：
// 注入的 system 位于首位，原 system 顺延至其后，内容不被改写。
func TestIdentityInject_ExistingSystemNotOverwritten(t *testing.T) {
	s := newIdentityInjectService()

	account := &Account{Extra: map[string]any{"identity_prompt": "injected system"}}
	body := []byte(`{"messages":[{"role":"system","content":"client system"},{"role":"user","content":"hi"}]}`)

	out := s.injectAccountIdentitySystemMessage(account, body)

	roles := decodeMessagesRoles(t, out)
	require.Equal(t, []string{"system", "system", "user"}, roles, "client system must not be overwritten, only prepend")

	var parsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))
	require.Equal(t, "injected system", parsed.Messages[0].Content)
	require.Equal(t, "client system", parsed.Messages[1].Content)
}

// TestIdentityInject_MessagesMissingOrNonArray 验证 messages 缺失或非数组时不注入。
func TestIdentityInject_MessagesMissingOrNonArray(t *testing.T) {
	s := newIdentityInjectService()
	account := &Account{Extra: map[string]any{"identity_prompt": "should not inject"}}

	// messages 字段缺失。
	missing := []byte(`{"model":"kimi"}`)
	out := s.injectAccountIdentitySystemMessage(account, missing)
	require.Equal(t, missing, out, "missing messages must leave body unchanged")

	// messages 为非数组（此处为字符串，异常上游体）。
	nonArray := []byte(`{"model":"kimi","messages":"not-an-array"}`)
	out = s.injectAccountIdentitySystemMessage(account, nonArray)
	require.Equal(t, nonArray, out, "non-array messages must leave body unchanged")

	// 非 JSON 的畸形 body 也必须原样返回，绝不报错注入。
	malformed := []byte(`{not valid json`)
	out = s.injectAccountIdentitySystemMessage(account, malformed)
	require.Equal(t, malformed, out, "malformed body must be returned as-is")
}
