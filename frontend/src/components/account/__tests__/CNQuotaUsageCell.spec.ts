import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import CNQuotaUsageCell from '../CNQuotaUsageCell.vue'
import UsageProgressBar from '../UsageProgressBar.vue'
import type { Account } from '@/types'

const { queryQuota } = vi.hoisted(() => ({
  queryQuota: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    cnProviders: { queryQuota }
  }
}))

// 保留 vue-i18n 真实导出：UsageProgressBar / @/utils/format 依赖 @/i18n 的模块级
// createI18n。t 带 named params 时把参数序列化进返回值，便于断言聚合数值。
vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key} ${JSON.stringify(params)}` : key
    })
  }
})

// TH 订阅账号（账号 207 同型）：platform 挂 kimi，base_url 指向 tokenharbor.ai，
// extra 含 th_pass_snapshot（has_pass=true，含 renews_at 续期 + reset_at 周期重置 +
// plan_used_pct / plan_exhausted 官方津贴）+ th_usage_snapshot（today/7d 两键，无 30d）。
const thPassAccount = {
  id: 207,
  platform: 'kimi',
  type: 'apikey',
  credentials: { base_url: 'https://tokenharbor.ai/v1' },
  extra: {
    th_pass_snapshot: {
      has_pass: true,
      pass_name: 'Agent Pass',
      renews_at: new Date(Date.now() + 29 * 24 * 60 * 60 * 1000).toISOString(),
      reset_at: new Date(Date.now() + 4 * 24 * 60 * 60 * 1000).toISOString(),
      plan_used_pct: 62,
      plan_exhausted: false,
      spend_after_allowance: false,
      fetched_at: new Date().toISOString()
    },
    th_usage_snapshot: {
      windows: {
        today: { requests: 10, tokens_in: 100, tokens_out: 200 },
        '7d': { requests: 120, tokens_in: 1000, tokens_out: 2000 }
      },
      fetched_at: new Date().toISOString()
    }
  }
} as unknown as Account

// TH 免费账号（无订阅）：只渲染 7 天滚动已用计数（窗口仅 today/7d）。
const thFreeAccount = {
  id: 187,
  platform: 'kimi',
  type: 'apikey',
  credentials: { base_url: 'https://tokenharbor.ai/v1' },
  extra: {
    th_usage_snapshot: {
      windows: {
        today: { requests: 1, tokens_in: 1, tokens_out: 1 },
        '7d': { requests: 14, tokens_in: 700, tokens_out: 700 }
      },
      fetched_at: new Date().toISOString()
    }
  }
} as unknown as Account

// Kira 免费账号：base_url 指向 kiraai.vn（platform 不反映真实上游）。
const kiraResetAtIso = new Date(Date.now() + 2 * 60 * 60 * 1000).toISOString()
const kiraAccount = {
  id: 208,
  platform: 'kimi',
  type: 'apikey',
  credentials: { base_url: 'https://kiraai.vn/api/v1' },
  extra: {
    kira_usage_snapshot: {
      window: 'daily',
      used_percent: 30,
      used_tokens: 1000,
      limit_tokens: 6000000,
      reset_at: kiraResetAtIso,
      fetched_at: new Date().toISOString()
    },
    kimi_balance: 38406
  }
} as unknown as Account

describe('CNQuotaUsageCell', () => {
  beforeEach(() => {
    queryQuota.mockReset()
  })

  // L5 互斥：TH 有 Pass → 只渲染订阅用量窗口（Pass 档位/到期倒计时/today 切换/状态），
  // 免费窗口与 Kira 窗口不得同时出现。
  it('renders only the subscription window for a TH account with has_pass=true', () => {
    const wrapper = mount(CNQuotaUsageCell, { props: { account: thPassAccount } })

    expect(wrapper.find('[data-test="cn-quota-pass-window"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="cn-quota-free-window"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="cn-quota-kira-window"]').exists()).toBe(false)

    // Pass 档位名直接来自快照
    expect(wrapper.get('[data-test="cn-quota-pass-name"]').text()).toBe('Agent Pass')
    // 徽标数据源已从 renews_at 切到 reset_at（7 天周期重置）
    expect(wrapper.get('[data-test="cn-quota-pass-renews"]').text()).toContain(
      'admin.accounts.cnProviders.passResetsAt'
    )
    // 状态默认正常
    expect(wrapper.get('[data-test="cn-quota-status"]').text()).toBe(
      'admin.accounts.cnProviders.statusNormal'
    )
    // 默认今日窗口：requests=10
    const stats = wrapper.get('[data-test="cn-quota-window-stats"]').text()
    expect(stats).toContain('"requests":"10"')
    // 官方 Pass 津贴进度条（plan_used_pct=62）渲染
    const planBar = wrapper.find('[data-test="cn-quota-pass-plan-bar"]')
    expect(planBar.exists()).toBe(true)
    expect(planBar.findComponent(UsageProgressBar).props('utilization')).toBe(62)
  })

  // today/7d/renew 切换：today/7d 渲染官方 CSV 聚合，renew 档显示续期倒计时。
  it('switches today/7d/renew window: aggregates + renewal countdown', async () => {
    const wrapper = mount(CNQuotaUsageCell, { props: { account: thPassAccount } })

    const toggles = wrapper.findAll('[data-test="cn-quota-window-toggle"]')
    expect(toggles).toHaveLength(3)
    expect(toggles.map((b) => b.text())).toEqual([
      'admin.accounts.cnProviders.windowToday',
      'admin.accounts.cnProviders.window7d',
      'admin.accounts.cnProviders.windowRenew'
    ])

    // 7 天档：聚合计数 + reset_at 周期重置倒计时（passResetsAt 文案）
    await toggles[1].trigger('click')
    const stats7d = wrapper.get('[data-test="cn-quota-window-stats"]').text()
    expect(stats7d).toContain('"requests":"120"')
    expect(stats7d).toContain('admin.accounts.cnProviders.passResetsAt')

    // renew 档：订阅续期倒计时（passRenewsAt，同源同值）
    await toggles[2].trigger('click')
    expect(wrapper.get('[data-test="cn-quota-window-stats"]').text()).toContain(
      'admin.accounts.cnProviders.passRenewsAt'
    )

    await toggles[0].trigger('click')
    expect(wrapper.get('[data-test="cn-quota-window-stats"]').text()).toContain('"requests":"10"')
  })

  // 官方 plan_exhausted 触发"已耗尽"状态样式（沿用既有 exhausted 红色徽标）。
  it('shows exhausted status when plan_exhausted is true', () => {
    const exhaustedPlan = {
      ...thPassAccount,
      extra: {
        ...thPassAccount.extra,
        th_pass_snapshot: {
          ...thPassAccount.extra.th_pass_snapshot,
          plan_exhausted: true
        }
      }
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: exhaustedPlan } })

    expect(wrapper.get('[data-test="cn-quota-status"]').text()).toBe(
      'admin.accounts.cnProviders.statusExhausted'
    )
    expect(wrapper.get('[data-test="cn-quota-pass-plan-bar"]').exists()).toBe(true)
  })

  // 状态机 exhausted：状态徽标翻红并显示恢复时间（cn_quota_lifecycle.recovery_at）。
  it('shows exhausted status with recovery time from cn_quota_lifecycle', () => {
    const exhausted = {
      ...thPassAccount,
      extra: {
        ...thPassAccount.extra,
        cn_quota_lifecycle: {
          state: 'exhausted',
          recovery_at: new Date(Date.now() + 90 * 60 * 1000).toISOString()
        }
      }
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: exhausted } })

    expect(wrapper.get('[data-test="cn-quota-status"]').text()).toBe(
      'admin.accounts.cnProviders.statusExhausted'
    )
    expect(wrapper.get('[data-test="cn-quota-recovery"]').text()).toContain(
      'admin.accounts.cnProviders.recoverAt'
    )
  })

  // L5 互斥：TH 无 Pass → 只渲染免费窗口（7 天滚动、仅已用计数），无订阅窗口。
  it('renders only the free rolling-7d window for a TH account without pass', () => {
    const wrapper = mount(CNQuotaUsageCell, { props: { account: thFreeAccount } })

    expect(wrapper.find('[data-test="cn-quota-free-window"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="cn-quota-pass-window"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="cn-quota-kira-window"]').exists()).toBe(false)

    const label = wrapper.get('[data-test="cn-quota-free-7d"]').text()
    expect(label).toContain('admin.accounts.cnProviders.freeRolling7d')
    expect(label).toContain('"requests":"14"')
    // 无订阅：不渲染窗口切换按钮
    expect(wrapper.find('[data-test="cn-quota-window-toggle"]').exists()).toBe(false)
  })

  // L5 互斥：Kira → 当日进度条 + 已用/上限 + VND 余额 + reset 倒计时，无 TH 内容。
  it('renders only the Kira daily window with VND balance', () => {
    const wrapper = mount(CNQuotaUsageCell, { props: { account: kiraAccount } })

    expect(wrapper.find('[data-test="cn-quota-kira-window"]').exists()).toBe(true)
    expect(wrapper.find('[data-test="cn-quota-pass-window"]').exists()).toBe(false)
    expect(wrapper.find('[data-test="cn-quota-free-window"]').exists()).toBe(false)

    // 进度条由共享 UsageProgressBar 渲染：utilization 来自快照 used_percent
    const bar = wrapper.findComponent(UsageProgressBar)
    expect(bar.exists()).toBe(true)
    expect(bar.props('utilization')).toBe(30)
    expect(bar.props('resetsAt')).toBe(kiraResetAtIso)
    // reset_at 倒计时（2 小时后）
    expect(wrapper.text()).toContain("1h 59m")
    // 已用/上限
    expect(wrapper.get('[data-test="cn-quota-kira-used-limit"]').text()).toContain('"used":1000')
    expect(wrapper.get('[data-test="cn-quota-kira-used-limit"]').text()).toContain('"limit":6000000')
    // VND 余额（<platform>_balance）
    expect(wrapper.get('[data-test="cn-quota-kira-vnd"]').text()).toContain('"balance":"38406"')
  })

  // 手动查询：走既有 queryQuota 接口；请求中按钮禁用；成功后 daily 档立即生效。
  it('probes via the existing quota endpoint with a disabled button while loading', async () => {
    let resolveProbe!: (value: unknown) => void
    queryQuota.mockImplementation(
      () => new Promise((resolve) => { resolveProbe = resolve })
    )
    const wrapper = mount(CNQuotaUsageCell, { props: { account: kiraAccount } })

    const probeButton = wrapper.get('[data-test="cn-quota-probe"]')
    expect(probeButton.attributes('disabled')).toBeUndefined()

    await probeButton.trigger('click')
    expect(queryQuota).toHaveBeenCalledWith(kiraAccount.id)
    // 请求中：按钮禁用，重复点击不再触发
    expect(wrapper.get('[data-test="cn-quota-probe"]').attributes('disabled')).toBeDefined()
    await wrapper.get('[data-test="cn-quota-probe"]').trigger('click')
    expect(queryQuota).toHaveBeenCalledTimes(1)

    resolveProbe({
      success: true,
      tiers: [{ window: 'daily', used_percent: 42, reset_at: '' }]
    })
    await flushPromises()
    // 请求结束：按钮恢复可用，探测返回的 daily 档立即渲染
    expect(wrapper.get('[data-test="cn-quota-probe"]').attributes('disabled')).toBeUndefined()
    expect(wrapper.findComponent(UsageProgressBar).props('utilization')).toBe(42)
  })

  // 探测失败：保留快照展示，仅追加错误行。
  it('keeps the snapshot display and shows an error line when the probe fails', async () => {
    queryQuota.mockResolvedValue({ success: false, error: 'UPSTREAM_TIMEOUT' })
    const wrapper = mount(CNQuotaUsageCell, { props: { account: kiraAccount } })

    await wrapper.get('[data-test="cn-quota-probe"]').trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('UPSTREAM_TIMEOUT')
    // 快照值未被失败探测覆盖
    expect(wrapper.findComponent(UsageProgressBar).props('utilization')).toBe(30)
  })

  // 非 TH/Kira 账号不渲染该组件（互斥只作用于 TH/Kira 上游）。
  it('stays hidden for a plain CN account', () => {
    const plain = {
      id: 1,
      platform: 'zhipu',
      type: 'apikey',
      credentials: { account_mode: 'coding', base_url: 'https://open.bigmodel.cn/api/paas/v4' },
      extra: {}
    } as unknown as Account
    const wrapper = mount(CNQuotaUsageCell, { props: { account: plain } })
    expect(wrapper.find('[data-test="cn-quota-usage"]').exists()).toBe(false)
    expect(queryQuota).not.toHaveBeenCalled()
  })
})
