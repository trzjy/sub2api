package service

// 本文件实现 cursor 平台的**鉴权材料按账号解析**与**出站分派挂载**（E2 卡，
// 方案 docs-local/cursor-platform-fix-plan.md §2 E2）。
//
// E1（cursor_upstream_forward.go / cursor_upstream_nodes.go）已提供出站原语：
// 换 host 到 shareai 号池中转节点 + 注入 x-jg-auth 鉴权头，请求体与流式帧原样透传。
// E2 只做接线，不重复实现 E1 的任何机制。
//
// 本卡的三条硬红线（与 E1 一致，不得放宽）：
//  1. 鉴权材料**按账号**解析（quota_key_id → 解密激活码 → wasm provider），
//     绝不做成全局单例共享——不同账号的激活码不同，共享即等于把 A 号的凭证发给 B 号。
//  2. 鉴权材料不可得 → 失败关闭，绝不发出无鉴权请求去白耗 shareai 额度。
//  3. 绝不把 OpenAI 形状的请求体原样发往 Cursor 原生端点，也绝不降级重试到其它上游。

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

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

// cursorUpstreamStreamMaxBytes 是对话响应体的读取上限。
//
// 正常长回答在百 KB 量级；给 32 MiB 上限足以容纳超长上下文回答，又不至于让
// 失控上游把内存吃满。超限即失败关闭（ErrCursorProtocol 之外的上限错误）。
const cursorUpstreamStreamMaxBytes = 32 << 20

// ---------------------------------------------------------------------------
// 节点解析器：进程级单例（缓存需要，非共享凭证）
// ---------------------------------------------------------------------------

var (
	cursorNodesOnce     sync.Once
	cursorNodesResolver *CursorUpstreamNodesResolver
)

// cursorUpstreamNodesSingleton 返回进程级节点解析器单例。
//
// 单例的理由是**缓存**，不是共享状态：节点发现带 10 分钟 TTL，进程内共用一个实例
// 才能让动态换线在 10 分钟内只打一次 COS；每请求新建解析器会让缓存永不命中，每次
// 对话都打上游（放大故障面且拖慢首包）。节点本身是站点级信息、不含任何账号凭证，
// 故节点单例不违反「鉴权材料不得全局共享」的红线——后者只针对 auth provider。
func cursorUpstreamNodesSingleton() *CursorUpstreamNodesResolver {
	cursorNodesOnce.Do(func() {
		cursorNodesResolver = NewCursorUpstreamNodesResolver()
	})
	return cursorNodesResolver
}

// ---------------------------------------------------------------------------
// 鉴权材料：按账号解析（quota_key_id → 解密 → wasm）
// ---------------------------------------------------------------------------

// ErrCursorQuotaKeyUnresolved 表示账号未绑定可解析的 quota key，出站鉴权材料不可得。
var ErrCursorQuotaKeyUnresolved = errors.New("cursor: account has no resolvable quota key (fail closed)")

// cursorAccountAuthProvider 为**单个账号**提供 x-jg-auth 鉴权材料。
//
// 绑定该账号的 quota key ID：每次取材料时按 ID 取回 quota_keys 行、解密激活码，
// 并按同一 ID 解析出站 deviceId（无则生成落库、之后永久复用），再交 E1 的 wasm
// provider 合成材料。材料在 provider 内部缓存（E1 已实现），故稳态下每账号每次
// 对话只跑一次 wasm。
type cursorAccountAuthProvider struct {
	quotaKeyID int64
	quotaKeys  QuotaKeyRepository
	enc        QuotaKeyEncryptor
	deviceIDs  CursorDeviceIDResolver

	// mu 保护 cached 的读写。缓存键即本 provider 绑定的**单一账号**，故不存在
	// 跨账号串号风险；激活码轮换会换新的 quota key 行，由账号实例自然隔离。
	mu     sync.Mutex
	cached string
}

// newCursorAccountAuthProvider 为指定账号构造鉴权材料提供者。
//
// quotaKeyID 取自账号自身的 QuotaKeyID；quotaKeys / enc / deviceIDs 为 nil 时
// 构造失败，禁止静默回落明文（与 cursor_upstream_client.go 同口径）。
func newCursorAccountAuthProvider(
	account *Account,
	quotaKeys QuotaKeyRepository,
	enc QuotaKeyEncryptor,
	deviceIDs CursorDeviceIDResolver,
) (CursorUpstreamAuthProvider, error) {
	if account == nil || account.QuotaKeyID == nil || *account.QuotaKeyID <= 0 {
		return nil, ErrCursorQuotaKeyUnresolved
	}
	if quotaKeys == nil || enc == nil || deviceIDs == nil {
		return nil, errors.New("cursor: quota key repository, encryptor and device id resolver are required")
	}
	return &cursorAccountAuthProvider{
		quotaKeyID: *account.QuotaKeyID,
		quotaKeys:  quotaKeys,
		enc:        enc,
		deviceIDs:  deviceIDs,
	}, nil
}

// CursorUpstreamAuth 实现 CursorUpstreamAuthProvider：返回该账号的 x-jg-auth 头值。
// 任何失败都返回错误（绝不返回空串——空串会得到语义不明的 401）。
func (p *cursorAccountAuthProvider) CursorUpstreamAuth(ctx context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached != "" {
		return p.cached, nil
	}

	_, code, err := resolveCursorQuotaKeyCodeWithRepo(ctx, p.quotaKeyID, p.quotaKeys, p.enc)
	if err != nil {
		return "", err
	}

	// 出站 deviceId：按 quota key ID 从站点侧解析（库内没有则随机生成一份并落库，
	// 之后永久复用）。**绝不**由激活码派生（B-10 已推翻该做法：插件语义是
	// uuidv4() 随机生成后固化，与激活码无关；按码派生会得到与设备绑定不匹配的值）。
	resolvedDeviceID, err := p.deviceIDs.DeviceIDFor(ctx, p.quotaKeyID)
	if err != nil {
		return "", fmt.Errorf("cursor: resolve device id for quota key %d: %w", p.quotaKeyID, err)
	}
	if strings.TrimSpace(resolvedDeviceID) == "" {
		return "", fmt.Errorf("%w: quota key %d resolved to empty device id", ErrCursorQuotaKeyUnresolved, p.quotaKeyID)
	}

	// 复用 E1 的 wasm provider 生成 x-jg-auth：材料 = zk03({d: deviceId, a: code})。
	material, err := NewCursorWasmAuthProvider(resolvedDeviceID, code).CursorUpstreamAuth(ctx)
	if err != nil {
		return "", err
	}
	p.cached = material
	return material, nil
}

// resolveCursorAccountCode 按账号解析出解密后的激活码明文（供 AuthService 换 token）。
//
// 与 cursorAccountAuthProvider 的材料解析同源同纪律：失败关闭、错误信息不含明文。
func resolveCursorAccountCode(
	ctx context.Context,
	account *Account,
	quotaKeys QuotaKeyRepository,
	enc QuotaKeyEncryptor,
) (string, error) {
	if account == nil || account.QuotaKeyID == nil || *account.QuotaKeyID <= 0 {
		return "", ErrCursorQuotaKeyUnresolved
	}
	if quotaKeys == nil || enc == nil {
		return "", ErrCursorQuotaKeyUnresolved
	}
	_, code, err := resolveCursorQuotaKeyCodeWithRepo(ctx, *account.QuotaKeyID, quotaKeys, enc)
	if err != nil {
		return "", err
	}
	return code, nil
}

// resolveCursorQuotaKeyCodeWithRepo 是「quota key ID → 激活码明文」的唯一实现，
// 被 x-jg-auth 材料解析与 AuthService 换 token 两处复用（避免二次 DB 查询与
// 两套失败关闭口径）。
func resolveCursorQuotaKeyCodeWithRepo(
	ctx context.Context,
	quotaKeyID int64,
	quotaKeys QuotaKeyRepository,
	enc QuotaKeyEncryptor,
) (int64, string, error) {
	qk, err := quotaKeys.GetByID(ctx, quotaKeyID)
	if err != nil {
		// 错误信息绝不含激活码明文。
		return 0, "", fmt.Errorf("cursor: load quota key %d: %w", quotaKeyID, err)
	}
	if qk == nil || strings.TrimSpace(qk.CodeEncrypted) == "" {
		return 0, "", fmt.Errorf("%w: quota key %d has no activation code", ErrCursorQuotaKeyUnresolved, quotaKeyID)
	}
	plain, err := enc.Decrypt([]byte(qk.CodeEncrypted))
	if err != nil {
		return 0, "", fmt.Errorf("cursor: decrypt activation code: %w", err)
	}
	code := strings.TrimSpace(string(plain))
	if code == "" {
		return 0, "", fmt.Errorf("%w: quota key %d decrypted to empty activation code", ErrCursorQuotaKeyUnresolved, quotaKeyID)
	}
	return quotaKeyID, code, nil
}

// 编译期断言：本文件依赖 ent.QuotaKey 的 CodeEncrypted 字段（与 E1 取号链同源）。
var _ = ent.QuotaKey{}.CodeEncrypted

// ---------------------------------------------------------------------------
// 出站分派
// ---------------------------------------------------------------------------

// ErrCursorProtocolBridgeUnavailable 表示 Cursor 原生协议 ↔ OpenAI 入站形状的转换层
// **未装配**（转发器或依赖缺失）。保留该错误以兼容既有失败关闭断言——
// E6 起转换层已实现（cursor_protocol_request.go / cursor_protocol_response.go），
// 正常路径不再返回它，只有依赖未注入时才会命中。
var ErrCursorProtocolBridgeUnavailable = errors.New("cursor: protocol bridge to Cursor native endpoints is not wired (fail closed)")

// isCursorAccount 判定账号是否为 cursor 平台账号（分派前置谓词）。
func isCursorAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformCursor
}

// SetCursorAuthDeps 注入 cursor 出站鉴权所需的窄依赖（装配点专用）。
//
// 采用 setter 而非改 NewOpenAIGatewayService 签名：cursor 的鉴权链路只在
// forwardCursor 内消费，而 OpenAIGatewayService 的构造签名被 wire_gen 与大量
// 测试共用，改签名会牵动全链路装配与既有测试替身。setter 与既有
// QuotaKeyService.SetAutoEntry / ChannelMonitorV2Service.SetAccountRepository 同构。
//
// enc 为 nil 时保持未注入态：forwardCursor 会因依赖缺失而失败关闭，绝不回落明文。
//
// deviceIDs 用于解析出站 deviceId（B-10）；生产装配注入 *QuotaKeyService 即可。
func (s *OpenAIGatewayService) SetCursorAuthDeps(quotaKeys QuotaKeyRepository, enc QuotaKeyEncryptor, deviceIDs CursorDeviceIDResolver) {
	if s == nil {
		return
	}
	s.cursorQuotaKeys = quotaKeys
	s.cursorQuotaKeyEnc = enc
	s.cursorDeviceIDs = deviceIDs
}

// SetCursorForwarder 注入 cursor 出站转发器（E6 卡装配点专用）。
//
// 为什么用 setter 而非在 forwardCursor 内 new：节点解析器必须复用进程级单例
// （cursorUpstreamNodesSingleton），否则 10 分钟的节点缓存永不命中，每次对话都
// 打一次 COS 动态换线接口。测试也据此注入 httptest 服务器。
func (s *OpenAIGatewayService) SetCursorForwarder(f *CursorUpstreamForwarder) {
	if s == nil {
		return
	}
	s.cursorForwarder = f
}

// forwardCursor 是 cursor 平台的转发入口，挂在 OpenAIGatewayService 的 platform 分支上
// （与 forwardCodeBuddy 同构：均在通用 OpenAI 路径**之前**分流）。
//
// 必须先于通用路径分流，原因与 codebuddy 同：cursor 上游是 Cursor **原生**协议端点
// （`/aiserver.v1.*Service/*`，二进制 protobuf + envelope 帧），与 OpenAI
// Chat Completions 形状不兼容。落到通用路径会把 OpenAI 形状的 body 发到 Cursor 端点，
// 必然被上游拒绝，且白耗 shareai 额度。
//
// E6 起本函数执行真实转发，五步（任一步失败即失败关闭，绝不降级）：
//
//	① 模型闸门：账号 model_mapping 必须**显式命中**
//	② 设备级鉴权：x-jg-auth = zk03({d: deviceId, a: 激活码})
//	③ 用户级鉴权：AuthService/GetSessionToken 换 session_token（带 TTL 缓存）
//	④ 请求转换：OpenAI → StreamUnifiedChatRequestWithTools（protobuf + envelope）
//	⑤ 响应转换：envelope 帧流 → OpenAI SSE（或单条 chat.completion）
func (s *OpenAIGatewayService) forwardCursor(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	reqStream bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	if !isCursorAccount(account) {
		return nil, errors.New("cursor: forwardCursor requires a cursor platform account")
	}

	// ① 模型闸门：cursor 无内置模型目录，模型完全来自账号 model_mapping。
	//
	// 这里要求 ResolveMappedModel 的 matched=true（账号映射**显式命中**该模型），而不是
	// 只看 GetMappedModel 的返回值——后者在映射为空/未命中时会回落到请求模型名本身，
	// 那样闸门形同虚设，会把任意 OpenAI 模型名发往 Cursor 原生端点（必然被上游拒绝
	// 并白耗 shareai 额度）。空映射账号已在 IsModelSupported 失败关闭，此处是纵深防御。
	upstreamModel, matched := account.ResolveMappedModel(originalModel)
	if !matched || strings.TrimSpace(upstreamModel) == "" {
		return nil, fmt.Errorf("cursor: account %d does not map model %q (fail closed)", account.ID, originalModel)
	}

	fwd := s.cursorForwarderOrDefault()
	if fwd == nil {
		return nil, ErrCursorProtocolBridgeUnavailable
	}

	// ② 设备级鉴权材料（x-jg-auth）：按账号解析，材料不可得即失败关闭。
	authProvider, err := newCursorAccountAuthProvider(account, s.cursorQuotaKeys, s.cursorQuotaKeyEnc, s.cursorDeviceIDs)
	if err != nil {
		return nil, err
	}

	// ③ 用户级 token（B-13）：AuthService 换取，带 TTL 缓存。
	tokenSource, err := s.cursorBuildSessionTokenSource(ctx, fwd, authProvider, account)
	if err != nil {
		return nil, err
	}

	// ④ 请求转换：OpenAI → Cursor 原生 protobuf（失败关闭：多模态 / 未知角色直接报错）。
	var ccReq apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &ccReq); err != nil {
		return nil, fmt.Errorf("cursor: parse chat completions request: %w", err)
	}
	payload, err := cursorConvertRequest(&ccReq, upstreamModel)
	if err != nil {
		return nil, err
	}

	sessionToken, err := tokenSource.cursorSessionTokenWithAuth(ctx, authProvider)
	if err != nil {
		return nil, err
	}

	resp, err := fwd.Do(ctx, &CursorUpstreamRequest{
		Path: cursorPathStreamUnifiedChatWithTools,
		Body: payload,
		// 账号级鉴权材料：转发器是进程级共享的，材料必须逐请求携带，
		// 否则会把 A 号的 x-jg-auth 发给 B 号（方案 §2 E1 遗留风险登记）。
		AuthProvider: authProvider,
		Headers: map[string]string{
			"Content-Type":             cursorConnectContentType,
			"Connect-Protocol-Version": "1",
			// 用户级身份：AuthService 换来的 session_token（**不是** x-jg-auth 的
			// 激活码材料）。两者是不同维度，缺一即 ERROR_UNAUTHORIZED（§1.6）。
			"Authorization": "Bearer " + sessionToken,
		},
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		upstreamBody, rerr := readCursorUpstreamBody(resp.Body, cursorAuthResponseMaxBytes)
		if rerr != nil {
			return nil, cursorUpstreamStatusError(resp.StatusCode, nil, sessionToken)
		}
		// sessionToken 作为 secret 传入：上游错误文案可能回显 Authorization 头的值，
		// 不抹掉就会把会话 token 写进 ops 事件与客户端回传。
		code, msg := cursorExtractUpstreamErrorSummary(upstreamBody, sessionToken)
		s.cursorRecordUpstreamError(c, account, resp.StatusCode, code, msg, resp.TargetURL)
		return nil, cursorUpstreamStatusError(resp.StatusCode, upstreamBody, sessionToken)
	}

	rawBody, err := readCursorUpstreamBody(resp.Body, cursorUpstreamStreamMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("cursor: read upstream stream: %w", err)
	}

	// ⑤ 响应转换。Cursor 上游只有一种形态（envelope 字节流），故流式/非流式
	// 入站都由同一解析器处理：流式转 SSE，非流式合成单条 JSON。
	if reqStream {
		sse, perr := cursorParseStreamToSSE(rawBody, originalModel, sessionToken)
		if perr != nil {
			s.cursorRecordStreamError(c, account, perr, resp.TargetURL)
			return nil, perr
		}
		httpResp := cursorToHTTPResponse(resp, http.StatusOK, "text/event-stream", sse)
		streamResult, herr := s.handleStreamingResponse(ctx, httpResp, c, account, startTime, originalModel, upstreamModel)
		if herr != nil {
			return nil, herr
		}
		return cursorForwardResultFromStreaming(streamResult, originalModel, upstreamModel, startTime, httpResp.Header), nil
	}

	completion, perr := cursorParseStreamToCompletion(rawBody, originalModel, sessionToken)
	if perr != nil {
		s.cursorRecordStreamError(c, account, perr, resp.TargetURL)
		return nil, perr
	}
	httpResp := cursorToHTTPResponse(resp, http.StatusOK, "application/json", completion)
	nonStreamResult, herr := s.handleNonStreamingResponse(ctx, httpResp, c, account, originalModel, upstreamModel)
	if herr != nil {
		return nil, herr
	}
	return cursorForwardResultFromNonStreaming(nonStreamResult, originalModel, upstreamModel, startTime, httpResp.Header), nil
}

// cursorToHTTPResponse 把 Cursor 上游响应包装成 *http.Response，使通用
// handleStreamingResponse / handleNonStreamingResponse 能直接消费。
//
// 响应头只保留诊断安全字段（Content-Type 等），**不透传上游全部头**：Cursor 的
// 响应头可能带账号维度标识（与 codebuddy 的 redactCodeBuddyUpstreamErrorBody
// 同类风险），透传给客户端等于泄露账号信息。
func cursorToHTTPResponse(
	resp *CursorUpstreamResponse,
	status int,
	contentType string,
	body []byte,
) *http.Response {
	header := make(http.Header, 4)
	header.Set("Content-Type", contentType)
	header.Del("Content-Length")
	if rid := strings.TrimSpace(resp.Header.Get("x-request-id")); rid != "" {
		header.Set("x-request-id", rid)
	}
	return &http.Response{
		StatusCode:    status,
		Status:        http.StatusText(status),
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       &http.Request{Method: http.MethodPost},
	}
}

// cursorForwardResultFromStreaming 由流式结果构造转发结果（与 codebuddy 同口径）。
func cursorForwardResultFromStreaming(
	r *openaiStreamingResult, originalModel, upstreamModel string, startTime time.Time, respHeader http.Header,
) *OpenAIForwardResult {
	if r == nil {
		return &OpenAIForwardResult{
			Model: originalModel, UpstreamModel: upstreamModel, Stream: true,
			Duration: time.Since(startTime),
		}
	}
	usage := r.usage
	if usage == nil {
		usage = &OpenAIUsage{}
	}
	return &OpenAIForwardResult{
		UpstreamHeaders:  respHeader,
		ResponseID:       strings.TrimSpace(r.responseID),
		Usage:            *usage,
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		Stream:           true,
		OpenAIWSMode:     false,
		ResponseHeaders:  respHeader.Clone(),
		Duration:         time.Since(startTime),
		FirstTokenMs:     r.firstTokenMs,
		SearchCount:      r.searchCount,
		ImageCount:       r.imageCount,
		ImageOutputSizes: r.imageOutputSizes,
	}
}

// cursorForwardResultFromNonStreaming 由非流式结果构造转发结果。
func cursorForwardResultFromNonStreaming(
	r *openaiNonStreamingResult, originalModel, upstreamModel string, startTime time.Time, respHeader http.Header,
) *OpenAIForwardResult {
	if r == nil {
		return &OpenAIForwardResult{
			Model: originalModel, UpstreamModel: upstreamModel, Stream: false,
			Duration: time.Since(startTime),
		}
	}
	usage := r.OpenAIUsage
	if usage == nil {
		usage = &OpenAIUsage{}
	}
	return &OpenAIForwardResult{
		UpstreamHeaders:  respHeader,
		ResponseID:       strings.TrimSpace(r.responseID),
		Usage:            *usage,
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		Stream:           false,
		OpenAIWSMode:     false,
		ResponseHeaders:  respHeader.Clone(),
		Duration:         time.Since(startTime),
		SearchCount:      r.searchCount,
		ImageCount:       r.imageCount,
		ImageOutputSizes: r.imageOutputSizes,
	}
}

// cursorForwarderOrDefault 返回装配的转发器；未注入时构造一个用节点单例的默认实例。
//
// 默认实例只含**节点解析器**（站点级、非凭证），鉴权材料一律按请求从
// authProvider 取，故此处构造不构成凭证共享。
func (s *OpenAIGatewayService) cursorForwarderOrDefault() *CursorUpstreamForwarder {
	if s != nil && s.cursorForwarder != nil {
		return s.cursorForwarder
	}
	return NewCursorUpstreamForwarder(WithCursorUpstreamNodes(cursorUpstreamNodesSingleton()))
}

// cursorBuildSessionTokenSource 构造该账号的 AuthService token 来源。
//
// **按账号缓存 source**：token source 内含 session_token 缓存（TTL 30 分钟），
// 若每次请求都 new 一个，缓存永不命中 ⇒ 每次对话都换一次 token，把上游打爆
// （上游换 token 本身有速率限制）。缓存键是 quota_key_id，与凭证维度一致，
// 不会跨账号串号。
func (s *OpenAIGatewayService) cursorBuildSessionTokenSource(
	ctx context.Context,
	fwd *CursorUpstreamForwarder,
	authProvider CursorUpstreamAuthProvider,
	account *Account,
) (*cursorSessionTokenSource, error) {
	if account == nil || account.QuotaKeyID == nil || *account.QuotaKeyID <= 0 {
		return nil, fmt.Errorf("%w: account has no quota key", ErrCursorSessionTokenUnavailable)
	}
	qkID := *account.QuotaKeyID

	s.cursorTokenMu.Lock()
	cached, ok := s.cursorTokenSources[qkID]
	s.cursorTokenMu.Unlock()
	if ok {
		return cached, nil
	}

	code, err := s.cursorResolveAuthCode(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCursorSessionTokenUnavailable, err)
	}
	src, err := newCursorSessionTokenSource(fwd, authProvider, code)
	if err != nil {
		return nil, err
	}

	s.cursorTokenMu.Lock()
	defer s.cursorTokenMu.Unlock()
	// 双检：并发首建时复用先到者，避免同一账号产生多个 source（多份缓存）。
	if existing, ok := s.cursorTokenSources[qkID]; ok {
		return existing, nil
	}
	if s.cursorTokenSources == nil {
		s.cursorTokenSources = make(map[int64]*cursorSessionTokenSource)
	}
	s.cursorTokenSources[qkID] = src
	return src, nil
}

// cursorResolveAuthCode 取回该账号的激活码明文（供 AuthService 换 token）。
//
// 依赖缺失时失败关闭——绝不拿空 code 去换 token（上游会 501 DEPRECATED，
// 白耗请求且拿不到任何东西）。
func (s *OpenAIGatewayService) cursorResolveAuthCode(ctx context.Context, account *Account) (string, error) {
	if s == nil || s.cursorQuotaKeys == nil || s.cursorQuotaKeyEnc == nil {
		return "", ErrCursorQuotaKeyUnresolved
	}
	return resolveCursorAccountCode(ctx, account, s.cursorQuotaKeys, s.cursorQuotaKeyEnc)
}

// cursorRecordUpstreamError 把上游错误记入 ops 事件（不含任何凭证）。
func (s *OpenAIGatewayService) cursorRecordUpstreamError(
	c *gin.Context, account *Account, status int, code, msg, upstreamURL string,
) {
	if c == nil {
		return
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           account.Platform,
		AccountID:          account.ID,
		AccountName:        account.Name,
		UpstreamStatusCode: status,
		UpstreamURL:        upstreamURL,
		Kind:               "http_error",
		Message:            msg,
		Detail:             code,
	})
}

// cursorRecordStreamError 把流内错误记入 ops 事件。
func (s *OpenAIGatewayService) cursorRecordStreamError(
	c *gin.Context, account *Account, err error, upstreamURL string,
) {
	if c == nil || err == nil {
		return
	}
	s.cursorRecordUpstreamError(c, account, 0, "", err.Error(), upstreamURL)
}
