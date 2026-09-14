import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AccountUsageCell from '../AccountUsageCell.vue'
import type { Account } from '@/types'

// 方案 N6：CodeBuddy 母账号余额/配额走 account.Extra 快照。
// 本 spec 钉住两件事：
//   1) 错误键就是后端写入的 codebuddy_quota_error（此前前端读的是 codebuddy_credit_error，
//      键名不一致导致探测失败态永远不显示）；
//   2) 无快照时是「未探测」空态，绝不渲染 0%。
const { getUsage } = vi.hoisted(() => ({ getUsage: vi.fn() }))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getUsage,
      getUsageBatch: vi.fn().mockResolvedValue({}),
    },
  },
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key }),
  }
})

const makeAccount = (extra: Record<string, unknown> | undefined, platform = 'codebuddy'): Account =>
  ({
    id: 7,
    name: 'jossin',
    platform,
    type: 'oauth',
    proxy_id: null,
    concurrency: 3,
    priority: 0,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: false,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    credentials: {},
    extra,
  }) as Account

const mountCell = (account: Account) =>
  mount(AccountUsageCell, {
    props: { account },
    global: {
      stubs: {
        UsageProgressBar: {
          props: ['label', 'utilization', 'resetsAt'],
          template: '<div data-test="usage-progress-bar" :data-utilization="utilization">{{ label }}|{{ resetsAt }}</div>',
        },
        AccountQuotaInfo: true,
        OpenAIQuotaResetCell: true,
        GrokQuotaProbeCell: true,
        CNProviderQuotaCell: true,
        CNProviderBalanceCell: true,
        OllamaCloudUsageCell: true,
      },
    },
  })

beforeEach(() => {
  getUsage.mockReset()
  getUsage.mockResolvedValue(null)
})

afterEach(() => {
  vi.restoreAllMocks()
})

describe('AccountUsageCell — CodeBuddy 母账号配额快照', () => {
  it('有快照时渲染利用率与周期截止', async () => {
    const wrapper = mountCell(
      makeAccount({
        codebuddy_credit_used_percent: 42,
        codebuddy_credit_total: 1000,
        codebuddy_credit_used: 420,
        codebuddy_credit_reset_at: '2026-09-16T00:00:00Z',
        codebuddy_credit_updated_at: '2026-09-15T00:00:00Z',
      }),
    )
    await flushPromises()

    const bar = wrapper.find('[data-test="usage-progress-bar"]')
    expect(bar.exists()).toBe(true)
    expect(bar.attributes('data-utilization')).toBe('42')
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.codebuddyCredit')
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.codebuddyCreditNotProbed')
  })

  it('无快照时显示「未探测」空态且不渲染 0%', async () => {
    const wrapper = mountCell(makeAccount({}))
    await flushPromises()

    expect(wrapper.find('[data-test="usage-progress-bar"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('admin.accounts.usageWindow.codebuddyCreditNotProbed')
    expect(wrapper.text()).not.toContain('0%')
  })

  it('读取后端实际写入的 codebuddy_quota_error 键作为探测失败态', async () => {
    const wrapper = mountCell(makeAccount({ codebuddy_quota_error: '上游 500' }))
    await flushPromises()

    expect(wrapper.text()).toContain('上游 500')
    // 旧键名不应再驱动 UI
    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.codebuddyCreditNotProbed')
  })

  it('探测失败时保留上一次成功快照（错误行另行追加）', async () => {
    const wrapper = mountCell(
      makeAccount({
        codebuddy_credit_used_percent: 66,
        codebuddy_credit_total: 1000,
        codebuddy_credit_used: 660,
        codebuddy_credit_reset_at: '2026-09-16T00:00:00Z',
        codebuddy_quota_error: '上游超时',
      }),
    )
    await flushPromises()

    expect(wrapper.find('[data-test="usage-progress-bar"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('660 / 1.0K') // used / total 快照摘要
    expect(wrapper.text()).toContain('上游超时')
  })

  it('影子账号（platform 已改为目标平台）不渲染 CodeBuddy 母账号额度块', async () => {
    const wrapper = mountCell(
      makeAccount(
        { codebuddy_credit_used_percent: 42, codebuddy_credit_total: 1000, codebuddy_credit_used: 420 },
        'deepseek',
      ),
    )
    await flushPromises()

    expect(wrapper.text()).not.toContain('admin.accounts.usageWindow.codebuddyCredit')
  })
})
