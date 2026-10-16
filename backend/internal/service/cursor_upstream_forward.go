package service

// 本文件实现 cursor 平台**出站转发**（E1 卡，方案 docs-local/cursor-platform-fix-plan.md §2 E1）。
//
// 取证结论（wasm 活体执行，脚本见 ~/.sub2api-acceptance/cursor-20261016-wasm-forensics/）：
// 插件不直连 Cursor 官方，而是把 Cursor 客户端的出站改写到 shareai 号池中转，
// 并且只做两件事：**换 host** + **注入鉴权头**。对话端点的路径、请求体形态、流式帧格式
// 全部沿用 Cursor 客户端原生协议（插件不改协议），故本站点无需复刻任何协议。
//
// wasm 活体实测得到的出站事实：
//   - zk27 generateH2RequestAuthReplacement(..., isMaster, route) 产出：
//       request.header.set('x-jg-auth', <encryptedCredentials>);
//       request.header.set('x-jg-model', '1');          // 仅 isMaster=true 时
//   - zk21 generateH2AuthorityReplacement(v, host) → this.authority="<host>"/*cursorH2Patch*/
//   - zk23 generateH2ConnectReplacement(...) → agent.connect("<host>", {rejectUnauthorized:false})
//   - zk17 generateAlwaysLocalReplacement(...) 只改 host 与鉴权头，**不动 path**
//   - zk03 encryptLocalData（插件 :8090 generateEncryptedCredentials）输入
//     JSON.stringify({d: deviceId, a: activationCode})，输出 base64(JSON{iv,ct,tag})
//   - zk05 buildAuthUrl → cursor://cursorFreeAuth?accessToken=&refreshToken=&email=
//
// 失败关闭（本文件硬红线）：
//   - 鉴权材料不可得（wasm 不可用 / 激活码缺失 / 加密失败）→ 返回明确错误，绝不发出请求。
//   - 路径不在已取证白名单 → 返回明确错误。
//   - 绝不降级重试到 Cursor 官方端点，绝不伪造 usage。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	wasmcrypto "github.com/Wei-Shaw/sub2api/internal/service/wasm"
)

// cursor 出站常量（全部来自 wasm 活体实测，见文件头注释）。
const (
	// cursorJgAuthHeader 是插件注入的鉴权头名。
	cursorJgAuthHeader = "x-jg-auth"
	// cursorJgModelHeader 是插件注入的模型标记头名。
	cursorJgModelHeader = "x-jg-model"
	// cursorJgModelHeaderValue 是 x-jg-model 的取值（isMaster=true 时）。
	cursorJgModelHeaderValue = "1"

	// cursorUpstreamDefaultTimeout 是出站请求默认超时（与取号客户端同量级）。
	cursorUpstreamDefaultTimeout = 180 * time.Second
)

// cursor 出站失败关闭错误（调用方可 errors.Is 识别）。
var (
	// ErrCursorAuthMaterialUnavailable 表示鉴权材料不可得：禁止发出请求。
	ErrCursorAuthMaterialUnavailable = errors.New("cursor: auth material unavailable (fail closed)")

	// ErrCursorUnsupportedEndpoint 表示端点/路径不在已取证范围内。
	ErrCursorUnsupportedEndpoint = errors.New("cursor: unsupported upstream endpoint (fail closed)")
)

// 已取证的 Cursor 原生端点路径。
//
// 依据：插件只替换 authority / connect 目标与请求头，请求 path 原样保留
// （zk17 / zk27 实测产物均不改 path）。故此处仅放行 Cursor 原生对话相关路径；
// 未列举的一律失败关闭，避免把任意路径打到中转节点。
const (
	cursorPathStreamUnifiedChatWithToolsSSE         = "/aiserver.v1.AIServiceService/StreamUnifiedChatWithToolsSSE"
	cursorPathStreamUnifiedChatWithToolsSSEWithMcp  = "/aiserver.v1.AIServiceService/StreamUnifiedChatWithToolsSSEWithMcp"
	cursorPathGenerateUnifiedCompletionWithToolsSSE = "/aiserver.v1.AIServiceService/GenerateUnifiedCompletionWithToolsSSE"
	cursorPathAvailableModels                       = "/aiserver.v1.AIServiceService/GetAvailableModels"
)

// cursorAllowedUpstreamPaths 是已取证的出站路径白名单（失败关闭用）。
var cursorAllowedUpstreamPaths = map[string]struct{}{
	cursorPathStreamUnifiedChatWithToolsSSE:         {},
	cursorPathStreamUnifiedChatWithToolsSSEWithMcp:  {},
	cursorPathGenerateUnifiedCompletionWithToolsSSE: {},
	cursorPathAvailableModels:                       {},
}

// IsCursorAllowedUpstreamPath 报告给定路径是否在已取证白名单内（E2 卡可用）。
func IsCursorAllowedUpstreamPath(path string) bool {
	_, ok := cursorAllowedUpstreamPaths[path]
	return ok
}

// CursorUpstreamRequest 是一次 cursor 出站请求的描述（最终形态）。
type CursorUpstreamRequest struct {
	// Path 是 Cursor 原生端点路径（须在白名单内）。
	Path string
	// Body 是请求体字节（沿用 Cursor 原生协议，本层不改写）。
	Body []byte
	// Headers 是除鉴权/模型标记外的附加头（如 Cursor 的 Authorization）。
	Headers map[string]string
	// UseHTTP2 为 true 时走 HTTP2Base，否则走 HTTP1Base。
	UseHTTP2 bool
}

// CursorUpstreamResponse 是 cursor 上游的原始响应（流式帧原样透传）。
type CursorUpstreamResponse struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
	// TargetURL 是实际出站 URL（诊断用，不含任何凭证）。
	TargetURL string
}

// CursorUpstreamAuthProvider 提供 x-jg-auth 鉴权材料。
//
// 实现方须基于**确定性推导**（device-id 由激活码派生）产出材料；材料不可得时返回错误，
// 绝不返回空串（空串会得到语义不明的 401）。
type CursorUpstreamAuthProvider interface {
	CursorUpstreamAuth(ctx context.Context) (string, error)
}

// CursorUpstreamOption 用于测试或自定义装配转发器。
type CursorUpstreamOption func(*CursorUpstreamForwarder)

// WithCursorUpstreamHTTPClient 覆盖出站 HTTP 客户端（测试用）。
func WithCursorUpstreamHTTPClient(hc *http.Client) CursorUpstreamOption {
	return func(f *CursorUpstreamForwarder) {
		if hc != nil {
			f.httpClient = hc
		}
	}
}

// WithCursorUpstreamNodes 注入节点解析器（测试用）。
func WithCursorUpstreamNodes(r *CursorUpstreamNodesResolver) CursorUpstreamOption {
	return func(f *CursorUpstreamForwarder) {
		if r != nil {
			f.nodes = r
		}
	}
}

// WithCursorUpstreamAuthProvider 注入鉴权材料提供者（测试用）。
func WithCursorUpstreamAuthProvider(p CursorUpstreamAuthProvider) CursorUpstreamOption {
	return func(f *CursorUpstreamForwarder) {
		if p != nil {
			f.authProvider = p
		}
	}
}

// CursorUpstreamForwarder 是 cursor 平台的出站转发器。
type CursorUpstreamForwarder struct {
	nodes        *CursorUpstreamNodesResolver
	httpClient   *http.Client
	authProvider CursorUpstreamAuthProvider
}

// NewCursorUpstreamForwarder 构造 cursor 出站转发器。
//
// authProvider 为 nil 时转发器拒绝发出任何请求（失败关闭）。
func NewCursorUpstreamForwarder(opts ...CursorUpstreamOption) *CursorUpstreamForwarder {
	f := &CursorUpstreamForwarder{
		nodes:      NewCursorUpstreamNodesResolver(),
		httpClient: &http.Client{Timeout: cursorUpstreamDefaultTimeout},
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

// HTTP1BaseURL 返回当前 HTTP/1.1 出站 base（供 E2 卡拼 GetOpenAIBaseURL）。
func (f *CursorUpstreamForwarder) HTTP1BaseURL(ctx context.Context) string {
	return f.nodes.Resolve(ctx).HTTP1Base
}

// HTTP2BaseURL 返回当前 HTTP/2 出站 base。
func (f *CursorUpstreamForwarder) HTTP2BaseURL(ctx context.Context) string {
	return f.nodes.Resolve(ctx).HTTP2Base
}

// Do 执行一次 cursor 出站请求：host 换成号池中转节点 + 注入鉴权头，
// 请求路径 / 请求体 / 流式帧一律沿用 Cursor 原生协议。
//
// 失败关闭：路径不在白名单、鉴权材料不可得 → 直接返回错误，不发出请求。
func (f *CursorUpstreamForwarder) Do(
	ctx context.Context,
	req *CursorUpstreamRequest,
) (*CursorUpstreamResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("%w: nil request", ErrCursorUnsupportedEndpoint)
	}
	if !IsCursorAllowedUpstreamPath(req.Path) {
		return nil, fmt.Errorf("%w: path %q", ErrCursorUnsupportedEndpoint, req.Path)
	}

	// 鉴权材料先取：不可得则失败关闭，绝不发出无鉴权请求去白耗中转额度。
	auth, err := f.cursorAuth(ctx)
	if err != nil {
		return nil, err
	}

	nodes := f.nodes.Resolve(ctx)
	base := nodes.HTTP1Base
	if req.UseHTTP2 {
		base = nodes.HTTP2Base
	}
	target := strings.TrimRight(base, "/") + req.Path

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(req.Body))
	if err != nil {
		return nil, fmt.Errorf("cursor: build upstream request: %w", err)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	if httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	// 鉴权与模型标记头（wasm zk27 活体实测产物）。
	httpReq.Header.Set(cursorJgAuthHeader, auth)
	httpReq.Header.Set(cursorJgModelHeader, cursorJgModelHeaderValue)

	resp, err := f.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("cursor: upstream request failed: %w", err)
	}
	return &CursorUpstreamResponse{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       resp.Body,
		TargetURL:  target,
	}, nil
}

// cursorAuth 取鉴权材料：provider 缺失或返回错误都视为不可得（失败关闭）。
func (f *CursorUpstreamForwarder) cursorAuth(ctx context.Context) (string, error) {
	if f.authProvider == nil {
		return "", fmt.Errorf("%w: no auth provider configured", ErrCursorAuthMaterialUnavailable)
	}
	auth, err := f.authProvider.CursorUpstreamAuth(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrCursorAuthMaterialUnavailable, err)
	}
	if strings.TrimSpace(auth) == "" {
		return "", fmt.Errorf("%w: empty auth material", ErrCursorAuthMaterialUnavailable)
	}
	return auth, nil
}

// ---------------------------------------------------------------------------
// 鉴权材料生成：复用内嵌 wasm 的 zk03（encryptLocalData）
// ---------------------------------------------------------------------------

// cursorEncryptedCredentialsInput 是 zk03 的输入明文，等价于插件
// JSON.stringify({d: deviceId, a: activationCode})（键序 d 在前、a 在后 ——
// 与插件 :8092 的对象字面量顺序一致，不可调换，否则密文语义错位）。
type cursorEncryptedCredentialsInput struct {
	D string `json:"d"`
	A string `json:"a"`
}

// cursorWasmAuthProvider 基于内嵌 wasm 生成 x-jg-auth 材料。
//
// device-id 由激活码确定性派生（复用 cursor_upstream_client.go 的 deviceID(code)），
// 因此鉴权材料可在站点侧独立生成，不依赖任何本机客户端状态（方案 §1.3 裁定）。
type cursorWasmAuthProvider struct {
	activationCode string

	mu sync.Mutex
	// cached 缓存最近一次生成的材料。AES-GCM 每次 IV 不同，但同一份材料
	// 在其有效期内重复使用与插件行为一致（插件亦把材料落盘复用），
	// 故缓存以避免每次对话都跑一次 wasm。
	cached string
}

// NewCursorWasmAuthProvider 构造基于内嵌 wasm 的鉴权材料提供者。
//
// activationCode 由调用方经 QuotaKeyEncryptor 解密后传入；明文绝不落日志。
func NewCursorWasmAuthProvider(activationCode string) CursorUpstreamAuthProvider {
	return &cursorWasmAuthProvider{activationCode: activationCode}
}

// CursorUpstreamAuth 实现 CursorUpstreamAuthProvider：返回 x-jg-auth 头值。
func (p *cursorWasmAuthProvider) CursorUpstreamAuth(_ context.Context) (string, error) {
	if strings.TrimSpace(p.activationCode) == "" {
		return "", errors.New("empty activation code")
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached != "" {
		return p.cached, nil
	}

	payload, err := json.Marshal(cursorEncryptedCredentialsInput{
		D: deviceID(p.activationCode),
		A: p.activationCode,
	})
	if err != nil {
		// 错误信息绝不含激活码明文。
		return "", fmt.Errorf("build credentials payload: %w", err)
	}
	cipher, err := wasmcrypto.EncryptLocalData(string(payload))
	if err != nil {
		return "", fmt.Errorf("encrypt credentials: %w", err)
	}
	p.cached = cipher
	return cipher, nil
}
