package apicompat

import (
	"encoding/json"
	"strings"
	"testing"
)

// dsmlRealBashSample 取自线上真实泄漏（会话 8037857e，deepseek-v4.1-flash-1017）。
const dsmlRealBashSample = "<｜DSML｜tool_calls>\n" +
	"<｜DSML｜invoke name=\"Bash\">\n" +
	"<｜DSML｜parameter name=\"command\" string=\"true\">cd /mnt/data/sub2api && cat >> 当前任务.md << 'EOF'\n" +
	"\n### 2026-10-29 Cursor 读循环终态观测收尾（分支 `fix/cursor-final-terminal-20261029`，commit `feb305ebb`）\n" +
	"\n接力 434706daa 之后被强制中断的终态观测收尾。\n" +
	"EOF\n" +
	"echo '=== 回写完成 ==='; tail -3 当前任务.md</｜DSML｜parameter>\n" +
	"<｜DSML｜parameter name=\"description\" string=\"true\">回写任务文档登记收尾</｜DSML｜parameter>\n" +
	"</｜DSML｜invoke>\n" +
	"</｜DSML｜tool_calls>"

// dsmlRealEditSample 取自线上真实泄漏（会话 405c692f，glm-5.3-flash）。
// 关键：old_string / new_string 带制表符缩进，必须逐字保留。
const dsmlRealEditSample = "<｜DSML｜tool_calls>\n" +
	"<｜DSML｜invoke name=\"Edit\">\n" +
	"<｜DSML｜parameter name=\"file_path\" string=\"true\">/mnt/data/sub2api/.worktrees/x/backend/internal/service/admin_group.go</｜DSML｜parameter>\n" +
	"<｜DSML｜parameter name=\"new_string\" string=\"true\">func (s *adminServiceImpl) GetGroupModelsListCandidates(ctx context.Context, id int64, platform string) ([]string, error) {\n" +
	"\tplatform = strings.TrimSpace(platform)\n" +
	"\treturn s.groupRepo.ListModelsCandidates(ctx, id, platform)\n" +
	"}</｜DSML｜parameter>\n" +
	"<｜DSML｜parameter name=\"old_string\" string=\"true\">func (s *adminServiceImpl) GetGroupModelsListCandidates(ctx context.Context, id int64, platform string) ([]string, error) {\n" +
	"\tplatform = strings.TrimSpace(platform)\n" +
	"\treturn s.groupRepo.ListModelsCandidates(ctx, id, platform)\n" +
	"}</｜DSML｜parameter>\n" +
	"</｜DSML｜invoke>\n" +
	"</｜DSML｜tool_calls>"

func TestParseDSMLToolCalls_RealBashSample(t *testing.T) {
	calls := ParseDSMLToolCalls(dsmlRealBashSample)
	if len(calls) != 1 {
		t.Fatalf("期望 1 个调用，实际 %d 个", len(calls))
	}
	if calls[0].Name != "Bash" {
		t.Fatalf("工具名应为 Bash，实际 %q", calls[0].Name)
	}
	args := map[string]any{}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("参数应为合法 JSON: %v", err)
	}
	cmd, _ := args["command"].(string)
	if !strings.Contains(cmd, "cat >> 当前任务.md") {
		t.Fatalf("command 参数内容不正确: %q", cmd)
	}
	if !strings.Contains(cmd, "\n\n### 2026-10-29") {
		t.Fatalf("command 参数里的换行必须原样保留: %q", cmd)
	}
	if args["description"] != "回写任务文档登记收尾" {
		t.Fatalf("description 参数不正确: %v", args["description"])
	}
}

func TestParseDSMLToolCalls_RealEditSampleKeepsIndentation(t *testing.T) {
	calls := ParseDSMLToolCalls(dsmlRealEditSample)
	if len(calls) != 1 {
		t.Fatalf("期望 1 个调用，实际 %d 个", len(calls))
	}
	if calls[0].Name != "Edit" {
		t.Fatalf("工具名应为 Edit，实际 %q", calls[0].Name)
	}
	args := map[string]any{}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("参数应为合法 JSON: %v", err)
	}
	// 缩进必须逐字保留（NVIDIA TensorRT-LLM PR #21025 的教训）。
	newStr, _ := args["new_string"].(string)
	if !strings.Contains(newStr, "\n\tplatform = strings.TrimSpace(platform)") {
		t.Fatalf("new_string 的制表符缩进被吃掉了: %q", newStr)
	}
	if !strings.Contains(newStr, "\n}") {
		t.Fatalf("new_string 的结尾结构不正确: %q", newStr)
	}
}

// TestParseDSMLToolCalls_V41Dialect 覆盖 V4.1 带空格方言（线上两种都出现过）。
func TestParseDSMLToolCalls_V41Dialect(t *testing.T) {
	in := `<｜DSML｜ calls>
<｜DSML｜ invoke name="websearch">
<｜DSML｜ parameter name="query" string="true">天气</｜DSML｜ parameter>
</｜DSML｜ invoke>
</｜DSML｜ calls>`
	calls := ParseDSMLToolCalls(in)
	if len(calls) != 1 {
		t.Fatalf("V4.1 方言期望 1 个调用，实际 %d 个", len(calls))
	}
	if calls[0].Name != "websearch" {
		t.Fatalf("工具名应为 websearch，实际 %q", calls[0].Name)
	}
	args := map[string]any{}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("参数应为合法 JSON: %v", err)
	}
	if args["query"] != "天气" {
		t.Fatalf("query 参数不正确: %v", args["query"])
	}
}

// TestParseDSMLToolCalls_StringFalseUsesJSON 覆盖 string="false" 走 JSON 解析。
func TestParseDSMLToolCalls_StringFalseUsesJSON(t *testing.T) {
	in := `<｜DSML｜tool_calls><｜DSML｜invoke name="X">` +
		`<｜DSML｜parameter name="count" string="false">5</｜DSML｜parameter>` +
		`<｜DSML｜parameter name="obj" string="false">{"a":1}</｜DSML｜parameter>` +
		`</｜DSML｜invoke></｜DSML｜tool_calls>`
	calls := ParseDSMLToolCalls(in)
	if len(calls) != 1 {
		t.Fatalf("期望 1 个调用，实际 %d 个", len(calls))
	}
	args := map[string]any{}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("参数应为合法 JSON: %v", err)
	}
	if args["count"] != float64(5) {
		t.Fatalf("count 应为数字 5，实际 %v(%T)", args["count"], args["count"])
	}
	obj, ok := args["obj"].(map[string]any)
	if !ok || obj["a"] != float64(1) {
		t.Fatalf("obj 应为对象，实际 %v", args["obj"])
	}
}

// TestParseDSMLToolCalls_MissingStringAttr 覆盖 string 属性缺失（官方：按 false 处理）。
func TestParseDSMLToolCalls_MissingStringAttr(t *testing.T) {
	in := `<｜DSML｜tool_calls><｜DSML｜invoke name="X">` +
		`<｜DSML｜parameter name="plain">hello</｜DSML｜parameter>` +
		`</｜DSML｜invoke></｜DSML｜tool_calls>`
	calls := ParseDSMLToolCalls(in)
	if len(calls) != 1 {
		t.Fatalf("期望 1 个调用，实际 %d 个", len(calls))
	}
	args := map[string]any{}
	if err := json.Unmarshal([]byte(calls[0].Arguments), &args); err != nil {
		t.Fatalf("参数应为合法 JSON: %v", err)
	}
	// 不是合法 JSON → 回退字面文本。
	if args["plain"] != "hello" {
		t.Fatalf("plain 参数应回退为字面文本，实际 %v", args["plain"])
	}
}

// TestParseDSMLToolCalls_ParallelCalls 覆盖并行多次调用。
func TestParseDSMLToolCalls_ParallelCalls(t *testing.T) {
	in := `<｜DSML｜tool_calls>` +
		`<｜DSML｜invoke name="A"><｜DSML｜parameter name="x" string="true">1</｜DSML｜parameter></｜DSML｜invoke>` +
		`<｜DSML｜invoke name="B"><｜DSML｜parameter name="y" string="true">2</｜DSML｜parameter></｜DSML｜invoke>` +
		`</｜DSML｜tool_calls>`
	calls := ParseDSMLToolCalls(in)
	if len(calls) != 2 {
		t.Fatalf("期望 2 个调用，实际 %d 个", len(calls))
	}
	if calls[0].Name != "A" || calls[1].Name != "B" {
		t.Fatalf("工具名顺序不正确: %q %q", calls[0].Name, calls[1].Name)
	}
}

// TestParseDSMLToolCalls_FragmentYieldsNil 覆盖残片：解析不出就返回 nil（调用方丢弃）。
func TestParseDSMLToolCalls_FragmentYieldsNil(t *testing.T) {
	frag := "</｜DSML｜parameter>\n</｜DSML｜invoke>\n</｜DSML｜tool_calls>"
	if calls := ParseDSMLToolCalls(frag); calls != nil {
		t.Fatalf("残片应解析为空，实际 %d 个", len(calls))
	}
}

// TestParseDSMLToolCalls_ProductionVariants 覆盖 vLLM 收录的生产变体拼写。
func TestParseDSMLToolCalls_ProductionVariants(t *testing.T) {
	for _, start := range dsmlToolStartVariants {
		in := start + `<｜DSML｜invoke name="X">` +
			`<｜DSML｜parameter name="k" string="true">v</｜DSML｜parameter>` +
			`</｜DSML｜invoke>` + strings.Replace(start, "<", "</", 1)
		calls := ParseDSMLToolCalls(in)
		if len(calls) != 1 {
			t.Fatalf("变体 %q 期望 1 个调用，实际 %d 个", start, len(calls))
		}
		if calls[0].Name != "X" {
			t.Fatalf("变体 %q 工具名错误: %q", start, calls[0].Name)
		}
	}
}

// TestStripDSML_RemovesProtocolTags 确认协议标签不外泄给客户端。
func TestStripDSML_RemovesProtocolTags(t *testing.T) {
	dirty := "前面的话" + dsmlRealBashSample + "后面的话"
	clean := StripDSML(dirty)
	if strings.Contains(clean, dsmlMarker) {
		t.Fatalf("摘除后仍含 DSML 标记: %q", clean)
	}
	if !strings.Contains(clean, "前面的话") || !strings.Contains(clean, "后面的话") {
		t.Fatalf("摘除时误伤了正文: %q", clean)
	}
}

// TestStripDSML_RemovesFragments 确认孤立残片也被清掉。
func TestStripDSML_RemovesFragments(t *testing.T) {
	dirty := "正文" + "</｜DSML｜parameter>" + "还有正文"
	clean := StripDSML(dirty)
	if strings.Contains(clean, dsmlMarker) {
		t.Fatalf("残片未被清掉: %q", clean)
	}
}

// TestStripDSML_LeavesPlainText 确认普通文本不受影响。
func TestStripDSML_LeavesPlainText(t *testing.T) {
	plain := "这是一段完全正常的回复，里面没有任何特殊标记。"
	if got := StripDSML(plain); got != plain {
		t.Fatalf("普通文本被改动了: %q", got)
	}
}

// TestDSMLToolCallToChatToolCall 确认转成 CC 形状正确。
func TestDSMLToolCallToChatToolCall(t *testing.T) {
	calls := ParseDSMLToolCalls(dsmlRealBashSample)
	if len(calls) != 1 {
		t.Fatalf("期望 1 个调用，实际 %d 个", len(calls))
	}
	tc := DSMLToolCallToChatToolCall(calls[0], 0)
	if tc.Type != "function" {
		t.Fatalf("type 应为 function，实际 %q", tc.Type)
	}
	if tc.Function.Name != "Bash" {
		t.Fatalf("function.name 应为 Bash，实际 %q", tc.Function.Name)
	}
	if tc.Index == nil || *tc.Index != 0 {
		t.Fatalf("index 应为 0，实际 %v", tc.Index)
	}
	if tc.ID == "" {
		t.Fatal("id 不应为空")
	}
	if !json.Valid([]byte(tc.Function.Arguments)) {
		t.Fatalf("arguments 应为合法 JSON: %q", tc.Function.Arguments)
	}
}

// TestDSMLStreamBuffer_AccumulatesAcrossFrames 覆盖流式跨帧：DSML 被切开也能凑齐。
func TestDSMLStreamBuffer_AccumulatesAcrossFrames(t *testing.T) {
	var b DSMLStreamBuffer
	frames := []string{
		"<｜DSML｜tool_",
		"calls>\n<｜DSML｜invoke name=\"Bash\">\n",
		"<｜DSML｜parameter name=\"command\" string=\"true\">ls -la",
		" /tmp</｜DSML｜parameter>\n",
		"</｜DSML｜invoke>\n</｜DSML｜tool_calls>",
	}
	var got []DSMLToolCall
	for _, f := range frames {
		plain, calls := b.Feed(f)
		if plain != "" {
			t.Fatalf("DSML 期间的透传内容不应包含协议片段: %q", plain)
		}
		if len(calls) > 0 {
			got = calls
		}
	}
	if len(got) != 1 {
		t.Fatalf("跨帧后应还原出 1 个调用，实际 %d 个", len(got))
	}
	if got[0].Name != "Bash" {
		t.Fatalf("工具名应为 Bash，实际 %q", got[0].Name)
	}
	args := map[string]any{}
	if err := json.Unmarshal([]byte(got[0].Arguments), &args); err != nil {
		t.Fatalf("参数应为合法 JSON: %v", err)
	}
	if args["command"] != "ls -la /tmp" {
		t.Fatalf("command 不正确: %v", args["command"])
	}
}

// TestDSMLStreamBuffer_PassesThroughPlainText 确认普通 delta 原样透传。
func TestDSMLStreamBuffer_PassesThroughPlainText(t *testing.T) {
	var b DSMLStreamBuffer
	plain, calls := b.Feed("普通正文")
	if plain != "普通正文" {
		t.Fatalf("普通正文应原样透传: %q", plain)
	}
	if len(calls) != 0 {
		t.Fatalf("普通正文不应产出调用: %d", len(calls))
	}
}

// TestDSMLStreamBuffer_FragmentDiscardedOnFlush 确认流尾残片被丢弃、不外泄。
func TestDSMLStreamBuffer_FragmentDiscardedOnFlush(t *testing.T) {
	var b DSMLStreamBuffer
	b.Feed("<｜DSML｜tool_calls><｜DSML｜invoke name=\"X\">")
	if calls := b.Flush(); calls != nil {
		t.Fatalf("残片应被丢弃，实际 %d 个", len(calls))
	}
}
