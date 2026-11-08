import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createMemoryHistory, createRouter, type Router } from 'vue-router'

import AccountsView from '../AccountsView.vue'

// 「所属激活码」下拉 + 深链 ?quota_key_id=N：
//   - 合法正整数：下拉应用该激活码过滤（请求带上 quota_key_id）；
//   - 非法值（0 / 负数 / 非数字 / 空串）：忽略该参数，页面不报错、不过滤。
const {
  listAccounts,
  listWithEtag,
  getBatchTodayStats,
  getUpstreamBillingProbeSettings,
  getAllProxies,
  getAllGroups,
  listQuotaKeys,
  showError
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getUpstreamBillingProbeSettings: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  listQuotaKeys: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      listWithEtag,
      getBatchTodayStats,
      getUpstreamBillingProbeSettings,
      batchDelete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      bulkUpdate: vi.fn()
    },
    proxies: {
      getAll: getAllProxies
    },
    groups: {
      getAll: getAllGroups
    },
    quotaKeys: {
      list: listQuotaKeys
    }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showSuccess: vi.fn(),
    showInfo: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    token: 'test-token',
    isSimpleMode: false
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const DataTableStub = {
  props: { data: { type: Array, default: () => [] } },
  template: '<div v-for="row in data" :key="row.id" :data-account-name="row.name" />'
}

function createTestRouter(query: Record<string, string> = {}): Router {
  const router = createRouter({
    history: createMemoryHistory(),
    routes: [{ path: '/', component: { template: '<div />' } }]
  })
  void query
  return router
}

async function mountView(query: Record<string, string> = {}) {
  const router = createTestRouter(query)
  await router.push({ path: '/', query })
  await router.isReady()

  const wrapper = mount(AccountsView, {
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
  await flushPromises()
  return wrapper
}

// 读取首次 list 请求携带的筛选 params。
function initialParams(): Record<string, unknown> {
  return listAccounts.mock.calls[0][2] as Record<string, unknown>
}

beforeEach(() => {
  localStorage.clear()
  listAccounts.mockReset().mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20, pages: 0 })
  listWithEtag.mockReset().mockResolvedValue({ notModified: true, etag: 'etag', data: null })
  getBatchTodayStats.mockReset().mockResolvedValue({ stats: {} })
  getUpstreamBillingProbeSettings.mockReset().mockResolvedValue({ enabled: true })
  getAllProxies.mockReset().mockResolvedValue([])
  getAllGroups.mockReset().mockResolvedValue([])
  listQuotaKeys.mockReset().mockResolvedValue({
    items: [
      { id: 16, status: 'active', exhausted: false, code_masked: 'XXXX-****-****-5678', priority: 1 },
      { id: 548, status: 'active', exhausted: false, code_masked: 'XXXX-****-****-1234', priority: 1 }
    ],
    total: 2,
    page: 1,
    page_size: 2,
    pages: 1
  })
  showError.mockReset()
})

afterEach(() => {
  vi.restoreAllMocks()
})

describe('AccountsView quota key filter', () => {
  it('applies the quota_key_id deep link to the account list request', async () => {
    const wrapper = await mountView({ quota_key_id: '16' })

    expect(initialParams().quota_key_id).toBe('16')
    expect(showError).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('ignores a non-positive or non-numeric quota_key_id deep link', async () => {
    for (const raw of ['0', '-1', 'abc', '', '1.5']) {
      listAccounts.mockClear()
      const wrapper = await mountView({ quota_key_id: raw })

      expect(initialParams().quota_key_id).toBe('')
      expect(showError).not.toHaveBeenCalled()
      wrapper.unmount()
    }
  })

  it('does not apply a quota key filter when the deep link carries none', async () => {
    const wrapper = await mountView()

    expect(initialParams().quota_key_id).toBe('')
    wrapper.unmount()
  })

  it('loads the quota key option list for the dropdown and survives a failing load', async () => {
    const wrapper = await mountView({ quota_key_id: '16' })

    expect(listQuotaKeys).toHaveBeenCalled()
    expect(initialParams().quota_key_id).toBe('16')
    wrapper.unmount()

    listQuotaKeys.mockReset().mockRejectedValue(new Error('quota key list failed'))
    listAccounts.mockClear()
    const failedWrapper = await mountView({ quota_key_id: '16' })

    expect(initialParams().quota_key_id).toBe('16')
    expect(showError).not.toHaveBeenCalled()
    failedWrapper.unmount()
  })
})
