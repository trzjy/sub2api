import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountUsageCell from '../AccountUsageCell.vue'
import type { Account } from '@/types'

// CARD-WCQ-FE04：WebConsoleQuotaCell 挂载点回归。
// WebConsoleQuotaCell 挂载即按快照陈旧度自动探测，需把网络层整体挡住。
const { probeWebConsoleQuota } = vi.hoisted(() => ({
  probeWebConsoleQuota: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: { getUsage: vi.fn().mockResolvedValue(null) },
    cnProviders: {
      queryQuota: vi.fn().mockResolvedValue({ success: true, tiers: [] }),
      queryBalance: vi.fn().mockResolvedValue({ success: true })
    }
  }
}))

vi.mock('@/api/admin/cnProviders', () => ({
  probeWebConsoleQuota,
  startWebConsoleAuth: vi.fn().mockResolvedValue({ status: 'pending' }),
  pollWebConsoleAuth: vi.fn().mockResolvedValue({ status: 'pending' })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

function makeAccount(overrides: Partial<Account>): Account {
  return {
    id: 1,
    name: 'account',
    platform: 'zhipu',
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
    ...overrides
  }
}

// 只对 CN 子单元格打桩，WebConsoleQuotaCell 用真实组件（断言其真实 data-test 根节点）。
const cnOnlyStubs = {
  CNProviderQuotaCell: { template: '<div data-test="cn-quota-cell" />' },
  CNProviderBalanceCell: { template: '<div data-test="cn-balance-cell" />' },
  UsageProgressBar: true,
  AccountQuotaInfo: true
}

describe('AccountUsageCell — Web 控制台额度上游', () => {
  beforeEach(() => {
    probeWebConsoleQuota.mockReset()
    probeWebConsoleQuota.mockResolvedValue({
      success: true,
      snapshot: { provider: 'compshare', plan_name: 'test-plan', fetched_at: new Date().toISOString() }
    })
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

  it.each(['compshare', 'sensenova', 'longcat'] as const)(
    'provider=%s 时渲染 WebConsoleQuotaCell，且不落入 CN 子单元格',
    async (provider) => {
      const wrapper = mount(AccountUsageCell, {
        props: {
          account: makeAccount({
            id: 9200,
            platform: 'zhipu',
            credentials: { account_mode: 'coding' },
            web_console_quota_provider: provider
          })
        },
        global: { stubs: cnOnlyStubs }
      })

      await flushPromises()

      expect(wrapper.find('[data-test="web-console-quota"]').exists()).toBe(true)
      expect(wrapper.find('[data-test="cn-quota-cell"]').exists()).toBe(false)
      expect(wrapper.find('[data-test="cn-balance-cell"]').exists()).toBe(false)
    }
  )

  it('分支优先于 CN 判定：kimi 平台 + provider 仍走 WebConsoleQuotaCell', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 9201,
          platform: 'kimi',
          credentials: { account_mode: 'coding' },
          web_console_quota_provider: 'compshare'
        })
      },
      global: { stubs: cnOnlyStubs }
    })

    await flushPromises()

    expect(wrapper.find('[data-test="web-console-quota"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="cn-quota-cell"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="cn-balance-cell"]').exists()).toBe(false)
  })

  it('provider 为空时行为不变：仍渲染既有 CN 子单元格', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 9202,
          platform: 'zhipu',
          credentials: { account_mode: 'coding' },
          web_console_quota_provider: undefined
        })
      },
      global: { stubs: cnOnlyStubs }
    })

    await flushPromises()

    expect(wrapper.find('[data-test="web-console-quota"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="cn-quota-cell"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="cn-balance-cell"]').exists()).toBe(true)
  })

  it('WebConsoleQuotaCell 触发 updated 时向上转发 account-updated', async () => {
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({
          id: 9203,
          platform: 'deepseek',
          credentials: { account_mode: 'coding' },
          web_console_quota_provider: 'sensenova'
        })
      },
      global: {
        stubs: {
          ...cnOnlyStubs,
          WebConsoleQuotaCell: {
            props: ['account'],
            emits: ['updated'],
            template: '<button data-test="wcq-trigger" @click="$emit(\'updated\')">x</button>'
          }
        }
      }
    })

    await wrapper.find('[data-test="wcq-trigger"]').trigger('click')

    const emitted = wrapper.emitted<Account[]>('account-updated')
    expect(emitted).toBeTruthy()
    expect(emitted?.[0]?.[0]?.id).toBe(9203)
  })
})
