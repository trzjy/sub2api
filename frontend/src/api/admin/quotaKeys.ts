/**
 * Admin Quota Keys (Cursor 激活码) API endpoints
 *
 * 封装 C5b 落地的 8 个 admin 端点（方案 §4.4 / §4.5）：
 *   GET    /admin/quota_keys
 *   POST   /admin/quota_keys
 *   GET    /admin/quota_keys/:id
 *   PUT    /admin/quota_keys/:id
 *   DELETE /admin/quota_keys/:id
 *   POST   /admin/quota_keys/:id/refresh
 *   POST   /admin/quota_keys/:id/clear_exhausted
 *   POST   /admin/quota_keys/sweep_expired
 *
 * 安全约束（F1）：前端绝不持有激活码原文（code）；仅传输一次用于创建，
 * 之后只接收脱敏的 code_masked。明文 code 不进入 state / 日志 / URL。
 *
 * 类型与 accounts.ts 同型：apiClient + 本地类型定义（不污染 @/types 全局契约）。
 */

import { apiClient } from '../client'
import type { QuotaKeyUsageProjection } from '@/types'

/** 激活码脱敏视图（与后端 QuotaKeyStatus / dto.QuotaKey 对齐）。 */
export interface QuotaKey {
  id: number
  status: string
  exhausted: boolean
  code_masked: string
  // 契约口径：列表端点（dto.QuotaKey）与详情端点（内嵌 QuotaKeyStatus）返回同一份
  // 行级元数据——expires_at / last_refresh_at / last_refresh_error / notes 两端都有值，
  // 不存在"列表缺字段、详情才有"的差异。字段投影在服务层与 handler 各只有一处实现
  // （service.QuotaKeyStatus → maskedQuotaKey → dto.QuotaKey），任何端点都不得再内联
  // 复制字段子集，否则列表会静默丢字段（历史上的"到期时间未知"就是这么来的）。
  last_refresh_at: string | null
  last_refresh_error: string | null
  expires_at: string | null
  // 账号层字段由详情端点回填（列表与详情同源）；账号层不承载 key 级额度数值
  // （两层额度独立，见 QuotaKeysView 的 key 级用量弹窗）。
  account_count?: number
  working_count?: number
  notes?: string | null
  // 激活码优先级（数值越小越优先，与账号 priority 同义）。该码名下账号的优先级跟随此值：
  // 拉号入库时写入，修改时后端同事务同步（日限额码取小值优先消耗，月总限额码取大值兜底）。
  priority: number
  // 并发数（B 批上移到激活码管理，不含余额查询）：null / 省略表示沿用所属账号列的默认 3；
  // 正整数（≥ 1，或按表单校验 ≥ 0）时后端同事务同步该码名下全部账号的并发数。
  concurrency?: number | null
  // 负载因子（B 批上移）：null / 省略表示清空（卸载）该码名下账号的负载因子；正整数 1..10000。
  load_factor?: number | null
  // 计费倍率（B 批上移）：≥ 0，默认 1.0，允许 0；后端同事务同步该码名下全部账号的计费倍率。
  rate_multiplier?: number
  // 来源额度快照投影（方案 §25.6 消费闭包⑤ / §25.7-B）。
  // 口径：quota_keys.usage_snapshot 的**已提交**值，列表/详情/probe 与账号行同源同值。
  // 无快照时省略（不是 0）；查询失败时 total/used 是上次成功值，须带时间并标注失败。
  usage?: QuotaKeyUsageProjection
  // 上游标识（引用 quota_upstreams.key）。列表端点回填后前端据此匹配上游显示名；
  // 后端尚未在 quota_keys 列表响应中返回该字段时本项为 undefined，前端回落显示原始标识。
  upstream_key?: string | null
  // 模型限制与映射（码级权威值，与 credentials.model_mapping 同构：
  // 白名单存恒等映射 {"m":"m"}，映射存 {"from":"to"}）。
  // null / 省略表示码级未显式覆盖，沿用上游默认实供清单。
  model_mapping?: Record<string, string> | null
  // 码级权威分组 ID 列表。null / 省略 = 未显式指定（拉号按模型家族自动推导挂组）；
  // [] = 显式解绑全部分组；[1,2] = 码级权威，编辑时后端同事务同步名下账号的 account_groups。
  group_ids?: number[] | null
}

/**
 * 激活码下的账号项（脱敏标量，仅展示用）。
 *
 * 字段 = 后端 `handler/admin.quotaKeyAccountItem` 实际返回 ∩ 视图实际读取：
 * id / name / platform / status。后端仍会多返 type 与 created_at，但视图不消费，
 * 故不在此声明（前端只声明真正依赖的字段，避免再次凭印象手抄出幻影字段）。
 */
export interface QuotaKeyAccount {
  id: number
  name: string
  platform: string
  status: string
}

/** 激活码详情：脱敏视图 + 其下账号分页。 */
export interface QuotaKeyDetail extends QuotaKey {
  accounts: QuotaKeyAccount[]
  /** 账号总数（未软删、不分页）。与 account_count 同源，供分页控件用。 */
  total: number
  page: number
  page_size: number
}

/** 创建激活码请求（仅此处一次性接收明文 code，前端不持久化）。 */
export interface CreateQuotaKeyRequest {
  code: string
  expires_at?: string | null
  notes?: string | null
  /** 调度优先级（数值越小越优先）；缺省不传时后端按默认 1 落库。 */
  priority?: number
  /**
   * 所属上游的稳定标识（对应 quota_upstreams.key，如 'shareai' / 'kn_cy'）。
   * 缺省/空串 = 不指定，由后端落列默认值（'shareai'）。
   * 传非空值时后端会校验该上游存在且未软删，不存在则拒绝创建（失败关闭，不静默回落）。
   */
  upstream_key?: string | null
  // 并发数（B 批上移）：null / 省略表示沿用账号列默认 3；正整数（≥ 1，或按表单校验 ≥ 0）时
  // 后端同事务同步该码名下账号的并发数。
  concurrency?: number | null
  // 负载因子（B 批上移）：null / 省略表示清空该码名下账号的负载因子；正整数 1..10000。
  load_factor?: number | null
  // 计费倍率（B 批上移）：≥ 0，默认 1.0，允许 0；后端同事务同步该码名下账号的计费倍率。
  rate_multiplier?: number
  // 模型限制与映射（码级权威值，与 credentials.model_mapping 同构）：
  // 不传/undefined = 创建时沿用上游默认实供清单；显式传入则必须至少包含 1 个有效模型，
  // 空对象 {} 禁止落库（后端校验拒绝）。null 表示显式清空码级覆盖。
  model_mapping?: Record<string, string> | null
  // 码级权威分组 ID 列表。显式给出即作为码级权威分组，空数组表示不绑定任何分组。
  group_ids?: number[] | null
}

/** 编辑激活码请求（expires_at / notes / priority）。 */
// 注意：上游归属（upstream_key）创建后不可改——本仓没有改归属的后端能力，
// 上游决定该码由哪个拉号客户端消费，改归属会让已入库账号与拉号链路语义错配。
export interface UpdateQuotaKeyRequest {
  expires_at?: string | null
  notes?: string | null
  /** 不传表示不改；传值时后端同事务同步该码名下账号的 priority。 */
  priority?: number
  // 并发数（B 批上移）：不传表示不改；传 null / 省略表示沿用账号列默认 3；正整数
  // （≥ 1，或按表单校验 ≥ 0）时后端同事务同步该码名下账号的并发数。
  concurrency?: number | null
  // 负载因子（B 批上移）：不传表示不改；传 null / 省略表示清空该码名下账号的负载因子；正整数 1..10000。
  load_factor?: number | null
  // 计费倍率（B 批上移）：不传表示不改；≥ 0，默认 1.0，允许 0；后端同事务同步该码名下账号的计费倍率。
  rate_multiplier?: number
  // 模型限制与映射（码级权威值，与 credentials.model_mapping 同构）：
  // 不传/undefined = 编辑时不改模型限制；显式传入则必须至少包含 1 个有效模型，
  // 空对象 {} 禁止落库（后端校验拒绝）。null 表示显式清空码级覆盖。
  model_mapping?: Record<string, string> | null
  // 码级权威分组 ID 列表。不传 = 不改；传 null 或数组 = 覆盖；空数组 = 解绑全部。
  group_ids?: number[] | null
}

export interface QuotaKeyListResponse {
  items: QuotaKey[]
  total: number
  page: number
  page_size: number
  pages: number
  quota_key_summaries?: QuotaKey[]
}

export interface SweepExpiredResponse {
  swept: number
}

/** 列出全部激活码（后端返回全站统一的分页信封）。 */
export async function list(): Promise<QuotaKeyListResponse> {
  const { data } = await apiClient.get<QuotaKeyListResponse>('/admin/quota_keys')
  return data
}

/** 创建激活码（提交明文 code 一次，响应仅含 code_masked）。 */
export async function create(payload: CreateQuotaKeyRequest): Promise<QuotaKey> {
  const { data } = await apiClient.post<QuotaKey>('/admin/quota_keys', payload)
  return data
}

/**
 * 获取单个激活码及其下账号列表（账号列表分页）。
 *
 * 后端详情端点支持 page / page_size（response.ParsePagination），默认 page=1、page_size=20；
 * 响应回带 total / page / page_size 供分页控件使用。不传参时保持既有行为。
 */
export async function get(
  id: number,
  page: number = 1,
  pageSize: number = 20
): Promise<QuotaKeyDetail> {
  const { data } = await apiClient.get<QuotaKeyDetail>(`/admin/quota_keys/${id}`, {
    params: { page, page_size: pageSize }
  })
  return data
}

/** 编辑激活码（expires_at / notes）。 */
export async function update(id: number, payload: UpdateQuotaKeyRequest): Promise<QuotaKey> {
  const { data } = await apiClient.put<QuotaKey>(`/admin/quota_keys/${id}`, payload)
  return data
}

/** 删除激活码（唯一 DELETE 路径）。 */
export async function remove(id: number): Promise<{ message: string }> {
  const { data } = await apiClient.delete<{ message: string }>(`/admin/quota_keys/${id}`)
  return data
}

/** 手动拉号（刷新激活码状态）。 */
export async function refresh(id: number): Promise<QuotaKey> {
  const { data } = await apiClient.post<QuotaKey>(`/admin/quota_keys/${id}/refresh`)
  return data
}

/** 解除 exhausted 标记。 */
export async function clearExhausted(id: number): Promise<QuotaKey> {
  const { data } = await apiClient.post<QuotaKey>(`/admin/quota_keys/${id}/clear_exhausted`)
  return data
}

/** 一键清理过期激活码。 */
export async function sweepExpired(): Promise<SweepExpiredResponse> {
  const { data } = await apiClient.post<SweepExpiredResponse>('/admin/quota_keys/sweep_expired')
  return data
}

export type QuotaKeyStatusToggle = 'active' | 'disabled'

/** 启用 / 停用激活码（仅 active ↔ disabled；已过期的码不允许直接启用）。 */
export async function setStatus(id: number, status: QuotaKeyStatusToggle): Promise<QuotaKey> {
  const { data } = await apiClient.post<QuotaKey>(`/admin/quota_keys/${id}/status`, { status })
  return data
}

/**
 * 用量查询（D2-a probe_quota）返回的 key_info 摘要。
 *
 * 口径：全部字段均为「激活码（key）层」（第一层）——总额度 max / 已用 used / 期限
 * expired_at / 设备 device_* / 取号结果 accounts_written。账号余额属第二层，
 * 不由本结构复制派生（见 QuotaKeysView 两层展示）。
 */
export interface KeyQuotaInfo {
  group: string
  status_code: number
  max: number
  used: number
  device_count: number
  device_max_count: number
  mode: number
  expired_at: string | null
  accounts_written: number
}

/**
 * 用量查询（probe_quota）响应：既有 key_info 摘要 + 来源额度快照投影（方案 §25.6⑤）。
 *
 * usage 取自**已提交**的 quota_keys.usage_snapshot，与列表/详情/账号行同源同值；
 * 无快照时省略（不是 0），不得把 probe 的未落库结果当已提交事实展示。
 */
export interface ProbeQuotaResponse extends KeyQuotaInfo {
  usage?: QuotaKeyUsageProjection
}

/**
 * 上游条目（quota_upstreams 注册表）展示投影。
 * 标识 key 与类型 type 由后端决定、前端只读不可改（改 key 会打断 quota_keys.upstream_key
 * 引用，改 type 会改变拉号客户端语义）。响应体不含任何凭据。
 */
export interface QuotaUpstream {
  id: number
  key: string
  name: string
  type: string
  renewal_url?: string | null
  notes?: string | null
}

/** 上游条目列表响应：{ items: QuotaUpstream[] }。 */
export interface QuotaUpstreamListResponse {
  items: QuotaUpstream[]
}

/** 编辑上游条目请求：仅 name / renewal_url / notes 可写；key 与 type 不可改。 */
export interface UpdateQuotaUpstreamRequest {
  name: string
  renewal_url?: string | null
  notes?: string | null
}

/** 列出全部上游条目（标识 + 显示名 + 类型 + 续费 URL）。 */
export async function listUpstreams(): Promise<QuotaUpstreamListResponse> {
  const { data } = await apiClient.get<QuotaUpstreamListResponse>('/admin/quota_upstreams')
  return data
}

/** 编辑上游条目（仅显示名 / 续费 URL / 备注；key 与 type 不可改）。 */
export async function updateUpstream(
  key: string,
  payload: UpdateQuotaUpstreamRequest
): Promise<QuotaUpstream> {
  const { data } = await apiClient.put<QuotaUpstream>(
    `/admin/quota_upstreams/${encodeURIComponent(key)}`,
    payload
  )
  return data
}

/** 手动触发某激活码的 key 级用量查询（lookup），返回既有摘要 + 已提交额度快照投影。 */
export async function probeQuota(id: number): Promise<ProbeQuotaResponse> {
  const { data } = await apiClient.post<ProbeQuotaResponse>(`/admin/quota_keys/${id}/probe_quota`)
  return data
}

/**
 * 同步上游实供模型清单返回（与 accounts.ts 的 syncUpstreamModels 同义，码级模型限制用）。
 * 复用其结构：models 为上游实供模型 ID 列表；warnings 为同步过程中的非致命告警
 * （如部分模型探活失败）。前端据此派生码级 model_mapping 权威值
 * （白名单存恒等映射 {"m":"m"}，映射存 {"from":"to"}）。
 */
export interface SyncUpstreamModelsResponse {
  models: string[]
  warnings?: { code?: string; message?: string }[]
}

/**
 * 同步某激活码名下账号的上游实供模型清单。
 * 后端路径：POST /admin/quota_keys/:id/models/sync-upstream。
 */
export async function syncUpstreamModels(id: number): Promise<SyncUpstreamModelsResponse> {
  const { data } = await apiClient.post<SyncUpstreamModelsResponse>(`/admin/quota_keys/${id}/models/sync-upstream`)
  return data
}

const quotaKeysAPI = {
  list,
  create,
  get,
  update,
  remove,
  refresh,
  clearExhausted,
  sweepExpired,
  setStatus,
  probeQuota,
  listUpstreams,
  updateUpstream,
  syncUpstreamModels
}

export default quotaKeysAPI
