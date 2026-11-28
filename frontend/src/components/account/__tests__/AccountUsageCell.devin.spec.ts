import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountUsageCell from '../AccountUsageCell.vue'
import type { Account, AccountUsageInfo, WindowStats } from '@/types'

// Devin 用量由父级批量注入（batchedUsage.devin_usage）；getUsage 仅用于单账号兜底与主动刷新。
const { getUsage } = vi.hoisted(() => ({
  getUsage: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getUsage,
      getById: vi.fn()
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

function makeAccount(overrides: Partial<Account> = {}): Account {
  return {
    id: 992,
    name: 'devin-992',
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
    ...overrides
  }
}

const todayStats: WindowStats = {
  requests: 12,
  tokens: 3400,
  cost: 0.05,
  standard_cost: 0.05,
  user_cost: 0.02
}

// Stub 暴露共享进度条的全部语义：标签/已用/剩余/耗尽/过期/重置/统计。
const usageBarStub = {
  props: ['label', 'utilization', 'resetsAt', 'color', 'remainingPercent', 'exhausted', 'stale', 'title', 'windowStats'],
  template: `<div class="usage-bar" :data-label="label" :data-utilization="utilization" :data-remaining="remainingPercent" :data-exhausted="String(exhausted)" :data-stale="String(stale)" :data-reset="resetsAt" :data-stats="windowStats ? windowStats.requests : ''" :title="title">{{ label }}|{{ utilization }}|{{ remainingPercent }}|{{ exhausted }}</div>`
}
const devinStubs = { UsageProgressBar: usageBarStub, AccountQuotaInfo: true }

interface MountDevinOptions {
  account?: Partial<Account>
  batchedUsage?: AccountUsageInfo | null
  batchedUsageError?: string | null
  todayStats?: WindowStats | null
  manualRefreshToken?: number
  realBars?: boolean
}

function mountDevin(options: MountDevinOptions = {}) {
  const requestBatchedUsage = vi.fn()
  const wrapper = mount(AccountUsageCell, {
    props: {
      account: makeAccount(options.account),
      requestBatchedUsage,
      batchedUsage: options.batchedUsage === undefined ? null : options.batchedUsage,
      batchedUsageError: options.batchedUsageError ?? null,
      todayStats: options.todayStats === undefined ? null : options.todayStats,
      manualRefreshToken: options.manualRefreshToken ?? 0
    },
    global: options.realBars ? undefined : { stubs: devinStubs }
  })
  return { wrapper, requestBatchedUsage }
}

function bar(wrapper: ReturnType<typeof mount>, labelKey: string) {
  return wrapper.find(`.usage-bar[data-label="${labelKey}"]`)
}

const dailyLabel = 'admin.accounts.usageWindow.windows.daily'
const weeklyLabel = 'admin.accounts.usageWindow.windows.weekly'
const quotaMarkerReason = JSON.stringify({
  until_unix: 1,
  matched_keyword: 'devin_upstream_quota_exhausted'
})

describe('AccountUsageCell Devin 订阅式用量展示', () => {
  beforeEach(() => {
    getUsage.mockReset()
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-11-28T12:00:00Z'))
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

  afterEach(() => {
    vi.useRealTimers()
  })

  it('Daily 与 Weekly 通过通用窗口映射展示，两个窗口同时完整显示', async () => {
    const { wrapper, requestBatchedUsage } = mountDevin({
      todayStats,
      batchedUsage: {
        devin_usage: {
          plan: 'pro',
          balance_usd: 10,
          daily_remaining_percent: 100,
          weekly_remaining_percent: 50,
          daily_reset_at: '2026-11-29T00:00:00Z',
          weekly_reset_at: '2026-12-06T00:00:00Z'
        }
      }
    })
    await flushPromises()

    const text = wrapper.text()
    expect(text).toContain('pro')
    expect(text).toContain('$10.00')
    // 标签来自 i18n 映射键（值中文由 locale spec 断言），不出现英文 Daily/Weekly 硬编码。
    expect(text).not.toContain('|Daily|')
    expect(text).not.toContain('|Weekly|')

    const daily = bar(wrapper, dailyLabel)
    const weekly = bar(wrapper, weeklyLabel)
    expect(daily.exists()).toBe(true)
    expect(weekly.exists()).toBe(true)
    expect(daily.attributes('data-utilization')).toBe('0')
    expect(daily.attributes('data-remaining')).toBe('100')
    expect(daily.attributes('data-reset')).toBe('2026-11-29T00:00:00Z')
    expect(daily.attributes('data-stats')).toBe('12')
    expect(weekly.attributes('data-utilization')).toBe('50')
    expect(weekly.attributes('data-remaining')).toBe('50')
    expect(weekly.attributes('data-reset')).toBe('2026-12-06T00:00:00Z')
    expect(wrapper.find('[data-test="devin-quota-exhausted"]').exists()).toBe(false)
    expect(requestBatchedUsage).toHaveBeenCalled()
  })

  it('真实进度条同时输出已用百分比与剩余百分比', async () => {
    const { wrapper } = mountDevin({
      realBars: true,
      batchedUsage: {
        devin_usage: {
          daily_remaining_percent: 100,
          weekly_remaining_percent: 50,
          daily_reset_at: '2026-11-29T00:00:00Z'
        }
      }
    })
    await flushPromises()

    expect(wrapper.findAll('span.text-right').map((s) => s.text())).toEqual(['0%', '50%'])
    expect(
      wrapper.findAll('[data-test="usage-progress-remaining"]').map((s) => s.text())
    ).toEqual([
      'admin.accounts.usageWindow.remaining::{"percent":"100"}',
      'admin.accounts.usageWindow.remaining::{"percent":"50"}'
    ])
  })

  it('marker 与最新窗口快照冲突时：快照说了算，不显示额度耗尽也不显示窗口未知', async () => {
    const { wrapper } = mountDevin({
      account: {
        devin_quota_exhausted: true,
        temp_unschedulable_reason: quotaMarkerReason
      },
      batchedUsage: {
        devin_usage: {
          daily_remaining_percent: 100,
          weekly_remaining_percent: 50
        }
      }
    })
    await flushPromises()

    expect(wrapper.find('[data-test="devin-quota-exhausted"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.windowDataUnavailable')
    expect(bar(wrapper, dailyLabel).exists()).toBe(true)
    expect(bar(wrapper, weeklyLabel).exists()).toBe(true)
  })

  it('weekly 耗尽时明确指向每周窗口，不与 Daily 剩余 100% 矛盾', async () => {
    const { wrapper } = mountDevin({
      account: { devin_quota_exhausted: true },
      batchedUsage: {
        devin_usage: {
          daily_remaining_percent: 100,
          weekly_remaining_percent: 0
        }
      }
    })
    await flushPromises()

    expect(bar(wrapper, dailyLabel).attributes('data-exhausted')).toBe('false')
    expect(bar(wrapper, weeklyLabel).attributes('data-exhausted')).toBe('true')
    const banner = wrapper.get('[data-test="devin-quota-exhausted"]').text()
    expect(banner).toContain('admin.accounts.usageWindow.exhaustedWindows')
    expect(banner).toContain(weeklyLabel)
    expect(banner).not.toContain(dailyLabel)
  })

  it('两个窗口都耗尽时同时指向每日与每周', async () => {
    const { wrapper } = mountDevin({
      batchedUsage: {
        devin_usage: { daily_remaining_percent: 0, weekly_remaining_percent: 0 }
      }
    })
    await flushPromises()

    const banner = wrapper.get('[data-test="devin-quota-exhausted"]').text()
    expect(banner).toContain(dailyLabel)
    expect(banner).toContain(weeklyLabel)
  })

  it('只有真实缺少窗口数据时才显示未知，并标出具体是哪个窗口', async () => {
    const { wrapper } = mountDevin({
      account: { devin_quota_exhausted: true },
      batchedUsage: { devin_usage: { plan: 'pro' } }
    })
    await flushPromises()

    expect(wrapper.find('[data-test="devin-quota-exhausted"]').exists()).toBe(true)
    // 没有快照时 marker 是唯一证据，如实注明窗口数据未获取。
    expect(wrapper.get('[data-test="devin-quota-exhausted"]').text()).toContain(
      'admin.accounts.usageWindow.windowDataUnavailable'
    )
    expect(wrapper.get('[data-test="devin-daily-unavailable"]').text()).toContain(dailyLabel)
    expect(wrapper.get('[data-test="devin-weekly-unavailable"]').text()).toContain(weeklyLabel)
    expect(wrapper.findAll('.usage-bar').length).toBe(0)
  })

  it('只缺 Weekly 时 Daily 照常显示，Weekly 单独标注数据未获取', async () => {
    const { wrapper } = mountDevin({
      batchedUsage: { devin_usage: { daily_remaining_percent: 40 } }
    })
    await flushPromises()

    expect(bar(wrapper, dailyLabel).exists()).toBe(true)
    expect(wrapper.get('[data-test="devin-weekly-unavailable"]').text()).toContain(
      'admin.accounts.usageWindow.windowDataUnavailable'
    )
    expect(wrapper.text()).not.toContain('窗口未知')
  })

  it('过期数据时文字与进度条状态一致：都标为 stale', async () => {
    const { wrapper } = mountDevin({
      batchedUsage: {
        devin_usage: {
          daily_remaining_percent: 50,
          weekly_remaining_percent: 70,
          fetched_at: '2026-11-27T00:00:00Z'
        }
      }
    })
    await flushPromises()

    expect(wrapper.get('[data-test="devin-freshness"]').text()).toContain(
      'admin.accounts.usageWindow.stale'
    )
    expect(bar(wrapper, dailyLabel).attributes('data-stale')).toBe('true')
    expect(bar(wrapper, weeklyLabel).attributes('data-stale')).toBe('true')
  })

  it('新鲜数据不标过期', async () => {
    const { wrapper } = mountDevin({
      batchedUsage: {
        devin_usage: {
          daily_remaining_percent: 50,
          fetched_at: '2026-11-28T11:50:00Z'
        }
      }
    })
    await flushPromises()

    expect(wrapper.get('[data-test="devin-freshness"]').exists()).toBe(true)
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.stale')
    expect(bar(wrapper, dailyLabel).attributes('data-stale')).toBe('false')
  })

  it('查询失败：单独显示错误，窗口条置灰，不误判为额度耗尽', async () => {
    const { wrapper } = mountDevin({
      batchedUsage: {
        devin_usage: { daily_remaining_percent: 60, weekly_remaining_percent: 70 },
        error: 'devin upstream query failed'
      }
    })
    await flushPromises()

    expect(wrapper.get('[data-test="devin-query-error"]').text()).toContain(
      'devin upstream query failed'
    )
    expect(bar(wrapper, dailyLabel).attributes('data-stale')).toBe('true')
    expect(bar(wrapper, weeklyLabel).attributes('data-stale')).toBe('true')
    expect(wrapper.find('[data-test="devin-quota-exhausted"]').exists()).toBe(false)
  })

  it('手动刷新令牌变化时重新请求批量用量', async () => {
    const { wrapper, requestBatchedUsage } = mountDevin({
      manualRefreshToken: 1,
      batchedUsage: { devin_usage: { daily_remaining_percent: 60 } }
    })
    await flushPromises()
    requestBatchedUsage.mockClear()

    await wrapper.setProps({ manualRefreshToken: 2 })
    await flushPromises()

    expect(requestBatchedUsage).toHaveBeenCalledWith(expect.anything(), { force: true })
  })

  it('查询按钮触发主动刷新并把结果同步回父级', async () => {
    const freshUsage: AccountUsageInfo = {
      source: 'active',
      devin_usage: { daily_remaining_percent: 20, weekly_remaining_percent: 30 }
    }
    getUsage.mockResolvedValue(freshUsage)
    const { wrapper } = mountDevin({
      batchedUsage: { devin_usage: { daily_remaining_percent: 100 } }
    })
    await flushPromises()

    const buttons = wrapper.findAll('button')
    const queryButton = buttons.find((b) => b.text().includes('admin.accounts.usageWindow.activeQuery'))
    expect(queryButton).toBeDefined()
    await queryButton!.trigger('click')
    await flushPromises()

    expect(getUsage).toHaveBeenCalledWith(992, 'active', true)
    expect(wrapper.emitted('usage-loaded')?.some((args) => args[0] === freshUsage)).toBe(true)
    expect(bar(wrapper, dailyLabel).attributes('data-utilization')).toBe('80')
  })

  it('devin 非 apikey（oauth）不触发用量拉取', async () => {
    const requestBatchedUsage = vi.fn()
    mount(AccountUsageCell, {
      props: {
        account: makeAccount({ id: 993, type: 'oauth' }),
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
      props: { account: makeAccount({ id: 994, type: 'apikey' }) },
      global: { stubs: devinStubs }
    })
    await flushPromises()
    expect(getUsage).toHaveBeenCalled()

    getUsage.mockReset()
    mount(AccountUsageCell, {
      props: { account: makeAccount({ id: 995, type: 'oauth' }) },
      global: { stubs: devinStubs }
    })
    await flushPromises()
    expect(getUsage).not.toHaveBeenCalled()
  })

  it('本站折算费用仍复用既有展示', async () => {
    const { wrapper } = mountDevin({
      batchedUsage: {
        devin_usage: {
          plan: 'pro',
          local_cost: {
            window_start: '2026-11-17T00:00:00Z',
            window_end: '2026-11-17T23:59:59Z',
            usd: 0.0112665,
            priced_models: ['gemini-4-hydrogen-xhigh'],
            unpriced_models: ['unpriced-a']
          }
        }
      }
    })
    await flushPromises()

    expect(wrapper.text()).toContain('本站折算 ≈ $0.0113')
    expect(wrapper.text()).toContain('未定价')
    expect(wrapper.text()).toContain('unpriced-a')
  })
})
