import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import AccountFreshnessModal from '../AccountFreshnessModal.vue'
import type { Account } from '@/types'
import type { AccountFreshnessResponse } from '@/api/admin/accounts'

// 账号状态新鲜度三维展示（派发单 D4b 项 3）：观测时间戳 / 有效阈值 / 告警状态由后端
// `display_state` 唯一决定；waiting_probe 按「等待主动复探」空态呈现，不显示倒计时。

const { getFreshness } = vi.hoisted(() => ({ getFreshness: vi.fn() }))

vi.mock('@/api/admin', () => ({
  adminAPI: { accounts: { getFreshness } },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key, te: () => true }),
  }
})

const BaseDialogStub = {
  props: ['show', 'title', 'width'],
  template: '<div data-testid="base-dialog"><slot /><slot name="footer" /></div>',
}

function makeAccount(): Account {
  return {
    id: 7,
    name: 'tokenharbor-free',
    platform: 'kimi',
    type: 'apikey',
  } as Account
}

function makeResponse(
  observations: AccountFreshnessResponse['observations']
): AccountFreshnessResponse {
  return { account_id: 7, observations }
}

async function mountModal(response: AccountFreshnessResponse) {
  getFreshness.mockResolvedValue(response)
  const wrapper = mount(AccountFreshnessModal, {
    props: { show: false, account: makeAccount() },
    global: { stubs: { BaseDialog: BaseDialogStub } },
  })
  await wrapper.setProps({ show: true })
  await flushPromises()
  return wrapper
}

describe('AccountFreshnessModal', () => {
  beforeEach(() => {
    getFreshness.mockReset()
  })

  it('renders waiting_probe as the "await re-probe" empty state without a countdown', async () => {
    const wrapper = await mountModal(
      makeResponse([
        {
          scope: 'deepseek-v4.1-flash:free',
          dimension: 'account_model',
          observed_at: '',
          attempted_at: '2026-10-03T10:00:00Z',
          effective_threshold_seconds: 1800,
          stale: false,
          active_alert: false,
          display_state: 'waiting_probe',
        },
      ])
    )

    const row = wrapper.get('[data-testid="freshness-row-waiting_probe"]')
    expect(row.text()).toContain('admin.accounts.freshness.waitingProbe')
    expect(row.text()).toContain('admin.accounts.freshness.dimensionAccountModel')
    // 阈值与尝试时间可见
    expect(row.text()).toContain('admin.accounts.freshness.thresholdMinutes')
    // 无观测时间：横线空态，不是失败样式
    expect(row.text()).toContain('-')
    const badge = row.get('span')
    expect(badge.attributes('class')).not.toContain('bg-red-100')
    expect(badge.attributes('class')).not.toContain('bg-emerald-100')
    // 不得出现任何恢复倒计时文案（既有倒计时语义仅限 precise_reset 条目）
    expect(row.text()).not.toContain('admin.accounts.tokenHarbor.rateLimitedPrefix')
    expect(row.text()).not.toContain('admin.accounts.tokenHarbor.rateLimitedUntil')
  })

  it('renders the stale state with its alert flag and threshold', async () => {
    const wrapper = await mountModal(
      makeResponse([
        {
          scope: 'tokenharbor_account_level_probe',
          dimension: 'account_level',
          observed_at: '2026-10-03T00:00:00Z',
          attempted_at: '2026-10-03T09:00:00Z',
          effective_threshold_seconds: 600,
          stale: true,
          active_alert: true,
          display_state: 'stale',
        },
      ])
    )

    const row = wrapper.get('[data-testid="freshness-row-stale"]')
    expect(row.text()).toContain('admin.accounts.freshness.stale')
    expect(row.text()).toContain('admin.accounts.freshness.dimensionAccountLevel')
    expect(row.text()).toContain('admin.accounts.freshness.alertActive')
    // 陈旧是告警态：红色徽章
    expect(row.get('span').attributes('class')).toContain('bg-red-100')
    // 观测时间已渲染（非空态横线）
    expect(row.text()).not.toContain('admin.accounts.freshness.waitingProbe')
  })

  it('renders the empty state when the account has no observations', async () => {
    const wrapper = await mountModal(makeResponse([]))

    expect(wrapper.find('[data-testid="freshness-empty"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="freshness-table"]').exists()).toBe(false)
    expect(getFreshness).toHaveBeenCalledWith(7)
  })
})
