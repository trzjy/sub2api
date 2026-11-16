package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Devin（Cascade）反代转发层（派发单 C4，方案 plan.md §3.4/§4）。
//
// 这是 devin 平台转发的唯一入口 forwardDevin：入站 OpenAI Chat Completions body
// → BuildDevinChatParams（C3）→ NewDevinChatFrame（C2，protobuf+gzip+Connect 帧）
// → 上游 GetChatMessage（Connect RPC 流式）→ 回程解析为 OpenAI chat.completion /
// chat.completion.chunk，经 web_protocol_bridge 回桥到入站协议。
//
// 流式读侧复用 newWebStreamGuard（select 四路：数据 / 间隔超时 / keepalive / ctx 取消），
// 与 handleWebKimiStreamingResponse 同构；区别在于：
//   - 帧来自 DevinStreamReader.Next()（二进制 Connect 帧，非 Connect JSON envelope）；
//   - END_STREAM 帧携带 trailer JSON，error.code 非空按流中段错误处理；
//   - 数据帧 decodeGetChatMessageResponse，正文/思考/工具调用/usage 分路处理。
//
// 空回答闸门：上游 200 但无正文/思考/工具调用视为失败，绝不当成功（禁区要求）。

const (
	// devinAuthTimeout 是换取 user JWT 的独立超时（方案 §3.4：30s）。
	devinAuthTimeout = 30 * time.Second
	// devinClientUA 对齐 connect-go 默认 User-Agent（派发单步骤 6）。
	devinClientUA = "connect-go/1.18.1 (go1.26.3)"
)

// forwardDevin 是 devin 平台转发的唯一入口。
func (s *OpenAIGatewayService) forwardDevin(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	reqStream bool,
	startTime time.Time,
	mode webResponseMode,
) (*OpenAIForwardResult, error) {
	if account == nil {
		return nil, errors.New("forwardDevin requires a non-nil account")
	}

	// 1. 凭证（失败关闭）。
	token := strings.TrimSpace(account.GetDevinToken())
	if token == "" {
		return nil, errors.New("devin account is missing api_key credential")
	}

	// 剥离 devin 不支持的 JSON Schema 关键字（propertyNames / prefixItems）。
	// 三个入口（chat / responses / messages）最终都汇合到这里，一次清洗即可全覆盖。
	body = stripDevinUnsupportedToolSchemaKeywords(body)

	// 2. 模型映射：空回退原名；设置 ops 观测的上游模型。
	upstreamModel := account.GetMappedModel(originalModel)
	if strings.TrimSpace(upstreamModel) == "" {
		upstreamModel = originalModel
	}
	SetOpsUpstreamModel(c, upstreamModel)

	// 3. 代理（与 web 适配器同写法）。
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	// 4. 构造 chat 参数（C3），cascadeID 由 C3 生成（newDevinCascadeID）。
	// BuildDevinChatParams 在 cascadeID 为空时自行生成；这里显式传入以便
	// 请求与响应使用同一会话 ID，且失败关闭（随机数不可用时报错）。
	cascadeID, err := newDevinCascadeID()
	if err != nil {
		return nil, err
	}
	params, err := BuildDevinChatParams(body, upstreamModel, cascadeID)
	if err != nil {
		return nil, err
	}
	// chat 请求 Metadata 需要 session token（F1 已补 APIKey 字段），必须设置。
	params.APIKey = token

	// 5. 鉴权：GetUserJwt 30s 超时；base 取 CustomAPIServerURL 否则默认。
	authCtx, cancelAuth := context.WithTimeout(ctx, devinAuthTimeout)
	defer cancelAuth()
	authResp, err := NewDevinUpstreamClient().GetUserJwt(authCtx, token)
	if err != nil {
		return nil, fmt.Errorf("devin get user jwt: %w", err)
	}
	base := strings.TrimSpace(authResp.CustomAPIServerURL)
	if base == "" {
		base = DevinAPIURL
	}

	// 6. 构造 Connect 帧与上游请求。
	frame, chatBaseURL, err := NewDevinChatFrame(params, authResp.UserJWT, base)
	if err != nil {
		return nil, err
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()
	req, err := http.NewRequestWithContext(
		upstreamCtx,
		http.MethodPost,
		strings.TrimRight(chatBaseURL, "/")+DevinChatMessagePath,
		bytes.NewReader(frame),
	)
	if err != nil {
		return nil, fmt.Errorf("devin chat create request: %w", err)
	}
	req = req.WithContext(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI))
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Connect-Content-Encoding", "gzip")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Connect-Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", devinClientUA)

	resp, err := s.doOpenAIUpstream(ctx, req, proxyURL, account)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(startTime).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false, upstreamModel)
	}
	defer func() { _ = resp.Body.Close() }()

	// 7. 非 200：走既有错误响应路径（失败关闭）。
	if resp.StatusCode != http.StatusOK {
		respBody := s.readUpstreamErrorBody(resp)
		return s.handleErrorResponse(ctx, resp, c, account, respBody, upstreamModel)
	}

	// 8/9. 流式 / 非流式回程。
	includeUsage := gjson.GetBytes(body, "stream_options.include_usage").Bool()
	if reqStream {
		return s.handleDevinStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel, startTime, mode, includeUsage)
	}
	return s.handleDevinNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel, startTime, mode)
}

// stripDevinUnsupportedToolSchemaKeywords 剥离 devin 上游不支持的 JSON Schema
// 关键字：propertyNames 直接删除；prefixItems 在缺少可用 items 时先取数组中
// 评分最高的元素补成 items，随后无论是否补齐都删除 prefixItems。
//
// 解析失败、无 tools 键、tools 非数组、或没有任何改动时原样返回 body（字节不变）。
// 注意：这里不做 type 大小写规范化，缺失时补的 items 使用小写 "string"。
func stripDevinUnsupportedToolSchemaKeywords(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var payload map[string]any
	// UseNumber 让数字按原文保留（json.Number），避免整份 body 重序列化时
	// 大整数经 float64 丢精度（如 9007199254740993 被改写成 9007199254740992）。
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&payload); err != nil {
		return body
	}
	tools, ok := payload["tools"].([]any)
	if !ok {
		return body
	}

	changed := false
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		params, ok := fn["parameters"]
		if !ok {
			continue
		}
		stripDevinSchemaNode(params, &changed)
	}

	if !changed {
		return body
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return normalized
}

// stripDevinSchemaNode 递归清理单个 schema 节点（原地修改 map/slice），
// 通过 changed 汇总是否发生过改动。
//
// 只下钻 JSON Schema 的 schema 承载位置（properties/patternProperties/$defs/
// definitions/dependentSchemas 的 map 值，additionalProperties/items/contains/
// if/then/else/not/unevaluatedItems/unevaluatedProperties 单值，以及
// anyOf/oneOf/allOf/prefixItems 数组元素），绝不进入 enum/const/default/examples
// 等数据字段；任何承载位置命中 propertyNames 一律删除。
func stripDevinSchemaNode(node any, changed *bool) {
	switch v := node.(type) {
	case map[string]any:
		// prefixItems：先递归各元素清理其内嵌脏关键字，再按既有逻辑折叠为 items
		// 并删除。回退的 items 使用小写 "string"，不做 type 大小写规范化。
		if rawPrefix, exists := v["prefixItems"]; exists {
			if prefixItems, ok := rawPrefix.([]any); ok {
				for _, elem := range prefixItems {
					stripDevinSchemaNode(elem, changed)
				}
				if len(prefixItems) > 0 {
					itemsValue, hasItems := v["items"]
					_, itemsIsBool := itemsValue.(bool)
					if !hasItems || itemsValue == nil || itemsIsBool {
						best := devinBestPrefixItem(prefixItems)
						if best == nil {
							best = map[string]any{"type": "string"}
						}
						v["items"] = best
					}
				}
			}
			delete(v, "prefixItems")
			*changed = true
		}

		// 任何 schema 承载位置命中 propertyNames 一律删除。
		if _, exists := v["propertyNames"]; exists {
			delete(v, "propertyNames")
			*changed = true
		}

		// map 承载位置：值逐个递归。
		for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
			if children, ok := v[key].(map[string]any); ok {
				for _, child := range children {
					stripDevinSchemaNode(child, changed)
				}
			}
		}

		// 单值承载位置；items 为数组形态时进入 []any 分支逐元素递归。
		for _, key := range []string{"additionalProperties", "items", "contains", "if", "then", "else", "not", "unevaluatedItems", "unevaluatedProperties"} {
			if child, ok := v[key]; ok {
				stripDevinSchemaNode(child, changed)
			}
		}

		// 数组承载位置。
		for _, key := range []string{"anyOf", "oneOf", "allOf"} {
			if branches, ok := v[key].([]any); ok {
				for _, branch := range branches {
					stripDevinSchemaNode(branch, changed)
				}
			}
		}
	case []any:
		for _, elem := range v {
			stripDevinSchemaNode(elem, changed)
		}
	}
}

// devinBestPrefixItem 从 prefixItems 候选中选出评分最高的第一个；全部 0 分
// 返回 nil（由调用方回退到小写 "string"）。
func devinBestPrefixItem(options []any) any {
	var best any
	bestScore := 0
	for _, option := range options {
		if score := devinSchemaOptionScore(option); score > bestScore {
			bestScore = score
			best = option
		}
	}
	return best
}

// devinSchemaOptionScore 评分：含 properties 或 type=="object" → 3；含 items
// 或 type=="array" → 2；含 enum/const 或非空标量 type → 1；否则 0。
func devinSchemaOptionScore(val any) int {
	m, ok := val.(map[string]any)
	if !ok {
		return 0
	}
	typeStr, _ := m["type"].(string)
	if _, hasProps := m["properties"]; hasProps || typeStr == "object" {
		return 3
	}
	if _, hasItems := m["items"]; hasItems || typeStr == "array" {
		return 2
	}
	if _, hasEnum := m["enum"]; hasEnum {
		return 1
	}
	if _, hasConst := m["const"]; hasConst {
		return 1
	}
	if typeStr != "" && typeStr != "null" {
		return 1
	}
	return 0
}

// devinChunkEnvelope 以 OpenAI chat.completion.chunk 形状构造一帧回客户端包络。
// delta 由调用方组装（content / reasoning_content / tool_calls 可任意组合），终帧
// finish_reason 非空；usage 仅在需要时（include_usage）附在独立 usage 帧。
// 抽成纯函数便于单测（派发单测试要求）。
func devinChunkEnvelope(responseID, originalModel string, delta gin.H, finishReason string, usage *OpenAIUsage) gin.H {
	d := make(gin.H, len(delta)+0)
	for k, v := range delta {
		d[k] = v
	}
	choice := gin.H{"index": 0, "delta": d, "finish_reason": finishReason}
	chunk := gin.H{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   originalModel,
		"choices": []gin.H{choice},
	}
	if usage != nil {
		chunk["usage"] = usage
	}
	return chunk
}

// devinUsageFrame 构造独立的 usage 帧（choices 为空，仅携带 usage），对齐
// OpenAI stream_options.include_usage 的末帧语义。
func devinUsageFrame(responseID, originalModel string, usage *OpenAIUsage) gin.H {
	return gin.H{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   originalModel,
		"choices": []gin.H{},
		"usage":   usage,
	}
}

// devinShouldEmitUsage 判定流式收尾是否写独立 usage 帧：入站 stream_options.include_usage
// 仅对 chat 模式生效；responses / anthropic 回桥需要 usage 填充，故无条件回传一帧。
func devinShouldEmitUsage(mode webResponseMode, includeUsage bool) bool {
	return includeUsage || mode != webResponseModeChat
}

// devinNonStreamMessage 构造非流式 message 的 content / reasoning_content：使用未 Trim
// 的原文以保留上游空白（缩进、换行），仅用 Trim 后的副本判断是否有实际内容。
func devinNonStreamMessage(text, reasoning string) gin.H {
	message := gin.H{"role": "assistant", "content": text}
	if strings.TrimSpace(reasoning) != "" {
		message["reasoning_content"] = reasoning
	}
	return message
}

// devinUsageFromStats 把上游 ModelUsageStats 归一为 ChatUsage（C3），再由
// DevinUsageToOpenAI 转成 OpenAIUsage。nil 输入返回全零。抽成纯函数便于单测。
func devinUsageFromStats(s *ModelUsageStats) ChatUsage {
	if s == nil {
		return ChatUsage{}
	}
	return ChatUsage{
		Input:      s.InputTokens,
		Output:     s.OutputTokens,
		CacheWrite: s.CacheWriteTokens,
		CacheRead:  s.CacheReadTokens,
	}
}

// devinUsageStatsToOpenAI 是 ModelUsageStats → OpenAIUsage 的便捷转换。
func devinUsageStatsToOpenAI(s *ModelUsageStats) OpenAIUsage {
	chatUsage := devinUsageFromStats(s)
	return DevinUsageToOpenAI(&chatUsage)
}

// devinTrailerError 解析 END_STREAM trailer JSON 的 error.code / error.message。
// code 支持数字（非 0）与字符串（非空）两种形态；无有效 code 视为正常 trailer。
func devinTrailerError(payload []byte) (code, message string) {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return "", ""
	}
	c := gjson.GetBytes(payload, "error.code")
	switch c.Type {
	case gjson.Number:
		if c.Int() != 0 {
			code = c.String()
		}
	case gjson.String:
		code = strings.TrimSpace(c.String())
	}
	message = strings.TrimSpace(gjson.GetBytes(payload, "error.message").String())
	return code, message
}

// devinReadResultFrame 给 DevinStreamReader.Next() 的每帧加 1 字节前缀，区分
// 普通数据帧（0x00）与 END_STREAM trailer 帧（0x01）。webStreamGuard 只投递
// []byte，故此把 endStream 标志编进 payload，主循环再据首字节分路。
func devinReadResultFrame(payload []byte, endStream bool) []byte {
	out := make([]byte, 1+len(payload))
	if endStream {
		out[0] = 0x01
	}
	copy(out[1:], payload)
	return out
}

// devinAssembledToolCall 聚合非流式工具调用的完整形态（完整参数，按 index 排序）。
type devinAssembledToolCall struct {
	id    string
	name  string
	args  strings.Builder
	index int
}

// appendDevinToolDeltas 把 ToolCallAccumulator 的增量并入完整调用列表（非流式用）。
func appendDevinToolDeltas(calls *[]*devinAssembledToolCall, deltas []ChatToolCallDelta) {
	for _, d := range deltas {
		for len(*calls) <= d.Index {
			*calls = append(*calls, &devinAssembledToolCall{index: len(*calls)})
		}
		call := (*calls)[d.Index]
		if d.ID != "" {
			call.id = d.ID
		}
		if d.Name != "" {
			call.name = d.Name
		}
		_, _ = call.args.WriteString(d.ArgumentsJSON)
	}
}

// devinStreamingToolDelta 把工具调用增量转成 OpenAI delta.tool_calls 数组。
// 首帧带 id/type/name，后续只带 arguments，index 稳定（派发单步骤 8）。
func devinStreamingToolDelta(deltas []ChatToolCallDelta, headerSent map[int]bool) []gin.H {
	out := make([]gin.H, 0, len(deltas))
	for _, d := range deltas {
		fn := gin.H{"arguments": d.ArgumentsJSON}
		item := gin.H{"index": d.Index, "type": "function", "function": fn}
		if !headerSent[d.Index] {
			item["id"] = d.ID
			fn["name"] = d.Name
			headerSent[d.Index] = true
		}
		out = append(out, item)
	}
	return out
}

// handleDevinStreamingResponse 流式回程：DevinStreamReader 逐帧读取 → 解析为
// chat.completion.chunk（正文 / 思考 / 工具调用 / usage）→ writeWebStreamChunk 回写。
// 读侧四路 select、keepalive、midstream error 收口、客户端断开排水均与 web 模板同构。
func (s *OpenAIGatewayService) handleDevinStreamingResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	originalModel string,
	upstreamModel string,
	startTime time.Time,
	mode webResponseMode,
	includeUsage bool,
) (*OpenAIForwardResult, error) {
	if s.responseHeaderFilter != nil {
		responseheaders.WriteFilteredHeaders(c.Writer.Header(), resp.Header, s.responseHeaderFilter)
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	responseID := ""
	var usage *OpenAIUsage
	var textAggregated strings.Builder
	written := false
	hasContent := false
	lastStopReason := int32(StopReasonUnspecified)
	midstreamErr := false
	midstreamErrMsg := ""
	st := newWebClientStreamState(mode, originalModel)

	toolAcc := &ToolCallAccumulator{}
	toolHeaderSent := make(map[int]bool)

	reader := NewDevinStreamReader(resp.Body)
	guardParams := s.webStreamGuardParams(func() ([]byte, error) {
		payload, endStream, err := reader.Next()
		if err != nil {
			return nil, err // io.EOF 原样上抛，守卫视作流结束
		}
		return devinReadResultFrame(payload, endStream), nil
	})
	guard := newWebStreamGuard(guardParams)
	defer guard.stop()

	lastDataAt := time.Now()
	clientDisconnected := false
	ctxDoneCh := ctx.Done()

streamLoop:
	for {
		select {
		case <-ctxDoneCh:
			// 客户端断开：不掐断上游读，继续排水采完 usage（计费需要）。
			clientDisconnected = true
			ctxDoneCh = nil

		case ev, ok := <-guard.events:
			if !ok {
				break streamLoop
			}
			if ev.err != nil {
				if isOpenAICNFirstByteTimeout(ev.err) && !written {
					return nil, s.failoverOpenAICNFirstByteTimeout(ctx, account, upstreamModel)
				}
				recordWebStreamReadError(c, account, upstreamModel, ev.err)
				return nil, fmt.Errorf("devin stream read: %w", ev.err)
			}
			lastDataAt = time.Now()
			guard.resetKeepalive()

			if len(ev.data) == 0 {
				continue
			}
			endStream := ev.data[0] == 0x01
			payload := ev.data[1:]
			if endStream {
				// END_STREAM trailer：error.code 非空 → 流中段错误。
				code, msg := devinTrailerError(payload)
				if code != "" {
					if !written {
						// 尚未写出任何客户端字节：走完整错误路径（失败关闭）。
						errResp := &http.Response{
							StatusCode: resp.StatusCode,
							Header:     resp.Header,
							Body:       io.NopCloser(bytes.NewReader(payload)),
						}
						return s.handleErrorResponse(ctx, errResp, c, account, payload, upstreamModel)
					}
					opsMsg := sanitizeUpstreamErrorMessage(msg)
					if opsMsg == "" {
						opsMsg = "devin upstream trailer error: " + code
					}
					appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
						Platform:           account.Platform,
						AccountID:          account.ID,
						AccountName:        account.Name,
						UpstreamStatusCode: resp.StatusCode,
						Kind:               "midstream_error",
						Message:            opsMsg,
					})
					midstreamErr = true
					midstreamErrMsg = opsMsg
					break streamLoop
				}
				// 正常 trailer：继续读下一帧。
				continue
			}

			msg, derr := decodeGetChatMessageResponse(payload)
			if derr != nil {
				return nil, fmt.Errorf("devin decode stream frame: %w", derr)
			}
			if msg.MessageID != "" && responseID == "" {
				responseID = msg.MessageID
			} else if msg.RequestID != "" && responseID == "" {
				responseID = msg.RequestID
			}
			if msg.Usage != nil {
				u := devinUsageStatsToOpenAI(msg.Usage)
				usage = &u // usage 记最后一个
			}
			if msg.StopReason != 0 {
				lastStopReason = msg.StopReason
			}

			// 工具调用增量。
			if len(msg.DeltaToolCalls) > 0 {
				tdeltas, terr := toolAcc.Add(msg.DeltaToolCalls)
				if terr != nil {
					return nil, terr
				}
				if len(tdeltas) > 0 {
					hasContent = true
					written = true
					if err := writeWebStreamChunk(c, st, devinChunkEnvelope(
						responseID, originalModel,
						gin.H{"tool_calls": devinStreamingToolDelta(tdeltas, toolHeaderSent)},
						"", nil,
					)); err != nil {
						clientDisconnected = true
						continue
					}
				}
			}

			// 思考增量 → reasoning_content。
			if msg.DeltaThinking != "" {
				hasContent = true
				written = true
				if err := writeWebStreamChunk(c, st, devinChunkEnvelope(
					responseID, originalModel,
					gin.H{"reasoning_content": msg.DeltaThinking}, "", nil,
				)); err != nil {
					clientDisconnected = true
					continue
				}
			}

			// 正文增量 → content。
			if msg.DeltaText != "" {
				hasContent = true
				written = true
				textAggregated.WriteString(msg.DeltaText)
				if err := writeWebStreamChunk(c, st, devinChunkEnvelope(
					responseID, originalModel,
					gin.H{"content": msg.DeltaText}, "", nil,
				)); err != nil {
					clientDisconnected = true
					continue
				}
			}

		case <-guard.intervalCh:
			if !guard.streamIdle(guardParams.interval) {
				continue
			}
			if clientDisconnected {
				return nil, fmt.Errorf("devin stream read: %w", errWebStreamIdleTimeout)
			}
			slog.Warn("devin stream data interval timeout",
				"account_id", account.ID,
				"model", originalModel,
				"upstream_model", upstreamModel,
				"interval", guardParams.interval.String())
			recordWebStreamReadError(c, account, upstreamModel, errWebStreamIdleTimeout)
			return nil, fmt.Errorf("devin stream read: %w", errWebStreamIdleTimeout)

		case <-guard.keepaliveCh:
			if clientDisconnected || !webStreamKeepalivePending(written, guardParams.keepalive, lastDataAt) {
				guard.resetKeepalive()
				continue
			}
			if err := writeWebStreamKeepalive(c); err != nil {
				clientDisconnected = true
				continue
			}
			lastDataAt = time.Now()
			guard.resetKeepalive()
		}
	}

	// 收口用量统一归一（nil → 全零）：midstream trailer 错误、客户端断开、终帧写失败
	// 三条收口路径都必须带上已采集的 Usage，故在此计算，供各收口共用（计费不得丢）。
	finalUsage := usage
	if finalUsage == nil {
		finalUsage = &OpenAIUsage{}
	}
	// committedResult 构造"响应已提交"的收口结果：让"客户端断开 / midstream 错误 /
	// 终帧写失败"三条路径计费口径一致，统一带 Usage 并按连接状态标注 ClientDisconnect。
	committedResult := func(disconnected bool) (*OpenAIForwardResult, error) {
		MarkResponseCommitted(c)
		return &OpenAIForwardResult{
			UpstreamHeaders:  resp.Header,
			ResponseID:       responseID,
			Usage:            *finalUsage,
			Model:            originalModel,
			UpstreamModel:    upstreamModel,
			UpstreamEndpoint: DevinChatMessagePath,
			Stream:           true,
			ResponseHeaders:  resp.Header.Clone(),
			Duration:         time.Since(startTime),
			ClientDisconnect: disconnected,
		}, nil
	}

	// 流中段错误收口：不伪造正常 finish_reason+usage 终止帧。
	if midstreamErr {
		if err := writeWebStreamMidstreamError(c, st, midstreamErrMsg); err != nil {
			return nil, err
		}
		return committedResult(clientDisconnected)
	}

	// 空回答闸门：上游 200 但无正文/思考/工具调用 → 失败关闭。
	if !hasContent {
		return nil, errors.New("devin upstream returned an empty response (no text/thinking/tool_calls)")
	}

	// 收尾：Finish 校验工具调用参数（零参补 {}），并把补充增量写出。
	finalToolDeltas, ferr := toolAcc.Finish()
	if ferr != nil {
		return nil, ferr
	}
	if !clientDisconnected && len(finalToolDeltas) > 0 {
		if err := writeWebStreamChunk(c, st, devinChunkEnvelope(
			responseID, originalModel,
			gin.H{"tool_calls": devinStreamingToolDelta(finalToolDeltas, toolHeaderSent)},
			"", nil,
		)); err != nil {
			clientDisconnected = true
		}
	}

	finishReason := DevinStopReasonToOpenAI(lastStopReason, len(toolHeaderSent) > 0)

	// 客户端断开：不再向下游写，但必须继续走完收口并返回带 usage 的结果（不得丢计费）。
	if clientDisconnected {
		return committedResult(true)
	}

	// 终帧 finish_reason + （可选）独立 usage 帧 + finalizeWebStream。
	if err := writeWebStreamChunk(c, st, devinChunkEnvelope(
		responseID, originalModel, gin.H{}, finishReason, nil,
	)); err != nil {
		return committedResult(true)
	}
	// include_usage 仅对 chat 生效；responses/anthropic 必须无条件回传 usage 帧供回桥填充。
	if devinShouldEmitUsage(mode, includeUsage) {
		if err := writeWebStreamChunk(c, st, devinUsageFrame(responseID, originalModel, finalUsage)); err != nil {
			return committedResult(true)
		}
	}
	if err := finalizeWebStream(c, st); err != nil {
		return committedResult(true)
	}
	MarkResponseCommitted(c)

	return &OpenAIForwardResult{
		UpstreamHeaders:  resp.Header,
		ResponseID:       responseID,
		Usage:            *finalUsage,
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		UpstreamEndpoint: DevinChatMessagePath,
		Stream:           true,
		ResponseHeaders:  resp.Header.Clone(),
		Duration:         time.Since(startTime),
	}, nil
}

// handleDevinNonStreamingResponse 非流式回程：读全 body → 逐帧聚合为单条
// chat.completion（content / reasoning_content / tool_calls / finish_reason / usage）
// → writeWebCompletion → 返回结果。
func (s *OpenAIGatewayService) handleDevinNonStreamingResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
	originalModel string,
	upstreamModel string,
	startTime time.Time,
	mode webResponseMode,
) (*OpenAIForwardResult, error) {
	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, openAITooLargeError)
	if err != nil {
		if isOpenAICNFirstByteTimeout(err) {
			return nil, s.failoverOpenAICNFirstByteTimeout(ctx, account, upstreamModel)
		}
		return nil, err
	}

	responseID := ""
	var usage *OpenAIUsage
	var textAggregated strings.Builder
	var reasoningAggregated strings.Builder
	hasContent := false
	lastStopReason := int32(StopReasonUnspecified)
	toolAcc := &ToolCallAccumulator{}
	var assembled []*devinAssembledToolCall

	reader := NewDevinStreamReader(bytes.NewReader(body))
	for {
		payload, endStream, rerr := reader.Next()
		if rerr != nil {
			return nil, fmt.Errorf("devin upstream response read: %w", rerr)
		}
		if endStream {
			if code, _ := devinTrailerError(payload); code != "" {
				errResp := &http.Response{
					StatusCode: resp.StatusCode,
					Header:     resp.Header,
					Body:       io.NopCloser(bytes.NewReader(payload)),
				}
				return s.handleErrorResponse(ctx, errResp, c, account, payload, upstreamModel)
			}
			break
		}
		msg, derr := decodeGetChatMessageResponse(payload)
		if derr != nil {
			return nil, fmt.Errorf("devin decode response frame: %w", derr)
		}
		if msg.MessageID != "" && responseID == "" {
			responseID = msg.MessageID
		} else if msg.RequestID != "" && responseID == "" {
			responseID = msg.RequestID
		}
		if msg.Usage != nil {
			u := devinUsageStatsToOpenAI(msg.Usage)
			usage = &u
		}
		if msg.StopReason != 0 {
			lastStopReason = msg.StopReason
		}
		if len(msg.DeltaToolCalls) > 0 {
			tdeltas, terr := toolAcc.Add(msg.DeltaToolCalls)
			if terr != nil {
				return nil, terr
			}
			if len(tdeltas) > 0 {
				hasContent = true
			}
			appendDevinToolDeltas(&assembled, tdeltas)
		}
		if msg.DeltaThinking != "" {
			hasContent = true
			reasoningAggregated.WriteString(msg.DeltaThinking)
		}
		if msg.DeltaText != "" {
			hasContent = true
			textAggregated.WriteString(msg.DeltaText)
		}
	}

	if !hasContent {
		return nil, errors.New("devin upstream returned an empty response (no text/thinking/tool_calls)")
	}

	finalToolDeltas, ferr := toolAcc.Finish()
	if ferr != nil {
		return nil, ferr
	}
	appendDevinToolDeltas(&assembled, finalToolDeltas)

	message := devinNonStreamMessage(textAggregated.String(), reasoningAggregated.String())
	if len(assembled) > 0 {
		toolCalls := make([]gin.H, 0, len(assembled))
		for _, call := range assembled {
			args := call.args.String()
			if args == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, gin.H{
				"id":   call.id,
				"type": "function",
				"function": gin.H{
					"name":      call.name,
					"arguments": args,
				},
			})
		}
		message["tool_calls"] = toolCalls
	}

	finalUsage := usage
	if finalUsage == nil {
		finalUsage = &OpenAIUsage{}
	}
	finishReason := DevinStopReasonToOpenAI(lastStopReason, len(assembled) > 0)
	completion := gin.H{
		"id":      responseID,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   originalModel,
		"choices": []gin.H{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": finalUsage,
	}
	if err := writeWebCompletion(c, newWebClientStreamState(mode, originalModel), completion); err != nil {
		return nil, err
	}
	MarkResponseCommitted(c)

	return &OpenAIForwardResult{
		UpstreamHeaders:  resp.Header,
		ResponseID:       responseID,
		Usage:            *finalUsage,
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		UpstreamEndpoint: DevinChatMessagePath,
		Stream:           false,
		ResponseHeaders:  resp.Header.Clone(),
		Duration:         time.Since(startTime),
	}, nil
}
