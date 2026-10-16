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
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/Wei-Shaw/sub2api/ent"
)

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
// 再交 E1 的 wasm provider 生成材料。材料在 provider 内部缓存（E1 已实现），
// 故稳态下每账号每次对话只跑一次 wasm。
type cursorAccountAuthProvider struct {
	quotaKeyID int64
	quotaKeys  QuotaKeyRepository
	enc        QuotaKeyEncryptor

	// mu 保护 cached 的读写。缓存键即本 provider 绑定的**单一账号**，故不存在
	// 跨账号串号风险；激活码轮换会换新的 quota key 行，由账号实例自然隔离。
	mu     sync.Mutex
	cached string
}

// newCursorAccountAuthProvider 为指定账号构造鉴权材料提供者。
//
// quotaKeyID 取自账号自身的 QuotaKeyID；quotaKeys / enc 为 nil 时构造失败，
// 禁止静默回落明文（与 cursor_upstream_client.go 同口径）。
func newCursorAccountAuthProvider(account *Account, quotaKeys QuotaKeyRepository, enc QuotaKeyEncryptor) (CursorUpstreamAuthProvider, error) {
	if account == nil || account.QuotaKeyID == nil || *account.QuotaKeyID <= 0 {
		return nil, ErrCursorQuotaKeyUnresolved
	}
	if quotaKeys == nil || enc == nil {
		return nil, errors.New("cursor: quota key repository and encryptor are required")
	}
	return &cursorAccountAuthProvider{
		quotaKeyID: *account.QuotaKeyID,
		quotaKeys:  quotaKeys,
		enc:        enc,
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

	qk, err := p.quotaKeys.GetByID(ctx, p.quotaKeyID)
	if err != nil {
		// 错误信息绝不含激活码明文。
		return "", fmt.Errorf("cursor: load quota key %d: %w", p.quotaKeyID, err)
	}
	if qk == nil || strings.TrimSpace(qk.CodeEncrypted) == "" {
		return "", fmt.Errorf("%w: quota key %d has no activation code", ErrCursorQuotaKeyUnresolved, p.quotaKeyID)
	}
	plain, err := p.enc.Decrypt([]byte(qk.CodeEncrypted))
	if err != nil {
		return "", fmt.Errorf("cursor: decrypt activation code: %w", err)
	}
	code := strings.TrimSpace(string(plain))
	if code == "" {
		return "", fmt.Errorf("%w: quota key %d decrypted to empty activation code", ErrCursorQuotaKeyUnresolved, p.quotaKeyID)
	}

	// 复用 E1 的 wasm provider 生成 x-jg-auth（device-id 由激活码确定性派生）。
	material, err := NewCursorWasmAuthProvider(code).CursorUpstreamAuth(ctx)
	if err != nil {
		return "", err
	}
	p.cached = material
	return material, nil
}

// 编译期断言：本文件依赖 ent.QuotaKey 的 CodeEncrypted 字段（与 E1 取号链同源）。
var _ = ent.QuotaKey{}.CodeEncrypted

// ---------------------------------------------------------------------------
// 出站分派
// ---------------------------------------------------------------------------

// ErrCursorProtocolBridgeUnavailable 表示 Cursor 原生协议 ↔ OpenAI 入站形状的转换层
// 尚未实现，故 cursor 转发一律失败关闭。这是 E2 卡的边界，不是缺陷：转换层需要
// Cursor 原生请求体构造规则与私有 SSE 帧语义，二者均未取证（见本文件末尾说明）。
var ErrCursorProtocolBridgeUnavailable = errors.New("cursor: protocol bridge to Cursor native endpoints is not implemented yet (fail closed)")

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
func (s *OpenAIGatewayService) SetCursorAuthDeps(quotaKeys QuotaKeyRepository, enc QuotaKeyEncryptor) {
	if s == nil {
		return
	}
	s.cursorQuotaKeys = quotaKeys
	s.cursorQuotaKeyEnc = enc
}

// forwardCursor 是 cursor 平台的转发入口，挂在 OpenAIGatewayService 的 platform 分支上
// （与 forwardCodeBuddy 同构：均在通用 OpenAI 路径**之前**分流）。
//
// 必须先于通用路径分流，原因与 codebuddy 同：cursor 上游是 Cursor **原生**协议端点
// （/aiserver.v1.AIServiceService/*，protobuf-JSON 请求体 + 私有 SSE 帧），与 OpenAI
// Chat Completions 形状不兼容。落到通用路径会把 OpenAI 形状的 body 发到 Cursor 端点，
// 必然被上游拒绝，且白耗 shareai 额度。
//
// 本函数当前**失败关闭**：Cursor 原生协议与 OpenAI 入站形状之间的转换层尚未实现。
// 它在此之前完成两件必要的事——模型闸门与鉴权材料按账号预解析——使转换层落地后
// 只需补桥接逻辑，且任何情况下都不会发出无鉴权或形状错误的请求。
func (s *OpenAIGatewayService) forwardCursor(
	ctx context.Context,
	account *Account,
	originalModel string,
) error {
	if !isCursorAccount(account) {
		return errors.New("cursor: forwardCursor requires a cursor platform account")
	}

	// 模型闸门：cursor 无内置模型目录，模型完全来自账号 model_mapping。
	//
	// 这里要求 ResolveMappedModel 的 matched=true（账号映射**显式命中**该模型），而不是
	// 只看 GetMappedModel 的返回值——后者在映射为空/未命中时会回落到请求模型名本身，
	// 那样闸门形同虚设，会把任意 OpenAI 模型名发往 Cursor 原生端点（必然被上游拒绝
	// 并白耗 shareai 额度）。空映射账号已在 IsModelSupported 失败关闭，此处是纵深防御。
	upstreamModel, matched := account.ResolveMappedModel(originalModel)
	if !matched || strings.TrimSpace(upstreamModel) == "" {
		return fmt.Errorf("cursor: account %d does not map model %q (fail closed)", account.ID, originalModel)
	}

	// 鉴权材料按账号解析（quota_key_id → 解密激活码 → wasm），此处预热：
	// 材料不可得即失败关闭，绝不发出无鉴权请求去白耗 shareai 额度。
	provider, err := newCursorAccountAuthProvider(account, s.cursorQuotaKeys, s.cursorQuotaKeyEnc)
	if err != nil {
		return err
	}
	if _, aerr := provider.CursorUpstreamAuth(ctx); aerr != nil {
		return fmt.Errorf("%w: %v", ErrCursorAuthMaterialUnavailable, aerr)
	}

	// 转换层未实现 → 失败关闭。绝不把 OpenAI 形状的 body 原样发往 Cursor 原生端点，
	// 也绝不降级重试到其它上游（那会把 cursor 凭证发到非 cursor 上游）。
	return ErrCursorProtocolBridgeUnavailable
}