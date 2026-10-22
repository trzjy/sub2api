import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AccountsView from '../../../views/admin/AccountsView.vue'
import type { Account } from '@/types'

// 方案 §25.6 消费闭包⑥⑦ / §25.7-B：账号页两层额度展示的接线锁。
//
// 挂真实 AccountsView（不 stub QuotaKeyUsageCell），只把 DataTable 换成只渲染 usage 列
// 的桩，从而验证：
//   ① 账号行带 quota_key_usage 时渲染「所属激活码额度」（供应商总额/已用/算术剩余）；
//   ② 账号行带 vendor_account_quota.provided=false 时只渲染 note，不出现任何数字/百分比；
//   ③ 无这两维字段的（非 Cursor）账号完全不渲染该块（不凭空造块、不补 0）。
const { listAccounts, getBatchTodayStats, getUpstreamBillingProbeSettings, getBatchUsage } = vi.hoisted(
  () => ({
    listAccounts: vi.fn(),
    getBatchTodayStats: vi.fn(),
    getUpstreamBillingProbeSettings: vi.fn(),
    getBatchUsage: vi.fn()
  })
)

vi.mock('@/api/admin', () => {
  const anyFn = () =>
    new Proxy(function () {}, {
      get: () => anyFn(),
      apply: () => Promise.resolve({})
    })
  const accounts = new Proxy(
    {},
    {
      get: (_t, prop) => {
        if (prop === 'list') return listAccounts
        if (prop === 'getBatchTodayStats') return getBatchTodayStats
        if (prop === 'getUpstreamBillingProbeSettings') return getUpstreamBillingProbeSettings
        if (prop === 'getBatchUsage') return getBatchUsage
        return anyFn()
      }
    }
  )
  const listProxy = new Proxy(
    {},
    {
      get: (_t, prop) => (prop === 'getAll' ? () => Promise.resolve([]) : anyFn())
    }
  )
  return {
    adminAPI: {
      accounts,
      proxies: listProxy,
      groups: listProxy,
      quotaKeys: listProxy
    }
  }
})

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showInfo: vi.fn()
  })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({
    token: 'test-token'
  })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key
    })
  }
})

// 只渲染 usage 列，其余列不需要（避免无关组件的渲染依赖）。
const DataTableStub = {
  props: ['columns', 'data'],
  template: `
    <div data-test="data-table">
      <div v-for="row in data" :key="row.id" data-test="account-row" :data-account-id="row.id">
        <slot name="cell-usage" :row="row" />
      </div>
    </div>
  `
}

function mountView() {
  return mount(AccountsView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: {
          template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>'
        },
        DataTable: DataTableStub,
        HelpTooltip: { template: '<span data-test="help-tooltip" />' },
        Pagination: true,
        ConfirmDialog: true,
        AccountTableActions: { template: '<div><slot name="beforeCreate" /><slot name="after" /></div>' },
        AccountTableFilters: { template: '<div data-test="account-filters" />' },
        AccountBulkActionsBar: true,
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
        // 只 stub AccountUsageCell 的重子组件，避免其渲染依赖被触发
        UsageProgressBar: {
          props: ['label', 'utilization', 'resetsAt', 'color'],
          template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ resetsAt }}|{{ color }}</div>'
        },
        AccountQuotaInfo: true,
        CNProviderQuotaCell: { template: '<div data-test="cn-quota" />' },
        CNProviderBalanceCell: { template: '<div data-test="cn-balance" />' },
        OllamaCloudUsageCell: true,
        OpenAIQuotaResetCell: true,
        GrokQuotaProbeCell: true,
        Icon: true
      }
    }
  })
}

function makeCursorAccount(id: number, overrides: Partial<Account> = {}): Account {
  return {
    id,
    name: `cursor-${id}`,
    platform: 'cursor',
    type: 'apikey',
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: true,
    created_at: '2026-10-20T00:00:00Z',
    updated_at: '2026-10-20T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...overrides
  }
}

const CURSOR_WITH_USAGE_ID = 9101
const CURSOR_STALE_ID = 9102
const PLAIN_ID = 9103

function listResponse(items: Account[]) {
  return { items, total: items.length, page: 1, page_size: 20, pages: 1 }
}

describe('admin AccountsView 账号页两层额度展示（§25.6⑥⑦ / §25.7-B）', () => {
  beforeEach(() => {
    localStorage.clear()
    listAccounts.mockReset()
    getBatchTodayStats.mockReset()
    getUpstreamBillingProbeSettings.mockReset()
    getBatchUsage.mockReset()

    listAccounts.mockResolvedValue(
      listResponse([
        makeCursorAccount(CURSOR_WITH_USAGE_ID, {
          quota_key_id: 7,
          quota_key_label: '****abcd',
          quota_key_usage: {
            scope: 'quota_key',
            label: '所属激活码额度',
            total: 400,
            used: 86,
            remaining: 314,
            status_code: 200,
            checked_at: '2026-10-22T02:00:00Z',
            attempted_at: '2026-10-22T02:00:00Z',
            has_snapshot: true,
            query_failed: false
          },
          vendor_account_quota: { provided: false, note: '供应商账号额度未提供' }
        }),
        // 归属激活码但后端取不到该 key 的 summary（降级）：只有「未提供」注明，无任何数值。
        makeCursorAccount(CURSOR_STALE_ID, {
          quota_key_id: 8,
          quota_key_label: '****efgh',
          vendor_account_quota: { provided: false, note: '供应商账号额度未提供' }
        }),
        // 非 Cursor / 无归属激活码：两维都省略，不渲染任何额度块。
        makeCursorAccount(PLAIN_ID)
      ])
    )
    getBatchTodayStats.mockResolvedValue({ stats: {} })
    getUpstreamBillingProbeSettings.mockResolvedValue({ enabled: false, interval_minutes: 30 })
    getBatchUsage.mockResolvedValue({ usage: {}, errors: {} })

    Object.defineProperty(window, 'matchMedia', {
      writable: true,
      value: vi.fn().mockImplementation(() => ({
        matches: true,
        media: '(min-width: 768px)',
        onchange: null,
        addListener: vi.fn(),
        removeListener: vi.fn(),
        addEventListener: vi.fn(),
        removeEventListener: vi.fn(),
        dispatchEvent: vi.fn()
      }))
    })
  })

  it('有快照的账号行渲染「所属激活码额度」总额/已用/算术剩余 + 供应商额度未提供注明', async () => {
    const wrapper = mountView()
    await flushPromises()

    const row = wrapper.get(`[data-account-id="${CURSOR_WITH_USAGE_ID}"]`)
    const cell = row.get('[data-test="quota-key-usage-cell"]')

    expect(cell.get('[data-test="quota-key-usage-label"]').text()).toBe('所属激活码额度')
    expect(cell.get('[data-test="quota-key-usage-total"]').text()).toBe('400')
    expect(cell.get('[data-test="quota-key-usage-used"]').text()).toBe('86')
    expect(cell.get('[data-test="quota-key-usage-remaining"]').text()).toBe('314')
    // §25.7-B：账号页同时注明供应商账号额度未提供，且整行不出现供应商余额百分比。
    expect(cell.get('[data-test="vendor-account-quota-note"]').text()).toBe('供应商账号额度未提供')
    expect(cell.text()).not.toContain('%')

    wrapper.unmount()
  })

  it('只有 vendor_account_quota（无快照）的账号行只渲染 note，不渲染数字/百分比', async () => {
    const wrapper = mountView()
    await flushPromises()

    const row = wrapper.get(`[data-account-id="${CURSOR_STALE_ID}"]`)
    const cell = row.get('[data-test="quota-key-usage-cell"]')

    expect(row.find('[data-test="quota-key-usage"]').exists()).toBe(false)
    expect(cell.text()).toBe('供应商账号额度未提供')
    expect(cell.text()).not.toMatch(/\d/)
    expect(cell.text()).not.toContain('%')

    wrapper.unmount()
  })

  it('无两维字段的账号行完全不渲染额度块（不补 0、不造块）', async () => {
    const wrapper = mountView()
    await flushPromises()

    const row = wrapper.get(`[data-account-id="${PLAIN_ID}"]`)
    expect(row.find('[data-test="quota-key-usage-cell"]').exists()).toBe(false)
    expect(row.find('[data-test="vendor-account-quota-note"]').exists()).toBe(false)

    wrapper.unmount()
  })
})
