import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import CNProviderBalanceCell from '../CNProviderBalanceCell.vue'
import type { Account } from '@/types'

const { queryBalance } = vi.hoisted(() => ({
  queryBalance: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    cnProviders: { queryBalance }
  }
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string, named?: Record<string, unknown>) => {
      if (key === 'admin.accounts.cnProviders.remainingCount' && named) {
        return `剩余 ${named.count}`
      }
      return key
    }
  })
}))

const account = {
  id: 7,
  platform: 'kimi',
  type: 'apikey',
  credentials: { account_mode: 'payg' },
  extra: {
    kimi_balance: 12.5,
    kimi_balance_currency: 'CNY'
  }
} as Account

describe('CNProviderBalanceCell', () => {
  beforeEach(() => {
    queryBalance.mockReset()
  })

  it('renders the persisted balance as static text with an explicit query action', async () => {
    const wrapper = mount(CNProviderBalanceCell, { props: { account } })
    await flushPromises()

    // Snapshot value renders without any probe.
    expect(queryBalance).not.toHaveBeenCalled()
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('CNY 12.50')

    // The control reads as an action; the i18n mock returns the key itself.
    const probeButton = wrapper.get('[data-test="cn-provider-balance-probe"]')
    expect(probeButton.text()).toBe('admin.accounts.cnProviders.probe')

    await probeButton.trigger('click')
    await flushPromises()
    expect(queryBalance).toHaveBeenCalledWith(account.id)
  })

  it('does NOT render the low-balance badge when only the extra marker is set (single source of truth)', () => {
    const extraOnlyAccount = {
      ...account,
      extra: { kimi_balance: 0.4, kimi_balance_low: true }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: extraOnlyAccount } })

    expect(wrapper.text()).not.toContain('admin.accounts.cnProviders.balanceLow')
  })

  it('does NOT render the low-balance badge when the DTO field is absent', () => {
    const wrapper = mount(CNProviderBalanceCell, { props: { account } })

    expect(wrapper.text()).not.toContain('admin.accounts.cnProviders.balanceLow')
  })

  it('keeps the snapshot balance visible when a query fails', async () => {
    queryBalance.mockResolvedValue({ success: false, error: 'HTTP 401' })
    const wrapper = mount(CNProviderBalanceCell, { props: { account } })

    await wrapper.get('[data-test="cn-provider-balance-probe"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('CNY 12.50')
    expect(wrapper.text()).toContain('HTTP 401')
  })

  it('shows remaining count next to the unlimited plan badge from snapshot', () => {
    const unlimitedAccount = {
      ...account,
      extra: {
        kimi_balance_unlimited: true,
        kimi_balance: 1,
        kimi_balance_plan_name: 'Plan A'
      }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: unlimitedAccount } })

    expect(wrapper.text()).toContain('admin.accounts.cnProviders.unlimitedWithPlan')
    const remaining = wrapper.get('[data-test="cn-provider-balance-remaining"]')
    expect(remaining.text()).toContain('剩余 1')
  })

  it('does not render remaining count when unlimited but no numeric snapshot', () => {
    const unlimitedNoBalance = {
      ...account,
      extra: {
        kimi_balance_unlimited: true,
        kimi_balance_plan_name: 'Plan A'
      }
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: unlimitedNoBalance } })

    expect(wrapper.find('[data-test="cn-provider-balance-remaining"]').exists()).toBe(false)
  })

  // ===== 方案 §3.4 / R5-F3：balance_low 出口 + Kira paid 结构化行 =====

  // balance_low 出口：顶层 DTO 字段（逐字锁定）true → 渲染余额不足徽标；
  // 既有余额金额取数路径不变。
  it('shows the low-balance badge from the top-level DTO balance_low field', () => {
    const dtoLowAccount = {
      ...account,
      balance_low: true
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: dtoLowAccount } })

    expect(wrapper.text()).toContain('admin.accounts.cnProviders.balanceLow')
    // 既有余额金额展示不变
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('CNY 12.50')
  })

  // Kira paid 结构化行：source=kira_vnd_balance 维度驱动余额行状态徽标；
  // 既有余额金额（currentEntries）取数路径不变。
  it('adds a status badge to the balance row for a Kira paid dimension', () => {
    const kiraPaidAccount = {
      ...account,
      quota_dimensions: [
        { kind: 'paid', scope: 'account', target: '', status: 'exhausted', servable: 'no', source: 'kira_vnd_balance', observed_at: '2026-10-09T02:00:00Z' }
      ]
    } as Account

    const wrapper = mount(CNProviderBalanceCell, { props: { account: kiraPaidAccount } })

    expect(wrapper.find('[data-test="cn-provider-balance-kira-status"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('admin.accounts.cnProviders.statusExhausted')
    // 既有余额金额展示不变
    expect(wrapper.get('[data-test="cn-provider-balance-value"]').text()).toContain('CNY 12.50')
  })
})
