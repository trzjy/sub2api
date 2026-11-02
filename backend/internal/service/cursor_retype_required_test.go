package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// 11-03 活体：上游只交来空参数（Bash input={}），旧实现把 required 原样保留却从不
// 消费，空对象被当成"重打成功"直接交付给客户端 → 客户端 InputValidationError →
// 回填后撞续轮校验（CUR-RES-04）。本组用例锁死新口径：缺必填即 missing_required，
// 且该类失败不可降级交付。
func TestRetypeArgsMissingRequiredIsRejected(t *testing.T) {
	bashSchema := []byte(`{
		"type":"object",
		"properties":{"command":{"type":"string"},"description":{"type":"string"}},
		"required":["command"],
		"additionalProperties":false
	}`)

	_, err := cursorRetypeMcpArgs([]byte(`{}`), bashSchema)
	if err == nil {
		t.Fatal("空参数必须按缺必填拒绝，实际被放行")
	}
	var re *cursorRetypeError
	if !errors.As(err, &re) {
		t.Fatalf("错误类型不是 cursorRetypeError: %v", err)
	}
	if re.Reason() != cursorRetypeMissingRequired {
		t.Fatalf("reason = %q, want %q", re.Reason(), cursorRetypeMissingRequired)
	}
	// 必须不可降级：降级就会把残调用交给客户端，正是本次要消灭的行为。
	if cursorRetypeDegradable(re.Reason()) {
		t.Fatal("missing_required 不得判为可降级")
	}
}

func TestRetypeArgsRequiredPresentPasses(t *testing.T) {
	bashSchema := []byte(`{
		"type":"object",
		"properties":{"command":{"type":"string"},"timeout":{"type":"number"}},
		"required":["command"],
		"additionalProperties":false
	}`)

	out, err := cursorRetypeMcpArgs([]byte(`{"command":"ls -la"}`), bashSchema)
	if err != nil {
		t.Fatalf("完整参数不应失败: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	if got["command"] != "ls -la" {
		t.Fatalf("command = %v, want ls -la", got["command"])
	}
}

// 无 required 声明的工具（ListAgents / CronList 这类无参工具）空对象必须照旧放行：
// 修复只拦"缺必填"，不得扩大成"一律不许空参数"。
func TestRetypeArgsNoRequiredAllowsEmpty(t *testing.T) {
	schema := []byte(`{"type":"object","properties":{},"additionalProperties":false}`)
	out, err := cursorRetypeMcpArgs([]byte(`{}`), schema)
	if err != nil {
		t.Fatalf("无 required 的工具空参数应放行: %v", err)
	}
	if strings.TrimSpace(string(out)) != "{}" {
		t.Fatalf("out = %s, want {}", out)
	}
}

// schema 归一化必须保留 required，否则上面的校验在真实链路（走索引后的 schema）失效。
func TestNormalizedSchemaKeepsRequired(t *testing.T) {
	raw := []byte(`{
		"type":"object",
		"properties":{"command":{"type":"string"}},
		"required":["command"],
		"additionalProperties":false
	}`)
	normalized := cursorNormalizeIndexedSchema("Bash", raw)
	if normalized == nil {
		t.Fatal("归一化不应失败")
	}
	var root map[string]any
	if err := json.Unmarshal(normalized, &root); err != nil {
		t.Fatalf("归一化输出不是合法 JSON: %v", err)
	}
	req, ok := root["required"]
	if !ok {
		t.Fatalf("归一化后丢失 required: %s", string(normalized))
	}
	list, ok := req.([]any)
	if !ok || len(list) != 1 || list[0] != "command" {
		t.Fatalf("required = %v, want [command]", req)
	}
	// 用归一化后的 schema 复核：空参数仍要被拒。
	if _, err := cursorRetypeMcpArgs([]byte(`{}`), normalized); err == nil {
		t.Fatal("归一化 schema 下空参数仍应拒绝")
	}
}

// 形状差异类（多给未声明字段）保持可降级：11-03 03:42 的 Write 场景不能被这次修复反向打死。
func TestRetypeDegradableKeepsShapeDiffs(t *testing.T) {
	for _, reason := range []cursorRetypeErrReason{
		cursorRetypeAdditionalPropPresent,
		cursorRetypeObjectTypeMismatch,
		cursorRetypeNumericOutOfRange,
		cursorRetypeItemsNotAllowed,
	} {
		if !cursorRetypeDegradable(reason) {
			t.Fatalf("%q 应保持可降级", reason)
		}
	}
	for _, reason := range []cursorRetypeErrReason{
		cursorRetypeMissingRequired,
		cursorRetypeSchemaAbsent,
		cursorRetypeSchemaInvalid,
		cursorRetypeSchemaUnresolvable,
		cursorRetypeProjectionUnresolvable,
	} {
		if cursorRetypeDegradable(reason) {
			t.Fatalf("%q 不得判为可降级", reason)
		}
	}
}
