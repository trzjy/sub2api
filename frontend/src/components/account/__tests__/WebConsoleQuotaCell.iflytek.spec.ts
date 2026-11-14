import { describe, expect, it, vi, beforeEach } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import WebConsoleQuotaCell from '../WebConsoleQuotaCell.vue'
import type { Account, WebConsoleQuotaSnapshot } from '@/types'

// IF-FE-2：WebConsoleQuotaCell 的 iflytek 分支（余额 + Token Plan + coding plan tiers）。
// 不触网：probeWebConsoleQuota 整体打桩；快照直接经 account.extra 注入，避免 onMounted 自动探测。
const { probeWebConsoleQuota } = vi.hoisted(() => ({
  probeWebConsoleQuota: vi.fn()
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
    ...overrides
  }
}

function makeIflytekAccount(snapshot: Partial<WebConsoleQuotaSnapshot> | null, provider = 'iflytek'): Account {
  const base = makeAccount({
    id: 982,
    platform: 'deepseek',
    web_console_quota_provider: provider
  })
  const webConsoleSnapshot = snapshot
    ? ({ provider, auth_state: 'ok', fetched_at: new Date().toISOString(), ...snapshot } as WebConsoleQuotaSnapshot)
    : undefined
  return {
    ...base,
    extra: {
      web_console_quota_snapshot: webConsoleSnapshot
    } as Account['extra']
  }
}

describe('WebConsoleQuotaCell — iflytek 余额与套餐', () => {
  beforeEach(() => {
    probeWebConsoleQuota.mockReset()
    probeWebConsoleQuota.mockResolvedValue({ success: true, snapshot: null })
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

  it('balance: 0 + 空 token_plan → 显示 wcq-iflytek-balance（含 0.00），且不渲染 exhausted 警告', async () => {
    const wrapper = mount(WebConsoleQuotaCell, {
      props: { account: makeIflytekAccount({ balance: 0, token_plan: undefined, coding_plan_tiers: [] }) },
      global: { stubs: { UsageProgressBar: true } }
    })
    await flushPromises()

    const balance = wrapper.find('[data-test="wcq-iflytek-balance"]')
    expect(balance.exists()).toBe(true)
    expect(balance.text()).toContain('0.00')

    // 余额为 0 对免费号是正常态：不得出现「已耗尽」红字。
    expect(wrapper.find('[data-test="wcq-exhausted"]').exists()).toBe(false)
    // 无套餐 → token-plan 节点不渲染。
    expect(wrapper.find('[data-test="wcq-iflytek-token-plan"]').exists()).toBe(false)
  })

  it('balance: 12.5 → 文本含 12.50', async () => {
    const wrapper = mount(WebConsoleQuotaCell, {
      props: { account: makeIflytekAccount({ balance: 12.5 }) },
      global: { stubs: { UsageProgressBar: true } }
    })
    await flushPromises()

    const balance = wrapper.find('[data-test="wcq-iflytek-balance"]')
    expect(balance.exists()).toBe(true)
    expect(balance.text()).toContain('12.50')
  })

  it('token_plan 有席位 → 显示 wcq-iflytek-token-plan 且文本含 "1 / 3"', async () => {
    const wrapper = mount(WebConsoleQuotaCell, {
      props: {
        account: makeIflytekAccount({
          balance: 0,
          token_plan: { total_count: 3, assigned_count: 2, remaining_count: 1 }
        })
      },
      global: { stubs: { UsageProgressBar: true } }
    })
    await flushPromises()

    const tokenPlan = wrapper.find('[data-test="wcq-iflytek-token-plan"]')
    expect(tokenPlan.exists()).toBe(true)
    expect(tokenPlan.text()).toContain('1 / 3')
  })

  it('coding_plan_tiers 三条 → 渲染 3 个 wcq-tier', async () => {
    const wrapper = mount(WebConsoleQuotaCell, {
      props: {
        account: makeIflytekAccount({
          balance: 0,
          coding_plan_tiers: [
            { window: '5h', limit: 100, used: 10 },
            { window: 'week', limit: 200, used: 20 },
            { window: 'month', limit: 300, used: 30 }
          ]
        })
      },
      global: { stubs: { UsageProgressBar: true } }
    })
    await flushPromises()

    expect(wrapper.findAll('[data-test="wcq-tier"]')).toHaveLength(3)
  })

  it('provider 非 iflytek 时不渲染 iflytek 节点（回归保护）', async () => {
    const wrapper = mount(WebConsoleQuotaCell, {
      props: {
        account: makeIflytekAccount(
          { balance: 12.5, token_plan: { total_count: 3, assigned_count: 2, remaining_count: 1 } },
          'longcat'
        )
      },
      global: { stubs: { UsageProgressBar: true } }
    })
    await flushPromises()

    expect(wrapper.find('[data-test="wcq-iflytek-balance"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="wcq-iflytek-token-plan"]').exists()).toBe(false)
  })

  it('balance 非 number（缺失）时不渲染余额节点', async () => {
    const wrapper = mount(WebConsoleQuotaCell, {
      props: { account: makeIflytekAccount({ token_plan: undefined }) },
      global: { stubs: { UsageProgressBar: true } }
    })
    await flushPromises()

    expect(wrapper.find('[data-test="wcq-iflytek-balance"]').exists()).toBe(false)
  })
})
