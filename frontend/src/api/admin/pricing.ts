/**
 * Admin pricing console API.
 *
 * 价格管理中心：远程价格表同步状态/手动同步、全局价格目录（来源分层）、
 * 未覆盖模型双通道扫描、生效价试算与全局自定义价格层 CRUD。
 * 所有价格字段单位：每 token USD（展示层负责换算为每百万 token）。
 */

import { apiClient } from '../client'

export interface PricingSyncStatus {
  remote_url: string
  hash_url: string
  data_file: string
  model_count: number
  last_updated: string
  local_hash: string
  last_attempt_at: string
  last_error: string
  syncing: boolean
  scheduler_enabled: boolean
  hash_check_interval_minutes: number
  update_interval_hours: number
}

export interface PricingGapEntry {
  model: string
  first_seen: string
  last_seen: string
  count: number
}

export interface CustomLayerSummary {
  entries: number
  enabled: number
}

export interface PricingStatusResponse {
  sync: PricingSyncStatus
  custom: CustomLayerSummary
  live_gaps: PricingGapEntry[]
}

export interface CatalogEntry {
  model: string
  source: 'custom' | 'litellm' | 'fallback' | string
  custom_id?: number
  billing_mode?: string
  input_per_mtok: number
  output_per_mtok: number
  cache_write_per_mtok: number
  cache_read_per_mtok: number
  token_pricing_absent?: boolean
}

export interface CatalogResponse {
  items: CatalogEntry[]
  total: number
}

export interface ModelStatBrief {
  model: string
  requests: number
  total_tokens: number
  actual_cost: number
}

export interface UncoveredEntry {
  model: string
  verdict: 'uncovered' | 'fuzzy' | string
  references: string[]
  usage?: ModelStatBrief
  zero_cost_only?: boolean
  input_per_mtok?: number
  output_per_mtok?: number
}

export interface UncoveredResponse {
  items: UncoveredEntry[]
  scanned: number
  window: string
  scanned_at: string
}

export interface PricingPreview {
  model: string
  source: string
  billing_mode: string
  group_id: number
  group_name: string
  rate_multiplier: number
  input_per_mtok: number
  output_per_mtok: number
  cache_write_per_mtok: number
  cache_read_per_mtok: number
  sample_input_cost: number
  sample_output_cost: number
}

export interface PricingInterval {
  id?: number
  pricing_id?: number
  min_tokens: number
  max_tokens?: number | null
  tier_label?: string
  input_price?: number | null
  output_price?: number | null
  cache_write_price?: number | null
  cache_read_price?: number | null
  input_multiplier?: number | null
  output_multiplier?: number | null
  cache_write_multiplier?: number | null
  cache_read_multiplier?: number | null
  per_request_price?: number | null
  sort_order?: number
}

export interface CustomModelPricing {
  id: number
  models: string[]
  billing_mode: string
  input_price?: number | null
  output_price?: number | null
  cache_write_price?: number | null
  cache_read_price?: number | null
  fast_multiplier?: number | null
  flex_multiplier?: number | null
  image_input_price?: number | null
  image_output_price?: number | null
  per_request_price?: number | null
  intervals: PricingInterval[]
  enabled: boolean
  remark: string
  created_by?: number
  created_at?: string
  updated_at?: string
}

export interface CustomModelPricingPayload {
  models: string[]
  billing_mode: string
  input_price?: number | null
  output_price?: number | null
  cache_write_price?: number | null
  cache_read_price?: number | null
  fast_multiplier?: number | null
  flex_multiplier?: number | null
  image_input_price?: number | null
  image_output_price?: number | null
  per_request_price?: number | null
  intervals: PricingInterval[]
  enabled: boolean
  remark: string
}

export async function getStatus(): Promise<PricingStatusResponse> {
  const { data } = await apiClient.get('/admin/pricing/status')
  return data
}

export async function syncNow(): Promise<PricingStatusResponse> {
  const { data } = await apiClient.post('/admin/pricing/sync')
  return data
}

export async function getCatalog(params: {
  search?: string
  source?: string
  page?: number
  page_size?: number
}): Promise<CatalogResponse> {
  const { data } = await apiClient.get('/admin/pricing/catalog', { params })
  return data
}

export async function getUncovered(days = 30): Promise<UncoveredResponse> {
  const { data } = await apiClient.get('/admin/pricing/uncovered', { params: { days } })
  return data
}

export async function getPreview(model: string, groupId = 0): Promise<PricingPreview> {
  const { data } = await apiClient.get('/admin/pricing/preview', { params: { model, group_id: groupId } })
  return data
}

export async function listCustom(): Promise<CustomModelPricing[]> {
  const { data } = await apiClient.get('/admin/pricing/custom')
  return data
}

export async function getCustom(id: number): Promise<CustomModelPricing> {
  const { data } = await apiClient.get(`/admin/pricing/custom/${id}`)
  return data
}

export async function createCustom(payload: CustomModelPricingPayload): Promise<CustomModelPricing> {
  const { data } = await apiClient.post('/admin/pricing/custom', payload)
  return data
}

export async function updateCustom(id: number, payload: CustomModelPricingPayload): Promise<CustomModelPricing> {
  const { data } = await apiClient.put(`/admin/pricing/custom/${id}`, payload)
  return data
}

export async function deleteCustom(id: number): Promise<{ message: string }> {
  const { data } = await apiClient.delete(`/admin/pricing/custom/${id}`)
  return data
}

export const pricingAPI = {
  getStatus,
  syncNow,
  getCatalog,
  getUncovered,
  getPreview,
  listCustom,
  getCustom,
  createCustom,
  updateCustom,
  deleteCustom
}

export default pricingAPI
