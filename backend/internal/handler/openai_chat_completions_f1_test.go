//go:build unit

package handler

// F2-a（本卡）测试：OpenAI chat/completions 路径在 failover 耗尽分支补齐 streamStarted
// 同步——响应已提交（心跳已提交 SSE 200，c.Writer.Written() 为 true）但 streamStarted 仍为
// false 时，必须把 streamStarted 置为 true，使 handleFailoverExhausted 写出流内 SSE error
// 帧（含 data: 前缀、可区分 type service.UpstreamFirstByteTimeoutType），而不是把通用 JSON
// 错误体塞进已开的 SSE 流。
//
// 背景（生产实证，见方案文档 §1/§2）：客户走 OpenAI 兼容 kimi 账号，路径 /v1/chat/completions。
// 首字节护栏超时 504 触发 failover 耗尽，但客户端最终收到的是通用 JSON 文案，而不是可区分 type
// 的流内帧——根因就是该路径 streamStarted 恒为 false、未与「响应是否已提交」对齐。
//
// 反证（硬性交付项）：删掉 syncStreamStartedIfResponseCommitted 的同步逻辑（回到基线形态）后，
// 本文件所有 TestF2f* 用例必须 FAIL；还原后必须 PASS。

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newF2fGinContext 构造一个最小 gin 测试上下文。
func newF2fGinContext() (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c, w
}

// TestF2fStreamStartedSync_CommittedWriterFlips 覆盖最核心的同步语义：响应已提交
// （心跳已提交 SSE 200）时，sync 必须把 streamStarted 从 false 翻转为 true。
// 这是 F2-a 修复的本质；删掉同步后本用例断言失败（反证）。
func TestF2fStreamStartedSync_CommittedWriterFlips(t *testing.T) {
	c, _ := newF2fGinContext()
	// 模拟心跳已提交 SSE 200：生产心跳向 Writer 写出 SSE 注释帧，gin 在首次 Write 时
	// 自动提交 200 头并将 Writer.Written() 置为 true。这里写出同型字节还原该真实组合。
	_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
	require.True(t, c.Writer.Written(), "前置条件：响应必须已提交")

	var streamStarted bool
	syncStreamStartedIfResponseCommitted(c, &streamStarted)

	assert.True(t, streamStarted,
		"响应已提交但 streamStarted 仍为 false 时，sync 必须将其翻转为 true")
}

// TestF2fStreamStartedSync_UncommittedStaysFalse 覆盖反向：响应确实未提交时，
// sync 不得把 streamStarted 翻转为 true（保持既有的非流式分支语义不变）。
func TestF2fStreamStartedSync_UncommittedStaysFalse(t *testing.T) {
	c, _ := newF2fGinContext()
	require.False(t, c.Writer.Written(), "前置条件：响应必须未提交")

	var streamStarted bool
	syncStreamStartedIfResponseCommitted(c, &streamStarted)

	assert.False(t, streamStarted,
		"响应未提交时，sync 不得翻转 streamStarted（保持非流式 JSON 分支）")
}

// TestF2fFirstByteTimeout_CommittedEmitsSSEFrame 覆盖验收点 (a)/(c)/(d)：
// 响应已提交 + streamStarted 经 sync 置 true → 写出流内 SSE error 帧（含 data: 前缀），
// 帧 type 可区分（service.UpstreamFirstByteTimeoutType），message 引用流内帧 SSOT 文案且
// 不含任何类取消触发词。注意：这里复现生产路径"先 sync 再 handleFailoverExhausted"，
// 与 openai_chat_completions.go 的调用顺序一致。
func TestF2fFirstByteTimeout_CommittedEmitsSSEFrame(t *testing.T) {
	c, w := newF2fGinContext()
	// 模拟心跳已提交 SSE 200（生产实证中的真实组合：心跳写出 SSE 注释帧提交 200）。
	_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
	require.True(t, c.Writer.Written(), "前置条件：响应必须已提交")

	h := &OpenAIGatewayHandler{}
	var streamStarted bool
	syncStreamStartedIfResponseCommitted(c, &streamStarted)
	require.True(t, streamStarted, "同步后 streamStarted 必须为 true")

	h.handleFailoverExhausted(c, m1cFirstByteTimeoutFailover(), streamStarted)

	body := w.Body.String()

	// (a) 必须是流内 SSE error 帧，不是 JSON 体形态。
	require.Contains(t, body, "data:",
		"响应已提交 + sync 后必须写出流内 SSE error 帧（含 data: 前缀），实际: %q", body)
	require.Contains(t, body, "event: error",
		"必须写出 event: error 流内帧，实际: %q", body)
	// JSON 体形态以 {"error":{...}} 开头（无 data: 前缀）；帧形态不应是纯 JSON。
	require.NotEqual(t, '{', strings.TrimSpace(body)[0],
		"不得是纯 JSON 错误体形态（应含 SSE data: 前缀），实际: %q", body)

	// (d) 帧必须携带可区分的类型标识。
	require.Contains(t, body, service.UpstreamFirstByteTimeoutType,
		"流内帧必须携带可区分的错误类型标识 %q，实际: %q",
		service.UpstreamFirstByteTimeoutType, body)

	// 解析 data 行上的 JSON 错误体。
	idx := strings.Index(body, "data: ")
	require.GreaterOrEqual(t, idx, 0, "响应体应包含 data: 行，实际: %q", body)
	raw := strings.TrimSpace(body[idx+len("data: "):])
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &parsed),
		"流内帧 JSON 应可解析，原始: %q", raw)
	errorObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok, "错误帧应含 error 对象，实际: %v", parsed)

	assert.Equal(t, service.UpstreamFirstByteTimeoutType, errorObj["type"],
		"错误帧 type 必须是可区分的首字节超时类型")
	assert.Equal(t, service.UpstreamFirstByteTimeoutType, errorObj["code"],
		"错误帧 code 必须与 type 一致，确保客户端可区分")

	msg, _ := errorObj["message"].(string)
	require.NotEmpty(t, msg, "错误帧必须携带 message")
	// 流内帧应使用流内错误帧专属文案 SSOT。
	assert.Equal(t, infraerrors.UpstreamFirstByteTimeoutStreamFrame, msg,
		"流已开始时应引用流内错误帧专属 SSOT 文案")
	// (c) 文案不含任何类取消触发词（沿用 F1-b 禁用词表）。
	assertNoCancellationLikeWords(t, msg)
}

// TestF2fFirstByteTimeout_UncommittedEmitsJSON 覆盖反向场景：响应确实未提交 →
// 写出 JSON 错误体（既有行为不变），不得是流内 SSE 帧。
func TestF2fFirstByteTimeout_UncommittedEmitsJSON(t *testing.T) {
	c, w := newF2fGinContext()
	require.False(t, c.Writer.Written(), "前置条件：响应必须未提交")

	h := &OpenAIGatewayHandler{}
	var streamStarted bool
	syncStreamStartedIfResponseCommitted(c, &streamStarted)
	require.False(t, streamStarted, "未提交时 streamStarted 必须为 false")

	h.handleFailoverExhausted(c, m1cFirstByteTimeoutFailover(), streamStarted)

	body := w.Body.String()
	// 不得写出流内 SSE 帧。
	require.NotContains(t, body, "event: error",
		"未提交响应时不得写出流内 SSE 帧，实际: %q", body)
	require.NotContains(t, body, "data:",
		"未提交响应时不得写出 data: 前缀帧，实际: %q", body)

	// 必须是 JSON 错误体。
	var parsed map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &parsed),
		"未提交时应返回 JSON 错误体，实际: %q", body)
	errorObj, ok := parsed["error"].(map[string]any)
	require.True(t, ok, "JSON 错误体应含 error 对象，实际: %v", parsed)
	assert.Equal(t, service.UpstreamFirstByteTimeoutType, errorObj["type"],
		"未提交错误体 type 也必须是可区分的首字节超时类型")
	msg, _ := errorObj["message"].(string)
	require.NotEmpty(t, msg)
	assertNoCancellationLikeWords(t, msg)
	// 流未开始应使用 JSON 错误体专属文案 SSOT。
	assert.Equal(t, infraerrors.UpstreamFirstByteTimeoutJSON, msg,
		"流未开始时应引用 JSON 错误体专属 SSOT 文案")
}

// ---------------------------------------------------------------------------
// F2-b（主会话补强）：锁定生产调用点。
//
// 盲区说明：F2-a 的四个 TestF2f* 用例全部在测试里**自己调用**
// syncStreamStartedIfResponseCommitted(c, &streamStarted)，再调用
// handleFailoverExhausted——**绕过了生产代码里的调用点**。
// 因此把生产文件 openai_chat_completions.go 里那行同步调用删掉（回到基线形态），
// 这四个用例仍然全部 PASS（主会话已实测确认），即测试没有锁住"调用点是否接上"。
//
// 补强方式：源码级锁定——用 go/ast 解析生产文件，断言 ChatCompletions 函数体内
// 每一处**传入 streamStarted 变量**的 handleFailoverExhausted 调用，在其所属语句块内
// 之前都存在 syncStreamStartedIfResponseCommitted(c, &streamStarted) 调用。
// 删掉任一生产调用 → 本用例 FAIL。
// ---------------------------------------------------------------------------

// f2bLockedCallSites 统计本次锁定的调用点数量（要求至少一处）。
var f2bLockedCallSites int

// TestF2bSourceLock_SyncCallPrecedesHandleFailoverExhausted 源码级锁定生产调用点。
//
// 反证：把生产文件里任一 syncStreamStartedIfResponseCommitted 调用删掉后，
// 本用例必须 FAIL。
func TestF2bSourceLock_SyncCallPrecedesHandleFailoverExhausted(t *testing.T) {
	fset := token.NewFileSet()
	// 解析本包同目录下的生产文件（测试与生产文件同目录）。
	f, err := parser.ParseFile(fset, "openai_chat_completions.go", nil, parser.ParseComments)
	require.NoError(t, err, "必须能解析生产文件 openai_chat_completions.go")

	// 定位 ChatCompletions 函数体。
	var chatBody *ast.BlockStmt
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "ChatCompletions" || fd.Body == nil {
			continue
		}
		chatBody = fd.Body
		break
	}
	require.NotNil(t, chatBody, "必须找到 OpenAIGatewayHandler.ChatCompletions 函数体")

	f2bLockedCallSites = 0

	// checkStmts 检查同一语句块内的语句序列（保持源码顺序）。
	//
	// 关键：不能用 ast.Inspect 全量收集再按 Pos 排序——外层 if 语句的 Pos 早于其内部
	// 语句，会把「同步调用在耗尽调用之前」的关系打乱。这里逐块收集，块内顺序即源码顺序。
	var checkStmts func(stmts []ast.Stmt)
	checkStmts = func(stmts []ast.Stmt) {
		for i, st := range stmts {
			syncBefore := false
			for j := 0; j < i; j++ {
				if f2bStmtCallsSyncHelper(stmts[j]) {
					syncBefore = true
					break
				}
			}
			if f2bFailoverCallWithStreamStartedVar(st) != nil {
				require.Truef(t, syncBefore,
					"ChatCompletions 内有一处 handleFailoverExhausted(c, ..., streamStarted) 调用，"+
						"但其所在语句块内、该调用之前没有 syncStreamStartedIfResponseCommitted(c, &streamStarted)；"+
						"响应已提交（心跳已提交 SSE 200）而 streamStarted 仍为 false 时，"+
						"耗尽写回会把 JSON 错误体塞进已开的 SSE 流（生产实证失效形态）。")
				f2bLockedCallSites++
			}

			// 递归进入嵌套块。
			switch inner := st.(type) {
			case *ast.IfStmt:
				checkStmts(inner.Body.List)
				if inner.Else != nil {
					if eb, ok := inner.Else.(*ast.BlockStmt); ok {
						checkStmts(eb.List)
					}
				}
			case *ast.ForStmt:
				checkStmts(inner.Body.List)
			case *ast.RangeStmt:
				checkStmts(inner.Body.List)
			case *ast.SwitchStmt:
				for _, cc := range inner.Body.List {
					if cb, ok := cc.(*ast.CaseClause); ok {
						checkStmts(cb.Body)
					}
				}
			case *ast.BlockStmt:
				checkStmts(inner.List)
			}
		}
	}

	checkStmts(chatBody.List)

	require.GreaterOrEqual(t, f2bLockedCallSites, 1,
		"必须至少找到一处传入 streamStarted 变量的 handleFailoverExhausted 调用点"+
			"（生产文件结构变化时须同步更新本用例）")
}

// f2bFailoverCallWithStreamStartedVar 判断语句 stmt 是否是
// handleFailoverExhausted(c, ..., streamStarted) 调用，且第三个实参是 streamStarted 变量。
// 传字面量 true 的调用点不依赖 streamStarted 取值，不在锁定范围。
func f2bFailoverCallWithStreamStartedVar(stmt ast.Stmt) *ast.CallExpr {
	var callExpr *ast.CallExpr
	switch st := stmt.(type) {
	case *ast.ExprStmt:
		callExpr, _ = st.X.(*ast.CallExpr)
	case *ast.ReturnStmt:
		if len(st.Results) == 1 {
			callExpr, _ = st.Results[0].(*ast.CallExpr)
		}
	}
	if callExpr == nil {
		return nil
	}
	sel, ok := callExpr.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "handleFailoverExhausted" {
		return nil
	}
	if len(callExpr.Args) < 3 {
		return nil
	}
	id, ok := callExpr.Args[2].(*ast.Ident)
	if !ok || id.Name != "streamStarted" {
		return nil
	}
	return callExpr
}

// f2bStmtCallsSyncHelper 判断语句 stmt 是否调用了 syncStreamStartedIfResponseCommitted，
// 且实参包含 &streamStarted。
func f2bStmtCallsSyncHelper(stmt ast.Stmt) bool {
	found := false
	ast.Inspect(stmt, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok || ident.Name != "syncStreamStartedIfResponseCommitted" {
			return true
		}
		for _, arg := range call.Args {
			unary, ok := arg.(*ast.UnaryExpr)
			if !ok || unary.Op != token.AND {
				continue
			}
			if id, ok := unary.X.(*ast.Ident); ok && id.Name == "streamStarted" {
				found = true
			}
		}
		return true
	})
	return found
}
