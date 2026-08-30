import { apiClient } from '@/api/client'
import type {
  XianyuOverview,
  XianyuWorkerConfig,
  XianyuAccount,
  XianyuProduct,
  XianyuItemPool,
  XianyuBindingRule,
  XianyuOrderClaim,
  XianyuLoginSessionStatus,
  PaginatedResponse
} from '@/types'

export interface XianyuDeliveryFilter {
  status?: string
  search?: string
  page?: number
  page_size?: number
}

export async function getOverview(options?: { signal?: AbortSignal }): Promise<XianyuOverview> {
  const { data } = await apiClient.get<XianyuOverview>('/admin/xianyu/overview', { signal: options?.signal })
  return data
}

export async function listWorkerConfigs(options?: { signal?: AbortSignal }): Promise<XianyuWorkerConfig[]> {
  const { data } = await apiClient.get<XianyuWorkerConfig[]>('/admin/xianyu/worker-configs', { signal: options?.signal })
  return data
}

export async function saveWorkerConfig(input: Partial<XianyuWorkerConfig> & { api_token?: string }): Promise<XianyuWorkerConfig> {
  const { data } = await apiClient.post<XianyuWorkerConfig>('/admin/xianyu/worker-configs', input)
  return data
}

export async function checkHealth(): Promise<void> {
  await apiClient.post('/admin/xianyu/health/check')
}

export async function listAccounts(options?: { signal?: AbortSignal }): Promise<XianyuAccount[]> {
  const { data } = await apiClient.get<XianyuAccount[]>('/admin/xianyu/accounts', { signal: options?.signal })
  return data
}

export async function syncAccounts(): Promise<void> {
  await apiClient.post('/admin/xianyu/accounts/sync')
}

export async function enableAccount(accountId: string): Promise<void> {
  await apiClient.post('/admin/xianyu/accounts/enable', { account_id: accountId })
}

export async function disableAccount(accountId: string): Promise<void> {
  await apiClient.post('/admin/xianyu/accounts/disable', { account_id: accountId })
}

export async function refreshCookie(accountId: string): Promise<XianyuAccount> {
  const { data } = await apiClient.post<XianyuAccount>('/admin/xianyu/accounts/refresh-cookie', { account_id: accountId })
  return data
}

export async function clearCredentials(accountId: string): Promise<void> {
  await apiClient.post('/admin/xianyu/accounts/clear-credentials', { account_id: accountId })
}

export async function createLoginSession(accountId: string): Promise<XianyuLoginSessionStatus> {
  const { data } = await apiClient.post<XianyuLoginSessionStatus>('/admin/xianyu/accounts/login-session', { account_id: accountId })
  return data
}

export async function queryLoginSession(accountId: string, options?: { signal?: AbortSignal }): Promise<XianyuLoginSessionStatus> {
  const { data } = await apiClient.get<XianyuLoginSessionStatus>(`/admin/xianyu/accounts/${encodeURIComponent(accountId)}/login-session`, { signal: options?.signal })
  return data
}

export async function listProducts(options?: { signal?: AbortSignal }): Promise<XianyuProduct[]> {
  const { data } = await apiClient.get<XianyuProduct[]>('/admin/xianyu/products', { signal: options?.signal })
  return data
}

export async function syncProducts(): Promise<void> {
  await apiClient.post('/admin/xianyu/products/sync')
}

export async function bindProduct(productId: number, poolId?: number | null): Promise<void> {
  await apiClient.post('/admin/xianyu/products/bind', { product_id: productId, pool_id: poolId ?? null })
}

export async function listBindingRules(options?: { signal?: AbortSignal }): Promise<XianyuBindingRule[]> {
  const { data } = await apiClient.get<XianyuBindingRule[]>('/admin/xianyu/binding-rules', { signal: options?.signal })
  return data
}

export async function saveBindingRule(input: Partial<XianyuBindingRule>): Promise<XianyuBindingRule> {
  const { data } = await apiClient.post<XianyuBindingRule>('/admin/xianyu/binding-rules', input)
  return data
}

export async function listItemPools(options?: { signal?: AbortSignal }): Promise<XianyuItemPool[]> {
  const { data } = await apiClient.get<XianyuItemPool[]>('/admin/xianyu/item-pools', { signal: options?.signal })
  return data
}

export async function saveItemPool(input: Partial<XianyuItemPool>): Promise<XianyuItemPool> {
  const { data } = await apiClient.post<XianyuItemPool>('/admin/xianyu/item-pools', input)
  return data
}

export async function listDeliveries(filter: XianyuDeliveryFilter = {}, options?: { signal?: AbortSignal }): Promise<PaginatedResponse<XianyuOrderClaim>> {
  const { data } = await apiClient.get<PaginatedResponse<XianyuOrderClaim>>('/admin/xianyu/deliveries', {
    params: {
      status: filter.status || undefined,
      search: filter.search || undefined,
      page: filter.page || 1,
      page_size: filter.page_size || 20
    },
    signal: options?.signal
  })
  return data
}

export async function resendDelivery(orderNo: string): Promise<string> {
  const { data } = await apiClient.post<{ code: string }>('/admin/xianyu/deliveries/resend', { order_no: orderNo })
  return data.code
}

export interface XianyuControlSettings {
  delivery_enabled: boolean
  account_auto_refresh: boolean
  product_auto_bind: boolean
  sync_interval_minutes: number
}

export async function getSettings(options?: { signal?: AbortSignal }): Promise<XianyuControlSettings> {
  const { data } = await apiClient.get<XianyuControlSettings>('/admin/xianyu/settings', { signal: options?.signal })
  return data
}

export async function saveSettings(input: Partial<XianyuControlSettings>): Promise<void> {
  await apiClient.put('/admin/xianyu/settings', input)
}

export const xianyuAPI = {
  getOverview,
  listWorkerConfigs,
  saveWorkerConfig,
  checkHealth,
  listAccounts,
  syncAccounts,
  enableAccount,
  disableAccount,
  refreshCookie,
  clearCredentials,
  createLoginSession,
  queryLoginSession,
  listProducts,
  syncProducts,
  bindProduct,
  listBindingRules,
  saveBindingRule,
  listItemPools,
  saveItemPool,
  listDeliveries,
  resendDelivery,
  getSettings,
  saveSettings
}

export default xianyuAPI
