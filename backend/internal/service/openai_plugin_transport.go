package service

import (
	"context"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func (s *OpenAIGatewayService) SetPluginManager(manager *PluginManager) {
	s.pluginManager = manager
}

// doOpenAIUpstream 只在 OpenAI OAuth 能力绑定已启用时把真实请求交给插件。
// 插件返回标准 http.Response，响应解析、错误映射、SSE 和计费仍由现有核心链处理。
//
// 该版本为 CN 聊天转发主路径（chat_completions 含 cc_pipeline、messages、
// responses、anthropic_native 及 web_* 网页转发）提供 60s 首包超时 watchdog：
// CN 清单平台账号出站加超时，超时错误经消费链映射为 UpstreamFailoverError
// （NextAccountRetry + 冷却，带模型）。非转发辅助路径（embeddings、alpha_search
// 等）必须使用 doOpenAIUpstreamNoWatchdog 恢复无 watchdog 的既有行为（闸②整改）。
//
// clientCtx 是原始客户端 context（含取消信号）。调用方经 detachUpstreamContext/
// detachStreamUpstreamContext 建出的 request.Context() 已剥离客户端取消信号，
// watchdog 必须以独立的 clientCtx 参与取消裁决——客户端已断开时不得误记 504
// 冷却（闸②整改，见 newOpenAICNFirstByteWatchdog）。
func (s *OpenAIGatewayService) doOpenAIUpstream(clientCtx context.Context, request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	// CN 清单平台（派发单 1 同一谓词）：出站加 60s 首包超时。独立 context 层
	// （WithCancel + 定时器），到期取消当前上游尝试；body 首字节到达后流式
	// 传输不受影响。非 CN 路径（OpenAI/anthropic/gemini/codebuddy/other）不加。
	if !isCNFamilyFastpathAccount(account) {
		return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
	}
	if clientCtx == nil {
		clientCtx = request.Context()
	}
	// wctx 必须从 request.Context()（携带构建器写入的 HTTPUpstreamProfileOpenAI
	// 等连接池/协议/代理值）派生，WithContext 不会丢失 profile；clientCtx 只作为
	// 独立取消裁决信号传入 watchdog，不得整体装到 request 上（闸②终审整改项 2）。
	watchdog, wctx := newOpenAICNFirstByteWatchdog(request.Context(), clientCtx, s.openAICNFirstByteTimeoutFor())
	req := request.WithContext(wctx)
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		// 错误分类只读 watchdog 原子终态 decided，顺序上先看 Fired()：
		// 60s 定时器已 CAS 成 timedOut（唯一胜者）并取消请求后，客户端可能在
		// Do 返回前随后取消（parentDone() 为真）。此前后者优先会把已裁定的
		// 超时覆盖为底层取消错误，504 冷却与换号随之丢失。已胜出的超时先行
		// 处理，客户端后到不覆盖。
		if watchdog.Fired() {
			// 内部首包超时先于 Do 完成（上游连响应头都没给）：返回网关内部
			// 超时错误，与客户端取消（context.Canceled）严格区分。
			return nil, errOpenAICNFirstByteTimeout
		}
		// 客户端取消（decided 已是 settled 非超时终态）：按取消处理，绝不因
		// 随后到达的定时器触发而误判为内部超时（不冷却不换号）。
		if watchdog.parentDone() {
			watchdog.release()
			return resp, err
		}
		watchdog.release()
		return resp, err
	}
	resp.Body = &openAICNFirstByteTimeoutBody{ReadCloser: resp.Body, w: watchdog}
	return resp, nil
}

// doOpenAIUpstreamWithGuard 是 doOpenAIUpstream 的护栏收口包装（CF524 D1 + D2 + G5，
// 仅 OpenAI OAuth 家族 8 个上游执行点经此出网；handler 安装点属 P2，本卡只交付消费端）。
//
// 收口语义严格同源主方案 cf524ExecuteUpstreamWithGuard：
//   - 无快照 → 原样透传内层 doOpenAIUpstream（零行为变化，这是无兜底契约：未安装快照的
//     调用方——embeddings/count_tokens/alpha_search/测试桩——不在本方案覆盖面，护栏零介入）。
//   - 有快照：窗口耗尽前不发送新尝试的硬边界——剩余 < 最小可行窗口(5s) 直接返回
//     newUpstreamFirstByteTimeoutError（不向上游发请求），交由既有 FailoverExhausted 路径。
//     否则以 AttemptWindow 构建护栏，护栏 reqCtx 写入上游请求 context：窗口耗尽取消 reqCtx
//     中断 header 等待；body 读取不在 deadline 内（护栏取消边界仅覆盖响应头等待）。
//   - 心跳 owner 存在（仅流式安装）时 Resume（失败仅记 warn，零语义影响——心跳是可选
//     优化，不终止请求）；三分支与主方案逐条对应：护栏超时 → 停表+心跳 stop-and-wait →
//     返回护栏超时错误；传输错误 → 停表+停拍上抛；响应头到达 → 停表+心跳 OnUpstreamHeaderArrived
//     +Stop（晚到契约三分支顺序）→ 返回 resp。
//   - G5 观测单点在本函数内发射（obs.ctx 必须是未包护栏 reqCtx 的原始 clientCtx，否则护栏
//     超时被误判 client_gone）；tracker 存 gin context（c.Set）。
//
// 命名返回值（踩坑清单 #4）：调用方 defer 读 err 安全。CN watchdog 在内层实现内部零改动；
// 护栏只在自己 fired 时介入（#14）：watchdog 先 fired → 内层返回既有内部超时分类，本层
// guard.TimedOut()=false 不介入。
func (s *OpenAIGatewayService) doOpenAIUpstreamWithGuard(c *gin.Context, clientCtx context.Context, request *http.Request, proxyURL string, account *Account) (resp *http.Response, err error) {
	// G2a 收口 1：从请求 context 取预算快照与心跳 owner（消费端只读，禁止重建）。
	snapshot, hasSnapshot := RequestBudgetSnapshotFromContext(clientCtx)
	hb, _ := UpstreamHeartbeatFromContext(clientCtx)
	// 无快照即原样透传内层实现（未安装快照的调用方零行为变化，无兜底分支）。
	if !hasSnapshot {
		return s.doOpenAIUpstream(clientCtx, request, proxyURL, account)
	}
	// D1 最小可行窗口：剩余预算 < 5s 视为预算耗尽，不发请求直接返回护栏超时错误，
	// 交由 handler 既有 FailoverExhausted 路径（ShouldRetryNextAccount()=false）。
	remaining := snapshot.RemainingBudget(time.Now())
	if remaining < upstreamFirstByteMinViableWindow {
		return nil, newUpstreamFirstByteTimeoutError(remaining)
	}
	// 以不可变截止时间取剩余窗口构建护栏（换号不重置）；护栏 reqCtx 写入上游请求
	// context，窗口耗尽取消 reqCtx 中断 header 等待。
	window := snapshot.AttemptWindow(time.Now())
	guard, upstreamCtx := newUpstreamFirstByteGuard(clientCtx, window)

	// G5 观测单点载体：ctx 必须是原始 clientCtx（未包护栏 reqCtx），避免护栏取消误判
	// client_gone；c 用于 tracker 落 gin context 域。stream 以 hb owner 存在作为流式代理
	// （owner 仅 clientStream 且 hb>0 时安装）。
	obs := &cf524GuardObservation{
		c:         c,
		ctx:       clientCtx,
		platform:  account.Platform,
		stream:    hb != nil,
		attempt:   1,
		startedAt: time.Now(),
		snapshot:  snapshot,
	}

	// D2 心跳 Resume（owner 存在且护栏有效时；流式由 owner 安装契约保证）。失败仅记
	// warn 日志（path/request_id/account_id），继续既有流程，零语义变化。
	if hb != nil && guard != nil {
		if resumeErr := hb.Resume(); resumeErr != nil {
			requestID, _ := request.Context().Value(ctxkey.RequestID).(string)
			logger.FromContext(request.Context()).Warn("gateway.cf524_heartbeat_resume_failed",
				zap.String("path", request.URL.Path),
				zap.String("request_id", requestID),
				zap.Int64("account_id", account.ID),
				zap.Error(resumeErr),
			)
		}
	}

	// 护栏 reqCtx 写入上游请求 context：非 CN 路径 Do(request) 直接继承该 ctx 取消；
	// CN 路径内层 watchdog 自 request.Context()(=upstreamCtx) 派生 wctx，guard 取消同样
	// 抵达 Do。clientCtx（原始）仍作为内层 watchdog 的取消裁决信号，不受 guard 取消影响。
	guardedReq := request.WithContext(upstreamCtx)
	resp, err = s.doOpenAIUpstream(clientCtx, guardedReq, proxyURL, account)

	// 三分支（对照 gateway_forward.go:990-1035 逐条同型）。
	if guard != nil && guard.TimedOut() {
		// 状态机优先级②（D2）：guard 终态先于一切 → 心跳 OnGuardDecided + stop-and-wait
		// 后返回 failover 错误，绝不先于心跳停等就换号。
		if hb != nil {
			hb.OnGuardDecided()
			hb.Stop()
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		obs.noteHeartbeatIfCommitted(hb)
		obs.observeGuardTriggered()
		return nil, newUpstreamFirstByteTimeoutError(snapshot.RemainingBudget(time.Now()))
	}
	if err != nil {
		// 真实传输错误：停表（零取消收尾）+ 心跳 stop-and-wait，避免泄漏 goroutine，随后上抛。
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if guard != nil {
			guard.Stop()
		}
		if hb != nil {
			hb.Stop()
		}
		obs.noteHeartbeatIfCommitted(hb)
		return nil, err
	}
	// 响应头到达（未超时）：护栏停表零取消；心跳按晚到契约裁决（Rejected/AfterCommit），
	// 单一 writer 所有权：交还 writer 前必须 stop-and-wait。
	if guard != nil {
		guard.Stop()
	}
	if hb != nil {
		hb.OnUpstreamHeaderArrived()
		hb.Stop()
	}
	obs.noteHeartbeatIfCommitted(hb)
	obs.markUpstreamHeadersReceived()
	return resp, nil
}

// doOpenAIUpstreamNoWatchdog 是无首包超时的出站转发版本，供非转发辅助路径
// （embeddings、alpha_search、count_tokens、live、images、grok、codebuddy、
// ws_http_bridge、passthrough 等）使用。这些路径原本无首包超时行为（闸②整改：
// 注入点收窄——只保留主路径的 watchdog），且其账号多为非 CN 平台；即使账号
// 命中 CN 谓词也不加 watchdog，恢复派发单 1 之前的既有行为。
func (s *OpenAIGatewayService) doOpenAIUpstreamNoWatchdog(request *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}

// doOpenAIAccountTestUpstream 让 OpenAI OAuth 账号测试与真实转发使用同一插件路径。
// API Key 和未命中插件的账号保持各自原有的 HTTPUpstream 行为。
func (s *AccountTestService) doOpenAIAccountTestUpstream(
	request *http.Request,
	proxyURL string,
	account *Account,
	useTLSFallback bool,
) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	if useTLSFallback {
		return s.httpUpstream.DoWithTLS(
			request,
			proxyURL,
			account.ID,
			account.Concurrency,
			s.tlsFPProfileService.ResolveTLSProfile(account),
		)
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, account.Concurrency)
}
