package service

// 本文件实现 cursor 平台的**出站鉴权按账号解析**与**出站分派挂载**（E2 卡接线骨架；
// P1-A2 重写鉴权形态与流内拒绝处置）。
//
// E1（cursor_upstream_forward.go / cursor_upstream_nodes.go）已提供出站原语：
// 换 host 到 shareai 号池中转节点 + 注入 x-jg-auth 鉴权头，请求体与流式帧原样透传。
// 本文件只做接线，不重复实现 E1 的任何机制。
//
// ════════════════════════════════════════════════════════════════════════════
// P1-A2 出站鉴权形态（方案 §15.2-1、§16.1，证据 103 / 134 / 135）：
//
//	Authorization: Bearer <账号已存 access_token>
//	x-jg-auth:     wasm zk03({d: deviceId, a: 激活码})
//
//  1. **Standard 保留账号 authorization（已存 access_token）**，同时携带 x-jg-auth；
//     绝不把激活码当 OAuth code 去换 session token（旧链已删，见合同 §4），
//     绝不跨账号复用任何会话材料。
//  2. **deviceId 与取号链同源三分支**（与 P1-B4 同判据、同结果）：
//     ① 已持久化 device_id → 用它；② 未持久化且该码已持有账号（存量）→ 取号链同款
//     派生值 deviceID(code)，**不生成、不落库**；③ 未持久化且无账号（新码）→
//     DeviceIDFor 生成并持久化，全链复用。
//  3. **无 Bearer 样本成功不构成授权**：账号凭证缺失时失败关闭并明确报错，
//     绝不删账号对象、绝不回收 Bearer（I2 红线）。
//
// 流内拒绝（HTTP200 正常流帧中出现固定模板「密钥已过期或额度耗尽，请续费后使用」）
// 一律按终态错误返回：**不得**当成功、**不得**伪造 usage；随后经 lookup 核对，
// 仅当 lookup 确认「密钥已过期」才走既有 key 状态写入口标 expired，
// lookup 无法确认时按上游失败返回（不删号、不标账号耗尽）。
// ════════════════════════════════════════════════════════════════════════════
//
// 本卡硬红线（与 E1/E2 一致，不得放宽）：
//  1. 鉴权材料**按账号**解析（quota_key_id → 解密激活码 + 同源 deviceId → wasm provider），
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
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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
// 拒绝了本次出站请求。
//
// 该哨兵原先随 cursor_auth_token.go（旧 session 链）一并定义；P1-A2 删除旧链整文件后
// 把哨兵留在转发侧——消费方（cursor_protocol_response.go 的错误分类与既有失败关闭
// 断言）只认这个身份，语义与取值均不得变。旧链专有哨兵
// ErrCursorSessionTokenUnavailable 则随旧链一并删除，全树无残留引用。
var ErrCursorRelayAuthRejected = errors.New("cursor: relay rejected the request with an auth error")

// ErrCursorKeyRejectedTerminal 表示上游在 HTTP200 正常流帧中回以激活码终态拒绝模板
// （「密钥已过期或额度耗尽，请续费后使用」+ 动态后缀标识）。
//
// 判据与 A1 交付的哨兵 ErrCursorKeyRejectedUpstream 同源（同一固定模板、同一触发条件）；
// 本卡在解析前对流内原始字节做同判据扫描，故不依赖 A1 侧符号即可终态返回。
// 命中即终态错误：**不得**当成功、**不得**伪造 usage。
// ErrCursorKeyRejectedTerminal 整合后统一到 A1 契约哨兵（见 cursor_protocol_response.go）
var ErrCursorKeyRejectedTerminal = ErrCursorKeyRejectedUpstream // alias（errors.Is 双哨兵均成立）

// cursorAccountAuthProvider 为**单个账号**提供 x-jg-auth 鉴权材料。
//
// 绑定该账号的 quota key ID：每次取材料时按 ID 取回 quota_keys 行、解密激活码，
// 并按同源三分支解析 deviceId（见 cursorResolveOutboundDeviceID），再交 E1 的 wasm
// provider 合成材料。材料在 provider 内部缓存，故稳态下每账号每次对话只跑一次 wasm。
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

	device, code, err := cursorResolveOutboundAuthMaterial(ctx, p.quotaKeyID, p.quotaKeys, p.enc, p.deviceIDs)
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
	return material, nil
}

// cursorResolveOutboundAuthMaterial 解析单账号出站设备级材料的两个输入（device + 激活码）。
//
// 只查一次 quota_keys 行：deviceId 的持久化判定与激活码解密共用同一份行快照，避免
// 两次读之间被并发写穿造成"读了旧 device / 新 code"的错配。
func cursorResolveOutboundAuthMaterial(
	ctx context.Context,
	quotaKeyID int64,
	quotaKeys QuotaKeyRepository,
	enc QuotaKeyEncryptor,
	deviceIDs CursorDeviceIDResolver,
) (device, code string, err error) {
	if quotaKeys == nil || enc == nil {
		return "", "", ErrCursorQuotaKeyUnresolved
	}
	qk, err := quotaKeys.GetByID(ctx, quotaKeyID)
	if err != nil {
		// 错误信息绝不含激活码明文。
		return "", "", fmt.Errorf("cursor: load quota key %d: %w", quotaKeyID, err)
	}
	code, err = cursorDecryptQuotaKeyCode(qk, enc)
	if err != nil {
		return "", "", err
	}
	device, err = cursorResolveOutboundDeviceID(ctx, quotaKeys, deviceIDs, qk, code)
	if err != nil {
		return "", "", err
	}
	return device, code, nil
}

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
// **与取号链（P1-B4）同源三分支、同结果**，故同一激活码在 refresh/activate/lookup
// 与出站两条链上恒为同一设备身份：
//
//	① quota_keys.device_id 已持久化 → 用它（全链复用）；
//	② 未持久化且该码已持有账号（存量）→ 取号链同款派生值 deviceID(code)，
//	   **不生成、不落库、不迁移**（§16 红线：存量先核对语义，绝不自动改设备身份）；
//	③ 未持久化且无账号（新码）→ DeviceIDFor 生成并持久化，全链复用。
//
// 分支②是本卡的关键修正：旧实现在出站侧无条件调用 DeviceIDFor，会给存量码**凭空
// 生成并落库一份与取号链不同的随机 device**（证据 124：站点存量码的 device_source
// 恒为 existing_site_pull_derived_no_persistence），造成同一码两链设备身份分裂。
//
// 存量判定直接复用既有仓储契约 ListAccountsByQuotaKey（与 B4 的 HasAccount 同输入同语义），
// 不引入新的装配依赖。查询失败即失败关闭：绝不带着猜测的设备身份出站。
func cursorResolveOutboundDeviceID(
	ctx context.Context,
	quotaKeys QuotaKeyRepository,
	deviceIDs CursorDeviceIDResolver,
	qk *ent.QuotaKey,
	code string,
) (string, error) {
	if quotaKeys == nil || qk == nil {
		return "", fmt.Errorf("%w: quota key row unavailable", ErrCursorQuotaKeyUnresolved)
	}
	// ① 已持久化：全链复用（含取号链，见 B4 resolveDeviceID 分支①）。
	if qk.DeviceID != nil && strings.TrimSpace(*qk.DeviceID) != "" {
		return strings.TrimSpace(*qk.DeviceID), nil
	}
	accounts, err := quotaKeys.ListAccountsByQuotaKey(ctx, qk.ID)
	if err != nil {
		return "", fmt.Errorf("%w: list accounts for quota key %d: %v", ErrCursorQuotaKeyUnresolved, qk.ID, err)
	}
	// ② 存量（已持有账号）：保持派生值，与取号链逐字同值，绝不生成新设备。
	if len(accounts) > 0 {
		return deviceID(code), nil
	}
	// ③ 新码：与取号链同一 DeviceIDFor 入口生成并持久化，保证两链同值。
	if deviceIDs == nil {
		return "", fmt.Errorf("%w: quota key %d has no device id resolver", ErrCursorDeviceIDUnavailable, qk.ID)
	}
	resolved, err := deviceIDs.DeviceIDFor(ctx, qk.ID)
	if err != nil {
		return "", fmt.Errorf("cursor: resolve device id for quota key %d: %w", qk.ID, err)
	}
	if strings.TrimSpace(resolved) == "" {
		return "", fmt.Errorf("%w: quota key %d resolved to empty device id", ErrCursorDeviceIDUnavailable, qk.ID)
	}
	return resolved, nil
}

// cursorResolveAccountBearer 取选中账号的用户级 Bearer（已存 access_token 明文）。
//
// 失败关闭：空凭证一律报错返回，绝不回落空串、绝不删账号对象或回收凭证
// （证据 135 结论第 2 条：无 Bearer 成功只是"上游未拒绝"，不构成本站放行依据）。
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
// 流内拒绝：固定模板识别 + lookup 核对链
// ---------------------------------------------------------------------------

// cursorKeyRejectedTemplate 是上游在 HTTP200 正常流帧中回吐的激活码终态拒绝固定模板
// （证据 125-expiry-recheck-254 / 134-current-token-expired-key 逐字样本）。
//
// 模板后跟动态后缀标识（形如 ` (1792327810746-28d0be39)`），故只按前缀匹配，
// 后缀原样保留进结构化字段供排障。
const cursorKeyRejectedTemplate = "密钥已过期或额度耗尽，请续费后使用"

// cursorStreamRejection 是流内拒绝的结构化证据（对应 A1 哨兵所附的同名字段）。
type cursorStreamRejection struct {
	// Snippet 是原始模板片段（含动态后缀标识），排障用，绝不含任何凭证。
	Snippet string
	// TurnID 是模板后括号内的动态标识（证据样本：1792327810746-28d0be39），
	// 用于把同一次拒绝与上游侧记录对齐；解析不出时为空。
	TurnID string
}

// cursorDetectKeyRejectedUpstream 在**原始流字节**上做终态拒绝判据扫描。
//
// 为什么扫原始字节而不是等解析器报错：判据是"HTTP200 正常流帧中出现固定模板"，
// 而模板以明文出现在 text 帧里——扫原始字节是判据的超集，且在解析之前即可终态返回，
// 天然满足"不得当成功、不得伪造 usage"。A1 落地哨兵后，本判据与其等价且取道更早。
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

// cursorKeyStatus 是 lookup 对激活码终态的判定结果。
type cursorKeyStatus int

const (
	// cursorKeyStatusUnknown 表示"无法确认"（传输失败 / 回包不可解析 / 语义不在已取证范围）。
	// 绝不据此删号、绝不据此标账号耗尽。
	cursorKeyStatusUnknown cursorKeyStatus = iota
	// cursorKeyStatusActive 表示 lookup 确认码仍在用（code=0 且带 key_info）。
	cursorKeyStatusActive
	// cursorKeyStatusExpired 表示 lookup 确认终态（code=1 + 密钥已过期）。
	cursorKeyStatusExpired
)

// cursorKeyStatusLookup 是"流内拒绝后核对激活码终态"的窄依赖。
//
// 生产实现复用既有 lookup 入口（CursorAutoEntryService 的 lookupQuota，
// cursor_auto_entry_service.go），不新造第二条查询链；测试可注入替身。
type cursorKeyStatusLookup interface {
	LookupKeyStatus(ctx context.Context, quotaKeyID int64) (cursorKeyStatus, error)
}

// SetCursorKeyStatusLookup 注入激活码终态核对入口（装配/测试用）。
//
// 未注入时走 cursorKeyStatusLookupOrDefault 的同包回退；都取不到即视为"无法确认"。
func (s *OpenAIGatewayService) SetCursorKeyStatusLookup(l cursorKeyStatusLookup) {
	if s == nil {
		return
	}
	s.cursorKeyStatusLookup = l
}

// cursorKeyStatusLookupOrDefault 取激活码终态核对入口。
//
// 回退路径说明（同包可见，不新增装配点、不改 wire_gen）：生产经 SetCursorAuthDeps
// 注入的 deviceIDs 就是 *QuotaKeyService（wire_gen 一行注入，见该函数注释），而其
// autoEntry 正是既有 *CursorAutoEntryService（wire 另装配的 SetAutoEntry）。故此处
// 取回既有实例复用其 lookup 入口；任一层断裂 → 返回 nil → 按"无法确认"处理（失败关闭）。
func (s *OpenAIGatewayService) cursorKeyStatusLookupOrDefault() cursorKeyStatusLookup {
	if s == nil {
		return nil
	}
	if s.cursorKeyStatusLookup != nil {
		return s.cursorKeyStatusLookup
	}
	if s.cursorQuotaKeys == nil || s.cursorQuotaKeyEnc == nil {
		return nil
	}
	qks, ok := s.cursorDeviceIDs.(*QuotaKeyService)
	if !ok || qks == nil || qks.autoEntry == nil {
		return nil
	}
	autoEntry, ok := qks.autoEntry.(*CursorAutoEntryService)
	if !ok || autoEntry == nil {
		return nil
	}
	return &cursorAutoEntryKeyStatusLookup{
		svc:       autoEntry,
		quotaKeys: s.cursorQuotaKeys,
		enc:       s.cursorQuotaKeyEnc,
		deviceIDs: s.cursorDeviceIDs,
	}
}

// cursorAutoEntryKeyStatusLookup 以既有 lookup 入口核对激活码终态。
//
// 复用 CursorAutoEntryService.lookupQuota 而不是 ProbeKeyQuota：核对只需终态判定，
// 不应附带"把配额快照写进每个账号 extra"的副作用，也不该用其随机 device-id 源
// （证据 124：站点存量码的 lookup 设备源是取号链派生值）。
type cursorAutoEntryKeyStatusLookup struct {
	svc       *CursorAutoEntryService
	quotaKeys QuotaKeyRepository
	enc       QuotaKeyEncryptor
	deviceIDs CursorDeviceIDResolver
}

// LookupKeyStatus 实现 cursorKeyStatusLookup。
func (l *cursorAutoEntryKeyStatusLookup) LookupKeyStatus(ctx context.Context, quotaKeyID int64) (cursorKeyStatus, error) {
	qk, err := l.quotaKeys.GetByID(ctx, quotaKeyID)
	if err != nil {
		return cursorKeyStatusUnknown, fmt.Errorf("cursor: load quota key %d: %w", quotaKeyID, err)
	}
	code, err := cursorDecryptQuotaKeyCode(qk, l.enc)
	if err != nil {
		return cursorKeyStatusUnknown, err
	}
	device, err := cursorResolveOutboundDeviceID(ctx, l.quotaKeys, l.deviceIDs, qk, code)
	if err != nil {
		return cursorKeyStatusUnknown, err
	}
	if _, err := l.svc.lookupQuota(ctx, code, device); err != nil {
		// 证据 49 / 124：code=1 msg=密钥已过期 ⇒ 激活码级终态。
		// 其余（限频措辞、未知回包、传输失败）一律"无法确认"，绝不判过期。
		if strings.Contains(err.Error(), "密钥已过期") {
			return cursorKeyStatusExpired, nil
		}
		return cursorKeyStatusUnknown, err
	}
	return cursorKeyStatusActive, nil
}

// cursorMarkKeyExpiredByUpstream 复用 quota_key_service.go 的既有条件写入口
// （markKeyExpiredByUpstream）把上游确认终态的激活码标 expired。
//
// 口径与 sweeper 同型：仅当 DB 当前 status='active' 才写入，不覆盖 sweep 已置状态，
// 且**不写任何由 msg 推导的日期**（expires_at 保持原值）。本卡不新增第二套写链；
// 绝不软删账号、绝不标账号耗尽（I2 红线）——S 卡消费该状态作为调度资格。
//
// 入参 devices 在生产即 *QuotaKeyService（SetCursorAuthDeps 注入），故以类型断言取回
// 既有实例；断言失败或 ent client 缺失 → 返回错误（调用方按"未确认"处理）。
func cursorMarkKeyExpiredByUpstream(
	ctx context.Context,
	devices CursorDeviceIDResolver,
	quotaKeyID int64,
	reason error,
) error {
	qks, ok := devices.(*QuotaKeyService)
	if !ok || qks == nil || qks.client == nil {
		return fmt.Errorf("%w: quota key expiry writer unavailable", ErrCursorQuotaKeyUnresolved)
	}
	tx, err := qks.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("cursor: begin quota key expiry tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := qks.markKeyExpiredByUpstream(ctx, tx, quotaKeyID, reason); err != nil {
		return fmt.Errorf("cursor: mark quota key %d expired: %w", quotaKeyID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("cursor: commit quota key expiry: %w", err)
	}
	committed = true
	return nil
}

// cursorHandleKeyRejected 处置"流内拒绝"：记 ops → lookup 核对 → 终态错误返回。
//
// 三条硬口径：
//  1. 无论 lookup 结果如何，本函数**只返回错误**（终态），绝不返回任何 usage、
//     绝不把该流当成功——上游这段响应里没有可计费的产出。
//  2. 仅当 lookup 明确返回「密钥已过期」才标 expired（走既有写口）；
//  3. lookup 无法确认（无入口 / 报错 / 非过期语义）→ 按上游失败返回，
//     不删号、不标账号耗尽。
func (s *OpenAIGatewayService) cursorHandleKeyRejected(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	rej cursorStreamRejection,
	upstreamURL string,
) error {
	// ops 上报：只含模板片段（已脱敏：片段内不含任何凭证）。
	s.cursorRecordUpstreamError(c, account, 0, "key_rejected", "上游流内拒绝："+rej.Snippet, upstreamURL)

	terminal := cursorMatchKeyRejected(rej.Snippet)
	if terminal == nil {
		terminal = &CursorKeyRejectedError{Template: rej.Snippet}
	}

	lookup := s.cursorKeyStatusLookupOrDefault()
	if lookup == nil || account == nil || account.QuotaKeyID == nil {
		return terminal
	}
	status, err := lookup.LookupKeyStatus(ctx, *account.QuotaKeyID)
	if err != nil || status != cursorKeyStatusExpired {
		// 无法确认 / 未确认过期 → 上游失败返回，不删号、不标耗尽。
		return terminal
	}
	reason := fmt.Errorf("cursor: upstream confirmed key expired: %s", rej.Snippet)
	if werr := cursorMarkKeyExpiredByUpstream(ctx, s.cursorDeviceIDs, *account.QuotaKeyID, reason); werr != nil {
		// 写失败只上报，不改变"本次请求终态失败"的结论。
		s.cursorRecordUpstreamError(c, account, 0, "key_expired_write_failed", werr.Error(), upstreamURL)
	}
	return terminal
}

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
// deviceIDs 用于解析出站 deviceId（同源三分支）；生产装配注入 *QuotaKeyService 即可
// （同实例亦承担激活码终态写链，见 cursorMarkKeyExpiredByUpstream）。
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
// P1-A2 起本函数五步（任一步失败即失败关闭，绝不降级）：
//
//	① 模型闸门：账号 model_mapping 必须**显式命中**
//	② 用户级鉴权：账号已存 access_token（Bearer），缺失即失败关闭
//	③ 设备级鉴权：x-jg-auth = zk03({d: 同源 deviceId, a: 激活码})
//	④ 请求转换：OpenAI → StreamUnifiedChatRequestWithTools（protobuf + envelope）
//	⑤ 响应转换：envelope 帧流 → OpenAI SSE（或单条 chat.completion），
//	   并先做流内拒绝判据扫描（命中即终态错误，不产出 usage）
//
// 对外统一走包级入口 CursorForwardInbound（cursor_bridge.go，契约 0.1）：本函数是
// 其内部实现，不是跨卡接口。
func (s *OpenAIGatewayService) forwardCursor(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	reqStream bool,
	startTime time.Time,
) (*ForwardResult, error) {
	if !isCursorAccount(account) {
		return nil, errors.New("cursor: forwardCursor requires a cursor platform account")
	}

	// §18.5 T 阶段日志：选中账号即记 account_selected（沿用现有 handler 事件口径）。
	cursorPhaseLog(ctx, zapcore.DebugLevel, "account_selected", originalModel,
		zap.Int64("account_id", account.ID))
	// 客户端取消贯穿整条链路：在函数返回前统一补一条 context_cancel 终态事件，
	// 不依赖特定返回路径（httptrace/DNS/读取均可能先行报错）。
	defer func() {
		if ctx.Err() != nil {
			cursorPhaseLog(ctx, zapcore.InfoLevel, "context_cancel", originalModel,
				zap.String("outcome", cursorHTTPErrClass(ctx.Err())))
		}
	}()

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
	// 模型闸门通过：admission_done（调度/映射准入完成，进入出站阶段）。
	cursorPhaseLog(ctx, zapcore.DebugLevel, "admission_done", upstreamModel,
		zap.Int64("account_id", account.ID))

	fwd := s.cursorForwarderOrDefault()
	if fwd == nil {
		return nil, ErrCursorProtocolBridgeUnavailable
	}

	// ② 用户级 Bearer（账号已存 access_token）：缺失即失败关闭，绝不空凭证出站。
	// 鉴权前后各记一次事件，避免把鉴权/WASM 等待误归到"建连"阶段。
	cursorPhaseLog(ctx, zapcore.DebugLevel, "auth_begin", upstreamModel,
		zap.Int64("account_id", account.ID))
	bearer, err := cursorResolveAccountBearer(account)
	if err != nil {
		return nil, err
	}

	// ③ 设备级鉴权材料（x-jg-auth）：按账号解析，材料不可得即失败关闭。
	authProvider, err := newCursorAccountAuthProvider(account, s.cursorQuotaKeys, s.cursorQuotaKeyEnc, s.cursorDeviceIDs)
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

	if _, err := authProvider.CursorUpstreamAuth(ctx); err != nil {
		return nil, err
	}
	cursorPhaseLog(ctx, zapcore.DebugLevel, "auth_done", upstreamModel,
		zap.Int64("account_id", account.ID))
	cursorPhaseLog(ctx, zapcore.DebugLevel, "convert_done", upstreamModel)
	cursorPhaseLog(ctx, zapcore.InfoLevel, "agent_run_begin", upstreamModel)

	fwdReq := &CursorUpstreamRequest{
		Path: cursorPathAgentRun,
		Body: payload,
		// 账号级鉴权材料：转发器是进程级共享的，材料必须逐请求携带，
		// 否则会把 A 号的 x-jg-auth 发给 B 号（方案 §2 E1 遗留风险登记）。
		AuthProvider: authProvider,
		Model:        upstreamModel,
		Headers: map[string]string{
			"Content-Type":             cursorConnectContentType,
			"Connect-Protocol-Version": "1",
			// 用户级身份：账号已存 access_token（**不是**换来的 session token，
			// 也不是 x-jg-auth 的激活码材料）。两个维度都要，缺一即失败关闭。
			"Authorization":           "Bearer " + bearer,
			"x-cursor-client-version": cursorClientVersion,
			"x-cursor-checksum":       cursorClientChecksum(authProvider.device, time.Now()),
		},
	}

	// ════════════════════════════════════════════════════════════════════════════
	// O11-C1：补齐客户端上下文头（工具请求形态对齐）。
	//
	// 生产实测：grok-4.7 带 tools 的请求在 first_frame 后挂起无后续（request_id
	// 74d9673a，180s 超时）；同账号同节点同 payload 字段的活体探针（17 项头）5-15s
	// 完成工具往返。两者唯一字面差异 = 转发头——本处只发 6 项，缺客户端上下文头时
	// 带 mcp_tools 的上游请求挂起（纯文本请求不受影响）。
	//
	// 下列 11 项头逐字取活体全通探针 headers()（除 x-request-id 与 x-jg-auth 已由
	// 鉴权注入），是工具请求被上游受理的必需形态。值恒定（客户端身份/环境常量），
	// 不依赖任何请求级上下文。既不改动既有 6 项的值、鉴权注入、payload 编码、duplex、
	// 协议转换，也不引入请求级动态值。
	// ════════════════════════════════════════════════════════════════════════════
	fwdReq.Headers["x-cursor-client-type"] = "ide"
	fwdReq.Headers["x-cursor-client-layout"] = "unifiedAgent"
	fwdReq.Headers["x-cursor-client-os"] = "linux"
	fwdReq.Headers["x-cursor-client-arch"] = "x64"
	fwdReq.Headers["x-cursor-client-device-type"] = "desktop"
	fwdReq.Headers["x-ghost-mode"] = "true"
	fwdReq.Headers["x-new-onboarding-completed"] = "false"
	fwdReq.Headers["x-cursor-timezone"] = "Asia/Shanghai"
	fwdReq.Headers["x-cursor-streaming"] = "true"
	fwdReq.Headers["x-jg-net-optimize"] = "http2"
	fwdReq.Headers["user-agent"] = "connect-es/1.6.1"

	resp, err := fwd.Do(ctx, fwdReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		upstreamBody, rerr := readCursorUpstreamBody(resp.Body, cursorAuthResponseMaxBytes)
		if rerr != nil {
			return nil, cursorUpstreamStatusError(resp.StatusCode, nil, bearer)
		}
		// bearer 作为 secret 传入：上游错误文案可能回显 Authorization 头的值，
		// 不抹掉就会把账号会话令牌写进 ops 事件与客户端回传。
		code, msg := cursorExtractUpstreamErrorSummary(upstreamBody, bearer)
		s.cursorRecordUpstreamError(c, account, resp.StatusCode, code, msg, resp.TargetURL)
		return nil, cursorUpstreamStatusError(resp.StatusCode, upstreamBody, bearer)
	}

	if reqStream {
		stream := &cursorSSEStream{source: resp.Body, parser: cursorNewStreamParser(originalModel, bearer)}
		httpResp := cursorToHTTPResponse(resp, http.StatusOK, "text/event-stream", nil)
		httpResp.Body = stream
		streamResult, herr := s.handleStreamingResponse(ctx, httpResp, c, account, startTime, originalModel, upstreamModel)
		if streamErr := stream.terminalError(); streamErr != nil {
			var rejected *CursorKeyRejectedError
			if errors.As(streamErr, &rejected) {
				return nil, s.cursorHandleKeyRejected(ctx, c, account, cursorStreamRejection{Snippet: rejected.Template, TurnID: rejected.TurnID}, resp.TargetURL)
			}
			s.cursorRecordStreamError(c, account, streamErr, resp.TargetURL)
			return nil, streamErr
		}
		if herr != nil {
			return nil, herr
		}
		// 流式写回完成（桥接最终 HTTP 写出）：终态成功事件。
		cursorPhaseLog(ctx, zapcore.InfoLevel, "response_write_done", upstreamModel,
			zap.String("outcome", "ok"), zap.Bool("stream", true))
		return cursorForwardResultFromStreaming(streamResult, originalModel, upstreamModel, startTime, httpResp.Header), nil
	}

	rawBody, err := readCursorUpstreamBody(resp.Body, cursorUpstreamStreamMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("cursor: read upstream stream: %w", err)
	}
	if rej, rejected := cursorDetectKeyRejectedUpstream(rawBody); rejected {
		cursorPhaseLog(ctx, zapcore.WarnLevel, "response_parse_error", upstreamModel,
			zap.String("outcome", "key_rejected"))
		return nil, s.cursorHandleKeyRejected(ctx, c, account, rej, resp.TargetURL)
	}

	completion, perr := cursorParseStreamToCompletion(rawBody, originalModel, bearer)
	if perr != nil {
		cursorPhaseLog(ctx, zapcore.WarnLevel, "response_parse_error", upstreamModel,
			zap.String("outcome", "parse_error"))
		s.cursorRecordStreamError(c, account, perr, resp.TargetURL)
		return nil, perr
	}
	cursorPhaseLog(ctx, zapcore.InfoLevel, "response_parse_done", upstreamModel,
		zap.String("outcome", "ok"), zap.Bool("stream", false))
	httpResp := cursorToHTTPResponse(resp, http.StatusOK, "application/json", completion)
	nonStreamResult, herr := s.handleNonStreamingResponse(ctx, httpResp, c, account, originalModel, upstreamModel)
	if herr != nil {
		return nil, herr
	}
	// 非流式写回完成：终态成功事件（与流式 response_write_done 同口径）。
	cursorPhaseLog(ctx, zapcore.InfoLevel, "response_write_done", upstreamModel,
		zap.String("outcome", "ok"), zap.Bool("stream", false))
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

// cursorUsageToClaude 把 OpenAI 形状 usage 落到契约 0.1 的 ForwardResult.usage 字段。
//
// 桥接只做字段搬运，**不做任何折算、不补默认值**：用量由调用方 handler 的既有
// RecordUsage 链计费一次，桥内绝不二次计费。
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

// cursorForwardResultFromStreaming 由流式结果构造契约 0.1 的转发结果。
func cursorForwardResultFromStreaming(
	r *openaiStreamingResult, originalModel, upstreamModel string, startTime time.Time, respHeader http.Header,
) *ForwardResult {
	if r == nil {
		return &ForwardResult{
			Model: originalModel, UpstreamModel: upstreamModel, Stream: true,
			Duration: time.Since(startTime),
		}
	}
	return &ForwardResult{
		UpstreamHeaders:  respHeader,
		RequestID:        strings.TrimSpace(r.responseID),
		Usage:            cursorUsageToClaude(r.usage),
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		Stream:           true,
		Duration:         time.Since(startTime),
		FirstTokenMs:     r.firstTokenMs,
		SearchCount:      r.searchCount,
		ImageCount:       r.imageCount,
		ImageOutputSizes: r.imageOutputSizes,
	}
}

// cursorForwardResultFromNonStreaming 由非流式结果构造契约 0.1 的转发结果。
func cursorForwardResultFromNonStreaming(
	r *openaiNonStreamingResult, originalModel, upstreamModel string, startTime time.Time, respHeader http.Header,
) *ForwardResult {
	if r == nil {
		return &ForwardResult{
			Model: originalModel, UpstreamModel: upstreamModel, Stream: false,
			Duration: time.Since(startTime),
		}
	}
	return &ForwardResult{
		UpstreamHeaders:  respHeader,
		RequestID:        strings.TrimSpace(r.responseID),
		Usage:            cursorUsageToClaude(r.OpenAIUsage),
		Model:            originalModel,
		UpstreamModel:    upstreamModel,
		Stream:           false,
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
