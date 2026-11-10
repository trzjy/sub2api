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

// TestIdentityInject_ExistingSystemNotOverwritten 验证客户端已有 system 时合并而非覆盖：
// identity_prompt 前置拼接进首条 system 消息（保留单条 system 满足严格上游约束），
// 原 system 内容与附加字段不被覆盖；连续多条前导 system 也折叠为单条。
func TestIdentityInject_ExistingSystemNotOverwritten(t *testing.T) {
	s := newIdentityInjectService()

	t.Run("string system content merged into single leading system", func(t *testing.T) {
		account := &Account{Extra: map[string]any{"identity_prompt": "injected system"}}
		body := []byte(`{"messages":[{"role":"system","name":"sys0","content":"client system"},{"role":"user","content":"hi"}]}`)

		out := s.injectAccountIdentitySystemMessage(account, body)

		roles := decodeMessagesRoles(t, out)
		require.Equal(t, []string{"system", "user"}, roles, "must merge into single leading system message")

		var parsed struct {
			Messages []struct {
				Role    string `json:"role"`
				Name    string `json:"name"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, json.Unmarshal(out, &parsed))
		require.Equal(t, "injected system\n\nclient system", parsed.Messages[0].Content)
		require.Equal(t, "sys0", parsed.Messages[0].Name, "existing system message metadata must be preserved")
	})

	t.Run("array system content prepends text part and preserves part fields", func(t *testing.T) {
		account := &Account{Extra: map[string]any{"identity_prompt": "injected system"}}
		body := []byte(`{"messages":[{"role":"system","content":[{"type":"text","text":"client system","cache_control":{"type":"ephemeral"}}]},{"role":"user","content":"hi"}]}`)

		out := s.injectAccountIdentitySystemMessage(account, body)

		roles := decodeMessagesRoles(t, out)
		require.Equal(t, []string{"system", "user"}, roles)

		var parsed struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, json.Unmarshal(out, &parsed))
		var parts []struct {
			Type         string         `json:"type"`
			Text         string         `json:"text"`
			CacheControl map[string]any `json:"cache_control,omitempty"`
		}
		require.NoError(t, json.Unmarshal(parsed.Messages[0].Content, &parts))
		require.Len(t, parts, 2)
		require.Equal(t, "injected system", parts[0].Text)
		require.Equal(t, "client system", parts[1].Text)
		require.Equal(t, map[string]any{"type": "ephemeral"}, parts[1].CacheControl)
		var userContent string
		require.NoError(t, json.Unmarshal(parsed.Messages[1].Content, &userContent))
		require.Equal(t, "hi", userContent)
	})

	t.Run("multiple leading system messages collapsed into one", func(t *testing.T) {
		account := &Account{Extra: map[string]any{"identity_prompt": "injected system"}}
		body := []byte(`{"messages":[{"role":"system","content":"sys1"},{"role":"system","content":"sys2"},{"role":"user","content":"hi"}]}`)

		out := s.injectAccountIdentitySystemMessage(account, body)
		roles := decodeMessagesRoles(t, out)
		require.Equal(t, []string{"system", "user"}, roles)

		var parsed struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, json.Unmarshal(out, &parsed))
		require.Equal(t, "injected system\n\nsys1\n\nsys2", parsed.Messages[0].Content)
	})

	t.Run("empty or null leading system content replaced cleanly", func(t *testing.T) {
		account := &Account{Extra: map[string]any{"identity_prompt": "injected system"}}
		for _, body := range [][]byte{
			[]byte(`{"messages":[{"role":"system","content":""},{"role":"user","content":"hi"}]}`),
			[]byte(`{"messages":[{"role":"system","content":null},{"role":"user","content":"hi"}]}`),
		} {
			out := s.injectAccountIdentitySystemMessage(account, body)
			roles := decodeMessagesRoles(t, out)
			require.Equal(t, []string{"system", "user"}, roles)
			var parsed struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			require.NoError(t, json.Unmarshal(out, &parsed))
			require.Equal(t, "injected system", parsed.Messages[0].Content)
		}
	})
}

// TestIdentityInject_LeadingSystemMetadataPreserved 验证多条连续前导 system 中，
// 当后续 system（i>=1）携带除 role/content 外的对象级元数据（name、cache_control
// 等）时，该消息及其字段不得被静默丢弃：identity_prompt 与无元数据的纯
// role/content 前导 system 合并进 messages[0]，而带元数据的后续 system 原样保留。
func TestIdentityInject_LeadingSystemMetadataPreserved(t *testing.T) {
	s := newIdentityInjectService()

	t.Run("second leading system with name is preserved", func(t *testing.T) {
		account := &Account{Extra: map[string]any{"identity_prompt": "injected system"}}
		body := []byte(`{"messages":[{"role":"system","content":"sys1"},{"role":"system","name":"sys2","content":"sys2"},{"role":"user","content":"hi"}]}`)

		out := s.injectAccountIdentitySystemMessage(account, body)

		roles := decodeMessagesRoles(t, out)
		require.Equal(t, []string{"system", "system", "user"}, roles, "metadata-carrying second system must be preserved as a separate message")

		var parsed struct {
			Messages []struct {
				Role    string `json:"role"`
				Name    string `json:"name,omitempty"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, json.Unmarshal(out, &parsed))
		require.Equal(t, "injected system\n\nsys1", parsed.Messages[0].Content, "first system merged with prompt")
		require.Empty(t, parsed.Messages[0].Name, "first system had no name; must stay empty")
		require.Equal(t, "sys2", parsed.Messages[1].Name, "second system name metadata must be preserved")
		require.Equal(t, "sys2", parsed.Messages[1].Content, "second system content must be preserved")
	})

	t.Run("second leading system with cache_control is preserved", func(t *testing.T) {
		account := &Account{Extra: map[string]any{"identity_prompt": "injected system"}}
		body := []byte(`{"messages":[{"role":"system","content":"sys1"},{"role":"system","content":"sys2","cache_control":{"type":"ephemeral"}},{"role":"user","content":"hi"}]}`)

		out := s.injectAccountIdentitySystemMessage(account, body)

		roles := decodeMessagesRoles(t, out)
		require.Equal(t, []string{"system", "system", "user"}, roles)

		var parsed struct {
			Messages []struct {
				Role         string         `json:"role"`
				Content      string         `json:"content"`
				CacheControl map[string]any `json:"cache_control,omitempty"`
			} `json:"messages"`
		}
		require.NoError(t, json.Unmarshal(out, &parsed))
		require.Equal(t, "injected system\n\nsys1", parsed.Messages[0].Content)
		require.Nil(t, parsed.Messages[0].CacheControl, "first system had no cache_control")
		require.Equal(t, "sys2", parsed.Messages[1].Content)
		require.Equal(t, map[string]any{"type": "ephemeral"}, parsed.Messages[1].CacheControl, "second system cache_control metadata must be preserved")
	})

	t.Run("first system metadata kept while second metadata system preserved", func(t *testing.T) {
		account := &Account{Extra: map[string]any{"identity_prompt": "injected system"}}
		body := []byte(`{"messages":[{"role":"system","name":"sys0","content":"sys1"},{"role":"system","name":"sys2","content":"sys2"},{"role":"user","content":"hi"}]}`)

		out := s.injectAccountIdentitySystemMessage(account, body)

		roles := decodeMessagesRoles(t, out)
		require.Equal(t, []string{"system", "system", "user"}, roles)

		var parsed struct {
			Messages []struct {
				Role    string `json:"role"`
				Name    string `json:"name,omitempty"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, json.Unmarshal(out, &parsed))
		require.Equal(t, "injected system\n\nsys1", parsed.Messages[0].Content)
		require.Equal(t, "sys0", parsed.Messages[0].Name, "first system own metadata must be preserved")
		require.Equal(t, "sys2", parsed.Messages[1].Name, "second system name metadata must be preserved")
		require.Equal(t, "sys2", parsed.Messages[1].Content)
	})

	t.Run("only the first metadata system stops folding; earlier pure systems merge", func(t *testing.T) {
		account := &Account{Extra: map[string]any{"identity_prompt": "injected system"}}
		body := []byte(`{"messages":[{"role":"system","content":"sys1"},{"role":"system","content":"sys2"},{"role":"system","name":"sys3","content":"sys3"},{"role":"user","content":"hi"}]}`)

		out := s.injectAccountIdentitySystemMessage(account, body)

		roles := decodeMessagesRoles(t, out)
		require.Equal(t, []string{"system", "system", "user"}, roles, "only systems before the metadata one are folded")

		var parsed struct {
			Messages []struct {
				Role    string `json:"role"`
				Name    string `json:"name,omitempty"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, json.Unmarshal(out, &parsed))
		require.Equal(t, "injected system\n\nsys1\n\nsys2", parsed.Messages[0].Content, "two pure leading systems merged")
		require.Empty(t, parsed.Messages[0].Name)
		require.Equal(t, "sys3", parsed.Messages[1].Name, "metadata system preserved")
		require.Equal(t, "sys3", parsed.Messages[1].Content)
	})

	t.Run("array content with metadata stop keeps part fields", func(t *testing.T) {
		account := &Account{Extra: map[string]any{"identity_prompt": "injected system"}}
		body := []byte(`{"messages":[{"role":"system","content":[{"type":"text","text":"sys1"}]},{"role":"system","content":[{"type":"text","text":"sys2"}],"cache_control":{"type":"ephemeral"}},{"role":"user","content":"hi"}]}`)

		out := s.injectAccountIdentitySystemMessage(account, body)

		roles := decodeMessagesRoles(t, out)
		require.Equal(t, []string{"system", "system", "user"}, roles)

		var parsed struct {
			Messages []struct {
				Role         string          `json:"role"`
				Content      json.RawMessage `json:"content"`
				CacheControl map[string]any  `json:"cache_control,omitempty"`
			} `json:"messages"`
		}
		require.NoError(t, json.Unmarshal(out, &parsed))
		var firstParts []struct {
			Text string `json:"text"`
		}
		require.NoError(t, json.Unmarshal(parsed.Messages[0].Content, &firstParts))
		require.Len(t, firstParts, 2)
		require.Equal(t, "injected system", firstParts[0].Text)
		require.Equal(t, "sys1", firstParts[1].Text)
		require.Nil(t, parsed.Messages[0].CacheControl)
		var secondParts []struct {
			Text string `json:"text"`
		}
		require.NoError(t, json.Unmarshal(parsed.Messages[1].Content, &secondParts))
		require.Len(t, secondParts, 1)
		require.Equal(t, "sys2", secondParts[0].Text)
		require.Equal(t, map[string]any{"type": "ephemeral"}, parsed.Messages[1].CacheControl)
	})
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
