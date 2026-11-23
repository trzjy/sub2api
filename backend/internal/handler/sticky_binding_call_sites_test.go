//go:build unit

package handler

import (
	"os"
	"strings"
	"testing"
)

const (
	bindingDirectCall  = "BindStickySession("
	bindingWrapperCall = "BindStickySessionAfterProfitAdmission("
)

// successForwardMarkers 是"转发成功入口"的保守字符串标记集合：
// 命中任一标记即视为该文件存在转发调用，不做控制流分析。
var successForwardMarkers = []string{
	"ForwardMessages(",
	"ForwardAnthropicMessages(",
	"ForwardOpenAI",
	"ForwardGemini",
	"ForwardResponses",
	"ForwardChatCompletions",
}

// documentedSuccessPathAllowlist 是源码锁的文件级白名单。
// 这些生产文件含绑定写入点，但其转发入口的调用名不在 successForwardMarkers
// 候选集合内（转发经本协议专用封装/helper 完成），故跳过"同文件须含转发标记"
// 的保守断言。各文件的绑定点本身均位于转发成功分支之后（见文件内注释）。
var documentedSuccessPathAllowlist = map[string]string{
	"gateway_handler_chat_completions.go": "CC 专用转发封装，入口标记不在候选集合；绑定点位于成功分支（失败路径已 return）",
	"gateway_handler_responses.go":        "Responses 专用转发封装，入口标记不在候选集合；绑定点位于成功分支",
	"openai_chat_completions.go":          "OpenAI Chat 专用转发封装（submitChatUsage），入口标记不在候选集合；绑定点位于成功分支",
	"openai_gateway_handler.go":           "OpenAI 专用转发封装（submitResponsesUsage），入口标记不在候选集合；绑定点位于成功分支",
}

// handlerProductionSources 读取 handler 包当前目录下所有生产 .go 源文件，
// 返回 文件名 -> 源码 的映射。测试运行时 cwd 即本包目录。
func handlerProductionSources(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("os.ReadDir(\".\") 失败: %v", err)
	}
	sources := make(map[string]string)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读取生产文件 %s 失败: %v", name, err)
		}
		sources[name] = string(b)
	}
	return sources
}

// TestHandlerBindsStickySessionOnlyThroughWrapper 禁止 handler 包绕过利润门
// 直连 BindStickySession(，所有写入必须走 BindStickySessionAfterProfitAdmission(。
func TestHandlerBindsStickySessionOnlyThroughWrapper(t *testing.T) {
	sources := handlerProductionSources(t)
	wrapperFileCount := 0
	for name, src := range sources {
		if strings.Contains(src, bindingDirectCall) {
			t.Errorf("生产文件 %s 出现直连 %s 调用：绑定必须统一走 %s（利润门）",
				name, bindingDirectCall, bindingWrapperCall)
		}
		if strings.Contains(src, bindingWrapperCall) {
			wrapperFileCount++
		}
	}
	if wrapperFileCount < 1 {
		t.Fatalf("没有任何生产文件出现 %s（命中文件数=0）；扫描疑似失效，防回退锁不成立", bindingWrapperCall)
	}
}

// TestHandlerBindingWritesAreDocumentedSuccessPaths 保守断言：含绑定写入点的
// 生产文件，同文件须含至少一个转发成功入口标记，防止绑定写入出现在完全不转发
// 的选号文件里。不做控制流分析；不满足的文件须在 documentedSuccessPathAllowlist
// 中显式声明原因。
func TestHandlerBindingWritesAreDocumentedSuccessPaths(t *testing.T) {
	sources := handlerProductionSources(t)

	// 白名单保鲜：条目必须仍指向含绑定写入点的现存生产文件，否则应清理。
	for name := range documentedSuccessPathAllowlist {
		src, ok := sources[name]
		if !ok {
			t.Errorf("白名单条目 %s 已不是现存生产文件，请清理白名单", name)
			continue
		}
		if !strings.Contains(src, bindingWrapperCall) {
			t.Errorf("白名单条目 %s 已不含绑定写入点，请清理白名单", name)
		}
	}

	checked := 0
	for name, src := range sources {
		if !strings.Contains(src, bindingWrapperCall) {
			continue
		}
		checked++
		if reason, ok := documentedSuccessPathAllowlist[name]; ok {
			t.Logf("白名单跳过 %s: %s", name, reason)
			continue
		}
		hasForward := false
		for _, marker := range successForwardMarkers {
			if strings.Contains(src, marker) {
				hasForward = true
				break
			}
		}
		if !hasForward {
			t.Errorf("生产文件 %s 含绑定写入点 %s，但不含任何转发成功入口标记 %v；疑似在完全不转发的选号文件写绑定",
				name, bindingWrapperCall, successForwardMarkers)
		}
	}
	if checked == 0 {
		t.Fatalf("没有任何生产文件含绑定写入点（命中文件数=0）；扫描疑似失效，防回退锁不成立")
	}
}
