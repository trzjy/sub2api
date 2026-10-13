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

/** 激活码脱敏视图（与后端 QuotaKeyStatus DTO 对齐）。 */
export interface QuotaKey {
  id: number
  platform: string
  status: string
  exhausted: boolean
  code_masked: string
  last_refresh_at: string | null
  last_refresh_error: string | null
  expires_at: string | null
  // 以下字段派发单要求展示；后端当前 QuotaKeyStatus 未必返回，故可选。
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

export interface SweepExpiredResponse {
  cleared: number
}

/** 列出全部激活码（后端 QuotaKeyService.List 返回切片，非分页）。 */
export async function list(): Promise<QuotaKey[]> {
  const { data } = await apiClient.get<QuotaKey[]>('/admin/quota_keys')
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

const quotaKeysAPI = {
  list,
  create,
  get,
  update,
  remove,
  refresh,
  clearExhausted,
  sweepExpired
}

export default quotaKeysAPI
