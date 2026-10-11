/**
 * Admin Channel Monitor API endpoints
 * Handles channel monitor (uptime/health) management for administrators
 */

// w34 conflict-resolution fusion (second wave, B10 — frontend admin channelMonitor DTO 字段级)
//
// 开工取证（判定"零灌入"形态）：工作树本文件与 b0-ours 逐字节相同
//   （git hash-object == git rev-parse b0-ours:<path> == bdf43f8e），上游对已有文件的
//   hunk 一个都没被自动合并带进来。所以下面每一条 keep-ours 都是**预防性现状锁**
//   （防 B11 原样灌回），不是回退已合并的改动 —— B11 不要把本块误读成"已解过冲突"。
//   三方规模：base(b60f2bbc) 9 项 Provider / 358 行；ours 10 项 / 366 行；upstream(7ddd5697) 10 项 / 359 行。
//   双方同改仅一处（Provider union），git merge-file --diff3 rc=1 报唯一文本冲突块；
//   另两处各是单方改动，落在非冲突区，当前未进工作树，故同样按预防锁登记。
//   对应测试：__tests__/channelMonitor.fusion.spec.ts（15 条，逐条映射下面编号）。
//
// 融合点 1 — Provider 联合类型：keep OURS 'muse'；take UPSTREAM（暂缓，跨卡依赖）
//   双方都在 base 尾部（'minimax' 之后、同一插入位置）各加 1 项：ours 加 muse、上游加 opencode_go。
//   两个加项互不重叠、且都不在 base 里 → 语义上是"同一供应商的两种命名"，不是可并集的 orthogonal 增量。
//   keep OURS 的理由（不止"默认我方"，有三条硬证据）：
//     (1a) 穷尽性会破：ProviderIcon.vue:46 是 Record<Provider, IconData>（映射类型要求全覆盖），
//          该文件由 w53 已交付并明确"不将 opencode_go 写入 Record"。单方给本 union 加 opencode_go
//          → vue-tsc 立即报缺键（spec 第 4 条用纯函数实算该后果，不靠声称）。
//     (1b) 上游符号缺席：backend/internal/domain/platforms.go 不存在、service 包剥注释后零命中
//          PlatformOpenCodeGo / IsMultiProtocolAPIKeyProvider ⇒ 派发单权威表第 5 条"定义随自动合并已入树"
//          在本机整体不成立（复证 w14/w27 取证）。此时给前端 union 加一个后端无枚举值的成员，
//          只会把"编译错"降级成"运行期 provider 校验失败"，半个契约不该由本卡单方面落地。
//     (1c) 品牌终裁未决：w63 (d) 已把"muse 与 opencode_go 是 alias 还是并存"作为 BLOCKED 交主会话，
//          本卡取"无 alias"以与在树 w53/w63 一致；若终裁改判需同时回改三张卡（见收敛单元 (d)）。
//   报警锚点：spec 第 3 条（剥注释后本文件不得含 opencode_go）、第 4 条（穷尽性实算）。
//
// 融合点 2 — ChannelMonitor.channel_status / channel_observed_at：keep OURS（E 类，非上游删除）
//   两键是**我方独有新增**：base 无、上游无（三条件实测 key in base && key not in upstream && key in tree
//   的第一条即为 false ⇒ 属 E 类"我方新增"，绝不是"上游删除项"；写成删除会让 B11 把待保留键当已废弃清掉）。
//   序列化生产者在册：backend/internal/handler/admin/channel_monitor_handler.go:111 与
//   handler/channel_monitor_user_handler.go:70 的 json:"channel_status"；消费方 MonitorCard.vue:46、
//   ChannelMonitorView.vue:100-101,249 + 三条既有 spec。删除即连带断链，故 keep OURS 无条件。
//   上游对本文件零删除（diff ours→upstream 的 '-' 行只可能是 ours 独有键，实测上游从未声明过它们）。
//   报警锚点：spec 第 5/6/7 条。
//
// 融合点 3 — CreateParams.endpoint 文档注释：keep OURS；take UPSTREAM（暂缓，绑定未收敛的后端）
//   上游把"探活模式必填（base origin）"改成"（可含路径前缀的 HTTPS base URL）"，是同供应商
//   功能包（backend channel_monitor_validate.go 删掉 u.Path 非空即拒的分支 + const.go 改错误文案 +
//   checker.go joinURL 支持前缀）的**注释侧**。我方后端仍强制 base origin
//   （channel_monitor_validate.go:144 的 u.Path != "" && u.Path != "/" 分支在册、
//   channel_monitor_const.go:188 仍是 "endpoint must be base origin only (no path/query/fragment)"，
//   两个文件均无卡认领）。
//   吸收上游注释 = 让表单对用户的承诺与后端 400 相矛盾（同 w60 对
//   payment.validationEasyPayCustomMethodUpstreamTypeInvalid 的判据：先查文案描述的行为是否绑定上游实现）。
//   故本卡零改动，仅登记；收敛时**注释与后端两处必须同翻**。
//   报警锚点：spec 第 8 条（注释↔字段声明配对）、第 9/10 条（后端两处现状锁 —— B11 一旦放宽
//   origin 限制，那两条必红，提示同时把本行注释翻成上游形态）。
//
// 非冲突区观察（本卡零改动，移交 B11）
//   O1 minimax 后端校验缺口：base 的 monitorProviders/probeCapableProviders 两个 map 都不含
//      MonitorProviderMiniMax（常量在 channel_monitor_const.go:72 有定义），上游相对 base 各加 1 行修掉。
//      我方前端 base 就有 'minimax' ⇒ 既有缺口（选了 minimax 建监控会被后端 400），与本卡融合无关但是
//      真实上游修复，且 validate.go/const.go 均无卡认领 ⇒ 请 B11 吸收上游那 4 行 map 增量。
//   O2 muse 未进前端 SSOT 列表：constants/channelMonitor.ts 的 PROVIDERS 数组只有 base 的 9 项，
//      无 PROVIDER_MUSE ⇒ union 含 muse 但筛选栏/表单下拉取不到它。该文件无卡认领，登记给品牌终裁 (d)。
//   报警锚点：spec 第 11/12 条（两条现状锁，任一被上游修复点亮即转红）。
//
// 收敛单元与 owner（opencode_go 这条线的完整落地清单）
//   (a) backend/internal/domain/platforms.go + PlatformOpenCodeGo/IsMultiProtocolAPIKeyProvider 入树
//       — owner: B11 真 git merge（上游独有文件当前入树数 = 0）。
//   (b) account 平台判定分支（权威表第 4 条 other/codebuddy/muse/web + 上游 IsMultiProtocolAPIKeyProvider
//       追加）— owner: account.go 卡。
//   (c) ProviderIcon.vue 的 Record<Provider, IconData> 键集 — owner: w53（已交付"不写 opencode_go"）。
//   (d) muse ↔ opencode_go 的 alias/并存终裁 — owner: 主会话（w63 (d) 已登记 BLOCKED）。
//       若终裁为"并存"：本 union 加 opencode_go + (c) 加图标键 + w53 ProviderIcon.spec.ts 的
//       首字母 'O' 回退断言需删 + w62 types/index.ts 值集复核 + 本卡 spec 第 3/4 条需删。
//       若终裁为"alias 到 muse"：本 union 仍不加成员，(c) 不变，仅需在 w63/w53 与本卡 notes 补一句终裁结论。
//   本卡在当前 (a)-(d) 未收敛前的终态 = 工作树现状（keep OURS 全部三处），无任何代码层改动。
import { apiClient } from '../client'

// w34 融合点 1：keep OURS 'muse'；上游 'opencode_go' 暂缓（跨卡依赖，见文件头 notes 与 fusion spec 第 1-4 条）
export type Provider =
  | 'openai'
  | 'anthropic'
  | 'gemini'
  | 'grok'
  | 'antigravity'
  | 'kimi'
  | 'zhipu'
  | 'deepseek'
  | 'minimax'
  | 'muse'
export type MonitorStatus = 'operational' | 'degraded' | 'failed' | 'error'
export type BodyOverrideMode = 'off' | 'merge' | 'replace'
export type APIMode = 'chat_completions' | 'responses'
/**
 * probe = LLM 探活（默认）；quota = 仅查关联账号用量（零 LLM 成本）；
 * quota_probe = 探活 + 配额快照挂主模型行。
 */
export type CheckMode = 'probe' | 'quota' | 'quota_probe'

/** 配额快照中的单个用量窗口（与后端 domain.MonitorQuotaTier 一致）。 */
export interface MonitorQuotaTier {
  /** 5h | 7d | 7d-sonnet | 7d-fable | 30d | daily | weekly | total */
  window: string
  /** 同窗口多档时的机器标识（gemini shared/pro/flash、grok requests/tokens、antigravity 模型名） */
  label?: string
  used_percent: number
  used?: number
  limit?: number
  /** RFC3339；空表示无重置时间 */
  reset_at?: string
}

export interface MonitorBalance {
  currency: string
  balance: number
}

/** 归一化配额快照（与后端 domain.MonitorQuotaSnapshot 一致）。 */
export interface MonitorQuotaSnapshot {
  /** usage | cn_quota | cn_balance */
  source: string
  success: boolean
  tiers?: MonitorQuotaTier[]
  balance?: number | null
  balances?: MonitorBalance[]
  currency?: string
  plan_level?: string
  /** 401/403 鉴权失败标记（推导为 failed 状态） */
  credential_invalid?: boolean
  error?: string
  fetched_at: string
}

export interface ChannelMonitor {
  id: number
  name: string
  provider: Provider
  api_mode: APIMode
  endpoint: string
  api_key_masked: string
  /**
   * True when the stored encrypted API key cannot be decrypted (e.g. the
   * encryption key has changed). Admin must re-edit the monitor to provide
   * a fresh key. Backend skips checks for these monitors.
   */
  api_key_decrypt_failed?: boolean
  primary_model: string
  extra_models: string[]
  group_name: string
  enabled: boolean
  interval_seconds: number
  /** 每次调度在 interval 基础上 ± [0, jitter] 的随机偏移（秒），0 = 固定间隔 */
  jitter_seconds: number
  last_checked_at: string | null
  created_by: number
  created_at: string
  updated_at: string
  /** Latest status of the primary model (empty when no history yet) */
  primary_status: MonitorStatus | ''
  /** Latest latency of the primary model in ms (null when no history yet) */
  primary_latency_ms: number | null
  /** Primary model 7-day availability percentage (0-100) */
  availability_7d: number
  /** Latest status per extra model (used for hover tooltip) */
  extra_models_status: ExtraModelStatus[]
  /** 请求自定义快照字段（高级设置） */
  template_id: number | null
  extra_headers: Record<string, string>
  body_override_mode: BodyOverrideMode
  body_override: Record<string, unknown> | null
  /** 检测模式：probe（默认）/ quota / quota_probe */
  check_mode: CheckMode
  /** 配额模式关联的账号 ID；探活模式为 null */
  account_id: number | null
  /** 主模型最近一次配额快照（配额模式；无历史时为 null） */
  latest_quota?: MonitorQuotaSnapshot | null
  // w34 融合点 2：keep OURS（E 类我方独有新增，非上游删除；序列化生产者在册，见文件头 notes）
  /**
   * 渠道级档位（后端经推导产出，非 per-model 直读）。空字符串/缺省 = 无数据，
   * 展示层按空态（横线）呈现，不得显示为失败。
   */
  channel_status?: string
  /** 渠道级权威观测时间；无数据时为空（展示层显示横线，不显示为失败）。 */
  channel_observed_at?: string
}

export interface ExtraModelStatus {
  model: string
  status: MonitorStatus | ''
  latency_ms: number | null
}

export interface ListParams {
  page?: number
  page_size?: number
  provider?: Provider
  enabled?: boolean
  search?: string
}

export interface ListResponse {
  items: ChannelMonitor[]
  total: number
  page: number
  page_size: number
  pages: number
}

export interface CreateParams {
  name: string
  provider: Provider
  api_mode?: APIMode
  // w34 融合点 3：keep OURS 注释；上游"可含路径前缀"的修订暂缓（我方后端仍强制 base origin，见文件头 notes）
  /** 探活模式必填（base origin）；quota 模式可留空 */
  endpoint: string
  /** 探活模式必填；quota 模式可留空 */
  api_key: string
  /** 缺省 probe；antigravity 仅支持 quota */
  check_mode?: CheckMode
  /** 配额模式必填：数据源账号（provider 需与账号平台一致）。
   * update 语义：>0=换绑，0=解绑（切回 probe 模式时前端发 0 清空存量关联）；
   * create 绝不发 0——后端会把 0 存成 &0 触发外键违约。 */
  account_id?: number | null
  primary_model: string
  extra_models?: string[]
  group_name?: string
  enabled?: boolean
  interval_seconds: number
  jitter_seconds?: number
  template_id?: number | null
  extra_headers?: Record<string, string>
  body_override_mode?: BodyOverrideMode
  body_override?: Record<string, unknown> | null
}

// Update request: api_key 空串 = 不修改；clear_template=true 时把 template_id 置空；
// account_id=0 显式解绑关联账号（null = 不动，见 CreateParams 注释）
export type UpdateParams = Partial<CreateParams> & {
  clear_template?: boolean
}

export interface CheckResult {
  model: string
  status: MonitorStatus
  latency_ms: number | null
  ping_latency_ms: number | null
  message: string
  checked_at: string
  /** 配额模式（quota / quota_probe 主模型行）附带的配额快照 */
  quota?: MonitorQuotaSnapshot | null
}

export interface RunNowResponse {
  results: CheckResult[]
}

export interface HistoryItem {
  id: number
  model: string
  status: MonitorStatus
  latency_ms: number | null
  ping_latency_ms: number | null
  message: string
  checked_at: string
  /** 配额快照（配额模式行；探活行为空） */
  quota?: MonitorQuotaSnapshot | null
}

export interface HistoryParams {
  model?: string
  limit?: number
}

export interface HistoryResponse {
  items: HistoryItem[]
}

/**
 * List channel monitors with pagination and filters
 */
export async function list(
  params: ListParams = {},
  options?: { signal?: AbortSignal }
): Promise<ListResponse> {
  const { data } = await apiClient.get<ListResponse>('/admin/channel-monitors', {
    params,
    signal: options?.signal,
  })
  return data
}

/**
 * Get a channel monitor by ID
 */
export async function get(id: number): Promise<ChannelMonitor> {
  const { data } = await apiClient.get<ChannelMonitor>(`/admin/channel-monitors/${id}`)
  return data
}

/**
 * Create a new channel monitor
 */
export async function create(params: CreateParams): Promise<ChannelMonitor> {
  const { data } = await apiClient.post<ChannelMonitor>('/admin/channel-monitors', params)
  return data
}

/**
 * Duplicate a monitor without exposing its stored API key to the browser.
 * Keep the operation key after ambiguous failures so a retry replays the
 * original server-side operation instead of creating another monitor.
 */
const duplicateOperationKeys = new Map<string, string>()

interface DuplicateOperationScope {
  adminID: string
  key: string
}

function getCurrentAdminID(): string | null {
  try {
    const rawUser = globalThis.localStorage?.getItem('auth_user')
    if (!rawUser) return null

    const user: unknown = JSON.parse(rawUser)
    if (typeof user !== 'object' || user === null) return null

    const id = (user as { id?: unknown }).id
    if (typeof id !== 'number' || !Number.isSafeInteger(id) || id <= 0) return null
    return String(id)
  } catch {
    return null
  }
}

function duplicateOperationScope(id: number): DuplicateOperationScope | null {
  const adminID = getCurrentAdminID()
  if (!adminID) return null

  return {
    adminID,
    key: `sub2api:admin:channel-monitor-duplicate:${adminID}:${id}`,
  }
}

function getStoredDuplicateOperationKey(storageKey: string): string | null {
  try {
    return globalThis.sessionStorage?.getItem(storageKey) ?? null
  } catch {
    return null
  }
}

function storeDuplicateOperationKey(storageKey: string, key: string | null): void {
  try {
    if (key) globalThis.sessionStorage?.setItem(storageKey, key)
    else globalThis.sessionStorage?.removeItem(storageKey)
  } catch {
    // In-memory retry protection still works when browser storage is unavailable.
  }
}

export async function duplicate(id: number): Promise<ChannelMonitor> {
  const scope = duplicateOperationScope(id)
  let idempotencyKey = scope
    ? duplicateOperationKeys.get(scope.key) ?? getStoredDuplicateOperationKey(scope.key)
    : null
  if (!idempotencyKey) {
    const requestID = globalThis.crypto?.randomUUID?.() ?? `${Date.now()}-${Math.random().toString(36).slice(2)}`
    idempotencyKey = `channel-monitor-duplicate-${scope?.adminID ?? 'unknown-admin'}-${id}-${requestID}`
  }
  if (scope) {
    duplicateOperationKeys.set(scope.key, idempotencyKey)
    storeDuplicateOperationKey(scope.key, idempotencyKey)
  }

  const { data } = await apiClient.post<ChannelMonitor>(
    `/admin/channel-monitors/${id}/duplicate`,
    undefined,
    { headers: { 'Idempotency-Key': idempotencyKey } }
  )

  if (scope) {
    duplicateOperationKeys.delete(scope.key)
    storeDuplicateOperationKey(scope.key, null)
  }
  return data
}

/**
 * Update an existing channel monitor.
 * api_key field: empty string means "do not modify".
 */
export async function update(id: number, params: UpdateParams): Promise<ChannelMonitor> {
  const { data } = await apiClient.put<ChannelMonitor>(`/admin/channel-monitors/${id}`, params)
  return data
}

/**
 * Delete a channel monitor
 */
export async function del(id: number): Promise<void> {
  await apiClient.delete(`/admin/channel-monitors/${id}`)
}

/**
 * Trigger an immediate manual check for a channel monitor.
 * Returns the latest check results for primary + extra models.
 */
export async function runNow(id: number): Promise<RunNowResponse> {
  const { data } = await apiClient.post<RunNowResponse>(`/admin/channel-monitors/${id}/run`)
  return data
}

/**
 * List historical check results for a monitor.
 */
export async function listHistory(
  id: number,
  params: HistoryParams = {}
): Promise<HistoryResponse> {
  const { data } = await apiClient.get<HistoryResponse>(
    `/admin/channel-monitors/${id}/history`,
    { params }
  )
  return data
}

export const channelMonitorAPI = {
  list,
  get,
  create,
  duplicate,
  update,
  del,
  runNow,
  listHistory,
}

export default channelMonitorAPI
