package service

// 本文件是 **契约 0.1：Cursor 文本桥接入口**（P1-A2 交付，P1-C1/C2 直接调用）。
//
// 固定桥接契约（跨卡接口，消费方不得更改签名）：
//
//	func CursorForwardInbound(ctx context.Context, c *gin.Context, account *Account,
//	                          inbound string, body []byte) (*ForwardResult, error)
//
//   - `inbound` 取值：`"openai_chat"`（Chat Completions JSON）。C1/C2 先用既有
//     apicompat 转换器把入站体转成 Chat Completions 再调用本入口。
//   - 实现内部复用 OpenAIGatewayService **既注入**的 cursor forwarder / 鉴权依赖
//     （包级安装模式，与 codeBuddyQuotaInstance 同构：InstallCursorBridge +
//     包级实例）；未安装或依赖缺失 → 明确错误，失败关闭。
//   - 返回 `*ForwardResult` 沿既有 usage 字段承载**真实** usage，由调用方 handler
//     既有 RecordUsage 链计费一次；**桥内不得二次计费**（本文件只做分派与字段搬运）。
//
// 为什么是包级函数而不是结构体方法：C1/C2 所在入口拿不到 *OpenAIGatewayService 句柄
// （v2 修正——c1/c2 BLOCKED 实证后改为包级入口），包级入口把"取哪个网关实例"收敛到
// 本文件内部，跨卡只依赖这一个函数签名。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// cursorInboundOpenAIChat 是唯一已取证的入站形状：Chat Completions JSON。
const cursorInboundOpenAIChat = "openai_chat"

// ErrCursorBridgeInboundUnsupported 表示入站形状不在桥接受理范围内（失败关闭）。
//
// 绝不"猜形状"直接转发：把未转换的 Responses / Messages 形状发给 Cursor 原生端点
// 必然被拒，且白耗 shareai 额度。
var ErrCursorBridgeInboundUnsupported = errors.New("cursor: unsupported inbound shape for text bridge (fail closed)")

// ErrCursorBridgeNotInstalled 表示桥接实例尚未安装（进程未装配 OpenAIGatewayService）。
var ErrCursorBridgeNotInstalled = errors.New("cursor: text bridge instance is not installed (fail closed)")

var (
	cursorBridgeMu       sync.RWMutex
	cursorBridgeInstance *OpenAIGatewayService
)

// InstallCursorBridge 安装桥接所用的网关实例。
//
// 生产由 NewOpenAIGatewayService 在构造末尾自安装（与 codeBuddyQuotaInstance 同一
// 包级安装模式，避免改动构造签名波及大量既有装配与测试）；测试可显式安装替身。
// s 为 nil 时清空实例（未安装态一律失败关闭）。
func InstallCursorBridge(s *OpenAIGatewayService) {
	cursorBridgeMu.Lock()
	defer cursorBridgeMu.Unlock()
	cursorBridgeInstance = s
}

// cursorBridgeInstalled 取当前安装的网关实例（nil = 未安装）。
func cursorBridgeInstalled() *OpenAIGatewayService {
	cursorBridgeMu.RLock()
	defer cursorBridgeMu.RUnlock()
	return cursorBridgeInstance
}

// CursorForwardInbound 是 Cursor 文本桥接的**唯一对外入口**（契约 0.1）。
//
// 参数：
//   - ctx / c：请求上下文（c 用于 ops 事件与回写客户端响应）；
//   - account：**选中账号**（出站 Bearer 与设备级材料均按该账号解析，绝不跨账号复用）；
//   - inbound：入站形状标识，当前仅受理 "openai_chat"；
//   - body：已归一为 Chat Completions 形状的请求体。
//
// 返回 `*ForwardResult`（usage 为真实用量），由调用方 handler 计费一次。
// 任何一步失败都返回错误，绝不降级重试、绝不伪造 usage。
func CursorForwardInbound(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	inbound string,
	body []byte,
) (*ForwardResult, error) {
	if !isCursorAccount(account) {
		return nil, errors.New("cursor: CursorForwardInbound requires a cursor platform account")
	}
	if inbound != cursorInboundOpenAIChat {
		return nil, fmt.Errorf("%w: inbound %q", ErrCursorBridgeInboundUnsupported, inbound)
	}
	svc := cursorBridgeInstalled()
	if svc == nil {
		return nil, fmt.Errorf("%w: inbound %q", ErrCursorBridgeNotInstalled, inbound)
	}
	view := newOpenAIRequestView(body)
	if strings.TrimSpace(view.Model) == "" {
		return nil, errors.New("cursor: inbound chat request carries no model (fail closed)")
	}
	return svc.forwardCursor(ctx, c, account, body, view.Model, view.Stream, time.Now())
}

// cursorForwardResultToOpenAI 把契约 0.1 的转发结果搬进 OpenAI 网关的结果形状。
//
// 只做字段搬运：Usage 逐字段映射（真实用量，不折算、不补默认值），模型/流式/耗时/
// 首字/图片/搜索等诊断字段同名直搬。**绝不在此计费**——OpenAI 两入口的既有
// RecordUsage 链各计一次，桥内无第二次写入。
func cursorForwardResultToOpenAI(r *ForwardResult) *OpenAIForwardResult {
	if r == nil {
		return nil
	}
	out := &OpenAIForwardResult{
		// cursor 上游只有"响应 id"这一个标识（无独立 request id），沿用既有口径
		// 落在 ResponseID 上，避免把响应标识误当成请求标识回填 RequestID。
		ResponseID:       r.RequestID,
		UpstreamHeaders:  r.UpstreamHeaders,
		Usage:            cursorClaudeUsageToOpenAI(r.Usage),
		Model:            r.Model,
		UpstreamModel:    r.UpstreamModel,
		Stream:           r.Stream,
		Duration:         r.Duration,
		FirstTokenMs:     r.FirstTokenMs,
		ClientDisconnect: r.ClientDisconnect,
		ImageCount:       r.ImageCount,
		ImageOutputSizes: r.ImageOutputSizes,
		SearchCount:      r.SearchCount,
		AudioUsage:       r.AudioUsage,
	}
	if r.UpstreamHeaders != nil {
		out.ResponseHeaders = r.UpstreamHeaders.Clone()
	}
	return out
}

// cursorClaudeUsageToOpenAI 把桥接 usage 逐字段映射回 OpenAI usage（真实用量，零折算）。
func cursorClaudeUsageToOpenAI(u ClaudeUsage) OpenAIUsage {
	return OpenAIUsage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		ImageOutputTokens:        u.ImageOutputTokens,
	}
}
