//go:build unit

package service

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestClassifyUpstreamTransportError pins which transport-level upstream failures
// are "persistent" (retrying the same proxy/account is pointless — evict + alert)
// versus "transient" (a blip — fail over to a healthy account but do not evict).
//
// The motivating incident: a SOCKS5 proxy whose credentials expired returned
// `username/password authentication failed`, yet the account kept being scheduled.
func TestClassifyUpstreamTransportError(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		persistent bool
	}{
		// Durable — config/credential/routing problems. Retrying same proxy won't help.
		{"socks5 proxy credential rejected", errors.New(`Post "https://chatgpt.com/backend-api/codex/responses": socks connect tcp 85.255.176.68:12324->chatgpt.com:443: username/password authentication failed`), true},
		{"proxy connection refused", errors.New(`proxyconnect tcp: dial tcp 1.2.3.4:1080: connect: connection refused`), true},
		{"no route to host", errors.New(`dial tcp 1.2.3.4:443: connect: no route to host`), true},
		{"dns resolution failure", errors.New(`dial tcp: lookup proxy.example.com: no such host`), true},
		{"network unreachable", errors.New(`dial tcp 1.2.3.4:443: connect: network is unreachable`), true},

		// Transient — a temporary blip. Fail over, but do NOT evict the account.
		{"client timeout", errors.New(`Post "https://chatgpt.com/...": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`), false},
		{"i/o timeout", errors.New(`dial tcp 1.2.3.4:443: i/o timeout`), false},
		{"connection reset by peer", errors.New(`read tcp 10.0.0.1:5->2.2.2.2:443: read: connection reset by peer`), false},
		{"unexpected eof", errors.New(`unexpected EOF`), false},
		{"broken pipe", errors.New(`write tcp 10.0.0.1:5->2.2.2.2:443: write: broken pipe`), false},

		{"nil error", nil, false},

		// ── Typed-error cases ──────────────────────────────────────────────
		// ECONNREFUSED wrapped in the canonical net.OpError shape Go produces.
		{
			"ECONNREFUSED via net.OpError",
			&net.OpError{
				Op:  "dial",
				Net: "tcp",
				Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
			},
			true,
		},
		// Bare syscall error (errors.Is traverses the chain).
		{"ECONNREFUSED bare", syscall.ECONNREFUSED, true},
		{"EHOSTUNREACH bare", syscall.EHOSTUNREACH, true},
		{"ENETUNREACH bare", syscall.ENETUNREACH, true},

		// *net.DNSError with IsNotFound — permanent DNS lookup failure.
		{
			"DNS not found (IsNotFound=true)",
			&net.DNSError{Err: "no such host", Name: "proxy.example.com", IsNotFound: true},
			true,
		},
		// *net.DNSError with IsNotFound=false — transient DNS timeout (not persistent).
		{
			"DNS timeout (IsNotFound=false)",
			&net.DNSError{Err: "i/o timeout", Name: "proxy.example.com", IsTimeout: true},
			false,
		},

		// context.Canceled — client gone; NOT classified as persistent.
		{"context.Canceled", context.Canceled, false},
		// context.DeadlineExceeded — slow upstream; NOT persistent.
		{"context.DeadlineExceeded", context.DeadlineExceeded, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyUpstreamTransportError(tc.err).Persistent
			if got != tc.persistent {
				t.Fatalf("classifyUpstreamTransportError(%q).Persistent = %v, want %v", errString(tc.err), got, tc.persistent)
			}
		})
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// TestHandleOpenAIUpstreamTransportError_PreservesExistingFailoverError 是 H1-a 核心用例：
// 通用首字节护栏超时错误（已是 *UpstreamFailoverError，带 Reason 与可区分 type）经传输层
// 出口后，必须原样返回——不得被重新包装成 502 + upstream_error，否则下游 handler
// 永远无法识别首字节超时（生产实证：M1/F1F2/G1 三轮修复因此失效）。
func TestHandleOpenAIUpstreamTransportError_PreservesExistingFailoverError(t *testing.T) {
	// 构造与护栏超时同型的 *UpstreamFailoverError（StatusCode=504, Reason=网关首字节超时,
	// ResponseBody 含可区分 type upstream_first_byte_timeout）。
	src := newUpstreamFirstByteTimeoutError(10 * time.Second)
	require.Equal(t, http.StatusGatewayTimeout, src.StatusCode)
	require.Equal(t, GatewayFirstByteTimeoutReason, src.Reason)
	require.True(t, bytes.Contains(src.ResponseBody, []byte(UpstreamFirstByteTimeoutType)))

	// 生产调用方恒传非 nil account；函数体在到达保留分支前会读取 account.Platform 等字段。
	acct := &Account{ID: 1, Name: "h1a-test", Platform: PlatformOpenAI}

	got := (&OpenAIGatewayService{}).handleOpenAIUpstreamTransportError(
		context.Background(), nil, acct, src, false, "")

	// 必须是同一个 *UpstreamFailoverError 实例（原样透传，未重新包装）。
	var fo *UpstreamFailoverError
	require.ErrorAs(t, got, &fo)
	require.Same(t, src, fo, "出口不得重新包装已有 *UpstreamFailoverError，必须原样返回")

	// 语义字段未丢：Reason 仍为网关首字节超时。
	require.Equal(t, GatewayFirstByteTimeoutReason, fo.Reason,
		"Reason 不得丢失，否则 handler 无法按 Reason 识别首字节超时")
	// StatusCode 未被改成 502。
	require.Equal(t, http.StatusGatewayTimeout, fo.StatusCode,
		"StatusCode 不得被改成 502")
	// ResponseBody 未被换成通用 502 体（upstream_error），仍含可区分 type。
	require.False(t, bytes.Equal(fo.ResponseBody, openAITransportFailoverBody),
		"ResponseBody 不得被换成通用 502 体 openAITransportFailoverBody")
	require.Contains(t, string(fo.ResponseBody), UpstreamFirstByteTimeoutType,
		"ResponseBody 必须仍含可区分 type upstream_first_byte_timeout")
	require.NotContains(t, string(fo.ResponseBody), "upstream_error",
		"ResponseBody 不得被抹平为通用 upstream_error")
}

// TestHandleOpenAIUpstreamTransportError_PlainTransportErrorStillWrapped 守卫既有行为：
// 普通传输错误（非 failover 错误）仍应被包装成 502 + upstream_error（既有行为不变）。
func TestHandleOpenAIUpstreamTransportError_PlainTransportErrorStillWrapped(t *testing.T) {
	plainErr := errors.New("dial tcp: connection refused")
	acct := &Account{ID: 2, Name: "h1a-plain", Platform: PlatformOpenAI}

	got := (&OpenAIGatewayService{}).handleOpenAIUpstreamTransportError(
		context.Background(), nil, acct, plainErr, false, "")

	var fo *UpstreamFailoverError
	require.ErrorAs(t, got, &fo, "普通传输错误必须被包装成 *UpstreamFailoverError")
	require.Equal(t, http.StatusBadGateway, fo.StatusCode,
		"普通传输错误必须仍是 502")
	require.True(t, bytes.Equal(fo.ResponseBody, openAITransportFailoverBody),
		"普通传输错误必须仍用通用 502 体 openAITransportFailoverBody")
	require.Contains(t, string(fo.ResponseBody), "upstream_error",
		"普通传输错误必须仍是通用 upstream_error")
	require.Empty(t, fo.Reason, "普通传输错误不应带有首字节超时 Reason")
}

// TestHandleOpenAIUpstreamTransportError_CNPriority 守卫既有行为：
// CN 首包超时分支（isOpenAICNFirstByteTimeout 可识别的 errOpenAICNFirstByteTimeout）必须优先
// 命中，走 CN 冷却链（failoverOpenAICNFirstByteTimeout），不被新增的保留分支提前截走。
//
// 注：errOpenAICNFirstByteTimeout 是普通 errors.New 实例（非 *UpstreamFailoverError），
// 因此新增保留分支（仅 errors.As(&UpstreamFailoverError)）本就截取不到它——此处用例仍作为
// 回归守护，确认 CN 路径产出 504 + 网关首字节超时 Reason 的预期形态不变。
func TestHandleOpenAIUpstreamTransportError_CNPriority(t *testing.T) {
	cnErr := errOpenAICNFirstByteTimeout
	acct := &Account{ID: 3, Name: "h1a-cn", Platform: PlatformOpenAI}

	got := (&OpenAIGatewayService{}).handleOpenAIUpstreamTransportError(
		context.Background(), nil, acct, cnErr, false, "gpt-4")

	var fo *UpstreamFailoverError
	require.ErrorAs(t, got, &fo, "CN 首包超时必须走 failover 路径")
	require.Equal(t, http.StatusGatewayTimeout, fo.StatusCode,
		"CN 首包超时必须是 504")
	require.Equal(t, GatewayFirstByteTimeoutReason, fo.Reason,
		"CN 首包超时必须带网关首字节超时 Reason")
	require.True(t, bytes.Equal(fo.ResponseBody, openAICNFirstByteTimeoutFailoverBody),
		"CN 首包超时必须仍用 CN 错误体")
}

// TestHandleOpenAIUpstreamTransportError_FirstByteTimeoutSurvivesExit_EndToEnd 贯通锁定
// 「传输层出口不抹平首字节超时语义」这一层。
//
// 为什么需要这条：M1(8d5b95886) / F1F2(74caf4670) / G1(77adae3b8) 三轮修复的
// 单测**全绿**，但生产实证每次都未生效——客户端始终收到通用 upstream_error + 502。
// 根因就在本函数：它把已是 *UpstreamFailoverError 的错误（通用首字节护栏超时
// newUpstreamFirstByteTimeoutError，带 Reason 与可区分 type）重新包装成
// {StatusCode: 502, ResponseBody: openAITransportFailoverBody}，把语义全部抹平。
//
// 本用例从**出口函数**这一层锁定：护栏超时错误经出口后，Reason 与 ResponseBody
// 里的可区分 type 必须存活，且 StatusCode 不得被改成 502。
// 反证：去掉出口的保留分支 → 本用例必须 FAIL。
func TestHandleOpenAIUpstreamTransportError_FirstByteTimeoutSurvivesExit_EndToEnd(t *testing.T) {
	svc := &OpenAIGatewayService{}
	ctx := context.Background()

	// 通用首字节护栏超时错误：正是 doOpenAIUpstreamWithGuard 护栏耗尽时产出的那个。
	guardErr := newUpstreamFirstByteTimeoutError(30 * time.Second)
	require.NotNil(t, guardErr)
	require.Equal(t, GatewayFirstByteTimeoutReason, guardErr.Reason,
		"前置条件：护栏超时错误必须带首字节超时语义标记")
	require.Contains(t, string(guardErr.ResponseBody), UpstreamFirstByteTimeoutType,
		"前置条件：护栏超时错误的响应体必须含可区分的类型标识")
	require.Equal(t, http.StatusGatewayTimeout, guardErr.StatusCode)

	// 经传输层出口。
	acct := &Account{ID: 1, Name: "h1-endtoend", Platform: PlatformOpenAI}
	outErr := svc.handleOpenAIUpstreamTransportError(ctx, nil, acct, guardErr, false, "")
	require.Error(t, outErr)

	var outFailover *UpstreamFailoverError
	require.ErrorAs(t, outErr, &outFailover,
		"出口必须返回 failover 错误形态")

	// 核心断言：语义未被抹平。
	require.Equal(t, GatewayFirstByteTimeoutReason, outFailover.Reason,
		"首字节超时语义标记必须在传输层出口存活（被抹平则下游 handler 永远无法识别首字节超时）")
	require.NotEqual(t, http.StatusBadGateway, outFailover.StatusCode,
		"首字节超时的状态码不得被传输层出口改成 502（否则与真实上游 502 无法区分）")
	require.Contains(t, string(outFailover.ResponseBody), UpstreamFirstByteTimeoutType,
		"可区分的类型标识必须在传输层出口存活于响应体中")
	require.NotContains(t, string(outFailover.ResponseBody), "upstream_error\"",
		"响应体不得被换成通用 upstream_error 形态")
}
