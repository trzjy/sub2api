package service

// 本文件实现 **cursor 平台的模型目录同步适配层**（P1-B1）。
//
// 复用现有管理端 `/accounts/:id/models/sync-upstream` 入口（见
// SyncUpstreamModelCatalog 的 cursor 前置分派），完整成功结果经既有安全凭证写链
// 原子写回同一 `credentials.model_mapping`；任何失败都明确报错，不提交半份。
//
// 目录来源与字段依据（不得臆造；权威 = 真实回包 + 客户端 schema 审计）：
//   - aiserver.v1.AiService/AvailableModels（Unary，裸 protobuf，无 5 字节 envelope，
//     Content-Type: application/proto）——真实请求体 hex `1801280150015801`
//     （字段 1 is_nightly=1、2 include_long_context_models=1、
//     3 exclude_max_named_models=1、5 use_model_parameters=1；其余可选字段按 proto3 省略）。
//   - aiserver.v1.AvailableModelsResponse { 1 model_names(repeated string)
//     2 models(repeated AvailableModel) }；真实回包 48 条 models=f2。
//   - AvailableModel：1 name(string) 2 default_on(bool) 3 is_long_context_only(bool,opt)
//     5 supports_agent(bool,opt) 17 client_display_name(string,opt)
//     18 server_model_name(string,opt) 19 supports_non_max_mode(bool,opt)
//     27 only_supports_cmd_k(bool,opt) 29 parameter_definitions(message,repeated)
//     30 variants(ModelVariantConfig,repeated) 35 is_hidden(bool,opt)
//     36 legacy_slugs(string,repeated) 37 id_aliases(string,repeated)。
//   - ModelVariantConfig：1 parameter_values(repeated ModelParameterValue)
//     3 is_max_mode(bool) 11 legacy_slug(string,opt)；
//     ModelParameterValue：1 id(string) 2 value(string)。
//   —— 证据 /home/zjy/.sub2api-acceptance/sub2api-cursor-takeover-20261019/
//      11-base-catalog.bin（真实 200 回包）、34-catalog-schema-audit.json（字段号/wire 类型）。
//
// **基础身份口径（本文件唯一对外映射来源）**：一条 AvailableModel 的基础身份唯一为
// f1 name → f18 server_model_name。展示名（f17）、legacy_slugs(f36)、id_aliases(f37)
// 与 variants(f30) 的 legacy_slug **都不是**无参数基础身份：variant 的 legacy_slug 携带
// parameter_values（context / reasoning_effort / fast 等档位语义），把它扁平映射成基础名
// 等于声明"该参数化档位可用不带参数的模型完成"，属于编造上游能力。故同步只写 name→server。
//
// 历史（**不再是基础目录来源**）：agent.v1.GetUsableModels 返回 241 条 client
// ModelDetails，其 model_id 是 241 个**互不相同**的档位 wire ID（如 gpt-5-high、
// claude-opus-4-8-thinking）。按 wire ID 去重折叠**不可能**得到基础目录（重复次数恒为 0），
// 该职责连同"别名 first-wins"一并移除；协议里其它真实用途与历史取证保留。见方案 §18.3 第 7 条。
//
// 本文件固定语义：
//   - 原子写：解析失败 / 空目录 / 缺基础身份 / 同 name 不同 server / 上游失败 → 整体报错，
//     **不写半份**；成功才有一次单行 UpdateCredentials。
//   - 不自动恢复运营停用状态、不批量放开存量人工限制：写链只**补缺键**，
//     既不删除也不覆写任何既有映射键（人工键及其空值都保留）。
//   - 不新增定时器/ticker/管理API/数据库字段/参数化档位支持；只挂现有手动入口。
//   - 不按标点相似性归一模型名（claude-fable-5.1 与 claude-fable-5-1 是不同标识）。

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

const (
	// cursorModelSyncPathAvailableModels 是 cursor **基础模型目录**的 Unary 端点路径。
	//
	// 11-base-catalog.cjs 真实请求路径；与对话转发用的 agent.v1 端点是两条路径。
	cursorModelSyncPathAvailableModels = "/aiserver.v1.AiService/AvailableModels"
)

// cursorModelSyncOutboundHeaders 对齐真实探针 11-base-catalog.cjs 的出站头：
// 用户级 Bearer（账号 access_token）+ 设备级 x-jg-auth + 客户端版本 + 网络优化。
// client-version 的值复用对话链路的 cursorClientVersion 常量（同款值，不另写一个）。
const (
	cursorModelSyncClientVersionHeader = "x-cursor-client-version"
	cursorModelSyncNetOptimizeHeader   = "x-jg-net-optimize"
	cursorModelSyncNetOptimizeValue    = "http1.1"
)

// cursorModelSyncRequestHex 是 AvailableModelsRequest 的请求体（真实抓包 hex，
// 证据 11-base-catalog.cjs 与 34-catalog-schema-audit.json 的 request_hex）。
//
// 这是**已在真实上游跑通**的请求形状，不得自行增删字段——加字段等于改变
// "我们向商家声明要看什么目录"。
var cursorModelSyncRequestHex = "1801280150015801"

// cursorModelSyncResponseMaxBytes 是目录响应的读取上限（防无界读）。真实回包约 330 KiB。
const cursorModelSyncResponseMaxBytes = 8 << 20

// AvailableModelsResponse / AvailableModel / ModelVariantConfig 的字段号。
const (
	cursorFldAvailModelsResponseNames  = 1 // repeated string model_names
	cursorFldAvailModelsResponseModels = 2 // repeated AvailableModel

	cursorFldAvailModelName                = 1  // string  基础身份·请求侧
	cursorFldAvailModelDefaultOn           = 2  // bool
	cursorFldAvailModelLongContextOnly     = 3  // bool  optional is_long_context_only
	cursorFldAvailModelSupportsAgent       = 5  // bool  optional supports_agent
	cursorFldAvailModelClientDisplayName   = 17 // string optional client_display_name
	cursorFldAvailModelServerModelName     = 18 // string optional server_model_name
	cursorFldAvailModelSupportsNonMaxMode  = 19 // bool  optional supports_non_max_mode
	cursorFldAvailModelOnlySupportsCmdK    = 27 // bool  optional only_supports_cmd_k
	cursorFldAvailModelParameterDefinition = 29 // message repeated parameter_definitions
	cursorFldAvailModelVariants            = 30 // message repeated variants
	cursorFldAvailModelIsHidden            = 35 // bool  optional is_hidden
	cursorFldAvailModelLegacySlugs         = 36 // string repeated legacy_slugs
	cursorFldAvailModelIDAliases           = 37 // string repeated id_aliases

	cursorFldVariantParameterValues = 1  // message repeated parameter_values
	cursorFldVariantIsMaxMode       = 3  // bool  is_max_mode
	cursorFldVariantLegacySlug      = 11 // string optional legacy_slug

	cursorFldParameterValueID    = 1 // string
	cursorFldParameterValueValue = 2 // string
)

// cursorCatalogModel 是 AvailableModels 目录中的一条**基础模型**。
//
// 只承载基础身份与解码校验所需的字段；variants/legacy_slugs/id_aliases 逐字解出来
// 只为校验 wire 形状与 fixture 关系核对，不参与映射（见文件头"基础身份口径"）。
type cursorCatalogModel struct {
	// Name 是基础身份的请求侧名字（AvailableModel.name，对外 mapping 键）。
	Name string
	// ServerModelName 是上游转发用的模型名（AvailableModel.server_model_name）。
	ServerModelName string
	// ClientDisplayName 仅展示用，**不作模型身份**。
	ClientDisplayName string

	SupportsAgent          bool
	SupportsNonMaxMode     bool
	OnlySupportsCmdK       bool
	IsLongContextOnly      bool
	IsHidden               bool
	LegacySlugs            []string
	IDAliases              []string
	VariantLegacySlugs     []string
	VariantParameterValues [][2]string // [id, value]
}

// cursorCatalogSource 是「cursor 上游基础模型目录」的窄来源契约。
//
// 为什么要这层 seam：模型同步必须能对四种失败分支（上游失败 / 解析失败 / 空目录 /
// 成功）做单测，而真实出站依赖号池 key + 激活码解密 + deviceId 解析 + wasm。默认实现
// 走真实 AvailableModels 出站，测试经 InstallCursorCatalogSource 安装替身。
// 该「包级安装位点」模式与既有 codeBuddyQuotaInstance 同款。
type cursorCatalogSource interface {
	CursorCatalog(ctx context.Context, account *Account) ([]cursorCatalogModel, error)
}

// cursorCatalogSourceInstance 是包级安装的目录来源（nil = 用默认实现）。
var cursorCatalogSourceInstance cursorCatalogSource

// InstallCursorCatalogSource 安装 cursor 基础模型目录来源（nil 表示恢复默认实现）。
func InstallCursorCatalogSource(src cursorCatalogSource) {
	cursorCatalogSourceInstance = src
}

// cursorModelSyncResolveSource 取本次同步使用的目录来源：显式安装优先，否则装配默认实现。
//
// 默认实现的鉴权依赖取自 AccountTestService 已注入的 OpenAIGatewayService（与
// forwardCursor 同源：配额 key 仓库 / 加密器 / deviceId 解析器，**未私自新增第二份**）。
// 依赖缺失即明确报错（Kind=configuration），绝不回落无凭证请求。
func (s *AccountTestService) cursorModelSyncResolveSource() (cursorCatalogSource, error) {
	if cursorCatalogSourceInstance != nil {
		return cursorCatalogSourceInstance, nil
	}
	if s == nil {
		return nil, newUpstreamModelSyncConfigError("Cursor model sync source is not configured", nil)
	}
	gateway := s.openaiGatewayService
	if gateway == nil || gateway.cursorExecution == nil {
		return nil, newUpstreamModelSyncConfigError(
			"Cursor model sync requires the cursor execution service (auth dependencies)", nil)
	}
	exec := gateway.cursorExecution
	return newCursorDefaultCatalogSource(s, withCursorModelSyncAuth(func(ctx context.Context, account *Account) (CursorUpstreamAuthProvider, error) {
		return exec.AuthProviderForAccount(account)
	})), nil
}

// cursorDefaultCatalogSource 是走真实 AvailableModels 出站的默认来源。
type cursorDefaultCatalogSource struct {
	svc            *AccountTestService
	resolveBaseURL func(ctx context.Context) string
	resolveAuth    func(ctx context.Context, account *Account) (CursorUpstreamAuthProvider, error)
}

// cursorModelSyncSourceOption 用于测试或自定义装配目录来源。
type cursorModelSyncSourceOption func(*cursorDefaultCatalogSource)

// withCursorModelSyncBaseURL 覆盖出站节点基址（测试用 httptest）。
func withCursorModelSyncBaseURL(fn func(ctx context.Context) string) cursorModelSyncSourceOption {
	return func(src *cursorDefaultCatalogSource) {
		if fn != nil {
			src.resolveBaseURL = fn
		}
	}
}

// withCursorModelSyncAuth 覆盖账号级鉴权材料提供者（测试用替身）。
func withCursorModelSyncAuth(fn func(ctx context.Context, account *Account) (CursorUpstreamAuthProvider, error)) cursorModelSyncSourceOption {
	return func(src *cursorDefaultCatalogSource) {
		if fn != nil {
			src.resolveAuth = fn
		}
	}
}

// newCursorDefaultCatalogSource 构造默认目录来源。
func newCursorDefaultCatalogSource(svc *AccountTestService, opts ...cursorModelSyncSourceOption) *cursorDefaultCatalogSource {
	src := &cursorDefaultCatalogSource{
		svc: svc,
		resolveBaseURL: func(ctx context.Context) string {
			return cursorUpstreamNodesSingleton().Resolve(ctx).HTTP1Base
		},
		resolveAuth: func(ctx context.Context, account *Account) (CursorUpstreamAuthProvider, error) {
			gateway := svc.openaiGatewayService
			if gateway == nil || gateway.cursorExecution == nil {
				return nil, newUpstreamModelSyncConfigError(
					"Cursor model sync requires the cursor execution service (auth dependencies)", nil)
			}
			return gateway.cursorExecution.AuthProviderForAccount(account)
		},
	}
	for _, o := range opts {
		o(src)
	}
	return src
}

// CursorCatalog 实现 cursorCatalogSource：拉取 AvailableModels 目录并解析。
//
// 失败关闭口径与 cursor 其余出站一致：节点基址为空 / 鉴权材料不可得 / 鉴权依赖缺失
// → 不发请求直接报错（不白耗 shareai 额度）。
func (src *cursorDefaultCatalogSource) CursorCatalog(ctx context.Context, account *Account) ([]cursorCatalogModel, error) {
	if src == nil || src.svc == nil {
		return nil, newUpstreamModelSyncConfigError("Cursor model sync source is not configured", nil)
	}
	if account == nil {
		return nil, newUpstreamModelSyncConfigError("Account is required", nil)
	}
	// 用户级 Bearer（账号已存 access_token）：与对话链路同款，缺一即失败关闭，
	// 绝不空凭证出站、绝不回落"只带 x-jg-auth 试试"（证据：缺 Bearer 被上游 401 拒）。
	bearer, err := cursorResolveAccountBearer(account)
	if err != nil {
		return nil, cursorModelSyncAsSyncError(err, UpstreamModelSyncErrorConfiguration, "Cursor account bearer is unavailable")
	}
	if src.svc.httpUpstream == nil {
		return nil, newUpstreamModelSyncConfigError("Cursor model sync requires an upstream HTTP client", nil)
	}
	reqBody, err := cursorModelSyncRequestBody()
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid cursor available models request", err)
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
	target := base + cursorModelSyncPathAvailableModels

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(reqBody))
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid cursor available models request", err)
	}
	req.ContentLength = int64(len(reqBody))
	req.Header.Set("Content-Type", cursorUnaryContentType)
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Accept", cursorUnaryContentType)
	req.Header.Set(cursorJgAuthHeader, auth)
	// 用户级身份：账号已存 access_token（与对话链路同款维度）。两者都要，缺一即失败关闭。
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set(cursorModelSyncClientVersionHeader, cursorClientVersion)
	req.Header.Set(cursorModelSyncNetOptimizeHeader, cursorModelSyncNetOptimizeValue)

	// 沿用既有「同TLS指纹/代理口径」的出站辅助（见 upstream_models.go），与其它平台的
	// 模型列表探测保持同一出口。
	resp, err := src.svc.doUpstreamModelsRequest(req, upstreamModelsProxyURL(account), account)
	if err != nil {
		return nil, newUpstreamModelSyncUpstreamError("Failed to request cursor available models", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := readCursorUpstreamBody(resp.Body, cursorModelSyncResponseMaxBytes)
	if err != nil {
		return nil, newUpstreamModelSyncUpstreamError("Failed to read cursor available models", err)
	}
	if resp.StatusCode != http.StatusOK {
		// auth 作为 secret 传入：上游错误文案可能回显 x-jg-auth 材料。
		inner := cursorUpstreamStatusError(resp.StatusCode, raw, auth)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, newUpstreamModelSyncCredentialError("Cursor upstream rejected the account credentials", resp.StatusCode, inner)
		}
		return nil, &UpstreamModelSyncError{
			Kind:       UpstreamModelSyncErrorUpstream,
			Message:    fmt.Sprintf("Cursor available models request failed with HTTP %d", resp.StatusCode),
			StatusCode: resp.StatusCode,
			Err:        inner,
		}
	}
	return parseCursorAvailableModelsResponse(raw)
}

// cursorModelSyncRequestBody 解析真实请求体 hex（常量，解析失败即编程错误）。
func cursorModelSyncRequestBody() ([]byte, error) {
	body, err := hex.DecodeString(cursorModelSyncRequestHex)
	if err != nil {
		return nil, fmt.Errorf("decode available models request: %w", err)
	}
	if len(body) == 0 {
		return nil, errors.New("available models request body is empty")
	}
	return body, nil
}

// parseCursorAvailableModelsResponse 解析 AvailableModelsResponse（repeated models=f2）。
//
// 失败关闭（整体报错，一次都不写）：
//   - 空响应 / 非法 protobuf / 截断 → 解析失败；
//   - 没有任何 models 项 → 空目录；
//   - 某条缺基础身份（name 或 server_model_name 为空）→ 缺基础身份；
//   - 同一 name 出现不同的 server_model_name → 身份冲突。
//
// 已知字段的 wire 类型与 schema 不符时报错；**未知合法字段跳过**（上游加字段不应
// 让本站目录同步整体失败）。跨 base 重复的 id_aliases 本身合法，不据此拒绝整份目录。
func parseCursorAvailableModelsResponse(body []byte) ([]cursorCatalogModel, error) {
	if len(body) == 0 {
		return nil, fmt.Errorf("%w: empty cursor available models response", cursorModelSyncParseFailed())
	}
	fields, err := cursorDecodeProtobuf(body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", cursorModelSyncParseFailed(), err)
	}
	models := make([]cursorCatalogModel, 0, len(fields))
	serverByName := make(map[string]string, len(fields))
	for _, field := range fields {
		if field.Number != cursorFldAvailModelsResponseModels {
			continue // model_names 等其它顶层字段不参与基础目录
		}
		if field.Wire != cursorWireBytes {
			return nil, cursorCatalogTypeError("AvailableModelsResponse.models", field)
		}
		entryFields, entryErr := cursorDecodeProtobuf(field.Bytes)
		if entryErr != nil {
			return nil, fmt.Errorf("%w: %v", cursorModelSyncParseFailed(), entryErr)
		}
		entry, entryErr := cursorCatalogModelFromFields(entryFields)
		if entryErr != nil {
			return nil, entryErr
		}
		if existing, claimed := serverByName[entry.Name]; claimed && existing != entry.ServerModelName {
			return nil, fmt.Errorf("%w: model %q is declared with conflicting server model names %q and %q",
				cursorModelSyncParseFailed(), entry.Name, existing, entry.ServerModelName)
		}
		serverByName[entry.Name] = entry.ServerModelName
		models = append(models, entry)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("%w: response carries no available model entry", cursorModelSyncParseFailed())
	}
	return models, nil
}

// cursorCatalogModelFromFields 把一条 AvailableModel 的字段序列映射成基础模型项。
//
// wire 类型严格按客户端 schema：string 字段收到非 bytes、bool 字段收到非 varint 一律
// 报错（不"尽力而为"地忽略——错位解析出的身份会写进线上 mapping）。
// repeated 字段逐个累积：cursorFindField 是 last-one-wins，不能用于 repeated 语义。
func cursorCatalogModelFromFields(fields []cursorPBField) (cursorCatalogModel, error) {
	var entry cursorCatalogModel
	for _, field := range fields {
		var err error
		switch field.Number {
		case cursorFldAvailModelName:
			entry.Name, err = cursorCatalogStringField(field, "AvailableModel.name")
		case cursorFldAvailModelClientDisplayName:
			entry.ClientDisplayName, err = cursorCatalogStringField(field, "AvailableModel.client_display_name")
		case cursorFldAvailModelServerModelName:
			entry.ServerModelName, err = cursorCatalogStringField(field, "AvailableModel.server_model_name")
		case cursorFldAvailModelLongContextOnly:
			entry.IsLongContextOnly, err = cursorCatalogBoolField(field, "AvailableModel.is_long_context_only")
		case cursorFldAvailModelSupportsAgent:
			entry.SupportsAgent, err = cursorCatalogBoolField(field, "AvailableModel.supports_agent")
		case cursorFldAvailModelSupportsNonMaxMode:
			entry.SupportsNonMaxMode, err = cursorCatalogBoolField(field, "AvailableModel.supports_non_max_mode")
		case cursorFldAvailModelOnlySupportsCmdK:
			entry.OnlySupportsCmdK, err = cursorCatalogBoolField(field, "AvailableModel.only_supports_cmd_k")
		case cursorFldAvailModelIsHidden:
			entry.IsHidden, err = cursorCatalogBoolField(field, "AvailableModel.is_hidden")
		case cursorFldAvailModelLegacySlugs:
			entry.LegacySlugs, err = cursorCatalogRepeatedStringField(entry.LegacySlugs, field, "AvailableModel.legacy_slugs")
		case cursorFldAvailModelIDAliases:
			entry.IDAliases, err = cursorCatalogRepeatedStringField(entry.IDAliases, field, "AvailableModel.id_aliases")
		case cursorFldAvailModelParameterDefinition:
			// parameter_definitions 只校验 wire 形状（其 message 名未在本次范围内解析）。
			if field.Wire != cursorWireBytes {
				return cursorCatalogModel{}, cursorCatalogTypeError("AvailableModel.parameter_definitions", field)
			}
		case cursorFldAvailModelVariants:
			if field.Wire != cursorWireBytes {
				return cursorCatalogModel{}, cursorCatalogTypeError("AvailableModel.variants", field)
			}
			variantFields, verr := cursorDecodeProtobuf(field.Bytes)
			if verr != nil {
				return cursorCatalogModel{}, fmt.Errorf("%w: %v", cursorModelSyncParseFailed(), verr)
			}
			legacySlug, pairs, verr := cursorVariantFromFields(variantFields)
			if verr != nil {
				return cursorCatalogModel{}, verr
			}
			if legacySlug != "" {
				entry.VariantLegacySlugs = append(entry.VariantLegacySlugs, legacySlug)
			}
			entry.VariantParameterValues = append(entry.VariantParameterValues, pairs...)
		}
		if err != nil {
			return cursorCatalogModel{}, err
		}
	}
	if strings.TrimSpace(entry.Name) == "" || strings.TrimSpace(entry.ServerModelName) == "" {
		return cursorCatalogModel{}, fmt.Errorf(
			"%w: available model entry lacks a base identity (name=%q server_model_name=%q)",
			cursorModelSyncParseFailed(), entry.Name, entry.ServerModelName)
	}
	entry.Name = strings.TrimSpace(entry.Name)
	entry.ServerModelName = strings.TrimSpace(entry.ServerModelName)
	return entry, nil
}

// cursorVariantFromFields 解析一条 ModelVariantConfig。
//
// 只解出 legacy_slug(f11) 与 parameter_values(f1) 的 id/value 对，**不**用于生成映射键：
// 参数化档位名与无参数基础名不是同一个可调用目标（见文件头口径）。
func cursorVariantFromFields(fields []cursorPBField) (string, [][2]string, error) {
	var legacySlug string
	var pairs [][2]string
	for _, field := range fields {
		switch field.Number {
		case cursorFldVariantLegacySlug:
			slug, err := cursorCatalogStringField(field, "ModelVariantConfig.legacy_slug")
			if err != nil {
				return "", nil, err
			}
			legacySlug = strings.TrimSpace(slug)
		case cursorFldVariantIsMaxMode:
			if _, err := cursorCatalogBoolField(field, "ModelVariantConfig.is_max_mode"); err != nil {
				return "", nil, err
			}
		case cursorFldVariantParameterValues:
			if field.Wire != cursorWireBytes {
				return "", nil, cursorCatalogTypeError("ModelVariantConfig.parameter_values", field)
			}
			valueFields, verr := cursorDecodeProtobuf(field.Bytes)
			if verr != nil {
				return "", nil, fmt.Errorf("%w: %v", cursorModelSyncParseFailed(), verr)
			}
			id, value, verr := cursorParameterValueFromFields(valueFields)
			if verr != nil {
				return "", nil, verr
			}
			if id != "" {
				pairs = append(pairs, [2]string{id, value})
			}
		}
	}
	return legacySlug, pairs, nil
}

// cursorParameterValueFromFields 解析 ModelParameterValue（1 id / 2 value，均为 string）。
func cursorParameterValueFromFields(fields []cursorPBField) (string, string, error) {
	var id, value string
	for _, field := range fields {
		var err error
		switch field.Number {
		case cursorFldParameterValueID:
			id, err = cursorCatalogStringField(field, "ModelParameterValue.id")
		case cursorFldParameterValueValue:
			value, err = cursorCatalogStringField(field, "ModelParameterValue.value")
		}
		if err != nil {
			return "", "", err
		}
	}
	return strings.TrimSpace(id), strings.TrimSpace(value), nil
}

func cursorCatalogStringField(field cursorPBField, label string) (string, error) {
	if field.Wire != cursorWireBytes {
		return "", cursorCatalogTypeError(label, field)
	}
	return field.String(), nil
}

func cursorCatalogBoolField(field cursorPBField, label string) (bool, error) {
	if field.Wire != cursorWireVarint {
		return false, cursorCatalogTypeError(label, field)
	}
	return field.Varint != 0, nil
}

func cursorCatalogRepeatedStringField(acc []string, field cursorPBField, label string) ([]string, error) {
	value, err := cursorCatalogStringField(field, label)
	if err != nil {
		return nil, err
	}
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		acc = append(acc, trimmed)
	}
	return acc, nil
}

func cursorCatalogTypeError(label string, field cursorPBField) error {
	return fmt.Errorf("%w: %s (field %d) has wire type %d, which contradicts the catalog schema",
		cursorModelSyncParseFailed(), label, field.Number, field.Wire)
}

// cursorModelSyncParseFailed 是目录解析失败的标记错误（无外部哨兵语义，仅供错误文本）。
func cursorModelSyncParseFailed() error {
	return errors.New("cursor available models response was not usable")
}

// cursorModelCatalog 是适配结果：对外模型清单（基础 name）+ 待补写入 mapping 的 name→server。
type cursorModelCatalog struct {
	Models  []string
	Mapping map[string]string
}

// adaptCursorCatalogModels 把基础目录项适配为「对外模型 + mapping」。
//
// **只写基础身份 name → server_model_name**，逐条依据：
//   - 不按 legacy_slugs(f36) / id_aliases(f37) 生成别名键：别名 first-wins 需要一份
//     跨 base 的冲突裁决策略，本次没有裁定；且真实目录里 `opus`、`gpt`、`gpt-mini` 等
//     别名同时被 3–5 个 base 声明，先到先得会把任意一个 base 钉死成别名唯一目标。
//   - 不按 variants(f30).legacy_slug 生成键：该名携带 parameter_values（max/effort 档位），
//     映射到无参数基础名等于宣称"该参数化档位可用基础模型完成"。参数化档位支持不在本次范围。
//   - 不按标点相似性归一（`claude-fable-5.1` ≠ `claude-fable-5-1`）。
//
// 身份键（name → server）必须写：转发侧闸门要求 model_mapping **显式命中**才会出站
// （见 forwardCursor），只靠 ssot 目录不足让账号可用。
func adaptCursorCatalogModels(entries []cursorCatalogModel) (*cursorModelCatalog, error) {
	if len(entries) == 0 {
		return nil, cursorModelSyncParseFailed()
	}
	seen := make(map[string]struct{}, len(entries))
	mapping := make(map[string]string, len(entries))
	models := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		server := strings.TrimSpace(entry.ServerModelName)
		if name == "" || server == "" {
			// 基础身份缺失：解析阶段已整体失败，这里是纵深防御。
			return nil, cursorModelSyncParseFailed()
		}
		if _, exists := seen[name]; !exists {
			seen[name] = struct{}{}
			models = append(models, name)
		}
		if _, claimed := mapping[name]; !claimed {
			mapping[name] = server
		}
	}
	if len(models) == 0 {
		return nil, cursorModelSyncParseFailed()
	}
	sort.Strings(models)
	return &cursorModelCatalog{Models: models, Mapping: mapping}, nil
}

// fetchCursorCatalog 取本次同步的 cursor 基础目录原始项（已按 UpstreamModelSyncError 归类）。
//
// SyncUpstreamModelCatalog 与 fetchUpstreamModelList 共用这一条抓取口径，避免两处
// 独立装配导致错误分类漂移（任一侧漏判都会让 cursor 落到 OpenAI /v1/models 通用路径）。
func (s *AccountTestService) fetchCursorCatalog(ctx context.Context, account *Account) ([]cursorCatalogModel, error) {
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
	entries, err := src.CursorCatalog(ctx, account)
	if err != nil {
		// 已是 *UpstreamModelSyncError 的原样透传（保留具体分类与状态码），
		// 其余（含解析失败）统一按上游返回不可用归类。
		return nil, cursorModelSyncAsSyncError(err, UpstreamModelSyncErrorUpstream, "Cursor upstream returned an unusable model catalog")
	}
	return entries, nil
}

// adaptCursorCatalogModelsOrError 适配目录项；空目录/不可用目录→ Upstream 类错误。
func adaptCursorCatalogModelsOrError(entries []cursorCatalogModel) (*cursorModelCatalog, error) {
	adapted, err := adaptCursorCatalogModels(entries)
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

	entries, err := s.fetchCursorCatalog(ctx, account)
	if err != nil {
		return nil, err
	}
	adapted, adaptErr := adaptCursorCatalogModelsOrError(entries)
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

// applyCursorModelMapping 把目录基础身份并入既有 model_mapping（**只补缺键**）。
//
// 为什么只补缺键：①运营停用态/人工 alias 不得被批量放开或改写（固定语义）；
// ②已存在的键值无从区分人工与既往同步，改写任何一方都等于替运营做决定。
// 因此本同步器对「已存在的键」一律跳过（含人工把值置空的键）；写链是单行
// UpdateCredentials（一次成，无半份）。
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
