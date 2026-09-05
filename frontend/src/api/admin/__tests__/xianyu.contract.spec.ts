import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import axios from 'axios'
import type { AxiosInstance } from 'axios'

vi.mock('@/i18n', () => ({
  getLocale: () => 'zh-CN',
}))

vi.mock('@/stores', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }),
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }),
}))

type AdapterResponse = {
  status: number
  data: unknown
  headers: Record<string, string>
  config: Record<string, unknown>
  statusText: string
}

function success(data: unknown): AdapterResponse {
  return { status: 200, data: { code: 0, message: 'ok', data }, headers: {}, config: {}, statusText: 'OK' }
}

function adapterResponse(data: unknown): AdapterResponse {
  return { status: 200, data, headers: {}, config: {}, statusText: 'OK' }
}

describe('xianyu admin api contract', () => {
  let apiClient: AxiosInstance
  let adapter: ReturnType<typeof vi.fn>

  beforeEach(async () => {
    localStorage.clear()
    window.history.replaceState({}, '', '/')
    vi.resetModules()
    const client = await import('@/api/client')
    apiClient = client.apiClient
    adapter = vi.fn()
    apiClient.defaults.adapter = adapter
  })

  afterEach(() => {
    vi.restoreAllMocks()
    vi.unstubAllEnvs()
  })

  it('getOverview unwraps standard { code, data } exactly once', async () => {
    const overview = { worker_healthy: true, enabled_accounts: 2, running_tasks: 1, unmapped_products: 0, pools: [], today_delivered: 1, today_failed: 0, pending_deliveries: 0 }
    adapter.mockResolvedValue(success(overview))
    const { getOverview } = await import('@/api/admin/xianyu')
    await expect(getOverview()).resolves.toEqual(overview)
  })

  it('listAccounts returns account array after interceptor unwrap', async () => {
    const accounts = [{ id: 1, account_id: 'a1', nickname: 'n' }]
    adapter.mockResolvedValue(success(accounts))
    const { listAccounts } = await import('@/api/admin/xianyu')
    await expect(listAccounts()).resolves.toEqual(accounts)
  })

  it('listDeliveries returns paginated payload after interceptor unwrap', async () => {
    const page = { items: [{ order_no: 'o1' }], total: 1, page: 1, page_size: 20, pages: 1 }
    adapter.mockResolvedValue(success(page))
    const { listDeliveries } = await import('@/api/admin/xianyu')
    await expect(listDeliveries()).resolves.toEqual(page)
  })

  it('listWorkerDeliveries returns paginated Worker delivery payload after interceptor unwrap', async () => {
    const page = { items: [{ order_no: 'w1', delivery_kind: 'auto' }], total: 1, page: 1, page_size: 20, pages: 1 }
    adapter.mockResolvedValue(success(page))
    const { listWorkerDeliveries } = await import('@/api/admin/xianyu')
    await expect(listWorkerDeliveries()).resolves.toEqual(page)
  })

  it('resendDelivery returns the code string (not undefined) after send succeeds', async () => {
    adapter.mockResolvedValue(success({ code: 'CODE-123' }))
    const { resendDelivery } = await import('@/api/admin/xianyu')
    await expect(resendDelivery('o1')).resolves.toBe('CODE-123')
  })

  it('createLoginSession returns session payload with qr code', async () => {
    adapter.mockResolvedValue(success({ status: 'waiting', qr_code: 'data:image/png;base64,xx' }))
    const { createLoginSession } = await import('@/api/admin/xianyu')
    await expect(createLoginSession('a1')).resolves.toEqual({ status: 'waiting', qr_code: 'data:image/png;base64,xx' })
  })

  it('queryLoginSession queries by session_id and returns session status payload', async () => {
    adapter.mockResolvedValue(success({ status: 'success' }))
    const { queryLoginSession } = await import('@/api/admin/xianyu')
    await expect(queryLoginSession('sess-9')).resolves.toEqual({ status: 'success' })
    expect(adapter).toHaveBeenCalledTimes(1)
    expect(adapter.mock.calls[0][0].url).toBe('/admin/xianyu/accounts/login-session/sess-9')
  })

  it('createLoginSession returns session_id so polling uses the Worker session', async () => {
    adapter.mockResolvedValue(success({ status: 'waiting', session_id: 'sess-9', qr_code: 'data:image/png;base64,qq' }))
    const { createLoginSession } = await import('@/api/admin/xianyu')
    const session = await createLoginSession('')
    expect(session.session_id).toBe('sess-9')
    expect(session.qr_code).toContain('data:image/png')
  })

  it('clearCredentials resolves after successful unwrap', async () => {
    adapter.mockResolvedValue(success({ message: 'account credentials cleared' }))
    const { clearCredentials } = await import('@/api/admin/xianyu')
    await expect(clearCredentials('a1')).resolves.toBeUndefined()
  })

  it('getSettings returns control settings payload', async () => {
    const settings = { delivery_enabled: true, account_auto_refresh: true, product_auto_bind: false, sync_interval_minutes: 5 }
    adapter.mockResolvedValue(success(settings))
    const { getSettings } = await import('@/api/admin/xianyu')
    await expect(getSettings()).resolves.toEqual(settings)
  })

  it('listBindingRules returns rules array', async () => {
    adapter.mockResolvedValue(success([{ id: 1, priority: 1, account_pk: 1, match_type: 'keyword', keyword: 'k', pool_id: 2, status: 'active' }]))
    const { listBindingRules } = await import('@/api/admin/xianyu')
    await expect(listBindingRules()).resolves.toHaveLength(1)
  })

  it('listItemPools returns pools array', async () => {
    adapter.mockResolvedValue(success([{ id: 1, name: 'standard', slug: 'standard', description: '', low_stock_threshold: 2, status: 'active' }]))
    const { listItemPools } = await import('@/api/admin/xianyu')
    await expect(listItemPools()).resolves.toHaveLength(1)
  })

  it('rejects when backend returns non-zero code without treating it as success', async () => {
    adapter.mockResolvedValue({ ...adapterResponse({ code: 1001, message: '参数错误', data: null }), status: 200 })
    const { listItemPools } = await import('@/api/admin/xianyu')
    await expect(listItemPools()).rejects.toEqual(expect.objectContaining({ code: 1001 }))
  })

  it('keeps void actions idempotent under interceptor unwrap', async () => {
    adapter.mockResolvedValue(success({ message: 'accounts synced' }))
    const { syncAccounts } = await import('@/api/admin/xianyu')
    await expect(syncAccounts()).resolves.toBeUndefined()
  })
})
