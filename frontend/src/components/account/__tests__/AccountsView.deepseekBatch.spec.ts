import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AccountsView from '../../../views/admin/AccountsView.vue'
import type { Account } from '@/types'

// ④ AccountsView 批量门控：opt-in deepseek 命中、未 opt-in 不命中。
// 通过挂载真实 AccountsView（AccountUsageCell 不 stub），观察批量用量抓取
// adminAPI.accounts.getBatchUsage 收到的账号 ID，从而验证 accountSupportsBatchUsage
// 对 deepseek opt-in 的判定（与 muse 一致纳入批量抓取）。
const {
  getBatchUsage,
  listAccounts,
  getBatchTodayStats,
  getUpstreamBillingProbeSettings
} = vi.hoisted(() => ({
  getBatchUsage: vi.fn(),
  listAccounts: vi.fn(),
  getBatchTodayStats: vi.fn(),
  getUpstreamBillingProbeSettings: vi.fn()
}))

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
        if (prop === 'getBatchUsage') return getBatchUsage
        if (prop === 'list') return listAccounts
        if (prop === 'getBatchTodayStats') return getBatchTodayStats
        if (prop === 'getUpstreamBillingProbeSettings') return getUpstreamBillingProbeSettings
        return anyFn()
      }
    }
  )
  // proxies / groups 的 getAll 需要返回数组（被弹窗组件作为 Array 类型 prop 接收）
  const listProxy = new Proxy(
    {},
    {
      get: (_t, prop) =>
        prop === 'getAll' ? () => Promise.resolve([]) : anyFn()
    }
  )
  return {
    adminAPI: {
      accounts,
      proxies: listProxy,
      groups: listProxy
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

// 渲染 usage 列单元格（真实 AccountUsageCell）+ 占位列，避免 stub 掉被测门控的触发源
const DataTableStub = {
  props: ['columns', 'data'],
  template: `
    <div data-test="data-table">
      <div v-for="row in data" :key="row.id" data-test="account-row">
        <slot name="cell-usage" :row="row" />
        <slot name="cell-rate_multiplier" :row="row" />
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
        // 仅 stub 真实 AccountUsageCell 的重子组件，避免其渲染依赖被触发
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

function makeDeepseekAccount(id: number, usageProbe?: string): Account {
  return {
    id,
    name: `deepseek-${id}`,
    platform: 'deepseek',
    type: 'apikey',
    proxy_id: null,
    concurrency: 1,
    priority: 1,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: true,
    created_at: '2026-03-15T00:00:00Z',
    updated_at: '2026-03-15T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...(usageProbe ? { credentials: { usage_probe: usageProbe } } : {})
  }
}

const OPT_IN_ID = 8101
const NON_OPT_IN_ID = 8201

describe('admin AccountsView deepseek opt-in 批量门控', () => {
  beforeEach(() => {
    localStorage.clear()
    getBatchUsage.mockReset()
    listAccounts.mockReset()
    getBatchTodayStats.mockReset()
    getUpstreamBillingProbeSettings.mockReset()

    listAccounts.mockResolvedValue({
      items: [
        makeDeepseekAccount(OPT_IN_ID, 'opencode_zen'),
        makeDeepseekAccount(NON_OPT_IN_ID)
      ],
      total: 2,
      page: 1,
      page_size: 20,
      pages: 1
    })
    getBatchTodayStats.mockResolvedValue({ stats: {} })
    getUpstreamBillingProbeSettings.mockResolvedValue({ enabled: true, interval_minutes: 30 })
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

  it('opt-in deepseek 命中批量抓取、未 opt-in 不命中', async () => {
    const wrapper = mountView()
    await flushPromises()
    // 触发批量抓取 flush（queueBatchedUsage 用 setTimeout(0) 排程），用真实计时器等待排程执行
    await new Promise((resolve) => setTimeout(resolve, 50))
    await flushPromises()

    expect(getBatchUsage).toHaveBeenCalled()
    const allIds = (getBatchUsage as unknown as { mock: { calls: unknown[][] } }).mock.calls.flatMap(
      (call) => call[0] as number[]
    )
    expect(allIds).toContain(OPT_IN_ID)
    expect(allIds).not.toContain(NON_OPT_IN_ID)
  })
})
