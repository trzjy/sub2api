import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'

import AccountUsageCell from '../AccountUsageCell.vue'
import type { Account } from '@/types'

// 方案 N6：CodeBuddy 母账号余额/配额走 account.Extra 聚合键（CB-SUM-FE-R1：逐包明细已删除）。
// 本 spec 钉住两件事：
//   1) 错误键就是后端写入的 codebuddy_credit_error（§4.1 SSOT；旧错误键名已废弃）；
//   2) 进度条 + 合计行（来自聚合键）+ 无快照空态（绝不渲染 0%）+ has_excluded 标注。
// 逐包行已移除（基线提交 9a254a0d9），本 spec 持续钉住「包名不再渲染」负断言。
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
  // 最小化字典：仅为占位符替换提供映射（真实插值发生在消息值，而非 key）。
  const messages: Record<string, string> = {
    'admin.accounts.usageWindow.codebuddyCredit': '积分',
    'admin.accounts.usageWindow.codebuddyCreditNotProbed': '未探测',
    'admin.accounts.usageWindow.codebuddyPackageTotalRow': '剩余 {remaining} / 总额 {total}',
    'admin.accounts.usageWindow.codebuddyPackageUpdated': '更新于 {date}',
    'admin.accounts.usageWindow.codebuddyPackageExcludedNote': '部分分包未计入合计',
  }
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) => {
        const template = messages[key] ?? key
        if (!params) return template
        let out = template
        for (const [k, v] of Object.entries(params)) out = out.replace(`{${k}}`, String(v))
        return out
      },
    }),
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
  it('有聚合快照时渲染利用率、周期截止与合计行（逐包行已移除）', async () => {
    const wrapper = mountCell(
      makeAccount({
        codebuddy_credit_total_remaining: '300.50000000',
        codebuddy_credit_total_total: '1000.00000000',
        codebuddy_credit_packages_updated_at: '2026-09-29T10:00:00Z',
        codebuddy_credit_used_percent: 30.05,
        codebuddy_credit_reset_at: '2026-10-01T00:00:00Z',
      }),
    )
    await flushPromises()

    const bar = wrapper.find('[data-test="usage-progress-bar"]')
    expect(bar.exists()).toBe(true)
    expect(bar.attributes('data-utilization')).toBe('30.05')
    expect(wrapper.text()).toContain('积分')
    // 逐包行已移除：包名不再渲染（负断言，聚合键下根本没有包名概念）
    expect(wrapper.text()).not.toContain('Pro 月包')
    // 合计行（更新时间格式随运行环境 Intl locale，宽松断言日期年份）
    expect(wrapper.text()).toContain('剩余 300.50 / 总额 1000.00')
    expect(wrapper.text()).toContain('更新于')
    expect(wrapper.text()).toContain('2026')
    expect(wrapper.text()).not.toContain('未探测')
  })

  it('页面示例形态 600/2500/76：明确渲染「剩余 / 总额」且百分比一致', async () => {
    const wrapper = mountCell(
      makeAccount({
        codebuddy_credit_total_remaining: '600.00000000',
        codebuddy_credit_total_total: '2500.00000000',
        codebuddy_credit_packages_updated_at: '2026-10-19T02:33:35.593+08:00',
        codebuddy_credit_used_percent: 76,
      }),
    )
    await flushPromises()

    const bar = wrapper.find('[data-test="usage-progress-bar"]')
    expect(bar.exists()).toBe(true)
    // used_percent=76 由后端推导，进度条直接消费；(2500-600)/2500=76%
    expect(bar.attributes('data-utilization')).toBe('76')
    // 「剩余 / 总额」明确文案，不再有「合计」歧义
    expect(wrapper.text()).toContain('剩余 600.00 / 总额 2500.00')
    expect(wrapper.text()).not.toContain('合计 600.00 / 2500.00')
  })

  it('无快照时显示「未探测」空态且不渲染 0%', async () => {
    const wrapper = mountCell(makeAccount({}))
    await flushPromises()

    expect(wrapper.find('[data-test="usage-progress-bar"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('未探测')
    expect(wrapper.text()).not.toContain('0%')
  })

  it('读取后端实际写入的 codebuddy_credit_error 键作为探测失败态', async () => {
    const wrapper = mountCell(makeAccount({ codebuddy_credit_error: '上游 500' }))
    await flushPromises()

    expect(wrapper.text()).toContain('上游 500')
    // 有错误、无快照 → 渲染错误行，不显示「未探测」空态（错误行独立）
    expect(wrapper.text()).not.toContain('未探测')
  })

  it('探测失败时保留上一次成功快照（错误行另行追加）', async () => {
    const wrapper = mountCell(
      makeAccount({
        codebuddy_credit_total_remaining: '660.00000000',
        codebuddy_credit_total_total: '1000.00000000',
        codebuddy_credit_used_percent: 66,
        codebuddy_credit_error: '上游超时',
      }),
    )
    await flushPromises()

    expect(wrapper.find('[data-test="usage-progress-bar"]').exists()).toBe(true)
    // 合计行：660.00 / 1000.00
    expect(wrapper.text()).toContain('剩余 660.00 / 总额 1000.00')
    expect(wrapper.text()).toContain('上游超时')
  })

  it('has_excluded=true 时标注「部分分包未计入合计」', async () => {
    const wrapper = mountCell(
      makeAccount({
        codebuddy_credit_total_remaining: '10.00000000',
        codebuddy_credit_total_total: '20.00000000',
        codebuddy_credit_has_excluded: true,
        codebuddy_credit_used_percent: 40,
      }),
    )
    await flushPromises()

    // 合计只含聚合 total：10.00 / 20.00
    expect(wrapper.text()).toContain('剩余 10.00 / 总额 20.00')
    // 逐包行已移除：包名不再渲染
    expect(wrapper.text()).not.toContain('积分包')
    expect(wrapper.text()).not.toContain('美元包')
    expect(wrapper.text()).toContain('部分分包未计入合计')
  })

  it('影子账号（platform 已改为目标平台）不渲染 CodeBuddy 母账号额度块', async () => {
    const wrapper = mountCell(
      makeAccount(
        {
          codebuddy_credit_total_remaining: '300.50000000',
          codebuddy_credit_total_total: '1000.00000000',
          codebuddy_credit_used_percent: 30.05,
        },
        'deepseek',
      ),
    )
    await flushPromises()

    expect(wrapper.text()).not.toContain('积分')
  })
})
