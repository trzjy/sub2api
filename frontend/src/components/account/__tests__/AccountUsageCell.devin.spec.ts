import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountUsageCell from '../AccountUsageCell.vue'
import type { Account } from '@/types'

// devin 分支纯前端消费：usage 数据由父级批量用量注入（batchedUsage.devin_usage）。
// getUsage 仅用于断言单账号兜底（shouldFetchUsage）是否被触发。
const { getUsage } = vi.hoisted(() => ({
  getUsage: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getUsage
    }
  }
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params && Object.keys(params).length ? `${key}::${JSON.stringify(params)}` : key
    })
  }
})

function makeAccount(overrides: Partial<Account>): Account {
  return {
    id: 1,
    name: 'account',
    platform: 'devin',
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
    ...overrides,
  }
}

const usageBarStub = {
  props: ['label', 'utilization', 'resetsAt', 'color'],
  template: '<div class="usage-bar">{{ label }}|{{ utilization }}|{{ resetsAt }}|{{ color }}</div>'
}

const devinStubs = {
  UsageProgressBar: usageBarStub,
  AccountQuotaInfo: true
}

describe('AccountUsageCell devin 用量放行', () => {
  beforeEach(() => {
    getUsage.mockReset()
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
        dispatchEvent: vi.fn(),
      }))
    })
  })

  it('devin apikey + batchedUsage.devin_usage 渲染 plan/余额/日周重置，且不渲染通用 5h/7d 进度条', async () => {
    const requestBatchedUsage = vi.fn()
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 992, platform: 'devin', type: 'apikey' }),
        requestBatchedUsage,
        batchedUsage: {
          devin_usage: {
            plan: 'pro',
            balance_usd: 10,
            daily_reset_at: '2026-11-18T00:00:00Z',
            weekly_reset_at: '2026-11-24T00:00:00Z'
          }
        }
      },
      global: { stubs: devinStubs }
    })

    await flushPromises()

    const text = wrapper.text()
    // showUsageWindows 放行 devin → 根节点渲染 → devin 专用分支呈现 plan/余额/重置
    expect(text).toContain('pro')
    expect(text).toContain('$10.00')
    expect(text).toContain('Daily reset:')
    expect(text).toContain('Weekly reset:')
    // devin 专用分支不走通用 5h/7d 进度条
    expect(wrapper.findAll('.usage-bar').length).toBe(0)
    // shouldFetchUsage 放行 devin apikey（批量模式触发父级拉取）
    expect(requestBatchedUsage).toHaveBeenCalled()
  })

  it('devin 非 apikey（oauth）shouldFetchUsage=false，不触发用量拉取', async () => {
    const requestBatchedUsage = vi.fn()
    mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 993, platform: 'devin', type: 'oauth' }),
        requestBatchedUsage,
        batchedUsage: {}
      },
      global: { stubs: devinStubs }
    })

    await flushPromises()

    expect(requestBatchedUsage).not.toHaveBeenCalled()
  })

  it('单账号兜底：devin apikey 触发 getUsage，devin oauth 不触发', async () => {
    mount(AccountUsageCell, {
      props: { account: makeAccount({ id: 994, platform: 'devin', type: 'apikey' }) },
      global: { stubs: devinStubs }
    })
    await flushPromises()
    expect(getUsage).toHaveBeenCalled()

    getUsage.mockReset()
    mount(AccountUsageCell, {
      props: { account: makeAccount({ id: 995, platform: 'devin', type: 'oauth' }) },
      global: { stubs: devinStubs }
    })
    await flushPromises()
    expect(getUsage).not.toHaveBeenCalled()
  })
})
