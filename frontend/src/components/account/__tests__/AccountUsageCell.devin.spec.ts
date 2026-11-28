import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import AccountUsageCell from '../AccountUsageCell.vue'
import type { Account, AccountUsageInfo } from '@/types'

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

// Stub 把每个窗口的已用/剩余/耗尽/重置都暴露成 data-* 属性，便于精确断言。
const usageBarStub = {
  props: ['label', 'utilization', 'resetsAt', 'color', 'remainingPercent', 'exhausted', 'stale', 'title'],
  template: `<div class="usage-bar" :data-label="label" :data-utilization="utilization" :data-remaining="remainingPercent" :data-exhausted="String(exhausted)" :data-stale="String(stale)" :data-reset="resetsAt" :title="title">{{ label }}|{{ utilization }}|{{ remainingPercent }}|{{ exhausted }}</div>`
}

const devinStubs = {
  UsageProgressBar: usageBarStub,
  AccountQuotaInfo: true
}

interface MountDevinOptions {
  account?: Partial<Account>
  batchedUsage?: AccountUsageInfo | null
  batchedUsageError?: string | null
  /** 用真实 UsageProgressBar 渲染，验证已用/剩余列文本 */
  realBars?: boolean
}

function mountDevin(options: MountDevinOptions = {}) {
  const requestBatchedUsage = vi.fn()
  const wrapper = mount(AccountUsageCell, {
    props: {
      account: makeAccount(options.account),
      requestBatchedUsage,
      batchedUsage: options.batchedUsage === undefined ? null : options.batchedUsage,
      batchedUsageError: options.batchedUsageError ?? null
    },
    global: options.realBars ? undefined : { stubs: devinStubs }
  })
  return { wrapper, requestBatchedUsage }
}

function bar(wrapper: ReturnType<typeof mount>, label: string) {
  return wrapper.find(`.usage-bar[data-label="${label}"]`)
}

const quotaMarkerReason = JSON.stringify({
  until_unix: 1,
  matched_keyword: 'devin_upstream_quota_exhausted'
})

describe('AccountUsageCell devin 双窗口用量显示', () => {
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

  it('每日与每周两个独立窗口同时显示，已用/剩余百分比与重置时间正确', async () => {
    const { wrapper, requestBatchedUsage } = mountDevin({
      batchedUsage: {
        devin_usage: {
          plan: 'pro',
          balance_usd: 10,
          // 后端下发的是「剩余百分比」；真实短窗口是 daily，不是 5h。
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

    expect(bar(wrapper, 'Daily').exists()).toBe(true)
    expect(bar(wrapper, 'Weekly').exists()).toBe(true)
    expect(bar(wrapper, 'Daily').attributes('data-utilization')).toBe('0')
    expect(bar(wrapper, 'Daily').attributes('data-remaining')).toBe('100')
    expect(bar(wrapper, 'Daily').attributes('data-reset')).toBe('2026-11-29T00:00:00Z')
    expect(bar(wrapper, 'Weekly').attributes('data-utilization')).toBe('50')
    expect(bar(wrapper, 'Weekly').attributes('data-remaining')).toBe('50')
    expect(bar(wrapper, 'Weekly').attributes('data-reset')).toBe('2026-12-06T00:00:00Z')
    expect(wrapper.find('[data-test="devin-exhausted-reason"]').exists()).toBe(false)
    expect(requestBatchedUsage).toHaveBeenCalled()
  })

  it('真实进度条并列显示已用百分比与剩余百分比（不伪装成 5h/7d）', async () => {
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
      'admin.accounts.usageWindow.devinRemaining::{"percent":"100"}',
      'admin.accounts.usageWindow.devinRemaining::{"percent":"50"}'
    ])
  })

  it('weekly 耗尽而 daily 仍有额度：明确标出 weekly 耗尽与上游额度状态', async () => {
    const { wrapper } = mountDevin({
      account: {
        devin_quota_exhausted: true,
        temp_unschedulable_reason: quotaMarkerReason
      },
      batchedUsage: {
        devin_usage: {
          daily_remaining_percent: 60,
          weekly_remaining_percent: 0
        }
      }
    })
    await flushPromises()

    expect(bar(wrapper, 'Daily').attributes('data-exhausted')).toBe('false')
    expect(bar(wrapper, 'Weekly').attributes('data-exhausted')).toBe('true')
    expect(wrapper.find('[data-test="devin-quota-exhausted"]').exists()).toBe(true)

    const reason = wrapper.get('[data-test="devin-exhausted-reason"]').text()
    expect(reason).toContain('admin.accounts.usageWindow.devinWeekly')
    expect(reason).not.toContain('admin.accounts.usageWindow.devinDaily')

    // active/schedulable=true 不能覆盖真实上游额度状态。
    const split = wrapper.get('[data-test="devin-state-split"]').text()
    expect(split).toContain('"active":"true"')
    expect(split).toContain('"schedulable":"true"')
    expect(split).toContain('admin.accounts.usageWindow.devinUpstreamExhausted')
  })

  it('daily 耗尽而 weekly 仍有额度：只有 daily 标为耗尽，且不与停调 marker 混为一谈', async () => {
    const { wrapper } = mountDevin({
      batchedUsage: {
        devin_usage: {
          daily_remaining_percent: 0,
          weekly_remaining_percent: 80
        }
      }
    })
    await flushPromises()

    expect(bar(wrapper, 'Daily').attributes('data-exhausted')).toBe('true')
    expect(bar(wrapper, 'Weekly').attributes('data-exhausted')).toBe('false')
    const reason = wrapper.get('[data-test="devin-exhausted-reason"]').text()
    expect(reason).toContain('admin.accounts.usageWindow.devinDaily')
    expect(reason).not.toContain('admin.accounts.usageWindow.devinWeekly')
    // 没有 marker 就不显示「因额度耗尽停调」横幅。
    expect(wrapper.find('[data-test="devin-quota-exhausted"]').exists()).toBe(false)
  })

  it('两个窗口同时耗尽：两个窗口都标耗尽，原因同时列出 daily 与 weekly', async () => {
    const { wrapper } = mountDevin({
      batchedUsage: {
        devin_usage: {
          daily_remaining_percent: 0,
          weekly_remaining_percent: 0
        }
      }
    })
    await flushPromises()

    expect(bar(wrapper, 'Daily').attributes('data-exhausted')).toBe('true')
    expect(bar(wrapper, 'Weekly').attributes('data-exhausted')).toBe('true')
    const reason = wrapper.get('[data-test="devin-exhausted-reason"]').text()
    expect(reason).toContain('admin.accounts.usageWindow.devinDaily')
    expect(reason).toContain('admin.accounts.usageWindow.devinWeekly')
  })

  it('stop marker 在但未带 devin_quota_exhausted 字段时仍能识别（旧后端兼容）', async () => {
    const { wrapper } = mountDevin({
      account: { temp_unschedulable_reason: quotaMarkerReason },
      batchedUsage: { devin_usage: { weekly_remaining_percent: 0 } }
    })
    await flushPromises()

    expect(wrapper.find('[data-test="devin-quota-exhausted"]').exists()).toBe(true)
    expect(wrapper.get('[data-test="devin-exhausted-reason"]').text()).toContain(
      'admin.accounts.usageWindow.devinWeekly'
    )
  })

  it('额度耗尽停调但数据不可读：如实标注「窗口未知」，不猜窗口也不画假进度条', async () => {
    const { wrapper } = mountDevin({
      account: { devin_quota_exhausted: true },
      batchedUsage: { devin_usage: { plan: 'pro' } }
    })
    await flushPromises()

    expect(wrapper.find('[data-test="devin-quota-exhausted"]').exists()).toBe(true)
    expect(wrapper.get('[data-test="devin-exhausted-reason"]').text()).toContain(
      'admin.accounts.usageWindow.devinExhaustedWindowUnknown'
    )
    expect(wrapper.findAll('.usage-bar').length).toBe(0)
  })

  it('额度百分比缺失时显示「数据未获取」，不渲染成 0% 或 100%', async () => {
    const { wrapper } = mountDevin({
      batchedUsage: {
        devin_usage: {
          plan: 'pro',
          balance_usd: 10,
          daily_reset_at: '2026-11-29T00:00:00Z',
          weekly_reset_at: '2026-12-06T00:00:00Z'
        }
      }
    })
    await flushPromises()

    expect(wrapper.findAll('.usage-bar').length).toBe(0)
    expect(wrapper.find('[data-test="devin-daily-unknown"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="devin-weekly-unknown"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('pro')
    expect(wrapper.text()).toContain('$10.00')
    expect(wrapper.text()).not.toContain('0%')
    expect(wrapper.text()).not.toContain('100%')
  })

  it('上游查询失败：显示明确查询错误，不渲染假额度，也不误判为额度耗尽', async () => {
    const { wrapper } = mountDevin({
      batchedUsage: {
        source: 'active',
        error: 'devin upstream query failed'
      }
    })
    await flushPromises()

    expect(wrapper.get('[data-test="devin-query-error"]').text()).toContain(
      'devin upstream query failed'
    )
    expect(wrapper.find('[data-test="devin-no-data"]').exists()).toBe(true)
    expect(wrapper.findAll('.usage-bar').length).toBe(0)
    expect(wrapper.find('[data-test="devin-quota-exhausted"]').exists()).toBe(false)
  })

  it('批量请求失败：即使有旧快照也优先展示请求错误，旧进度条置灰而非冒充实时额度', async () => {
    const { wrapper } = mountDevin({
      batchedUsage: {
        devin_usage: {
          plan: 'pro',
          balance_usd: 10,
          daily_remaining_percent: 40,
          weekly_remaining_percent: 70
        }
      },
      batchedUsageError: 'devin refresh failed'
    })
    await flushPromises()

    expect(wrapper.get('[data-test="devin-query-error"]').text()).toContain('devin refresh failed')
    expect(bar(wrapper, 'Daily').attributes('data-stale')).toBe('true')
    expect(bar(wrapper, 'Weekly').attributes('data-stale')).toBe('true')
    // 快照仍可见，但已明确标注查询失败与置灰，不会伪装成正常额度。
    expect(wrapper.text()).toContain('pro')
  })

  it('数据显示获取时间与过期状态：过期快照不冒充实时额度', async () => {
    const fresh = mountDevin({
      batchedUsage: {
        devin_usage: {
          daily_remaining_percent: 50,
          fetched_at: '2026-11-28T11:50:00Z'
        }
      }
    })
    await flushPromises()
    expect(fresh.wrapper.find('[data-test="devin-freshness"]').exists()).toBe(true)
    expect(fresh.wrapper.text()).not.toContain('admin.accounts.usageWindow.devinStale')

    const stale = mountDevin({
      batchedUsage: {
        devin_usage: {
          daily_remaining_percent: 50,
          fetched_at: '2026-11-27T00:00:00Z'
        }
      }
    })
    await flushPromises()
    expect(stale.wrapper.get('[data-test="devin-freshness"]').text()).toContain(
      'admin.accounts.usageWindow.devinStale'
    )
  })

  it('schedulable=false 但额度数据可读时仍展示真实额度，本地状态与上游额度分离', async () => {
    const { wrapper } = mountDevin({
      account: { status: 'inactive', schedulable: false },
      batchedUsage: {
        devin_usage: {
          daily_remaining_percent: 30,
          weekly_remaining_percent: 70
        }
      }
    })
    await flushPromises()

    const split = wrapper.get('[data-test="devin-state-split"]').text()
    expect(split).toContain('"active":"false"')
    expect(split).toContain('"schedulable":"false"')
    expect(split).toContain('admin.accounts.usageWindow.devinUpstreamAvailable')
    expect(bar(wrapper, 'Daily').exists()).toBe(true)
    expect(bar(wrapper, 'Weekly').exists()).toBe(true)
    expect(wrapper.find('[data-test="devin-quota-exhausted"]').exists()).toBe(false)
  })

  it('非额度原因的临时停调仍显示 amber 徽章，且不与额度耗尽横幅混淆', async () => {
    const { wrapper } = mountDevin({
      account: {
        temp_unschedulable_active: true,
        temp_unschedulable_reason: 'ratelimit'
      },
      batchedUsage: {
        devin_usage: { daily_remaining_percent: 40, weekly_remaining_percent: 70 }
      }
    })
    await flushPromises()

    expect(wrapper.find('[data-test="devin-temp-unschedulable"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="devin-quota-exhausted"]').exists()).toBe(false)
  })

  it('devin 非 apikey（oauth）shouldFetchUsage=false，不触发用量拉取', async () => {
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

  it('devin apikey + local_cost（有 usd）→ 渲染本站折算费用（toFixed(4)）与窗口口径 title', async () => {
    const { wrapper } = mountDevin({
      batchedUsage: {
        devin_usage: {
          plan: 'pro',
          balance_usd: 10,
          local_cost: {
            window_start: '2026-11-17T00:00:00Z',
            window_end: '2026-11-17T23:59:59Z',
            usd: 0.0112665,
            priced_models: ['gemini-4-hydrogen-xhigh'],
            unpriced_models: null
          }
        }
      }
    })
    await flushPromises()

    expect(wrapper.text()).toContain('本站折算 ≈ $0.0113')
    const costEl = wrapper.findAll('[title]').find((el) =>
      el.attributes('title')?.includes('官方单价 × 本网关记录 token')
    )
    expect(costEl).toBeDefined()
    expect(costEl!.attributes('title')).toContain(
      '窗口：2026-11-17T00:00:00Z → 2026-11-17T23:59:59Z'
    )
    expect(wrapper.text()).not.toContain('未定价')
  })

  it('devin apikey + local_cost.unpriced_models 非空 → 显示未定价名单，title 说明未定价模型不计入', async () => {
    const { wrapper } = mountDevin({
      batchedUsage: {
        devin_usage: {
          local_cost: {
            window_start: '2026-11-17T00:00:00Z',
            window_end: '2026-11-17T23:59:59Z',
            usd: 0.5,
            priced_models: ['gemini-4-hydrogen-xhigh'],
            unpriced_models: ['unpriced-a', 'unpriced-b']
          }
        }
      }
    })
    await flushPromises()

    expect(wrapper.text()).toContain('本站折算 ≈ $0.5000')
    expect(wrapper.text()).toContain('未定价')
    expect(wrapper.text()).toContain('unpriced-a')
    expect(wrapper.text()).toContain('unpriced-b')
    const costEl = wrapper.findAll('[title]').find((el) =>
      el.attributes('title')?.includes('官方单价 × 本网关记录 token')
    )
    expect(costEl!.attributes('title')).toContain('未定价模型不计入')
  })

  it('devin apikey 无 local_cost → 不渲染本站折算行（旧快照不回归）', async () => {
    const { wrapper } = mountDevin({
      batchedUsage: {
        devin_usage: {
          plan: 'pro',
          balance_usd: 10,
          daily_reset_at: '2026-11-29T00:00:00Z'
        }
      }
    })
    await flushPromises()
    expect(wrapper.text()).toContain('$10.00')
    expect(wrapper.text()).not.toContain('本站折算')
  })
})
