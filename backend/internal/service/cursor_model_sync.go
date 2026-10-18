package service

// 本文件实现 **cursor 平台的模型目录同步适配层**（P1-B1）。
//
// 复用现有管理端 `/accounts/:id/models/sync-upstream` 入口（见
// SyncUpstreamModelCatalog 的 cursor 前置分派），完整成功结果经既有安全凭证写链
// 原子写回同一 `credentials.model_mapping`；任何失败都明确报错，不提交半份。
//
// 目录来源与字段依据（不得臆造）：
//   - agent.v1.GetUsableModelsResponse { repeated ModelDetails models = 1; }
//     agent.v1.GetUsableModelsRequest  { repeated string custom_model_ids = 1; }
//     —— 证据 /root/cursor-p1-evidence/40-extra-schemas.json
//   - agent.v1.ModelDetails { 1 model_id(string) 2 thinking_details(message)
//     3 display_model_id(string) 4 display_name(string) 5 display_name_short(string)
//     6 aliases(repeated string) 7 max_mode(bool) 8..10 credentials oneof }
//     —— 证据 /root/cursor-p1-evidence/32-agent-protocol-schemas.json（key "ode"）
//   - GetUsableModels 是 Unary 端点：请求体走**裸 protobuf**（无 5 字节 envelope），
//     Content-Type 用 `application/proto`；误用 streaming 的 connect+proto 会 415
//     （既有实证口径见 cursor_protocol_response.go）。
//   - 证据 65：usable 响应 241 个 models 项，且带 Bearer 与不带 Bearer 逐字相同。
//
// 本卡固定语义（卡 nem §5 / §15.4-I1）：
//   - 241 档位项**不照抄**为 241 个对外模型：同一 server_model_name 的多份档位项折叠
//     为一条对外模型（含 max_mode 变体），目录结构仍保留 server_model_name /
//     legacy_slugs 两层，不丢信息。
//   - 原子写：解析失败 / 空目录 / 上游失败 → 整体报错，**不写半份**；成功才有一次
//     单行 UpdateCredentials。
//   - 不自动恢复运营停用状态、不批量放开存量人工限制：写链只**补缺键**，
//     既不删除也不覆写任何既有映射键。
//   - 不新增定时器/ticker；只挂现有手动入口。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

const (
	// cursorModelSyncPathGetUsableModels 是 cursor 可用模型目录的 Unary 端点路径。
	//
	// 与已活体取证的 AvailableModels 同属 `aiserver.v1.AiService` 服务族（见
	// cursor_upstream_forward.go 的路径白名单注释），方法名取证据中的 GetUsableModels。
	cursorModelSyncPathGetUsableModels = "/aiserver.v1.AiService/GetUsableModels"

	// cursorModelSyncResponseMaxBytes 是目录响应的读取上限（防无界读）。
	cursorModelSyncResponseMaxBytes = 8 << 20
)

// cursorUsableModel 是 GetUsableModels 目录中的一条 client ModelDetails。
//
// 字段名按本卡固定语义命名：`ServerModelName` = 上游 **wire ID**（ModelDetails.model_id，
// 对话请求 ModelDetails.model_name 必须填它，填展示名会被上游拒 ERROR_BAD_MODEL_NAME）；
// `LegacySlugs` = 该模型仍被接受的旧名（ModelDetails.aliases）。
type cursorUsableModel struct {
	ServerModelName string
	DisplayModelID  string
	DisplayName     string
	LegacySlugs     []string
	MaxMode         bool
}

// GetUsableModels 响应 / ModelDetails 的字段号（证据 40 / 32，勿臆造）。
const (
	cursorFldUsableModelsResponseModels = 1

	cursorFldUsableModelID        = 1
	cursorFldUsableThinkingDetail = 2
	cursorFldUsableDisplayModelID = 3
	cursorFldUsableDisplayName    = 4
	cursorFldUsableAliases        = 6
	cursorFldUsableMaxMode        = 7
)

// cursorUsableModelsSource 是「cursor 上游可用模型目录」的窄来源契约。
//
// 为什么要这层 seam：模型同步必须能对三种失败分支（上游失败 / 空目录 / 成功）做单测，
// 而真实出站依赖号池 key + 激活码解密 + deviceId 解析 + wasm。默认实现走真实
// GetUsableModels 出站，测试经 InstallCursorUsableModelsSource 安装替身。
// 该「包级安装位点」模式与既有 codeBuddyQuotaInstance 同款。
type cursorUsableModelsSource interface {
	CursorUsableModels(ctx context.Context, account *Account) ([]cursorUsableModel, error)
}

// cursorUsableModelsInstance 是包级安装的目录来源（nil = 用默认实现）。
var cursorUsableModelsInstance cursorUsableModelsSource

// InstallCursorUsableModelsSource 安装 cursor 模型目录来源（nil 表示恢复默认实现）。
func InstallCursorUsableModelsSource(src cursorUsableModelsSource) {
	cursorUsableModelsInstance = src
}

// cursorModelSyncResolveSource 取本次同步使用的目录来源：显式安装优先，否则装配默认实现。
//
// 默认实现的鉴权依赖取自 AccountTestService 已注入的 OpenAIGatewayService（与
// forwardCursor 同源：配额 key 仓库 / 加密器 / deviceId 解析器，**未私自新增第二份**）。
// 依赖缺失即明确报错（Kind=configuration），绝不回落无凭证请求。
func (s *AccountTestService) cursorModelSyncResolveSource() (cursorUsableModelsSource, error) {
	if cursorUsableModelsInstance != nil {
		return cursorUsableModelsInstance, nil
	}
	if s == nil {
		return nil, newUpstreamModelSyncConfigError("Cursor model sync source is not configured", nil)
	}
	gateway := s.openaiGatewayService
	if gateway == nil || gateway.cursorQuotaKeys == nil || gateway.cursorQuotaKeyEnc == nil || gateway.cursorDeviceIDs == nil {
		return nil, newUpstreamModelSyncConfigError(
			"Cursor model sync requires the cursor auth dependencies (quota key repository, encryptor, device id resolver)", nil)
	}
	return newCursorDefaultUsableModelsSource(s, withCursorModelSyncAuth(func(ctx context.Context, account *Account) (CursorUpstreamAuthProvider, error) {
		return newCursorAccountAuthProvider(account, gateway.cursorQuotaKeys, gateway.cursorQuotaKeyEnc, gateway.cursorDeviceIDs)
	})), nil
}

// cursorDefaultUsableModelsSource 是走真实 GetUsableModels 出站的默认来源。
type cursorDefaultUsableModelsSource struct {
	svc            *AccountTestService
	resolveBaseURL func(ctx context.Context) string
	resolveAuth    func(ctx context.Context, account *Account) (CursorUpstreamAuthProvider, error)
}

// cursorModelSyncSourceOption 用于测试或自定义装配目录来源。
type cursorModelSyncSourceOption func(*cursorDefaultUsableModelsSource)

// withCursorModelSyncBaseURL 覆盖出站节点基址（测试用 httptest）。
func withCursorModelSyncBaseURL(fn func(ctx context.Context) string) cursorModelSyncSourceOption {
	return func(src *cursorDefaultUsableModelsSource) {
		if fn != nil {
			src.resolveBaseURL = fn
		}
	}
}

// withCursorModelSyncAuth 覆盖账号级鉴权材料提供者（测试用替身）。
func withCursorModelSyncAuth(fn func(ctx context.Context, account *Account) (CursorUpstreamAuthProvider, error)) cursorModelSyncSourceOption {
	return func(src *cursorDefaultUsableModelsSource) {
		if fn != nil {
			src.resolveAuth = fn
		}
	}
}

// newCursorDefaultUsableModelsSource 构造默认目录来源。
func newCursorDefaultUsableModelsSource(svc *AccountTestService, opts ...cursorModelSyncSourceOption) *cursorDefaultUsableModelsSource {
	src := &cursorDefaultUsableModelsSource{
		svc: svc,
		resolveBaseURL: func(ctx context.Context) string {
			return cursorUpstreamNodesSingleton().Resolve(ctx).HTTP1Base
		},
		resolveAuth: func(ctx context.Context, account *Account) (CursorUpstreamAuthProvider, error) {
			gateway := svc.openaiGatewayService
			if gateway == nil || gateway.cursorQuotaKeys == nil || gateway.cursorQuotaKeyEnc == nil || gateway.cursorDeviceIDs == nil {
				return nil, newUpstreamModelSyncConfigError(
					"Cursor model sync requires the cursor auth dependencies (quota key repository, encryptor, device id resolver)", nil)
			}
			return newCursorAccountAuthProvider(account, gateway.cursorQuotaKeys, gateway.cursorQuotaKeyEnc, gateway.cursorDeviceIDs)
		},
	}
	for _, o := range opts {
		o(src)
	}
	return src
}

// CursorUsableModels 实现 cursorUsableModelsSource：拉取 GetUsableModels 目录并解析。
//
// 失败关闭口径与 cursor 其余出站一致：节点基址为空 / 鉴权材料不可得 / 鉴权依赖缺失
// → 不发请求直接报错（不白耗 shareai 额度）。
func (src *cursorDefaultUsableModelsSource) CursorUsableModels(ctx context.Context, account *Account) ([]cursorUsableModel, error) {
	if src == nil || src.svc == nil {
		return nil, newUpstreamModelSyncConfigError("Cursor model sync source is not configured", nil)
	}
	if account == nil {
		return nil, newUpstreamModelSyncConfigError("Account is required", nil)
	}
	if src.svc.httpUpstream == nil {
		return nil, newUpstreamModelSyncConfigError("Cursor model sync requires an upstream HTTP client", nil)
	}

	authProvider, err := src.resolveAuth(ctx, account)
	if err != nil {
		return nil, cursorModelSyncAsSyncError(err, UpstreamModelSyncErrorConfiguration, "Failed to resolve cursor auth material")
	}
	auth, err := cursorUpstreamAuthFrom(ctx, authProvider)
	if err != nil {
		return nil, cursorModelSyncAsSyncError(err, UpstreamModelSyncErrorConfiguration, "Cursor auth material is unavailable")
	}

	base := strings.TrimRight(strings.TrimSpace(src.resolveBaseURL(ctx)), "/")
	if base == "" {
		return nil, newUpstreamModelSyncConfigError("Cursor upstream node base URL is unavailable", nil)
	}
	target := base + cursorModelSyncPathGetUsableModels

	// GetUsableModelsRequest 不留 custom_model_ids：全量目录。空 message = 空请求体。
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(nil))
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid cursor usable models request", err)
	}
	req.Header.Set("Content-Type", cursorUnaryContentType)
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", cursorUnaryContentType)
	req.Header.Set(cursorJgAuthHeader, auth)
	req.Header.Set(cursorJgModelHeader, cursorJgModelHeaderValue)

	// 沿用既有「同TLS指纹/代理口径」的出站辅助（见 upstream_models.go），与其它平台的
	// 模型列表探测保持同一出口。
	resp, err := src.svc.doUpstreamModelsRequest(req, upstreamModelsProxyURL(account), account)
	if err != nil {
		return nil, newUpstreamModelSyncUpstreamError("Failed to request cursor usable models", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := readCursorUpstreamBody(resp.Body, cursorModelSyncResponseMaxBytes)
	if err != nil {
		return nil, newUpstreamModelSyncUpstreamError("Failed to read cursor usable models", err)
	}
	if resp.StatusCode != http.StatusOK {
		// auth 作为 secret 传入：上游错误文案可能回显 x-jg-auth 材料。
		inner := cursorUpstreamStatusError(resp.StatusCode, body, auth)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, newUpstreamModelSyncCredentialError("Cursor upstream rejected the account credentials", resp.StatusCode, inner)
		}
		return nil, &UpstreamModelSyncError{
			Kind:       UpstreamModelSyncErrorUpstream,
			Message:    fmt.Sprintf("Cursor usable models request failed with HTTP %d", resp.StatusCode),
			StatusCode: resp.StatusCode,
			Err:        inner,
		}
	}
	return parseCursorUsableModelsResponse(body)
}

// parseCursorUsableModelsResponse 解析 GetUsableModelsResponse（repeated ModelDetails）。
//
// 失败关闭：空响应 / 非法 protobuf / 没有任何可用项 → 报错（调用方据此整体放弃本次同步，
// 绝不写半份）。单项缺 model_id 只是不可用于转发，跳过该项但不影响其余（与其在 N 项里
// 丢一条就整体作废相比，这更符合目录同步目的；是否可用由转发侧模型闸门兜底）。
func parseCursorUsableModelsResponse(body []byte) ([]cursorUsableModel, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: empty cursor usable models response", cursorModelSyncParseFailed())
	}
	fields, err := cursorDecodeProtobuf(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", cursorModelSyncParseFailed(), err)
	}
	models := make([]cursorUsableModel, 0, len(fields))
	for _, field := range fields {
		// 只认 field 1（models）；其它顶层字段（若有）不影响目录。
		if field.Number != cursorFldUsableModelsResponseModels || field.Wire != cursorWireBytes {
			continue
		}
		entryFields, entryErr := cursorDecodeProtobuf(field.Bytes)
		if entryErr != nil {
			return nil, fmt.Errorf("%w: %v", cursorModelSyncParseFailed(), entryErr)
		}
		entry := cursorUsableModelFromFields(entryFields)
		if strings.TrimSpace(entry.ServerModelName) == "" {
			// 无 wire ID 的项无法作为转发目标：跳过，不由目录暴露。
			continue
		}
		models = append(models, entry)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("%w: response carries no usable model entry", cursorModelSyncParseFailed())
	}
	return models, nil
}

// cursorUsableModelFromFields 把一条 ModelDetails 的字段序列映射成目录项。
//
// repeated 字段（这里只有 aliases）逐个累积：既有 cursorFindField 是 last-one-wins，
// 不能用于 repeated 语义。
func cursorUsableModelFromFields(fields []cursorPBField) cursorUsableModel {
	var entry cursorUsableModel
	for _, field := range fields {
		switch field.Number {
		case cursorFldUsableModelID:
			if field.Wire == cursorWireBytes {
				entry.ServerModelName = strings.TrimSpace(field.String())
			}
		case cursorFldUsableDisplayModelID:
			if field.Wire == cursorWireBytes {
				entry.DisplayModelID = strings.TrimSpace(field.String())
			}
		case cursorFldUsableDisplayName:
			if field.Wire == cursorWireBytes {
				entry.DisplayName = strings.TrimSpace(field.String())
			}
		case cursorFldUsableAliases:
			if field.Wire == cursorWireBytes {
				if slug := strings.TrimSpace(field.String()); slug != "" {
					entry.LegacySlugs = append(entry.LegacySlugs, slug)
				}
			}
		case cursorFldUsableMaxMode:
			if field.Wire == cursorWireVarint && field.Varint != 0 {
				entry.MaxMode = true
			}
		}
	}
	return entry
}

// cursorModelSyncParseFailed 是目录解析失败的标记错误（无外部哨兵语义，仅供错误文本）。
func cursorModelSyncParseFailed() error {
	return errors.New("cursor usable models response was not usable")
}

// cursorUsableModelCatalog 是适配结果：对外模型清单 + 待补写入 mapping 的请求名 → wire ID。
type cursorUsableModelCatalog struct {
	Models            []string
	Mapping           map[string]string
	CollapsedTierRows int
}

// adaptCursorUsableModels 把上游目录项适配为「对外模型 + mapping」。
//
// 折叠规则（卡内固定语义：241 档位项不照抄为 241 对外模型）：
//   - 以 server_model_name 为唯一对外身份：同名档位项（如 max_mode 变体）折叠为一条；
//   - legacy_slugs 保留为 mapping 的**别名键**（请求名 → 同一 wire ID），不新增对外模型。
//
// 身份键（server_model_name → 自身）必须写：转发侧闸门要求 model_mapping **显式命中**
// 才会出站（见 forwardCursor），纯 ssot 目录不足让账号可用。
func adaptCursorUsableModels(entries []cursorUsableModel) (*cursorUsableModelCatalog, error) {
	if len(entries) == 0 {
		return nil, cursorModelSyncParseFailed()
	}
	seen := make(map[string]struct{}, len(entries))
	mapping := make(map[string]string, len(entries))
	models := make([]string, 0, len(entries))
	collapsed := 0
	for _, entry := range entries {
		server := strings.TrimSpace(entry.ServerModelName)
		if server == "" {
			continue
		}
		if _, exists := seen[server]; exists {
			collapsed++
		} else {
			seen[server] = struct{}{}
			models = append(models, server)
		}
		if _, claimed := mapping[server]; !claimed {
			mapping[server] = server
		}
		for _, slug := range entry.LegacySlugs {
			slug = strings.TrimSpace(slug)
			if slug == "" || slug == server {
				continue
			}
			if _, claimed := mapping[slug]; !claimed {
				mapping[slug] = server
			}
		}
	}
	if len(models) == 0 {
		return nil, cursorModelSyncParseFailed()
	}
	sort.Strings(models)
	return &cursorUsableModelCatalog{
		Models:            models,
		Mapping:           mapping,
		CollapsedTierRows: collapsed,
	}, nil
}

// fetchCursorUsableModels 取本次同步的 cursor 目录原始项（已按 UpstreamModelSyncError 归类）。
//
// SyncUpstreamModelCatalog 与 fetchUpstreamModelList 共用这一条抓取口径，避免两处
// 独立装配导致错误分类漂移（任一侧漏判都会让 cursor 落到 OpenAI /v1/models 通用路径）。
func (s *AccountTestService) fetchCursorUsableModels(ctx context.Context, account *Account) ([]cursorUsableModel, error) {
	if s == nil {
		return nil, newUpstreamModelSyncConfigError("Account test service is not configured", nil)
	}
	if account == nil {
		return nil, newUpstreamModelSyncConfigError("Account is required", nil)
	}
	src, err := s.cursorModelSyncResolveSource()
	if err != nil {
		return nil, err
	}
	entries, err := src.CursorUsableModels(ctx, account)
	if err != nil {
		// 已是 *UpstreamModelSyncError 的原样透传（保留具体分类与状态码），
		// 其余（含解析失败）统一按上游返回不可用归类。
		return nil, cursorModelSyncAsSyncError(err, UpstreamModelSyncErrorUpstream, "Cursor upstream returned an unusable model catalog")
	}
	return entries, nil
}

// adaptCursorUsableModelsOrError 适配目录项；空目录/不可用目录→ Upstream 类错误。
func adaptCursorUsableModelsOrError(entries []cursorUsableModel) (*cursorUsableModelCatalog, error) {
	adapted, err := adaptCursorUsableModels(entries)
	if err != nil {
		return nil, newUpstreamModelSyncUpstreamError("Cursor upstream returned an unusable model catalog", err)
	}
	return adapted, nil
}

// syncCursorModelCatalog 是 cursor 平台的 catalog 同步入口（被 SyncUpstreamModelCatalog 分派）。
//
// 原子写语义：抓取/解析任一失败 → 立即返回错误，**一次都不调写链**，已有 mapping 原样；
// 成功 → 经既有安全凭证写链（persistAccountCredentials）单行更新同一 model_mapping。
// preview（account.ID==0 或无 repo）只返回目录，不落库（与 muse 同步同款口径）。
func (s *AccountTestService) syncCursorModelCatalog(ctx context.Context, account *Account) (*UpstreamModelCatalog, error) {
	if s == nil {
		return nil, newUpstreamModelSyncConfigError("Account test service is not configured", nil)
	}
	if account == nil {
		return nil, newUpstreamModelSyncConfigError("Account is required", nil)
	}

	entries, err := s.fetchCursorUsableModels(ctx, account)
	if err != nil {
		return nil, err
	}
	adapted, adaptErr := adaptCursorUsableModelsOrError(entries)
	if adaptErr != nil {
		return nil, adaptErr
	}

	if account.ID > 0 && s.accountRepo != nil {
		if writeErr := s.applyCursorModelMapping(ctx, account, adapted.Mapping); writeErr != nil {
			return nil, writeErr
		}
	}

	return &UpstreamModelCatalog{
		Models:   adapted.Models,
		Metadata: make(map[string]UpstreamModelMetadata),
	}, nil
}

// applyCursorModelMapping 把目录别名并入既有 model_mapping（**只补缺键**）。
//
// 为什么只补缺键：①运营停用态/人工 alias 不得被批量放开或改写（卡内固定语义）；
// ②已存在的键值无从区分人工与既往同步，改写任何一方都等于替运营做决定。
// 因此本同步器对「已存在的键」一律跳过；写链是单行 UpdateCredentials（一次成，无半份）。
func (s *AccountTestService) applyCursorModelMapping(ctx context.Context, account *Account, mapping map[string]string) error {
	if account == nil || len(mapping) == 0 {
		return nil
	}
	current := account.GetModelMapping()
	raw := make(map[string]any, len(current)+len(mapping))
	for requestName, upstreamModel := range current {
		raw[requestName] = upstreamModel
	}
	added := 0
	for requestName, upstreamModel := range mapping {
		if _, exists := raw[requestName]; exists {
			continue
		}
		raw[requestName] = upstreamModel
		added++
	}
	if added == 0 {
		return nil // 幂等：无新增键不落库
	}
	credentials := shallowCopyMap(account.Credentials)
	credentials["model_mapping"] = raw
	if err := persistAccountCredentials(ctx, s.accountRepo, account, credentials); err != nil {
		return newUpstreamModelSyncInternalError("Failed to save cursor model mapping", err)
	}
	return nil
}

// cursorModelSyncAsSyncError 把错误归入 UpstreamModelSyncError：
// 已经是该类型的原样返回，否则按给定 kind 包一层（保证管理端能按类映射状态码）。
func cursorModelSyncAsSyncError(err error, kind UpstreamModelSyncErrorKind, message string) error {
	if err == nil {
		return nil
	}
	var syncErr *UpstreamModelSyncError
	if errors.As(err, &syncErr) {
		return syncErr
	}
	return &UpstreamModelSyncError{Kind: kind, Message: message, Err: err}
}
