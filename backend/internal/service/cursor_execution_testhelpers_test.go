package service

// 本文件提供 Cursor 测试共用的**唯一执行部件**构造助手（对应 §24.2-2 的直接 DI）。
//
// 背景：旧测试通过 Cursor 专有 setter / 包级安装往网关上挂依赖；这些符号已按
// §24.2-2 / §25.14-C 全部删除，测试改为构造一个真实的 CursorExecutionService 并
// 直接注入网关（与生产 wire 装配同构）。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

// newCursorTestExecutionService 构造测试用执行部件（依赖与生产同构，仅 transport 为替身）。
//
// 任一依赖为 nil 即测试失败：执行部件不接受"缺依赖时自建默认转发器"的回退。
func newCursorTestExecutionService(
	t *testing.T,
	quotaKeys QuotaKeyRepository,
	enc QuotaKeyEncryptor,
	deviceIDs CursorDeviceIDResolver,
	forwarder *CursorUpstreamForwarder,
) *CursorExecutionService {
	t.Helper()
	svc, err := NewCursorExecutionService(quotaKeys, enc, deviceIDs, forwarder)
	if err != nil {
		t.Fatalf("new cursor execution service: %v", err)
	}
	t.Cleanup(svc.Stop)
	return svc
}

// attachCursorTestExecution 构造执行部件并挂到网关上（替代旧的三组 setter）。
func attachCursorTestExecution(
	t *testing.T,
	gateway *OpenAIGatewayService,
	quotaKeys QuotaKeyRepository,
	enc QuotaKeyEncryptor,
	deviceIDs CursorDeviceIDResolver,
	forwarder *CursorUpstreamForwarder,
) *CursorExecutionService {
	t.Helper()
	svc := newCursorTestExecutionService(t, quotaKeys, enc, deviceIDs, forwarder)
	if gateway != nil {
		gateway.cursorExecution = svc
	}
	return svc
}

// executeCursorForTest 用原始 Chat Completions body 走**唯一执行入口**。
//
// 等价于旧 `OpenAIGatewayService.forwardCursor(ctx, c, account, body, model, stream, start)`
// 的调用形态（该方法的职责已按 §24.2-2 迁入执行部件）：这里只做一次解码与入口收敛。
func executeCursorForTest(
	t *testing.T,
	gateway *OpenAIGatewayService,
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	model string,
	stream bool,
	startTime time.Time,
) (*ForwardResult, error) {
	t.Helper()
	if gateway == nil || gateway.cursorExecution == nil {
		return nil, ErrCursorExecutionUnavailable
	}
	var ccReq apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &ccReq); err != nil {
		return nil, err
	}
	return gateway.cursorExecution.Execute(ctx, c, account, &CursorExecutionRequest{
		Chat:      &ccReq,
		Model:     model,
		Stream:    stream,
		StartTime: startTime,
		Protocol:  cursorProtocolChat,
	})
}
