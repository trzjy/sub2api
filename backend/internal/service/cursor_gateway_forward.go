package service

// 本文件是 Cursor 平台的**出站鉴权边界**与**错误/观测边界**。
//
// 执行入口与状态所有权已按 §24.2-2 收敛到 cursor_execution_service.go 的
// CursorExecutionService（唯一实例，直接 DI）。本文件只保留：
//
//  1. 按账号解析的出站鉴权材料（Bearer + 同源 deviceId 的 x-jg-auth）；
//  2. 流内拒绝模板识别与终态错误构造；
//  3. ops 上游错误/流内错误的记录；
//  4. usage 字段的逐字段搬运（绝不折算、绝不补默认值）。
//
// 历史上本文件还包含 forwardCursor（OpenAIGatewayService 方法）、三组 Cursor 专有
// setter、以及"未注入时另建默认转发器"的回退构造与包级安装的桥接入口。这些符号已随
// §24.2-2 / §25.14-C 一并删除，全树不得再出现（含测试）。
//
// ════════════════════════════════════════════════════════════════════════════
// 出站鉴权形态（方案 §15.2-1、§16.1，证据 103 / 134 / 135）：
//
//	Authorization: Bearer <账号已存 access_token>
//	x-jg-auth:     wasm zk03({d: deviceId, a: 激活码})
//
//  1. **Standard 保留账号 authorization（已存 access_token）**，同时携带 x-jg-auth；
//     绝不把激活码当 OAuth code 去换 session token，绝不跨账号复用会话材料。
//  2. **deviceId 与取号链同源三分支**（与 P1-B4 同判据、同结果）。
//  3. **无 Bearer 样本成功不构成授权**：账号凭证缺失时失败关闭并明确报错，
//     绝不删账号对象、绝不回收 Bearer（I2 红线）。
//
// 流内拒绝（HTTP200 正常流帧中出现固定模板「密钥已过期或额度耗尽，请续费后使用」）
// 一律按终态错误返回：**不得**当成功、**不得**伪造 usage。按 §25.14-C，激活码
// 终态只由唯一 lookup 观测入口（CursorAutoEntryService）更新，流内拒绝**不再**
// 触发任何来源期限/状态写入，也不再发起流内网络核对。
// ════════════════════════════════════════════════════════════════════════════
//
// 本卡硬红线（与 E1/E2 一致，不得放宽）：
//  1. 鉴权材料**按账号**解析，绝不做成全局单例共享。
//  2. 鉴权材料不可得 → 失败关闭，绝不发出无鉴权请求去白耗 shareai 额度。
//  3. 绝不把 OpenAI 形状的请求体原样发往 Cursor 原生端点，也绝不降级重试到其它上游。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/gin-gonic/gin"
)

// cursorUpstreamStreamMaxBytes 是对话响应体的读取上限。
//
// 正常长回答在百 KB 量级；给 32 MiB 上限足以容纳超长上下文回答，又不至于让
// 失控上游把内存吃满。超限即失败关闭（ErrCursorProtocol 之外的上限错误）。
const cursorUpstreamStreamMaxBytes = 32 << 20

// cursorCredentialAccessToken 是账号凭证里用户级会话令牌的键（号池拉号落库时写入，
// 经既有 account_credentials 加密链保护；GetCredential 取到的是已解密明文）。
const cursorCredentialAccessToken = "access_token"

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
// 才能让动态换线在 10 分钟内只打一次 COS。节点本身是站点级信息、不含任何账号凭证。
func cursorUpstreamNodesSingleton() *CursorUpstreamNodesResolver {
	cursorNodesOnce.Do(func() {
		cursorNodesResolver = NewCursorUpstreamNodesResolver()
	})
	return cursorNodesResolver
}

// ---------------------------------------------------------------------------
// 鉴权材料：按账号解析（quota_key_id → 解密 → 同源 deviceId → wasm）
// ---------------------------------------------------------------------------

// ErrCursorQuotaKeyUnresolved 表示账号未绑定可解析的 quota key，出站鉴权材料不可得。
var ErrCursorQuotaKeyUnresolved = errors.New("cursor: account has no resolvable quota key (fail closed)")

// ErrCursorAccountBearerMissing 表示选中账号未存 access_token（用户级凭证缺失）。
//
// 证据 134-no-bearer-active-key：无 Bearer 也能拿到 HTTP200 成功样本——但那是"上游
// 未拒绝"，**不等于**本站可以拿空凭证出站。账号凭证缺失一律失败关闭并明确报错：
// 绝不因此删账号对象、绝不回收/清空已存 Bearer（I2 红线）。
var ErrCursorAccountBearerMissing = errors.New("cursor: account carries no access_token credential (fail closed)")

// ErrCursorRelayAuthRejected 表示中转/上游以鉴权错误（401/403 或等价鉴权失败体）
// 拒绝了本次出站请求。消费方（cursor_protocol_response.go 的错误分类与既有失败关闭
// 断言）只认这个身份，语义与取值均不得变。
var ErrCursorRelayAuthRejected = errors.New("cursor: relay rejected the request with an auth error")

// ErrCursorKeyRejectedTerminal 表示上游在 HTTP200 正常流帧中回以激活码终态拒绝模板。
// 命中即终态错误：**不得**当成功、**不得**伪造 usage。
var ErrCursorKeyRejectedTerminal = ErrCursorKeyRejectedUpstream // alias（errors.Is 双哨兵均成立）

// cursorAccountAuthProvider 为**单个账号**提供 x-jg-auth 鉴权材料。
//
// 绑定该账号的 quota key ID：每次取材料时按 ID 取回 quota_keys 行、解密激活码，
// 并按同源三分支解析 deviceId，再交 E1 的 wasm provider 合成材料。材料在 provider
// 内部缓存，故稳态下每账号每次对话只跑一次 wasm。
//
// **只提供设备级材料**：用户级 Bearer 走 cursorResolveAccountBearer（账号维度），
// 两者都要，缺一即失败关闭。绝不存在任何跨账号共享的会话材料。
type cursorAccountAuthProvider struct {
	quotaKeyID int64
	quotaKeys  QuotaKeyRepository
	enc        QuotaKeyEncryptor
	deviceIDs  CursorDeviceIDResolver

	// mu 保护 cached 的读写。缓存键即本 provider 绑定的**单一账号**，故不存在
	// 跨账号串号风险；激活码轮换会换新的 quota key 行，由账号实例自然隔离。
	mu     sync.Mutex
	cached string
	device string
	// fingerprint 是**生成 cached 那一次**来源行/解密结果给出的内部指纹
	// （§25.17.3「A05首次来源指纹」）。与 cached 同一次赋值，绝不是事后另读一次
	// MAC 反推，故换码夹在生成头与建会话之间时不会把旧头标成新 MAC。
	//
	// 只在进程内传递：绝不进日志、DTO、工具 ID 或任何对外文本。
	fingerprint cursorSourceFingerprint
}

// cursorSourceFingerprint 是首次 x-jg-auth 生成时固定的**内部**来源指纹。
//
// QuotaKeyID 与 CodeMAC 取自生成该鉴权材料的**同一次** quota_keys 行快照与同一次
// 解密明文：CodeMAC 复用取号链的同一 HMAC（quotaKeyMACOfPlaintext），绝不重算另一份。
type cursorSourceFingerprint struct {
	QuotaKeyID int64
	CodeMAC    string
}

// valid 判定该指纹是否可作为"同源"的可靠依据：两个字段都必须来自真实来源行。
func (f cursorSourceFingerprint) valid() bool {
	return f.QuotaKeyID > 0 && strings.TrimSpace(f.CodeMAC) != ""
}

// newCursorAccountAuthProvider 为指定账号构造鉴权材料提供者。
func newCursorAccountAuthProvider(
	account *Account,
	quotaKeys QuotaKeyRepository,
	enc QuotaKeyEncryptor,
	deviceIDs CursorDeviceIDResolver,
) (*cursorAccountAuthProvider, error) {
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

	device, code, fp, err := cursorResolveOutboundAuthMaterial(ctx, p.quotaKeyID, p.quotaKeys, p.enc, p.deviceIDs)
	if err != nil {
		return "", err
	}

	// 复用 E1 的 wasm provider 生成 x-jg-auth：材料 = zk03({d: deviceId, a: code})。
	material, err := NewCursorWasmAuthProvider(device, code).CursorUpstreamAuth(ctx)
	if err != nil {
		return "", err
	}
	p.cached = material
	p.device = device
	// 与 cached 同一次赋值：指纹来自生成该头的那一次来源行/解密结果。
	p.fingerprint = fp
	return material, nil
}

// SourceFingerprint 返回生成当前 x-jg-auth 那一次固定的内部来源指纹。
//
// 材料尚未生成（未调用过 CursorUpstreamAuth）或指纹不可靠时返回 (零值, false)：
// 调用方据此**明确失败**，绝不以空值代表"同源"。
func (p *cursorAccountAuthProvider) SourceFingerprint() (cursorSourceFingerprint, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached == "" || !p.fingerprint.valid() {
		return cursorSourceFingerprint{}, false
	}
	return p.fingerprint, true
}

// cursorResolveOutboundAuthMaterial 解析单账号出站设备级材料的两个输入（device + 激活码），
// 并**从同一次来源行/解密结果**带出内部来源指纹（§25.17.3「A05首次来源指纹」）。
//
// 只查一次 quota_keys 行：deviceId 的持久化判定、激活码解密与指纹三者共用同一份行快照，
// 避免两次读之间被并发写穿造成"读了旧 device / 新 code"的错配，也不允许事后另读一次
// MAC 反推已生成的头。
func cursorResolveOutboundAuthMaterial(
	ctx context.Context,
	quotaKeyID int64,
	quotaKeys QuotaKeyRepository,
	enc QuotaKeyEncryptor,
	deviceIDs CursorDeviceIDResolver,
) (device, code string, fp cursorSourceFingerprint, err error) {
	if quotaKeys == nil || enc == nil {
		return "", "", cursorSourceFingerprint{}, ErrCursorQuotaKeyUnresolved
	}
	qk, err := quotaKeys.GetByID(ctx, quotaKeyID)
	if err != nil {
		// 错误信息绝不含激活码明文。
		return "", "", cursorSourceFingerprint{}, fmt.Errorf("cursor: load quota key %d: %w", quotaKeyID, err)
	}
	code, err = cursorDecryptQuotaKeyCode(qk, enc)
	if err != nil {
		return "", "", cursorSourceFingerprint{}, err
	}
	identity, err := cursorResolveOutboundDeviceIdentity(ctx, quotaKeys, deviceIDs, qk, code)
	if err != nil {
		return "", "", cursorSourceFingerprint{}, err
	}
	// identity.CodeMAC 是解析时刻由**同一份明文**经取号链同一 HMAC 算出的指纹，
	// 与上面这台设备即将据以生成的头同源，故可直接固定进会话 spec。
	keyID := quotaKeyID
	if qk != nil && qk.ID > 0 {
		keyID = qk.ID
	}
	fp = cursorSourceFingerprint{QuotaKeyID: keyID, CodeMAC: identity.CodeMAC}
	if !fp.valid() {
		return "", "", cursorSourceFingerprint{}, fmt.Errorf(
			"%w: source fingerprint unavailable for quota key %d", ErrCursorSourceFingerprintUnavailable, quotaKeyID)
	}
	return identity.DeviceID, code, fp, nil
}

// ErrCursorSourceFingerprintUnavailable 表示拿不到可靠的首次来源指纹（QuotaKeyID/MAC）。
//
// 失败关闭：调用方不得以空值代表"与本次鉴权同源"，也不得为此另建鉴权器或另读一次来源。
var ErrCursorSourceFingerprintUnavailable = errors.New("cursor: reliable source fingerprint unavailable (fail closed)")

// cursorDecryptQuotaKeyCode 解密 quota key 行的激活码明文（失败关闭、错误不含明文）。
func cursorDecryptQuotaKeyCode(qk *ent.QuotaKey, enc QuotaKeyEncryptor) (string, error) {
	if qk == nil || strings.TrimSpace(qk.CodeEncrypted) == "" {
		return "", fmt.Errorf("%w: quota key has no activation code", ErrCursorQuotaKeyUnresolved)
	}
	plain, err := enc.Decrypt([]byte(qk.CodeEncrypted))
	if err != nil {
		return "", fmt.Errorf("cursor: decrypt activation code: %w", err)
	}
	code := strings.TrimSpace(string(plain))
	if code == "" {
		return "", fmt.Errorf("%w: quota key decrypted to empty activation code", ErrCursorQuotaKeyUnresolved)
	}
	return code, nil
}

// cursorResolveOutboundDeviceID 解析出站 x-jg-auth 的 `d`（deviceId）。
//
// **与取号链（P1-B4）/ Probe 共用唯一三分支解析** resolveCursorDeviceIdentity（§25.14-C
// 「设备解析与依赖」），故同一激活码在 refresh/activate/lookup 与出站两条链上恒为同一设备
// 身份，且本函数不再保留独立分支本体（去重 W2-B2）：
//
//	① quota_keys.device_id 已持久化 → 用它（全链复用）；
//	② 未持久化且该码已持有账号（存量）→ 取号链同款派生值 deviceID(code)，
//	   **不生成、不落库、不迁移**；
//	③ 未持久化且无账号（新码）→ DeviceIDFor 生成并持久化，全链复用。
func cursorResolveOutboundDeviceID(
	ctx context.Context,
	quotaKeys QuotaKeyRepository,
	deviceIDs CursorDeviceIDResolver,
	qk *ent.QuotaKey,
	code string,
) (string, error) {
	identity, err := cursorResolveOutboundDeviceIdentity(ctx, quotaKeys, deviceIDs, qk, code)
	if err != nil {
		return "", err
	}
	return identity.DeviceID, nil
}

// cursorResolveOutboundDeviceIdentity 是上面三分支解析的本体：额外带出解析时刻的
// CodeMAC（同一次明文经取号链同一 HMAC），供首次来源指纹固定使用。
func cursorResolveOutboundDeviceIdentity(
	ctx context.Context,
	quotaKeys QuotaKeyRepository,
	deviceIDs CursorDeviceIDResolver,
	qk *ent.QuotaKey,
	code string,
) (cursorDeviceIdentity, error) {
	if quotaKeys == nil || qk == nil {
		return cursorDeviceIdentity{}, fmt.Errorf("%w: quota key row unavailable", ErrCursorQuotaKeyUnresolved)
	}
	// 三分支逐字等价地委托给唯一的 resolveCursorDeviceIdentity（① 持久值 / ② 存量派生
	// / ③ 真新码首写），出站语义不变。deviceIDs 仅留作签名兼容位，实际首写改由
	// resolveCursorDeviceIdentity 经 repo 同入口完成。
	identity, err := resolveCursorDeviceIdentity(ctx, quotaKeys, qk, code)
	if err != nil {
		return cursorDeviceIdentity{}, err
	}
	if strings.TrimSpace(identity.DeviceID) == "" {
		return cursorDeviceIdentity{}, fmt.Errorf("%w: quota key %d resolved to empty device id", ErrCursorDeviceIDUnavailable, qk.ID)
	}
	return identity, nil
}

// cursorResolveAccountBearer 取选中账号的用户级 Bearer（已存 access_token 明文）。
//
// 失败关闭：空凭证一律报错返回，绝不回落空串、绝不删账号对象或回收凭证。
func cursorResolveAccountBearer(account *Account) (string, error) {
	if account == nil {
		return "", fmt.Errorf("%w: no account", ErrCursorAccountBearerMissing)
	}
	token := strings.TrimSpace(account.GetCredential(cursorCredentialAccessToken))
	if token == "" {
		return "", fmt.Errorf("%w: account %d", ErrCursorAccountBearerMissing, account.ID)
	}
	return token, nil
}

// 编译期断言：本文件依赖 ent.QuotaKey 的 CodeEncrypted / DeviceID 字段（与取号链同源）。
var (
	_ = ent.QuotaKey{}.CodeEncrypted
	_ = ent.QuotaKey{}.DeviceID
)

// ---------------------------------------------------------------------------
// 流内拒绝：固定模板识别 + 终态错误构造
// ---------------------------------------------------------------------------

// cursorKeyRejectedTemplate 是上游在 HTTP200 正常流帧中回吐的激活码终态拒绝固定模板。
//
// 模板后跟动态后缀标识（形如 ` (1792327810746-28d0be39)`），故只按前缀匹配，
// 后缀原样保留进结构化字段供排障。
const cursorKeyRejectedTemplate = "密钥已过期或额度耗尽，请续费后使用"

// cursorStreamRejection 是流内拒绝的结构化证据（对应 A1 哨兵所附的同名字段）。
type cursorStreamRejection struct {
	// Snippet 是原始模板片段（含动态后缀标识），排障用，绝不含任何凭证。
	Snippet string
	// TurnID 是模板后括号内的动态标识，用于把同一次拒绝与上游侧记录对齐。
	TurnID string
}

// cursorDetectKeyRejectedUpstream 在**原始流字节**上做终态拒绝判据扫描。
func cursorDetectKeyRejectedUpstream(raw []byte) (cursorStreamRejection, bool) {
	text := string(raw)
	idx := strings.Index(text, cursorKeyRejectedTemplate)
	if idx < 0 {
		return cursorStreamRejection{}, false
	}
	end := idx + len(cursorKeyRejectedTemplate)
	limit := end + 64
	if limit > len(text) {
		limit = len(text)
	}
	snippet := strings.TrimSpace(text[idx:limit])
	return cursorStreamRejection{Snippet: snippet, TurnID: cursorRejectedTurnID(snippet)}, true
}

// cursorRejectedTurnID 取模板后的动态后缀标识（括号内）。
func cursorRejectedTurnID(snippet string) string {
	open := strings.Index(snippet, "(")
	if open < 0 {
		return ""
	}
	tail := snippet[open+1:]
	end := strings.Index(tail, ")")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(tail[:end])
}

// cursorHandleKeyRejected 处置"流内拒绝"：记 ops → 返回终态错误。
//
// 按 §25.14-C：流内固定拒绝只返回明确上游错误并记录安全阶段；**不再**发起流内
// lookup 核对，也**不再**修改来源期限/账号余额（那些只归唯一观测入口）。
func cursorHandleKeyRejected(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	rej cursorStreamRejection,
	upstreamURL string,
) error {
	cursorRecordUpstreamError(c, account, 0, "key_rejected", "上游流内拒绝："+rej.Snippet, upstreamURL)
	if terminal := cursorMatchKeyRejected(rej.Snippet); terminal != nil {
		return terminal
	}
	return &CursorKeyRejectedError{Template: rej.Snippet}
}

// ---------------------------------------------------------------------------
// 出站分派谓词
// ---------------------------------------------------------------------------

// ErrCursorProtocolBridgeUnavailable 表示 Cursor 原生协议 ↔ 入站形状的转换层
// **未装配**（执行部件或写出目标缺失）。保留该错误以兼容既有失败关闭断言。
var ErrCursorProtocolBridgeUnavailable = errors.New("cursor: protocol bridge to Cursor native endpoints is not wired (fail closed)")

// isCursorAccount 判定账号是否为 cursor 平台账号（分派前置谓词）。
func isCursorAccount(account *Account) bool {
	return account != nil && account.Platform == PlatformCursor
}

// ---------------------------------------------------------------------------
// 结果与观测
// ---------------------------------------------------------------------------

// cursorUsageToClaude 把 OpenAI 形状 usage 落到契约 0.1 的 ForwardResult.usage 字段。
//
// 桥接只做字段搬运，**不做任何折算、不补默认值**：用量由调用方 handler 的既有
// RecordUsage 链计费一次，执行层绝不二次计费。
func cursorUsageToClaude(u *OpenAIUsage) ClaudeUsage {
	if u == nil {
		return ClaudeUsage{}
	}
	return ClaudeUsage{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		ImageOutputTokens:        u.ImageOutputTokens,
	}
}

// cursorRecordUpstreamError 把上游错误记入 ops 事件（不含任何凭证）。
func cursorRecordUpstreamError(
	c *gin.Context, account *Account, status int, code, msg, upstreamURL string,
) {
	if c == nil {
		return
	}
	platform := ""
	accountID := int64(0)
	accountName := ""
	if account != nil {
		platform = account.Platform
		accountID = account.ID
		accountName = account.Name
	}
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:           platform,
		AccountID:          accountID,
		AccountName:        accountName,
		UpstreamStatusCode: status,
		UpstreamURL:        upstreamURL,
		Kind:               "http_error",
		Message:            msg,
		Detail:             code,
	})
}

// cursorRecordStreamError 把流内错误记入 ops 事件。
func cursorRecordStreamError(c *gin.Context, account *Account, err error, upstreamURL string) {
	if c == nil || err == nil {
		return
	}
	cursorRecordUpstreamError(c, account, 0, "", err.Error(), upstreamURL)
}

// 编译期断言：Usage 搬运不引入时间依赖（保留 time 引用以便后续阶段日志扩展）。
var _ = time.Second
