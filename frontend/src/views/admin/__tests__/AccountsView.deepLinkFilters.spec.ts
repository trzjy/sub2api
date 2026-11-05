import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'

// 下钻跳转（来自分组管理）通过 URL query 携带明确的筛选意图，此时必须隔离
// localStorage 中保存的历史 platform/status 偏好，避免跨平台分组账号被静默过滤。
function createTestRouter(query: Record<string, string> = {}): Router {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/', component: { template: '<div />' } }]
  })
  return router
}

const {
  listAccounts,
  listWithEtag,
  getById,
  getBatchTodayStats,
  getUpstreamBillingProbeSettings,
  getAllProxies,
  getAllGroups,
  showError,
  showSuccess,
  showWarning,
  showInfo
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getById: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getUpstreamBillingProbeSettings: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  showError: vi.fn(),
  showSuccess: vi.fn(),
  showWarning: vi.fn(),
  showInfo: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      getById,
      listWithEtag,
      getBatchTodayStats,
      getUpstreamBillingProbeSettings,
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      toggleSchedulable: vi.fn(),
      refreshCredentials: vi.fn()
    },
    proxies: { getAll: getAllProxies },
    groups: { getAll: getAllGroups }
  }
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess, showWarning, showInfo })
}))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'test-token', isSimpleMode: false })
}))
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const DataTableStub = defineComponent({
  props: { data: { type: Array, default: () => [] } },
  template: '<div v-for="row in data" :key="row.id" :data-account-name="row.name" />'
})

function mountView(router: Router) {
  return mount(AccountsView, {
    attachTo: document.body,
    global: {
      plugins: [router],
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
        DataTable: DataTableStub,
        AccountTableActions: { template: '<div><slot name="after" /></div>' },
        AccountTableFilters: true,
        AccountBulkActionsBar: true,
        Pagination: true,
        ConfirmDialog: true,
        AccountActionMenu: true,
        ImportDataModal: true,
        ReAuthAccountModal: true,
        AccountTestModal: true,
        AccountStatsModal: true,
        ScheduledTestsPanel: true,
        SyncFromCrsModal: true,
        TempUnschedStatusModal: true,
        ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true,
        CreateAccountModal: true,
        EditAccountModal: true,
        BulkEditAccountModal: true,
        PlatformTypeBadge: true,
        AccountCapacityCell: true,
        AccountStatusIndicator: true,
        AccountTodayStatsCell: true,
        AccountGroupsCell: true,
        AccountUsageCell: true,
        UpstreamBillingRateCell: true,
        HelpTooltip: true,
        Icon: true,
        Teleport: true
      }
    }
  })
}

import AccountsView from '../AccountsView.vue'

const listRow = {
  id: 42,
  name: 'deep-link row',
  platform: 'gemini',
  type: 'oauth',
  status: 'active',
  schedulable: true,
  concurrency: 2,
  priority: 1,
  group_ids: [134],
  extra: {},
  credentials: {}
}

const fullAccount = {
  ...listRow,
  groups: [{ id: 134, name: 'gemini metering', platform: 'gemini' }],
  account_groups: [{ account_id: 42, group_id: 134 }],
  credentials: { api_key: 'redacted' },
  extra: { detail_only: true }
}

// 读取首次 list 请求携带的筛选 params。
function initialParams() {
  return listAccounts.mock.calls[0][2] as Record<string, unknown>
}

beforeEach(() => {
  localStorage.clear()
  listAccounts.mockReset().mockResolvedValue({ items: [listRow], total: 1, page: 1, page_size: 20, pages: 1 })
  listWithEtag.mockReset().mockResolvedValue({ notModified: true, etag: 'etag', data: null })
  getById.mockReset().mockResolvedValue(fullAccount)
  getBatchTodayStats.mockReset().mockResolvedValue({ stats: {} })
  getUpstreamBillingProbeSettings.mockReset().mockResolvedValue({ enabled: true })
  getAllProxies.mockReset().mockResolvedValue([])
  getAllGroups.mockReset().mockResolvedValue([{ id: 134, name: 'gemini metering', platform: 'gemini' }])
  showError.mockReset()
  showSuccess.mockReset()
  showWarning.mockReset()
  showInfo.mockReset()
})

afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
})

async function flush() {
  await flushPromises()
  await flushPromises()
  await flushPromises()
}

describe('AccountsView deep-link filter isolation (CARD-FE-001)', () => {
  it('scenario 1: restores persisted platform/status when no targeted query present', async () => {
    localStorage.setItem('account-filter-values', JSON.stringify({ platform: 'antigravity', status: 'rate_limited' }))
    const router = createTestRouter()
    await router.push({ path: '/', query: {} })
    await router.isReady()

    const wrapper = mountView(router)
    await flush()

    const params = initialParams()
    expect(params.platform).toBe('antigravity')
    expect(params.status).toBe('rate_limited')
    expect(params.group).toBe('')
    wrapper.unmount()
  })

  it('scenario 2: group drill-down ignores persisted platform, keeps explicit status', async () => {
    localStorage.setItem('account-filter-values', JSON.stringify({ platform: 'antigravity', status: 'rate_limited' }))
    const router = createTestRouter()
    await router.push({ path: '/', query: { group: '134', status: 'available' } })
    await router.isReady()

    const wrapper = mountView(router)
    await flush()

    const params = initialParams()
    expect(params.platform).toBe('')
    expect(params.group).toBe('134')
    expect(params.status).toBe('available')
    wrapper.unmount()
  })

  it('scenario 3: group total drill-down ignores both persisted platform and status', async () => {
    localStorage.setItem('account-filter-values', JSON.stringify({ platform: 'antigravity', status: 'rate_limited' }))
    const router = createTestRouter()
    await router.push({ path: '/', query: { group: '134' } })
    await router.isReady()

    const wrapper = mountView(router)
    await flush()

    const params = initialParams()
    expect(params.platform).toBe('')
    expect(params.status).toBe('')
    expect(params.group).toBe('134')
    wrapper.unmount()
  })

  it('scenario 4: explicit URL platform wins over persisted preference', async () => {
    localStorage.setItem('account-filter-values', JSON.stringify({ platform: 'antigravity', status: 'rate_limited' }))
    const router = createTestRouter()
    await router.push({ path: '/', query: { group: '134', platform: 'cursor' } })
    await router.isReady()

    const wrapper = mountView(router)
    await flush()

    const params = initialParams()
    expect(params.platform).toBe('cursor')
    expect(params.group).toBe('134')
    expect(params.status).toBe('')
    wrapper.unmount()
  })

  it('scenario 4b: explicit URL status wins over persisted preference', async () => {
    localStorage.setItem('account-filter-values', JSON.stringify({ platform: 'antigravity', status: 'rate_limited' }))
    const router = createTestRouter()
    await router.push({ path: '/', query: { group: '134', status: 'temp_limited' } })
    await router.isReady()

    const wrapper = mountView(router)
    await flush()

    const params = initialParams()
    expect(params.status).toBe('temp_limited')
    expect(params.platform).toBe('')
    wrapper.unmount()
  })

  it('scenario 5: reacts to dynamic route query changes and refreshes the list', async () => {
    vi.useFakeTimers()
    const router = createTestRouter()
    await router.push({ path: '/', query: { group: '134' } })
    await router.isReady()

    const wrapper = mountView(router)
    await flush()

    const initialCallCount = listAccounts.mock.calls.length
    const initialParamsSnapshot = initialParams()
    expect(initialParamsSnapshot.group).toBe('134')
    expect(initialParamsSnapshot.platform).toBe('')

    // 切换到另一个分组，路由 query 变化应自动同步 params 并触发列表刷新。
    await router.push({ path: '/', query: { group: '200' } })
    await flush()

    // 推进防抖定时器，触发列表刷新（params 同步在 watch 回调内同步完成）。
    await vi.advanceTimersByTimeAsync(400)
    await flush()

    expect(listAccounts.mock.calls.length).toBeGreaterThan(initialCallCount)
    const latestParams = listAccounts.mock.calls[listAccounts.mock.calls.length - 1][2] as Record<string, unknown>
    expect(latestParams.group).toBe('200')
    expect(latestParams.platform).toBe('')
    wrapper.unmount()
  })

  it('scenario 6: group-context filtering does not overwrite global preference cache', async () => {
    localStorage.setItem('account-filter-values', JSON.stringify({ platform: 'antigravity', status: 'rate_limited' }))
    const router = createTestRouter()
    await router.push({ path: '/', query: { group: '134', status: 'available' } })
    await router.isReady()

    const wrapper = mountView(router)
    await flush()

    // 在分组筛选上下文中触发一次 debouncedReload（会调用 saveAccountFilterValues）。
    vi.useFakeTimers()
    await router.push({ path: '/', query: { group: '134', status: 'temp_limited' } })
    await flush()
    await vi.advanceTimersByTimeAsync(400)
    await flush()
    vi.useRealTimers()
    await flush()

    // 处于分组筛选上下文时，全局偏好缓存必须保持不变。
    const persisted = JSON.parse(localStorage.getItem('account-filter-values') || '{}')
    expect(persisted).toEqual({ platform: 'antigravity', status: 'rate_limited' })
    wrapper.unmount()
  })
})
