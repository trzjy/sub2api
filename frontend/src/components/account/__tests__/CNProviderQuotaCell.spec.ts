import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import CNProviderQuotaCell from '../CNProviderQuotaCell.vue'
import type { Account } from '@/types'

const { queryQuota } = vi.hoisted(() => ({
  queryQuota: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    cnProviders: { queryQuota }
  }
}))

vi.mock('vue-i18n', () => ({
  useI18n: () => ({
    t: (key: string) => key
  })
}))

const account = {
  id: 7,
  platform: 'zhipu',
  type: 'apikey',
  credentials: { account_mode: 'coding' },
  extra: {
    zhipu_5h_used_percent: 0,
    zhipu_weekly_used_percent: 27,
    zhipu_5h_reset_at: '2026-08-18T12:30:00+08:00',
    zhipu_weekly_reset_at: '2026-08-22T00:00:00+08:00',
    zhipu_usage_updated_at: new Date().toISOString()
  }
} as Account

// 火山方舟订阅号：platform=deepseek，靠 base_url=ark.cn-beijing.volces.com 识别，
// 快照键前缀为 volcano_（而非 deepseek_）。刷新快照直接渲染，无需探测。
const volcanoAccount = {
  id: 29,
  platform: 'deepseek',
  type: 'apikey',
  credentials: {
    account_mode: 'coding',
    base_url: 'https://ark.cn-beijing.volces.com/api/plan'
  },
  extra: {
    volcano_5h_used_percent: 32.210872,
    volcano_weekly_used_percent: 9.203106285714286,
    volcano_5h_reset_at: '2026-09-04T01:10:03Z',
    volcano_weekly_reset_at: '2026-09-06T16:00:00Z',
    volcano_usage_updated_at: new Date().toISOString()
  }
} as Account

describe('CNProviderQuotaCell', () => {
  beforeEach(() => {
    queryQuota.mockReset()
  })

  it('keeps the compact quota stack readable inside the account table cell', async () => {
    queryQuota.mockResolvedValue({
      success: true,
      tiers: [
        { window: '5h', used_percent: 0, reset_at: '2026-08-18T12:30:00+08:00' },
        { window: 'weekly', used_percent: 27, reset_at: '2026-08-22T00:00:00+08:00' }
      ]
    })
    const wrapper = mount(CNProviderQuotaCell, { props: { account } })

    const root = wrapper.get('[data-test="cn-provider-quota"]')
    expect(root.classes()).toContain('min-w-[220px]')

    const probeButton = root.get('button')
    expect(probeButton.classes()).toContain('whitespace-nowrap')
    expect(probeButton.classes()).toContain('leading-4')
    await probeButton.trigger('click')
    await flushPromises()

    const tiers = root.findAll('[data-test="cn-provider-quota-tier"]')
    expect(tiers).toHaveLength(2)
    for (const tier of tiers) {
      expect(tier.classes()).toContain('min-w-0')
      expect(tier.classes()).toContain('leading-4')
    }

    const labels = root.findAll('[data-test="cn-provider-quota-label"]')
    expect(labels).toHaveLength(2)
    for (const label of labels) {
      expect(label.classes()).toContain('w-14')
      expect(label.classes()).toContain('whitespace-nowrap')
    }

    expect(queryQuota).toHaveBeenCalledWith(account.id)
  })

  it('labels the refresh control with an explicit action verb, not a data caption', async () => {
    const wrapper = mount(CNProviderQuotaCell, { props: { account } })
    await flushPromises()

    // The snapshot is fresh (usage_updated_at = now): bars render without probing.
    expect(queryQuota).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('27%')

    // The control reads as an action ("query"), unlike the old noun label
    // ("5-hour window/weekly window") which looked like a passive caption.
    // The i18n mock returns the key itself.
    const probeButton = wrapper.get('[data-test="cn-provider-quota-probe"]')
    expect(probeButton.text()).toBe('admin.accounts.cnProviders.probe')

    await probeButton.trigger('click')
    await flushPromises()
    expect(queryQuota).toHaveBeenCalledWith(account.id)
  })

  it('renders a volcano-coding account from its volcano_* snapshot without probing', async () => {
    const wrapper = mount(CNProviderQuotaCell, { props: { account: volcanoAccount } })
    await flushPromises()

    // Fresh snapshot: bars render straight from volcano_* extra keys, no probe call.
    expect(queryQuota).not.toHaveBeenCalled()
    const tiers = wrapper.findAll('[data-test="cn-provider-quota-tier"]')
    expect(tiers).toHaveLength(2)
    expect(wrapper.text()).toContain('32%')
    expect(wrapper.text()).toContain('9%')
  })

  // 非火山 deepseek coding（base_url 非 volces）不渲染该单元格：需后端识别才会探测。
  it('stays hidden for a deepseek coding account without a volces base_url', async () => {
    const plainDeepseek = {
      id: 30,
      platform: 'deepseek',
      type: 'apikey',
      credentials: { account_mode: 'coding', base_url: 'https://api.deepseek.com' },
      extra: { deepseek_5h_used_percent: 10 }
    } as Account
    const wrapper = mount(CNProviderQuotaCell, { props: { account: plainDeepseek } })
    await flushPromises()
    expect(wrapper.find('[data-test="cn-provider-quota"]').exists()).toBe(false)
    expect(queryQuota).not.toHaveBeenCalled()
  })

  // 火山订阅号账号在 deepseek 创建/编辑界面保存为 payg：仍应按 base_url 挂载配额单元格。
  it('shows quota cell for a payg volcano deepseek via credentials.base_url', async () => {
    const paygVolcano = {
      id: 29,
      platform: 'deepseek',
      type: 'apikey',
      credentials: { account_mode: 'payg', base_url: 'https://ark.cn-beijing.volces.com/api/coding/v3' },
      extra: {
        volcano_5h_used_percent: 12.5,
        volcano_weekly_used_percent: 0,
        volcano_5h_reset_at: '2026-09-04T01:10:03Z',
        volcano_usage_updated_at: new Date().toISOString()
      }
    } as Account
    const wrapper = mount(CNProviderQuotaCell, { props: { account: paygVolcano } })
    await flushPromises()
    const root = wrapper.find('[data-test="cn-provider-quota"]')
    expect(root.exists()).toBe(true)
    expect(wrapper.text()).toContain('13%')
  })

  // 火山地址仅存在于 api_base_urls.chat_completions（adaptive 协议）时也应挂载，
  // 与后端 GetOpenAIBaseURL 的优先级一致。
  it('shows quota cell when only api_base_urls.chat_completions is the volcano address', async () => {
    const adaptiveVolcano = {
      id: 29,
      platform: 'deepseek',
      type: 'apikey',
      credentials: {
        account_mode: 'payg',
        api_protocol: 'adaptive',
        api_base_urls: { chat_completions: 'https://ark.cn-beijing.volces.com/api/coding/v3' }
      },
      extra: {
        volcano_5h_used_percent: 5,
        volcano_weekly_used_percent: 0,
        volcano_5h_reset_at: '2026-09-04T01:10:03Z',
        volcano_usage_updated_at: new Date().toISOString()
      }
    } as Account
    const wrapper = mount(CNProviderQuotaCell, { props: { account: adaptiveVolcano } })
    await flushPromises()
    expect(wrapper.find('[data-test="cn-provider-quota"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('5%')
  })

  // 有 reset_at 时显示真实重置倒计时（非推算）。
  it('shows the real 5h reset countdown when reset_at is present', async () => {
    const withReset = {
      id: 29,
      platform: 'deepseek',
      type: 'apikey',
      credentials: { account_mode: 'payg', base_url: 'https://ark.cn-beijing.volces.com/api/plan' },
      extra: {
        volcano_5h_used_percent: 40,
        volcano_weekly_used_percent: 0,
        volcano_5h_reset_at: new Date(Date.now() + 2 * 60 * 60 * 1000).toISOString(),
        volcano_usage_updated_at: new Date().toISOString()
      }
    } as Account
    const wrapper = mount(CNProviderQuotaCell, { props: { account: withReset } })
    await flushPromises()
    expect(wrapper.text()).toContain('2h')
  })

  // 没有 reset_at 时仍显示用量条，但不伪造/推算倒计时（不渲染 `·` 重置片段）。
  it('shows usage but does not fabricate a countdown when reset_at is absent', async () => {
    const noReset = {
      id: 29,
      platform: 'deepseek',
      type: 'apikey',
      credentials: { account_mode: 'payg', base_url: 'https://ark.cn-beijing.volces.com/api/coding/v3' },
      extra: {
        volcano_5h_used_percent: 60,
        volcano_weekly_used_percent: 0,
        volcano_usage_updated_at: new Date().toISOString()
      }
    } as Account
    const wrapper = mount(CNProviderQuotaCell, { props: { account: noReset } })
    await flushPromises()
    expect(wrapper.text()).toContain('60%')
    // 无 reset_at：不渲染 `· 重置` 片段（formatReset 不会被虚构时间驱动）。
    expect(wrapper.text()).not.toMatch(/·/)
  })
})
