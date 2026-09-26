package service

import (
	"context"
	"net/http"
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
