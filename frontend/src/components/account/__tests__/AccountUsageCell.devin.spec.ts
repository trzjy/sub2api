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

  it('devin apikey + devin_usage percent → 真实进度条按 0–100 百分数渲染（remaining=100→0%、50→50%）与 plan/余额，不再重复重置文本', async () => {
    const requestBatchedUsage = vi.fn()
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 992, platform: 'devin', type: 'apikey' }),
        requestBatchedUsage,
        batchedUsage: {
          devin_usage: {
            plan: 'pro',
            balance_usd: 10,
            // utilization 为 0–100 百分数：remaining=100→0、remaining=50→50
            daily_remaining_percent: 100,
            weekly_remaining_percent: 50,
            daily_reset_at: '2026-11-18T00:00:00Z',
            weekly_reset_at: '2026-11-24T00:00:00Z'
          }
        }
      }
    })

    await flushPromises()

    const text = wrapper.text()
    // showUsageWindows 放行 devin → 根节点渲染 → devin 专用分支呈现 plan/余额
    expect(text).toContain('pro')
    expect(text).toContain('$10.00')
    // 真实 UsageProgressBar（不 stub）：label 与百分比列（span.text-right）按 Daily/Weekly 顺序渲染
    expect(text).toContain('Daily')
    expect(text).toContain('Weekly')
    const percents = wrapper.findAll('span.text-right').map((s) => s.text())
    expect(percents).toEqual(['0%', '50%'])
    // percent 存在时不再重复显示对应重置时间文本行
    expect(text).not.toContain('Daily reset:')
    expect(text).not.toContain('Weekly reset:')
    // shouldFetchUsage 放行 devin apikey（批量模式触发父级拉取）
    expect(requestBatchedUsage).toHaveBeenCalled()
  })

  it('devin 剩余 0% → 真实进度条显示 100%（0–100 百分数契约）', async () => {
    const requestBatchedUsage = vi.fn()
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 999, platform: 'devin', type: 'apikey' }),
        requestBatchedUsage,
        batchedUsage: {
          devin_usage: {
            plan: 'pro',
            balance_usd: 10,
            daily_remaining_percent: 0,
            weekly_remaining_percent: 0
          }
        }
      }
    })

    await flushPromises()

    // remaining=0 → utilization=100；resetsAt 缺省，不渲染重置时间，百分比列仍来自真实组件
    const percents = wrapper.findAll('span.text-right').map((s) => s.text())
    expect(percents).toEqual(['100%', '100%'])
  })

  it('devin apikey + devin_usage percent 缺失 → 回退渲染 Daily/Weekly 重置文本，不渲染窗口进度条', async () => {
    const requestBatchedUsage = vi.fn()
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 991, platform: 'devin', type: 'apikey' }),
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
    expect(text).toContain('Daily reset:')
    expect(text).toContain('Weekly reset:')
    expect(wrapper.findAll('.usage-bar').length).toBe(0)
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

  it('devin apikey + batchedUsage 仅 error（无 devin_usage）→ 渲染错误文本、不渲染 plan', async () => {
    const requestBatchedUsage = vi.fn()
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 996, platform: 'devin', type: 'apikey' }),
        requestBatchedUsage,
        batchedUsage: {
          source: 'active',
          error: 'devin upstream query failed'
        }
      },
      global: { stubs: devinStubs }
    })

    await flushPromises()

    const text = wrapper.text()
    // error 分支：amber 文本展示后端 error（含 title），且不渲染 devin plan / 重置
    expect(text).toContain('devin upstream query failed')
    expect(text).not.toContain('pro')
    expect(text).not.toContain('Daily reset:')
    expect(text).not.toContain('Weekly reset:')
  })

  it('devin apikey + batchedUsageError（batchedUsage 为 null）→ 渲染组件级请求错误文本，不渲染 plan/重置', async () => {
    const requestBatchedUsage = vi.fn()
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 997, platform: 'devin', type: 'apikey' }),
        requestBatchedUsage,
        batchedUsage: null,
        batchedUsageError: 'devin batch usage request failed'
      },
      global: { stubs: devinStubs }
    })

    await flushPromises()

    const text = wrapper.text()
    // 组件级请求错误（batchedUsageError → error）优先渲染，不渲染 devin plan / 重置
    expect(text).toContain('devin batch usage request failed')
    expect(text).not.toContain('pro')
    expect(text).not.toContain('Daily reset:')
    expect(text).not.toContain('Weekly reset:')
  })

  it('devin apikey + 已有成功快照 + batchedUsageError → 优先显示请求错误文本而非旧余额', async () => {
    const requestBatchedUsage = vi.fn()
    const wrapper = mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 998, platform: 'devin', type: 'apikey' }),
        requestBatchedUsage,
        batchedUsage: {
          devin_usage: {
            plan: 'pro',
            balance_usd: 10,
            daily_reset_at: '2026-11-18T00:00:00Z',
            weekly_reset_at: '2026-11-24T00:00:00Z'
          }
        },
        batchedUsageError: 'devin refresh failed; stale balance hidden'
      },
      global: { stubs: devinStubs }
    })

    await flushPromises()

    const text = wrapper.text()
    // 即使存在旧的成功快照，组件级请求错误也要优先显示，不再展示旧 plan/余额/重置
    expect(text).toContain('devin refresh failed; stale balance hidden')
    expect(text).not.toContain('pro')
    expect(text).not.toContain('$10.00')
    expect(text).not.toContain('Daily reset:')
    expect(text).not.toContain('Weekly reset:')
  })
})
