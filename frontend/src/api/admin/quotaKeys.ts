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

/** 激活码脱敏视图（与后端 QuotaKeyStatus / dto.QuotaKey 对齐）。 */
export interface QuotaKey {
  id: number
  status: string
  exhausted: boolean
  code_masked: string
  // 契约口径（P1-B3 对齐后端，不新增字段）：
  // - 列表端点（dto.QuotaKey）只返 id / code_masked / status / exhausted /
  //   account_count / working_count，expires_at / last_refresh_* 为空。
  // - 详情端点额外内嵌 QuotaKeyStatus，才带 last_refresh_at / last_refresh_error /
  //   expires_at（QuotaKeyDetail 沿用本类型）。
  last_refresh_at: string | null
  last_refresh_error: string | null
  expires_at: string | null
  // 账号层字段由详情端点回填（列表与详情同源）；账号层不承载 key 级额度数值
  // （两层额度独立，见 QuotaKeysView 的 key 级用量弹窗）。
  account_count?: number
  working_count?: number
  notes?: string | null
  created_at?: string
  updated_at?: string
}

/** 激活码下的账号项（脱敏标量，仅展示用）。 */
export interface QuotaKeyAccount {
  id: number
  name: string
  platform: string
  status: string
  quota_key_id?: number | null
  quota_key_label?: string | null
}

/** 激活码详情：脱敏视图 + 其下账号列表。 */
export interface QuotaKeyDetail extends QuotaKey {
  accounts: QuotaKeyAccount[]
}

/** 创建激活码请求（仅此处一次性接收明文 code，前端不持久化）。 */
export interface CreateQuotaKeyRequest {
  code: string
  expires_at?: string | null
  notes?: string | null
}

/** 编辑激活码请求（仅允许 expires_at / notes）。 */
export interface UpdateQuotaKeyRequest {
  expires_at?: string | null
  notes?: string | null
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

/** 获取单个激活码及其下账号列表。 */
export async function get(id: number): Promise<QuotaKeyDetail> {
  const { data } = await apiClient.get<QuotaKeyDetail>(`/admin/quota_keys/${id}`)
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

/** 入库可配置（D2-a）：单条模型引用。 */
export interface AutoEntryModelRef {
  id: string
  vendor: string
  probe_status?: string
}

/** 入库可配置（D2-a）：入库默认值（priority/rate_multiplier 落 account 列；concurrency 留空=沿用列默认 3）。 */
export interface AutoEntryDefaults {
  concurrency: number | null
  priority: number
  rate_multiplier: number
}

/** 入库可配置（D2-a）：完整配置（settings 表 cursor_auto_entry_config 的镜像）。 */
export interface AutoEntryConfig {
  vendors: Record<string, boolean>
  models: AutoEntryModelRef[]
  entry_defaults: AutoEntryDefaults
  // 可选模型全集（CUR-F2）：后端实供清单 CursorRealSupplyModels 投影，前端"厂商 → 模型多选"
  // 的渲染源。只读派生，不回写。取消勾选的模型仍从中渲染，可重新勾回。
  available_models?: AutoEntryModelRef[]
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

/** 读取 cursor 入库可配置（厂商勾选 + 模型清单 + 入库参数）。 */
export async function getAutoEntryConfig(): Promise<AutoEntryConfig> {
  const { data } = await apiClient.get<AutoEntryConfig>('/admin/quota_keys/auto_entry_config')
  return data
}

/** 校验并持久化 cursor 入库可配置。 */
export async function updateAutoEntryConfig(payload: AutoEntryConfig): Promise<AutoEntryConfig> {
  const { data } = await apiClient.put<AutoEntryConfig>('/admin/quota_keys/auto_entry_config', payload)
  return data
}

/** 手动触发某激活码的 key 级用量查询（lookup），写额度快照到其全部账号。 */
export async function probeQuota(id: number): Promise<KeyQuotaInfo> {
  const { data } = await apiClient.post<KeyQuotaInfo>(`/admin/quota_keys/${id}/probe_quota`)
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
  getAutoEntryConfig,
  updateAutoEntryConfig,
  probeQuota
}

export default quotaKeysAPI
