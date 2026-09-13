/**
 * Admin Promo Intel API endpoints
 * 优惠情报：资讯源管理、情报列表/分诊、每日简报、整理模型设置
 */

import { apiClient } from '../client'

export type PromoIntelSourceCategory =
  | 'announcement'
  | 'changelog'
  | 'pricing'
  | 'activity'
  | 'blog'

export type PromoIntelItemCategory =
  | 'free_quota'
  | 'discount'
  | 'subscription'
  | 'price_change'
  | 'new_model'
  | 'new_product'
  | 'event'
  | 'policy'
  | 'other'

export type PromoIntelRelevance = 'high' | 'medium' | 'low'
export type PromoIntelItemStatus = 'pending' | 'useful' | 'ignored'
export type PromoIntelExtractStatus = 'llm' | 'pending'

export interface PromoIntelSource {
  id: number
  name: string
  vendor: string
  category: PromoIntelSourceCategory
  url: string
  fetch_interval_minutes: number
  enabled: boolean
  llm_extract: boolean
  notes: string
  last_fetched_at: string | null
  last_status: '' | 'ok' | 'error'
  last_error: string
  created_at: string
  updated_at: string
}

export interface PromoIntelItem {
  id: number
  source_id: number
  source_name: string
  vendor: string
  category: PromoIntelItemCategory
  title: string
  summary: string
  details: string
  discount_info: string
  valid_until: string
  url: string
  relevance: PromoIntelRelevance
  status: PromoIntelItemStatus
  extract_status: PromoIntelExtractStatus
  digest_date: string
  raw_excerpt: string
  created_at: string
  updated_at: string
}

export interface SourceListParams {
  page?: number
  page_size?: number
  vendor?: string
  enabled?: boolean
  search?: string
}

export interface ItemListParams {
  page?: number
  page_size?: number
  vendor?: string
  category?: PromoIntelItemCategory | ''
  relevance?: PromoIntelRelevance | ''
  status?: PromoIntelItemStatus | ''
  digest_date?: string
  search?: string
}

export interface ListResponse<T> {
  items: T[]
  total: number
  page: number
  page_size: number
  pages: number
}

export interface SourceCreateParams {
  name: string
  vendor: string
  category?: PromoIntelSourceCategory
  url: string
  fetch_interval_minutes?: number
  enabled?: boolean
  llm_extract?: boolean
  notes?: string
}

export type SourceUpdateParams = Partial<SourceCreateParams>

export interface FetchNowResponse {
  source: PromoIntelSource | null
  content_changed: boolean
  items_created: number
  items_updated: number
  skipped_reason: string
}

export interface Briefing {
  date: string
  total: number
  high_count: number
  pending: number
  by_vendor: Record<string, number>
  by_category: Record<string, number>
  items: PromoIntelItem[]
}

export interface IntelSettings {
  enabled: boolean
  source: 'self' | 'external'
  protocol: 'openai' | 'anthropic'
  self_api_key_id: number
  self_api_key_name: string
  self_model: string
  llm_configured: boolean
  llm_base_url: string
  llm_api_key_set: boolean
  llm_api_key_mask: string
  llm_model: string
}

export interface AdminAPIKeyRef {
  id: number
  name: string
}

export interface IntelSettingsUpdateParams {
  enabled?: boolean
  source?: 'self' | 'external'
  protocol?: 'openai' | 'anthropic'
  self_api_key_id?: number
  self_model?: string
  /** 空串/回显掩码 = 保持不变；"-" = 显式清空 */
  llm_api_key?: string
  llm_base_url?: string
  llm_model?: string
}

export async function listSources(
  params: SourceListParams = {},
  options?: { signal?: AbortSignal }
): Promise<ListResponse<PromoIntelSource>> {
  const { data } = await apiClient.get<ListResponse<PromoIntelSource>>('/admin/promo-intel/sources', {
    params,
    signal: options?.signal,
  })
  return data
}

export async function createSource(params: SourceCreateParams): Promise<PromoIntelSource> {
  const { data } = await apiClient.post<PromoIntelSource>('/admin/promo-intel/sources', params)
  return data
}

export async function updateSource(id: number, params: SourceUpdateParams): Promise<PromoIntelSource> {
  const { data } = await apiClient.put<PromoIntelSource>(`/admin/promo-intel/sources/${id}`, params)
  return data
}

export async function deleteSource(id: number): Promise<void> {
  await apiClient.delete(`/admin/promo-intel/sources/${id}`)
}

export async function fetchSourceNow(id: number): Promise<FetchNowResponse> {
  const { data } = await apiClient.post<FetchNowResponse>(`/admin/promo-intel/sources/${id}/fetch`)
  return data
}

export async function listItems(
  params: ItemListParams = {},
  options?: { signal?: AbortSignal }
): Promise<ListResponse<PromoIntelItem>> {
  const { data } = await apiClient.get<ListResponse<PromoIntelItem>>('/admin/promo-intel/items', {
    params,
    signal: options?.signal,
  })
  return data
}

export async function updateItemStatus(id: number, status: PromoIntelItemStatus): Promise<PromoIntelItem> {
  const { data } = await apiClient.put<PromoIntelItem>(`/admin/promo-intel/items/${id}/status`, { status })
  return data
}

export async function getBriefing(date?: string): Promise<Briefing> {
  const { data } = await apiClient.get<Briefing>('/admin/promo-intel/briefing', {
    params: date ? { date } : {},
  })
  return data
}

export async function listApiKeys(): Promise<{ items: AdminAPIKeyRef[] }> {
  const { data } = await apiClient.get<{ items: AdminAPIKeyRef[] }>('/admin/promo-intel/api-keys')
  return data
}

export async function getSettings(): Promise<IntelSettings> {
  const { data } = await apiClient.get<IntelSettings>('/admin/promo-intel/settings')
  return data
}

export async function updateSettings(params: IntelSettingsUpdateParams): Promise<IntelSettings> {
  const { data } = await apiClient.put<IntelSettings>('/admin/promo-intel/settings', params)
  return data
}

export async function testSettings(): Promise<{ message: string }> {
  const { data } = await apiClient.post<{ message: string }>('/admin/promo-intel/settings/test')
  return data
}

export const promoIntelAPI = {
  listSources,
  createSource,
  updateSource,
  deleteSource,
  fetchSourceNow,
  listItems,
  updateItemStatus,
  getBriefing,
  listApiKeys,
  getSettings,
  updateSettings,
  testSettings,
}

export default promoIntelAPI
